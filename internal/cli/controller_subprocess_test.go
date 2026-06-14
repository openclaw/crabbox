package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fixedIdentityExecControllerRunner struct {
	*execControllerWorkspaceRunner
}

type confirmedAbsentCleanupTestBackend struct {
	cleanupCalls int
	cleanupErr   error
}

func (b *confirmedAbsentCleanupTestBackend) Spec() ProviderSpec {
	return ProviderSpec{Name: "external"}
}

func (b *confirmedAbsentCleanupTestBackend) CleanupConfirmedAbsentLocalState(_ context.Context, _ ConfirmedAbsentLocalCleanupRequest) error {
	b.cleanupCalls++
	return b.cleanupErr
}

func (r *fixedIdentityExecControllerRunner) ProviderIdentity(context.Context) (controllerProviderIdentity, error) {
	return controllerProviderIdentity{Route: "external", Scope: "test-provider-scope", IdempotentFixedLeaseID: true}, nil
}

func TestControllerWarmupArgsUseFixedRoutingAndCapabilities(t *testing.T) {
	runner := execControllerWorkspaceRunner{opts: execControllerRunnerOptions{
		Provider: "external",
	}}
	args := runner.warmupArgs("cbx_123456789abc", "cbx-ctl-demo-box-0000000000000000", controllerWorkspaceRequest{
		ID: "demo-box", Profile: "public-desktop", Class: "standard", ServerType: "cpu8", TTLSeconds: 3600, IdleTimeoutSeconds: 900,
		Capabilities: controllerCapabilities{Desktop: true, Browser: true, Code: true},
	})
	for _, want := range []string{
		"warmup", "--keep=true", "--lease-id", "cbx_123456789abc", "--slug", "cbx-ctl-demo-box-0000000000000000", "--provider", "external",
		"--profile", "public-desktop", "--class", "standard", "--type", "cpu8",
		"--ttl", "3600s", "--idle-timeout", "900s", "--desktop=true", "--browser=true", "--code=true",
	} {
		if !slices.Contains(args, want) {
			t.Fatalf("args=%q missing %q", args, want)
		}
	}
}

func TestExecControllerRunnerReadsProviderIdentityContract(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "crabbox")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"provider\":\"external\",\"providerScope\":\"opaque-scope\",\"idempotentLeaseId\":true,\"coordinatorRegistrationUrl\":\"https://coordinator.example.test/root\"}'\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Binary: binary, Provider: "external", StateFile: filepath.Join(t.TempDir(), "state.json")}}
	identity, err := runner.ProviderIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if identity.Route != "external" || identity.Scope != "opaque-scope" || !identity.IdempotentFixedLeaseID || identity.CoordinatorRegistrationURL != "https://coordinator.example.test/root" {
		t.Fatalf("identity=%#v", identity)
	}
}

func TestControllerWarmupArgsUseOnlyPersistedRequestProfile(t *testing.T) {
	runner := execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Provider: "external"}}
	withProfile := runner.warmupArgs("cbx_123456789abc", "stable-slug", controllerWorkspaceRequest{Profile: "persisted-profile"})
	if joined := strings.Join(withProfile, " "); !strings.Contains(joined, "--profile persisted-profile") {
		t.Fatalf("persisted profile missing from args: %q", withProfile)
	}
	withoutProfile := runner.warmupArgs("cbx_123456789abc", "stable-slug", controllerWorkspaceRequest{})
	if slices.Contains(withoutProfile, "--profile") {
		t.Fatalf("restart injected a non-persisted profile: %q", withoutProfile)
	}
}

func TestControllerChildEnvReplacesConfigAndOmitsContent(t *testing.T) {
	request := controllerWorkspaceRequest{
		ID: "demo-box", Repo: "example/app", Branch: "main", Profile: "public-desktop",
		ProviderScope: "persisted-provider-scope",
		Command:       "do not export", Prompt: "also private",
	}
	env := controllerChildEnv([]string{
		"PATH=/bin", "CRABBOX_CONFIG=old", "CRABBOX_ADAPTER_ID=ambient",
		controllerAcquireIdentityAddressEnv + "=127.0.0.1:1",
		controllerAcquireIdentityTokenEnv + "=ambient-token",
		controllerCoordinatorRegistrationExpectedEnv + "=1",
		controllerCoordinatorRegistrationURLEnv + "=https://ambient.example.test",
	}, "/safe/config.yaml", request)
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"CRABBOX_CONFIG=/safe/config.yaml",
		"CRABBOX_ADAPTER_WORKSPACE_ID=demo-box",
		controllerProcessTreeOwnedEnv + "=1",
		"CRABBOX_ADAPTER_REPO=example/app",
		"CRABBOX_ADAPTER_BRANCH=main",
		"CRABBOX_ADAPTER_PROFILE=public-desktop",
		controllerProviderScopeEnv + "=persisted-provider-scope",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "do not export") || strings.Contains(joined, "also private") || strings.Contains(joined, "CRABBOX_CONFIG=old") {
		t.Fatalf("private or replaced value leaked into child env: %s", joined)
	}
	if strings.Contains(joined, controllerAcquireIdentityAddressEnv) || strings.Contains(joined, controllerAcquireIdentityTokenEnv) {
		t.Fatalf("ambient acquire identity channel leaked into child env: %s", joined)
	}
	if strings.Contains(joined, controllerCoordinatorRegistrationExpectedEnv) || strings.Contains(joined, controllerCoordinatorRegistrationURLEnv) {
		t.Fatalf("ambient coordinator registration binding leaked into child env: %s", joined)
	}
	if strings.Contains(joined, "CRABBOX_ADAPTER_ID=ambient") {
		t.Fatalf("ambient adapter ID leaked into unbound child env: %s", joined)
	}
}

func TestControllerRunnerAddsRuntimeAdapterIdentity(t *testing.T) {
	runner := execControllerWorkspaceRunner{opts: execControllerRunnerOptions{AdapterID: "mac-lab"}}
	env := controllerChildEnvWithOverrides(
		[]string{"PATH=/bin", "CRABBOX_ADAPTER_ID=ambient"},
		"",
		controllerWorkspaceRequest{ID: "fleet-a-is-123"},
		runner.adapterChildEnv(nil),
	)
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"CRABBOX_ADAPTER_ID=mac-lab",
		"CRABBOX_ADAPTER_WORKSPACE_ID=fleet-a-is-123",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "CRABBOX_ADAPTER_ID=ambient") {
		t.Fatalf("ambient adapter id survived: %s", joined)
	}
}

func TestControllerAcquireIdentityGateWaitsForPersistenceAcknowledgment(t *testing.T) {
	identity := controllerAcquireIdentity{
		LeaseID: "cbx_123456789abc", Slug: "stable-slug", Provider: "external", ResourceID: "provider/resource-123",
	}
	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	gate, err := newControllerAcquireIdentityGate(func(got controllerAcquireIdentity) error {
		if got != identity {
			return fmt.Errorf("identity=%#v", got)
		}
		close(callbackStarted)
		<-releaseCallback
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	env := gate.environment()
	t.Setenv(controllerAcquireIdentityAddressEnv, env[controllerAcquireIdentityAddressEnv])
	t.Setenv(controllerAcquireIdentityTokenEnv, env[controllerAcquireIdentityTokenEnv])
	done := make(chan error, 1)
	go func() { done <- acknowledgeControllerAcquireIdentity(context.Background(), identity) }()
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		gate.close()
		t.Fatal("identity callback did not start")
	}
	select {
	case err := <-done:
		gate.close()
		t.Fatalf("child crossed identity gate before persistence callback completed: %v", err)
	default:
	}
	close(releaseCallback)
	select {
	case err := <-done:
		if err != nil {
			gate.close()
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		gate.close()
		t.Fatal("child did not receive identity acknowledgment")
	}
	gate.close()
	result := gate.wait()
	if result.Err != nil || result.Identity != identity {
		t.Fatalf("gate result=%#v", result)
	}
}

func TestControllerAcquireIdentityEnvironmentIsStrippedFromProviderChildren(t *testing.T) {
	env := stripControllerAcquireIdentityEnv([]string{
		"PATH=/bin",
		controllerAcquireIdentityAddressEnv + "=127.0.0.1:1234",
		controllerAcquireIdentityTokenEnv + "=secret",
		controllerProcessTreeOwnedEnv + "=1",
		"KEEP=value",
	})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "ACQUIRE_IDENTITY") || !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "KEEP=value") {
		t.Fatalf("sanitized provider env=%q", env)
	}
}

func TestControllerAcquireIdentityGateDoesNotSerializeBehindUnauthenticatedConnection(t *testing.T) {
	identity := controllerAcquireIdentity{
		LeaseID: "cbx_123456789abc", Slug: "stable-slug", Provider: "external", ResourceID: "provider/resource-123",
	}
	gate, err := newControllerAcquireIdentityGate(func(got controllerAcquireIdentity) error {
		if got != identity {
			return fmt.Errorf("identity=%#v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	env := gate.environment()
	slow, err := net.Dial("tcp4", env[controllerAcquireIdentityAddressEnv])
	if err != nil {
		gate.close()
		t.Fatal(err)
	}
	defer slow.Close()
	t.Setenv(controllerAcquireIdentityAddressEnv, env[controllerAcquireIdentityAddressEnv])
	t.Setenv(controllerAcquireIdentityTokenEnv, env[controllerAcquireIdentityTokenEnv])
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := acknowledgeControllerAcquireIdentity(ctx, identity); err != nil {
		gate.close()
		t.Fatalf("authenticated identity stalled behind unauthenticated connection: %v", err)
	}
	gate.close()
	result := gate.wait()
	if result.Err != nil || result.Identity != identity {
		t.Fatalf("gate result=%#v", result)
	}
}

func TestControllerAcquireIdentityGatePreAuthDeadlineDoesNotBoundPersistence(t *testing.T) {
	identity := controllerAcquireIdentity{
		LeaseID: "cbx_123456789abc", Slug: "stable-slug", Provider: "external", ResourceID: "provider/resource-123",
	}
	gate, err := newControllerAcquireIdentityGate(func(got controllerAcquireIdentity) error {
		if got != identity {
			return fmt.Errorf("identity=%#v", got)
		}
		time.Sleep(controllerAcquireIdentityPreAuthTTL + 100*time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	env := gate.environment()
	t.Setenv(controllerAcquireIdentityAddressEnv, env[controllerAcquireIdentityAddressEnv])
	t.Setenv(controllerAcquireIdentityTokenEnv, env[controllerAcquireIdentityTokenEnv])
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := acknowledgeControllerAcquireIdentity(ctx, identity); err != nil {
		gate.close()
		t.Fatalf("durable identity callback inherited pre-auth deadline: %v", err)
	}
	gate.close()
	if result := gate.wait(); result.Err != nil || result.Identity != identity {
		t.Fatalf("gate result=%#v", result)
	}
}

func TestControllerAcquireIdentityRequiresRawProviderCloudID(t *testing.T) {
	lease := LeaseTarget{
		LeaseID: "cbx_123456789abc",
		Server: Server{
			ID: 99, Name: "display-fallback", CloudID: "provider/raw-resource", Provider: "external",
			Labels: map[string]string{"slug": "stable-slug"},
		},
	}
	identity := controllerAcquireIdentityFromLease("external", lease)
	if identity.ResourceID != "provider/raw-resource" {
		t.Fatalf("raw acquire identity=%#v", identity)
	}
	lease.Server.CloudID = ""
	identity = controllerAcquireIdentityFromLease("external", lease)
	if identity.ResourceID != "" {
		t.Fatalf("display identity was substituted for missing raw cloud ID: %#v", identity)
	}
	if err := validateControllerAcquireIdentity(identity); err == nil || !strings.Contains(err.Error(), "resource ID") {
		t.Fatalf("missing raw provider cloud ID error=%v", err)
	}
}

func TestControllerChildSuppressesOrdinaryRegisteredWebVNCDaemon(t *testing.T) {
	cfg := baseConfig()
	cfg.BrokerMode = BrokerModeRegistered
	cfg.Coordinator = "https://broker.example.test"
	cfg.Desktop = true
	cfg.BrokerAutoWebVNC = true
	t.Setenv(controllerWorkspaceIDEnv, "demo-box")
	if shouldStartRegisteredWebVNCDaemon(cfg, true) {
		t.Fatal("controller child would start an unmanaged registered WebVNC daemon")
	}
	t.Setenv(controllerWorkspaceIDEnv, "")
	if !shouldStartRegisteredWebVNCDaemon(cfg, true) {
		t.Fatal("ordinary registered warmup no longer starts configured WebVNC daemon")
	}
}

func TestControllerTrackedChildWaitsForDurableLaunchGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("controller host is unsupported on Windows")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "launched")
	binary := filepath.Join(dir, "crabbox")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf launched >"+shellQuote(marker)+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &execControllerWorkspaceRunner{opts: execControllerRunnerOptions{
		Binary: binary, StateFile: filepath.Join(dir, "state.json"),
	}}
	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runner.runWithStarted(context.Background(), controllerWorkspaceRequest{ID: "launch-gate-box"}, []string{"inspect"}, io.Discard, func() error {
			close(callbackStarted)
			<-releaseCallback
			return nil
		})
	}()
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("tracked child callback did not start")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("provider command crossed launch gate: %v", err)
	}
	entries, err := os.ReadDir(controllerChildStateDirectory(runner.opts.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Fatalf("durable child identity missing before launch: %v", entries)
	}
	close(releaseCallback)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("tracked child did not finish")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("provider command did not run after gate release: %v", err)
	}
	entries, err = os.ReadDir(controllerChildStateDirectory(runner.opts.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("completed child identity was not removed: %v", entries)
	}
}

func TestControllerTrackedChildTerminatesDescendantsAfterDirectChildExit(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("controller process groups require Linux or macOS")
	}
	dir := t.TempDir()
	descendantPath := filepath.Join(dir, "descendant.pid")
	binary := filepath.Join(dir, "crabbox")
	script := "#!/bin/sh\nsleep 60 &\nprintf '%s\\n' \"$!\" >" + shellQuote(descendantPath) + "\nexit 0\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &execControllerWorkspaceRunner{opts: execControllerRunnerOptions{
		Binary: binary, StateFile: filepath.Join(dir, "state.json"),
	}}
	if err := runner.run(context.Background(), controllerWorkspaceRequest{ID: "descendant-box"}, []string{"inspect"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(descendantPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("descendant pid=%q err=%v", data, err)
	}
	if command, alive := webVNCDaemonProcessCommand(pid); alive && !strings.Contains(strings.ToLower(command), "<defunct>") {
		t.Fatalf("detached lifecycle descendant survived successful wrapper exit pid=%d command=%q", pid, command)
	}
}

func TestControllerTrackedChildTerminatesDescendantsOnCancellation(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("controller process groups require Linux or macOS")
	}
	dir := t.TempDir()
	descendantPath := filepath.Join(dir, "descendant.pid")
	binary := filepath.Join(dir, "crabbox")
	script := "#!/bin/sh\nsleep 60 &\nprintf '%s\\n' \"$!\" >" + shellQuote(descendantPath) + "\nwait\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &execControllerWorkspaceRunner{opts: execControllerRunnerOptions{
		Binary: binary, StateFile: filepath.Join(dir, "state.json"),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runner.run(ctx, controllerWorkspaceRequest{ID: "cancel-descendant-box"}, []string{"inspect"}, io.Discard)
	}()
	var pid int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(descendantPath)
		if err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if pid > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid <= 0 {
		cancel()
		<-done
		t.Fatal("descendant did not start")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run error=%v", err)
	}
	if command, alive := webVNCDaemonProcessCommand(pid); alive && !strings.Contains(strings.ToLower(command), "<defunct>") {
		t.Fatalf("lifecycle descendant survived cancellation pid=%d command=%q", pid, command)
	}
}

func TestControllerStartupTerminatesTrackedLifecycleChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("controller host is unsupported on Windows")
	}
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if err := saveControllerState(statePath, controllerState{Version: controllerStateVersion, Workspaces: map[string]controllerWorkspaceRecord{}}); err != nil {
		t.Fatal(err)
	}
	nonce := "11223344556677889900aabbccddeeff"
	child := exec.Command("sh", "-c", "while :; do sleep 1; done", "crabbox-controller-child-"+nonce)
	configureControllerCommand(child)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	childDone := false
	t.Cleanup(func() {
		if !childDone {
			_ = stopDaemonProcess(child.Process, child.Process.Pid)
			_ = child.Wait()
		}
	})
	started, err := webVNCDaemonProcessStartIdentity(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	identityDir := controllerChildStateDirectory(statePath)
	if err := ensureControllerStateDirectory(identityDir); err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(identityDir, "orphan.json")
	if err := writeControllerChildIdentity(identityPath, controllerChildIdentity{
		Version: controllerChildIdentityVersion, PID: child.Process.Pid, ProcessStarted: started,
		BootID: currentProcessBootIdentityForTest(t), Nonce: nonce, WorkspaceID: "orphan-box", Operation: "warmup",
	}); err != nil {
		t.Fatal(err)
	}
	runner := &fixedIdentityExecControllerRunner{execControllerWorkspaceRunner: &execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Binary: os.Args[0], Provider: "external", StateFile: statePath}}}
	ctx, cancel := context.WithCancel(context.Background())
	service, err := newControllerService(ctx, controllerServiceOptions{
		StateFile: statePath, MaxConcurrent: 1,
		CreateTimeout: time.Second, InspectTimeout: time.Second, StopTimeout: time.Second,
		ConnectionTimeout: time.Second, ReadyReconcileInterval: time.Hour,
	}, runner, "token", io.Discard)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		cancel()
		service.waitForShutdown()
		t.Fatal("tracked orphan child exited successfully instead of being terminated")
	}
	childDone = true
	if _, err := os.Stat(identityPath); !errors.Is(err, os.ErrNotExist) {
		cancel()
		service.waitForShutdown()
		t.Fatalf("recovered child identity still exists: %v", err)
	}
	cancel()
	service.waitForShutdown()
}

func TestControllerTrackedChildDiesWithControllerProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("controller host is unsupported on Windows")
	}
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	childPIDPath := filepath.Join(dir, "child.pid")
	binary := filepath.Join(dir, "crabbox-child")
	script := "#!/bin/sh\nprintf '%s\\n' \"$$\" >" + shellQuote(childPIDPath) + "\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestControllerTrackedChildParentHelper$")
	helper.Env = append(os.Environ(),
		"CRABBOX_TEST_TRACKED_CHILD_STATE="+statePath,
		"CRABBOX_TEST_TRACKED_CHILD_BINARY="+binary,
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
	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(childPIDPath)
		if err == nil {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 0 {
		t.Fatal("tracked lifecycle child did not start")
	}
	started, err := webVNCDaemonProcessStartIdentity(childPID)
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	helperDone = true
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, currentErr := webVNCDaemonProcessStartIdentity(childPID)
		command, alive := webVNCDaemonProcessCommand(childPID)
		if currentErr != nil || current != started || !alive || strings.Contains(strings.ToLower(command), "<defunct>") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("lifecycle child pid %d survived controller crash", childPID)
}

func TestControllerTrackedChildParentHelper(t *testing.T) {
	statePath := os.Getenv("CRABBOX_TEST_TRACKED_CHILD_STATE")
	binary := os.Getenv("CRABBOX_TEST_TRACKED_CHILD_BINARY")
	if statePath == "" || binary == "" {
		return
	}
	runner := &execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Binary: binary, StateFile: statePath}}
	_ = runner.run(context.Background(), controllerWorkspaceRequest{ID: "crash-child-box"}, []string{"warmup"}, io.Discard)
}

func TestControllerWebVNCURLParser(t *testing.T) {
	output := "vnc target: reachable 127.0.0.1:5900 managed=true\nportal bridge: connected=true\nwebvnc: https://portal.example.test/vnc#password=secret\npassword: secret\n"
	if got := controllerWebVNCURL(output); got != "https://portal.example.test/vnc#password=secret" {
		t.Fatalf("URL=%q", got)
	}
	if got := controllerWebVNCURL("webvnc: run crabbox webvnc --id demo\n"); got != "" {
		t.Fatalf("command hint parsed as URL: %q", got)
	}
}

func TestControllerWebVNCReadyParser(t *testing.T) {
	for _, output := range []string{
		"vnc target: reachable 127.0.0.1:5900 managed=true\nportal bridge: connected=true viewers=0\n",
		"vnc target: reachable 127.0.0.1:6080 managed=false\ndirect ssh webvnc: running\n",
	} {
		if !controllerWebVNCReady(output) {
			t.Fatalf("ready output rejected: %q", output)
		}
	}
	for _, output := range []string{
		"portal bridge: connected=true\n",
		"vnc target: reachable 127.0.0.1:5900 managed=true\nportal bridge: connected=false\n",
		"vnc target: reachable 127.0.0.1:5900 managed=true\ndirect ssh webvnc: unauthenticated (wrong password)\nwebvnc: http://127.0.0.1:5942/vnc.html?password=secret\n",
		"vnc target: unreachable 127.0.0.1:5900\nwebvnc: https://portal.example.test/vnc\n",
	} {
		if controllerWebVNCReady(output) {
			t.Fatalf("unready output accepted: %q", output)
		}
	}
}

func TestControllerWebVNCDaemonLiveParser(t *testing.T) {
	output := "webvnc daemon: pid=123 log=/tmp/bridge.log\nwebvnc daemon: command=/bin/sh -c 'crabbox webvnc --id demo --local-port 5942'\n"
	if !controllerWebVNCDaemonLive(output) {
		t.Fatal("live daemon output not recognized")
	}
	if got := controllerWebVNCDaemonPID(output); got != 123 {
		t.Fatalf("daemon PID=%d", got)
	}
	for _, output := range []string{
		"webvnc daemon: stale pid=123 log=/tmp/bridge.log\n",
		"webvnc daemon: no pid file for demo\n",
		"webvnc daemon: expected log=/tmp/bridge.log\n",
		"webvnc daemon: pid=123 log=/tmp/bridge.log\nwebvnc daemon: command=/bin/sleep 999\n",
	} {
		if controllerWebVNCDaemonLive(output) {
			t.Fatalf("non-live daemon output recognized: %q", output)
		}
	}
}

func TestControllerWebVNCDaemonLocalPortParser(t *testing.T) {
	output := "webvnc daemon: pid=123 log=/tmp/bridge.log\n" +
		"webvnc daemon: command=/bin/sh -c 'crabbox webvnc --id cbx_test --local-port 5942 --redact-credentials=true'\n"
	if got := controllerWebVNCDaemonLocalPort(output); got != "5942" {
		t.Fatalf("local port=%q", got)
	}
	if got := controllerWebVNCDaemonLocalPort("webvnc daemon: local-port=5943\n"); got != "5943" {
		t.Fatalf("reserved local port=%q", got)
	}
	if got := controllerWebVNCDaemonLocalPort("webvnc daemon: command=crabbox webvnc --id cbx_test\n"); got != "" {
		t.Fatalf("missing port parsed as %q", got)
	}
}

func TestControllerStopCleansDaemonsAndTreatsMissingLeaseAsStopped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	binary := filepath.Join(dir, "crabbox")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$CONTROLLER_CALLS\"\ncase \"$*\" in *--confirmed-absent-local-cleanup=true*) exit 0;; esac\nif [ \"$1\" = stop ]; then exit 4; fi\nif [ \"$1\" = list ]; then printf '[]\\n'; fi\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROLLER_CALLS", calls)
	runner := execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Binary: binary}}
	request := controllerWorkspaceRequest{
		ID: "demo-box", ProviderLeaseID: "cbx_missing123", ProviderAttemptLeaseID: "cbx_missing123",
		ProviderSlug: "missing-box", ProviderResourceID: "provider/missing",
		ProviderRoute: "external", ProviderScope: "scope-a", CoordinatorRegistrationURL: "https://coordinator.example.test/root",
	}
	if err := runner.Stop(context.Background(), "cbx_missing123", request); err != nil {
		t.Fatalf("idempotent stop: %v", err)
	}
	if err := runner.StopLocal(context.Background(), "cbx_missing123", controllerWorkspaceRequest{ID: "demo-box"}); err != nil {
		t.Fatalf("local cleanup: %v", err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		"webvnc daemon stop --id cbx_missing123",
		"stop --id cbx_missing123",
		"--expected-provider-lease-id cbx_missing123",
		"--expected-provider-attempt-lease-id cbx_missing123",
		"--expected-provider-slug missing-box",
		"--expected-provider-resource-id provider/missing",
		"--expected-provider-scope scope-a",
		"list --json --refresh --all",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("calls missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "webvnc daemon stop --id demo-box") {
		t.Fatalf("cleanup stopped workspace-ID daemon instead of resolved lease daemon:\n%s", got)
	}
	if strings.Contains(got, "--confirmed-absent-local-cleanup=true") {
		t.Fatalf("single absence observation removed local provider state:\n%s", got)
	}
	providerReleaseCalls := 0
	for _, call := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.HasPrefix(call, "stop ") && !strings.Contains(call, "--confirmed-absent-local-cleanup=true") {
			providerReleaseCalls++
		}
	}
	if providerReleaseCalls != 1 {
		t.Fatalf("provider release calls=%d want=1:\n%s", providerReleaseCalls, got)
	}
}

func TestControllerLifecycleCommandsUsePersistedExternalRoutingWithoutClaim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	leaseID := "cbx_routeonly123"
	routingPath, err := PersistExternalRouting(leaseID, ExternalConfig{Command: "provider-command", WorkRoot: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
	calls := filepath.Join(root, "calls")
	binary := filepath.Join(root, "crabbox")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$CONTROLLER_CALLS\"\n" +
		"if [ \"$1\" = inspect ]; then printf '{}\\n'; fi\n" +
		"if [ \"$1\" = list ]; then printf '[]\\n'; fi\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROLLER_CALLS", calls)
	runner := execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Binary: binary}}
	request := controllerWorkspaceRequest{
		ID: "route-only", ProviderRoute: "external", ProviderScope: "scope-a",
		ProviderLeaseID: leaseID, ProviderAttemptLeaseID: leaseID,
		ProviderSlug: "route-only", ProviderResourceID: "provider/route-only",
		CoordinatorRegistrationURL: "https://coordinator.example.test/root",
	}
	if _, err := runner.Inspect(context.Background(), leaseID, request); err != nil {
		t.Fatal(err)
	}
	if absent, err := runner.ConfirmAbsent(context.Background(), leaseID, request); err != nil || !absent {
		t.Fatalf("absent=%t err=%v", absent, err)
	}
	if err := runner.Stop(context.Background(), leaseID, request); err != nil {
		t.Fatal(err)
	}
	if err := runner.CleanupAbsent(context.Background(), leaseID, request); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 {
		t.Fatalf("calls=%q", string(data))
	}
	for _, line := range lines {
		if !strings.Contains(line, "--provider external") || !strings.Contains(line, "--external-routing-file "+routingPath) {
			t.Fatalf("lifecycle command did not use persisted route: %q", line)
		}
	}
	if !strings.Contains(lines[3], "--expected-coordinator-registration-url https://coordinator.example.test/root") {
		t.Fatalf("confirmed-absence cleanup omitted coordinator binding: %q", lines[3])
	}
}

func TestControllerPreAcquireInventoryUsesPersistedScopeWithoutRoutingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	binary := filepath.Join(root, "crabbox")
	calls := filepath.Join(root, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$CONTROLLER_CALLS\"\nprintf '[]\\n'\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROLLER_CALLS", calls)
	runner := execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Binary: binary}}
	request := controllerWorkspaceRequest{
		ID: "pre-acquire", ProviderRoute: "external", ProviderScope: "scope-a",
		ProviderAttemptLeaseID: "cbx_preacquire123", ProviderSlug: "pre-acquire",
	}
	absent, err := runner.ConfirmAbsent(context.Background(), request.ProviderAttemptLeaseID, request)
	if err != nil || !absent {
		t.Fatalf("absent=%t err=%v", absent, err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	call := string(data)
	if !strings.Contains(call, "list --json --refresh --all --provider external") {
		t.Fatalf("inventory did not use persisted provider route: %q", call)
	}
	if strings.Contains(call, "--external-routing-file") {
		t.Fatalf("pre-acquisition inventory required a nonexistent route file: %q", call)
	}
}

func TestConfirmedAbsentLocalCleanupRemovesOnlyFullyMatchingClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_abc123abc123"
	scope := "scope-a"
	server := Server{Provider: "external", CloudID: "provider/resource", Labels: map[string]string{"provider": "external", "slug": "fast-coral"}}
	if err := claimLeaseForRepoProviderScopePondEndpoint(leaseID, "fast-coral", "external", scope, "", "/repo", time.Minute, false, server, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	backend := &confirmedAbsentCleanupTestBackend{}
	expected := ProviderIdentityExpectation{
		LeaseID: leaseID, AttemptLeaseID: leaseID, Slug: "fast-coral", ResourceID: "provider/resource",
	}
	if err := cleanupConfirmedAbsentLocalState(context.Background(), backend, expected, scope); err != nil {
		t.Fatal(err)
	}
	if backend.cleanupCalls != 1 {
		t.Fatalf("sidecar cleanup calls=%d", backend.cleanupCalls)
	}
	if claim, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists || claim.LeaseID != "" {
		t.Fatalf("claim=%#v exists=%t err=%v", claim, exists, err)
	}
}

func TestConfirmedAbsentLocalCleanupPreservesClaimOnIdentityOrSidecarFailure(t *testing.T) {
	for _, test := range []struct {
		name        string
		resourceID  string
		cleanupErr  error
		wantCleanup int
	}{
		{name: "identity mismatch", resourceID: "provider/replacement"},
		{name: "sidecar failure", resourceID: "provider/resource", cleanupErr: errors.New("routing changed"), wantCleanup: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			leaseID := "cbx_abc123abc123"
			scope := "scope-a"
			server := Server{Provider: "external", CloudID: "provider/resource", Labels: map[string]string{"provider": "external", "slug": "fast-coral"}}
			if err := claimLeaseForRepoProviderScopePondEndpoint(leaseID, "fast-coral", "external", scope, "", "/repo", time.Minute, false, server, SSHTarget{}); err != nil {
				t.Fatal(err)
			}
			backend := &confirmedAbsentCleanupTestBackend{cleanupErr: test.cleanupErr}
			err := cleanupConfirmedAbsentLocalState(context.Background(), backend, ProviderIdentityExpectation{
				LeaseID: leaseID, AttemptLeaseID: leaseID, Slug: "fast-coral", ResourceID: test.resourceID,
			}, scope)
			if err == nil {
				t.Fatal("cleanup unexpectedly succeeded")
			}
			if backend.cleanupCalls != test.wantCleanup {
				t.Fatalf("sidecar cleanup calls=%d want=%d", backend.cleanupCalls, test.wantCleanup)
			}
			if claim, exists, readErr := readLeaseClaimWithPresence(leaseID); readErr != nil || !exists || claim.LeaseID != leaseID {
				t.Fatalf("claim=%#v exists=%t err=%v", claim, exists, readErr)
			}
		})
	}
}

func TestStopConfirmedAbsentLocalCleanupSkipsProviderReleasePath(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	leaseID := "cbx_abc123abc123"
	slug := "fast-coral"
	resourceID := "provider/resource"
	scope := "test-external:provider-command"
	if _, err := PersistExternalRouting(leaseID, ExternalConfig{Command: "provider-command", WorkRoot: "/home/tester/crabbox"}); err != nil {
		t.Fatal(err)
	}
	server := Server{Provider: "external", CloudID: resourceID, Labels: map[string]string{"provider": "external", "slug": slug}}
	if err := claimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, "external", scope, "", "/repo", time.Minute, false, server, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.stop(context.Background(), []string{
		"--confirmed-absent-local-cleanup=true", "--provider", "external", "--id", leaseID,
		"--expected-provider-lease-id", leaseID,
		"--expected-provider-attempt-lease-id", leaseID,
		"--expected-provider-slug", slug,
		"--expected-provider-resource-id", resourceID,
		"--expected-provider-scope", scope,
		"--expected-coordinator-registration-url", "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if claim, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists || claim.LeaseID != "" {
		t.Fatalf("claim=%#v exists=%t err=%v", claim, exists, err)
	}
}

func TestStopConfirmedAbsentLocalCleanupRejectsCoordinatorBindingDriftBeforeMutation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config-home"))
	configPath := filepath.Join(root, "config.yaml")
	t.Setenv("CRABBOX_CONFIG", configPath)
	leaseID := "cbx_drift123abc12"
	slug := "drift-coral"
	resourceID := "provider/drift"
	scope := "test-external:provider-command"
	config := "provider: external\nbroker:\n  url: https://new-coordinator.example.test/root\n  mode: registered\nexternal:\n  command: provider-command\n  capabilities:\n    idempotentLeaseId: true\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	routingPath, err := PersistExternalRouting(leaseID, ExternalConfig{Command: "provider-command", WorkRoot: "/home/tester/crabbox"})
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Provider: "external", CloudID: resourceID, Labels: map[string]string{"provider": "external", "slug": slug}}
	if err := claimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, "external", scope, "", "/repo", time.Minute, false, server, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err = app.stop(context.Background(), []string{
		"--confirmed-absent-local-cleanup=true", "--provider", "external", "--id", leaseID,
		"--expected-provider-lease-id", leaseID,
		"--expected-provider-attempt-lease-id", leaseID,
		"--expected-provider-slug", slug,
		"--expected-provider-resource-id", resourceID,
		"--expected-provider-scope", scope,
		"--expected-coordinator-registration-url", "https://old-coordinator.example.test/root",
	})
	if err == nil || !strings.Contains(err.Error(), "coordinator registration binding changed") {
		t.Fatalf("binding drift error=%v", err)
	}
	if _, exists, readErr := readLeaseClaimWithPresence(leaseID); readErr != nil || !exists {
		t.Fatalf("claim exists=%t err=%v", exists, readErr)
	}
	if _, statErr := os.Stat(routingPath); statErr != nil {
		t.Fatalf("routing state changed before binding validation: %v", statErr)
	}
}

func TestStopConfirmedAbsentDeregistrationFailureRetainsRouteForRetry(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config-home"))
	configPath := filepath.Join(root, "config.yaml")
	t.Setenv("CRABBOX_CONFIG", configPath)
	leaseID := "cbx_retry123abc12"
	slug := "retry-coral"
	resourceID := "provider/retry"
	externalCfg := ExternalConfig{
		Command: "provider-command", WorkRoot: "/home/tester/crabbox",
		Capabilities: ExternalCapabilitiesConfig{IdempotentLeaseID: true},
	}
	contractCfg := baseConfig()
	contractCfg.Provider = "external"
	contractCfg.External = externalCfg
	_, scope, _, err := controllerProviderIdentityForConfig(contractCfg)
	if err != nil {
		t.Fatal(err)
	}
	failDeregistration := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/"+leaseID+"/release" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if failDeregistration {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"temporary"}`))
			return
		}
		_, _ = w.Write([]byte(`{"lease":{"id":"` + leaseID + `","provider":"external","lifecycle":"registered","state":"released"}}`))
	}))
	defer server.Close()
	writeConfig := func(command string) {
		t.Helper()
		config := fmt.Sprintf("provider: external\nbroker:\n  url: %s\n  mode: registered\n  token: test-token\nexternal:\n  command: %s\n  capabilities:\n    idempotentLeaseId: true\n", server.URL, command)
		if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(externalCfg.Command)
	routingPath, err := PersistExternalRouting(leaseID, externalCfg)
	if err != nil {
		t.Fatal(err)
	}
	serverIdentity := Server{Provider: "external", CloudID: resourceID, Labels: map[string]string{"provider": "external", "slug": slug}}
	if err := claimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, "external", scope, "", "/repo", time.Minute, false, serverIdentity, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"--confirmed-absent-local-cleanup=true", "--provider", "external", "--id", leaseID,
		"--expected-provider-lease-id", leaseID,
		"--expected-provider-attempt-lease-id", leaseID,
		"--expected-provider-slug", slug,
		"--expected-provider-resource-id", resourceID,
		"--expected-provider-scope", scope,
		"--expected-coordinator-registration-url", server.URL,
	}
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	if err := app.stop(context.Background(), args); err == nil || !strings.Contains(err.Error(), "deregister coordinator lease") {
		t.Fatalf("deregistration error=%v", err)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || !exists {
		t.Fatalf("claim exists=%t err=%v", exists, err)
	}
	if _, err := os.Stat(routingPath); err != nil {
		t.Fatalf("routing removed before successful deregistration: %v", err)
	}

	// The persisted route must keep the retry bound to the original provider
	// scope even if the ambient external-provider configuration changes.
	failDeregistration = false
	writeConfig("different-provider-command")
	if err := app.stop(context.Background(), args); err != nil {
		t.Fatalf("retry after provider config drift: %v", err)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("claim exists after retry=%t err=%v", exists, err)
	}
	if _, err := os.Stat(routingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("routing remains after successful retry: %v", err)
	}
}

func TestControllerStopDoesNotTreatArbitraryExitFourAsMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "crabbox")
	script := "#!/bin/sh\nif [ \"$1\" = stop ]; then exit 4; fi\nif [ \"$1\" = list ]; then printf '[{\"labels\":{\"lease\":\"cbx_exists123\"}}]\\n'; fi\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Binary: binary}}
	if err := runner.Stop(context.Background(), "cbx_exists123", controllerWorkspaceRequest{ID: "demo-box"}); err == nil {
		t.Fatal("ambiguous exit code 4 was suppressed despite provider inventory containing the lease")
	}
}

func TestControllerAbsenceProofFailsClosedWhenCompleteInventoryUnsupported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "crabbox")
	script := "#!/bin/sh\nif [ \"$1\" = stop ]; then exit 4; fi\nif [ \"$1\" = list ]; then exit 2; fi\nexit 9\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Binary: binary}}
	if err := runner.Stop(context.Background(), "cbx_unknown123", controllerWorkspaceRequest{ID: "demo-box"}); err == nil {
		t.Fatal("stop suppressed failure when provider rejected complete inventory request")
	}
}

func TestControllerStopLocalReturnsDaemonError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "crabbox")
	script := "#!/bin/sh\nif [ \"$1 $2 $3\" = 'webvnc daemon stop' ]; then exit 5; fi\nexit 0\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Binary: binary}}
	if err := runner.StopLocal(context.Background(), "cbx_cleanup123", controllerWorkspaceRequest{ID: "demo-box"}); err == nil {
		t.Fatal("local cleanup error was suppressed")
	}
}

func TestControllerStopLocalDoesNotDependOnChildRegistry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "stopped")
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(controllerChildStateDirectory(statePath), []byte("blocks child registry"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "crabbox")
	script := "#!/bin/sh\nif [ \"$1 $2 $3\" = 'webvnc daemon stop' ]; then printf stopped >" + shellQuote(marker) + "; exit 0; fi\nexit 9\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Binary: binary, StateFile: statePath}}
	if err := runner.StopLocal(context.Background(), "cbx_cleanup123", controllerWorkspaceRequest{ID: "demo-box"}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "stopped" {
		t.Fatalf("independent local stop marker=%q err=%v", data, err)
	}
}

func TestControllerWarmupUsesPersistedProviderRoute(t *testing.T) {
	runner := execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Provider: "changed-provider"}}
	args := runner.warmupArgs("cbx_123456789abc", "stable-slug", controllerWorkspaceRequest{ProviderRoute: "persisted-provider"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--provider persisted-provider") || strings.Contains(joined, "changed-provider") {
		t.Fatalf("warmup provider route drifted: %q", args)
	}
}

func TestControllerListConfirmsAbsentIgnoresOnlyUnrelatedPartialInventory(t *testing.T) {
	identities := controllerAbsenceIdentities{
		LeaseIDs:    []string{"cbx_target", "cbx_attempt"},
		Names:       []string{"target-slug", "crabbox-target-slug-target"},
		ResourceIDs: []string{"provider/target"},
	}
	for _, test := range []struct {
		name string
		json string
		want bool
		err  bool
	}{
		{name: "empty", json: `[]`, want: true},
		{name: "other complete", json: `[{"CloudID":"provider/other","name":"other-name","labels":{"lease":"cbx_other","slug":"other-slug"}}]`, want: true},
		{name: "present lease", json: `[{"CloudID":"provider/other","name":"other-name","labels":{"lease":"cbx_target","slug":"other-slug"}}]`},
		{name: "present attempt", json: `[{"CloudID":"provider/other","name":"other-name","labels":{"lease":"cbx_attempt","slug":"other-slug"}}]`},
		{name: "present slug", json: `[{"CloudID":"provider/other","name":"other-name","labels":{"lease":"cbx_other","slug":"target-slug"}}]`},
		{name: "present provider name", json: `[{"CloudID":"provider/other","name":"crabbox-target-slug-target","labels":{"lease":"cbx_other","slug":"other-slug"}}]`},
		{name: "present resource", json: `[{"CloudID":"provider/target","name":"other-name","labels":{"lease":"cbx_other","slug":"other-slug"}}]`},
		{name: "unrelated legacy lease only", json: `[{"labels":{"lease":"cbx_other"}}]`, want: true},
		{name: "unrelated legacy name only", json: `[{"name":"legacy-other"}]`, want: true},
		{name: "partial matching lease", json: `[{"labels":{"lease":"cbx_target"}}]`, err: true},
		{name: "partial matching name", json: `[{"name":"target-slug"}]`, err: true},
		{name: "partial matching resource", json: `[{"CloudID":"provider/target"}]`, err: true},
		{name: "unrelated empty recognized", json: `[{"CloudID":"","name":"other-name","labels":{"lease":"cbx_other","slug":"other-slug"}}]`, want: true},
		{name: "unrelated padded recognized", json: `[{"CloudID":" provider/other ","name":"other-name","labels":{"lease":"cbx_other","slug":"other-slug"}}]`, want: true},
		{name: "padded matching resource", json: `[{"CloudID":" provider/target "}]`, err: true},
		{name: "unrelated invalid labels", json: `[{"name":"other-name","labels":null}]`, want: true},
		{name: "matching entry invalid labels", json: `[{"CloudID":"provider/target","name":"other-name","labels":null}]`, err: true},
		{name: "null", json: `null`, err: true},
		{name: "unknown unrelated shape", json: `[{"region":"west"}]`, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := controllerListConfirmsAbsent([]byte(test.json), identities)
			if (err != nil) != test.err || got != test.want {
				t.Fatalf("absent=%t err=%v want=%t err=%t", got, err, test.want, test.err)
			}
		})
	}
}

func TestControllerAbsenceIdentitySetUsesEveryPersistedIdentity(t *testing.T) {
	record := controllerWorkspaceRecord{
		Request:            controllerWorkspaceRequest{ID: "target-box"},
		LeaseID:            "cbx_target",
		AttemptLeaseID:     "cbx_attempt",
		Slug:               "target-slug",
		ProviderResourceID: "provider/target",
	}
	request := controllerRequestForRecord(record)
	identities, err := controllerAbsenceIdentitySet("cbx_target", request)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cbx_target", "cbx_attempt"} {
		if !slices.Contains(identities.LeaseIDs, want) {
			t.Fatalf("lease identities=%q missing %q", identities.LeaseIDs, want)
		}
	}
	for _, want := range []string{"target-slug", leaseProviderName("cbx_target", "target-slug"), leaseProviderName("cbx_attempt", "target-slug")} {
		if !slices.Contains(identities.Names, want) {
			t.Fatalf("name identities=%q missing %q", identities.Names, want)
		}
	}
	if !slices.Contains(identities.ResourceIDs, "provider/target") {
		t.Fatalf("resource identities=%q", identities.ResourceIDs)
	}
}

func TestControllerLimitedBufferReportsOverflow(t *testing.T) {
	var output controllerLimitedBuffer
	output.limit = 4
	if n, err := output.Write([]byte("12345")); err != nil || n != 5 {
		t.Fatalf("write bytes=%d err=%v", n, err)
	}
	if got := output.String(); got != "1234" {
		t.Fatalf("retained output=%q", got)
	}
	if err := output.overflowError("controller provider inventory"); err == nil || !strings.Contains(err.Error(), "exceeded 4-byte output limit") {
		t.Fatalf("overflow error=%v", err)
	}
}

func controllerDesktopTestRequest(leaseID string) controllerWorkspaceRequest {
	suffix := strings.TrimPrefix(leaseID, "cbx_")
	return controllerWorkspaceRequest{
		ID:                     "desktop-" + suffix,
		ProviderRoute:          "external",
		ProviderScope:          "scope-a",
		ProviderLeaseID:        leaseID,
		ProviderAttemptLeaseID: leaseID,
		ProviderSlug:           "desktop-" + suffix,
		ProviderResourceID:     "provider/" + suffix,
	}
}

func legacyControllerDesktopOwnerTokenForTest(r *execControllerWorkspaceRunner, identifier string, request controllerWorkspaceRequest) string {
	values := []string{
		filepath.Clean(r.controllerStatePath()), filepath.Clean(r.opts.Config),
		request.ProviderRoute, request.ProviderScope, request.ID, identifier,
		request.ProviderLeaseID, request.ProviderAttemptLeaseID, request.ProviderSlug, request.ProviderResourceID,
	}
	hash := sha256.New()
	for _, value := range values {
		_, _ = hash.Write([]byte(strconv.Itoa(len(value))))
		_, _ = hash.Write([]byte{':'})
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func TestControllerDesktopConnectionRestartsDaemonWithoutRecordedPort(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	started := filepath.Join(dir, "started")
	binary := filepath.Join(dir, "crabbox")
	runner := execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Binary: binary, Provider: "external"}}
	request := controllerDesktopTestRequest("cbx_legacy123")
	script := `#!/bin/sh
printf '%s\n' "$*" >>"$CONTROLLER_CALLS"
if [ "$1 $2 $3" = 'webvnc daemon status' ]; then
  if [ -f "$CONTROLLER_STARTED" ]; then
    port=$(cat "$CONTROLLER_STARTED")
    printf 'webvnc daemon: pid=123 log=/tmp/bridge.log\nwebvnc daemon: controller-owned=true no-provider-side-effects=true owner-match=true\nwebvnc daemon: command=/bin/sh -c crabbox-webvnc --no-provider-side-effects=true --local-port %s\n' "$port"
  else
    printf 'webvnc daemon: pid=123 log=/tmp/bridge.log\nwebvnc daemon: controller-owned=true no-provider-side-effects=true owner-match=true\nwebvnc daemon: command=/bin/sh -c crabbox-webvnc --no-provider-side-effects=true\n'
  fi
fi
if [ "$1 $2 $3" = 'webvnc daemon start' ]; then
	printf '5943\n' >"$CONTROLLER_STARTED"
	printf 'webvnc daemon: pid=123 log=/tmp/bridge.log\nwebvnc daemon: local-port=5943\n'
fi
if [ "$1 $2" = 'webvnc status' ]; then
  printf 'vnc target: reachable 127.0.0.1:5900 managed=true\nportal bridge: connected=true viewers=0 observers=0 slots=4\nwebvnc: http://127.0.0.1:6080/vnc.html\n'
fi
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROLLER_CALLS", calls)
	t.Setenv("CONTROLLER_STARTED", started)
	url, err := runner.DesktopConnection(context.Background(), "cbx_legacy123", request)
	if err != nil {
		t.Fatal(err)
	}
	if url != "http://127.0.0.1:6080/vnc.html" {
		t.Fatalf("URL=%q", url)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		"webvnc daemon stop --id cbx_legacy123",
		"webvnc daemon start --id cbx_legacy123 --provider external",
		"--controller-owned=true",
		"webvnc status --id cbx_legacy123 --provider external --local-port",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("calls missing %q:\n%s", want, got)
		}
	}
}

func TestControllerDesktopConnectionRevokesStartWithoutSupervisorPID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	binary := filepath.Join(dir, "crabbox")
	script := `#!/bin/sh
printf '%s\n' "$*" >>"$CONTROLLER_CALLS"
if [ "$1 $2 $3" = 'webvnc daemon status' ]; then
  printf 'webvnc daemon: no pid file for cbx_missing123\n'
fi
exit 0
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROLLER_CALLS", calls)
	runner := execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Binary: binary, Provider: "external"}}
	_, err := runner.DesktopConnection(context.Background(), "cbx_missing123", controllerDesktopTestRequest("cbx_missing123"))
	if err == nil || !strings.Contains(err.Error(), "did not report its supervisor pid") {
		t.Fatalf("error=%v", err)
	}
	data, readErr := os.ReadFile(calls)
	if readErr != nil {
		t.Fatal(readErr)
	}
	got := string(data)
	for _, want := range []string{"--controller-owned=true", "webvnc daemon stop --id cbx_missing123"} {
		if !strings.Contains(got, want) {
			t.Fatalf("calls=%q missing %q", got, want)
		}
	}
}

func TestControllerDesktopConnectionReplacesMismatchedDaemonOwnership(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	owned := filepath.Join(dir, "owned")
	portPath := filepath.Join(dir, "port")
	binary := filepath.Join(dir, "crabbox")
	request := controllerWorkspaceRequest{
		ID: "ownership-box", ProviderRoute: "external", ProviderScope: "scope-a",
		ProviderLeaseID: "cbx_owned123", ProviderAttemptLeaseID: "cbx_owned123",
		ProviderSlug: "owned-slug", ProviderResourceID: "provider/owned",
	}
	runner := execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Binary: binary, Provider: "external", StateFile: filepath.Join(dir, "controller.json")}}
	ownerID := runner.desktopControllerOwnerID(request.ProviderLeaseID, request)
	legacyOwnerToken := legacyControllerDesktopOwnerTokenForTest(&runner, request.ProviderLeaseID, request)
	if ownerID == legacyOwnerToken || len(ownerID) != sha256.Size*2 {
		t.Fatalf("owner ID was not domain-separated from raw ownership material: id=%q", ownerID)
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >>"$CONTROLLER_CALLS"
if [ "$1 $2 $3" = 'webvnc daemon status' ]; then
  owner_match=false
  port=5942
  if [ -f "$CONTROLLER_OWNED" ]; then owner_match=true; port=$(cat "$CONTROLLER_PORT"); fi
  printf 'webvnc daemon: pid=123 log=/tmp/bridge.log\nwebvnc daemon: controller-owned=true no-provider-side-effects=true owner-match=%s\nwebvnc daemon: command=/bin/sh -c crabbox-webvnc --no-provider-side-effects=true --local-port %s\n' "$owner_match" "$port"
fi
if [ "$1 $2 $3" = 'webvnc daemon start' ]; then
  : >"$CONTROLLER_OWNED"
  printf '5943\n' >"$CONTROLLER_PORT"
  printf 'webvnc daemon: pid=123 log=/tmp/bridge.log\nwebvnc daemon: local-port=5943\n'
fi
if [ "$1 $2" = 'webvnc status' ]; then
  printf 'vnc target: reachable 127.0.0.1:5900 managed=true\nportal bridge: connected=true viewers=0 observers=0 slots=4\nwebvnc: http://127.0.0.1:6080/vnc.html\n'
fi
exit 0
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROLLER_CALLS", calls)
	t.Setenv("CONTROLLER_OWNED", owned)
	t.Setenv("CONTROLLER_PORT", portPath)
	url, err := runner.DesktopConnection(context.Background(), request.ProviderLeaseID, request)
	if err != nil {
		t.Fatal(err)
	}
	if url != "http://127.0.0.1:6080/vnc.html" {
		t.Fatalf("URL=%q", url)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		"webvnc daemon stop --id " + request.ProviderLeaseID,
		"webvnc daemon start --id " + request.ProviderLeaseID,
		"--controller-owner-id " + ownerID,
		"--expected-provider-lease-id " + request.ProviderLeaseID,
		"--expected-provider-attempt-lease-id " + request.ProviderAttemptLeaseID,
		"--expected-provider-slug " + request.ProviderSlug,
		"--expected-provider-resource-id " + request.ProviderResourceID,
		"--expected-provider-scope " + request.ProviderScope,
		"--expected-listener-owner-pid 123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("calls=%q missing %q", got, want)
		}
	}
	if strings.Contains(got, legacyOwnerToken) || strings.Contains(got, "owner-token") {
		t.Fatalf("controller owner token leaked into subprocess argv/status: %q", got)
	}
}

func TestControllerDesktopConnectionRejectsURLWithoutReadyStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "crabbox")
	runner := execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Binary: binary}}
	request := controllerDesktopTestRequest("cbx_unready123")
	script := "#!/bin/sh\nif [ \"$1 $2 $3\" = 'webvnc daemon status' ]; then printf 'webvnc daemon: pid=123 log=/tmp/bridge.log\\nwebvnc daemon: controller-owned=true no-provider-side-effects=true owner-match=true\\nwebvnc daemon: command=/bin/sh -c crabbox-webvnc --no-provider-side-effects=true --local-port 5942\\n'; fi\nif [ \"$1 $2\" = 'webvnc status' ]; then printf 'vnc target: reachable 127.0.0.1:5900 managed=true\\nportal bridge: connected=false viewers=0 observers=0 slots=4\\nwebvnc: http://127.0.0.1:6080/vnc.html\\n'; fi\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.DesktopConnection(context.Background(), "cbx_unready123", request); err == nil {
		t.Fatal("URL-only WebVNC status was accepted without bridge readiness")
	}
}

func TestControllerDesktopConnectionAuthenticatesSelectedDirectSSHPort(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	port := availableControllerListenerTestPort(t)
	listenerProcess := startControllerListenerHelper(t, port)
	defer func() { stopControllerListenerHelper(listenerProcess) }()
	dir := t.TempDir()
	binary := filepath.Join(dir, "crabbox")
	runner := execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Binary: binary}}
	request := controllerDesktopTestRequest("cbx_direct123")
	script := strings.NewReplacer("DIRECT_PORT", port, "DAEMON_PID", strconv.Itoa(listenerProcess.Process.Pid)).Replace(`#!/bin/sh
if [ "$1 $2 $3" = 'webvnc daemon status' ]; then
  printf 'webvnc daemon: pid=DAEMON_PID log=/tmp/bridge.log\nwebvnc daemon: controller-owned=true no-provider-side-effects=true owner-match=true\nwebvnc daemon: command=/bin/sh -c crabbox-webvnc --no-provider-side-effects=true --local-port DIRECT_PORT\n'
fi
if [ "$1 $2" = 'webvnc status' ]; then
  printf 'vnc target: reachable 127.0.0.1:5900 managed=false\ndirect ssh webvnc: running\nwebvnc: http://127.0.0.1:DIRECT_PORT/vnc.html\n'
fi
`)
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	url, err := runner.DesktopConnection(context.Background(), "cbx_direct123", request)
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://127.0.0.1:" + port + "/vnc.html"; url != want {
		t.Fatalf("URL=%q want=%q", url, want)
	}
	stopControllerListenerHelper(listenerProcess)
	listenerProcess = nil
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := runner.DesktopConnection(ctx, "cbx_direct123", request); err == nil {
		t.Fatal("direct SSH desktop URL accepted after its daemon-owned listener closed")
	}
}

func TestControllerDesktopConnectionRejectsHostilePreboundDirectSSHPort(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	unrelated := exec.Command("sleep", "30")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = unrelated.Process.Kill()
		_ = unrelated.Wait()
	}()
	dir := t.TempDir()
	binary := filepath.Join(dir, "crabbox")
	runner := execControllerWorkspaceRunner{opts: execControllerRunnerOptions{Binary: binary}}
	request := controllerDesktopTestRequest("cbx_hostile123")
	script := strings.NewReplacer("DIRECT_PORT", port, "DAEMON_PID", strconv.Itoa(unrelated.Process.Pid)).Replace(`#!/bin/sh
if [ "$1 $2 $3" = 'webvnc daemon status' ]; then
  printf 'webvnc daemon: pid=DAEMON_PID log=/tmp/bridge.log\nwebvnc daemon: controller-owned=true no-provider-side-effects=true owner-match=true\nwebvnc daemon: command=/bin/sh -c crabbox-webvnc --no-provider-side-effects=true --local-port DIRECT_PORT\n'
fi
if [ "$1 $2" = 'webvnc status' ]; then
  printf 'vnc target: reachable 127.0.0.1:5900 managed=false\ndirect ssh webvnc: running\nwebvnc: http://127.0.0.1:DIRECT_PORT/vnc.html?password=secret\n'
fi
`)
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	tcpListener := listener.(*net.TCPListener)
	if err := tcpListener.SetDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.DesktopConnection(context.Background(), "cbx_hostile123", request); err == nil {
		t.Fatal("hostile prebound listener was accepted as daemon-owned")
	}
	connection, acceptErr := tcpListener.Accept()
	if acceptErr == nil {
		_ = connection.Close()
		t.Fatal("controller connected to hostile listener before ownership verification")
	}
	if networkErr, ok := acceptErr.(net.Error); !ok || !networkErr.Timeout() {
		t.Fatalf("hostile listener accept error=%v", acceptErr)
	}
}

func TestControllerOwnedListenerHelper(t *testing.T) {
	port := os.Getenv("CRABBOX_TEST_CONTROLLER_LISTENER_PORT")
	if port == "" {
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		_ = connection.Close()
	}
}

func startControllerListenerHelper(t *testing.T, port string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestControllerOwnedListenerHelper$")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "CRABBOX_TEST_CONTROLLER_LISTENER_PORT="+port)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var ownershipErr error
	for time.Now().Before(deadline) {
		if err := controllerVerifyDaemonOwnedListener(port, cmd.Process.Pid); err == nil {
			return cmd
		} else {
			ownershipErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	stopControllerListenerHelper(cmd)
	t.Fatalf("listener helper did not become verifiably ready: ownership=%v stderr=%s", ownershipErr, stderr.String())
	return nil
}

func availableControllerListenerTestPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func stopControllerListenerHelper(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

func TestValidateControllerConnectionURL(t *testing.T) {
	for _, value := range []string{
		"https://portal.example.test/vnc#password=secret",
		"http://127.0.0.1:6080/vnc.html",
		"http://localhost:6080/vnc.html",
		"http://[::1]:6080/vnc.html",
	} {
		if err := validateControllerConnectionURL(value); err != nil {
			t.Fatalf("URL %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{"http://example.test/vnc", "file:///tmp/viewer.html", "https://user:secret@example.test/vnc"} {
		if err := validateControllerConnectionURL(value); err == nil {
			t.Fatalf("URL %q accepted", value)
		}
	}
}

func TestValidateControllerURLTemplateRejectsPersistedCredentials(t *testing.T) {
	for _, value := range []string{
		"https://user:secret@example.test/{workspaceId}",
		"https://example.test/{workspaceId}#password=secret",
		"https://example.test/{workspaceId}?token=secret",
	} {
		if err := validateControllerURLTemplate(value); err == nil {
			t.Fatalf("URL template %q accepted", value)
		}
	}
}

func TestValidateControllerTerminalURLTemplate(t *testing.T) {
	for _, value := range []string{
		"wss://terminal.example.test/{workspaceId}",
		"ws://127.0.0.1:8788/{workspaceId}",
		"ws://localhost:8788/{workspaceId}",
		"ws://[::1]:8788/{workspaceId}",
	} {
		if err := validateControllerTerminalURLTemplate(value); err != nil {
			t.Fatalf("terminal URL template %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{
		"https://fleet.example.test/{workspaceId}",
		"ws://terminal.example.test/{workspaceId}",
		"wss://user:secret@terminal.example.test/{workspaceId}",
		"wss://terminal.example.test/{workspaceId}?token=secret",
	} {
		if err := validateControllerTerminalURLTemplate(value); err == nil {
			t.Fatalf("terminal URL template %q accepted", value)
		}
	}
}
