package localcontainer

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Opt-in executable proof. All runtime and SSH commands are synthetic fixtures;
// neither an installed Docker/Podman daemon nor a remote SSH server is contacted.
func TestMemoryDiagnosticsNativeCLI(t *testing.T) {
	binary := os.Getenv("CRABBOX_MEMORY_CLI_BINARY")
	if binary == "" {
		t.Skip("set CRABBOX_MEMORY_CLI_BINARY to an explicitly built CLI")
	}
	root := t.TempDir()
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, root)
	}
	for _, key := range []string{"CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_TOKEN", "DOCKER_CONTEXT", "DOCKER_HOST", "CONTAINER_HOST", "CONTAINER_CONNECTION"} {
		t.Setenv(key, "")
	}
	leaseID, containerID, claim, runner := createLocalContainerTouchClaim(t, time.Hour)
	config := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(config, []byte("provider: local-container\nlocalContainer:\n  runtime: docker\n  memory: 24g\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", config)
	t.Setenv("MEMORY_FIXTURE_ROOT", root)
	t.Setenv("PATH", root+string(os.PathListSeparator)+"/usr/bin:/bin")
	var containers []inspectContainer
	if err := json.Unmarshal([]byte(runner.responses[commandKey([]string{"inspect", containerID})].Stdout), &containers); err != nil {
		t.Fatal(err)
	}
	containers[0].HostConfig = json.RawMessage(`{"Memory":12884901888,"MemorySwap":0}`)
	containers[0].State = inspectState{Status: "exited"}
	writeContainer := func() {
		data, err := json.Marshal(containers)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "inspect.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeContainer()
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$MEMORY_FIXTURE_ROOT/runtime.log"
case "$1" in --context|--connection) shift 2 ;; esac
case "$1" in
 ps) printf '%s\n' '` + containerID + `' ;;
 inspect) /bin/cat "$MEMORY_FIXTURE_ROOT/inspect.json" ;;
 context) case "$2" in show) printf 'default\n' ;; inspect) printf 'unix:///tmp/docker touch.sock\n' ;; *) exit 96 ;; esac ;;
 info) case "$*" in *MemTotal*) printf 'daemon-test\n8317267968\n' ;; *) printf 'daemon-test\n' ;; esac ;;
 exec) if [ -f "$MEMORY_FIXTURE_ROOT/oom" ]; then printf 'oom_kill 6\n'; else printf 'oom_kill 5\n'; fi ;;
 *) exit 96 ;;
esac
`
	if err := os.WriteFile(filepath.Join(root, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	invoke := func(name string, args ...string) (string, string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"), "HOME=" + root,
			"XDG_STATE_HOME=" + os.Getenv("XDG_STATE_HOME"),
			"XDG_CONFIG_HOME=" + root, "XDG_CACHE_HOME=" + root,
			"CRABBOX_CONFIG=" + config, "MEMORY_FIXTURE_ROOT=" + root,
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if dir := os.Getenv("CRABBOX_MEMORY_CLI_RECEIPT_DIR"); dir != "" {
			code := -1
			if cmd.ProcessState != nil {
				code = cmd.ProcessState.ExitCode()
			}
			data, _ := json.MarshalIndent(map[string]any{"args": args, "stdout": stdout.String(), "stderr": stderr.String(), "exit": code}, "", "  ")
			if err := os.WriteFile(filepath.Join(dir, name+".json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return stdout.String(), stderr.String(), err
	}
	for _, command := range []string{"inspect", "status"} {
		stdout, stderr, err := invoke(command, command, "--provider", providerName, "--id", leaseID, "--json")
		if err != nil {
			t.Fatalf("%s: %v\n%s", command, err, stderr)
		}
		var view struct{ Labels map[string]string }
		if err := json.Unmarshal([]byte(stdout), &view); err != nil {
			t.Fatal(err)
		}
		if view.Labels[memoryDiagnosticPrefix+"container_memory_limit_bytes"] != "12884901888" || view.Labels[memoryDiagnosticPrefix+"runtime_memory_total_bytes"] != "8317267968" {
			t.Fatalf("actual CLI lost fresh memory facts: %s", stdout)
		}
	}
	after, err := core.ReadLeaseClaim(leaseID)
	if err != nil || after.Revision != claim.Revision {
		t.Fatal("native inspection changed claim")
	}
	// The executable uses its normal run path, but SSH only interprets fixture
	// markers. It never executes the supplied remote shell payload.
	ssh := `#!/bin/sh
for arg do if [ "$arg" = -G ]; then exec /usr/bin/ssh "$@"; fi; done
for arg do current=$arg; done
n=0
while [ "$n" -lt 8 ]; do
 case "$current" in
 *'payload_b64="'*'"; decoded=; if command -v base64'*)
  payload=${current#*'payload_b64="'}; payload=${payload%%'"; decoded=; if command -v base64'*}
  current=$(printf %s "$payload" | /usr/bin/base64 --decode) || exit 96; n=$((n+1)) ;;
 *) break ;;
 esac
done
case "$current" in
 *"protocol_action='acquire'"*) printf ACQUIRED ;;
 *"protocol_action='renew'"*) printf RENEWED ;;
 *"protocol_action='inspect'"*) printf OWNED ;;
 *"protocol_action='release'"*) printf RELEASED ;;
 *fixture-memory-command*) : > "$MEMORY_FIXTURE_ROOT/oom"; exit 137 ;;
 *fixture-ordinary-command*) exit 7 ;;
 *'tar -cz'*) exit 1 ;;
 *) exit 0 ;;
esac
`
	if err := os.WriteFile(filepath.Join(root, "ssh"), []byte(ssh), 0o755); err != nil {
		t.Fatal(err)
	}
	containers[0].State = inspectState{Status: "running", Running: true}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	containers[0].NetworkSettings.Ports[sshPort+"/tcp"] = []inspectPort{{HostIP: "127.0.0.1", HostPort: port}}
	writeContainer()
	claim.Labels["ssh_port"] = port
	gitRoot, err := exec.Command("/usr/bin/git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	server := core.Server{Provider: providerName, CloudID: containerID, Status: "ready", Labels: claim.Labels}
	server.PublicNet.IPv4.IP = "127.0.0.1"
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, claim.Slug, providerName, claim.ProviderScope, "", strings.TrimSpace(string(gitRoot)), time.Hour, true, server, core.SSHTarget{Host: "127.0.0.1", Port: port, User: "runner"}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		command string
		code    int
		oom     bool
	}{{"fixture-memory-command", 137, true}, {"fixture-ordinary-command", 7, false}, {"fixture-success-command", 0, false}} {
		before, _ := os.ReadFile(filepath.Join(root, "runtime.log"))
		_, stderr, err := invoke(test.command, "run", "--provider", providerName, "--id", leaseID, "--keep", "--no-sync", "--no-hydrate", "--timing-json", "--", test.command)
		var report core.TimingReport
		for _, line := range strings.Split(stderr, "\n") {
			if strings.HasPrefix(line, "{") {
				_ = json.Unmarshal([]byte(line), &report)
			}
		}
		if (err != nil) != (test.code != 0) || report.ExitCode != test.code {
			t.Fatalf("native run did not reach fixture exit %d: %v\n%s", test.code, err, stderr)
		}
		if got := report.ResourceExhaustion == core.ResourceExhaustionMemory; got != test.oom {
			t.Fatalf("native OOM=%t want %t\n%s", got, test.oom, stderr)
		}
		if test.oom && (report.FailureEvidence == nil || !strings.Contains(report.FailureEvidence.Hint, "does not add RAM")) {
			t.Fatal("native run lost contextual evidence")
		}
		wantDigests := 1
		if test.code == 0 {
			wantDigests = 0
		}
		if strings.Count(stderr, "failure digest\n") != wantDigests {
			t.Fatal("native digest not emitted once")
		}
		after, _ := os.ReadFile(filepath.Join(root, "runtime.log"))
		wantProbes := 0
		if test.oom {
			wantProbes = 1
		}
		if got := strings.Count(string(after), "MemTotal") - strings.Count(string(before), "MemTotal"); got != wantProbes {
			t.Fatalf("native capacity probes=%d want %d", got, wantProbes)
		}
		t.Logf("synthetic executable %s exit=%d OOM=%t", test.command, test.code, test.oom)
	}
	containers[0].HostConfig = json.RawMessage(`{"Memory":1073741824,"MemorySwap":2147483648}`)
	containers[0].State = inspectState{Status: "exited"}
	writeContainer()
	stdout, stderr, err := invoke("inspect-after-run", "inspect", "--provider", providerName, "--id", leaseID, "--json")
	if err != nil || !strings.Contains(stdout, `"diagnostic.memory.container_memory_limit_bytes":"1073741824"`) {
		t.Fatalf("retained inspection was not fresh: %v\n%s\n%s", err, stdout, stderr)
	}
	log, _ := os.ReadFile(filepath.Join(root, "runtime.log"))
	if strings.Contains(string(log), "\nrm ") || strings.Contains(string(log), "\nrun ") {
		t.Fatal("retained fixture attempted a lifecycle mutation")
	}
}
