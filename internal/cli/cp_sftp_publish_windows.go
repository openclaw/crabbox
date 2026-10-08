//go:build windows

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func lockWindowsSFTPDirectory(name string) ([]windows.Handle, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(absolute)
	current := volume + `\`
	parts := strings.FieldsFunc(strings.TrimPrefix(absolute, volume), func(c rune) bool { return c == '\\' || c == '/' })
	paths := []string{current}
	for _, part := range parts {
		current = filepath.Join(current, part)
		paths = append(paths, current)
	}
	var handles []windows.Handle
	for _, path := range paths {
		pointer, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, errors.Join(err, closeWindowsRunOutputDirectories(handles))
		}
		handle, err := windows.CreateFile(pointer, windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			return nil, errors.Join(err, closeWindowsRunOutputDirectories(handles))
		}
		handles = append(handles, handle)
		if err := validatePrivateWindowsFileType(handle, true); err != nil {
			return nil, errors.Join(err, closeWindowsRunOutputDirectories(handles))
		}
	}
	return handles, nil
}

func publishWindowsSFTPFile(ctx context.Context, source, destination string) (err error) {
	parents, err := lockWindowsSFTPDirectory(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeWindowsRunOutputDirectories(parents)) }()
	staging, err := lockWindowsSFTPDirectory(filepath.Dir(source))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeWindowsRunOutputDirectories(staging)) }()
	if _, statErr := os.Lstat(destination); statErr == nil {
		if _, err := windowsSFTPRegularFile(destination); err != nil {
			return err
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	file, err := os.OpenFile(source, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return replacePrivateRunOutputTemp(source, destination)
}
