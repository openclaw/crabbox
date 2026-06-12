package islo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
	core "github.com/openclaw/crabbox/internal/cli"
)

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
	leaseID, name, slug, err := resolveIsloLeaseID("crabbox-repo-abcdef", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "isb_crabbox-repo-abcdef" || name != "crabbox-repo-abcdef" || slug == "" {
		t.Fatalf("lease=%q name=%q slug=%q", leaseID, name, slug)
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
	if lease.SSH.Key != "" || len(lease.SSH.FallbackPorts) != 0 || !lease.SSH.SSHConfigProxy || !lease.SSH.DisableHostKeyChecking {
		t.Fatalf("islo ssh should not force Crabbox's default key or fallback ports: %#v", lease.SSH)
	}
	if lease.Server.PublicNet.IPv4.IP != "crabbox-repo-abcdef.islo" || lease.Server.Labels["ssh_host"] != "crabbox-repo-abcdef.islo" {
		t.Fatalf("server ssh labels=%#v public=%q", lease.Server.Labels, lease.Server.PublicNet.IPv4.IP)
	}
}

func TestIsloResolveSSHHonorsExplicitSSHOverrides(t *testing.T) {
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

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "crabbox-repo-abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.User != "alice" || lease.SSH.Port != "2022" || lease.SSH.Key != "/tmp/islo-key" {
		t.Fatalf("ssh target=%#v", lease.SSH)
	}
}

func TestIsloResolveSSHResumesPausedSandbox(t *testing.T) {
	client := &fakeIsloSyncClient{
		getSandbox: &gosdk.SandboxResponse{Name: "crabbox-repo-abcdef", Status: "paused"},
	}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test"}},
		rt:  Runtime{Stderr: io.Discard},
	}

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "crabbox-repo-abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if client.resumeCalls != 1 || lease.Server.Status != "running" {
		t.Fatalf("resumeCalls=%d status=%q", client.resumeCalls, lease.Server.Status)
	}
}

func TestIsloResolveSSHRejectsForeignClaimBeforeResume(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
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

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "crabbox-repo-abcdef"})
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
		rt:  Runtime{Stdout: io.Discard, Stderr: io.Discard},
	}

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    Repo{Root: root, Name: "repo"},
		Keep:    true,
		Command: []string{"true"},
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
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("result=%#v", result)
	}
	if client.deleteCalls != 1 {
		t.Fatalf("delete calls=%d want 1", client.deleteCalls)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Run took %s, want bounded cleanup", elapsed)
	}
	if !strings.Contains(stderr.String(), "warning: islo stop failed for crabbox-repo-abcdef: context deadline exceeded") {
		t.Fatalf("stderr=%q, want cleanup timeout warning", stderr.String())
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
	_, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "")
	if err == nil || !strings.Contains(err.Error(), "escapes /workspace") {
		t.Fatalf("createSandbox err=%v, want workdir containment error", err)
	}
	if client.createRequest != nil {
		t.Fatalf("CreateSandbox was called with %#v", client.createRequest)
	}
}

func TestIsloCreateSandboxPassesRelativeWorkdirToProvider(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{createName: "crabbox-repo-abcdef"}
	backend := &isloBackend{
		cfg: Config{Islo: IsloConfig{Workdir: "team/repo"}},
		rt:  Runtime{Stderr: io.Discard},
	}
	_, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if client.createRequest == nil || client.createRequest.Workdir == nil || *client.createRequest.Workdir != "team/repo" {
		t.Fatalf("create workdir=%v", client.createRequest)
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
	_, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "")
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
	leaseID, _, slug, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "web")
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
	leaseID, _, slug, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "node-a")
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
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	client := &fakeIsloSyncClient{
		createName: "crabbox-repo-abcdef",
		execErr:    errors.New("tailscale failed"),
		deleteErr:  errors.New("delete failed"),
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
	_, _, _, err := backend.createSandbox(context.Background(), client, Repo{Root: t.TempDir(), Name: "repo"}, false, "node-a")
	if err == nil || !strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("expected cleanup failure, got %v", err)
	}
	claim, ok, claimErr := resolveLeaseClaim("isb_crabbox-repo-abcdef")
	if claimErr != nil || !ok {
		t.Fatalf("claim should remain discoverable after failed rollback: ok=%t err=%v claim=%#v", ok, claimErr, claim)
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
		case "/sandboxes/":
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
	execErrOnCommand         error
	execErrOnCommandContains string
	execErrOnCommandSkip     int
	execErrOnCommandHook     func()
	execDeadlineCommand      string
	execDeadline             time.Time
	rejectCanceledContext    bool
	closeUploadReader        bool
	createRequest            *gosdk.SandboxCreate
	createName               string
	getSandbox               *gosdk.SandboxResponse
	getSandboxes             []*gosdk.SandboxResponse
	getSandboxErr            error
	getSandboxGone           bool
	resumeErr                error
	resumeCalls              int
	blockDelete              bool
	deleteErr                error
	deleteCalls              int
	pausedName               string
	resumedName              string
}

func (f *fakeIsloSyncClient) CreateSandbox(_ context.Context, req *gosdk.SandboxCreate) (*gosdk.SandboxResponse, error) {
	f.createRequest = req
	name := f.createName
	if name == "" {
		name = "crabbox-test-abcdef"
	}
	return &gosdk.SandboxResponse{Name: name}, nil
}

func (f *fakeIsloSyncClient) GetSandbox(_ context.Context, name string) (*gosdk.SandboxResponse, error) {
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
	return &gosdk.SandboxResponse{Name: name, Status: "running"}, nil
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
	return nil, nil
}

func (f *fakeIsloSyncClient) DeleteSandbox(ctx context.Context, _ string) error {
	f.deleteCalls++
	if f.blockDelete {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.deleteErr != nil {
		return f.deleteErr
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
	callIndex := len(f.execRequests) - 1
	command := strings.Join(req.GetCommand(), " ")
	if f.execDeadlineCommand != "" && strings.Contains(command, f.execDeadlineCommand) {
		f.execDeadline, _ = ctx.Deadline()
	}
	f.prepareCommands = append(f.prepareCommands, command)
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
