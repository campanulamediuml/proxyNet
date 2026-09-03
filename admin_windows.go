//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// isAdmin reports whether the current process is running with elevated privileges.
func isAdmin() bool {
	var sid *windows.SID
	err := windows.AllocateAndInitializeSid(
		&windows.SECURITY_NT_AUTHORITY,
		2,
		windows.SECURITY_BUILTIN_DOMAIN_RID,
		windows.DOMAIN_ALIAS_RID_ADMINS,
		0, 0, 0, 0, 0, 0,
		&sid,
	)
	if err != nil {
		return false
	}
	defer windows.FreeSid(sid)

	token := windows.Token(0)
	member, err := token.IsMember(sid)
	if err != nil {
		return false
	}
	return member
}

// openURL opens a URL in the default browser.
func openURL(url string) {
	verb := "open"
	_ = windows.ShellExecute(
		0,
		syscall.StringToUTF16Ptr(verb),
		syscall.StringToUTF16Ptr(url),
		nil,
		nil,
		windows.SW_NORMAL,
	)
}

// runAsAdmin restarts the current executable with UAC elevation.
func runAsAdmin() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable path: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	cwd := filepath.Dir(exe)
	verb := "runas"

	// Forward command-line arguments (e.g. -restore) to the elevated process.
	var args string
	for i, a := range os.Args[1:] {
		if i > 0 {
			args += " "
		}
		args += syscall.EscapeArg(a)
	}

	err = windows.ShellExecute(
		0,
		syscall.StringToUTF16Ptr(verb),
		syscall.StringToUTF16Ptr(exe),
		syscall.StringToUTF16Ptr(args),
		syscall.StringToUTF16Ptr(cwd),
		windows.SW_NORMAL,
	)
	if err != nil {
		return fmt.Errorf("shell execute: %w", err)
	}
	return nil
}
