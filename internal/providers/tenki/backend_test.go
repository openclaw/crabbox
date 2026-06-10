package tenki

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTenkiProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != tenkiProvider || spec.Kind != "ssh-lease" || spec.Coordinator != "never" {
		t.Fatalf("unexpected spec: %#v", spec)
	}
	if !spec.Features.Has("ssh") || !spec.Features.Has("crabbox-sync") {
		t.Fatalf("missing SSH lease features: %#v", spec.Features)
	}
}

func TestTenkiCreateAddsMetadata(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		args := strings.Join(req.Args, " ")
		if strings.HasPrefix(args, "sandbox create --endpoint https://api.tenki.test --workspace ws_1 --project proj_1 --no-wait --output json --name crabbox-blue ") {
			for _, want := range []string{
				"--metadata crabbox_provider=tenki",
				"--metadata crabbox_lease_id=cbx_123",
				"--metadata crabbox_slug=blue",
				"--metadata crabbox_idle_timeout_secs=1800",
				"--metadata crabbox_ttl_secs=3600",
				"--metadata crabbox_server_type=ubuntu:tenki",
				"--tags crabbox,crabbox-provider-tenki",
				"--sticky",
				"--max-duration 1h0m0s",
				"--idle-timeout 30m0s",
				"--cpu 4",
				"--memory-mb 8192",
				"--disk-size-gb 40",
				"--image ubuntu:tenki",
			} {
				if !strings.Contains(args, want) {
					t.Fatalf("create args missing %q:\n%s", want, args)
				}
			}
			return LocalCommandResult{Stdout: `{"id":"00000000-0000-0000-0000-000000000001"}`, ExitCode: 0}, nil
		}
		switch args {
		case "sandbox get --endpoint https://api.tenki.test --output json 00000000-0000-0000-0000-000000000001":
			return LocalCommandResult{Stdout: `{"id":"00000000-0000-0000-0000-000000000001","name":"crabbox-blue","state":"RUNNING","metadata":{"crabbox_provider":"tenki","crabbox_lease_id":"cbx_123","crabbox_slug":"blue"},"tags":["crabbox-provider-tenki"]}`}, nil
		default:
			t.Fatalf("unexpected command: %s %s", req.Name, args)
		}
		return LocalCommandResult{}, nil
	}
	backend := &tenkiBackend{
		cfg: Config{
			TTL:         time.Hour,
			IdleTimeout: 30 * time.Minute,
			Tenki: TenkiConfig{
				CLIPath:   "tenki",
				Endpoint:  "https://api.tenki.test",
				Workspace: "ws_1",
				Project:   "proj_1",
				Image:     "ubuntu:tenki",
				CPUs:      4,
				MemoryMB:  8192,
				DiskGB:    40,
				WorkRoot:  "/home/tenki/crabbox",
			},
		},
		rt: Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard},
	}

	session, err := backend.createSession(context.Background(), backend.configForRun(), "crabbox-blue", "cbx_123", "blue", true)
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "00000000-0000-0000-0000-000000000001" {
		t.Fatalf("session id=%q", session.ID)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls=%d want 2", len(runner.calls))
	}
}

func TestTenkiCreateTrimsImageAndSnapshotOptions(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		args := strings.Join(req.Args, " ")
		if strings.HasPrefix(args, "sandbox create ") {
			if strings.Contains(args, "--image") {
				t.Fatalf("whitespace image should be omitted:\n%s", args)
			}
			if !strings.Contains(args, "--snapshot snap-ready") {
				t.Fatalf("snapshot was not trimmed/emitted:\n%s", args)
			}
			return LocalCommandResult{Stdout: `{"id":"session-1"}`}, nil
		}
		if args == "sandbox get --output json session-1" {
			return LocalCommandResult{Stdout: `{"id":"session-1","name":"snap","state":"RUNNING"}`}, nil
		}
		t.Fatalf("unexpected command: %s %s", req.Name, args)
		return LocalCommandResult{}, nil
	}
	backend, err := NewTenkiBackend(ProviderSpec{}, Config{Tenki: TenkiConfig{
		CLIPath:  "tenki",
		Image:    "   ",
		Snapshot: "  snap-ready  ",
	}}, Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := backend.(*tenkiBackend).createSession(context.Background(), backend.(*tenkiBackend).configForRun(), "crabbox-snap", "cbx_123", "snap", true); err != nil {
		t.Fatal(err)
	}
}

func TestTenkiValidationRejectsNegativeResources(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{name: "cpus", cfg: Config{Tenki: TenkiConfig{CPUs: -1}}},
		{name: "memory", cfg: Config{Tenki: TenkiConfig{MemoryMB: -1}}},
		{name: "disk", cfg: Config{Tenki: TenkiConfig{DiskGB: -1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateTenkiOptions(tc.cfg); err == nil {
				t.Fatal("expected negative resource value to fail")
			}
		})
	}
	if err := validateTenkiOptions(Config{Tenki: TenkiConfig{CPUs: 0, MemoryMB: 0, DiskGB: 0}}); err != nil {
		t.Fatalf("zero resource sentinels should remain valid: %v", err)
	}
}

func TestTenkiApplyFlagsNormalizesImageAndSnapshot(t *testing.T) {
	provider := Provider{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := provider.RegisterFlags(fs, Config{})
	if err := fs.Parse([]string{"--tenki-image", "  ubuntu:tenki  "}); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Provider: tenkiProvider}
	if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Tenki.Image != "ubuntu:tenki" {
		t.Fatalf("image=%q, want trimmed value", cfg.Tenki.Image)
	}
}

func TestTenkiCreateRequiresJSONSessionID(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		if strings.Contains(strings.Join(req.Args, " "), "sandbox get") {
			t.Fatalf("unexpected get after unparsable create output: %s", strings.Join(req.Args, " "))
		}
		return LocalCommandResult{Stdout: "created sandbox\n", ExitCode: 0}, nil
	}
	backend := &tenkiBackend{
		cfg: Config{Tenki: TenkiConfig{CLIPath: "tenki"}},
		rt:  Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard},
	}
	if _, err := backend.createSession(context.Background(), backend.configForRun(), "crabbox-blue", "cbx_123", "blue", true); err == nil {
		t.Fatal("expected createSession to reject invalid create output")
	} else if !strings.Contains(err.Error(), "parse tenki sandbox create JSON") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTenkiResolveStatusOnlyDoesNotPrepareSSH(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		switch strings.Join(req.Args, " ") {
		case "sandbox list --output json --tags crabbox,crabbox-provider-tenki":
			return LocalCommandResult{Stdout: `[{"id":"session-1","name":"crabbox-blue","state":"PAUSED","metadata":{"crabbox_provider":"tenki","crabbox_lease_id":"cbx_123","crabbox_slug":"blue"},"tags":["crabbox-provider-tenki"]}]`}, nil
		default:
			t.Fatalf("status-only resolve should not prepare SSH, got command: %s %s", req.Name, strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &tenkiBackend{
		cfg: Config{Tenki: TenkiConfig{CLIPath: "tenki"}},
		rt:  Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard},
	}

	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "cbx_123", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Status != "paused" || lease.SSH.Host != "" {
		t.Fatalf("unexpected status-only lease: %#v", lease)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%d want 1", len(runner.calls))
	}
}

func TestTenkiResolveReadyProbePreparesSSH(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	certPath := filepath.Join(dir, "id_ed25519-cert.pub")
	if err := os.WriteFile(keyPath, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		switch strings.Join(req.Args, " ") {
		case "sandbox list --output json --tags crabbox,crabbox-provider-tenki":
			return LocalCommandResult{Stdout: `[{"id":"session-1","name":"crabbox-blue","state":"RUNNING","metadata":{"crabbox_provider":"tenki","crabbox_lease_id":"cbx_123","crabbox_slug":"blue"},"tags":["crabbox-provider-tenki"]}]`}, nil
		case "sandbox ssh-command --output json --session session-1 --user tenki --batch-mode --connect-timeout 10s":
			return LocalCommandResult{Stdout: `{"session_id":"session-1","user":"tenki","host":"sandbox","port":22,"identity_file":"` + keyPath + `","certificate_file":"` + certPath + `","proxy_command":"tenki sandbox ssh-proxy --session session-1"}`}, nil
		default:
			t.Fatalf("unexpected command: %s %s", req.Name, strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &tenkiBackend{
		cfg: Config{Tenki: TenkiConfig{CLIPath: "tenki"}},
		rt:  Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard},
	}

	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "cbx_123", StatusOnly: true, ReadyProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Host != "sandbox" || lease.SSH.Key != keyPath {
		t.Fatalf("ready probe did not prepare SSH target: %#v", lease.SSH)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls=%d want 2", len(runner.calls))
	}
}

func TestTenkiResolveReadyProbeDoesNotResumePausedSession(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		switch strings.Join(req.Args, " ") {
		case "sandbox list --output json --tags crabbox,crabbox-provider-tenki":
			return LocalCommandResult{Stdout: `[{"id":"session-1","name":"crabbox-blue","state":"PAUSED","metadata":{"crabbox_provider":"tenki","crabbox_lease_id":"cbx_123","crabbox_slug":"blue"},"tags":["crabbox-provider-tenki"]}]`}, nil
		default:
			t.Fatalf("paused readiness probe mutated session: %s %s", req.Name, strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &tenkiBackend{
		cfg: Config{Tenki: TenkiConfig{CLIPath: "tenki"}},
		rt:  Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard},
	}

	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "cbx_123", StatusOnly: true, ReadyProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Status != "paused" || lease.SSH.Host != "" {
		t.Fatalf("unexpected paused readiness probe lease: %#v", lease)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%d want 1", len(runner.calls))
	}
}

func TestTenkiResolveClaimUsesStoredSessionID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "tenki_session-1"
	if err := claimLeaseForRepoProvider(leaseID, "adopted", tenkiProvider, t.TempDir(), time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimEndpoint(leaseID, Server{Labels: map[string]string{"tenki_session_id": "session-1"}}, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		switch strings.Join(req.Args, " ") {
		case "sandbox get --output json session-1":
			return LocalCommandResult{Stdout: `{"id":"session-1","name":"unmanaged","state":"RUNNING"}`}, nil
		default:
			t.Fatalf("claim resolve should use stored session id, got command: %s %s", req.Name, strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &tenkiBackend{
		cfg: Config{Tenki: TenkiConfig{CLIPath: "tenki"}},
		rt:  Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard},
	}

	session, gotLeaseID, slug, err := backend.resolveSession(context.Background(), "adopted", false)
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "session-1" || gotLeaseID != leaseID || slug != "adopted" {
		t.Fatalf("resolved session=%#v lease=%q slug=%q", session, gotLeaseID, slug)
	}
}

func TestTenkiResolveReclaimPersistsSessionEndpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	certPath := filepath.Join(dir, "id_ed25519-cert.pub")
	if err := os.WriteFile(keyPath, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldWait := waitForSSHReadyFunc
	waitForSSHReadyFunc = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		return nil
	}
	t.Cleanup(func() { waitForSSHReadyFunc = oldWait })

	runner := &fakeRunner{}
	var commands []string
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		command := strings.Join(req.Args, " ")
		commands = append(commands, command)
		switch command {
		case "sandbox get --output json session-1":
			return LocalCommandResult{Stdout: `{"id":"session-1","name":"unmanaged","state":"RUNNING"}`}, nil
		case "sandbox ssh-command --output json --session session-1 --user tenki --batch-mode --connect-timeout 10s":
			return LocalCommandResult{Stdout: `{"session_id":"session-1","user":"tenki","host":"sandbox","port":22,"identity_file":"` + keyPath + `","certificate_file":"` + certPath + `","proxy_command":"tenki proxy session-1"}`}, nil
		default:
			t.Fatalf("unexpected command: %s %s", req.Name, command)
		}
		return LocalCommandResult{}, nil
	}
	backend := &tenkiBackend{
		cfg: Config{Tenki: TenkiConfig{CLIPath: "tenki"}},
		rt:  Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard},
	}

	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "session-1", Reclaim: true, Repo: Repo{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "tenki_session-1" {
		t.Fatalf("lease id=%q", lease.LeaseID)
	}
	claim, ok, err := resolveLeaseClaim("unmanaged")
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.Labels["tenki_session_id"] != "session-1" {
		t.Fatalf("claim labels=%v, want stored tenki session id", claim.Labels)
	}
	commands = nil
	lease, err = backend.Resolve(context.Background(), ResolveRequest{ID: "unmanaged", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "session-1" {
		t.Fatalf("resolved server=%s, want session-1", lease.Server.CloudID)
	}
	if len(commands) != 1 || commands[0] != "sandbox get --output json session-1" {
		t.Fatalf("commands=%v, want stored session get only", commands)
	}
}

func TestTenkiEnsureSessionReadyResumesPausedSession(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		switch strings.Join(req.Args, " ") {
		case "sandbox resume --session session-1":
			return LocalCommandResult{ExitCode: 0}, nil
		case "sandbox get --output json session-1":
			return LocalCommandResult{Stdout: `{"id":"session-1","state":"RUNNING"}`}, nil
		default:
			t.Fatalf("unexpected command: %s %s", req.Name, strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &tenkiBackend{
		cfg: Config{Tenki: TenkiConfig{CLIPath: "tenki"}},
		rt:  Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard},
	}

	session, err := backend.ensureSessionReadyForSSH(context.Background(), backend.configForRun(), tenkiSession{ID: "session-1", State: "PAUSED"})
	if err != nil {
		t.Fatal(err)
	}
	if session.State != "RUNNING" {
		t.Fatalf("state=%q", session.State)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls=%d want 2", len(runner.calls))
	}
}

func TestTenkiEnsureSessionReadySurfacesResumeFailure(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		switch strings.Join(req.Args, " ") {
		case "sandbox resume --session session-1":
			return LocalCommandResult{ExitCode: 0}, nil
		case "sandbox get --output json session-1":
			return LocalCommandResult{Stdout: `{"id":"session-1","state":"PAUSED","last_resume_error":"capacity unavailable"}`}, nil
		default:
			t.Fatalf("unexpected command: %s %s", req.Name, strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &tenkiBackend{
		cfg: Config{Tenki: TenkiConfig{CLIPath: "tenki"}},
		rt:  Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard},
	}

	_, err := backend.ensureSessionReadyForSSH(context.Background(), backend.configForRun(), tenkiSession{ID: "session-1", State: "PAUSED"})
	if err == nil || !strings.Contains(err.Error(), "capacity unavailable") {
		t.Fatalf("err=%v, want resume failure", err)
	}
}

func TestTenkiSSHTargetUsesProxyCommand(t *testing.T) {
	backend := &tenkiBackend{cfg: Config{Tenki: TenkiConfig{
		CLIPath:  "/opt/Tenki CLI/tenki",
		Endpoint: "https://api.tenki.test",
		Gateway:  "wss://gateway.tenki.test",
	}}}
	target := backend.sshTarget(tenkiSSHCommandOutput{
		SessionID:       "00000000-0000-0000-0000-000000000001",
		User:            "tenki",
		Host:            "sandbox",
		Port:            22,
		IdentityFile:    "/tmp/id_ed25519",
		CertificateFile: "/tmp/session-cert.pub",
		ProxyCommand:    "'/opt/Tenki CLI/tenki' sandbox ssh-proxy --session 00000000-0000-0000-0000-000000000001 --endpoint https://api.tenki.test --gateway wss://gateway.tenki.test",
	})
	if !target.SSHConfigProxy || target.Host != "sandbox" || target.User != "tenki" || target.Key != "/tmp/id_ed25519" || target.CertificateFile != "/tmp/session-cert.pub" {
		t.Fatalf("unexpected target: %#v", target)
	}
	if target.NoControlMaster || target.DisableHostKeyChecking {
		t.Fatalf("tenki target should keep SSH mux and host-key checks enabled: %#v", target)
	}
	if target.KnownHostsFile != "/tmp/known_hosts_00000000-0000-0000-0000-000000000001" {
		t.Fatalf("known_hosts=%q", target.KnownHostsFile)
	}
	for _, want := range []string{
		`"/opt/Tenki CLI/tenki" sandbox ssh-proxy`,
		"--session 00000000-0000-0000-0000-000000000001",
		"--endpoint https://api.tenki.test",
		"--gateway wss://gateway.tenki.test",
	} {
		if !strings.Contains(target.ProxyCommand, want) {
			t.Fatalf("proxy command %q missing %q", target.ProxyCommand, want)
		}
	}
}

func TestTenkiOpenSSHProxyCommandNormalizesSingleQuotes(t *testing.T) {
	got := tenkiOpenSSHProxyCommand(`'/opt/Tenki CLI/tenki' 'sandbox' 'ssh-proxy' '--session' 'session-1' '--gateway' 'wss://edge.example/v1/ssh/session-1'`)
	want := `"/opt/Tenki CLI/tenki" sandbox ssh-proxy --session session-1 --gateway wss://edge.example/v1/ssh/session-1`
	if got != want {
		t.Fatalf("proxy=%q want %q", got, want)
	}
}

func TestTenkiSessionToServerDoesNotExposeSessionIDAsIP(t *testing.T) {
	backend := &tenkiBackend{}
	server := backend.sessionToServer(Config{}, tenkiSession{ID: "session-1", Name: "crabbox-blue", State: "RUNNING"}, "cbx_123", "blue", true)
	if server.PublicNet.IPv4.IP != "" {
		t.Fatalf("ip=%q, want empty", server.PublicNet.IPv4.IP)
	}
	if server.CloudID != "session-1" || server.Labels["tenki_session_id"] != "session-1" {
		t.Fatalf("session id not preserved in server metadata: %#v", server)
	}
}

func TestTenkiListJSONUsesCrabboxLeaseID(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		if got := strings.Join(req.Args, " "); got != "sandbox list --output json --tags crabbox,crabbox-provider-tenki" {
			t.Fatalf("unexpected command: %s %s", req.Name, got)
		}
		return LocalCommandResult{Stdout: `[{"id":"session-1","name":"crabbox-blue","state":"RUNNING","metadata":{"crabbox_provider":"tenki","crabbox_lease_id":"cbx_123","crabbox_slug":"blue"},"tags":["crabbox-provider-tenki"]}]`}, nil
	}
	backend := &tenkiBackend{
		cfg: Config{Tenki: TenkiConfig{CLIPath: "tenki"}},
		rt:  Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard},
	}

	raw, err := backend.ListJSON(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	views, ok := raw.([]tenkiLeaseListView)
	if !ok || len(views) != 1 {
		t.Fatalf("unexpected JSON list view: %#v", raw)
	}
	view := views[0]
	if view.ID != "cbx_123" || view.ServerID != "session-1" || view.Slug != "blue" || view.Provider != tenkiProvider || view.State != "ready" {
		t.Fatalf("unexpected JSON list entry: %#v", view)
	}
}

func TestTenkiSessionToServerPreservesLeaseTimingMetadata(t *testing.T) {
	backend := &tenkiBackend{}
	server := backend.sessionToServer(Config{
		Class:       "beast",
		ProviderKey: "default-provider-key",
		Tenki: TenkiConfig{
			Image: "ubuntu:tenki",
		},
	}, tenkiSession{
		ID:    "session-1",
		Name:  "crabbox-blue",
		State: "RUNNING",
		Metadata: map[string]string{
			tenkiMetadataProvider:       tenkiProvider,
			tenkiMetadataLease:          "cbx_123",
			tenkiMetadataSlug:           "blue",
			"crabbox_created_at":        "1700000000",
			"crabbox_expires_at":        "1700001800",
			"crabbox_idle_timeout":      "600",
			"crabbox_idle_timeout_secs": "600",
			"crabbox_last_touched_at":   "1700000000",
			"crabbox_provider_key":      "tenki-provider-key",
			"crabbox_server_type":       "sandbox",
			"crabbox_ttl_secs":          "1800",
		},
	}, "cbx_123", "blue", true)
	for key, want := range map[string]string{
		"created_at":        "1700000000",
		"expires_at":        "1700001800",
		"idle_timeout":      "600",
		"idle_timeout_secs": "600",
		"last_touched_at":   "1700000000",
		"provider_key":      "tenki-provider-key",
		"server_type":       "ubuntu:tenki",
		"ttl_secs":          "1800",
	} {
		if got := server.Labels[key]; got != want {
			t.Fatalf("label %s=%q want %q; labels=%#v", key, got, want, server.Labels)
		}
	}
	if server.ServerType.Name != "ubuntu:tenki" {
		t.Fatalf("server type=%q", server.ServerType.Name)
	}
}

func TestTenkiWaitForSSHCommandUsesStructuredOutput(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	certPath := filepath.Join(dir, "id_ed25519-cert.pub")
	if err := os.WriteFile(keyPath, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		switch strings.Join(req.Args, " ") {
		case "sandbox ssh-command --output json --session session-1 --user tenki --batch-mode --connect-timeout 10s":
			return LocalCommandResult{Stdout: `{"session_id":"session-1","user":"tenki","host":"sandbox","port":22,"identity_file":"` + keyPath + `","certificate_file":"` + certPath + `","proxy_command":"tenki sandbox ssh-proxy --session session-1"}`}, nil
		default:
			t.Fatalf("unexpected command: %s %s", req.Name, strings.Join(req.Args, " "))
		}
		return LocalCommandResult{}, nil
	}
	backend := &tenkiBackend{
		cfg: Config{Tenki: TenkiConfig{CLIPath: "tenki"}},
		rt:  Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard},
	}
	output, err := backend.waitForTenkiSSHCommand(context.Background(), "session-1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if output.IdentityFile != keyPath || output.CertificateFile != certPath || output.ProxyCommand == "" {
		t.Fatalf("unexpected ssh-command output: %#v", output)
	}
}

type fakeRunner struct {
	calls []LocalCommandRequest
	run   func(LocalCommandRequest) (LocalCommandResult, error)
}

func (f *fakeRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	if f.run == nil {
		return LocalCommandResult{}, errors.New("unexpected command")
	}
	return f.run(req)
}
