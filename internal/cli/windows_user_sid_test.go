//go:build windows

package cli

import (
	"golang.org/x/sys/windows"

	"github.com/openclaw/crabbox/internal/privatefile"
)

func currentWindowsUserSID() (*windows.SID, error) {
	return privatefile.WindowsUserSID()
}
