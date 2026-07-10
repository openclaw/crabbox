//go:build !windows

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// openExternalRoutingFile walks every path component with O_NOFOLLOW, then
// validates the exact directory and file descriptors used for the read. This
// closes both final-file and ancestor symlink/rename races.
func openExternalRoutingFile(path string) (*os.File, string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("resolve external routing path: %w", err)
	}
	absPath = filepath.Clean(absPath)
	securityPath := externalRoutingSecurityPath(absPath)
	components := strings.Split(strings.TrimPrefix(securityPath, string(os.PathSeparator)), string(os.PathSeparator))
	if len(components) == 0 || components[0] == "" {
		return nil, "", fmt.Errorf("external routing path must name a file")
	}

	dirFD, err := unix.Open(string(os.PathSeparator), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open external routing root: %w", err)
	}
	defer func() {
		if dirFD >= 0 {
			_ = unix.Close(dirFD)
		}
	}()

	for index, component := range components[:len(components)-1] {
		nextFD, openErr := unix.Openat(dirFD, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			return nil, "", fmt.Errorf("open external routing directory %q without symlinks: %w", component, openErr)
		}
		var stat unix.Stat_t
		if statErr := unix.Fstat(nextFD, &stat); statErr != nil {
			_ = unix.Close(nextFD)
			return nil, "", fmt.Errorf("inspect external routing directory %q: %w", component, statErr)
		}
		if validateErr := validateExternalRoutingDirectory(&stat, index == len(components)-2); validateErr != nil {
			_ = unix.Close(nextFD)
			return nil, "", fmt.Errorf("external routing directory %q: %w", component, validateErr)
		}
		_ = unix.Close(dirFD)
		dirFD = nextFD
	}

	fileFD, err := unix.Openat(dirFD, components[len(components)-1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open external routing file without symlinks: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fileFD, &stat); err != nil {
		_ = unix.Close(fileFD)
		return nil, "", fmt.Errorf("inspect external routing file: %w", err)
	}
	if err := validateExternalRoutingFile(&stat); err != nil {
		_ = unix.Close(fileFD)
		return nil, "", err
	}
	file := os.NewFile(uintptr(fileFD), absPath)
	if file == nil {
		_ = unix.Close(fileFD)
		return nil, "", fmt.Errorf("create external routing file handle")
	}
	return file, absPath, nil
}

func externalRoutingSecurityPath(path string) string {
	if runtime.GOOS != "darwin" {
		return path
	}
	// Darwin exposes these root-owned compatibility aliases as symlinks. Map
	// only the fixed OS aliases to their canonical roots; every caller-owned
	// component is still opened and validated with O_NOFOLLOW.
	for _, alias := range []string{"/var", "/tmp", "/etc"} {
		if path == alias || strings.HasPrefix(path, alias+string(os.PathSeparator)) {
			return "/private" + path
		}
	}
	return path
}

func validateExternalRoutingDirectory(stat *unix.Stat_t, immediateParent bool) error {
	if stat == nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("must be a directory")
	}
	uid := uint32(os.Geteuid())
	if stat.Uid != uid && stat.Uid != 0 {
		return fmt.Errorf("must be owned by the current user or root")
	}
	worldWritableStickyRoot := stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0
	if stat.Mode&0o022 != 0 && !worldWritableStickyRoot {
		return fmt.Errorf("must not be writable by group or others")
	}
	if immediateParent {
		if stat.Uid != uid {
			return fmt.Errorf("must be owned by the current user")
		}
		if stat.Mode&0o077 != 0 {
			return fmt.Errorf("must not be accessible by group or others")
		}
	}
	return nil
}

func validateExternalRoutingFile(stat *unix.Stat_t) error {
	if stat == nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("external routing file must be a regular file")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("external routing file must be owned by the current user")
	}
	if stat.Mode&0o077 != 0 {
		return fmt.Errorf("external routing file must not be accessible by group or others")
	}
	return nil
}
