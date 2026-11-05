//go:build !windows

package cli

import (
	"context"
	"errors"
)

func windowsSFTPFileVersion(string) ([4]uint16, error) {
	return [4]uint16{}, errors.New("native Windows SFTP copy requires a Windows operator")
}

func publishWindowsSFTPFile(context.Context, string, string) error {
	return errors.New("native Windows SFTP copy requires a Windows operator")
}
