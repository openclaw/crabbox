//go:build darwin && arm64

package applevmhelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunStartCleansUpDaemonAndInstanceDirectoryOnReadinessTimeout(t *testing.T) {
	stateRoot := t.TempDir()
	name := "timeout-cleanup"
	pidFile := filepath.Join(t.TempDir(), "helper.pid")
	helperPath := filepath.Join(t.TempDir(), "fake-helper")
	helperScript := `#!/bin/sh
dd bs=1 count=1 <&3 >/dev/null 2>&1 || exit 24
printf '%s\n' "$$" > ` + strconv.Quote(pidFile) + `
printf 'fake helper waiting for readiness\n'
trap 'exit 0' TERM INT
while :; do sleep 1; done
`
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o755); err != nil {
		t.Fatalf("write fake helper: %v", err)
	}
	originalPrepare := prepareInstanceAssetsFunc
	originalEnsureVMD := ensureVMDFunc
	originalProcessStartTime := processStartTime
	originalWriteMetadata := writeMetadataFunc
	originalReadyTimeout := runStartReadyTimeout
	originalStartPoll := runStartPollInterval
	originalTerminateGrace := terminateInstanceGraceTime
	originalTerminatePoll := terminateInstancePollTime
	t.Cleanup(func() {
		prepareInstanceAssetsFunc = originalPrepare
		ensureVMDFunc = originalEnsureVMD
		processStartTime = originalProcessStartTime
		writeMetadataFunc = originalWriteMetadata
		runStartReadyTimeout = originalReadyTimeout
		runStartPollInterval = originalStartPoll
		terminateInstanceGraceTime = originalTerminateGrace
		terminateInstancePollTime = originalTerminatePoll
	})

	prepareInstanceAssetsFunc = func(_ context.Context, cfg startConfig) (Instance, error) {
		inst := cfg.Instance
		inst.SourceImage = cfg.Instance.Image
		inst.DiskPath = DiskPath(cfg.StateRoot, inst.Name)
		inst.SeedPath = SeedPath(cfg.StateRoot, inst.Name)
		inst.EFIVariableStorePath = EFIPath(cfg.StateRoot, inst.Name)
		inst.ConsoleLogPath = ConsoleLogPath(cfg.StateRoot, inst.Name)
		for _, path := range []string{inst.DiskPath, inst.SeedPath, inst.EFIVariableStorePath, inst.ConsoleLogPath} {
			data := []byte("test asset\n")
			if path == inst.ConsoleLogPath {
				data = []byte("test asset\n\x1b]0;owned\x07\rmoved")
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				return Instance{}, err
			}
		}
		return inst, nil
	}
	ensureVMDFunc = func(string) (string, error) { return helperPath, nil }
	processStartTime = func(pid int) (string, error) { return strconv.Itoa(pid) + "-start", nil }
	runStartReadyTimeout = time.Second
	runStartPollInterval = 5 * time.Millisecond
	terminateInstanceGraceTime = 500 * time.Millisecond
	terminateInstancePollTime = 5 * time.Millisecond

	err := runStart([]string{
		"--state-root", stateRoot,
		"--name", name,
		"--lease-id", "lease-test",
		"--slug", "my-app",
		"--image-request-stdin",
		"--ssh-user", "alice",
		"--ssh-public-key", "ssh-ed25519 AAAATEST alice@example.com",
		"--work-root", "/workspace",
		"--cpus", "2",
		"--memory-mib", "2048",
		"--disk-gib", "16",
	}, strings.NewReader(`{"image":"test.img"}`), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for helper daemon to report readiness") {
		t.Fatalf("runStart error = %v, want readiness timeout", err)
	}
	if !strings.Contains(err.Error(), "console.log tail") || !strings.Contains(err.Error(), "test asset") {
		t.Fatalf("runStart error missing startup diagnostics: %v", err)
	}
	if strings.ContainsAny(err.Error(), "\x1b\x07\r") || !strings.Contains(err.Error(), `\x1b]0;owned\x07\x0dmoved`) {
		t.Fatalf("runStart error exposes terminal controls: %q", err)
	}

	pidData, readErr := os.ReadFile(pidFile)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("read helper pid: %v", readErr)
	}
	if readErr == nil {
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(pidData)))
		if parseErr != nil {
			t.Fatalf("parse helper pid %q: %v", string(pidData), parseErr)
		}
		if err := waitForDeadPID(pid, 2*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if _, statErr := os.Stat(InstanceDir(stateRoot, name)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("instance directory stat error = %v, want os.ErrNotExist", statErr)
	}
}

func TestRunStartRejectsUnsafeWorkRoot(t *testing.T) {
	err := runStart([]string{
		"--state-root", t.TempDir(),
		"--name", "unsafe-work-root",
		"--lease-id", "lease-test",
		"--slug", "my-app",
		"--image-request-stdin",
		"--ssh-user", "alice",
		"--ssh-public-key", "ssh-ed25519 AAAATEST alice@example.com",
		"--work-root", "/work/$(touch)",
		"--cpus", "2",
		"--memory-mib", "2048",
		"--disk-gib", "16",
	}, strings.NewReader(`{"image":"test.img"}`), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "safe absolute POSIX path") {
		t.Fatalf("runStart error=%v, want unsafe work root rejection", err)
	}
}

func TestRunStartRejectsMemoryBelowMinimum(t *testing.T) {
	err := runStart([]string{
		"--state-root", t.TempDir(),
		"--name", "low-memory",
		"--lease-id", "lease-test",
		"--slug", "my-app",
		"--image-request-stdin",
		"--ssh-user", "alice",
		"--ssh-public-key", "ssh-ed25519 AAAATEST alice@example.com",
		"--work-root", "/work",
		"--cpus", "2",
		"--memory-mib", "512",
		"--disk-gib", "16",
	}, strings.NewReader(`{"image":"test.img"}`), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "at least 1024") {
		t.Fatalf("runStart error=%v, want memory minimum rejection", err)
	}
}

func TestListMetadataReturnsAgedMetadataLessDirectoryForCleanup(t *testing.T) {
	root := t.TempDir()
	name := "partial-instance"
	dir := InstanceDir(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-metadataLessStaleAfter - time.Minute)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}

	instances, err := listMetadata(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 || instances[0].Name != name || instances[0].Status != StatusStopped {
		t.Fatalf("instances=%+v", instances)
	}
	if !strings.Contains(instances[0].Error, "missing instance metadata") {
		t.Fatalf("instance error=%q", instances[0].Error)
	}
}

func TestListMetadataDoesNotSynthesizeMalformedMetadata(t *testing.T) {
	root := t.TempDir()
	name := "malformed-instance"
	dir := InstanceDir(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(MetadataPath(root, name), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-metadataLessStaleAfter - time.Minute)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}

	instances, err := listMetadata(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 0 {
		t.Fatalf("malformed metadata synthesized instances=%+v", instances)
	}
}

func TestListMetadataHidesFreshMetadataLessDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(InstanceDir(root, "active-preparation"), 0o700); err != nil {
		t.Fatal(err)
	}
	instances, err := listMetadata(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 0 {
		t.Fatalf("fresh partial instances=%+v", instances)
	}
}

func TestListMetadataReportsActivePreparationRegardlessOfAge(t *testing.T) {
	root := t.TempDir()
	name := "active-long-preparation"
	if err := os.MkdirAll(InstanceDir(root, name), 0o700); err != nil {
		t.Fatal(err)
	}
	expected := Instance{
		Name:      name,
		LeaseID:   "cbx_preparing123",
		Slug:      "preparing",
		Status:    StatusStarting,
		Image:     "local:test.img",
		SSHUser:   "alice",
		WorkRoot:  "/workspace",
		CPUs:      2,
		MemoryMiB: 2048,
		DiskGiB:   16,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := writePreparationMarker(root, name, expected); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-metadataLessStaleAfter - time.Hour)
	if err := os.Chtimes(InstanceDir(root, name), old, old); err != nil {
		t.Fatal(err)
	}

	instances, err := listMetadata(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 {
		t.Fatalf("active preparation instances=%+v", instances)
	}
	got := instances[0]
	if got.Name != expected.Name || got.LeaseID != expected.LeaseID || got.Slug != expected.Slug || got.Status != StatusStarting {
		t.Fatalf("active preparation instance=%+v", got)
	}
	if got.PID != 0 || got.PIDStartedAt != "" || got.SSHHost != "" || got.SSHPort != 0 {
		t.Fatalf("active preparation exposed premature runtime endpoint=%+v", got)
	}
}

func TestRunStartReportsHelperExitWithoutWaitingForReadinessTimeout(t *testing.T) {
	stateRoot := t.TempDir()
	name := "early-exit"
	helperPath := filepath.Join(t.TempDir(), "fake-helper")
	helperScript := `#!/bin/sh
dd bs=1 count=1 <&3 >/dev/null 2>&1 || exit 24
printf 'helper identity setup failed\n'
exit 23
`
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o755); err != nil {
		t.Fatalf("write fake helper: %v", err)
	}

	originalPrepare := prepareInstanceAssetsFunc
	originalEnsureVMD := ensureVMDFunc
	originalStartPoll := runStartPollInterval
	t.Cleanup(func() {
		prepareInstanceAssetsFunc = originalPrepare
		ensureVMDFunc = originalEnsureVMD
		runStartPollInterval = originalStartPoll
	})
	prepareInstanceAssetsFunc = func(_ context.Context, cfg startConfig) (Instance, error) {
		inst := cfg.Instance
		inst.SourceImage = cfg.Instance.Image
		inst.DiskPath = DiskPath(cfg.StateRoot, inst.Name)
		inst.SeedPath = SeedPath(cfg.StateRoot, inst.Name)
		inst.EFIVariableStorePath = EFIPath(cfg.StateRoot, inst.Name)
		inst.ConsoleLogPath = ConsoleLogPath(cfg.StateRoot, inst.Name)
		return inst, nil
	}
	ensureVMDFunc = func(string) (string, error) { return helperPath, nil }
	runStartPollInterval = 5 * time.Millisecond

	err := runStart([]string{
		"--state-root", stateRoot,
		"--name", name,
		"--lease-id", "lease-test",
		"--slug", "my-app",
		"--image-request-stdin",
		"--ssh-user", "alice",
		"--ssh-public-key", "ssh-ed25519 AAAATEST alice@example.com",
		"--work-root", "/workspace",
		"--cpus", "2",
		"--memory-mib", "2048",
		"--disk-gib", "16",
		"--ready-timeout", "15s",
	}, strings.NewReader(`{"image":"test.img"}`), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil ||
		(!strings.Contains(err.Error(), "helper daemon exited before the VM reached running state") &&
			!strings.Contains(err.Error(), "apple-vm helper stopped before reporting readiness")) {
		t.Fatalf("runStart error=%v, want early helper exit", err)
	}
	if strings.Contains(err.Error(), "helper daemon exited before the VM reached running state") &&
		!strings.Contains(err.Error(), "exit status 23") {
		t.Fatalf("runStart error missing process exit detail: %v", err)
	}
	if !strings.Contains(err.Error(), "helper identity setup failed") {
		t.Fatalf("runStart error missing exit detail: %v", err)
	}
	if _, statErr := os.Stat(InstanceDir(stateRoot, name)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("instance directory stat error=%v, want os.ErrNotExist", statErr)
	}
}

func TestRunStartStopsUnauthorizedChildWhenIdentityMetadataFails(t *testing.T) {
	stateRoot := t.TempDir()
	name := "identity-write-failure"
	helperPath := filepath.Join(t.TempDir(), "fake-helper")
	helperScript := `#!/bin/sh
dd bs=1 count=1 <&3 >/dev/null 2>&1 || exit 24
while :; do sleep 1; done
`
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o755); err != nil {
		t.Fatalf("write fake helper: %v", err)
	}

	originalPrepare := prepareInstanceAssetsFunc
	originalEnsureVMD := ensureVMDFunc
	originalProcessStartTime := processStartTime
	originalWriteMetadata := writeMetadataFunc
	originalTerminateGrace := terminateInstanceGraceTime
	originalTerminatePoll := terminateInstancePollTime
	t.Cleanup(func() {
		prepareInstanceAssetsFunc = originalPrepare
		ensureVMDFunc = originalEnsureVMD
		processStartTime = originalProcessStartTime
		writeMetadataFunc = originalWriteMetadata
		terminateInstanceGraceTime = originalTerminateGrace
		terminateInstancePollTime = originalTerminatePoll
	})
	prepareInstanceAssetsFunc = func(_ context.Context, cfg startConfig) (Instance, error) {
		return cfg.Instance, nil
	}
	ensureVMDFunc = func(string) (string, error) { return helperPath, nil }
	capturedPID := 0
	processStartTime = func(pid int) (string, error) {
		capturedPID = pid
		return strconv.Itoa(pid) + "-start", nil
	}
	writeMetadataFunc = func(string, Instance) error {
		return errors.New("injected identity metadata failure")
	}
	terminateInstanceGraceTime = 500 * time.Millisecond
	terminateInstancePollTime = 5 * time.Millisecond

	err := runStart([]string{
		"--state-root", stateRoot,
		"--name", name,
		"--lease-id", "lease-test",
		"--slug", "my-app",
		"--image-request-stdin",
		"--ssh-user", "alice",
		"--ssh-public-key", "ssh-ed25519 AAAATEST alice@example.com",
		"--work-root", "/workspace",
		"--cpus", "2",
		"--memory-mib", "2048",
		"--disk-gib", "16",
	}, strings.NewReader(`{"image":"test.img"}`), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "injected identity metadata failure") {
		t.Fatalf("runStart error=%v, want identity metadata failure", err)
	}
	if capturedPID <= 0 {
		t.Fatal("helper process identity was not inspected")
	}
	if err := waitForDeadPID(capturedPID, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(InstanceDir(stateRoot, name)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("instance directory stat error=%v, want os.ErrNotExist", statErr)
	}
}

func TestAuthorizeStartedHelperPersistsIdentityBeforeSignal(t *testing.T) {
	stateRoot := t.TempDir()
	name := "authorized-start"
	mustCreateInstanceDir(t, stateRoot, name)
	inst := Instance{
		Name:      name,
		PID:       os.Getpid(),
		Status:    StatusStarting,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	originalProcessStartTime := processStartTime
	t.Cleanup(func() { processStartTime = originalProcessStartTime })
	processStartTime = func(pid int) (string, error) {
		if pid != os.Getpid() {
			t.Fatalf("processStartTime pid=%d want %d", pid, os.Getpid())
		}
		return "authorized-start-time", nil
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()

	got, err := authorizeStartedHelper(stateRoot, name, inst, writer)
	if err != nil {
		t.Fatal(err)
	}
	var marker [1]byte
	if _, err := io.ReadFull(reader, marker[:]); err != nil {
		t.Fatal(err)
	}
	if marker[0] != startupAuthorizationByte {
		t.Fatalf("startup marker=%x want %x", marker[0], startupAuthorizationByte)
	}
	persisted, err := readMetadata(MetadataPath(stateRoot, name))
	if err != nil {
		t.Fatal(err)
	}
	if got.PIDStartedAt != "authorized-start-time" || persisted.PID != os.Getpid() || persisted.PIDStartedAt != "authorized-start-time" {
		t.Fatalf("authorized=%+v persisted=%+v", got, persisted)
	}
}

func TestRunDeleteRemovesMetadataLessInstanceDirectory(t *testing.T) {
	stateRoot := t.TempDir()
	name := "partial-instance"
	mustCreateInstanceDir(t, stateRoot, name)
	var stdout bytes.Buffer
	if err := runDelete([]string{"--state-root", stateRoot, "--name", name}, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var response DeleteResponse
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Deleted || response.Instance.Name != name {
		t.Fatalf("delete response=%+v", response)
	}
	if _, err := os.Stat(InstanceDir(stateRoot, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instance directory stat error=%v, want os.ErrNotExist", err)
	}
}

func TestRunDeleteRejectsActivePreparation(t *testing.T) {
	stateRoot := t.TempDir()
	name := "active-preparation"
	mustCreateInstanceDir(t, stateRoot, name)
	inst := Instance{
		Name:      name,
		Status:    StatusStarting,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := writeMetadata(MetadataPath(stateRoot, name), inst); err != nil {
		t.Fatal(err)
	}
	if err := writePreparationMarker(stateRoot, name, inst); err != nil {
		t.Fatal(err)
	}

	err := runDelete([]string{"--state-root", stateRoot, "--name", name}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "still starting") {
		t.Fatalf("runDelete error=%v, want active startup rejection", err)
	}
	if _, err := os.Stat(InstanceDir(stateRoot, name)); err != nil {
		t.Fatalf("active instance directory removed: %v", err)
	}
}

func TestReadProcessStartTimeIsTimezoneInvariant(t *testing.T) {
	t.Setenv("TZ", "Pacific/Honolulu")
	first, err := readProcessStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("TZ", "Asia/Tokyo"); err != nil {
		t.Fatal(err)
	}
	second, err := readProcessStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("process start time changed with TZ: %q != %q", first, second)
	}
	if !strings.HasPrefix(first, processIdentityPrefix) {
		t.Fatalf("process start identity=%q, want prefix %q", first, processIdentityPrefix)
	}
	parts := strings.Split(strings.TrimPrefix(first, processIdentityPrefix), ".")
	if len(parts) != 2 || len(parts[1]) != 6 {
		t.Fatalf("process start identity=%q, want seconds.microseconds", first)
	}
}

func TestLegacyProcessIdentityMigratesAfterVerifiedMatch(t *testing.T) {
	startedAt := time.Date(2026, time.June, 11, 16, 53, 13, 123456000, time.UTC)
	kernelIdentity := processIdentityPrefix + strconv.FormatInt(startedAt.Unix(), 10) + ".123456"
	inst := Instance{
		Name:         "legacy-identity",
		PID:          os.Getpid(),
		PIDStartedAt: startedAt.Format("Mon Jan _2 15:04:05 2006"),
		Status:       StatusRunning,
	}
	originalProcessStartTime := processStartTime
	originalProcessArguments := processArguments
	t.Cleanup(func() {
		processStartTime = originalProcessStartTime
		processArguments = originalProcessArguments
	})
	processStartTime = func(pid int) (string, error) {
		if pid != inst.PID {
			t.Fatalf("processStartTime pid=%d want %d", pid, inst.PID)
		}
		return kernelIdentity, nil
	}
	processArguments = func(pid int) ([]string, error) {
		if pid != inst.PID {
			t.Fatalf("processArguments pid=%d want %d", pid, inst.PID)
		}
		return []string{
			"/tmp/crabbox-apple-vm-helper-0123456789abcdef",
			"serve",
			"--state-root", "/tmp/apple-vm",
			"--name", inst.Name,
		}, nil
	}

	if matches, err := processIdentityMatches(inst); err != nil || !matches {
		t.Fatalf("legacy identity matches=%v err=%v", matches, err)
	}
	got := normalizeInstance(inst)
	if got.Status != StatusRunning || got.PID != inst.PID || got.PIDStartedAt != inst.PIDStartedAt {
		t.Fatalf("verified legacy identity should remain running: %+v", got)
	}
	migrated := migrateLegacyProcessIdentity(inst)
	if migrated.PIDStartedAt != kernelIdentity {
		t.Fatalf("migrated identity=%q want %q", migrated.PIDStartedAt, kernelIdentity)
	}
}

func TestLegacyProcessIdentityRejectsUnexpectedProcessArguments(t *testing.T) {
	startedAt := time.Date(2026, time.June, 11, 16, 53, 13, 123456000, time.UTC)
	inst := Instance{
		Name:         "legacy-identity",
		PID:          os.Getpid(),
		PIDStartedAt: startedAt.Format("Mon Jan _2 15:04:05 2006"),
		Status:       StatusRunning,
	}
	originalProcessStartTime := processStartTime
	originalProcessArguments := processArguments
	t.Cleanup(func() {
		processStartTime = originalProcessStartTime
		processArguments = originalProcessArguments
	})
	processStartTime = func(int) (string, error) {
		return processIdentityPrefix + strconv.FormatInt(startedAt.Unix(), 10) + ".123456", nil
	}
	processArguments = func(int) ([]string, error) {
		return []string{"/usr/bin/sleep", "60"}, nil
	}

	if matches, err := processIdentityMatches(inst); err != nil || matches {
		t.Fatalf("unexpected process matches=%v err=%v", matches, err)
	}
	got := normalizeInstance(inst)
	if got.Status != StatusStopped || got.PID != 0 || got.PIDStartedAt != "" {
		t.Fatalf("unexpected process should be treated as stale: %+v", got)
	}
}

func TestParseProcessArgumentsStopsBeforeEnvironment(t *testing.T) {
	raw := []byte{3, 0, 0, 0}
	raw = append(raw, []byte("/tmp/helper\x00\x00/tmp/helper\x00serve\x00--name\x00SECRET=value\x00")...)
	args, err := parseProcessArguments(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/tmp/helper", "serve", "--name"}
	if !slices.Equal(args, want) {
		t.Fatalf("args=%q want %q", args, want)
	}
}

func TestLegacyProcessStartSecondsSupportsLocalAndUTC(t *testing.T) {
	local := time.FixedZone("test-local", 2*60*60)
	value := "Thu Jun 11 16:53:13 2026"

	seconds, err := legacyProcessStartSecondsIn(value, local)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{
		time.Date(2026, time.June, 11, 16, 53, 13, 0, local).Unix(),
		time.Date(2026, time.June, 11, 16, 53, 13, 0, time.UTC).Unix(),
	}
	for _, expected := range want {
		if !slices.Contains(seconds, expected) {
			t.Fatalf("seconds=%v missing %d", seconds, expected)
		}
	}
}

func TestTerminateInstanceSkipsSignalWhenPIDIdentityMismatches(t *testing.T) {
	root := t.TempDir()
	name := "stale-pid"
	mustCreateInstanceDir(t, root, name)
	cmd := startSleepProcess(t)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	originalProcessStartTime := processStartTime
	t.Cleanup(func() { processStartTime = originalProcessStartTime })
	processStartTime = func(pid int) (string, error) {
		if pid != cmd.Process.Pid {
			t.Fatalf("processStartTime pid=%d want %d", pid, cmd.Process.Pid)
		}
		return "actual-start", nil
	}

	err := terminateInstance(root, name, Instance{PID: cmd.Process.Pid, PIDStartedAt: "old-start"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(InstanceDir(root, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instance directory stat error=%v, want os.ErrNotExist", err)
	}
	if !pidAlive(cmd.Process.Pid) {
		t.Fatal("identity mismatch should not signal the live process")
	}
}

func TestTerminateInstancePreservesStateWhenIdentityProbeFails(t *testing.T) {
	root := t.TempDir()
	name := "identity-probe-failure"
	mustCreateInstanceDir(t, root, name)
	cmd := startSleepProcess(t)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	originalProcessStartTime := processStartTime
	t.Cleanup(func() { processStartTime = originalProcessStartTime })
	processStartTime = func(pid int) (string, error) {
		if pid != cmd.Process.Pid {
			t.Fatalf("processStartTime pid=%d want %d", pid, cmd.Process.Pid)
		}
		return "", errors.New("transient ps failure")
	}

	err := terminateInstance(root, name, Instance{PID: cmd.Process.Pid, PIDStartedAt: "recorded-start"})
	if err == nil || !strings.Contains(err.Error(), "transient ps failure") {
		t.Fatalf("terminateInstance error=%v, want process identity probe failure", err)
	}
	if _, err := os.Stat(InstanceDir(root, name)); err != nil {
		t.Fatalf("instance state should be preserved: %v", err)
	}
	if !pidAlive(cmd.Process.Pid) {
		t.Fatal("identity probe failure should not signal the live process")
	}
}

func TestTerminateInstanceSignalsOnlyMatchingPIDIdentity(t *testing.T) {
	root := t.TempDir()
	name := "matching-pid"
	mustCreateInstanceDir(t, root, name)
	cmd := startSleepProcess(t)
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	originalProcessStartTime := processStartTime
	originalTerminateGrace := terminateInstanceGraceTime
	originalTerminatePoll := terminateInstancePollTime
	t.Cleanup(func() {
		processStartTime = originalProcessStartTime
		terminateInstanceGraceTime = originalTerminateGrace
		terminateInstancePollTime = originalTerminatePoll
	})
	processStartTime = func(pid int) (string, error) {
		if pid != cmd.Process.Pid {
			t.Fatalf("processStartTime pid=%d want %d", pid, cmd.Process.Pid)
		}
		return "matching-start", nil
	}
	terminateInstanceGraceTime = time.Second
	terminateInstancePollTime = 5 * time.Millisecond

	err := terminateInstance(root, name, Instance{PID: cmd.Process.Pid, PIDStartedAt: "matching-start"})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitForProcessExit(cmd, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	waited = true
	if _, err := os.Stat(InstanceDir(root, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instance directory stat error=%v, want os.ErrNotExist", err)
	}
}

func TestTerminateInstanceRechecksIdentityBeforeKill(t *testing.T) {
	root := t.TempDir()
	name := "pid-reused-during-stop"
	mustCreateInstanceDir(t, root, name)

	originalProcessStartTime := processStartTime
	originalProcessAlive := processAlive
	originalSignalProcess := signalProcess
	originalTerminateGrace := terminateInstanceGraceTime
	originalTerminatePoll := terminateInstancePollTime
	t.Cleanup(func() {
		processStartTime = originalProcessStartTime
		processAlive = originalProcessAlive
		signalProcess = originalSignalProcess
		terminateInstanceGraceTime = originalTerminateGrace
		terminateInstancePollTime = originalTerminatePoll
	})

	identityChecks := 0
	processStartTime = func(pid int) (string, error) {
		if pid != 4242 {
			t.Fatalf("processStartTime pid=%d want 4242", pid)
		}
		identityChecks++
		if identityChecks == 1 {
			return "original-start", nil
		}
		return "replacement-start", nil
	}
	processAlive = func(int) bool { return true }
	var signals []os.Signal
	signalProcess = func(pid int, signal os.Signal) error {
		if pid != 4242 {
			t.Fatalf("signal pid=%d want 4242", pid)
		}
		signals = append(signals, signal)
		return nil
	}
	terminateInstanceGraceTime = time.Second
	terminateInstancePollTime = time.Millisecond

	err := terminateInstance(root, name, Instance{PID: 4242, PIDStartedAt: "original-start"})
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 1 || signals[0] != syscall.SIGTERM {
		t.Fatalf("signals=%v, want SIGTERM only", signals)
	}
	if identityChecks < 2 {
		t.Fatalf("identity checks=%d, want revalidation after SIGTERM", identityChecks)
	}
	if _, err := os.Stat(InstanceDir(root, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instance directory stat error=%v, want os.ErrNotExist", err)
	}
}

func TestTerminateStartedHelperRechecksIdentityBeforeKill(t *testing.T) {
	originalProcessStartTime := processStartTime
	originalProcessAlive := processAlive
	originalSignalProcess := signalProcess
	originalTerminateGrace := terminateInstanceGraceTime
	originalTerminatePoll := terminateInstancePollTime
	t.Cleanup(func() {
		processStartTime = originalProcessStartTime
		processAlive = originalProcessAlive
		signalProcess = originalSignalProcess
		terminateInstanceGraceTime = originalTerminateGrace
		terminateInstancePollTime = originalTerminatePoll
	})

	identityChecks := 0
	processAlive = func(int) bool { return true }
	processStartTime = func(pid int) (string, error) {
		if pid != 4242 {
			t.Fatalf("processStartTime pid=%d want 4242", pid)
		}
		identityChecks++
		if identityChecks == 1 {
			return "original-start", nil
		}
		return "replacement-start", nil
	}
	var signals []os.Signal
	signalProcess = func(pid int, signal os.Signal) error {
		if pid != 4242 {
			t.Fatalf("signal pid=%d want 4242", pid)
		}
		signals = append(signals, signal)
		return nil
	}
	terminateInstanceGraceTime = time.Second
	terminateInstancePollTime = time.Millisecond

	if err := terminateStartedHelper(Instance{PID: 4242, PIDStartedAt: "original-start"}); err != nil {
		t.Fatal(err)
	}

	if len(signals) != 1 || signals[0] != syscall.SIGTERM {
		t.Fatalf("signals=%v, want SIGTERM only", signals)
	}
	if identityChecks < 2 {
		t.Fatalf("identity checks=%d, want revalidation after SIGTERM", identityChecks)
	}
}

func TestCleanupStartedHelperPreservesStateOnIdentityProbeFailure(t *testing.T) {
	root := t.TempDir()
	name := "identity-probe-failure"
	instanceRoot := InstanceDir(root, name)
	mustCreateInstanceDir(t, root, name)

	originalProcessStartTime := processStartTime
	originalProcessAlive := processAlive
	originalSignalProcess := signalProcess
	t.Cleanup(func() {
		processStartTime = originalProcessStartTime
		processAlive = originalProcessAlive
		signalProcess = originalSignalProcess
	})

	processAlive = func(pid int) bool {
		if pid != 4242 {
			t.Fatalf("processAlive pid=%d want 4242", pid)
		}
		return true
	}
	processStartTime = func(pid int) (string, error) {
		if pid != 4242 {
			t.Fatalf("processStartTime pid=%d want 4242", pid)
		}
		return "", errors.New("transient ps failure")
	}
	signalProcess = func(int, os.Signal) error {
		t.Fatal("must not signal a process with unverified identity")
		return nil
	}

	err := cleanupStartedHelper(Instance{PID: 4242, PIDStartedAt: "original-start"}, instanceRoot)
	if err == nil || !strings.Contains(err.Error(), "transient ps failure") {
		t.Fatalf("cleanup error=%v, want identity probe failure", err)
	}
	if _, err := os.Stat(instanceRoot); err != nil {
		t.Fatalf("instance state was removed after uncertain termination: %v", err)
	}
}

func TestCleanupUnauthorizedStartedHelperRemovesStateAfterConfirmedExit(t *testing.T) {
	root := t.TempDir()
	name := "unauthorized-exit"
	instanceRoot := InstanceDir(root, name)
	mustCreateInstanceDir(t, root, name)
	waitCh := make(chan error, 1)
	waitCh <- errors.New("exit status 24")

	if err := cleanupUnauthorizedStartedHelper(Instance{PID: 4242}, instanceRoot, waitCh); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(instanceRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instance state remains after confirmed child exit: %v", err)
	}
}

func TestCleanupUnauthorizedStartedHelperPreservesStateWithoutIdentityOrExit(t *testing.T) {
	root := t.TempDir()
	name := "unauthorized-timeout"
	instanceRoot := InstanceDir(root, name)
	mustCreateInstanceDir(t, root, name)

	originalTerminateGrace := terminateInstanceGraceTime
	t.Cleanup(func() { terminateInstanceGraceTime = originalTerminateGrace })
	terminateInstanceGraceTime = time.Millisecond

	err := cleanupUnauthorizedStartedHelper(Instance{PID: 4242}, instanceRoot, make(chan error))
	if err == nil || !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("cleanup error=%v, want child exit timeout", err)
	}
	if _, err := os.Stat(instanceRoot); err != nil {
		t.Fatalf("instance state was removed without confirmed child exit: %v", err)
	}
}

func TestNormalizeInstanceMarksReusedPIDStopped(t *testing.T) {
	originalProcessStartTime := processStartTime
	t.Cleanup(func() { processStartTime = originalProcessStartTime })
	processStartTime = func(pid int) (string, error) {
		if pid != os.Getpid() {
			t.Fatalf("processStartTime pid=%d want %d", pid, os.Getpid())
		}
		return "new-start", nil
	}

	inst := normalizeInstance(Instance{
		PID:          os.Getpid(),
		PIDStartedAt: "old-start",
		Status:       StatusRunning,
	})
	if inst.Status != StatusStopped || inst.PID != 0 || inst.PIDStartedAt != "" {
		t.Fatalf("normalized instance=%+v", inst)
	}
}

func TestNormalizeInstanceKeepsRunningOnIdentityProbeFailure(t *testing.T) {
	originalProcessStartTime := processStartTime
	t.Cleanup(func() { processStartTime = originalProcessStartTime })
	processStartTime = func(int) (string, error) {
		return "", errors.New("transient ps failure")
	}

	inst := normalizeInstance(Instance{
		PID:          os.Getpid(),
		PIDStartedAt: "known-start",
		Status:       StatusRunning,
	})
	if inst.Status != StatusRunning || inst.PID != os.Getpid() || inst.PIDStartedAt != "known-start" {
		t.Fatalf("normalized instance=%+v", inst)
	}
}

func TestNormalizeInstanceStopsStalePIDlessStartup(t *testing.T) {
	now := time.Now().UTC()
	inst := normalizeInstance(Instance{
		Status:    StatusStarting,
		CreatedAt: now.Add(-pidlessStartupStaleAfter - time.Minute),
		UpdatedAt: now.Add(-pidlessStartupStaleAfter - time.Minute),
	})
	if inst.Status != StatusStopped || inst.PID != 0 {
		t.Fatalf("normalized instance=%+v", inst)
	}
}

func TestNormalizeInstanceKeepsFreshPIDlessStartup(t *testing.T) {
	now := time.Now().UTC()
	inst := normalizeInstance(Instance{
		Status:    StatusStarting,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if inst.Status != StatusStarting || inst.PID != 0 {
		t.Fatalf("normalized instance=%+v", inst)
	}
}

func TestHelperDaemonEnvExcludesCallerCredentials(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	env := helperDaemonEnv()
	joined := strings.Join(env, "\n")
	for _, name := range []string{"GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY"} {
		if strings.Contains(joined, name+"=") {
			t.Fatalf("daemon environment includes %s", name)
		}
	}
	if !strings.Contains(joined, "PATH=/usr/bin:/bin:/usr/sbin:/sbin") {
		t.Fatalf("daemon environment missing deterministic PATH: %q", joined)
	}
}

func TestHandleStartReadinessMetadataStoppedBeforeReadiness(t *testing.T) {
	root := t.TempDir()
	name := "stopped-before-ready"
	pid := unusedPID(t)
	mustCreateInstanceDir(t, root, name)

	handled, err := handleStartReadinessMetadata(root, name, Instance{
		Name:      name,
		Status:    StatusStopped,
		PID:       pid,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}, Instance{PID: pid}, &bytes.Buffer{})

	if !handled {
		t.Fatal("expected stopped status to be handled")
	}
	if err == nil {
		t.Fatal("expected stopped-before-readiness error")
	}
	if got := err.Error(); !strings.Contains(got, "apple-vm helper stopped before reporting readiness (status=stopped)") {
		t.Fatalf("expected stopped-before-readiness error, got %q", got)
	}
	if strings.Contains(err.Error(), "helper daemon exited before the VM reached running state") {
		t.Fatalf("expected specific stopped error, got misleading daemon-exited error: %v", err)
	}
	if _, err := os.Stat(InstanceDir(root, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected startup instance directory to be removed, stat err=%v", err)
	}
}

func TestHandleStartReadinessMetadataStoppingDeadPIDCleansInstanceDir(t *testing.T) {
	root := t.TempDir()
	name := "stopping-dead"
	pid := unusedPID(t)
	mustCreateInstanceDir(t, root, name)

	handled, err := handleStartReadinessMetadata(root, name, Instance{
		Name:      name,
		Status:    StatusStopping,
		PID:       pid,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}, Instance{PID: pid}, &bytes.Buffer{})

	if !handled {
		t.Fatal("expected stopping status with a dead PID to be handled")
	}
	if err == nil {
		t.Fatal("expected stopped-before-readiness error")
	}
	if got := err.Error(); !strings.Contains(got, "apple-vm helper stopped before reporting readiness (status=stopped)") {
		t.Fatalf("expected normalized stopped-before-readiness error, got %q", got)
	}
	if _, err := os.Stat(InstanceDir(root, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected startup instance directory to be removed, stat err=%v", err)
	}
}

func waitForDeadPID(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return errors.New("helper process remained alive after runStart timeout cleanup")
}

func waitForProcessExit(cmd *exec.Cmd, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return errors.New("helper process remained alive after matching identity cleanup")
	case <-done:
		return nil
	}
}

func startSleepProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep process: %v", err)
	}
	return cmd
}

func mustCreateInstanceDir(t *testing.T, root, name string) {
	t.Helper()
	if err := os.MkdirAll(InstanceDir(root, name), 0o755); err != nil {
		t.Fatalf("create instance dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(InstanceDir(root, name), "sentinel"), []byte("created by start\n"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
}

func unusedPID(t *testing.T) int {
	t.Helper()
	for pid := 999999; pid > 100000; pid-- {
		if !pidAlive(pid) {
			return pid
		}
	}
	t.Fatal("failed to find an unused pid")
	return 0
}
