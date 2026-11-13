# Builds the WinPE the agent boots from, out of the Windows Recovery Environment
# of this machine or a given WIM, so nobody has to copy boot.wim from an ISO.
#
# WinRE is WinPE with recovery tools on top. The script mounts a copy, removes
# what the agent never uses and what only takes space, and exports the result
# with maximum compression: iPXE downloads the whole file before WinPE starts,
# so its size is boot time. The source is never changed.
#
# It needs administrator rights and the DISM of Windows; nothing is installed.
# The server runs it itself when EFI\WinPE\boot.wim is missing; by hand:
#
#   powershell -ExecutionPolicy Bypass -File server\winpe\build.ps1 -Out EFI\WinPE\boot.wim -Work data\winpe
#
# Removed:
#   - older versions of components in WinSxS, left by updates. Files in use are
#     hard links to the newest version, which stays;
#   - 32-bit support (SysWOW64, x86 and wow64 components): the agent and the
#     tools it runs are 64-bit;
#   - speech;
#   - the recovery environment itself: sources\recovery and its components.
#     The server replaces the shell with the agent anyway.
param(
    [Parameter(Mandatory)][string]$Out,
    [Parameter(Mandatory)][string]$Work,  # empty space for a copy and the mount; a short path, DISM dislikes long ones
    [string]$Source = ''                  # a WIM with WinPE or WinRE at index 1; the WinRE of this machine if empty
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
function Step([string]$text) { Write-Output ("{0:HH:mm:ss} {1}" -f (Get-Date), $text) }

function Dism([string[]]$arguments) {
    $output = & dism.exe /English @arguments 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "dism $($arguments[0]) failed ($LASTEXITCODE): $(($output | Select-Object -Last 3) -join ' ')"
    }
}

# FindWinRE returns the path of the WinRE of this Windows. reagentc names a
# folder on the hidden recovery partition, readable through GLOBALROOT without
# giving it a letter; a disabled WinRE lies in System32\Recovery.
function FindWinRE {
    $info = & reagentc.exe /info 2>&1 | Out-String
    if ($info -match 'Windows RE location:\s+(\S+)') {
        $path = $Matches[1].TrimEnd('\') + '\Winre.wim'  # Join-Path knows no GLOBALROOT
        if ([System.IO.File]::Exists($path)) { return $path }
    }
    $path = Join-Path $env:SystemRoot 'System32\Recovery\Winre.wim'
    if (Test-Path $path) { return $path }
    throw 'WinRE not found on this machine: give -Source, for example sources\boot.wim of a Windows ISO'
}

# Drop deletes paths of the mounted image. They belong to TrustedInstaller, so
# the administrators take them first.
function Drop([string[]]$paths) {
    foreach ($path in $paths) {
        if (-not (Test-Path -LiteralPath $path)) { continue }
        & takeown.exe /F $path /R /A /D Y 2>&1 | Out-Null
        & icacls.exe $path /grant '*S-1-5-32-544:F' /T /C /Q 2>&1 | Out-Null
        Remove-Item -LiteralPath $path -Recurse -Force
    }
}

# Superseded returns the folders of WinSxS that hold an older version of a
# component also present in a newer one.
function Superseded([string]$sxs) {
    $pattern = '^(.+)_31bf3856ad364e35_(\d+\.\d+\.\d+\.\d+)_([^_]*)_[0-9a-f]+$'
    Get-ChildItem -LiteralPath $sxs -Directory |
        Where-Object { $_.Name -match $pattern } |
        ForEach-Object {
            $null = $_.Name -match $pattern
            [pscustomobject]@{ Path = $_.FullName; Key = $Matches[1] + '|' + $Matches[3]; Version = [version]$Matches[2] }
        } |
        Group-Object Key | Where-Object Count -gt 1 |
        ForEach-Object { $_.Group | Sort-Object Version -Descending | Select-Object -Skip 1 | ForEach-Object Path }
}

function Build {
$admin = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
if (-not $admin.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'DISM needs administrator rights: run the server or this script as an administrator'
}
$Out = [System.IO.Path]::GetFullPath($Out)
$Work = [System.IO.Path]::GetFullPath($Work)
$copy = Join-Path $Work 'source.wim'
$mount = Join-Path $Work 'mount'
$built = "$Out.new"
New-Item -ItemType Directory -Force $Work, $mount, (Split-Path $Out) | Out-Null
if (Get-ChildItem -LiteralPath $mount -Force) {
    # A build that died mounted: give the folder back before using it.
    & dism.exe /English /Unmount-Image /MountDir:$mount /Discard 2>&1 | Out-Null
    & dism.exe /English /Cleanup-Mountpoints 2>&1 | Out-Null
}

if (-not $Source) { $Source = FindWinRE }
Step "source $Source"
[System.IO.File]::Copy($Source, $copy, $true)
Set-ItemProperty -LiteralPath $copy -Name IsReadOnly -Value $false

$mounted = $false
try {
    Step 'mount'
    Dism '/Mount-Image', "/ImageFile:$copy", '/Index:1', "/MountDir:$mount"
    $mounted = $true
    $windows = Join-Path $mount 'Windows'
    $sxs = Join-Path $windows 'WinSxS'

    Step 'remove older versions of components'
    Drop (Superseded $sxs)

    Step 'remove 32-bit support, speech and the recovery environment'
    $parts = '^(x86|wow64)_|speech|texttospeech|rejuvenation|recoveryenvironment|-winre-'
    Drop (Get-ChildItem -LiteralPath $sxs -Directory | Where-Object Name -match $parts | ForEach-Object FullName)
    Drop @(
        (Join-Path $windows 'SysWOW64'),
        (Join-Path $windows 'Speech'),
        (Join-Path $windows 'Speech_OneCore'),
        (Join-Path $mount 'sources\recovery')
    )

    Step 'unmount'
    Dism '/Unmount-Image', "/MountDir:$mount", '/Commit'
    $mounted = $false

    # Export writes only what the image still uses, so the removed files really
    # leave the file. wimboot reads LZX ("max") but not LZMS ("recovery").
    Step 'export'
    if (Test-Path -LiteralPath $built) { Remove-Item -LiteralPath $built -Force }
    Dism '/Export-Image', "/SourceImageFile:$copy", '/SourceIndex:1', "/DestinationImageFile:$built", '/Compress:max'
    Move-Item -LiteralPath $built -Destination $Out -Force
    $from = ([System.IO.FileInfo]$Source).Length, (Get-Item -LiteralPath $Out).Length | ForEach-Object { [int]($_ / 1MB) }
    Step ("done {0} MB -> {1} MB: {2}" -f $from[0], $from[1], $Out)
}
finally {
    if ($mounted) { & dism.exe /English /Unmount-Image /MountDir:$mount /Discard 2>&1 | Out-Null }
    Remove-Item -LiteralPath $copy, $built -Force -ErrorAction SilentlyContinue
}
}

# One line for the server log and the panel instead of the PowerShell error
# block. The finally of Build has run by the time the catch prints it.
try { Build }
catch {
    Write-Output "error: $($_.Exception.Message)"
    exit 1
}
