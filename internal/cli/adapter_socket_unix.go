//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

func adapterConnectHostSupported() error { return nil }

func normalizeAdapterUnixSocketPath(value string) (string, error) {
	path := filepath.Clean(value)
	if value == "" || !filepath.IsAbs(value) || path != value {
		return "", fmt.Errorf("Unix socket path must be absolute and clean")
	}
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if err != nil {
		return "", fmt.Errorf("inspect Unix socket directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("Unix socket directory must be a real directory")
	}
	if !adapterFileOwnedByCurrentUser(info) {
		return "", fmt.Errorf("Unix socket directory must be owned by the current user")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("Unix socket directory must not be writable by group or others")
	}
	return path, nil
}

func adapterFileOwnedByCurrentUser(info os.FileInfo) bool {
	statInfo, ok := info.Sys().(*syscall.Stat_t)
	return ok && statInfo.Uid == uint32(os.Getuid())
}

func validateAdapterUnixSocket(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect local adapter Unix socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("local adapter endpoint is not a Unix socket")
	}
	if !adapterFileOwnedByCurrentUser(info) {
		return fmt.Errorf("local adapter Unix socket must be owned by the current user")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("local adapter Unix socket must not be accessible by group or others")
	}
	return nil
}

func listenAdapterUnixSocket(value string) (net.Listener, func(), error) {
	path, err := normalizeAdapterUnixSocketPath(value)
	if err != nil {
		return nil, nil, err
	}
	ownershipLock, err := acquireControllerStateLock(path)
	if err != nil {
		return nil, nil, fmt.Errorf("lock Unix socket ownership: %w", err)
	}
	keepOwnershipLock := false
	defer func() {
		if !keepOwnershipLock {
			_ = ownershipLock.Unlock()
		}
	}()
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 || !adapterFileOwnedByCurrentUser(info) {
			return nil, nil, fmt.Errorf("refusing to replace non-owned Unix socket path")
		}
		if conn, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond); dialErr == nil {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("Unix socket is already accepting connections")
		} else if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			if _, currentErr := os.Lstat(path); errors.Is(currentErr, os.ErrNotExist) {
				// The endpoint disappeared between inspection and probe. Continue
				// under the ownership lock and install our listener below.
			} else {
				return nil, nil, fmt.Errorf("probe existing Unix socket without replacing it: %w", dialErr)
			}
		}
		current, currentErr := os.Lstat(path)
		if currentErr == nil {
			if !os.SameFile(info, current) {
				return nil, nil, fmt.Errorf("Unix socket changed while checking whether it was stale")
			}
			if current.Mode()&os.ModeSocket == 0 || current.Mode()&os.ModeSymlink != 0 || !adapterFileOwnedByCurrentUser(current) {
				return nil, nil, fmt.Errorf("refusing to replace changed or non-owned Unix socket path")
			}
			if err := os.Remove(path); err != nil {
				return nil, nil, fmt.Errorf("remove stale Unix socket: %w", err)
			}
		} else if !errors.Is(currentErr, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("reinspect stale Unix socket: %w", currentErr)
		}
	} else if !os.IsNotExist(statErr) {
		return nil, nil, fmt.Errorf("inspect Unix socket path: %w", statErr)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on Unix socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, nil, fmt.Errorf("secure Unix socket: %w", err)
	}
	if err := validateAdapterUnixSocket(path); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, nil, err
	}
	installed, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		return nil, nil, fmt.Errorf("inspect installed Unix socket: %w", err)
	}
	keepOwnershipLock = true
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			current, statErr := os.Lstat(path)
			if statErr == nil && os.SameFile(installed, current) {
				_ = os.Remove(path)
			}
			_ = ownershipLock.Unlock()
		})
	}
	return listener, cleanup, nil
}

func newAdapterLocalClient(socketPath string, desktopRequestTimeout time.Duration) (*http.Client, error) {
	path, err := normalizeAdapterUnixSocketPath(socketPath)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if err := validateAdapterUnixSocket(path); err != nil {
				return nil, err
			}
			conn, err := dialer.DialContext(ctx, "unix", path)
			if err != nil {
				return nil, err
			}
			if err := verifyAdapterUnixPeer(conn); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return conn, nil
		},
		ResponseHeaderTimeout: desktopRequestTimeout,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}
