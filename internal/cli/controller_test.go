package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestControllerHostSupportContract(t *testing.T) {
	err := controllerHostSupported()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		if err == nil {
			t.Fatalf("controller hosting unexpectedly supported on %s", runtime.GOOS)
		}
		return
	}
	if err != nil {
		t.Fatalf("controller hosting rejected on %s: %v", runtime.GOOS, err)
	}
}

func TestControllerStateValidateIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveControllerState(path, controllerState{
		Version: controllerStateVersion,
		Workspaces: map[string]controllerWorkspaceRecord{
			"validated-box": {
				Request: controllerWorkspaceRequest{ID: "validated-box"}, Status: "stopped",
				Message: "workspace stopped", CreatedAt: now, UpdatedAt: now,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"adapter", "state", "validate", "--state-file", path}); err != nil {
		t.Fatalf("validate: %v stderr=%s", err, stderr.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("state validation modified the state file")
	}
	if !bytes.Contains(stdout.Bytes(), []byte("adapter state valid version=")) {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestControllerStateValidateRejectsIncompatibleSchema(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for name, data := range map[string]string{
		"unknown field":    `{"version":5,"workspaces":{},"unexpected":true}`,
		"old version":      `{"version":3,"workspaces":{}}`,
		"invalid record":   `{"version":5,"workspaces":{"bad-box":{"request":{"id":"bad-box","capabilities":{}},"status":"unknown","message":"bad","createdAt":"` + now + `","updatedAt":"` + now + `"}}}`,
		"HTTPS attach URL": `{"version":5,"workspaces":{"bad-box":{"request":{"id":"bad-box","capabilities":{}},"status":"stopped","attachUrl":"https://fleet.example.test/workspaces/bad-box","message":"bad","createdAt":"` + now + `","updatedAt":"` + now + `"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			err := (App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).controllerStateValidate([]string{"--state-file", path})
			if err == nil {
				t.Fatal("invalid controller state was accepted")
			}
		})
	}
}

func TestControllerPolicyLeaseSeconds(t *testing.T) {
	for _, test := range []struct {
		name  string
		value time.Duration
		want  int
		bad   bool
	}{
		{name: "disabled", value: 0, want: 0},
		{name: "four hours", value: 4 * time.Hour, want: 14400},
		{name: "fractional", value: time.Minute + time.Nanosecond, bad: true},
		{name: "too short", value: time.Second, bad: true},
		{name: "too long", value: 8 * 24 * time.Hour, bad: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := controllerPolicyLeaseSeconds("required-ttl", test.value)
			if test.bad {
				if err == nil {
					t.Fatalf("value=%s accepted as %d", test.value, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("got=%d err=%v want=%d", got, err, test.want)
			}
		})
	}
}

func TestControllerPolicyLeaseValuesRejectsIdleBeyondTTL(t *testing.T) {
	if _, _, err := controllerPolicyLeaseValues(time.Hour, 2*time.Hour); err == nil {
		t.Fatal("idle policy beyond TTL was accepted")
	}
	ttl, idle, err := controllerPolicyLeaseValues(4*time.Hour, 4*time.Hour)
	if err != nil || ttl != 14400 || idle != 14400 {
		t.Fatalf("ttl=%d idle=%d err=%v", ttl, idle, err)
	}
}

func TestControllerPolicyEnvDurationRejectsInvalidValue(t *testing.T) {
	t.Setenv("CRABBOX_ADAPTER_REQUIRED_TTL", "not-a-duration")
	if _, err := controllerPolicyEnvDuration("CRABBOX_ADAPTER_REQUIRED_TTL"); err == nil {
		t.Fatal("invalid required TTL environment value was silently disabled")
	}
	t.Setenv("CRABBOX_ADAPTER_REQUIRED_TTL", "4h")
	if got, err := controllerPolicyEnvDuration("CRABBOX_ADAPTER_REQUIRED_TTL"); err != nil || got != 4*time.Hour {
		t.Fatalf("duration=%s err=%v", got, err)
	}
}

func TestControllerPolicyFlagsOverrideInvalidEnvironment(t *testing.T) {
	for _, test := range []struct {
		name     string
		env      string
		args     []string
		wantTTL  time.Duration
		wantIdle time.Duration
	}{
		{
			name: "required ttl",
			env:  "CRABBOX_ADAPTER_REQUIRED_TTL",
			args: []string{"--required-ttl=4h"}, wantTTL: 4 * time.Hour,
		},
		{
			name: "required idle timeout",
			env:  "CRABBOX_ADAPTER_REQUIRED_IDLE_TIMEOUT",
			args: []string{"--required-idle-timeout", "30m"}, wantIdle: 30 * time.Minute,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CRABBOX_ADAPTER_REQUIRED_TTL", "")
			t.Setenv("CRABBOX_ADAPTER_REQUIRED_IDLE_TIMEOUT", "")
			t.Setenv(test.env, "not-a-duration")
			fs := newFlagSet("controller policy precedence", &bytes.Buffer{})
			ttl := fs.Duration("required-ttl", 0, "")
			idle := fs.Duration("required-idle-timeout", 0, "")
			if err := parseFlags(fs, test.args); err != nil {
				t.Fatal(err)
			}
			if err := controllerPolicyEnvDefault(fs, "required-ttl", "CRABBOX_ADAPTER_REQUIRED_TTL", ttl); err != nil {
				t.Fatalf("required TTL precedence: %v", err)
			}
			if err := controllerPolicyEnvDefault(fs, "required-idle-timeout", "CRABBOX_ADAPTER_REQUIRED_IDLE_TIMEOUT", idle); err != nil {
				t.Fatalf("required idle timeout precedence: %v", err)
			}
			if *ttl != test.wantTTL || *idle != test.wantIdle {
				t.Fatalf("ttl=%s idle=%s want ttl=%s idle=%s", *ttl, *idle, test.wantTTL, test.wantIdle)
			}
		})
	}
}

func TestControllerServePolicyFlagsOverrideInvalidEnvironment(t *testing.T) {
	if err := controllerHostSupported(); err != nil {
		t.Skip(err)
	}
	for _, test := range []struct {
		name string
		env  string
		args []string
	}{
		{name: "required ttl", env: "CRABBOX_ADAPTER_REQUIRED_TTL", args: []string{"--required-ttl=4h"}},
		{name: "required idle timeout", env: "CRABBOX_ADAPTER_REQUIRED_IDLE_TIMEOUT", args: []string{"--required-idle-timeout", "30m"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CRABBOX_ADAPTER_REQUIRED_TTL", "")
			t.Setenv("CRABBOX_ADAPTER_REQUIRED_IDLE_TIMEOUT", "")
			t.Setenv(test.env, "not-a-duration")
			missingToken := filepath.Join(t.TempDir(), "missing-token")
			args := append(append([]string{}, test.args...), "--token-file", missingToken)
			err := (App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).controllerServe(context.Background(), args)
			if err == nil {
				t.Fatal("controller unexpectedly started without a token file")
			}
			if strings.Contains(err.Error(), test.env) || !strings.Contains(err.Error(), "read adapter token file") {
				t.Fatalf("valid flag did not override invalid %s: %v", test.env, err)
			}
		})
	}
}

func TestControllerStateLockIsExclusiveAcrossProcesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("controller host is unsupported on Windows")
	}
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	readyPath := filepath.Join(dir, "ready")
	helper := exec.Command(os.Args[0], "-test.run=^TestControllerStateLockProcessHelper$")
	helper.Env = append(os.Environ(),
		"CRABBOX_TEST_CONTROLLER_LOCK_STATE="+statePath,
		"CRABBOX_TEST_CONTROLLER_LOCK_READY="+readyPath,
	)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	helperDone := false
	t.Cleanup(func() {
		if !helperDone {
			_ = helper.Process.Kill()
			_ = helper.Wait()
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatal("lock helper did not become ready")
	}
	if lock, err := acquireControllerStateLock(statePath); err == nil {
		_ = lock.Unlock()
		t.Fatal("second process acquired shared controller state lock")
	}
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	helperDone = true
	lock, err := acquireControllerStateLock(statePath)
	if err != nil {
		t.Fatalf("kernel did not release crashed controller lock: %v", err)
	}
	_ = lock.Unlock()
}

func TestControllerStateLockProcessHelper(t *testing.T) {
	statePath := os.Getenv("CRABBOX_TEST_CONTROLLER_LOCK_STATE")
	readyPath := os.Getenv("CRABBOX_TEST_CONTROLLER_LOCK_READY")
	if statePath == "" || readyPath == "" {
		return
	}
	lock, err := acquireControllerStateLock(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	if err := os.WriteFile(readyPath, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestReadControllerTokenRequiresPrivateRegularFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("controller hosting is unsupported on Windows")
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("secret-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readAdapterToken(path); err == nil {
		t.Fatal("expected broad token permissions to be rejected")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := readAdapterToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if token != "secret-token" {
		t.Fatalf("token=%q", token)
	}
	symlink := filepath.Join(t.TempDir(), "token-link")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readAdapterToken(symlink); err == nil {
		t.Fatal("expected token symlink to be rejected")
	}
}

func TestReadControllerTokenBoundsRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("controller hosting is unsupported on Windows")
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, make([]byte, (8<<10)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readAdapterToken(path); err == nil {
		t.Fatal("expected oversized token to be rejected")
	}
}
