package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type powerCall struct {
	argv []string
	env  map[string]string
}

type powerRunner struct {
	mu     sync.Mutex
	events *[]string
	calls  []powerCall
	result func(argv []string, stderr io.Writer) (core.LocalCommandResult, error)
}

func (r *powerRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	argv := append([]string{req.Name}, req.Args...)
	env := map[string]string{}
	for _, entry := range req.Env {
		if name, value, ok := strings.Cut(entry, "="); ok && strings.HasPrefix(name, "CRABBOX_") {
			env[name] = value
		}
	}
	r.mu.Lock()
	r.calls = append(r.calls, powerCall{argv: argv, env: env})
	if r.events != nil {
		*r.events = append(*r.events, strings.Join(argv, " "))
	}
	r.mu.Unlock()
	if r.result != nil {
		return r.result(argv, req.Stderr)
	}
	return core.LocalCommandResult{}, nil
}

func (r *powerRunner) callsFor(name string) []powerCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var matched []powerCall
	for _, call := range r.calls {
		if slices.Contains(call.argv, name) {
			matched = append(matched, call)
		}
	}
	return matched
}

func staticPowerFixture(t *testing.T, events *[]string) (core.Config, *powerRunner) {
	t.Helper()
	stubStaticArchitecture(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldWait := waitForSSH
	waitForSSH = func(context.Context, *core.SSHTarget, io.Writer) error {
		if events != nil {
			*events = append(*events, "wait-ssh")
		}
		return nil
	}
	t.Cleanup(func() { waitForSSH = oldWait })

	cfg := core.BaseConfig()
	cfg.Provider = staticProvider
	cfg.TargetOS = core.TargetLinux
	cfg.Static.Host = "buildbox.example.test"
	cfg.Static.StartCommand = []string{"./host-power", "up"}
	cfg.Static.StopCommand = []string{"./host-power", "down"}
	return cfg, &powerRunner{events: events}
}

func newStaticPowerBackend(cfg core.Config, runner core.CommandRunner, stderr io.Writer) *staticLeaseBackend {
	return NewStaticSSHLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: stderr, Exec: runner}).(*staticLeaseBackend)
}

func acquireStaticPowerLease(t *testing.T, cfg core.Config, runner core.CommandRunner, leaseID string) core.LeaseTarget {
	t.Helper()
	cfg.Static.ID = leaseID
	lease, err := newStaticPowerBackend(cfg, runner, io.Discard).Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err != nil {
		t.Fatalf("acquire %s: %v", leaseID, err)
	}
	return lease
}

func TestStaticStartCommandRunsBeforeSSHWaitWithLeaseEnvironment(t *testing.T) {
	var events []string
	cfg, runner := staticPowerFixture(t, &events)

	lease := acquireStaticPowerLease(t, cfg, runner, "static_power_one")

	if want := []string{"./host-power up", "wait-ssh"}; !slices.Equal(events, want) {
		t.Fatalf("events=%q want %q", events, want)
	}
	start := runner.callsFor("up")
	if len(start) != 1 || start[0].env["CRABBOX_LEASE_ID"] != lease.LeaseID || start[0].env["CRABBOX_STATIC_HOST"] != "buildbox.example.test" {
		t.Fatalf("start calls=%#v lease=%s", start, lease.LeaseID)
	}
}

func TestStaticStartCommandFailureFailsAcquire(t *testing.T) {
	var events []string
	cfg, runner := staticPowerFixture(t, &events)
	runner.result = func(_ []string, stderr io.Writer) (core.LocalCommandResult, error) {
		fmt.Fprint(stderr, strings.Repeat("noise\n", 2000)+"wake-on-lan: no reply from buildbox\n")
		return core.LocalCommandResult{ExitCode: 7}, errors.New("exit status 7")
	}
	cfg.Static.ID = "static_power_fail"

	_, err := newStaticPowerBackend(cfg, runner, io.Discard).Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})

	if code := core.ExitCodeForError(err, 0); code != 7 {
		t.Fatalf("exit code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "static.startCommand failed") || !strings.HasSuffix(err.Error(), "wake-on-lan: no reply from buildbox") {
		t.Fatalf("error lacks command stderr tail: %v", err)
	}
	if len(err.Error()) > staticPowerStderrTailSize+256 {
		t.Fatalf("error carries %d bytes; stderr must be tail-bounded", len(err.Error()))
	}
	if slices.Contains(events, "wait-ssh") {
		t.Fatalf("SSH wait ran after failed start: %q", events)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence("static_power_fail"); err != nil || exists {
		t.Fatalf("failed start published claim exists=%t err=%v", exists, err)
	}
	if stops := runner.callsFor("down"); len(stops) != 0 {
		t.Fatalf("failed start ran stop: %#v", stops)
	}
}

func TestStaticStopCommandRunsOnceAfterLastParallelRelease(t *testing.T) {
	cfg, runner := staticPowerFixture(t, nil)
	first := acquireStaticPowerLease(t, cfg, runner, "static_power_a")
	second := acquireStaticPowerLease(t, cfg, runner, "static_power_b")
	backend := newStaticPowerBackend(cfg, runner, io.Discard)

	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: first}); err != nil {
		t.Fatal(err)
	}
	if stops := runner.callsFor("down"); len(stops) != 0 {
		t.Fatalf("stop ran while %s still held the host: %#v", second.LeaseID, stops)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: second}); err != nil {
		t.Fatal(err)
	}
	stops := runner.callsFor("down")
	if len(stops) != 1 || stops[0].env["CRABBOX_LEASE_ID"] != second.LeaseID {
		t.Fatalf("stops=%#v want one for %s", stops, second.LeaseID)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: second}); err != nil {
		t.Fatal(err)
	}
	if got := len(runner.callsFor("down")); got != 1 {
		t.Fatalf("repeated release without a live claim stops=%d want 1", got)
	}
}

func TestStaticReleaseWithoutClaimDoesNotStop(t *testing.T) {
	cfg, runner := staticPowerFixture(t, nil)
	cfg.Static.ID = "static_power_unclaimed"
	server, target, leaseID, err := core.StaticLease(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer

	err = newStaticPowerBackend(cfg, runner, &stderr).ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}})

	if err != nil {
		t.Fatal(err)
	}
	if stops := runner.callsFor("down"); len(stops) != 0 {
		t.Fatalf("unclaimed release stopped the host: %#v", stops)
	}
	if !strings.Contains(stderr.String(), "carries no claim snapshot") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestStaticStopCommandFailureWarns(t *testing.T) {
	cfg, runner := staticPowerFixture(t, nil)
	lease := acquireStaticPowerLease(t, cfg, runner, "static_power_warn")
	runner.result = func(argv []string, stderr io.Writer) (core.LocalCommandResult, error) {
		fmt.Fprint(stderr, "host refused shutdown\n")
		return core.LocalCommandResult{ExitCode: 4}, errors.New("exit status 4")
	}
	var stderr bytes.Buffer

	err := newStaticPowerBackend(cfg, runner, &stderr).ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})

	if err != nil {
		t.Fatalf("stop failure failed release: %v", err)
	}
	if !strings.Contains(stderr.String(), "warning: static.stopCommand failed") || !strings.Contains(stderr.String(), "host refused shutdown") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID); err != nil || exists {
		t.Fatalf("claim retained exists=%t err=%v", exists, err)
	}
}

func TestStaticAcquireFailureAfterStartStopsUnclaimedHost(t *testing.T) {
	cfg, runner := staticPowerFixture(t, nil)
	waitForSSH = func(context.Context, *core.SSHTarget, io.Writer) error { return core.Exit(5, "ssh never came up") }
	cfg.Static.ID = "static_power_rollback"

	_, err := newStaticPowerBackend(cfg, runner, io.Discard).Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})

	if err == nil || !strings.Contains(err.Error(), "ssh never came up") {
		t.Fatalf("err=%v", err)
	}
	stops := runner.callsFor("down")
	if len(stops) != 1 || stops[0].env["CRABBOX_LEASE_ID"] != "static_power_rollback" {
		t.Fatalf("stops=%#v want one rollback stop", stops)
	}
}

func TestStaticStopCommandSkipsLeaseOnOtherHost(t *testing.T) {
	cfg, runner := staticPowerFixture(t, nil)
	lease := acquireStaticPowerLease(t, cfg, runner, "static_power_other")
	other := cfg
	other.Static.Host = "other.example.test"
	var stderr bytes.Buffer

	if err := newStaticPowerBackend(other, runner, &stderr).ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if stops := runner.callsFor("down"); len(stops) != 0 {
		t.Fatalf("stop for other host ran: %#v", stops)
	}
	if !strings.Contains(stderr.String(), "does not match static.host") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestStaticPowerCommandRunsRealProcess(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	cfg, _ := staticPowerFixture(t, nil)
	cfg.Static.StartCommand = []string{"sh", "-c", `printf '%s %s' "$CRABBOX_LEASE_ID" "$CRABBOX_STATIC_HOST" >&2; exit 3`}
	cfg.Static.ID = "static_power_real"

	_, err := NewStaticSSHLeaseBackend(Provider{}.Spec(), cfg, core.RuntimeForProviderOperation(io.Discard)).(*staticLeaseBackend).Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})

	if code := core.ExitCodeForError(err, 0); code != 3 || !strings.HasSuffix(err.Error(), "static_power_real buildbox.example.test") {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestStaticReleaseWaitsForConcurrentAcquireBeforeStopping(t *testing.T) {
	cfg, runner := staticPowerFixture(t, nil)
	first := acquireStaticPowerLease(t, cfg, runner, "static_power_first")
	waiting := make(chan struct{})
	proceed := make(chan struct{})
	waitForSSH = func(context.Context, *core.SSHTarget, io.Writer) error {
		close(waiting)
		<-proceed
		return nil
	}
	second := cfg
	second.Static.ID = "static_power_second"
	acquired := make(chan error, 1)
	go func() {
		_, err := newStaticPowerBackend(second, runner, io.Discard).Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
		acquired <- err
	}()
	<-waiting
	released := make(chan error, 1)
	go func() {
		released <- newStaticPowerBackend(cfg, runner, io.Discard).ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: first})
	}()
	select {
	case err := <-released:
		t.Fatalf("release finished while another acquisition held the host: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(proceed)
	if err := <-acquired; err != nil {
		t.Fatal(err)
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if stops := runner.callsFor("down"); len(stops) != 0 {
		t.Fatalf("stop raced a concurrent acquisition: %#v", stops)
	}
}

func TestTailBufferKeepsBoundedSuffix(t *testing.T) {
	tail := &tailBuffer{limit: 8}
	for _, chunk := range []string{"abc", "defgh", "ij", strings.Repeat("x", 20) + "12345678", "zz"} {
		if n, err := tail.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("write %q n=%d err=%v", chunk, n, err)
		}
		if len(tail.data) > tail.limit || cap(tail.data) > 2*tail.limit {
			t.Fatalf("tail grew len=%d cap=%d", len(tail.data), cap(tail.data))
		}
	}
	if got := tail.String(); got != "345678zz" {
		t.Fatalf("tail=%q", got)
	}
}

func TestStaticPowerCommandCancellationIsNotTimeout(t *testing.T) {
	cfg, runner := staticPowerFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	runner.result = func([]string, io.Writer) (core.LocalCommandResult, error) {
		cancel()
		return core.LocalCommandResult{ExitCode: -1}, context.Canceled
	}

	err := newStaticPowerBackend(cfg, runner, io.Discard).runStaticPowerCommand(ctx, "static.startCommand", cfg.Static.StartCommand, "static_power_cancel", cfg.Static.Host)

	if err == nil || strings.Contains(err.Error(), "timeout") || core.ExitCodeForError(err, 0) != 1 {
		t.Fatalf("err=%v", err)
	}
}

func TestStaticReleaseWaitsForAcquireWithoutPowerCommands(t *testing.T) {
	cfg, runner := staticPowerFixture(t, nil)
	first := acquireStaticPowerLease(t, cfg, runner, "static_power_hooked")
	waiting := make(chan struct{})
	proceed := make(chan struct{})
	waitForSSH = func(context.Context, *core.SSHTarget, io.Writer) error {
		close(waiting)
		<-proceed
		return nil
	}
	plain := cfg
	plain.Static.ID = "static_power_plain"
	plain.Static.StartCommand = nil
	plain.Static.StopCommand = nil
	acquired := make(chan error, 1)
	go func() {
		_, err := newStaticPowerBackend(plain, runner, io.Discard).Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
		acquired <- err
	}()
	<-waiting
	released := make(chan error, 1)
	go func() {
		released <- newStaticPowerBackend(cfg, runner, io.Discard).ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: first})
	}()
	select {
	case err := <-released:
		t.Fatalf("release finished while an acquisition without power commands held the host: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(proceed)
	if err := <-acquired; err != nil {
		t.Fatal(err)
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if stops := runner.callsFor("down"); len(stops) != 0 {
		t.Fatalf("stop raced an acquisition without power commands: %#v", stops)
	}
}

func TestStaticStopSkipsClaimChangedByAnotherProcess(t *testing.T) {
	cfg, runner := staticPowerFixture(t, nil)
	cfg.Static.ID = "static_power_stale"
	holder := newStaticPowerBackend(cfg, runner, io.Discard)
	stale, err := holder.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	other := newStaticPowerBackend(cfg, runner, io.Discard)
	resolved, err := other.Resolve(context.Background(), core.ResolveRequest{ID: stale.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Touch(context.Background(), core.TouchRequest{Lease: resolved, State: "busy"}); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	holder.RT.Stderr = &stderr

	if err := holder.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: stale}); err != nil {
		t.Fatal(err)
	}
	if stops := runner.callsFor("down"); len(stops) != 0 {
		t.Fatalf("stale lease stopped a host whose claim another process changed: %#v", stops)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(stale.LeaseID); err != nil || !exists {
		t.Fatalf("stale release removed the changed claim exists=%t err=%v", exists, err)
	}
	if !strings.Contains(stderr.String(), "claim is absent or changed") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestStaticStopFollowsOwnHeartbeat(t *testing.T) {
	cfg, runner := staticPowerFixture(t, nil)
	cfg.Static.ID = "static_power_heartbeat"
	backend := newStaticPowerBackend(cfg, runner, io.Discard)
	acquired, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Touch(context.Background(), core.TouchRequest{Lease: acquired, State: "running"}); err != nil {
		t.Fatal(err)
	}

	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: acquired}); err != nil {
		t.Fatal(err)
	}
	if stops := runner.callsFor("down"); len(stops) != 1 {
		t.Fatalf("own heartbeat blocked the stop: stops=%#v", stops)
	}
}

func TestStaticStopFromFreshResolve(t *testing.T) {
	cfg, runner := staticPowerFixture(t, nil)
	lease := acquireStaticPowerLease(t, cfg, runner, "static_power_resolved")
	cfg.Static.ID = lease.LeaseID
	stopper := newStaticPowerBackend(cfg, runner, io.Discard)
	resolved, err := stopper.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}

	if err := stopper.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: resolved}); err != nil {
		t.Fatal(err)
	}
	if stops := runner.callsFor("down"); len(stops) != 1 {
		t.Fatalf("resolved release stops=%#v want 1", stops)
	}
}

func TestStaticStopSkipsOlderAcquisitionOnSameBackend(t *testing.T) {
	cfg, runner := staticPowerFixture(t, nil)
	cfg.Static.ID = "static_power_reacquired"
	backend := newStaticPowerBackend(cfg, runner, io.Discard)
	repo := core.Repo{Root: t.TempDir()}
	older, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Touch(context.Background(), core.TouchRequest{Lease: newer, State: "running"}); err != nil {
		t.Fatal(err)
	}

	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: older}); err != nil {
		t.Fatal(err)
	}
	if stops := runner.callsFor("down"); len(stops) != 0 {
		t.Fatalf("older acquisition stopped the host under a newer one: %#v", stops)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(newer.LeaseID); err != nil || !exists {
		t.Fatalf("older release removed the newer claim exists=%t err=%v", exists, err)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: newer}); err != nil {
		t.Fatal(err)
	}
	if stops := runner.callsFor("down"); len(stops) != 1 {
		t.Fatalf("newer release stops=%#v want 1", stops)
	}
}

func TestStaticUnobservedReleaseKeepsConcurrentClaim(t *testing.T) {
	cfg, runner := staticPowerFixture(t, nil)
	cfg.Static.ID = "static_power_unobserved"
	server, target, leaseID, err := core.StaticLease(cfg)
	if err != nil {
		t.Fatal(err)
	}
	unobserved := core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}
	acquireStaticPowerLease(t, cfg, runner, leaseID)

	if err := newStaticPowerBackend(cfg, runner, io.Discard).ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: unobserved}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || !exists {
		t.Fatalf("unobserved release removed a concurrent claim exists=%t err=%v", exists, err)
	}
	if stops := runner.callsFor("down"); len(stops) != 0 {
		t.Fatalf("unobserved release stopped the host: %#v", stops)
	}
}
