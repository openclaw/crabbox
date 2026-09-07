package islo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
	sdkcore "github.com/islo-labs/go-sdk/core"
	core "github.com/openclaw/crabbox/internal/cli"
)

func isolateIsloTestHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}

func TestParseIsloSSE(t *testing.T) {
	body := strings.Join([]string{
		"event: stdout",
		"data: hello",
		"",
		"event: stderr",
		"data: warn",
		"",
		"event: exit",
		"data: 7",
		"",
	}, "\n")
	var stdout, stderr bytes.Buffer
	code, err := parseIsloSSE(strings.NewReader(body), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 || stdout.String() != "hello" || stderr.String() != "warn" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestParseIsloSSERequiresExitEvent(t *testing.T) {
	body := strings.Join([]string{
		"event: stdout",
		"data: partial",
		"",
	}, "\n")
	var stdout, stderr bytes.Buffer
	code, err := parseIsloSSE(strings.NewReader(body), &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "without exit event") {
		t.Fatalf("code=%d err=%v, want missing exit event error", code, err)
	}
	if stdout.String() != "partial" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestParseIsloSSESurfacesErrorEvent(t *testing.T) {
	// The Islo exec SSE stream can emit an "error" event for stream/VM-level
	// failures and may end without an "exit" event. The error payload must
	// surface instead of the generic missing-exit-event message.
	body := strings.Join([]string{
		"event: stdout",
		"data: starting",
		"",
		"event: error",
		"data: vm exec failed: out of memory",
		"",
	}, "\n")
	var stdout, stderr bytes.Buffer
	code, err := parseIsloSSE(strings.NewReader(body), &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("code=%d err=%v, want surfaced error payload", code, err)
	}
	if strings.Contains(err.Error(), "without exit event") {
		t.Fatalf("err=%v, should prefer the error payload over the generic message", err)
	}
	if stdout.String() != "starting" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestParseIsloSSERejectsInvalidExitEvent(t *testing.T) {
	body := strings.Join([]string{
		"event: exit",
		"data: nope",
		"",
	}, "\n")
	if _, err := parseIsloSSE(strings.NewReader(body), &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "invalid exit event") {
		t.Fatalf("err=%v, want invalid exit event error", err)
	}
}

func TestIsloExecCommandPreservesShellString(t *testing.T) {
	got, err := isloExecCommand([]string{"pnpm install && pnpm test"}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bash", "-lc", "pnpm install && pnpm test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command=%#v want %#v", got, want)
	}
}

func TestIsloExecCommandQuotesImplicitShellArgv(t *testing.T) {
	got, err := isloExecCommand([]string{"FOO=bar", "pnpm", "test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "bash" || got[1] != "-lc" || !strings.Contains(got[2], "FOO=") || !strings.Contains(got[2], "'pnpm'") {
		t.Fatalf("command=%#v", got)
	}
}

func TestLeadingEnvAssignmentUsesShell(t *testing.T) {
	if !leadingEnvAssignment([]string{"FOO=bar", "pnpm", "test"}) {
		t.Fatal("expected leading env assignment to require shell")
	}
	if leadingEnvAssignment([]string{"pnpm", "test"}) {
		t.Fatal("plain argv should not require shell")
	}
}

func TestIsloStatusReady(t *testing.T) {
	// The live Islo API emits exactly one ready state, "running" (case-insensitive).
	for _, status := range []string{"running", "RUNNING", " running "} {
		if !isloStatusReady(status) {
			t.Fatalf("expected %q ready", status)
		}
	}
	// Statuses Islo never reports as ready, including the legacy values crabbox
	// used to accept ("ready", "started", "active") that the API no longer emits.
	for _, status := range []string{"starting", "ready", "started", "active", "paused", "stopping", "stopped", "failed", "deleted", "unknown", ""} {
		if isloStatusReady(status) {
			t.Fatalf("status %q should not be ready", status)
		}
	}
}

func TestIsloStatusTerminal(t *testing.T) {
	for _, status := range []string{"failed", "stopped", "stopping", "deleted", "DELETED", " failed "} {
		if !isloStatusTerminal(status) {
			t.Fatalf("expected %q terminal", status)
		}
	}
	for _, status := range []string{"starting", "running", "paused", "unknown", ""} {
		if isloStatusTerminal(status) {
			t.Fatalf("status %q should not be terminal", status)
		}
	}
}

func TestIsloProviderDeclaresPauseResume(t *testing.T) {
	if !(Provider{}).Spec().Features.Has(core.FeaturePauseResume) {
		t.Fatal("islo provider must declare pause-resume")
	}
	if !(Provider{}).Spec().Features.Has(core.FeatureSSH) {
		t.Fatal("islo provider must declare ssh")
	}
}

func TestIsloProviderExposesLoginWithoutSSHLease(t *testing.T) {
	backend := NewIsloBackend((Provider{}).Spec(), Config{}, Runtime{})
	if _, ok := backend.(core.SSHLoginBackend); !ok {
		t.Fatalf("backend=%T does not expose SSH login", backend)
	}
	if _, ok := backend.(core.SSHLeaseBackend); ok {
		t.Fatalf("backend=%T must not expose a Crabbox-managed SSH lease", backend)
	}
}

func TestResolveIsloLeaseIDRejectsUnclaimedRawSandbox(t *testing.T) {
	if _, _, _, err := resolveIsloLeaseID("production", "", false); err == nil {
		t.Fatal("expected raw non-Crabbox sandbox to be rejected")
	}
	for _, name := range []string{
		"crabbox repo abc123",
		"crabbox/repo/abc123",
		"crabbox?repo=abc123",
		"Crabbox-repo-abc123",
	} {
		for _, id := range []string{name, isloLeasePrefix + name} {
			if _, _, _, err := resolveIsloLeaseID(id, "", false); err == nil {
				t.Errorf("resolveIsloLeaseID(%q) accepted a non-canonical sandbox name", id)
			}
		}
	}
	leaseID, name, slug, err := resolveIsloLeaseID("crabbox-repo-abcdef", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "isb_crabbox-repo-abcdef" || name != "crabbox-repo-abcdef" || slug == "" {
		t.Fatalf("lease=%q name=%q slug=%q", leaseID, name, slug)
	}
}

func TestIsloStopRejectsNonCanonicalSandboxBeforeDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{}
	restore := swapNewIsloClient(client)
	t.Cleanup(restore)
	backend := &isloBackend{spec: (Provider{}).Spec(), cfg: Config{Provider: isloProvider}}

	for _, id := range []string{"crabbox repo abc123", "isb_crabbox/repo/abc123"} {
		if err := backend.Stop(context.Background(), StopRequest{ID: id}); err == nil {
			t.Errorf("Stop(%q) accepted a non-canonical sandbox name", id)
		}
	}
	if client.deleteCalls != 0 {
		t.Fatalf("delete calls=%d, want 0", client.deleteCalls)
	}
}

func TestIsloMutationsRejectCanonicalSandboxWithoutExactClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{}
	restore := swapNewIsloClient(client)
	t.Cleanup(restore)
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	for name, mutate := range map[string]func() error{
		"stop":   func() error { return backend.Stop(context.Background(), StopRequest{ID: "crabbox-repo-abcdef"}) },
		"pause":  func() error { return backend.Pause(context.Background(), PauseRequest{ID: "crabbox-repo-abcdef"}) },
		"resume": func() error { return backend.Resume(context.Background(), ResumeRequest{ID: "crabbox-repo-abcdef"}) },
	} {
		t.Run(name, func(t *testing.T) {
			err := mutate()
			if err == nil || !strings.Contains(err.Error(), "no exact local claim") {
				t.Fatalf("error = %v, want exact-claim refusal", err)
			}
		})
	}
	if client.deleteCalls != 0 || client.pausedName != "" || client.resumedName != "" {
		t.Fatalf("provider mutation without claim: delete=%d pause=%q resume=%q", client.deleteCalls, client.pausedName, client.resumedName)
	}
}

func TestIsloStopDeletesExactlyClaimedSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	const leaseID = "isb_crabbox-repo-abcdef"
	if err := claimLeaseForRepoProvider(leaseID, "web", isloProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	target := core.SSHTarget{}
	if err := core.UseLeaseKnownHosts(&target, leaseID); err != nil {
		t.Fatal(err)
	}
	knownHostsDir := filepath.Dir(target.KnownHostsFile)
	client := &fakeIsloSyncClient{}
	restore := swapNewIsloClient(client)
	t.Cleanup(restore)
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	if err := backend.Stop(context.Background(), StopRequest{ID: "web"}); err != nil {
		t.Fatal(err)
	}
	if client.deleteCalls != 1 {
		t.Fatalf("delete calls=%d, want 1", client.deleteCalls)
	}
	if _, err := os.Stat(knownHostsDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease SSH state remains after stop: %v", err)
	}
}

func TestResolveIsloLeaseIDPreservesClaimSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	leaseID := "isb_crabbox-repo-abcdef"
	if err := claimLeaseForRepoProvider(leaseID, "web", isloProvider, root, time.Hour, false); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"web", leaseID, "crabbox-repo-abcdef"} {
		gotLeaseID, name, slug, err := resolveIsloLeaseID(id, root, false)
		if err != nil {
			t.Fatalf("id=%q err=%v", id, err)
		}
		if gotLeaseID != leaseID || name != "crabbox-repo-abcdef" || slug != "web" {
			t.Fatalf("id=%q lease=%q name=%q slug=%q", id, gotLeaseID, name, slug)
		}
	}
}

func TestResolveIsloLeaseIDForRepoRequiresExplicitVerifiedReclaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	backend := &isloBackend{cfg: Config{IdleTimeout: 7 * time.Minute, Pond: "demo"}}
	client := &fakeIsloSyncClient{getSandbox: &gosdk.SandboxResponse{
		ID:     isloTestResourceID,
		Name:   "crabbox-repo-abcdef",
		Status: "running",
	}}

	if _, _, _, err := backend.resolveLeaseIDForRepo(context.Background(), client, "crabbox-repo-abcdef", root, false); err == nil || !strings.Contains(err.Error(), "--reclaim") {
		t.Fatalf("resolve error = %v, want explicit reclaim refusal", err)
	}
	leaseID, _, slug, err := backend.resolveLeaseIDForRepo(context.Background(), client, "crabbox-repo-abcdef", root, true)
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveExactIsloLeaseClaim(leaseID)
	if err != nil || !ok || claim.RepoRoot != root || claim.Pond != "demo" || claim.IdleTimeoutSeconds != 420 || claim.Slug != slug {
		t.Fatalf("claim ok=%t err=%v claim=%#v", ok, err, claim)
	}
}

func TestResolveIsloLeaseIDForRepoDoesNotClaimMissingSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	backend := &isloBackend{}
	client := &fakeIsloSyncClient{getSandboxGone: true}
	leaseID := "isb_crabbox-repo-abcdef"

	if _, _, _, err := backend.resolveLeaseIDForRepo(context.Background(), client, leaseID, t.TempDir(), true); err == nil || !strings.Contains(err.Error(), "refusing to create a local claim") {
		t.Fatalf("resolve error = %v, want missing-sandbox refusal", err)
	}
	if _, ok, err := resolveExactIsloLeaseClaim(leaseID); err != nil || ok {
		t.Fatalf("claim created for missing sandbox: ok=%t err=%v", ok, err)
	}
}

func TestResolveIsloLeaseIDIgnoresSyntheticSlugCollision(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if err := claimLeaseForRepoProvider("isb_crabbox-other-abcdef", "isb-crabbox-repo-abcdef", isloProvider, root, time.Hour, false); err != nil {
		t.Fatal(err)
	}

	leaseID, name, slug, err := resolveIsloLeaseID("crabbox-repo-abcdef", root, false)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "isb_crabbox-repo-abcdef" || name != "crabbox-repo-abcdef" || slug == "isb-crabbox-repo-abcdef" {
		t.Fatalf("lease=%q name=%q slug=%q", leaseID, name, slug)
	}
	leaseID, name, slug, err = resolveIsloLeaseID("isb_crabbox-repo-abcdef", root, false)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "isb_crabbox-repo-abcdef" || name != "crabbox-repo-abcdef" || slug == "isb-crabbox-repo-abcdef" {
		t.Fatalf("explicit lease=%q name=%q slug=%q", leaseID, name, slug)
	}
}

func TestIsloCleanupCommandQuotesLeaseID(t *testing.T) {
	got := isloCleanupCommand("isb_crabbox-repo-a;touch")
	if got != "crabbox stop --provider islo 'isb_crabbox-repo-a;touch'" {
		t.Fatalf("cleanup command=%q", got)
	}
}

func TestIsloWorkspacePathDefaultsUnderWorkspace(t *testing.T) {
	if got, err := isloWorkspacePath(Config{}); err != nil || got != "/workspace/crabbox" {
		t.Fatalf("workspace=%q err=%v", got, err)
	}
	if got, err := isloWorkspacePath(Config{Islo: IsloConfig{Workdir: "repo"}}); err != nil || got != "/workspace/repo" {
		t.Fatalf("workspace=%q err=%v", got, err)
	}
	if got, err := isloWorkspacePath(Config{Islo: IsloConfig{Workdir: "team/repo"}}); err != nil || got != "/workspace/team/repo" {
		t.Fatalf("workspace=%q err=%v", got, err)
	}
}

func TestIsloWorkspacePathRejectsEscapes(t *testing.T) {
	for _, workdir := range []string{"/work/repo", "/etc", "../etc", "repo/../../../etc", ".", "./.."} {
		t.Run(workdir, func(t *testing.T) {
			if got, err := isloWorkspacePath(Config{Islo: IsloConfig{Workdir: workdir}}); err == nil {
				t.Fatalf("workspace=%q, want error for workdir %q", got, workdir)
			}
		})
	}
}

func TestIsloRunRejectsUnsafeWorkdirBeforeProviderClient(t *testing.T) {
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{Workdir: "../etc"}},
		rt:  Runtime{Stderr: io.Discard},
	}
	_, err := backend.Run(context.Background(), RunRequest{NoSync: true})
	if err == nil || !strings.Contains(err.Error(), "escapes /workspace") {
		t.Fatalf("Run err=%v, want workdir containment error", err)
	}
}

func TestIsloResolveSSHUsesSandboxHostnameDefaults(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	root := t.TempDir()
	leaseID := "isb_crabbox-repo-abcdef"
	if err := claimLeaseForRepoProvider(leaseID, "web", isloProvider, root, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{
		getSandbox: &gosdk.SandboxResponse{Name: "crabbox-repo-abcdef", ID: "sandbox-id", Status: "running", Image: "ubuntu"},
	}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test"}},
		rt:  Runtime{Stderr: io.Discard},
	}

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "web", Repo: Repo{Root: root}})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.Name != "crabbox-repo-abcdef" {
		t.Fatalf("lease=%q server=%q", lease.LeaseID, lease.Server.Name)
	}
	if lease.SSH.Host != "crabbox-repo-abcdef.islo" || lease.SSH.User != isloWorkloadUser || lease.SSH.Port != "22" {
		t.Fatalf("ssh target=%#v", lease.SSH)
	}
	if lease.SSH.Key != "" || len(lease.SSH.FallbackPorts) != 0 || !lease.SSH.SSHConfigProxy || lease.SSH.DisableHostKeyChecking {
		t.Fatalf("islo ssh should not force Crabbox's default key or fallback ports: %#v", lease.SSH)
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	wantKnownHosts := filepath.Join(filepath.Dir(keyPath), "known_hosts")
	if lease.SSH.KnownHostsFile != wantKnownHosts {
		t.Fatalf("KnownHostsFile=%q want %q", lease.SSH.KnownHostsFile, wantKnownHosts)
	}
	if lease.Server.PublicNet.IPv4.IP != "crabbox-repo-abcdef.islo" || lease.Server.Labels["ssh_host"] != "crabbox-repo-abcdef.islo" {
		t.Fatalf("server ssh labels=%#v public=%q", lease.Server.Labels, lease.Server.PublicNet.IPv4.IP)
	}
}

func TestIsloResolveSSHHonorsExplicitSSHOverrides(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	leaseID := "isb_crabbox-repo-abcdef"
	if err := claimLeaseForRepoProvider(leaseID, "web", isloProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{
		getSandbox: &gosdk.SandboxResponse{Name: "crabbox-repo-abcdef", Status: "running"},
	}
	restore := swapNewIsloClient(client)
	defer restore()
	cfg := Config{SSHUser: "alice", SSHPort: "2022", SSHKey: "/tmp/islo-key", Islo: IsloConfig{APIKey: "test"}}
	core.MarkSSHUserExplicit(&cfg)
	core.MarkSSHPortExplicit(&cfg)
	core.MarkSSHKeyExplicit(&cfg)
	backend := &isloBackend{cfg: cfg, rt: Runtime{Stderr: io.Discard}}

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.User != "alice" || lease.SSH.Port != "2022" || lease.SSH.Key != "/tmp/islo-key" {
		t.Fatalf("ssh target=%#v", lease.SSH)
	}
}

func TestIsloResolveSSHResumesPausedSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	root := t.TempDir()
	client := &fakeIsloSyncClient{
		getSandbox: &gosdk.SandboxResponse{ID: isloTestResourceID, Name: "crabbox-repo-abcdef", Status: "paused"},
	}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test"}},
		rt:  Runtime{Stderr: io.Discard},
	}

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{
		ID:      "crabbox-repo-abcdef",
		Repo:    Repo{Root: root},
		Reclaim: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.resumeCalls != 1 || lease.Server.Status != "running" {
		t.Fatalf("resumeCalls=%d status=%q", client.resumeCalls, lease.Server.Status)
	}
	if claim, ok, err := resolveExactIsloLeaseClaim(lease.LeaseID); err != nil || !ok || claim.RepoRoot != root {
		t.Fatalf("adopted claim ok=%t err=%v claim=%#v", ok, err, claim)
	}
}

func TestIsloResolveSSHRejectsUnclaimedSandboxBeforeResume(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	client := &fakeIsloSyncClient{
		getSandbox: &gosdk.SandboxResponse{Name: "crabbox-repo-abcdef", Status: "paused"},
	}
	restore := swapNewIsloClient(client)
	t.Cleanup(restore)
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test"}},
		rt:  Runtime{Stderr: io.Discard},
	}

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{
		ID:   "crabbox-repo-abcdef",
		Repo: Repo{Root: t.TempDir()},
	})
	if err == nil || !strings.Contains(err.Error(), "--reclaim") {
		t.Fatalf("Resolve error = %v, want explicit reclaim refusal", err)
	}
	if client.resumeCalls != 0 {
		t.Fatalf("resume calls=%d, want 0", client.resumeCalls)
	}
}

func TestIsloResolveSSHRejectsForeignClaimBeforeResume(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	leaseID := "isb_crabbox-repo-abcdef"
	if err := claimLeaseForRepoProvider(leaseID, "web", isloProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{
		getSandbox: &gosdk.SandboxResponse{Name: "crabbox-repo-abcdef", Status: "paused"},
	}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test"}},
		rt:  Runtime{Stderr: io.Discard},
	}

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{
		ID:   leaseID,
		Repo: Repo{Root: t.TempDir()},
	})
	if err == nil || !strings.Contains(err.Error(), "--reclaim") {
		t.Fatalf("expected ownership error, got %v", err)
	}
	if client.resumeCalls != 0 {
		t.Fatalf("resumeCalls=%d, want 0", client.resumeCalls)
	}
}

func TestIsloResolveSSHWaitsForStartingSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	leaseID := "isb_crabbox-repo-abcdef"
	if err := claimLeaseForRepoProvider(leaseID, "web", isloProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{
		getSandboxes: []*gosdk.SandboxResponse{
			{Name: "crabbox-repo-abcdef", Status: "starting"},
			{Name: "crabbox-repo-abcdef", Status: "running"},
		},
	}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test"}},
		rt:  Runtime{Stderr: io.Discard},
	}

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Status != "running" || len(client.getSandboxes) != 0 {
		t.Fatalf("status=%q remaining responses=%d", lease.Server.Status, len(client.getSandboxes))
	}
}

func TestIsloStatusViewIncludesTailscaleMetadata(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-node-a"
	if err := claimLeaseForRepoProvider(leaseID, "node-a", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscale(leaseID, "100.64.7.7", "node-a.tailnet.example"); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscaleSettings(leaseID, "node-a", []string{"tag:demo"}, "", "exit.tailnet.example", true); err != nil {
		t.Fatal(err)
	}

	view := isloStatusView(leaseID, &gosdk.SandboxResponse{Name: "crabbox-node-a", Status: "running"})
	if view.Tailscale == nil {
		t.Fatal("missing typed Tailscale metadata")
	}
	if !view.Tailscale.Enabled || view.Tailscale.IPv4 != "100.64.7.7" || view.Tailscale.FQDN != "node-a.tailnet.example" || view.Tailscale.State != "ready" {
		t.Fatalf("tailscale metadata=%#v", view.Tailscale)
	}
	if view.Tailscale.Hostname != "node-a" || strings.Join(view.Tailscale.Tags, ",") != "tag:demo" ||
		view.Tailscale.ExitNode != "exit.tailnet.example" || !view.Tailscale.ExitNodeAllowLANAccess {
		t.Fatalf("tailscale settings=%#v", view.Tailscale)
	}
}

func TestIsloStatusViewMarksTailscaleValidationUnknown(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-node-a"
	if err := claimLeaseForRepoProvider(leaseID, "node-a", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscale(leaseID, "100.64.7.7", ""); err != nil {
		t.Fatal(err)
	}

	view := isloStatusView(leaseID, &gosdk.SandboxResponse{Name: "crabbox-node-a", Status: "running"})
	applyIsloTailscaleValidationError(&view, errors.New("tailnet validation unavailable"))
	if view.Tailscale == nil || view.Tailscale.State != "unknown" || view.Tailscale.Error == "" {
		t.Fatalf("tailscale metadata=%#v", view.Tailscale)
	}
	if view.Labels["tailscale_state"] != "unknown" || view.Labels["tailscale_error"] == "" {
		t.Fatalf("tailscale labels=%#v", view.Labels)
	}
}

func TestIsloStatusViewPreservesUnavailableEnrollment(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-node-a"
	if err := claimLeaseForRepoProvider(leaseID, "node-a", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscaleSettings(leaseID, "node-a", []string{"tag:demo"}, "", "", false); err != nil {
		t.Fatal(err)
	}

	view := isloStatusView(leaseID, &gosdk.SandboxResponse{Name: "crabbox-node-a", Status: "running"})
	applyIsloTailscaleValidationError(&view, fmt.Errorf("%w: daemon stopped", core.ErrTailnetPeerUnavailable))
	if view.Tailscale == nil || !view.Tailscale.Enabled || view.Tailscale.State != "unavailable" {
		t.Fatalf("tailscale metadata=%#v", view.Tailscale)
	}
	if view.Tailscale.Hostname != "node-a" || strings.Join(view.Tailscale.Tags, ",") != "tag:demo" {
		t.Fatalf("tailscale settings=%#v", view.Tailscale)
	}
}

func TestIsloStatusViewDoesNotReportStoppedTailscaleReady(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-node-a"
	if err := claimLeaseForRepoProvider(leaseID, "node-a", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscale(leaseID, "100.64.7.7", ""); err != nil {
		t.Fatal(err)
	}

	view := isloStatusView(leaseID, &gosdk.SandboxResponse{Name: "crabbox-node-a", Status: "stopped"})
	if view.Tailscale == nil || view.Tailscale.State != "unavailable" {
		t.Fatalf("stopped tailscale metadata=%#v", view.Tailscale)
	}
	if view.Labels["tailscale_state"] != "unavailable" {
		t.Fatalf("stopped tailscale labels=%#v", view.Labels)
	}
}

func TestIsloClientUsesBoundedDefaultTransport(t *testing.T) {
	api, err := newIsloClient(Config{Islo: IsloConfig{APIKey: "test", BaseURL: "http://127.0.0.1:8787"}}, Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	client, ok := api.(*isloSDKClient)
	if !ok {
		t.Fatalf("api=%T, want *isloSDKClient", api)
	}
	if client.httpClient == nil || client.httpClient == http.DefaultClient {
		t.Fatalf("default http client=%#v, want bounded private client", client.httpClient)
	}
	if client.httpClient.Timeout != 0 {
		t.Fatalf("whole-response timeout=%s, want caller context to govern streams", client.httpClient.Timeout)
	}
	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport=%T, want *http.Transport", client.httpClient.Transport)
	}
	if transport.ResponseHeaderTimeout != isloDefaultResponseHeaderTimeout {
		t.Fatalf("response header timeout=%s, want %s", transport.ResponseHeaderTimeout, isloDefaultResponseHeaderTimeout)
	}
}

type recordingDefaultRoundTripper struct {
	calls int
}

func (r *recordingDefaultRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls++
	return nil, errors.New("deny all")
}

func TestNewIsloClientRejectsUnsupportedDefaultTransport(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	recorder := &recordingDefaultRoundTripper{}
	http.DefaultTransport = recorder

	api, err := newIsloClient(Config{Islo: IsloConfig{APIKey: "test", BaseURL: "http://127.0.0.1:8787"}}, Runtime{})
	if api != nil || err == nil || !strings.Contains(err.Error(), "non-nil *http.Transport") {
		t.Fatalf("api=%#v err=%v, want transport setup error", api, err)
	}
	if recorder.calls != 0 {
		t.Fatalf("custom default invoked %d times, want 0", recorder.calls)
	}
}

func TestNewIsloClientAcceptsExplicitClientWithUnsupportedDefault(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = &recordingDefaultRoundTripper{}
	injectedTransport := &recordingDefaultRoundTripper{}
	injected := &http.Client{Transport: injectedTransport}

	api, err := newIsloClient(Config{Islo: IsloConfig{APIKey: "test", BaseURL: "http://127.0.0.1:8787"}}, Runtime{HTTP: injected})
	if err != nil {
		t.Fatal(err)
	}
	client, ok := api.(*isloSDKClient)
	if !ok {
		t.Fatalf("api=%T, want *isloSDKClient", api)
	}
	if client.httpClient.Transport != injectedTransport {
		t.Fatal("newIsloClient did not preserve the explicit transport")
	}
}

func TestIsloRunReturnsSessionHandleForKeptSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{createName: "crabbox-repo-abcdef"}
	restore := swapNewIsloClient(client)
	defer restore()
	root := t.TempDir()
	if err := os.WriteFile(root+"/go.mod", []byte("module example.test/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test", Workdir: "repo"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: &bytes.Buffer{}},
	}

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:       Repo{Root: root, Name: "repo"},
		Keep:       true,
		TimingJSON: true,
		Command:    []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil {
		t.Fatal("missing session handle")
	}
	if len(client.execRequests) == 0 {
		t.Fatal("missing workload exec request")
	}
	workloadReq := client.execRequests[len(client.execRequests)-1]
	if workloadReq.GetUser() != nil {
		t.Fatalf("plain workload exec user=%v want image default", workloadReq.GetUser())
	}
	got := result.Session
	if got.Provider != isloProvider || got.LeaseID != "isb_crabbox-repo-abcdef" || got.Slug == "" || got.Reused || !got.Kept {
		t.Fatalf("session=%#v", got)
	}
	if got.CleanupCommand != "crabbox stop --provider islo 'isb_crabbox-repo-abcdef'" {
		t.Fatalf("cleanup command=%q", got.CleanupCommand)
	}
	report := decodeLastTimingReport(t, backend.rt.Stderr.(*bytes.Buffer).String())
	if report.RunStatus != "succeeded" || report.ErrorKind != "" {
		t.Fatalf("timing outcome status=%q kind=%q", report.RunStatus, report.ErrorKind)
	}
}

func TestIsloRunTimingJSONClassifiesCommandFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{
		createName: "crabbox-repo-abcdef",
		execCodes:  []int{0, 42},
	}
	restore := swapNewIsloClient(client)
	defer restore()
	var stderr bytes.Buffer
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test", Workdir: "repo"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: &stderr},
	}
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:       Repo{Root: t.TempDir(), Name: "repo"},
		Keep:       true,
		NoSync:     true,
		TimingJSON: true,
		Command:    []string{"false"},
	})
	if err == nil || result.ExitCode != 42 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	report := decodeLastTimingReport(t, stderr.String())
	if report.RunStatus != "failed" || report.ErrorKind != "command-exit" {
		t.Fatalf("timing outcome status=%q kind=%q", report.RunStatus, report.ErrorKind)
	}
}

func TestIsloRunCleanupDeleteUsesBoundedContext(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withIsloCleanupTimeout(t, 20*time.Millisecond)
	client := &fakeIsloSyncClient{createName: "crabbox-repo-abcdef", blockDelete: true}
	restore := swapNewIsloClient(client)
	defer restore()
	var stderr bytes.Buffer
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test", Workdir: "repo"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: &stderr},
	}
	start := time.Now()
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: t.TempDir(), Name: "repo"},
		NoSync:  true,
		Command: []string{"true"},
	})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup err=%v, want retained timeout cause", err)
	}
	if result.ExitCode != 1 || result.ErrorKind != core.RunErrorProvider || result.Session == nil || !result.Session.Kept {
		t.Fatalf("result=%#v", result)
	}
	if client.deleteCalls != 1 {
		t.Fatalf("delete calls=%d want 1", client.deleteCalls)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Run took %s, want bounded cleanup", elapsed)
	}
	var public ExitError
	if !core.AsExitError(err, &public) || public.Code != 1 || !strings.Contains(public.Message, "islo stop failed") {
		t.Fatalf("public code=%d message=%q, want cleanup failure", public.Code, public.Message)
	}
	if _, ok, claimErr := resolveLeaseClaim(result.Session.LeaseID); claimErr != nil || !ok {
		t.Fatalf("recovery claim retained=%v err=%v", ok, claimErr)
	}
}

func TestIsloRunRetrievesRequiredArtifactAndDownloadBeforeStop(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	local := filepath.Join(dir, "manifest.json")
	client := &fakeIsloSyncClient{
		createName: "crabbox-repo-abcdef",
		retrieveFiles: map[string][]byte{
			"reports/manifest.json": []byte(`{"status":"success"}`),
		},
	}
	restore := swapNewIsloClient(client)
	defer restore()
	var stderr bytes.Buffer
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test", Workdir: "repo"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: &stderr},
	}
	result, err := backend.Run(t.Context(), RunRequest{
		Repo:                  Repo{Root: t.TempDir(), Name: "repo"},
		NoSync:                true,
		Command:               []string{"true"},
		RequiredArtifactGlobs: []string{"reports/manifest.json"},
		Downloads:             []string{"reports/manifest.json=" + local},
	})
	if err != nil {
		t.Fatalf("Run err: %v\nstderr=%s", err, stderr.String())
	}
	if result.ExitCode != 0 || len(result.Artifacts) != 1 || result.Artifacts[0].Kind != "delegated-download" {
		t.Fatalf("result=%#v", result)
	}
	if data, err := os.ReadFile(local); err != nil || string(data) != `{"status":"success"}` {
		t.Fatalf("download data=%q err=%v", data, err)
	}
	if !client.commandContains("test \"$resolved\" = \"$expected\"") ||
		!client.commandContains("test \"$opened\" = \"$resolved\"") {
		t.Fatalf("retrieval did not enforce canonical workspace containment: %#v", client.prepareCommands)
	}
	if client.deleteCalls != 1 {
		t.Fatalf("delete calls=%d, want one after retrieval", client.deleteCalls)
	}
}

func TestIsloFetchRunFileUsesWorkloadUser(t *testing.T) {
	client := &fakeIsloSyncClient{
		retrieveFiles: map[string][]byte{"reports/proof.txt": []byte("ok")},
	}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test", Workdir: "repo"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	data, err := backend.fetchRunFileAs(t.Context(), core.DelegatedRunDownloadRequest{
		LeaseID:    "isb_crabbox-repo-abcdef",
		RemotePath: "reports/proof.txt",
		MaxBytes:   core.DelegatedRunDownloadMaxBytes,
	}, isloWorkloadUser)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ok" {
		t.Fatalf("data=%q", data)
	}
	req := client.execRequests[len(client.execRequests)-1]
	if req.GetUser() == nil || *req.GetUser() != isloWorkloadUser {
		t.Fatalf("retrieval user=%v want %q", req.GetUser(), isloWorkloadUser)
	}
	if got := req.GetCommand(); len(got) < 8 || got[0] != "timeout" || got[2] != "15s" || got[4] != "--noprofile" || got[5] != "--norc" {
		t.Fatalf("retrieval command=%#v, want bounded timeout", got)
	}
}

func TestIsloFetchRunFileHonorsContextDeadline(t *testing.T) {
	client := &fakeIsloSyncClient{blockArtifactRead: true}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test", Workdir: "repo"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := backend.fetchRunFileAs(ctx, core.DelegatedRunDownloadRequest{
		LeaseID:    "isb_crabbox-repo-abcdef",
		RemotePath: "reports/proof.txt",
		MaxBytes:   core.DelegatedRunDownloadMaxBytes,
	}, "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("artifact read took %s, want bounded cancellation", elapsed)
	}
}

func TestIsloRunPreservesLocalDownloadExitCode(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	parent := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(parent, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{
		createName:    "crabbox-repo-abcdef",
		retrieveFiles: map[string][]byte{"reports/proof.txt": []byte("ok")},
	}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test", Workdir: "repo"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	result, err := backend.Run(t.Context(), RunRequest{
		Repo:      Repo{Root: t.TempDir(), Name: "repo"},
		NoSync:    true,
		Command:   []string{"true"},
		Downloads: []string{"reports/proof.txt=" + filepath.Join(parent, "proof.txt")},
	})
	if err == nil {
		t.Fatal("expected local download failure")
	}
	if result.ExitCode != 2 {
		t.Fatalf("exit=%d, want 2: %v", result.ExitCode, err)
	}
}

func TestIsloRunRejectsOversizedRequiredArtifact(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{
		createName: "crabbox-repo-abcdef",
		retrieveFiles: map[string][]byte{
			"reports/manifest.json": bytes.Repeat([]byte("x"), core.DelegatedRunDownloadMaxBytes+1),
		},
	}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test", Workdir: "repo"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	result, err := backend.Run(t.Context(), RunRequest{
		Repo:                  Repo{Root: t.TempDir(), Name: "repo"},
		NoSync:                true,
		Command:               []string{"true"},
		RequiredArtifactGlobs: []string{"reports/manifest.json"},
	})
	if err == nil || !strings.Contains(err.Error(), "delegated artifact reports/manifest.json") {
		t.Fatalf("err=%v, want delegated artifact failure", err)
	}
	if result.ExitCode != 7 {
		t.Fatalf("exit=%d, want 7", result.ExitCode)
	}
}

func TestBoundedRunFileBufferRecordsOverflow(t *testing.T) {
	var buf boundedRunFileBuffer
	buf.max = 4
	if n, err := buf.Write([]byte("abcdef")); err != nil || n != 6 {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	if !buf.exceeded || buf.String() != "abcd" {
		t.Fatalf("buffer=%q exceeded=%t", buf.String(), buf.exceeded)
	}
}

func TestIsloCommandForErrorRedactsArchiveChunk(t *testing.T) {
	command := "printf %s 'sensitive-base64' >> '/tmp/archive.tgz.b64'"
	if got := isloCommandForError(command); got != "append archive chunk" {
		t.Fatalf("got=%q", got)
	}
}

func TestIsloRunMigratesReusedWorkspaceOwnership(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-old-abcdef"
	if err := claimLeaseForRepoProvider(leaseID, "old", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscale(leaseID, "100.64.7.7", ""); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{execOut: "CRABBOX_TS_IP=100.64.7.8"}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test", Workdir: "repo"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	_, err := backend.Run(context.Background(), RunRequest{
		ID:      leaseID,
		Keep:    true,
		NoSync:  true,
		Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.execRequests) < 4 {
		t.Fatalf("exec requests=%d want health, migration, prepare, workload", len(client.execRequests))
	}
	migration := client.execRequests[1]
	if migration.GetUser() == nil || *migration.GetUser() != isloAdminUser {
		t.Fatalf("migration user=%v want %q", migration.GetUser(), isloAdminUser)
	}
	command := strings.Join(migration.GetCommand(), " ")
	for _, want := range []string{"chown -R", "'islo:islo'", "'/workspace/repo'"} {
		if !strings.Contains(command, want) {
			t.Fatalf("migration command=%q missing %q", command, want)
		}
	}
	if strings.Contains(command, "workspace-owner-") {
		t.Fatalf("ownership repair must not use a one-shot marker: %q", command)
	}
}

func TestIsloRunMigratesFreshTailnetWorkspaceOwnership(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{
		createName: "crabbox-repo-abcdef",
		execOut:    "CRABBOX_TS_IP=100.64.7.7",
	}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{
			Islo:      IsloConfig{APIKey: "test", Workdir: "repo"},
			Tailscale: core.TailscaleConfig{Enabled: true, AuthKey: "tskey-secret"},
		},
		rt: Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	_, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: t.TempDir(), Name: "repo"},
		Keep:    true,
		NoSync:  true,
		Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !client.commandContains("chown -R") {
		t.Fatalf("fresh tailnet lease skipped workspace migration: %#v", client.prepareCommands)
	}
}

func TestIsloRunReturnsSessionHandleWhenFreshTailnetMigrationFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{
		createName:               "crabbox-repo-abcdef",
		execOut:                  "CRABBOX_TS_IP=100.64.7.7",
		execErrOnCommand:         errors.New("migration failed"),
		execErrOnCommandContains: "chown -R",
	}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{
			Islo:      IsloConfig{APIKey: "test", Workdir: "repo"},
			Tailscale: core.TailscaleConfig{Enabled: true, AuthKey: "tskey-secret"},
		},
		rt: Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: t.TempDir(), Name: "repo"},
		Keep:    true,
		NoSync:  true,
		Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "migration failed") {
		t.Fatalf("expected migration failure, got %v", err)
	}
	if result.Session == nil || result.Session.LeaseID != "isb_crabbox-repo-abcdef" || !result.Session.Kept {
		t.Fatalf("missing kept session after migration failure: %#v", result.Session)
	}
}

func TestIsloWorkspaceOwnershipRepairCommandIsValidBash(t *testing.T) {
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(isloWorkspaceOwnershipRepairCommand("/workspace/repo"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ownership repair script syntax: %v\n%s", err, out)
	}
}

func TestIsloRunRejectsReusedPlainLeaseTailscaleEnrollment(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-old-abcdef"
	if err := claimLeaseForRepoProvider(leaseID, "old", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{
			Islo:      IsloConfig{APIKey: "test", Workdir: "repo"},
			Tailscale: core.TailscaleConfig{Enabled: true, AuthKey: "tskey-secret"},
		},
		rt: Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	_, err := backend.Run(context.Background(), RunRequest{
		ID:      leaseID,
		Keep:    true,
		NoSync:  true,
		Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot enable Tailscale in place") {
		t.Fatalf("expected in-place Tailscale rejection, got %v", err)
	}
	if len(client.execRequests) != 0 {
		t.Fatalf("sandbox mutated before in-place enrollment rejection: %#v", client.prepareCommands)
	}
}

func TestIsloRunRequiresClaimBeforeReusedLeaseTailscaleEnrollment(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{
			Islo:      IsloConfig{APIKey: "test", Workdir: "repo"},
			Tailscale: core.TailscaleConfig{Enabled: true, AuthKey: "tskey-secret"},
		},
		rt: Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	_, err := backend.Run(context.Background(), RunRequest{
		ID:      "isb_crabbox-unclaimed-abcdef",
		Keep:    true,
		NoSync:  true,
		Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "lease claim") {
		t.Fatalf("expected missing claim error, got %v", err)
	}
	if len(client.execRequests) != 0 {
		t.Fatalf("sandbox mutated before missing claim failure: %#v", client.prepareCommands)
	}
}

func TestIsloRunAddsTailnetProxyDefaultsToWorkload(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-old-abcdef"
	if err := claimLeaseForRepoProvider(leaseID, "old", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscale(leaseID, "100.64.7.7", ""); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{
		execOuts: []string{"CRABBOX_TS_IP=100.64.7.8"},
	}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test", Workdir: "repo"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	_, err := backend.Run(context.Background(), RunRequest{
		ID:      leaseID,
		Keep:    true,
		NoSync:  true,
		Env:     map[string]string{"HTTP_PROXY": "http://override.example:8080"},
		Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	workload := client.execRequests[len(client.execRequests)-1]
	for name, want := range map[string]string{
		"ALL_PROXY":   "socks5://127.0.0.2:1055",
		"all_proxy":   "socks5://127.0.0.2:1055",
		"HTTP_PROXY":  "http://override.example:8080",
		"http_proxy":  "http://override.example:8080",
		"HTTPS_PROXY": "http://127.0.0.2:1055",
		"https_proxy": "http://127.0.0.2:1055",
	} {
		if workload.Env[name] == nil || *workload.Env[name] != want {
			t.Fatalf("workload %s=%v want %q", name, workload.Env[name], want)
		}
	}
	if workload.GetUser() == nil || *workload.GetUser() != isloWorkloadUser {
		t.Fatalf("tailnet workload user=%v want %q", workload.GetUser(), isloWorkloadUser)
	}
}

func TestIsloRunFailsClosedWhenEnrolledTailnetValidationUnavailable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-old-abcdef"
	if err := claimLeaseForRepoProvider(leaseID, "old", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscale(leaseID, "100.64.7.7", ""); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{
		execErrOnCommand:         errors.New("health API unavailable"),
		execErrOnCommandContains: `"BackendState"`,
	}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test", Workdir: "repo"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	_, err := backend.Run(context.Background(), RunRequest{
		ID:      leaseID,
		Keep:    true,
		NoSync:  true,
		Command: []string{"true"},
	})
	if !errors.Is(err, core.ErrTailnetPeerValidationUnavailable) {
		t.Fatalf("expected validation failure, got %v", err)
	}
	for _, req := range client.execRequests {
		if strings.Join(req.GetCommand(), " ") == "true" {
			t.Fatal("workload ran after tailnet validation failed")
		}
	}
}

func TestIsloWorkloadEnvExplicitAllProxySuppressesProtocolDefaults(t *testing.T) {
	env := isloWorkloadEnv(map[string]string{
		"all_proxy": "socks5://override.example:1080",
	}, true)
	if env["ALL_PROXY"] != "socks5://override.example:1080" || env["all_proxy"] != "socks5://override.example:1080" {
		t.Fatalf("ALL_PROXY pair=%#v", env)
	}
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		if _, ok := env[name]; ok {
			t.Fatalf("explicit ALL_PROXY should suppress %s default: %#v", name, env)
		}
	}
}

func TestIsloRunReturnsSessionHandleWhenPrepareFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{
		createName: "crabbox-repo-abcdef",
		execErr:    errors.New("prepare failed"),
	}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test", Workdir: "repo"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: t.TempDir(), Name: "repo"},
		Keep:    true,
		NoSync:  true,
		Command: []string{"true"},
	})
	if err == nil {
		t.Fatal("expected prepare error")
	}
	if result.Session == nil {
		t.Fatal("missing session handle")
	}
	if result.Session.LeaseID != "isb_crabbox-repo-abcdef" || !result.Session.Kept {
		t.Fatalf("session=%#v", result.Session)
	}
}

func TestIsloCreateSandboxRejectsUnsafeWorkdirBeforeAPI(t *testing.T) {
	client := &fakeIsloSyncClient{}
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{Workdir: "../etc"}},
		rt:  Runtime{Stderr: io.Discard},
	}
	_, _, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "")
	if err == nil || !strings.Contains(err.Error(), "escapes /workspace") {
		t.Fatalf("createSandbox err=%v, want workdir containment error", err)
	}
	if client.createRequest != nil {
		t.Fatalf("CreateSandbox was called with %#v", client.createRequest)
	}
}

func decodeLastTimingReport(t *testing.T, output string) timingReport {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		start := strings.Index(line, "{")
		if start < 0 {
			continue
		}
		var report timingReport
		if err := json.Unmarshal([]byte(line[start:]), &report); err != nil {
			t.Fatalf("timing json: %v\noutput=%s", err, output)
		}
		return report
	}
	t.Fatalf("output does not contain timing JSON: %s", output)
	return timingReport{}
}

func TestIsloCreateSandboxOmitsDefaultProviderCreateFields(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{createName: "crabbox-repo-abcdef"}
	backend := &isloBackend{
		cfg: core.BaseConfig(),
		rt:  Runtime{Stderr: io.Discard},
	}
	backend.cfg.Provider = isloProvider
	backend.cfg.Islo.Workdir = "team/repo"
	_, _, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if client.createRequest == nil {
		t.Fatal("CreateSandbox was not called")
	}
	if client.createRequest.Workdir != nil {
		t.Fatalf("default create workdir=%v, want omitted", *client.createRequest.Workdir)
	}
	if client.createRequest.Image != nil {
		t.Fatalf("default create image=%v, want omitted", *client.createRequest.Image)
	}
	if client.createRequest.Vcpus != nil || client.createRequest.MemoryMb != nil || client.createRequest.DiskGb != nil {
		t.Fatalf("default create sizing was sent: %#v", client.createRequest)
	}
}

func TestIsloCreateSandboxSendsNonDefaultProviderCreateFields(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{createName: "crabbox-repo-abcdef"}
	backend := &isloBackend{
		cfg: core.BaseConfig(),
		rt:  Runtime{Stderr: io.Discard},
	}
	backend.cfg.Provider = isloProvider
	backend.cfg.Islo.Image = "docker.io/library/custom:latest"
	backend.cfg.Islo.VCPUs = 4
	backend.cfg.Islo.MemoryMB = 8192
	backend.cfg.Islo.DiskGB = 80
	_, _, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if client.createRequest == nil {
		t.Fatal("CreateSandbox was not called")
	}
	if client.createRequest.Image == nil || *client.createRequest.Image != "docker.io/library/custom:latest" {
		t.Fatalf("custom create image=%v", client.createRequest.Image)
	}
	if client.createRequest.Vcpus == nil || *client.createRequest.Vcpus != 4 {
		t.Fatalf("custom create vcpus=%v", client.createRequest.Vcpus)
	}
	if client.createRequest.MemoryMb == nil || *client.createRequest.MemoryMb != 8192 {
		t.Fatalf("custom create memory=%v", client.createRequest.MemoryMb)
	}
	if client.createRequest.DiskGb == nil || *client.createRequest.DiskGb != 80 {
		t.Fatalf("custom create disk=%v", client.createRequest.DiskGb)
	}
}

func TestIsloCreateSandboxSendsExplicitDefaultProviderCreateFields(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{createName: "crabbox-repo-abcdef"}
	cfg := core.BaseConfig()
	cfg.Provider = isloProvider
	core.MarkIsloImageExplicit(&cfg)
	core.MarkIsloVCPUsExplicit(&cfg)
	core.MarkIsloMemoryMBExplicit(&cfg)
	core.MarkIsloDiskGBExplicit(&cfg)
	backend := &isloBackend{cfg: cfg, rt: Runtime{Stderr: io.Discard}}
	_, _, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if client.createRequest == nil {
		t.Fatal("CreateSandbox was not called")
	}
	if client.createRequest.Image == nil || *client.createRequest.Image != cfg.Islo.Image {
		t.Fatalf("explicit default image=%v", client.createRequest.Image)
	}
	if client.createRequest.Vcpus == nil || *client.createRequest.Vcpus != cfg.Islo.VCPUs {
		t.Fatalf("explicit default vcpus=%v", client.createRequest.Vcpus)
	}
	if client.createRequest.MemoryMb == nil || *client.createRequest.MemoryMb != cfg.Islo.MemoryMB {
		t.Fatalf("explicit default memory=%v", client.createRequest.MemoryMb)
	}
	if client.createRequest.DiskGb == nil || *client.createRequest.DiskGb != cfg.Islo.DiskGB {
		t.Fatalf("explicit default disk=%v", client.createRequest.DiskGb)
	}
}

func TestIsloProviderFlagsMarkDefaultCreateFieldsExplicit(t *testing.T) {
	cfg := core.BaseConfig()
	fs := flag.NewFlagSet("islo", flag.ContinueOnError)
	values := RegisterIsloProviderFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--islo-image", cfg.Islo.Image,
		"--islo-vcpus", strconv.Itoa(cfg.Islo.VCPUs),
		"--islo-memory-mb", strconv.Itoa(cfg.Islo.MemoryMB),
		"--islo-disk-gb", strconv.Itoa(cfg.Islo.DiskGB),
	}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyIsloProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if !core.IsloImageExplicit(cfg) || !core.IsloVCPUsExplicit(cfg) || !core.IsloMemoryMBExplicit(cfg) || !core.IsloDiskGBExplicit(cfg) {
		t.Fatalf("flag explicit markers missing: %#v", cfg)
	}
}

func TestIsloCreateSandboxRejectsMissingTailscaleAuthKeyBeforeAPI(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{createName: "crabbox-repo-abcdef"}
	backend := &isloBackend{
		cfg: Config{
			Islo: IsloConfig{Workdir: "team/repo"},
			Tailscale: core.TailscaleConfig{
				Enabled:    true,
				AuthKeyEnv: "TEST_TS_AUTH_KEY",
			},
		},
		rt: Runtime{Stderr: io.Discard},
	}
	_, _, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "")
	if err == nil || !strings.Contains(err.Error(), "$TEST_TS_AUTH_KEY") {
		t.Fatalf("expected missing auth key error, got %v", err)
	}
	if !strings.Contains(err.Error(), "reusable, ephemeral") {
		t.Fatalf("missing Islo auth key contract: %v", err)
	}
	if client.createRequest != nil {
		t.Fatalf("CreateSandbox was called with %#v", client.createRequest)
	}
}

func TestIsloCreateSandboxStoresPondClaimForList(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{createName: "crabbox-repo-abcdef"}
	backend := &isloBackend{
		cfg: Config{
			Pond: "Alpha Pond",
			Islo: IsloConfig{Workdir: "team/repo"},
		},
		rt: Runtime{Stderr: io.Discard},
	}
	leaseID, _, slug, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "web")
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveLeaseClaim(leaseID)
	if err != nil || !ok {
		t.Fatalf("resolve claim ok=%t err=%v", ok, err)
	}
	if claim.Pond != "alpha-pond" {
		t.Fatalf("claim pond=%q want alpha-pond", claim.Pond)
	}
	server := isloSandboxToServer(&gosdk.SandboxResponse{Name: client.createName, Status: "running"})
	if server.Labels["pond"] != "alpha-pond" {
		t.Fatalf("server pond label=%q labels=%#v", server.Labels["pond"], server.Labels)
	}
	if server.Labels["slug"] != normalizeLeaseSlug(slug) {
		t.Fatalf("server slug=%q want %q", server.Labels["slug"], normalizeLeaseSlug(slug))
	}
}

func TestIsloCreateSandboxTailscaleClaimAndOptions(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("TS_CONTROL_URL", "https://headscale.example.com")
	client := &fakeIsloSyncClient{
		createName: "crabbox-repo-abcdef",
		execOut:    "tailscale up ok\nCRABBOX_TS_IP=100.64.7.7\n",
	}
	backend := &isloBackend{
		cfg: Config{
			Pond: "Mesh Demo",
			Islo: IsloConfig{Workdir: "repo"},
			Tailscale: core.TailscaleConfig{
				Enabled:                true,
				AuthKey:                "tskey-secret",
				AuthKeyEnv:             "TEST_TS_AUTH_KEY",
				HostnameTemplate:       "cbx-{provider}-{slug}",
				Tags:                   []string{"tag:cbx-pond-demo"},
				ExitNode:               "exit.tailnet.ts.net",
				ExitNodeAllowLANAccess: true,
			},
		},
		rt: Runtime{Stderr: io.Discard},
	}
	leaseID, _, slug, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(client.execRequests) != 1 {
		t.Fatalf("expected one tailscale exec request, got %d", len(client.execRequests))
	}
	req := client.execRequests[0]
	if req.GetUser() == nil || *req.GetUser() != isloAdminUser {
		t.Fatalf("tailscale exec user=%v want %q", req.GetUser(), isloAdminUser)
	}
	for key, want := range map[string]string{
		"TS_HOST":                "cbx-islo-node-a",
		"TS_TAGS":                "tag:cbx-pond-demo",
		"TS_LOGIN_SERVER":        "https://headscale.example.com",
		"TS_EXIT_NODE":           "exit.tailnet.ts.net",
		"TS_EXIT_NODE_ALLOW_LAN": "true",
		"TS_STATE_DIR":           isloTailscaleStateDir(leaseID),
	} {
		got := ""
		if req.Env != nil && req.Env[key] != nil {
			got = *req.Env[key]
		}
		if got != want {
			t.Fatalf("exec env %s=%q want %q", key, got, want)
		}
	}
	claim, ok, err := resolveLeaseClaim(leaseID)
	if err != nil || !ok {
		t.Fatalf("resolve claim ok=%t err=%v", ok, err)
	}
	if claim.Slug != slug || claim.Pond != "mesh-demo" || claim.TailscaleIPv4 != "100.64.7.7" {
		t.Fatalf("claim=%#v slug=%q", claim, slug)
	}
	if claim.Labels["tailscale"] != "true" || claim.Labels["tailscale_ipv4"] != "100.64.7.7" || claim.Labels["tailscale_state"] != "ready" {
		t.Fatalf("tailscale labels=%#v", claim.Labels)
	}
	if claim.TailscaleHostname != "cbx-islo-node-a" || strings.Join(claim.TailscaleTags, ",") != "tag:cbx-pond-demo" {
		t.Fatalf("tailscale settings=%#v", claim)
	}
	server := isloSandboxToServer(&gosdk.SandboxResponse{Name: client.createName, Status: "running"})
	if server.Labels["tailscale_ipv4"] != "100.64.7.7" || server.Labels["tailscale_state"] != "ready" {
		t.Fatalf("server tailscale labels=%#v", server.Labels)
	}
}

func TestIsloCreateSandboxRetainsClaimWhenTailscaleRollbackFails(t *testing.T) {
	for _, changedOwner := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed_owner=%t", changedOwner), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			isolateIsloTestHome(t)
			client := &fakeIsloSyncClient{
				createName:       isloTeardownName,
				execErrOnCommand: errors.New("tailscale failed"),
			}
			var replacement core.LeaseClaim
			if changedOwner {
				client.execErrOnCommandHook = func() {
					if err := core.ClaimLeaseForRepoProvider(isloTeardownLeaseID, "node-a", isloProvider, t.TempDir(), time.Hour, true); err != nil {
						t.Fatal(err)
					}
					var ok bool
					var err error
					replacement, ok, err = resolveExactIsloLeaseClaim(isloTeardownLeaseID)
					if err != nil || !ok {
						t.Fatalf("replacement claim ok=%t err=%v", ok, err)
					}
				}
			} else {
				client.deleteErr = errors.New("delete failed")
			}
			backend := &isloBackend{
				cfg: Config{
					Pond: "Mesh Demo",
					Islo: IsloConfig{Workdir: "repo"},
					Tailscale: core.TailscaleConfig{
						Enabled: true,
						AuthKey: "tskey-secret",
					},
				},
				rt: Runtime{Stderr: io.Discard},
			}
			_, _, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "node-a")
			if err == nil || !strings.Contains(err.Error(), "cleanup failed") {
				t.Fatalf("expected cleanup failure, got %v", err)
			}
			claim, ok, claimErr := resolveExactIsloLeaseClaim(isloTeardownLeaseID)
			if claimErr != nil || !ok {
				t.Fatalf("claim should remain discoverable after failed rollback: ok=%t err=%v claim=%#v", ok, claimErr, claim)
			}
			if changedOwner && (client.deleteCalls != 0 || !reflect.DeepEqual(claim, replacement)) {
				t.Fatalf("rollback changed the replacement owner's sandbox or claim: deletes=%d claim=%#v want=%#v", client.deleteCalls, claim, replacement)
			}
		})
	}
}

func TestIsloSyncWorkspaceUploadsRepoArchive(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	root := t.TempDir()
	if err := os.WriteFile(root+"/go.mod", []byte("module example.test/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	client := &fakeIsloSyncClient{}
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{Workdir: "repo"}},
		rt:  Runtime{Stderr: io.Discard},
	}
	_, _, err := backend.syncWorkspace(context.Background(), client, "crabbox-test", RunRequest{
		Repo: Repo{Root: root, Name: "repo"},
	}, isloWorkloadUser)
	if err != nil {
		t.Fatal(err)
	}
	if client.uploadPath != "/workspace/repo" {
		t.Fatalf("upload path=%q", client.uploadPath)
	}
	if len(client.prepareCommands) != 2 || !strings.Contains(client.prepareCommands[0], "mkdir -p '/workspace/repo'") {
		t.Fatalf("prepare commands=%#v", client.prepareCommands)
	}
	if client.execRequests[0].GetUser() == nil || *client.execRequests[0].GetUser() != isloWorkloadUser {
		t.Fatalf("prepare user=%v want %q", client.execRequests[0].GetUser(), isloWorkloadUser)
	}
	repair := client.execRequests[1]
	if repair.GetUser() == nil || *repair.GetUser() != isloAdminUser || !strings.Contains(client.prepareCommands[1], "chown -R 'islo:islo' '/workspace/repo'") {
		t.Fatalf("ownership repair request=%#v command=%q", repair, client.prepareCommands[1])
	}
	if !tarGzipContains(t, client.uploaded.Bytes(), "go.mod") {
		t.Fatal("uploaded archive missing go.mod")
	}
}

func TestIsloSyncWorkspaceFallsBackToExecUpload(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	root := t.TempDir()
	if err := os.WriteFile(root+"/go.mod", []byte("module example.test/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	client := &fakeIsloSyncClient{uploadErr: errors.New("api upload failed"), closeUploadReader: true}
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{Workdir: "repo"}},
		rt:  Runtime{Stderr: io.Discard},
	}
	_, _, err := backend.syncWorkspace(context.Background(), client, "crabbox-test", RunRequest{
		Repo: Repo{Root: root, Name: "repo"},
	}, isloWorkloadUser)
	if err != nil {
		t.Fatal(err)
	}
	if !client.commandContains("base64 -d") || !client.commandContains("tar -xzf") {
		t.Fatalf("fallback commands=%#v", client.prepareCommands)
	}
	chownIndex, fallbackIndex := -1, -1
	for i, command := range client.prepareCommands {
		if strings.Contains(command, "chown -R") {
			chownIndex = i
		}
		if fallbackIndex < 0 && strings.Contains(command, "base64 -d") {
			fallbackIndex = i
		}
	}
	if chownIndex < 0 || fallbackIndex < 0 || chownIndex > fallbackIndex {
		t.Fatalf("ownership repair must precede fallback extraction: %#v", client.prepareCommands)
	}
	if client.execRequests[chownIndex].GetUser() == nil || *client.execRequests[chownIndex].GetUser() != isloAdminUser {
		t.Fatalf("ownership repair user=%v want %q", client.execRequests[chownIndex].GetUser(), isloAdminUser)
	}
}

func TestIsloExecUploadCleansTempFilesOnChunkFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeIsloSyncClient{
		execErrOnCommandContains: "printf",
		execErrOnCommandSkip:     1,
		execErrOnCommand:         errors.New("chunk transfer failed"),
		execErrOnCommandHook:     cancel,
		rejectCanceledContext:    true,
	}
	backend := &isloBackend{rt: Runtime{Stderr: io.Discard}}
	archive := bytes.NewReader(bytes.Repeat([]byte("x"), 49*1024))

	err := backend.uploadArchiveViaExec(ctx, client, "crabbox-test", "/workspace/repo", archive, "")
	if err == nil || !strings.Contains(err.Error(), "chunk transfer failed") {
		t.Fatalf("uploadArchiveViaExec err=%v, want chunk transfer failure", err)
	}
	lastCommand := client.prepareCommands[len(client.prepareCommands)-1]
	for _, want := range []string{"rm -f", ".tgz.b64", ".tgz"} {
		if !strings.Contains(lastCommand, want) {
			t.Fatalf("last command %q missing %q; commands=%#v", lastCommand, want, client.prepareCommands)
		}
	}
}

func TestIsloFallbackExtractCommandCleansUploadsOnFailure(t *testing.T) {
	cmd := isloFallbackExtractCommand("/tmp/crabbox-test.tgz.b64", "/tmp/crabbox-test.tgz", "/workspace/repo")
	for _, want := range []string{
		"base64 -d '/tmp/crabbox-test.tgz.b64' > '/tmp/crabbox-test.tgz'",
		"tar -xzf '/tmp/crabbox-test.tgz' -C '/workspace/repo'",
		"; status=$?; rm -f '/tmp/crabbox-test.tgz.b64' '/tmp/crabbox-test.tgz'; exit $status",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("command missing %q: %s", want, cmd)
		}
	}
	if strings.Index(cmd, "rm -f '/tmp/crabbox-test.tgz.b64'") < strings.Index(cmd, "tar -xzf") {
		t.Fatalf("cleanup should run after extract attempt: %s", cmd)
	}
}

func TestIsloExecForwardsEnv(t *testing.T) {
	client := &fakeIsloSyncClient{}
	backend := &isloBackend{rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	code, err := backend.exec(context.Background(), client, "crabbox-test", "/workspace/repo", []string{"env"}, false, map[string]string{
		"API_TOKEN": "secret",
		"CI":        "1",
	}, "")
	if err != nil || code != 0 {
		t.Fatalf("exec code=%d err=%v", code, err)
	}
	if len(client.execRequests) != 1 {
		t.Fatalf("exec requests=%d", len(client.execRequests))
	}
	env := client.execRequests[0].Env
	if env["API_TOKEN"] == nil || *env["API_TOKEN"] != "secret" || env["CI"] == nil || *env["CI"] != "1" {
		t.Fatalf("env=%#v", env)
	}
}

func TestRejectIsloSyncOptionsAllowsForceSyncLarge(t *testing.T) {
	if err := rejectIsloSyncOptions(RunRequest{ForceSyncLarge: true}); err != nil {
		t.Fatalf("force sync large should be honored by Islo archive sync: %v", err)
	}
	if err := rejectIsloSyncOptions(RunRequest{SyncOnly: true}); err == nil || !strings.Contains(err.Error(), "--sync-only") {
		t.Fatalf("sync-only err=%v", err)
	}
	if err := rejectIsloSyncOptions(RunRequest{ChecksumSync: true}); err == nil || !strings.Contains(err.Error(), "--checksum") {
		t.Fatalf("checksum err=%v", err)
	}
}

func TestNewIsloSandboxNameUsesCrabboxPrefix(t *testing.T) {
	name := newIsloSandboxName(Repo{Name: "repo"})
	if !strings.HasPrefix(name, "crabbox-repo-") {
		t.Fatalf("name=%q", name)
	}
	if !isCrabboxIsloSandboxName(name) {
		t.Fatalf("expected %q to be recognized as Crabbox-owned", name)
	}
}

func TestIsloSDKClientListUsesInjectedHTTPAndPaginates(t *testing.T) {
	authHits := 0
	listHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token":
			authHits++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"session_token":  "jwt-from-test",
				"cookie_max_age": 3600,
			})
		case "/sandboxes":
			listHits++
			if got := r.Header.Get("Authorization"); got != "Bearer jwt-from-test" {
				t.Fatalf("Authorization=%q", got)
			}
			offset := r.URL.Query().Get("offset")
			offsetValue, _ := strconv.Atoi(offset)
			items := []map[string]any{}
			if offset == "0" {
				for i := 0; i < 100; i++ {
					items = append(items, map[string]any{"id": "id", "name": "crabbox-a", "status": "running", "image": "ubuntu"})
				}
			} else if offset == "100" {
				items = append(items, map[string]any{"id": "id", "name": "crabbox-b", "status": "running", "image": "ubuntu"})
			} else {
				t.Fatalf("unexpected offset=%q", offset)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items":  items,
				"total":  101,
				"limit":  100,
				"offset": offsetValue,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	api, err := newIsloClient(Config{Islo: IsloConfig{APIKey: "ak_test", BaseURL: srv.URL}}, Runtime{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	items, err := api.ListSandboxes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 101 {
		t.Fatalf("items=%d", len(items))
	}
	if authHits != 1 || listHits != 2 {
		t.Fatalf("authHits=%d listHits=%d", authHits, listHits)
	}
}

func TestIsloSDKClientUploadArchiveStreamsMultipartTarball(t *testing.T) {
	authHits := 0
	uploadHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token":
			authHits++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"session_token":  "jwt-from-test",
				"cookie_max_age": 3600,
			})
		case "/sandboxes/crabbox-test/files-archive":
			uploadHits++
			if got := r.Header.Get("Authorization"); got != "Bearer jwt-from-test" {
				t.Fatalf("Authorization=%q", got)
			}
			if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "multipart/form-data; boundary=") {
				t.Fatalf("Content-Type=%q", got)
			}
			if got := r.URL.Query().Get("path"); got != "/workspace/repo" {
				t.Fatalf("path=%q", got)
			}
			part, err := r.MultipartReader()
			if err != nil {
				t.Fatal(err)
			}
			file, err := part.NextPart()
			if err != nil {
				t.Fatal(err)
			}
			if file.FormName() != "file" || file.FileName() != "archive.tar.gz" {
				t.Fatalf("part name=%q filename=%q", file.FormName(), file.FileName())
			}
			if got := file.Header.Get("Content-Type"); got != "application/gzip" {
				t.Fatalf("part Content-Type=%q", got)
			}
			body, err := io.ReadAll(file)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != "archive" {
				t.Fatalf("part body=%q", string(body))
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	api, err := newIsloClient(Config{Islo: IsloConfig{APIKey: "ak_test", BaseURL: srv.URL}}, Runtime{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.UploadArchive(t.Context(), "crabbox-test", "/workspace/repo", strings.NewReader("archive")); err != nil {
		t.Fatal(err)
	}
	if authHits != 1 || uploadHits != 1 {
		t.Fatalf("authHits=%d uploadHits=%d", authHits, uploadHits)
	}
}

func TestIsloPauseResumeCallProvider(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepoProvider("isb_crabbox-repo-abcdef", "web", isloProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}
	if err := backend.Pause(context.Background(), PauseRequest{ID: "crabbox-repo-abcdef"}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if client.pausedName != "crabbox-repo-abcdef" {
		t.Fatalf("pausedName=%q, want crabbox-repo-abcdef", client.pausedName)
	}
	if err := backend.Resume(context.Background(), ResumeRequest{ID: "crabbox-repo-abcdef"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if client.resumedName != "crabbox-repo-abcdef" {
		t.Fatalf("resumedName=%q, want crabbox-repo-abcdef", client.resumedName)
	}
	// A non-Crabbox sandbox id must be rejected before any provider call.
	if err := backend.Pause(context.Background(), PauseRequest{ID: "production"}); err == nil {
		t.Fatal("expected non-Crabbox sandbox to be rejected")
	}
}

type fakeIsloSyncClient struct {
	prepareCommands          []string
	execRequests             []*gosdk.ExecRequest
	uploadPath               string
	uploaded                 bytes.Buffer
	uploadErr                error
	execErr                  error
	execCode                 int
	execCodes                []int
	execOut                  string
	execOuts                 []string
	retrieveFiles            map[string][]byte
	blockArtifactRead        bool
	execErrOnCommand         error
	execErrOnCommandContains string
	execErrOnCommandSkip     int
	execErrOnCommandHook     func()
	execHook                 func(*gosdk.ExecRequest)
	execDeadlineCommand      string
	execDeadline             time.Time
	rejectCanceledContext    bool
	closeUploadReader        bool
	createRequest            *gosdk.CreateSandboxRequest
	createErr                error
	createSandboxHook        func()
	createName               string
	createID                 string
	createdBy                string
	createdByEntity          string
	getSandbox               *gosdk.SandboxResponse
	getSandboxes             []*gosdk.SandboxResponse
	getSandboxErr            error
	getSandboxHook           func()
	getSandboxNames          []string
	getSandboxGone           bool
	byID                     map[string]*gosdk.SandboxResponse
	byIDSandboxes            []*gosdk.SandboxResponse
	byIDErr                  error
	byIDCalls                []string
	resumeErr                error
	resumeCalls              int
	blockDelete              bool
	blockReads               bool
	deleteErr                error
	deleteHook               func()
	deleteCalls              int
	deletedNames             []string
	deleteCtxErrs            []error
	listCalls                int
	listResponse             []*gosdk.SandboxResponse
	pausedName               string
	resumedName              string
	// sandboxIDs and deleted model the live identity contract: a sandbox has an
	// immutable id alongside its name, `GET /sandboxes/{name}` answers 404
	// immediately after a delete, and `GET /sandboxes/-/by-id/{id}` keeps
	// answering with a "deleted" tombstone.
	sandboxIDs map[string]string
	deleted    map[string]bool
	deletedIDs map[string]bool
}

func isloTestNotFoundError() error {
	return &gosdk.NotFoundError{APIError: sdkcore.NewAPIError(http.StatusNotFound, errors.New("sandbox not found"))}
}

func (f *fakeIsloSyncClient) registerSandbox(name, id string) {
	if f.sandboxIDs == nil {
		f.sandboxIDs = map[string]string{}
	}
	if id != "" {
		f.sandboxIDs[name] = id
	}
}

// markDeleted models a sandbox the tenant deleted before this process looked at
// it: the name answers 404 and the id keeps answering with a tombstone.
func (f *fakeIsloSyncClient) markDeleted(name string) {
	if f.deleted == nil {
		f.deleted = map[string]bool{}
	}
	f.deleted[name] = true
	if id := f.sandboxIDs[name]; id != "" {
		if f.deletedIDs == nil {
			f.deletedIDs = map[string]bool{}
		}
		f.deletedIDs[id] = true
	}
}

func (f *fakeIsloSyncClient) liveSandbox(name string) *gosdk.SandboxResponse {
	sandbox := &gosdk.SandboxResponse{ID: f.sandboxIDs[name], Name: name, Status: "running"}
	if f.createdBy != "" {
		sandbox.CreatedBy = stringValue(f.createdBy)
	}
	return sandbox
}

func (f *fakeIsloSyncClient) CreateSandbox(_ context.Context, req *gosdk.CreateSandboxRequest) (*gosdk.SandboxResponse, error) {
	f.createRequest = req
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createID == "" {
		f.createID = isloTestResourceID
	}
	name := f.createName
	if name == "" {
		name = "crabbox-test-abcdef"
	}
	f.registerSandbox(name, f.createID)
	if f.createSandboxHook != nil {
		f.createSandboxHook()
	}
	sandbox := &gosdk.SandboxResponse{ID: f.createID, Name: name}
	if f.createdBy != "" {
		sandbox.CreatedBy = stringValue(f.createdBy)
	}
	if f.createdByEntity != "" {
		raw, err := json.Marshal(map[string]any{
			"id":                f.createID,
			"name":              name,
			"status":            "starting",
			"image":             "",
			"created_at":        "2026-01-01T00:00:00Z",
			"created_by":        f.createdBy,
			"created_by_entity": f.createdByEntity,
		})
		if err != nil {
			return nil, err
		}
		// created_by_entity is not in the pinned SDK model, so it only reaches
		// the adapter through the extra-properties bag UnmarshalJSON fills.
		sandbox = &gosdk.SandboxResponse{}
		if err := json.Unmarshal(raw, sandbox); err != nil {
			return nil, err
		}
	}
	return sandbox, nil
}

func (f *fakeIsloSyncClient) GetSandbox(ctx context.Context, name string) (*gosdk.SandboxResponse, error) {
	f.getSandboxNames = append(f.getSandboxNames, name)
	if f.getSandboxHook != nil {
		f.getSandboxHook()
	}
	if f.blockReads {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.getSandboxErr != nil {
		return nil, f.getSandboxErr
	}
	if f.getSandboxGone {
		return nil, nil
	}
	if len(f.getSandboxes) > 0 {
		sandbox := f.getSandboxes[0]
		f.getSandboxes = f.getSandboxes[1:]
		return sandbox, nil
	}
	if f.getSandbox != nil {
		return f.getSandbox, nil
	}
	if f.deleted[name] {
		return nil, isloTestNotFoundError()
	}
	return f.liveSandbox(name), nil
}

func (f *fakeIsloSyncClient) GetSandboxByID(ctx context.Context, id string) (*gosdk.SandboxResponse, error) {
	f.byIDCalls = append(f.byIDCalls, id)
	if f.blockReads {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.byIDErr != nil {
		return nil, f.byIDErr
	}
	if len(f.byIDSandboxes) > 0 {
		sandbox := f.byIDSandboxes[0]
		f.byIDSandboxes = f.byIDSandboxes[1:]
		return sandbox, nil
	}
	if sandbox, ok := f.byID[id]; ok {
		if sandbox == nil {
			return nil, isloTestNotFoundError()
		}
		return sandbox, nil
	}
	if f.deletedIDs[id] {
		return &gosdk.SandboxResponse{ID: id, Name: f.nameForID(id), Status: "deleted", DeletedAt: stringValue("2026-01-01T00:00:01Z")}, nil
	}
	for name, known := range f.sandboxIDs {
		if known == id && !f.deleted[name] {
			return f.liveSandbox(name), nil
		}
	}
	return nil, isloTestNotFoundError()
}

func (f *fakeIsloSyncClient) nameForID(id string) string {
	for name, known := range f.sandboxIDs {
		if known == id {
			return name
		}
	}
	return ""
}

func (f *fakeIsloSyncClient) ResumeSandbox(_ context.Context, name string) (*gosdk.SandboxResponse, error) {
	f.resumeCalls++
	f.resumedName = name
	if f.resumeErr != nil {
		return nil, f.resumeErr
	}
	f.getSandbox = &gosdk.SandboxResponse{Name: name, Status: "running"}
	return f.getSandbox, nil
}

func (f *fakeIsloSyncClient) ListSandboxes(context.Context) ([]*gosdk.SandboxResponse, error) {
	f.listCalls++
	return f.listResponse, nil
}

func (f *fakeIsloSyncClient) DeleteSandbox(ctx context.Context, name string) error {
	f.deleteCalls++
	if f.deleteHook != nil {
		f.deleteHook()
	}
	f.deletedNames = append(f.deletedNames, name)
	// Record whether the delete was dispatched on an already-expired context.
	// Counting the call alone cannot distinguish a delete that was actually sent
	// from one the transport would refuse, which is the billing leak the reserved
	// slice of the teardown budget exists to prevent.
	f.deleteCtxErrs = append(f.deleteCtxErrs, ctx.Err())
	if f.blockDelete {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if f.deleted == nil {
		f.deleted = map[string]bool{}
	}
	f.deleted[name] = true
	if id := f.sandboxIDs[name]; id != "" {
		if f.deletedIDs == nil {
			f.deletedIDs = map[string]bool{}
		}
		f.deletedIDs[id] = true
	}
	return nil
}

func (f *fakeIsloSyncClient) PauseSandbox(_ context.Context, name string) (*gosdk.SandboxResponse, error) {
	f.pausedName = name
	return &gosdk.SandboxResponse{Name: name, Status: "paused"}, nil
}

func (f *fakeIsloSyncClient) UploadArchive(_ context.Context, _ string, targetPath string, archive io.Reader) error {
	f.uploadPath = targetPath
	_, err := io.Copy(&f.uploaded, archive)
	if f.closeUploadReader {
		if closer, ok := archive.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	if f.uploadErr != nil {
		return f.uploadErr
	}
	return err
}

func (f *fakeIsloSyncClient) ExecStream(ctx context.Context, _ string, req *gosdk.ExecRequest, stdout, _ io.Writer) (int, error) {
	if f.rejectCanceledContext && ctx.Err() != nil {
		return 1, ctx.Err()
	}
	f.execRequests = append(f.execRequests, req)
	if f.execHook != nil {
		f.execHook(req)
	}
	callIndex := len(f.execRequests) - 1
	command := strings.Join(req.GetCommand(), " ")
	if f.execDeadlineCommand != "" && strings.Contains(command, f.execDeadlineCommand) {
		f.execDeadline, _ = ctx.Deadline()
	}
	f.prepareCommands = append(f.prepareCommands, command)
	if strings.Contains(command, "head -c") && strings.Contains(command, "base64") {
		if f.blockArtifactRead {
			<-ctx.Done()
			return 1, ctx.Err()
		}
		for path, data := range f.retrieveFiles {
			if !strings.Contains(command, path) {
				continue
			}
			if len(data) > core.DelegatedRunDownloadMaxBytes {
				data = data[:core.DelegatedRunDownloadMaxBytes+1]
			}
			if stdout != nil {
				_, _ = io.WriteString(stdout, base64.StdEncoding.EncodeToString(data))
			}
			return 0, nil
		}
		return 1, nil
	}
	output := f.execOut
	if callIndex < len(f.execOuts) {
		output = f.execOuts[callIndex]
	}
	if output != "" {
		_, _ = io.WriteString(stdout, output)
	}
	if f.execErr != nil {
		return 1, f.execErr
	}
	if f.execErrOnCommand != nil && strings.Contains(command, f.execErrOnCommandContains) {
		if f.execErrOnCommandSkip > 0 {
			f.execErrOnCommandSkip--
		} else {
			if f.execErrOnCommandHook != nil {
				f.execErrOnCommandHook()
			}
			return 1, f.execErrOnCommand
		}
	}
	if callIndex < len(f.execCodes) {
		return f.execCodes[callIndex], nil
	}
	return f.execCode, nil
}

func (f *fakeIsloSyncClient) CreateShare(context.Context, string, int, time.Duration) (IsloShare, error) {
	return IsloShare{}, nil
}

func (f *fakeIsloSyncClient) ListShares(context.Context, string) ([]IsloShare, error) {
	return nil, nil
}

func (f *fakeIsloSyncClient) commandContains(value string) bool {
	for _, command := range f.prepareCommands {
		if strings.Contains(command, value) {
			return true
		}
	}
	return false
}

func swapNewIsloClient(client isloAPI) func() {
	previous := newIsloClient
	newIsloClient = func(Config, Runtime) (isloAPI, error) {
		return client, nil
	}
	return func() {
		newIsloClient = previous
	}
}

func withIsloCleanupTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	original := isloCleanupTimeout
	isloCleanupTimeout = timeout
	t.Cleanup(func() { isloCleanupTimeout = original })
}

func tarGzipContains(t *testing.T, data []byte, name string) bool {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == name {
			return true
		}
	}
}

// This writer fails only timing serialization, not earlier human diagnostics.
type isloTimingFailureWriter struct {
	bytes.Buffer
	err error
}

func (w *isloTimingFailureWriter) Write(p []byte) (int, error) {
	if w.err != nil && bytes.HasPrefix(p, []byte("{")) {
		return 0, w.err
	}
	return w.Buffer.Write(p)
}

func TestIsloRunBoundFailureOwnership(t *testing.T) {
	transportErr := errors.New("workload transport failed")
	deleteErr := errors.New("owned delete failed")
	writerErr := errors.New("timing serialization failed")
	for _, tc := range []struct {
		name, stage                   string
		commandCode                   int
		runErr, cleanupErr, writerErr error
		keep, keepFailure             bool
		wantCode                      int
		wantKind                      core.RunErrorKind
		wantKept                      bool
		wantDeletes                   int
	}{
		{name: "success", wantDeletes: 1},
		{name: "kept success", keep: true, wantKept: true},
		{name: "cleanup only", cleanupErr: deleteErr, wantCode: 1, wantKind: core.RunErrorProvider, wantKept: true, wantDeletes: 1},
		{name: "command cleanup", commandCode: 42, cleanupErr: deleteErr, wantCode: 42, wantKind: core.RunErrorCommandExit, wantKept: true, wantDeletes: 1},
		{name: "command writer", commandCode: 42, writerErr: writerErr, keepFailure: true, wantCode: 42, wantKind: core.RunErrorCommandExit, wantKept: true},
		{name: "command cleanup writer", commandCode: 42, cleanupErr: deleteErr, writerErr: writerErr, wantCode: 42, wantKind: core.RunErrorCommandExit, wantKept: true, wantDeletes: 1},
		{name: "transport", runErr: transportErr, wantCode: 1, wantKind: core.RunErrorProvider, wantDeletes: 1},
		{name: "typed exec error remains one", runErr: ExitError{Code: 69, Message: "native exec transport unavailable"}, wantCode: 1, wantKind: core.RunErrorProvider, wantDeletes: 1},
		{name: "cancel writer", runErr: context.Canceled, writerErr: writerErr, keepFailure: true, wantCode: 1, wantKind: core.RunErrorCanceled, wantKept: true},
		{name: "deadline cleanup writer", runErr: context.DeadlineExceeded, cleanupErr: deleteErr, writerErr: writerErr, wantCode: 1, wantKind: core.RunErrorTimeout, wantKept: true, wantDeletes: 1},
		{name: "writer after deleted success", writerErr: writerErr, keepFailure: true, wantCode: 1, wantKind: core.RunErrorProvider, wantDeletes: 1},
		{name: "first typed writer", writerErr: ExitError{Code: 69, Message: "typed timing failure"}, wantCode: 69, wantKind: core.RunErrorProvider, wantDeletes: 1},
		{name: "prepare helper code", stage: "prepare", keepFailure: true, wantCode: 9, wantKind: core.RunErrorProvider, wantKept: true},
		{name: "prepare default cleanup", stage: "prepare", wantCode: 9, wantKind: core.RunErrorProvider, wantDeletes: 1},
		{name: "prepare cleanup writer", stage: "prepare", cleanupErr: deleteErr, writerErr: writerErr, wantCode: 9, wantKind: core.RunErrorProvider, wantKept: true, wantDeletes: 1},
		{name: "missing command", stage: "command preparation", keepFailure: true, wantCode: 1, wantKind: core.RunErrorProvider, wantKept: true},
		{name: "sync guardrail code", stage: "guardrail", keepFailure: true, wantCode: 6, wantKind: core.RunErrorProvider, wantKept: true},
		{name: "archive and fallback", stage: "archive", runErr: transportErr, keepFailure: true, wantCode: 1, wantKind: core.RunErrorProvider, wantKept: true},
		{name: "tailnet workspace repair", stage: "ownership", runErr: transportErr, keepFailure: true, wantCode: 1, wantKind: core.RunErrorProvider, wantKept: true},
		{name: "artifact classification", stage: "artifact", keepFailure: true, wantCode: 7, wantKind: core.RunErrorProvider, wantKept: true},
		{name: "artifact writer", stage: "artifact", writerErr: writerErr, keepFailure: true, wantCode: 7, wantKind: core.RunErrorProvider, wantKept: true},
		{name: "download cleanup writer", stage: "download", cleanupErr: deleteErr, writerErr: writerErr, wantCode: 2, wantKind: core.RunErrorProvider, wantKept: true, wantDeletes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			client := &fakeIsloSyncClient{createName: "crabbox-repo-abcdef", execCodes: []int{0, tc.commandCode}, deleteErr: tc.cleanupErr}
			restore := swapNewIsloClient(client)
			defer restore()
			writer := &isloTimingFailureWriter{err: tc.writerErr}
			clock := &fixedClock{now: time.Now()}
			client.deleteHook = func() { clock.now = clock.now.Add(2 * time.Second) }
			b := &isloBackend{cfg: Config{Islo: IsloConfig{APIKey: "test", Workdir: "repo"}}, rt: Runtime{Stdout: io.Discard, Stderr: writer, Clock: clock}}
			req := RunRequest{Repo: Repo{Root: t.TempDir(), Name: "repo"}, NoSync: true, TimingJSON: true, Command: []string{"proof-command"}, Env: map[string]string{"APP_MODE": "fixture"}, Keep: tc.keep, KeepOnFailure: tc.keepFailure}
			client.execErrOnCommand, client.execErrOnCommandContains = tc.runErr, "proof-command"
			switch tc.stage {
			case "prepare":
				client.execCodes = []int{9}
			case "command preparation":
				req.Command = nil
			case "ownership":
				b.cfg.Tailscale = core.TailscaleConfig{Enabled: true, AuthKey: "test-tailnet-auth"}
				client.execOut = "CRABBOX_TS_IP=100.64.7.7"
				client.execErrOnCommandContains = "chown -R"
			case "archive", "guardrail":
				req.NoSync = false
				if err := os.WriteFile(filepath.Join(req.Repo.Root, "input.txt"), []byte("fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("git", "init")
				cmd.Dir = req.Repo.Root
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git init: %v %s", err, out)
				}
				if tc.stage == "guardrail" {
					b.cfg.Sync.FailFiles = 1
				}
				client.uploadErr = errors.New("archive API rejected")
				client.execErrOnCommandContains = "printf %s"
			case "artifact":
				req.RequiredArtifactGlobs = []string{"reports/missing.txt"}
			case "download":
				client.retrieveFiles = map[string][]byte{"reports/proof.txt": []byte("proof")}
				parent := filepath.Join(t.TempDir(), "not-directory")
				if err := os.WriteFile(parent, []byte("occupied"), 0o600); err != nil {
					t.Fatal(err)
				}
				req.Downloads = []string{"reports/proof.txt=" + filepath.Join(parent, "proof.txt")}
			}
			result, err := b.Run(context.Background(), req)
			result = core.FinalizeRunResult(result, err)
			if result.ExitCode != tc.wantCode || result.ErrorKind != tc.wantKind {
				t.Errorf("result code=%d kind=%q want %d/%q err=%v", result.ExitCode, result.ErrorKind, tc.wantCode, tc.wantKind, err)
			}
			if result.Session == nil || result.Session.Kept != tc.wantKept || result.Session.Reused {
				t.Errorf("session=%+v want kept=%v fresh", result.Session, tc.wantKept)
			}
			if client.deleteCalls != tc.wantDeletes {
				t.Errorf("deletes=%d want=%d", client.deleteCalls, tc.wantDeletes)
			}
			if _, exists, claimErr := resolveLeaseClaim("isb_crabbox-repo-abcdef"); claimErr != nil || exists != tc.wantKept {
				t.Errorf("claim exists=%v err=%v want=%v", exists, claimErr, tc.wantKept)
			}
			if result.Total != time.Duration(tc.wantDeletes)*2*time.Second {
				t.Errorf("total=%s must include completed delete stage", result.Total)
			}
			if result.Command != 0 {
				t.Errorf("command timer included non-command work: %s", result.Command)
			}
			if tc.wantCode == 0 {
				if err != nil {
					t.Errorf("unexpected err=%v", err)
				}
			} else {
				var public ExitError
				if !core.AsExitError(err, &public) || public.Code != tc.wantCode {
					t.Errorf("public code=%d err=%v want=%d", public.Code, err, tc.wantCode)
				}
				for _, cause := range []error{tc.runErr, tc.cleanupErr, tc.writerErr} {
					if cause != nil && (!errors.Is(err, cause) || !strings.Contains(public.Message, cause.Error())) {
						t.Errorf("missing cause or public diagnostic %q: %v / %q", cause, err, public.Message)
					}
				}
			}
			if tc.writerErr == nil {
				report := decodeLastTimingReport(t, writer.String())
				if report.ExitCode != tc.wantCode || report.ErrorKind != tc.wantKind || report.RunStatus != result.Status || report.TotalMs != result.Total.Milliseconds() {
					t.Errorf("timing=%+v result=%+v", report, result)
				}
			}
			workloadCalls := 0
			for _, request := range client.execRequests {
				if strings.Contains(strings.Join(request.GetCommand(), " "), "proof-command") {
					workloadCalls++
					if request.GetEnv()["APP_MODE"] == nil || *request.GetEnv()["APP_MODE"] != "fixture" {
						t.Error("workload env changed")
					}
				}
			}
			wantWorkloadCalls := 1
			switch tc.stage {
			case "prepare", "command preparation", "archive", "guardrail", "ownership":
				wantWorkloadCalls = 0
			}
			if workloadCalls != wantWorkloadCalls {
				t.Errorf("workload calls=%d want=%d", workloadCalls, wantWorkloadCalls)
			}

		})
	}
}

func TestIsloRunFinalErrorRedactsProviderKeyAndRetainsCauses(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const key = "synthetic-islo-credential"
	primary := errors.New("workload rejected " + key)
	secondary := errors.New("delete rejected " + key)
	client := &fakeIsloSyncClient{createName: "crabbox-repo-abcdef", execErrOnCommand: primary, execErrOnCommandContains: "proof-command", deleteErr: secondary}
	restore := swapNewIsloClient(client)
	defer restore()
	b := &isloBackend{cfg: Config{Islo: IsloConfig{APIKey: key, Workdir: "repo"}}, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
	result, err := b.Run(t.Context(), RunRequest{Repo: Repo{Root: t.TempDir(), Name: "repo"}, NoSync: true, Command: []string{"proof-command"}})
	var public ExitError
	if !core.AsExitError(err, &public) || public.Code != 1 || result.ExitCode != 1 || !errors.Is(err, primary) || !errors.Is(err, secondary) {
		t.Fatalf("lost selected code or causes: result=%+v publicCode=%d", result, public.Code)
	}
	if strings.Contains(err.Error(), key) || !strings.Contains(public.Message, "[redacted]") || !strings.Contains(public.Message, "delete rejected") {
		t.Fatal("final public diagnostic did not preserve redacted primary and secondary messages")
	}
}

func TestIsloRunRetainsBoundReuseAfterTailnetFailure(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		cause       error
		kind        core.RunErrorKind
		writer      bool
	}{
		{"resume typed failure", "resume", ExitError{Code: 69, Message: "resume unavailable"}, core.RunErrorProvider, false},
		{"health typed failure", "health", ExitError{Code: 69, Message: "health unavailable"}, core.RunErrorProvider, false},
		{"health cancellation", "health", context.Canceled, core.RunErrorCanceled, false},
		{"health deadline writer", "health", context.DeadlineExceeded, core.RunErrorTimeout, true},
		{"repair typed failure", "repair", ExitError{Code: 69, Message: "repair unavailable"}, core.RunErrorProvider, false},
		{"repair cancellation", "repair", context.Canceled, core.RunErrorCanceled, false},
		{"metadata update failure", "metadata", nil, core.RunErrorProvider, false},
		{"not yet running", "starting", nil, core.RunErrorProvider, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			t.Setenv("XDG_STATE_HOME", state)
			claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "reuse", isloTeardownName, isloTestResourceID, isloTestClaimScope)
			if err := updateLeaseClaimTailscale(isloTeardownLeaseID, "100.64.7.7", ""); err != nil {
				t.Fatal(err)
			}
			original, _, _ := resolveExactIsloLeaseClaim(isloTeardownLeaseID)
			client := &fakeIsloSyncClient{getSandbox: &gosdk.SandboxResponse{ID: isloTestResourceID, Name: isloTeardownName, Status: "running"}, execOut: "CRABBOX_TS_IP=100.64.7.7"}
			clock := &fixedClock{now: time.Now()}
			client.getSandboxHook = func() { clock.now = clock.now.Add(time.Second) }
			claimPath := filepath.Join(state, "crabbox", "claims", isloTeardownLeaseID+".json")
			switch tc.stage {
			case "resume":
				client.getSandbox.Status = "paused"
				client.resumeErr = tc.cause
			case "health":
				client.execErrOnCommand = tc.cause
				client.execErrOnCommandContains = `"BackendState"`
			case "repair":
				client.execCodes = []int{1}
				client.execErrOnCommand = tc.cause
				client.execErrOnCommandContains = "TS_AUTH_VALUE"
			case "starting":
				client.getSandbox.Status = "starting"
			case "metadata":
				client.execHook = func(req *gosdk.ExecRequest) {
					if strings.Contains(strings.Join(req.GetCommand(), " "), `"BackendState"`) {
						if err := os.WriteFile(claimPath, []byte("invalid-json"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			writerErr := errors.New("timing writer failed")
			writer := &isloTimingFailureWriter{}
			if tc.writer {
				writer.err = writerErr
			}
			b := newIsloTeardownBackend(t, client, writer)
			b.rt.Clock = clock
			wantCode := 1
			if tc.stage == "metadata" {
				wantCode = 2 // Existing claim parse errors already have a public code.
			}
			result, err := b.Run(context.Background(), RunRequest{ID: isloTeardownLeaseID, NoSync: true, TimingJSON: true, Command: []string{"workload-must-not-run"}})
			if err == nil || core.ExitCodeForError(err, 1) != wantCode || result.ExitCode != wantCode || result.ErrorKind != tc.kind {
				t.Errorf("outcome=%d/%s public=%d err=%v", result.ExitCode, result.ErrorKind, core.ExitCodeForError(err, 1), err)
			}
			if result.Session == nil || !result.Session.Reused || !result.Session.Kept || result.Session.LeaseID != original.LeaseID {
				t.Errorf("bound reuse session=%+v", result.Session)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Errorf("missing actual cause %v", tc.cause)
			}
			if tc.writer && !errors.Is(err, writerErr) {
				t.Error("missing secondary writer cause")
			}
			if result.Command != 0 || result.Total != time.Second {
				t.Errorf("timing command=%s total=%s", result.Command, result.Total)
			}
			if !tc.writer {
				report := decodeLastTimingReport(t, writer.String())
				if report.ExitCode != wantCode || report.ErrorKind != tc.kind || report.RunStatus != result.Status || report.TotalMs != 1000 {
					t.Errorf("report=%+v", report)
				}
			}
			if client.deleteCalls != 0 || client.createRequest != nil {
				t.Fatal("reused failure acquired or deleted a resource")
			}
			for _, req := range client.execRequests {
				if strings.Contains(strings.Join(req.GetCommand(), " "), "workload-must-not-run") {
					t.Error("workload ran after failed tailnet preparation")
				}
				if req.GetUser() == nil || *req.GetUser() != isloAdminUser {
					t.Error("tailnet preparation lost root user")
				}
			}
			if tc.stage == "metadata" {
				data, readErr := os.ReadFile(claimPath)
				if readErr != nil || string(data) != "invalid-json" {
					t.Error("finalizer rewrote corrupted claim")
				}
			} else {
				current, exists, readErr := resolveExactIsloLeaseClaim(isloTeardownLeaseID)
				if readErr != nil || !exists || !sameIsloRunOwnership(original, current) {
					t.Error("reused failure lost original identity/owner")
				}
			}
		})
	}
}

func TestIsloRunTailnetAdmissionStillRejectsBeforeBinding(t *testing.T) {
	for _, kind := range []string{"missing claim", "corrupt claim", "scope", "id", "missing sandbox", "terminal", "unenrolled request"} {
		t.Run(kind, func(t *testing.T) {
			state := t.TempDir()
			t.Setenv("XDG_STATE_HOME", state)
			client := &fakeIsloSyncClient{getSandbox: &gosdk.SandboxResponse{ID: isloTestResourceID, Name: isloTeardownName, Status: "running"}}
			if kind != "missing claim" {
				scope := isloTestClaimScope
				if kind == "scope" {
					scope = "endpoint:https://other.example"
				}
				claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "reuse", isloTeardownName, isloTestResourceID, scope)
				if kind != "unenrolled request" {
					if err := updateLeaseClaimTailscale(isloTeardownLeaseID, "100.64.7.7", ""); err != nil {
						t.Fatal(err)
					}
				}
			}
			switch kind {
			case "corrupt claim":
				if err := os.WriteFile(filepath.Join(state, "crabbox", "claims", isloTeardownLeaseID+".json"), []byte("invalid-json"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "id":
				client.getSandbox.ID = "0195f3d2-5c1a-7c39-9c1e-000000000000"
			case "missing sandbox":
				client.getSandboxGone = true
			case "terminal":
				client.getSandbox.Status = "deleted"
			}
			b := newIsloTeardownBackend(t, client, io.Discard)
			if kind == "missing claim" || kind == "unenrolled request" {
				b.cfg.Tailscale.Enabled = true
			}
			result, err := b.Run(context.Background(), RunRequest{ID: isloTeardownLeaseID, NoSync: true, Command: []string{"workload-must-not-run"}})
			if err == nil || result.Session != nil {
				t.Errorf("admission err=%v session=%+v", err, result.Session)
			}
			if client.createRequest != nil || client.deleteCalls != 0 || client.resumeCalls != 0 || len(client.execRequests) != 0 {
				t.Error("unbound admission performed remote mutation/execution")
			}
		})
	}
}

func TestIsloRunPreservesPlainAndLegacyReuseAdmission(t *testing.T) {
	for _, enrolled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enrolled), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			claimIsloLegacyLease(t, isloTeardownLeaseID)
			client := &fakeIsloSyncClient{execOut: "CRABBOX_TS_IP=100.64.7.7"}
			if enrolled {
				if err := updateLeaseClaimTailscale(isloTeardownLeaseID, "100.64.7.7", ""); err != nil {
					t.Fatal(err)
				}
				client.getSandbox = &gosdk.SandboxResponse{Name: isloTeardownName, Status: "running"}
			} else {
				client.getSandboxErr = errors.New("plain reuse must not introduce a lookup")
			}
			b := newIsloTeardownBackend(t, client, io.Discard)
			result, err := b.Run(context.Background(), RunRequest{ID: isloTeardownLeaseID, NoSync: true, Command: []string{"true"}})
			if err != nil || result.Session == nil || !result.Session.Reused || !result.Session.Kept {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			claim, ok, err := resolveExactIsloLeaseClaim(isloTeardownLeaseID)
			if err != nil || !ok || isloClaimIdentity(claim).ID != "" {
				t.Error("legacy claim identity was promoted or lost")
			}
			if !enrolled && len(client.getSandboxNames) != 0 {
				t.Error("plain reuse added a live lookup")
			}
			if client.deleteCalls != 0 || client.createRequest != nil {
				t.Error("reuse created or deleted a resource")
			}
		})
	}
}

func TestIsloCreateFailureReportsUnconfirmedAttemptWithoutAcquisition(t *testing.T) {
	for _, mode := range []string{"run", "warmup"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			isolateIsloTestHome(t)
			cause := errors.Join(ExitError{Code: 69, Message: "synthetic create failure with synthetic-key"}, context.DeadlineExceeded)
			client := &fakeIsloSyncClient{createErr: cause}
			restore := swapNewIsloClient(client)
			defer restore()
			cfg := core.BaseConfig()
			cfg.Islo.APIKey = "synthetic-key"
			var output bytes.Buffer
			backend := &isloBackend{cfg: cfg, rt: Runtime{Stdout: &output, Stderr: &output}}
			repo := Repo{Root: t.TempDir(), Name: "proof"}
			var result RunResult
			var err error
			if mode == "run" {
				result, err = backend.Run(context.Background(), RunRequest{Repo: repo, Command: []string{"true"}})
			} else {
				err = backend.Warmup(context.Background(), WarmupRequest{Repo: repo})
			}
			if err == nil || core.ExitCodeForError(err, 1) != 69 || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("typed cause/code lost: %v", err)
			}
			name := *client.createRequest.Name
			if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "unconfirmed") || strings.Contains(err.Error(), cfg.Islo.APIKey) {
				t.Fatalf("missing unconfirmed attempt locator: %v", err)
			}
			if result.LeaseID != "" || result.Session != nil || strings.Contains(output.String(), "leased ") {
				t.Fatalf("failed create reported acquired result: %#v %s", result, output.String())
			}
			if client.deleteCalls != 0 || len(client.execRequests) != 0 || client.uploadPath != "" {
				t.Fatal("failed create caused cleanup or workload")
			}
			entries, readErr := os.ReadDir(filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims"))
			if !errors.Is(readErr, os.ErrNotExist) && (readErr != nil || len(entries) != 0) {
				t.Fatalf("failed create published claim: %v %v", entries, readErr)
			}
		})
	}
}

type incompleteIsloCreateClient struct {
	fakeIsloSyncClient
	response *gosdk.SandboxResponse
}

func (f *incompleteIsloCreateClient) CreateSandbox(_ context.Context, req *gosdk.CreateSandboxRequest) (*gosdk.SandboxResponse, error) {
	f.createRequest = req
	return f.response, nil
}

func TestIsloIncompleteCreateResponseReportsUnconfirmedAttempt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response *gosdk.SandboxResponse
	}{
		{"nil response", nil},
		{"missing name", &gosdk.SandboxResponse{ID: "synthetic-id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			isolateIsloTestHome(t)
			client := &incompleteIsloCreateClient{response: tc.response}
			restore := swapNewIsloClient(client)
			defer restore()
			cfg := core.BaseConfig()
			cfg.Islo.APIKey = "synthetic-key"
			var output bytes.Buffer
			backend := &isloBackend{cfg: cfg, rt: Runtime{Stdout: &output, Stderr: &output}}
			result, err := backend.Run(context.Background(), RunRequest{Repo: Repo{Root: t.TempDir(), Name: cfg.Islo.APIKey}, Command: []string{"true"}})
			if err == nil || core.ExitCodeForError(err, 1) != 5 {
				t.Fatalf("incomplete response changed exit5: %v", err)
			}
			if !strings.Contains(err.Error(), "unconfirmed") || !strings.Contains(err.Error(), "name=\"crabbox-") || strings.Contains(err.Error(), cfg.Islo.APIKey) {
				t.Fatalf("missing safe unconfirmed name: %v", err)
			}
			if result.LeaseID != "" || result.Session != nil || strings.Contains(output.String(), "leased ") {
				t.Fatalf("incomplete response reported acquisition: %#v %s", result, output.String())
			}
			if client.deleteCalls != 0 || len(client.execRequests) != 0 || client.uploadPath != "" {
				t.Fatal("incomplete response caused cleanup or workload")
			}
			entries, readErr := os.ReadDir(filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims"))
			if !errors.Is(readErr, os.ErrNotExist) && (readErr != nil || len(entries) != 0) {
				t.Fatalf("incomplete response published claim: %v %v", entries, readErr)
			}
		})
	}
}
