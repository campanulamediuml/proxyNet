//go:build !windows

package singbox

import "os/exec"

// hideWindow is a no-op on non-Windows platforms.
func hideWindow(cmd *exec.Cmd) {}
