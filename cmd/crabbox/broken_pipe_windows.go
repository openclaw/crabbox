package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

const brokenPipeExitCode = 1

func isBrokenPipe(err error) bool {
	return errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_NO_DATA)
}

func notifyBrokenPipe(func()) func() { return func() {} }
