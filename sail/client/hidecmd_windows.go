//go:build windows

package client

import (
	"os/exec"
	"syscall"
)

// command runs a helper program (reg, powershell) without a console window:
// a desktop app must not flash cmd or PowerShell windows in the background.
func command(name string, args ...string) *exec.Cmd {
	c := exec.Command(name, args...)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	return c
}
