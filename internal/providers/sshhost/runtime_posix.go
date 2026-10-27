//go:build !windows

package sshhost

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func validateRuntime(_ string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("native managed hosts require a root-owned agent for isolated accounts and cleanup")
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("native managed account runtime is currently available on Linux only")
	}
	for _, bin := range []string{"useradd", "userdel", "runuser", "getent", "pkill", "pgrep", "groupdel", "git", "rsync", "bwrap"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("managed host requires %s", bin)
		}
	}
	return nil
}
func newRuntimeState(host string, l hostAssignment) (*runtimeState, error) {
	if len(l.LeaseID) != 16 || !strings.HasPrefix(l.LeaseID, "cbx_") {
		return nil, fmt.Errorf("invalid lease ID")
	}
	for _, r := range l.LeaseID[4:] {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return nil, fmt.Errorf("invalid lease ID")
		}
	}
	user := "cbh" + l.LeaseID[4:]
	return &runtimeState{User: user, Marker: fmt.Sprintf("crabbox-%s-%s-%d-%d", host, l.LeaseID, l.Generation, l.AccessGeneration), Home: "/var/lib/crabbox-host-accounts/" + user + "/home"}, nil
}
func account(ctx context.Context, r *runtimeState) ([]string, error) {
	out, err := exec.CommandContext(ctx, "getent", "passwd", r.User).Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok && e.ExitCode() == 2 {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot inspect managed account")
	}
	fields := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(fields) != 7 || fields[0] != r.User || fields[4] != r.Marker || fields[5] != r.Home || fields[2] == "0" || fields[2] == "" {
		return nil, fmt.Errorf("managed account ownership mismatch")
	}
	if (r.UID != "" && r.UID != fields[2]) || (r.GID != "" && r.GID != fields[3]) {
		return nil, fmt.Errorf("managed account UID changed")
	}
	return fields, nil
}
func prepareRuntime(ctx context.Context, r *runtimeState, l hostAssignment, _ string, save func() error) error {
	if r.Home != "/var/lib/crabbox-host-accounts/"+r.User+"/home" {
		return fmt.Errorf("invalid managed home binding")
	}
	base := filepath.Dir(filepath.Dir(r.Home))
	if err := os.MkdirAll(base, 0711); err != nil {
		return err
	}
	info, err := os.Lstat(base)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 || info.Sys().(*syscall.Stat_t).Uid != 0 {
		return fmt.Errorf("unsafe managed account root")
	}
	control := filepath.Dir(r.Home)
	if err := os.Mkdir(control, 0711); err != nil && !os.IsExist(err) {
		return err
	}
	info, err = os.Lstat(control)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 || info.Sys().(*syscall.Stat_t).Uid != 0 {
		return fmt.Errorf("unsafe managed account directory")
	}
	fields, err := account(ctx, r)
	if err != nil {
		return err
	}
	if fields == nil {
		if _, err = os.Lstat(r.Home); !os.IsNotExist(err) {
			return fmt.Errorf("managed home already exists or cannot be inspected")
		}
		if err = exec.CommandContext(ctx, "useradd", "--user-group", "--create-home", "--home-dir", r.Home, "--comment", r.Marker, "--shell", "/bin/sh", r.User).Run(); err != nil {
			return fmt.Errorf("create managed account: %w", err)
		}
		fields, err = account(ctx, r)
		if err != nil || fields == nil {
			return fmt.Errorf("created account identity unavailable")
		}
	}
	r.UID = fields[2]
	r.GID = fields[3]
	if err = save(); err != nil {
		return err
	}
	uid, err := strconv.Atoi(r.UID)
	if err != nil || uid <= 0 {
		return fmt.Errorf("invalid managed UID")
	}
	gid, err := strconv.Atoi(fields[3])
	if err != nil {
		return err
	}
	if err = os.Chmod(r.Home, 0700); err != nil {
		return err
	}
	work := filepath.Join(r.Home, "work")
	if err = os.Mkdir(work, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	if err = os.Chown(work, uid, gid); err != nil {
		return err
	}
	private, public, err := generateHostKey()
	if err != nil {
		return err
	}
	r.HostKey = strings.TrimSpace(public)
	r.Endpoint = filepath.Join(control, "endpoint.json")
	r.Port, err = freePort()
	if err != nil {
		return err
	}
	if err = writeJSON(r.Endpoint, endpointConfig{PublicKey: l.PublicKey, PrivateKey: private, WorkRoot: work, Deadline: l.Deadline, Port: r.Port}, 0600); err != nil {
		return err
	}
	if err = os.Chown(control, 0, gid); err != nil {
		return err
	}
	if err = os.Chmod(control, 0710); err != nil {
		return err
	}
	if err = os.Chown(r.Endpoint, 0, gid); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	source, err := os.Open(executable)
	if err != nil {
		return err
	}
	defer source.Close()
	target := filepath.Join(control, "crabbox-endpoint")
	dest, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
	if err != nil {
		return err
	}
	_, err = io.Copy(dest, source)
	closeErr := dest.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Chmod(r.Endpoint, 0440); err != nil {
		return err
	}
	if err = save(); err != nil {
		return err
	}
	command := exec.Command("runuser", "-u", r.User, "--", "bwrap", "--unshare-user", "--unshare-pid", "--unshare-ipc", "--ro-bind", "/", "/", "--tmpfs", "/tmp", "--tmpfs", "/var/tmp", "--bind", r.Home, r.Home, "--proc", "/proc", "--dev", "/dev", "--chdir", work, target, "host", "endpoint", "--endpoint-state", r.Endpoint)
	command.Stderr = os.Stderr
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + r.Home, "USER=" + r.User, "LOGNAME=" + r.User}
	if err = command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}
func cleanupRuntime(ctx context.Context, r *runtimeState, _ string) error {
	if r.Home != "/var/lib/crabbox-host-accounts/"+r.User+"/home" || !strings.HasPrefix(r.User, "cbh") || strings.ContainsAny(r.User, "/\\") {
		return fmt.Errorf("invalid managed home binding")
	}
	fields, err := account(ctx, r)
	if err != nil {
		return err
	}
	if fields == nil {
		if _, err = os.Lstat(r.Home); !os.IsNotExist(err) {
			return fmt.Errorf("managed account absent but home remains; operator recovery required")
		}
		if err := cleanupPrivateGroup(ctx, r); err != nil {
			return err
		}
		return os.RemoveAll(filepath.Dir(r.Home))
	}
	uid := fields[2]
	// The account is unique to this generation. No process-name or machine-wide
	// cleanup, shutdown, reboot, or provider deletion is permitted here.
	_ = exec.CommandContext(ctx, "pkill", "-KILL", "-u", uid).Run()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = exec.CommandContext(ctx, "pgrep", "-u", uid).Run()
		if e, ok := err.(*exec.ExitError); ok && e.ExitCode() == 1 {
			break
		}
		if err != nil || time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("managed processes not confirmed stopped")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err = account(ctx, r); err != nil {
		return err
	}
	if err = exec.CommandContext(ctx, "userdel", "--remove", r.User).Run(); err != nil {
		return fmt.Errorf("managed account removal pending")
	}
	fields, err = account(ctx, r)
	if err != nil || fields != nil {
		return fmt.Errorf("managed account removal unconfirmed")
	}
	if _, err = os.Lstat(r.Home); !os.IsNotExist(err) {
		return fmt.Errorf("managed home removal unconfirmed")
	}
	if err := cleanupPrivateGroup(ctx, r); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Dir(r.Home))
}

func cleanupPrivateGroup(ctx context.Context, r *runtimeState) error {
	out, err := exec.CommandContext(ctx, "getent", "group", r.User).Output()
	if e, ok := err.(*exec.ExitError); ok && e.ExitCode() == 2 {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot inspect managed group")
	}
	fields := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(fields) != 4 || fields[0] != r.User || r.GID == "" || fields[2] != r.GID || fields[3] != "" {
		return fmt.Errorf("managed group ownership mismatch")
	}
	if err := exec.CommandContext(ctx, "groupdel", r.User).Run(); err != nil {
		return fmt.Errorf("managed group removal pending")
	}
	err = exec.CommandContext(ctx, "getent", "group", r.User).Run()
	if e, ok := err.(*exec.ExitError); ok && e.ExitCode() == 2 {
		return nil
	}
	return fmt.Errorf("managed group removal unconfirmed")
}
func writeEndpoint(r *runtimeState, c endpointConfig) error {
	uid, err := strconv.Atoi(r.UID)
	if err != nil || uid <= 0 {
		return fmt.Errorf("invalid endpoint owner")
	}
	fields, err := account(context.Background(), r)
	if err != nil || fields == nil {
		return fmt.Errorf("endpoint account unavailable")
	}
	gid, err := strconv.Atoi(fields[3])
	if err != nil {
		return err
	}
	// Publish only after ownership and readability are installed: the endpoint
	// continuously reads this file and must never observe a root-only replacement.
	return writeJSONWithCreate(r.Endpoint, c, 0440, func(dir, pattern string) (*os.File, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		if err = f.Chown(0, gid); err != nil {
			f.Close()
			os.Remove(f.Name())
			return nil, err
		}
		return f, nil
	})
}
