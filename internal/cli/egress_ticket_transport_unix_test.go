//go:build !windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Prove the shared primitives independently of the caller and receiver tests.
func TestEgressTicketExistingProtectedInputPrimitives(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	const ticket = "egress_0123456789abcdef0123456789abcdef"
	script := `#!/bin/sh
printf '%s\n' "$@" > "$CRABBOX_TEST_EGRESS_ARGV"
for arg do remote="$arg"; done
exec /bin/sh -c "$remote"
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(dir, "argv")
	t.Setenv("PATH", dir)
	t.Setenv("CRABBOX_TEST_EGRESS_ARGV", argsPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var delivered bytes.Buffer
	err := runSSHInput(ctx, SSHTarget{Host: "127.0.0.1", User: "crabbox", Port: "1", TargetOS: targetLinux}, "/bin/cat", strings.NewReader(ticket), &delivered, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), ticket) {
		t.Fatal("existing SSH input transport exposed synthetic ticket in argv")
	}
	got, err := readWebVNCDaemonCredentialStdin(&delivered)
	if err != nil || got != ticket {
		t.Fatalf("bounded credential reader received %q, err=%v", got, err)
	}
}

// All executable endpoints are local fixtures; the remote shell is real, but
// its binary/log paths are remapped and pkill cannot reach any real process.
func TestEgressStartTicketTransportKeepsCredentialOffArgv(t *testing.T) {
	testEgressStartTicketTransport(t, "")
}

func TestEgressStartTicketTransportFailureStopsBeforeHostMint(t *testing.T) {
	for _, failure := range []string{"ssh", "readiness"} {
		t.Run(failure, func(t *testing.T) { testEgressStartTicketTransport(t, failure) })
	}
}

func testEgressStartTicketTransport(t *testing.T, failure string) {
	clearConfigEnv(t)
	dir := t.TempDir()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("local shell observation fixture requires python3")
	}
	const ticket = "egress_0123456789abcdef0123456789abcdef"
	const leaseID = "cbx_0123456789ab"
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write("helper", "#!"+python+"\n"+`import json, os, sys
from pathlib import Path
p = Path(os.environ["CRABBOX_TEST_EGRESS_OBSERVATIONS"])
payload = sys.stdin.buffer.read()
(p / "helper.json").write_text(json.dumps({"argv": sys.argv, "stdin": payload.decode()}))
`)
	write("ssh", "#!"+python+"\n"+`import json, os, subprocess, sys, time
from pathlib import Path
p = Path(os.environ["CRABBOX_TEST_EGRESS_OBSERVATIONS"])
remote = sys.argv[-1]
if "--internal-ticket-stdin" not in remote:
    if "command -v nc" in remote and os.environ.get("CRABBOX_TEST_EGRESS_FAILURE") == "readiness":
        sys.exit(42)
    sys.exit(0) # install and readiness are fixtures, never real SSH or probes
payload = sys.stdin.buffer.read()
(p / "ssh.json").write_text(json.dumps({"argv": sys.argv, "stdin": payload.decode()}))
if os.environ.get("CRABBOX_TEST_EGRESS_FAILURE") == "ssh":
    sys.exit(42)
remote = remote.replace("/tmp/crabbox-egress-client.log", str(p / "helper.log"))
remote = remote.replace("/tmp/crabbox-egress-client", str(p / "helper"))
argv = ["/bin/sh", "-c", remote]
(p / "shell.json").write_text(json.dumps({"argv": argv}))
subprocess.run(argv, input=payload, check=True, timeout=5)
deadline = time.monotonic() + 5
while not (p / "helper.json").exists():
    if time.monotonic() > deadline:
        sys.exit(91)
    time.sleep(0.01)
`)
	write("pkill", "#!/bin/sh\nexit 0\n")
	write("scp", "#!/bin/sh\nexit 0\n")
	write("go", "#!/bin/sh\nexit 0\n")
	// nohup must be real; every other executable named by the remote start is
	// fixture-owned. Neither binary installation nor process cleanup runs here.
	nohup, err := exec.LookPath("nohup")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nohup, filepath.Join(dir, "nohup")); err != nil {
		t.Fatal(err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(git, filepath.Join(dir, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("CRABBOX_TEST_EGRESS_OBSERVATIONS", dir)
	t.Setenv("CRABBOX_TEST_EGRESS_FAILURE", failure)
	var clientMints, hostMints atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+leaseID:
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: leaseID, Provider: "aws", TargetOS: targetLinux, State: "active",
				Host: "127.0.0.1", SSHUser: "crabbox", SSHPort: "1",
				SSHFallbackPorts: []string{"1"}, WorkRoot: "/work/my-app",
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/"+leaseID+"/egress/ticket":
			var body struct{ Role, SessionID string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				http.Error(w, "invalid fixture request", 400)
				return
			}
			if body.Role == "host" {
				hostMints.Add(1)
				// End the foreground caller after the observed remote start.
				http.Error(w, "synthetic fixture finished", http.StatusForbidden)
				return
			}
			if body.Role != "client" || !strings.HasPrefix(body.SessionID, "egress_") {
				t.Errorf("unexpected ticket binding: %+v", body)
			}
			clientMints.Add(1)
			_ = json.NewEncoder(w).Encode(CoordinatorEgressTicket{
				Ticket: ticket, LeaseID: leaseID, Role: "client", SessionID: body.SessionID,
				ExpiresAt: time.Now().Add(120 * time.Second).Format(time.RFC3339),
			})
		default:
			t.Errorf("unexpected fixture request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	write("config.yaml", "provider: aws\nbroker:\n  url: "+server.URL+"\n  token: synthetic-coordinator-fixture\n")
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "config.yaml"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = (App{Stdout: io.Discard, Stderr: io.Discard}).egressStart(ctx, []string{
		"--provider", "aws", "--id", leaseID, "--allow", "example.com", "--network", "public",
	})
	wantErr := "synthetic fixture finished"
	wantHostMints := int32(1)
	stages := []string{"ssh", "shell", "helper"}
	if failure == "ssh" {
		wantErr, wantHostMints = "start remote egress client", 0
		stages = []string{"ssh"}
	} else if failure == "readiness" {
		wantErr, wantHostMints = "remote egress client did not listen", 0
	}
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("caller did not finish at the fixture boundary: %v", err)
	}
	if strings.Contains(err.Error(), ticket) {
		t.Fatal("ticket exposed in automatic caller error")
	}
	if clientMints.Load() != 1 || hostMints.Load() != wantHostMints {
		t.Fatalf("ticket requests: client=%d host=%d", clientMints.Load(), hostMints.Load())
	}
	for _, stage := range stages {
		var observation struct {
			Argv  []string `json:"argv"`
			Stdin string   `json:"stdin"`
		}
		data, err := os.ReadFile(filepath.Join(dir, stage+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &observation); err != nil {
			t.Fatal(err)
		}
		for i, arg := range observation.Argv {
			if strings.Contains(arg, ticket) {
				t.Errorf("%s argv[%d] exposes synthetic ticket: %q", stage, i, arg)
			}
		}
		if stage != "shell" && observation.Stdin != ticket {
			t.Errorf("%s protected stdin received %q, want exact synthetic ticket bytes", stage, observation.Stdin)
		}
	}
}
