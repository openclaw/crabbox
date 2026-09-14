//go:build windows

package cli

import (
	"fmt"
	"io/fs"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func gitOverlayTemporaryDirectoryMode(finalMode os.FileMode) os.FileMode {
	return finalMode | 0o222
}

func syncGitOverlaySymlinkTimes(path string, modTime time.Time) error {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("inspect overlay snapshot symlink: %w", err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return fmt.Errorf("overlay snapshot symlink is not a reparse point")
	}
	value := windows.NsecToFiletime(modTime.UnixNano())
	if err := windows.SetFileTime(handle, nil, &value, &value); err != nil {
		return fmt.Errorf("set overlay snapshot symlink time: %w", err)
	}
	return nil
}

func thawGitOverlaySnapshotFiles(root *os.Root) error {
	return fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		file, err := root.Open(path)
		if err != nil {
			return fmt.Errorf("open git overlay snapshot cleanup file %q: %w", path, err)
		}
		defer file.Close()
		opened, err := file.Stat()
		if err != nil {
			return fmt.Errorf("stat git overlay snapshot cleanup file handle %q: %w", path, err)
		}
		current, err := root.Lstat(path)
		if err != nil {
			return fmt.Errorf("stat git overlay snapshot cleanup file path %q: %w", path, err)
		}
		if !opened.Mode().IsRegular() ||
			!current.Mode().IsRegular() ||
			current.Mode()&os.ModeSymlink != 0 ||
			!os.SameFile(opened, current) {
			return fmt.Errorf("git overlay snapshot cleanup file changed at %q", path)
		}
		if opened.Mode().Perm()&0o200 != 0 {
			return nil
		}
		if err := root.Chmod(path, opened.Mode().Perm()|0o200); err != nil {
			return fmt.Errorf("thaw git overlay snapshot file %q: %w", path, err)
		}
		refreshed, err := root.Lstat(path)
		if err != nil {
			return fmt.Errorf("restat git overlay snapshot cleanup file path %q: %w", path, err)
		}
		if !refreshed.Mode().IsRegular() ||
			refreshed.Mode()&os.ModeSymlink != 0 ||
			!os.SameFile(opened, refreshed) ||
			refreshed.Mode().Perm()&0o200 == 0 {
			return fmt.Errorf("git overlay snapshot cleanup file changed while thawing at %q", path)
		}
		return nil
	})
}

func openGitOverlaySnapshotParent(path string) (*os.File, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("inspect overlay snapshot parent: %w", err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("overlay snapshot parent must not be a reparse point")
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("overlay snapshot parent must be a directory")
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("create overlay snapshot parent handle")
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	current, err := os.Lstat(path)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !opened.IsDir() ||
		!current.IsDir() ||
		current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(opened, current) {
		_ = file.Close()
		return nil, fmt.Errorf("overlay snapshot parent changed while opening")
	}
	return file, nil
}
