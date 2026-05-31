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
	if !target.NoControlMaster || !target.DisableHostKeyChecking {
		t.Fatalf("tenki target should disable SSH mux and persistent host keys: %#v", target)
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

func TestTenkiSSHMaterialPathsUsesManagedTenkiState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".config", "tenki", "ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pubKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest tenki-test\n"
	if err := os.WriteFile(filepath.Join(sshDir, "id_ed25519.pub"), []byte(pubKey), 0o600); err != nil {
		t.Fatal(err)
	}

	keyPath, certPath, err := tenkiSSHMaterialPaths("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if keyPath != filepath.Join(sshDir, "id_ed25519") {
		t.Fatalf("keyPath=%q", keyPath)
	}
	wantPrefix := filepath.Join(home, ".config", "tenki", "ssh-certs", "session-1") + string(os.PathSeparator)
	if !strings.HasPrefix(certPath, wantPrefix) || !strings.HasSuffix(certPath, "-cert.pub") {
		t.Fatalf("certPath=%q want prefix %q and -cert.pub suffix", certPath, wantPrefix)
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
