//go:build !windows

package client

import "os/exec"

// command runs a helper program (networksetup, gsettings, ...). On Windows
// the counterpart hides the console window that would otherwise flash.
func command(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }
