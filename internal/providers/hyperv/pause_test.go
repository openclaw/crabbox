package hyperv

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestPauseUsesSaveVMForRunningAndPausedGuests(t *testing.T) {
	for _, state := range []int{hypervStateRunning, hypervStatePaused} {
		t.Run(hypervState(state), func(t *testing.T) {
			b, runner, leaseID, _ := pauseTestLease(t, state, "192.0.2.10")

			if err := b.Pause(context.Background(), core.PauseRequest{ID: leaseID}); err != nil {
				t.Fatalf("Pause: %v", err)
			}
			if got := matchingScripts(runner.calls, "Save-VM"); len(got) != 1 {
				t.Fatalf("Save-VM calls=%v want one", got)
			}
			if query, save := firstScriptIndex(runner.calls, "Get-VM -ErrorAction Stop"), firstScriptIndex(runner.calls, "Save-VM"); query < 0 || save <= query {
				t.Fatalf("command order query=%d save=%d calls=%#v", query, save, runner.calls)
			}
			if got := matchingScripts(runner.calls, "Start-VM"); len(got) != 0 {
				t.Fatalf("Start-VM calls=%v want none", got)
			}
		})
	}
}

func TestPauseSavedGuestIsIdempotent(t *testing.T) {
	b, runner, leaseID, _ := pauseTestLease(t, hypervStateSaved, "192.0.2.10")

	if err := b.Pause(context.Background(), core.PauseRequest{ID: leaseID}); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if got := matchingScripts(runner.calls, "Save-VM"); len(got) != 0 {
		t.Fatalf("Save-VM calls=%v want none", got)
	}
}

func TestPausePreservesTailscaleIdentity(t *testing.T) {
	b, runner, leaseID, _ := pauseTestLease(t, hypervStateRunning, "192.0.2.10")

	if err := b.Pause(context.Background(), core.PauseRequest{ID: leaseID}); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.calls {
		script := commandScript(call)
		for _, forbidden := range []string{"tailscale logout", "tailscaled.state", "Stop-Service -Name Tailscale"} {
			if strings.Contains(script, forbidden) {
				t.Fatalf("pause mutated Tailscale identity through %q", forbidden)
			}
		}
	}
}

func TestResumeSavedGuestStartsWaitsAndRefreshesChangedIP(t *testing.T) {
	b, runner, leaseID, name := pauseTestLease(t, hypervStateSaved, "192.0.2.10")
	runner.respond = hyperVStateIPSequenceResponder(name, hypervStateSaved, []string{
		"192.0.2.10",
		"192.0.2.55",
		"192.0.2.55",
		"192.0.2.55",
	})
	var readyHost string
	var events []string
	runner.onRun = func(req core.LocalCommandRequest) {
		if len(req.Args) == 0 {
			return
		}
		script := req.Args[len(req.Args)-1]
		switch {
		case strings.Contains(script, "Start-VM"):
			events = append(events, "start")
		case strings.Contains(script, "Get-VMNetworkAdapter"):
			events = append(events, "ip")
		}
	}
	b.sshReady = func(_ context.Context, target *core.SSHTarget, _ io.Writer, phase string, _ time.Duration) error {
		events = append(events, "ssh:"+target.Host)
		readyHost = target.Host
		if phase != "hyperv resume" {
			t.Fatalf("phase=%q want hyperv resume", phase)
		}
		claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
		if err != nil || !ok {
			t.Fatalf("resolve claim during readiness: ok=%v err=%v", ok, err)
		}
		if claim.SSHHost != "192.0.2.10" {
			t.Fatalf("claim changed before SSH readiness: SSHHost=%q", claim.SSHHost)
		}
		return nil
	}

	if err := b.Resume(context.Background(), core.ResumeRequest{ID: leaseID}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := matchingScripts(runner.calls, "Start-VM"); len(got) != 1 {
		t.Fatalf("Start-VM calls=%v want one", got)
	}
	if readyHost != "192.0.2.55" {
		t.Fatalf("SSH readiness host=%q want changed IP", readyHost)
	}
	if got := strings.Join(events, ","); got != "start,ip,ssh:192.0.2.10,ip,ip,ssh:192.0.2.55,ip" {
		t.Fatalf("resume sequence=%q", got)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("resolve refreshed claim: ok=%v err=%v", ok, err)
	}
	if claim.SSHHost != "192.0.2.55" {
		t.Fatalf("claim SSHHost=%q want 192.0.2.55", claim.SSHHost)
	}
	if claim.Labels["state"] != "ready" || claim.Labels["hyperv_state"] != "running" {
		t.Fatalf("claim state=%q hyperv_state=%q", claim.Labels["state"], claim.Labels["hyperv_state"])
	}
}

func TestResumeRunningGuestIsIdempotent(t *testing.T) {
	b, runner, leaseID, name := pauseTestLease(t, hypervStateRunning, "192.0.2.10")
	runner.respond = hyperVStateResponder(name, hypervStateRunning, "192.0.2.10")
	b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }

	if err := b.Resume(context.Background(), core.ResumeRequest{ID: leaseID}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := matchingScripts(runner.calls, "Start-VM"); len(got) != 0 {
		t.Fatalf("Start-VM calls=%v want none", got)
	}
}

func TestResumeRejectsConcurrentClaimChange(t *testing.T) {
	b, _, leaseID, name := pauseTestLease(t, hypervStateSaved, "192.0.2.10")
	b.rt.Exec = &recordingRunner{respond: hyperVStateResponder(name, hypervStateSaved, "192.0.2.55")}
	b.sshReady = func(_ context.Context, _ *core.SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
		if err != nil || !ok {
			t.Fatalf("resolve claim: ok=%v err=%v", ok, err)
		}
		server := b.serverFromInstance(hypervVM{Name: name, State: hypervStateRunning}, claim, b.configForRun())
		server.Status = "ready"
		server.Labels["state"] = "ready"
		target := core.SSHTargetFromConfig(b.configForRun(), "192.0.2.99")
		if err := core.UpdateLeaseClaimEndpoint(leaseID, server, target); err != nil {
			t.Fatalf("concurrent claim update: %v", err)
		}
		return nil
	}

	err := b.Resume(context.Background(), core.ResumeRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("Resume err=%v want claim conflict", err)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("resolve changed claim: ok=%v err=%v", ok, err)
	}
	if claim.SSHHost != "192.0.2.99" {
		t.Fatalf("claim SSHHost=%q want concurrent update preserved", claim.SSHHost)
	}
}

func TestResumePausedGuestUsesResumeVM(t *testing.T) {
	b, runner, leaseID, name := pauseTestLease(t, hypervStatePaused, "192.0.2.10")
	runner.respond = hyperVStateResponder(name, hypervStatePaused, "192.0.2.10")
	b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }

	if err := b.Resume(context.Background(), core.ResumeRequest{ID: leaseID}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := matchingScripts(runner.calls, "Resume-VM"); len(got) != 1 {
		t.Fatalf("Resume-VM calls=%v want one", got)
	}
	if got := matchingScripts(runner.calls, "Start-VM"); len(got) != 0 {
		t.Fatalf("Start-VM calls=%v want none", got)
	}
}

func TestPauseRejectsClaimThatDoesNotExactlyOwnVM(t *testing.T) {
	testutil.IsolateUserDirs(t)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	const leaseID = "cbx_pausewrongowner"
	const name = "crabbox-pause-wrong-owner"
	runner := &recordingRunner{respond: hyperVStateResponder(name, hypervStateRunning, "192.0.2.10")}
	b := testBackend(runner)
	cfg := b.configForRun()
	server := b.serverFromInstance(hypervVM{Name: "crabbox-other-owner", State: hypervStateRunning}, core.LeaseClaim{
		LeaseID: leaseID,
		Slug:    "pause-wrong-owner",
		Labels:  map[string]string{"instance": name, "lease": leaseID},
	}, cfg)
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID, "pause-wrong-owner", providerName, instanceScope(name), "", t.TempDir(),
		cfg.IdleTimeout, false, server, core.SSHTargetFromConfig(cfg, "192.0.2.10"),
	); err != nil {
		t.Fatalf("persist mismatched claim: %v", err)
	}

	err := b.Pause(context.Background(), core.PauseRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("Pause err=%v want exact claim rejection", err)
	}
	if got := matchingScripts(runner.calls, "Save-VM"); len(got) != 0 {
		t.Fatalf("Save-VM calls=%v want none", got)
	}
}

func TestPauseResumeRejectStoppedAndMissingVMs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state int
	}{
		{name: "stopped", state: hypervStateStopped},
		{name: "missing", state: hypervMissingState},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, leaseID, _ := pauseTestLease(t, tc.state, "192.0.2.10")
			for _, run := range []struct {
				name string
				fn   func() error
			}{
				{name: "pause", fn: func() error { return b.Pause(context.Background(), core.PauseRequest{ID: leaseID}) }},
				{name: "resume", fn: func() error { return b.Resume(context.Background(), core.ResumeRequest{ID: leaseID}) }},
			} {
				t.Run(run.name, func(t *testing.T) {
					if err := run.fn(); err == nil {
						t.Fatalf("%s unexpectedly succeeded", run.name)
					}
				})
			}
		})
	}
}

func TestCleanupPreservesLiveSavedAndPausedLeases(t *testing.T) {
	for _, state := range []int{hypervStateSaved, hypervStatePaused} {
		t.Run(hypervState(state), func(t *testing.T) {
			b, runner, _, name := pauseTestLease(t, state, "192.0.2.10")
			runner.respond = hyperVCleanupResponder(name, state)

			if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
				t.Fatalf("Cleanup: %v", err)
			}
			if got := matchingScripts(runner.calls, "Remove-VM"); len(got) != 0 {
				t.Fatalf("Remove-VM calls=%v want none", got)
			}
		})
	}
}

func TestCleanupRemovesExpiredSavedAndPausedLeases(t *testing.T) {
	for _, state := range []int{hypervStateSaved, hypervStatePaused} {
		t.Run(hypervState(state), func(t *testing.T) {
			b, runner, leaseID, name := pauseTestLease(t, state, "192.0.2.10")
			claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
			if err != nil || !ok {
				t.Fatalf("resolve claim: ok=%v err=%v", ok, err)
			}
			server := b.serverFromInstance(hypervVM{Name: name, State: state}, claim, b.configForRun())
			server.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
			if err := updateLeaseClaimEndpointIfUnchanged(leaseID, claim, server, core.SSHTargetFromConfig(b.configForRun(), "192.0.2.10")); err != nil {
				t.Fatalf("expire claim: %v", err)
			}
			runner.respond = hyperVCleanupResponder(name, state)

			if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
				t.Fatalf("Cleanup: %v", err)
			}
			if got := matchingScripts(runner.calls, "Remove-VM"); len(got) != 1 {
				t.Fatalf("Remove-VM calls=%v want one", got)
			}
			if _, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName); err != nil || ok {
				t.Fatalf("claim after cleanup ok=%v err=%v", ok, err)
			}
		})
	}
}

func pauseTestLease(t *testing.T, state int, ip string) (*backend, *recordingRunner, string, string) {
	t.Helper()
	testutil.IsolateUserDirs(t)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })

	leaseID := "cbx_pause" + strings.ReplaceAll(hypervState(state), "-", "")
	name := "crabbox-pause-" + strings.ReplaceAll(hypervState(state), "-", "")
	runner := &recordingRunner{respond: hyperVStateResponder(name, state, ip)}
	b := testBackend(runner)
	b.rt.Stdout = io.Discard
	b.rt.Stderr = io.Discard
	b.resumeIPStableWindow = 0
	b.resumePollInterval = 0
	cfg := b.configForRun()
	claim := core.LeaseClaim{
		LeaseID:       leaseID,
		Slug:          "pause-" + hypervState(state),
		Provider:      providerName,
		ProviderScope: instanceScope(name),
		Labels:        map[string]string{"instance": name, "lease": leaseID, "state": "ready"},
	}
	lease := core.LeaseTarget{
		LeaseID: leaseID,
		Server:  b.serverFromInstance(hypervVM{Name: name, State: hypervStateRunning}, claim, cfg),
		SSH:     core.SSHTargetFromConfig(cfg, ip),
	}
	if err := persistLease(leaseID, claim.Slug, name, cfg, core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}}, &lease, nil); err != nil {
		t.Fatalf("persistLease: %v", err)
	}
	return b, runner, leaseID, name
}

func hyperVStateResponder(name string, state int, ip string) func(core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
	return hyperVStateIPSequenceResponder(name, state, []string{ip})
}

func hyperVStateIPSequenceResponder(name string, state int, ips []string) func(core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
	currentState := state
	ipIndex := 0
	return func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if len(req.Args) == 0 {
			return core.LocalCommandResult{}, nil, false
		}
		script := req.Args[len(req.Args)-1]
		switch {
		case strings.Contains(script, "Save-VM"):
			currentState = hypervStateSaved
			return core.LocalCommandResult{}, nil, true
		case strings.Contains(script, "Start-VM"), strings.Contains(script, "Resume-VM"):
			currentState = hypervStateRunning
			return core.LocalCommandResult{}, nil, true
		case strings.Contains(script, "Get-VM -ErrorAction Stop"):
			if currentState == hypervMissingState {
				return core.LocalCommandResult{Stdout: "null"}, nil, true
			}
			data, _ := json.Marshal(hypervVM{Name: name, State: currentState})
			return core.LocalCommandResult{Stdout: string(data)}, nil, true
		case strings.Contains(script, "Get-VMNetworkAdapter"):
			ip := ""
			if len(ips) > 0 {
				ip = ips[min(ipIndex, len(ips)-1)]
				ipIndex++
			}
			data, _ := json.Marshal([]string{ip})
			return core.LocalCommandResult{Stdout: string(data)}, nil, true
		default:
			return core.LocalCommandResult{}, nil, false
		}
	}
}

func hyperVCleanupResponder(name string, state int) func(core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
	stateResponder := hyperVStateResponder(name, state, "192.0.2.10")
	return func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if len(req.Args) > 0 {
			script := req.Args[len(req.Args)-1]
			if strings.Contains(script, "Get-VM | Where-Object") && strings.Contains(script, "-like 'crabbox-*'") {
				data, _ := json.Marshal([]hypervVM{{Name: name, State: state}})
				return core.LocalCommandResult{Stdout: string(data)}, nil, true
			}
		}
		return stateResponder(req)
	}
}

func matchingScripts(calls []core.LocalCommandRequest, command string) []string {
	var matches []string
	for _, call := range calls {
		if len(call.Args) == 0 {
			continue
		}
		script := call.Args[len(call.Args)-1]
		if strings.Contains(script, command) {
			matches = append(matches, script)
		}
	}
	return matches
}

func firstScriptIndex(calls []core.LocalCommandRequest, command string) int {
	for i, call := range calls {
		if len(call.Args) > 0 && strings.Contains(call.Args[len(call.Args)-1], command) {
			return i
		}
	}
	return -1
}
