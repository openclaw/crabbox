//go:build linux

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/crabbox/internal/remoteruntime"
	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

func TestLinuxNativeNoSyncRoundTrips(t *testing.T) {
	// Build the real supervisor without unrelated CLI/provider dependencies.
	// The transport still executes it locally without an SSH server.
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	pack := t.TempDir()
	fixture := t.TempDir()
	main := filepath.Join(fixture, "main.go")
	source := `package main
import (
 "context"
 "os"
 "os/signal"
 "syscall"
 "github.com/openclaw/crabbox/internal/remoteruntime"
)
func main() {
 if len(os.Args) < 2 || os.Args[1] != remoteruntime.Command { os.Exit(74) }
 ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
 go func() { <-ctx.Done(); stop() }()
 code := remoteruntime.RunCLI(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr)
 stop()
 os.Exit(code)
}
`
	if err := os.WriteFile(main, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{filepath.Join(root, "cmd/crabbox-runtime/main.go"): main}})
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(fixture, "overlay.json")
	if err := os.WriteFile(overlayPath, overlay, 0600); err != nil {
		t.Fatal(err)
	}
	var inputs []runtimeartifact.ArtifactInput
	for _, arch := range []string{"amd64", "arm64"} {
		name := "linux-" + arch
		cmd := exec.Command("go", "build", "-overlay", overlayPath, "-trimpath", "-o", filepath.Join(pack, name), "./cmd/crabbox-runtime")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, out)
		}
		inputs = append(inputs, runtimeartifact.ArtifactInput{Target: runtimeartifact.Target{OS: "linux", Arch: arch}, Path: name})
	}
	controller, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := runtimeartifact.MarshalLocal(t.Context(), pack, controller, inputs, remoteruntime.Protocol)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(pack, "manifest.json")
	if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	p, b, dir := setupSSHScriptRun(t)
	// Keep the no-op baseline independent of the runner's forwarded environment.
	// The final run exercises forwarding explicitly under the same call ceiling.
	for _, name := range []string{"CI", "NODE_OPTIONS"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(runtimeArtifactsEnv, manifestPath)
	b.lease.SSH.ReadyCheck = "true"
	b.lease.SSH.AuthSecret = false
	b.lease.SSH.SSHConfigProxy = false
	b.lease.SSH.Host = "127.0.0.1"
	b.lease.SSH.Port = startTCPReadinessFixture(t)
	log := filepath.Join(dir, "calls")
	t.Setenv("NATIVE_CALLS", log)
	t.Setenv("NATIVE_CACHE_ROOT", filepath.Join(dir, "runtime-cache"))
	writeExecutable(t, filepath.Join(dir, "ssh"), `#!/bin/sh
printf 'CALL %s\n' "$*" >> "$NATIVE_CALLS"
for arg; do remote=$arg; done
case "$remote" in *'/usr/local/bin/crabbox-ready'*) exit 0;; esac
remote=$(printf %s "$remote" | sed "s#/tmp/crabbox-runtime-cache-#$NATIVE_CACHE_ROOT-#g")
exec /bin/sh -c "$remote"
`)
	counts := make([]int, 4)
	for run := range counts {
		if run == 2 {
			cached := filepath.Join(dir, fmt.Sprintf("runtime-cache-%d", os.Geteuid()), "artifact")
			if err := os.Remove(cached); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cached, []byte("partial runtime"), 0500); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(log, nil, 0600); err != nil {
			t.Fatal(err)
		}
		script := "true\n"
		if run == 3 {
			t.Setenv("CI", "true")
			script = "test \"$CI\" = true\n"
		}
		var stdout, stderr bytes.Buffer
		started := time.Now()
		err = (App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(script)}).runCommand(t.Context(), []string{
			"--provider", p.Spec().Name, "--id", b.lease.LeaseID, "--network", "public", "--tailscale=false", "--keep", "--no-sync", "--script-stdin",
		})
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("run: %v\n%s\n%s", err, &stdout, &stderr)
		}
		calls, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		count := strings.Count(string(calls), "CALL ")
		counts[run] = count
		t.Logf("no-sync run %d: %d SSH calls in %s", run+1, count, elapsed)
		if uploaded := strings.Contains(string(calls), "upload.part"); uploaded != (run == 0 || run == 2) {
			t.Errorf("run %d runtime upload = %v", run+1, uploaded)
		}
		nonces := regexp.MustCompile(`CBX-REMOTE-1[^\n]*?([a-f0-9]{32})`).FindAllSubmatch(calls, -1)
		if len(nonces) == 0 {
			t.Fatal("run did not use the native supervisor")
		}
		for _, match := range nonces {
			if _, err := os.Lstat("/tmp/crabbox-command-" + string(match[1])); !os.IsNotExist(err) {
				t.Fatalf("successful settlement did not retire command stage %s: %v", match[1], err)
			}
		}
		if count > 20 {
			t.Errorf("no-sync run used %d SSH calls; want at most 20\n%s", count, calls)
		}
	}
	if counts[1] >= counts[0] {
		t.Errorf("cache hit used %d calls; first run used %d", counts[1], counts[0])
	}
}
