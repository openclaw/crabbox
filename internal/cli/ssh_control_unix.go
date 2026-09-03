//go:build !windows

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func sshControlLeaseDirectory(target SSHTarget) string {
	for _, file := range []string{target.KnownHostsFile, target.Key} {
		dir := filepath.Dir(file)
		key, err := testboxKeyPath(filepath.Base(dir))
		if err == nil && dir == filepath.Dir(key) {
			return dir
		}
	}
	return ""
}

func sshControlDirectory(leaseDir string) string {
	// 22-byte directory + 63-byte socket name + '/' + OpenSSH's 17-byte
	// temporary suffix fits Darwin's 104-byte sun_path, including its NUL.
	sum := sha256.Sum256([]byte(leaseDir))
	return filepath.Join("/tmp", "c"+base64.RawURLEncoding.EncodeToString(sum[:12]))
}

func inspectSSHControlPath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() || info.Mode().Perm()&0o077 != 0 ||
		(directory && !info.IsDir()) || (!directory && info.Mode()&os.ModeSocket == 0) {
		return fmt.Errorf("unsafe lease SSH control path %s", path)
	}
	return nil
}

func ensureSSHControlDirectory(target SSHTarget) error {
	leaseDir := sshControlLeaseDirectory(target)
	if leaseDir == "" || target.AuthSecret || target.NoControlMaster {
		return nil
	}
	if _, err := inspectTestboxLeaseDirectory(filepath.Base(leaseDir)); err != nil {
		return err
	}
	if info, err := os.Lstat(leaseDir); err != nil || !info.IsDir() {
		return fmt.Errorf("lease SSH directory is unavailable: %s", leaseDir)
	}
	dir := sshControlDirectory(leaseDir)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return inspectSSHControlPath(dir, true)
}

func closeLeaseSSHControlMasters(ctx context.Context, leaseDir string) error {
	dir := sshControlDirectory(leaseDir)
	if err := inspectSSHControlPath(dir, true); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sshCommandWaitDelay)
	defer cancel()
	results := make(chan error, len(entries))
	for _, entry := range entries {
		go func() {
			path := filepath.Join(dir, entry.Name())
			if err := inspectSSHControlPath(path, false); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					err = nil
				}
				results <- err
				return
			}
			results <- closeSSHControlMaster(ctx, path)
		}()
	}
	for range entries {
		err = errors.Join(err, <-results)
	}
	if err != nil {
		return err
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func removeInactiveSSHControlSocket(ctx context.Context, path string) (bool, error) {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if conn != nil {
		_ = conn.Close()
	}
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return false, err
	}
	after, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil || !os.SameFile(before, after) {
		return false, fmt.Errorf("lease SSH control socket changed during cleanup")
	}
	return true, os.Remove(path)
}

func closeSSHControlMaster(ctx context.Context, path string) error {
	control := func(operation string) (string, error) {
		// OpenSSH can fall through to ssh_connect after a failed mux handshake.
		// A nonconnecting proxy and explicit identity exclusions keep cleanup local.
		cmd := exec.CommandContext(ctx, directSSHExecutable(), "-F", os.DevNull,
			"-o", "ProxyCommand=/usr/bin/false", "-o", "IdentityFile=none",
			"-o", "CertificateFile=none", "-o", "IdentityAgent=none",
			"-S", path, "-O", operation, "--", "localhost")
		cmd.Env, cmd.WaitDelay = systemInspectionEnvironment(), sshCommandWaitDelay
		out := boundedSSHOutput{limit: 1024}
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		if out.exceeded {
			return "", ErrSSHOutputLimit
		}
		return strings.TrimSpace(out.String()), err
	}
	output, err := control("check")
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if absent, absentErr := removeInactiveSSHControlSocket(ctx, path); absent {
			return absentErr
		}
		return fmt.Errorf("inspect lease SSH master: %w", err)
	}
	// OpenSSH mux.c reports the master PID for -O check; bind its start identity
	// before requesting exit, then observe it without PID-targeted termination.
	value := strings.TrimSuffix(strings.TrimPrefix(output, "Master running (pid="), ")")
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 || output != fmt.Sprintf("Master running (pid=%d)", pid) {
		return fmt.Errorf("OpenSSH did not return a valid control-master identity")
	}
	started, err := webVNCDaemonProcessStartIdentity(pid)
	if err != nil {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			if absent, absentErr := removeInactiveSSHControlSocket(ctx, path); absent {
				return absentErr
			}
		}
		return fmt.Errorf("inspect SSH master start identity: %w", err)
	}
	_, exitErr := control("exit")
	for {
		current, err := inspectProcessSnapshot(pid)
		// Orphaned masters can remain zombies under a non-reaping container PID 1.
		// Join termination, not reaping, and still require the endpoint to be inactive.
		if err == nil && (current.started != started || current.exited) || err != nil && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			if absent, absentErr := removeInactiveSSHControlSocket(ctx, path); absent {
				return absentErr
			}
		}
		if exitErr != nil {
			return fmt.Errorf("close lease SSH master: %w", exitErr)
		}
		if err := sleepContext(ctx, 10*time.Millisecond); err != nil {
			return fmt.Errorf("wait for lease SSH master exit: %w", err)
		}
	}
}
