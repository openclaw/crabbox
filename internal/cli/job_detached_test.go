package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDetachedJobCompletionAndCollision(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native Linux job contract")
	}
	for _, code := range []int{0, 37, 255} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			home := t.TempDir()
			key, token := workspaceOwnerKey("fixture"), strings.Repeat("a", 64)
			call := func(action workspaceOwnerAction, token string) (string, error) {
				return runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(workspaceOwnerRemoteRequest{Action: action, Key: key, Token: token, TTL: time.Minute}))
			}
			if out, err := call(workspaceOwnerAcquire, token); err != nil || out != "ACQUIRED" {
				t.Fatalf("acquire: %q %v", out, err)
			}
			job := filepath.Join(home, ".crabbox", "jobs", key, "trial")
			if err := os.MkdirAll(job, 0o700); err != nil {
				t.Fatal(err)
			}
			if out, err := call(workspaceOwnerDetach, token); err != nil || out != "DETACHED" {
				t.Fatalf("detach: %q %v", out, err)
			}
			// The launch returns before the workload completes, with all streams
			// disconnected. The same protocol is transported through SSH.
			command := fmt.Sprintf("printf 'first\\nsecond\\n'; sleep 1; exit %d", code)
			if out, err := runPOSIXWorkspaceOwnerScript(t, home, detachedJobLaunchScript(key, token, "trial", command)); err != nil || out != "" {
				t.Fatalf("launch: %q %v", out, err)
			}
			if out, err := call(workspaceOwnerAcquire, strings.Repeat("b", 64)); err != nil || out != "CHILD" {
				t.Fatalf("collision: %q %v", out, err)
			}
			if out, err := call(workspaceOwnerRelease, token); err == nil || out != "CHILD" {
				t.Fatalf("ordinary release must not clear detached authority: %q %v", out, err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				state, _ := os.ReadFile(filepath.Join(job, "state"))
				if string(state) == "complete\n" {
					break
				}
				if time.Now().After(deadline) {
					log, _ := os.ReadFile(filepath.Join(job, "supervisor.log"))
					t.Fatalf("job did not finish: %s", log)
				}
				time.Sleep(20 * time.Millisecond)
			}
			out, err := runPOSIXWorkspaceOwnerScript(t, home, detachedJobReadScript(key, "trial", "status", 1))
			if err != nil {
				t.Fatal(err)
			}
			status, err := parseDetachedJobStatus("trial", out)
			if err != nil || status.State != "exited" || status.PID <= 0 || status.ExitCode == nil || *status.ExitCode != code {
				t.Fatalf("status=%+v err=%v", status, err)
			}
			out, err = runPOSIXWorkspaceOwnerScript(t, home, detachedJobReadScript(key, "trial", "logs", 1))
			if err != nil || out != "second" {
				t.Fatalf("bounded logs=%q err=%v", out, err)
			}
			if out, err := call(workspaceOwnerAcquire, strings.Repeat("b", 64)); err != nil || out != "ACQUIRED" {
				t.Fatalf("completed job retained lock: %q %v", out, err)
			}
		})
	}
}

func TestDetachedJobAmbiguousDispatchRetainsReservation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native Linux job contract")
	}
	home := t.TempDir()
	key, token := workspaceOwnerKey("fixture"), strings.Repeat("a", 64)
	for _, action := range []workspaceOwnerAction{workspaceOwnerAcquire, workspaceOwnerDetach} {
		if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(workspaceOwnerRemoteRequest{Action: action, Key: key, Token: token, TTL: time.Minute})); err != nil {
			t.Fatalf("%s: %s %v", action, out, err)
		}
	}
	// Simulate owner expiry with no recorded child: dispatch may have reached
	// the host, so absence alone must not authorize a new workspace writer.
	state := filepath.Join(home, ".crabbox", "workspace-owners", key+".owner")
	if err := os.WriteFile(state, []byte("v1\n"+token+"\n1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: key, Token: strings.Repeat("b", 64), TTL: time.Minute}))
	if err != nil || out != "CHILD" {
		t.Fatalf("expired detached dispatch was stolen: %q %v", out, err)
	}
}

func TestDetachedJobInputAndStatusContracts(t *testing.T) {
	for _, operation := range []string{"submit", "status", "logs", "push", "pull"} {
		var stdout, stderr bytes.Buffer
		err := (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"job", operation})
		if ExitCodeForError(err, 1) != 2 || stdout.Len() != 0 {
			t.Fatalf("%s accepted missing lease: %v %s", operation, err, stdout.String())
		}
	}
	for _, input := range []string{"", "exited 123", "exited 123 256", "exited 123 -1", "running -1", "running 123 0", "success 123", "missing extra"} {
		if status, err := parseDetachedJobStatus("trial", input); err == nil {
			t.Fatalf("accepted corrupt status %q: %+v", input, status)
		}
	}
	for _, input := range []string{"missing", "unknown 0", "running 123", "exited 123 255"} {
		if _, err := parseDetachedJobStatus("trial", input); err != nil {
			t.Fatalf("rejected status %q: %v", input, err)
		}
	}
	for _, input := range []string{"", ".", "../output", "/output", "a/../../b", "a/.git/config", "a/.crabbox/state", "a\\b", "a\nb"} {
		if safeDetachedJobPath(input) {
			t.Fatalf("accepted unsafe workspace path %q", input)
		}
	}
	for _, input := range []string{"results/output.json", "space and 'quote/file", "src"} {
		if !safeDetachedJobPath(input) {
			t.Fatalf("rejected workspace path %q", input)
		}
	}
}
