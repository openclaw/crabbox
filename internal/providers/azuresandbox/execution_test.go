package azuresandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestSandboxBootstrapScriptAndPrivateEnvironment(t *testing.T) {
	f := &fixture{}
	b, acquire := fixtureBackend(t, f)
	if _, err := b.acquire(t.Context(), acquire); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	b.rt.Stdout, b.rt.Stderr = &stdout, &stderr
	directory := t.TempDir()
	b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/executeShellCommand") {
			return f.request(t, r)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		// Exercise shell quoting against a real process, with only the fake host's
		// workspace path redirected to this test's isolated directory.
		command := strings.ReplaceAll(body["command"], "/workspace", directory)
		cmd := exec.CommandContext(r.Context(), "bash", "-c", command)
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		output, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				code = e.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		data, _ := json.Marshal(map[string]any{"exitCode": code, "stdout": string(output), "stderr": ""})
		return response(200, string(data)), nil
	})
	secret := "synthetic ' $(false) ; credential"
	result, err := b.Run(t.Context(), core.RunRequest{ID: acquire.RequestedLeaseID, Repo: acquire.Repo, Keep: true, NoSync: true,
		Env: map[string]string{"BOOTSTRAP_TOKEN": secret}, ScriptRequested: true,
		Script: &core.RunScriptSpec{Source: "stdin", Data: []byte("test \"$BOOTSTRAP_TOKEN\" = \"$1\" && printf bootstrap-ok")}, Command: []string{secret}})
	if err != nil || result.ExitCode != 0 || stdout.String() != "bootstrap-ok" {
		t.Fatalf("bootstrap: %+v %v stdout=%q", result, err, stdout.String())
	}
	if strings.Contains(stderr.String(), secret) {
		t.Fatal("credential leaked to diagnostics")
	}
}

func TestSandboxUnknownCommandRetainsLease(t *testing.T) {
	f := &fixture{}
	b, acquire := fixtureBackend(t, f)
	if _, err := b.acquire(t.Context(), acquire); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/executeShellCommand") {
			return f.request(t, r)
		}
		cancel()
		return nil, context.Canceled
	})
	_, err := b.Run(ctx, core.RunRequest{ID: acquire.RequestedLeaseID, Repo: acquire.Repo, Keep: true, NoSync: true, Command: []string{"true"}})
	if err == nil {
		t.Fatal("unknown command outcome reported success")
	}
	claim, err := b.claim(acquire.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent.State != "acquired" || f.deletes != 0 {
		t.Fatal("uncertain execution released ownership")
	}
}

func TestSandboxExpiredAllocationCannotExecuteOrRenew(t *testing.T) {
	f := &fixture{}
	b, acquire := fixtureBackend(t, f)
	if _, err := b.acquire(t.Context(), acquire); err != nil {
		t.Fatal(err)
	}
	claim, err := b.claim(acquire.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	claim.FixedCreateIntent.Attempt["expires_at"] = time.Now().Add(-time.Second).Format(time.RFC3339Nano)
	if _, err := executionDeadline(claim); err == nil {
		t.Fatal("expired allocation admitted")
	}
	claim.FixedCreateIntent.State = "released"
	claim.FixedCreateIntent.Attempt["expires_at"] = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := executionDeadline(claim); err == nil {
		t.Fatal("released allocation admitted")
	}
}

func TestSandboxInterruptedStopBlocksActivityAndAllowsRetry(t *testing.T) {
	f := &fixture{}
	b, acquire := fixtureBackend(t, f)
	if _, err := b.acquire(t.Context(), acquire); err != nil {
		t.Fatal(err)
	}
	b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodDelete {
			return response(503, "unavailable"), nil
		}
		return f.request(t, r)
	})
	if err := b.Stop(t.Context(), core.StopRequest{ID: acquire.RequestedLeaseID}); err == nil {
		t.Fatal("failed deletion reported success")
	}
	before, err := b.claim(acquire.RequestedLeaseID)
	if err != nil || before.FixedCreateIntent.Journal.Phase != "deleting" || f.box.State != "Running" {
		t.Fatalf("expected retained deleting claim and live resource: %+v %v", before, err)
	}
	requests := 0
	b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		return f.request(t, r)
	})
	file := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(file, []byte("must not upload"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"run", "copy", "heartbeat"} {
		// Reconstruct the adapter and reload the persisted claim for every call.
		fresh := &backend{cfg: b.cfg, rt: b.rt, api: b.api}
		var err error
		switch operation {
		case "run":
			_, err = fresh.Run(t.Context(), core.RunRequest{ID: acquire.RequestedLeaseID, Repo: acquire.Repo, Keep: true, NoSync: true, Command: []string{"true"}})
		case "copy":
			err = fresh.Copy(t.Context(), core.CopyRequest{ID: acquire.RequestedLeaseID, RepoRoot: acquire.Repo.Root, Source: file, Destination: "SANDBOX:/tmp/input"})
		case "heartbeat":
			_, err = fresh.Heartbeat(t.Context(), core.LeaseHeartbeatRequest{ID: acquire.RequestedLeaseID})
		}
		if err == nil || !strings.Contains(err.Error(), "cleanup is in progress") || requests != 0 {
			t.Fatalf("%s: err=%v requests=%d", operation, err, requests)
		}
		after, err := b.claim(acquire.RequestedLeaseID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("%s changed retained claim: %v", operation, err)
		}
	}
	if err := b.Stop(t.Context(), core.StopRequest{ID: acquire.RequestedLeaseID}); err != nil {
		t.Fatal(err)
	}
	if f.deletes != 1 || f.box != nil {
		t.Fatal("exact deletion retry did not finish")
	}
}

var _ core.DelegatedRunBackend = (*backend)(nil)
var _ core.DelegatedFixedWarmupBackend = (*backend)(nil)
var _ core.LeaseHeartbeatBackend = (*backend)(nil)
