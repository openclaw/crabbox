package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestWindowsTailscaleBootstrapVerifiesPinnedMSIBeforeInstall(t *testing.T) {
	cfg := testBackend(&recordingRunner{}).configForRun()
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.Hostname = "crabbox-blue"
	cfg.Tailscale.Tags = []string{"tag:crabbox", "tag:ci"}
	cfg.Tailscale.ExitNode = "100.64.0.1"
	cfg.Tailscale.ExitNodeAllowLANAccess = true

	script := windowsTailscaleBootstrapPowerShell(cfg)
	hashIndex := strings.Index(script, "Get-FileHash -Algorithm SHA256")
	installIndex := strings.Index(script, "Start-Process")
	for _, want := range []string{
		windowsTailscaleMSIURL,
		windowsTailscaleMSISHA256,
		"Tailscale MSI SHA-256 mismatch",
		`$quotedMSIPath = '"' + $msiPath + '"'`,
		`Tailscale\tailscale.exe`,
		"--hostname=crabbox-blue",
		"--advertise-tags=tag:crabbox,tag:ci",
		"--exit-node=100.64.0.1",
		"--exit-node-allow-lan-access",
		`C:\ProgramData\crabbox\tailscale`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("bootstrap script missing %q", want)
		}
	}
	if hashIndex < 0 || installIndex <= hashIndex {
		t.Fatalf("MSI install is not gated by SHA-256 verification: hash=%d install=%d", hashIndex, installIndex)
	}
	resetIndex := strings.Index(script, "Tailscale identity state remains")
	upIndex := strings.Index(script, "$upArgs.Add('up')")
	if resetIndex < 0 || upIndex <= resetIndex {
		t.Fatalf("cloned Tailscale identity is not cleared before login: reset=%d up=%d", resetIndex, upIndex)
	}
	if strings.Contains(script, "if (true)") || strings.Contains(script, "if (false)") {
		t.Fatalf("bootstrap emitted invalid PowerShell boolean literal: %s", script)
	}
	if strings.Contains(script, "-match '^100\\.'") {
		t.Fatalf("bootstrap assumes the official Tailscale IPv4 prefix: %s", script)
	}
}

func TestHyperVTailscaleDefersNoLANExitNodeUntilTailnetSelection(t *testing.T) {
	cfg := testBackend(&recordingRunner{}).configForRun()
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.AuthKey = "fixture-only-invalid-value"
	cfg.Tailscale.Hostname = "crabbox-blue"
	cfg.Tailscale.ExitNode = "exit.example.ts.net"
	cfg.Tailscale.ExitNodeAllowLANAccess = false

	windowsScript := windowsTailscaleBootstrapPowerShell(cfg)
	if strings.Contains(windowsScript, "$upArgs.Add('--exit-node=exit.example.ts.net')") {
		t.Fatalf("Windows bootstrap enabled the exit node before tailnet selection: %s", windowsScript)
	}
	if !strings.Contains(windowsScript, "Set-CrabboxTailscaleMetadata 'exit-node' 'exit.example.ts.net'") {
		t.Fatalf("Windows bootstrap did not retain desired exit-node metadata: %s", windowsScript)
	}
	linuxCfg := hypervTailscaleBootstrapConfig(cfg)
	if linuxCfg.Tailscale.ExitNode != cfg.Tailscale.ExitNode || !linuxCfg.Tailscale.DeferExitNode ||
		!linuxCfg.Tailscale.ResetIdentityBeforeUp {
		t.Fatalf("Linux bootstrap config=%#v original=%#v", linuxCfg.Tailscale, cfg.Tailscale)
	}
	labels := map[string]string{}
	markHyperVTailscaleLabels(labels, cfg)
	if labels[hypervTailscaleDeferredExitNodeLabel] != "true" {
		t.Fatalf("deferred exit-node labels=%v", labels)
	}
	linuxUserData := core.CloudInitUserData(linuxCfg, "ssh-ed25519 test")
	if strings.Contains(linuxUserData, "tailscale up --auth-key=file:/dev/stdin --hostname='crabbox-blue' --advertise-tags='' --exit-node=") ||
		!strings.Contains(linuxUserData, "printf '%s\\n' 'exit.example.ts.net' > /var/lib/crabbox/tailscale-exit-node") {
		t.Fatalf("Linux deferred exit-node user-data=%s", linuxUserData)
	}
	resetIndex := strings.Index(linuxUserData, "test ! -e /var/lib/tailscale/tailscaled.state")
	upIndex := strings.Index(linuxUserData, "tailscale up --auth-key=file:/dev/stdin")
	if resetIndex < 0 || upIndex <= resetIndex {
		t.Fatalf("Linux identity reset did not precede Tailscale login: reset=%d up=%d", resetIndex, upIndex)
	}
}

func TestWindowsTailscaleBootstrapIsValidPowerShell(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell parser is available on Windows")
	}
	cfg := testBackend(&recordingRunner{}).configForRun()
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.Hostname = "crabbox-blue"
	script := windowsTailscaleBootstrapPowerShell(cfg)
	parser := `$script = [Console]::In.ReadToEnd()
$tokens = $null
$errors = $null
[System.Management.Automation.Language.Parser]::ParseInput($script, [ref]$tokens, [ref]$errors) | Out-Null
if ($errors.Count -gt 0) { $errors | ForEach-Object { [Console]::Error.WriteLine($_.Message) }; exit 1 }`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", parser)
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("PowerShell parse failed: %v\n%s", err, out)
	}
}

func TestWindowsTailscaleAuthKeyUsesEnvironmentAndGuestArgument(t *testing.T) {
	runner := &recordingRunner{}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.AuthKey = "fixture-only-invalid-value"
	cfg.Tailscale.Hostname = "crabbox-blue"

	if err := b.bootstrapWindowsTailscale(context.Background(), "crabbox-blue", cfg.HyperV.User, cfg); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%d want 1", len(runner.calls))
	}
	call := runner.calls[0]
	command := strings.Join(call.Args, " ")
	if strings.Contains(command, cfg.Tailscale.AuthKey) {
		t.Fatal("Tailscale auth key leaked into host command argv")
	}
	if !strings.Contains(command, "-ArgumentList $env:_CRABBOX_TS_AUTHKEY") ||
		!strings.Contains(command, "param([string]$authKey)") {
		t.Fatalf("host command does not pass the auth key as a remoting argument: %s", command)
	}
	if got := requestEnv(call, "_CRABBOX_TS_AUTHKEY"); got != cfg.Tailscale.AuthKey {
		t.Fatalf("auth key environment=%q", got)
	}
}

func TestWindowsTailscaleRequiresAuthKeyBeforeGuestInvocation(t *testing.T) {
	runner := &recordingRunner{}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.Tailscale.Enabled = true

	if err := b.bootstrapWindowsTailscale(context.Background(), "crabbox-blue", cfg.HyperV.User, cfg); err == nil {
		t.Fatal("expected missing Tailscale auth key to fail")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("guest calls=%d want 0", len(runner.calls))
	}
}

func TestWindowsTailscaleBootstrapFailureLogsOutWithoutRetryingJoin(t *testing.T) {
	joinCalls := 0
	logoutCalls := 0
	ctx, cancel := context.WithCancel(context.Background())
	runner := &recordingRunner{
		respond: func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
			script := commandScript(req)
			switch {
			case strings.Contains(script, "$upArgs.Add('up')"):
				joinCalls++
				cancel()
				return core.LocalCommandResult{Stderr: "metadata write failed"}, errors.New("guest command failed"), true
			case strings.Contains(script, " logout "):
				logoutCalls++
				return core.LocalCommandResult{}, nil, true
			default:
				return core.LocalCommandResult{}, nil, false
			}
		},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.AuthKey = "fixture-only-invalid-value"
	cfg.Tailscale.Hostname = "crabbox-blue"

	if err := b.bootstrapWindowsTailscale(ctx, "crabbox-blue", cfg.HyperV.User, cfg); err == nil {
		t.Fatal("expected bootstrap failure")
	}
	if joinCalls != 1 || logoutCalls != 1 {
		t.Fatalf("join calls=%d logout calls=%d", joinCalls, logoutCalls)
	}
}

func TestLinuxTailscaleCleanupUsesResolvedKeyOnlyTarget(t *testing.T) {
	b := testBackend(&recordingRunner{})
	var captured core.SSHTarget
	b.logoutTailscale = func(_ context.Context, target core.SSHTarget) (string, error) {
		captured = target
		return "", nil
	}
	target := core.SSHTarget{
		User:        "runner",
		Host:        "192.0.2.10",
		Key:         `C:\keys\lease`,
		Port:        sshPort,
		TargetOS:    core.TargetLinux,
		AuthSecret:  false,
		WindowsMode: "",
	}
	b.logoutLinuxTailscaleBestEffort(target)
	if captured.Host != target.Host || captured.Key != target.Key || captured.TargetOS != core.TargetLinux || captured.AuthSecret {
		t.Fatalf("cleanup target=%#v", captured)
	}
}

func TestUpdateTailscaleMetadataRequiresExactClaimAndPersistsAtomically(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	b := testBackend(&recordingRunner{})
	cfg := b.configForRun()
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.AuthKey = "fixture-only-invalid-value"
	leaseID := "cbx_abcdef123456"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now().UTC())
	labels["instance"] = name
	labels["ssh_user"] = cfg.HyperV.User
	labels["ssh_port"] = sshPort
	labels["work_root"] = cfg.HyperV.WorkRoot
	server := core.Server{CloudID: name, Provider: providerName, Name: name, Labels: labels}
	target := core.SSHTarget{
		User:        cfg.HyperV.User,
		Host:        "192.0.2.10",
		Port:        sshPort,
		TargetOS:    core.TargetWindows,
		WindowsMode: core.WindowsModeNormal,
	}
	lease := core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}
	req := core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}}
	if err := persistLease(leaseID, slug, name, cfg, req, &lease, nil); err != nil {
		t.Fatal(err)
	}

	meta := core.TailscaleMetadata{
		Enabled:  true,
		Hostname: "crabbox-blue",
		FQDN:     "crabbox-blue.example.ts.net",
		IPv4:     "100.64.1.9",
		State:    "ready",
		Version:  windowsTailscaleVersion,
		DeviceID: "device-123",
	}
	updatedServer, err := b.UpdateTailscaleMetadata(context.Background(), lease, meta)
	if err != nil {
		t.Fatal(err)
	}
	if updatedServer.Labels["tailscale_state"] != "ready" || updatedServer.Labels["tailscale_ipv4"] != meta.IPv4 {
		t.Fatalf("updated labels=%v", updatedServer.Labels)
	}
	claim, ok, exact, err := core.ResolveLeaseClaimForProviderWithExact(leaseID, providerName)
	if err != nil || !ok || !exact {
		t.Fatalf("resolve claim ok=%v exact=%v err=%v", ok, exact, err)
	}
	if claim.TailscaleIPv4 != meta.IPv4 || claim.TailscaleFQDN != meta.FQDN ||
		claim.SSHHost != target.Host || claim.Labels["tailscale_state"] != "ready" {
		t.Fatalf("claim=%#v", claim)
	}
	encoded, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), cfg.Tailscale.AuthKey) {
		t.Fatal("Tailscale auth key persisted in the lease claim")
	}

	mismatched := lease
	mismatched.Server.CloudID = name + "-other"
	mismatched.Server.Labels = map[string]string{"instance": name + "-other"}
	if _, err := b.UpdateTailscaleMetadata(context.Background(), mismatched, meta); err == nil {
		t.Fatal("expected mismatched Hyper-V claim identity to be rejected")
	}
}

func TestReleaseRunsTailscaleCleanupBeforeVMRemoval(t *testing.T) {
	testutil.IsolateUserDirs(t)

	var events []string
	leaseID := "cbx_123456abcdef"
	slug := "release"
	name := core.LeaseProviderName(leaseID, slug)
	runner := &recordingRunner{
		onRun: func(req core.LocalCommandRequest) {
			if strings.Contains(commandScript(req), "Remove-VM") {
				events = append(events, "remove")
			}
		},
		respond: func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
			if strings.Contains(commandScript(req), "Select-Object Name, State | ConvertTo-Json") {
				return core.LocalCommandResult{Stdout: `{"Name":"` + name + `","State":2}`}, nil, true
			}
			return core.LocalCommandResult{}, nil, false
		},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.Tailscale.Enabled = true
	labels := core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", false, time.Now().UTC())
	labels["instance"] = name
	labels["ssh_user"] = cfg.HyperV.User
	labels["ssh_port"] = sshPort
	labels["work_root"] = cfg.HyperV.WorkRoot
	labels["state"] = "ready"
	server := core.Server{CloudID: name, Provider: providerName, Name: name, Labels: labels}
	target := core.SSHTarget{
		User:        cfg.HyperV.User,
		Host:        "192.0.2.10",
		Port:        sshPort,
		TargetOS:    core.TargetWindows,
		WindowsMode: core.WindowsModeNormal,
	}
	lease := core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}
	if err := persistLease(leaseID, slug, name, cfg, core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}}, &lease, nil); err != nil {
		t.Fatal(err)
	}

	req := core.ReleaseLeaseRequest{
		Lease: lease,
		GuardedRemoteCleanup: func(_ context.Context, cleanupLease core.LeaseTarget) {
			if cleanupLease.SSH.TargetOS != core.TargetWindows ||
				!strings.Contains(core.TailscaleIdentityResetScript(cleanupLease.SSH.TargetOS, cleanupLease.SSH.WindowsMode), "Stop-Service -Name Tailscale") {
				t.Fatalf("cleanup lease=%#v", cleanupLease)
			}
			events = append(events, "cleanup")
		},
	}
	if err := b.ReleaseLease(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	cleanupIndex, removeIndex := -1, -1
	for i, event := range events {
		switch event {
		case "cleanup":
			cleanupIndex = i
		case "remove":
			if removeIndex < 0 {
				removeIndex = i
			}
		}
	}
	if cleanupIndex < 0 || removeIndex <= cleanupIndex {
		t.Fatalf("release events=%v", events)
	}
}
