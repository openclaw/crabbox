package tenki

import (
	"context"
	"errors"
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
		switch strings.Join(req.Args, " ") {
		case "sandbox create --endpoint https://api.tenki.test --workspace ws_1 --project proj_1 --no-wait --name crabbox-blue --metadata crabbox_provider=tenki --metadata crabbox_lease_id=cbx_123 --metadata crabbox_slug=blue --tags crabbox,crabbox-provider-tenki --sticky --max-duration 1h0m0s --idle-timeout 30m0s --cpu 4 --memory-mb 8192 --disk-size-gb 40 --image ubuntu:tenki":
			return LocalCommandResult{Stdout: "id: 00000000-0000-0000-0000-000000000001\n", ExitCode: 0}, nil
		case "sandbox get --endpoint https://api.tenki.test --json 00000000-0000-0000-0000-000000000001":
			return LocalCommandResult{Stdout: `{"id":"00000000-0000-0000-0000-000000000001","name":"crabbox-blue","state":"RUNNING","metadata":{"crabbox_provider":"tenki","crabbox_lease_id":"cbx_123","crabbox_slug":"blue"},"tags":["crabbox-provider-tenki"]}`}, nil
		default:
			t.Fatalf("unexpected command: %s %s", req.Name, strings.Join(req.Args, " "))
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

func TestTenkiCreateRequiresParsedSessionID(t *testing.T) {
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
		t.Fatal("expected createSession to reject unparsable create output")
	} else if !strings.Contains(err.Error(), "did not return a session id") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTenkiEnsureSessionReadyResumesPausedSession(t *testing.T) {
	runner := &fakeRunner{}
	runner.run = func(req LocalCommandRequest) (LocalCommandResult, error) {
		runner.calls = append(runner.calls, req)
		switch strings.Join(req.Args, " ") {
		case "sandbox resume --session session-1":
			return LocalCommandResult{ExitCode: 0}, nil
		case "sandbox get --json session-1":
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
		case "sandbox get --json session-1":
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
	target := backend.sshTarget("00000000-0000-0000-0000-000000000001", "/tmp/id_ed25519", "/tmp/session-cert.pub")
	if !target.SSHConfigProxy || target.Host != "sandbox" || target.User != "tenki" || target.Key != "/tmp/id_ed25519" || target.CertificateFile != "/tmp/session-cert.pub" {
		t.Fatalf("unexpected target: %#v", target)
	}
	if target.NoControlMaster || !target.DisableHostKeyChecking {
		t.Fatalf("tenki target should keep SSH mux enabled and disable persistent host keys: %#v", target)
	}
	for _, want := range []string{
		"'/opt/Tenki CLI/tenki' sandbox ssh-proxy",
		"--session 00000000-0000-0000-0000-000000000001",
		"--endpoint https://api.tenki.test",
		"--gateway wss://gateway.tenki.test",
	} {
		if !strings.Contains(target.ProxyCommand, want) {
			t.Fatalf("proxy command %q missing %q", target.ProxyCommand, want)
		}
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

func TestTenkiSSHMaterialPathsUsesManagedTenkiState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".config", "tenki", "ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	certDir := filepath.Join(home, ".config", "tenki", "ssh-certs", "session-1")
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldCert := filepath.Join(certDir, "old-cert.pub")
	newCert := filepath.Join(certDir, "new-cert.pub")
	for _, certPath := range []string{oldCert, newCert} {
		if err := os.WriteFile(certPath, []byte("cert"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldCert, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	keyPath, certPath, err := tenkiSSHMaterialPaths("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if keyPath != filepath.Join(sshDir, "id_ed25519") {
		t.Fatalf("keyPath=%q", keyPath)
	}
	if certPath != newCert {
		t.Fatalf("certPath=%q want newest cert %q", certPath, newCert)
	}
}

func TestParseTenkiCreateSessionIDStripsANSI(t *testing.T) {
	got := parseTenkiCreateSessionID("\x1b[1mid:\x1b[0m 00000000-0000-0000-0000-000000000001\n")
	if got != "00000000-0000-0000-0000-000000000001" {
		t.Fatalf("id=%q", got)
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
