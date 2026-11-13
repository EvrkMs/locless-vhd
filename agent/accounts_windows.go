package main

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"locless-vhd/proto"
)

// Local accounts through netapi32, as `net user` does it, without a password on
// any command line.
var (
	netapi32                    = syscall.NewLazyDLL("netapi32.dll")
	procNetUserEnum             = netapi32.NewProc("NetUserEnum")
	procNetUserGetInfo          = netapi32.NewProc("NetUserGetInfo")
	procNetUserSetInfo          = netapi32.NewProc("NetUserSetInfo")
	procNetUserAdd              = netapi32.NewProc("NetUserAdd")
	procNetLocalGroupDelMembers = netapi32.NewProc("NetLocalGroupDelMembers")
	procNetAPIBufferFree        = netapi32.NewProc("NetApiBufferFree")
)

const (
	filterNormalAccount  = 0x2
	maxPreferredLength   = 0xFFFFFFFF
	nerrUserNotFound     = 2221
	errorMemberNotInList = 1377 // ERROR_MEMBER_NOT_IN_ALIAS
	errorMoreData        = 234
	ridAdministrator     = 500

	ufScript           = 0x0001
	ufAccountDisable   = 0x0002
	ufPasswdNotReqd    = 0x0020
	ufDontExpirePasswd = 0x10000
	userPrivUser       = 1
)

// userInfo20 is USER_INFO_20.
type userInfo20 struct {
	name, fullName, comment *uint16
	flags, userID           uint32
}

// userInfo1 is USER_INFO_1.
type userInfo1 struct {
	name, password    *uint16
	passwordAge, priv uint32
	homeDir, comment  *uint16
	flags             uint32
	scriptPath        *uint16
}

// applyAccounts enables the built-in administrator with the password and makes the
// player a standard user, creating it if needed.
func applyAccounts(a *proto.Accounts) error {
	var errs []error
	if a.AdminPassword != "" {
		errs = append(errs, setAdministrator(a.AdminPassword))
	}
	if a.PlayerUser != "" {
		errs = append(errs, makePlayer(a.PlayerUser))
	}
	return errors.Join(errs...)
}

// setAdministrator finds the built-in administrator by its RID, since its name
// depends on the language of Windows, sets the password and enables it.
func setAdministrator(password string) error {
	name, flags, err := builtinAdministrator()
	if err != nil {
		return fmt.Errorf("администратор: %w", err)
	}
	pw, err := syscall.UTF16PtrFromString(password)
	if err != nil {
		return err
	}
	if err := netUserSetInfo(name, 1003, unsafe.Pointer(&pw)); err != nil { // USER_INFO_1003
		return fmt.Errorf("пароль администратора: %w", err)
	}
	flags &^= ufAccountDisable | ufPasswdNotReqd
	if err := netUserSetInfo(name, 1008, unsafe.Pointer(&flags)); err != nil { // USER_INFO_1008
		return fmt.Errorf("включение администратора: %w", err)
	}
	return nil
}

func builtinAdministrator() (string, uint32, error) {
	var buf *byte
	var read, total, resume uint32
	r, _, _ := procNetUserEnum.Call(0, 20, filterNormalAccount, uintptr(unsafe.Pointer(&buf)), maxPreferredLength,
		uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&resume)))
	if buf != nil {
		defer procNetAPIBufferFree.Call(uintptr(unsafe.Pointer(buf)))
	}
	if r != 0 && r != errorMoreData {
		return "", 0, syscall.Errno(r)
	}
	for _, u := range unsafe.Slice((*userInfo20)(unsafe.Pointer(buf)), read) {
		if u.userID == ridAdministrator {
			return windows.UTF16PtrToString(u.name), u.flags, nil
		}
	}
	return "", 0, errors.New("встроенный администратор не найден")
}

// makePlayer keeps an existing account with its profile and only takes it out of
// the administrators. A missing one is created as a standard user without a
// password.
func makePlayer(name string) error {
	if !proto.ValidUserName(name) {
		return fmt.Errorf("недопустимое имя пользователя %q", name)
	}
	n, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	var buf *byte
	r, _, _ := procNetUserGetInfo.Call(0, uintptr(unsafe.Pointer(n)), 0, uintptr(unsafe.Pointer(&buf)))
	if buf != nil {
		procNetAPIBufferFree.Call(uintptr(unsafe.Pointer(buf)))
	}
	switch r {
	case 0:
	case nerrUserNotFound:
		empty, _ := syscall.UTF16PtrFromString("")
		u := userInfo1{name: n, password: empty, priv: userPrivUser, flags: ufScript | ufPasswdNotReqd | ufDontExpirePasswd}
		if r, _, _ := procNetUserAdd.Call(0, 1, uintptr(unsafe.Pointer(&u)), 0); r != 0 {
			return fmt.Errorf("создание %s: %w", name, syscall.Errno(r))
		}
	default:
		return fmt.Errorf("пользователь %s: %w", name, syscall.Errno(r))
	}
	return leaveAdministrators(name)
}

// leaveAdministrators takes the account out of the local Administrators group,
// found by its SID because the name is translated.
func leaveAdministrators(name string) error {
	sid, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	group, _, _, err := sid.LookupAccount("")
	if err != nil {
		return err
	}
	g, err := syscall.UTF16PtrFromString(group)
	if err != nil {
		return err
	}
	member, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	// LOCALGROUP_MEMBERS_INFO_3 is a single pointer to the name.
	r, _, _ := procNetLocalGroupDelMembers.Call(0, uintptr(unsafe.Pointer(g)), 3, uintptr(unsafe.Pointer(&member)), 1)
	if r != 0 && r != errorMemberNotInList {
		return fmt.Errorf("права администратора у %s: %w", name, syscall.Errno(r))
	}
	return nil
}

func netUserSetInfo(name string, level uint32, info unsafe.Pointer) error {
	n, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	if r, _, _ := procNetUserSetInfo.Call(0, uintptr(unsafe.Pointer(n)), uintptr(level), uintptr(info), 0); r != 0 {
		return syscall.Errno(r)
	}
	return nil
}
