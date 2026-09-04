//go:build windows

package singbox

import (
	"os/exec"
	"syscall"
)

// hideWindow prevents the sing-box console window from appearing when the
// parent runs as a GUI (windowsgui) process.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
