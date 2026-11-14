package main

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procWTSSendMessage = windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSSendMessageW")

// Styles of the message window, from winuser.h.
const (
	mbOK            = 0x00000000
	mbIconWarning   = 0x00000030
	mbSystemModal   = 0x00001000
	mbSetForeground = 0x00010000
	mbTopmost       = 0x00040000
)

// spaceShown is how long the message window stays before it closes by itself.
const spaceShown = 120 // seconds

// watchSpace checks the free space of the store every spaceCheck until stop
// closes, and tells the player at the console when it runs low.
func watchSpace(stop <-chan struct{}) {
	var watch spaceWatch
	tick := time.NewTicker(spaceCheck)
	defer tick.Stop()
	for {
		if free, ok := storeFree(); ok {
			if msg := watch.check(free, time.Now()); msg != "" {
				notifyConsole(spaceTitle, msg)
			}
		}
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}

// storeFree reads the free space of the store through its volume path, which
// needs no letter.
func storeFree() (uint64, bool) {
	volume, err := findStoreVolume()
	if err != nil {
		return 0, false
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(windows.StringToUTF16Ptr(volume), &avail, &total, &free); err != nil {
		return 0, false
	}
	return avail, true
}

// notifyConsole shows a message window to whoever sits at the PC. A service runs
// in session 0 and has no desktop of its own; WTSSendMessage puts the window on
// the desktop of the console session. It does not wait for the player: the
// window closes by itself after spaceShown seconds.
func notifyConsole(title, message string) {
	session := windows.WTSGetActiveConsoleSessionId()
	if session == 0xFFFFFFFF {
		return // nobody signed in
	}
	t, _ := windows.UTF16FromString(title)
	m, _ := windows.UTF16FromString(message)
	var response uint32
	procWTSSendMessage.Call(
		0, // WTS_CURRENT_SERVER_HANDLE
		uintptr(session),
		uintptr(unsafe.Pointer(&t[0])), uintptr((len(t)-1)*2), // lengths in bytes, without the 0
		uintptr(unsafe.Pointer(&m[0])), uintptr((len(m)-1)*2),
		mbOK|mbIconWarning|mbSystemModal|mbSetForeground|mbTopmost,
		spaceShown,
		uintptr(unsafe.Pointer(&response)),
		0, // do not wait for the answer
	)
}
