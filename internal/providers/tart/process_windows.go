//go:build windows

package tart

import "os/exec"

func detachCommand(_ *exec.Cmd) {}
