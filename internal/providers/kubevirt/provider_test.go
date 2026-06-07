package kubevirt

import (
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"gopkg.in/yaml.v3"
)

func testConfig(t *testing.T) core.Config {
	t.Helper()
	template := filepath.Join(t.TempDir(), "vm.yaml")
	data := `apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: replace-me
spec:
  runStrategy: Manual
  template:
    spec:
      domain:
        devices: {}
      volumes:
        - name: cloudinit
          cloudInitNoCloud:
            userData: |
              user: {{SSH_PUBLIC_KEY}}
`
	if err := os.WriteFile(template, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := core.BaseConfig()
	cfg.KubeVirt = core.KubeVirtConfig{
		Kubectl:         "kubectl-custom",
		Virtctl:         "virtctl-custom",
		Kubeconfig:      "/tmp/kube config",
		Context:         "test-context",
		Namespace:       "test-ns",
		Template:        template,
		SSHUser:         "tester",
		SSHKey:          "/tmp/id_test",
		SSHPublicKey:    "ssh-ed25519 AAAA test",
		SSHPort:         "2222",
		WorkRoot:        "/home/tester/crabbox",
		DeleteOnRelease: true,
	}
	return cfg
}

func isolateCrabboxState(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	return home
}

func claimKubeVirtLease(t *testing.T, cfg core.Config, leaseID, slug, repoRoot string, idleTimeout time.Duration, reclaim bool) {
	t.Helper()
	backend := &leaseBackend{cfg: cfg}
	if err := backend.claimLeaseForRepo(leaseID, slug, repoRoot, idleTimeout, reclaim); err != nil {
		t.Fatal(err)
	}
}

func TestProviderSpec(t *testing.T) {
	spec := (Provider{}).Spec()
	if spec.Name != providerName || spec.Family != "kubernetes" {
		t.Fatalf("spec=%#v", spec)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode} {
		if !spec.Features.Has(feature) {
			t.Fatalf("missing feature %s", feature)
		}
	}
}

func TestRouteConfigUsesProviderWorkRoot(t *testing.T) {
	cfg := testConfig(t)
	cfg.WorkRoot = core.BaseConfig().WorkRoot
	if err := (Provider{}).RouteConfig(&cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.WorkRoot != "/home/tester/crabbox" {
		t.Fatalf("work root=%q", cfg.WorkRoot)
	}
}

func TestConfigurePreservesExplicitTopLevelWorkRoot(t *testing.T) {
	cfg := testConfig(t)
	cfg.WorkRoot = "/workspace/top-level"
	cfg.KubeVirt.WorkRoot = core.BaseConfig().KubeVirt.WorkRoot
	backend, err := (Provider{}).Configure(cfg, core.Runtime{Exec: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := backend.(*leaseBackend).cfg.WorkRoot; got != "/workspace/top-level" {
		t.Fatalf("work root=%q", got)
	}
}

func TestConfigureProviderWorkRootOverridesTopLevelWorkRoot(t *testing.T) {
	cfg := testConfig(t)
	cfg.WorkRoot = "/workspace/top-level"
	cfg.KubeVirt.WorkRoot = "/workspace/provider"
	backend, err := (Provider{}).Configure(cfg, core.Runtime{Exec: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := backend.(*leaseBackend).cfg.WorkRoot; got != "/workspace/provider" {
		t.Fatalf("work root=%q", got)
	}
}

func TestConfigureRejectsUnsafeTopLevelWorkRoot(t *testing.T) {
	cfg := testConfig(t)
	cfg.WorkRoot = "/tmp"
	cfg.KubeVirt.WorkRoot = core.BaseConfig().KubeVirt.WorkRoot
	if _, err := (Provider{}).Configure(cfg, core.Runtime{Exec: &recordingRunner{}}); err == nil || !strings.Contains(err.Error(), "too broad") {
		t.Fatalf("err=%v", err)
	}
}

func TestConfigureRequiresExplicitContext(t *testing.T) {
	cfg := testConfig(t)
	cfg.KubeVirt.Context = ""
	if _, err := (Provider{}).Configure(cfg, core.Runtime{Exec: &recordingRunner{}}); err == nil || !strings.Contains(err.Error(), "context is required") {
		t.Fatalf("err=%v", err)
	}
}

func TestConfigureDoesNotRequireProvisioningTemplate(t *testing.T) {
	cfg := testConfig(t)
	cfg.KubeVirt.Template = ""
	if _, err := (Provider{}).Configure(cfg, core.Runtime{Exec: &recordingRunner{}}); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireRequiresProvisioningTemplate(t *testing.T) {
	cfg := testConfig(t)
	cfg.KubeVirt.Template = ""
	runner := &recordingRunner{}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if _, err := backend.Acquire(context.Background(), core.AcquireRequest{}); err == nil || !strings.Contains(err.Error(), "template is required") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestFlagsExpandKubeVirtUserPaths(t *testing.T) {
	home := isolateCrabboxState(t)
	cfg := testConfig(t)
	fs := flag.NewFlagSet("kubevirt", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--kubevirt-kubectl=~/bin/kubectl",
		"--kubevirt-virtctl=~/bin/virtctl",
		"--kubevirt-kubeconfig=~/.kube/config",
		"--kubevirt-template=~/templates/vm.yaml",
		"--kubevirt-ssh-key=~/.ssh/id_ed25519",
		"--kubevirt-ssh-public-key=~/.ssh/id_ed25519.pub",
	}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	for label, got := range map[string]string{
		"kubectl":      cfg.KubeVirt.Kubectl,
		"virtctl":      cfg.KubeVirt.Virtctl,
		"kubeconfig":   cfg.KubeVirt.Kubeconfig,
		"template":     cfg.KubeVirt.Template,
		"sshKey":       cfg.KubeVirt.SSHKey,
		"sshPublicKey": cfg.KubeVirt.SSHPublicKey,
	} {
		if !filepath.IsAbs(got) || !strings.HasPrefix(got, home+string(os.PathSeparator)) {
			t.Fatalf("%s path=%q home=%q", label, got, home)
		}
	}
}

func TestSSHKeyReadsFileFormPublicKey(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	privateKey := filepath.Join(dir, "id_ed25519")
	publicKeyPath := privateKey + ".pub"
	const publicKey = "ssh-ed25519 AAAA from-file"
	if err := os.WriteFile(publicKeyPath, []byte(publicKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.KubeVirt.SSHKey = privateKey
	cfg.KubeVirt.SSHPublicKey = publicKeyPath
	backend := &leaseBackend{cfg: cfg}
	keyPath, gotPublicKey, err := backend.sshKey("cbx_123")
	if err != nil {
		t.Fatal(err)
	}
	if keyPath != privateKey || gotPublicKey != publicKey {
		t.Fatalf("keyPath=%q publicKey=%q", keyPath, gotPublicKey)
	}
}

func TestRenderManifestSetsIdentityLabelsAndPlaceholders(t *testing.T) {
	cfg := testConfig(t)
	data, err := renderManifest(cfg.KubeVirt.Template, "crabbox-test-deadbeef", "test-ns", "cbx_123", "test", cfg.KubeVirt.SSHPublicKey, map[string]string{
		"expires_at": "12345",
		"keep":       "false",
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	metadata := doc["metadata"].(map[string]any)
	if metadata["name"] != "crabbox-test-deadbeef" || metadata["namespace"] != "test-ns" {
		t.Fatalf("metadata=%#v", metadata)
	}
	labels := metadata["labels"].(map[string]any)
	if labels[managedByLabel] != "crabbox" || labels[leaseIDLabel] != "cbx_123" || labels[slugLabel] != "test" {
		t.Fatalf("labels=%#v", labels)
	}
	annotations := metadata["annotations"].(map[string]any)
	if annotations[annotationBase+"expires_at"] != "12345" || annotations[annotationBase+"keep"] != "false" {
		t.Fatalf("annotations=%#v", annotations)
	}
	if strings.Contains(string(data), "{{SSH_PUBLIC_KEY}}") || !strings.Contains(string(data), cfg.KubeVirt.SSHPublicKey) {
		t.Fatalf("manifest=%s", data)
	}
}

func TestRenderManifestReplacesUnquotedSequencePlaceholder(t *testing.T) {
	cfg := testConfig(t)
	data := `apiVersion: kubevirt.io/v1
kind: VirtualMachine
spec:
  runStrategy: Manual
  values:
    - {{SSH_PUBLIC_KEY}}
`
	if err := os.WriteFile(cfg.KubeVirt.Template, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	rendered, err := renderManifest(cfg.KubeVirt.Template, "vm", "test-ns", "cbx_123", "test", cfg.KubeVirt.SSHPublicKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(rendered, &doc); err != nil {
		t.Fatal(err)
	}
	values := doc["spec"].(map[string]any)["values"].([]any)
	if len(values) != 1 || values[0] != cfg.KubeVirt.SSHPublicKey {
		t.Fatalf("values=%#v\nmanifest=%s", values, rendered)
	}
}

func TestRenderManifestRequiresManualRunStrategy(t *testing.T) {
	cfg := testConfig(t)
	data, err := os.ReadFile(cfg.KubeVirt.Template)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "runStrategy: Manual", "runStrategy: Always", 1))
	if err := os.WriteFile(cfg.KubeVirt.Template, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := renderManifest(cfg.KubeVirt.Template, "vm", "test-ns", "cbx_123", "test", cfg.KubeVirt.SSHPublicKey, nil); err == nil || !strings.Contains(err.Error(), "spec.runStrategy must be Manual") {
		t.Fatalf("err=%v", err)
	}
}

func TestProxyCommandUsesVirtctlControlPlaneForwarding(t *testing.T) {
	cfg := testConfig(t)
	backend := &leaseBackend{cfg: cfg}
	command := backend.proxyCommand("crabbox-test-deadbeef")
	for _, want := range []string{
		"'virtctl-custom'",
		"'--kubeconfig' '/tmp/kube config'",
		"'--context' 'test-context'",
		"'--namespace' 'test-ns'",
		"'port-forward' '--stdio=true' 'vm/crabbox-test-deadbeef/test-ns' '%p'",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("proxy command %q missing %q", command, want)
		}
	}
	target := backend.sshTarget("crabbox-test-deadbeef", "/tmp/id_test")
	if !target.SSHConfigProxy || target.ProxyCommand == "" || target.Port != "2222" {
		t.Fatalf("target=%#v", target)
	}
}

func TestCreateVMUsesKubectlApplyThenVirtctlStart(t *testing.T) {
	cfg := testConfig(t)
	runner := &recordingRunner{}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if err := backend.createVM(context.Background(), "crabbox-test-deadbeef", "cbx_123", "test", cfg.KubeVirt.SSHPublicKey, false); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls=%#v", runner.calls)
	}
	if !strings.Contains(runner.calls[0], "kubectl-custom --kubeconfig /tmp/kube config --context test-context --namespace test-ns apply -f ") {
		t.Fatalf("apply=%q", runner.calls[0])
	}
	if runner.calls[1] != "virtctl-custom --kubeconfig /tmp/kube config --context test-context --namespace test-ns start crabbox-test-deadbeef" {
		t.Fatalf("start=%q", runner.calls[1])
	}
	if !strings.Contains(runner.manifest, "crabbox.dev/lease-id: cbx_123") {
		t.Fatalf("manifest=%s", runner.manifest)
	}
}

func TestCreateVMRollsBackWhenStartFails(t *testing.T) {
	cfg := testConfig(t)
	runner := &recordingRunner{failAt: 2}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if err := backend.createVM(context.Background(), "crabbox-test-deadbeef", "cbx_123", "test", cfg.KubeVirt.SSHPublicKey, false); err == nil {
		t.Fatal("expected start failure")
	}
	if len(runner.calls) != 3 || !strings.Contains(runner.calls[2], "delete virtualmachine.kubevirt.io/crabbox-test-deadbeef") {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestStartVMIgnoresAlreadyRunningConflict(t *testing.T) {
	cfg := testConfig(t)
	runner := &recordingRunner{failAt: 1, failStderr: "VirtualMachine is already running"}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if err := backend.startVM(context.Background(), "vm-running"); err != nil {
		t.Fatalf("startVM: %v", err)
	}
}

func TestWaitForVMIReadyForSSHReturnsWhenRunning(t *testing.T) {
	cfg := testConfig(t)
	runner := &recordingRunner{stdout: `{"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if _, err := backend.waitForVMIReadyForSSH(context.Background(), "vm-running", time.Second); err != nil {
		t.Fatalf("waitForVMIReadyForSSH: %v", err)
	}
	if len(runner.calls) != 1 || !strings.Contains(runner.calls[0], "get virtualmachineinstances.kubevirt.io/vm-running") {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestWaitForVMIReadyForSSHAllowsScheduledStatus(t *testing.T) {
	cfg := testConfig(t)
	runner := &recordingRunner{stdout: `{"status":{"phase":"Scheduled","conditions":[{"type":"Ready","status":"False","reason":"GuestNotRunning","message":"Guest VM is not reported as running"}]}}`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	status, err := backend.waitForVMIReadyForSSH(context.Background(), "vm-scheduled", time.Second)
	if err != nil {
		t.Fatalf("waitForVMIReadyForSSH: %v", err)
	}
	if status.Phase != "Scheduled" {
		t.Fatalf("phase=%q", status.Phase)
	}
}

func TestWaitForVMIReadyForSSHReportsConditionsAndEvents(t *testing.T) {
	cfg := testConfig(t)
	runner := &recordingRunner{outputs: []string{
		`{"status":{"phase":"Scheduling","conditions":[{"type":"Ready","status":"False","reason":"GuestNotRunning","message":"Guest VM is not reported as running"}]}}`,
		`{"items":[{"type":"Normal","reason":"Created","message":"VirtualMachineInstance defined."}]}`,
	}}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	_, err := backend.waitForVMIReadyForSSH(context.Background(), "vm-stuck", time.Nanosecond)
	if err == nil {
		t.Fatal("expected timeout")
	}
	for _, want := range []string{"GuestNotRunning", "Guest VM is not reported as running", "VirtualMachineInstance defined"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
	if len(runner.calls) != 2 || !strings.Contains(runner.calls[1], "get events --field-selector involvedObject.name=vm-stuck") {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestReleaseDeletesGeneratedKey(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig(t)
	cfg.KubeVirt.SSHKey = ""
	cfg.KubeVirt.SSHPublicKey = ""
	keyPath, _, err := core.EnsureTestboxKey("cbx_release")
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{stdout: `{"metadata":{"name":"vm-release","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_release","crabbox.dev/slug":"release"}}}`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	lease := core.LeaseTarget{LeaseID: "cbx_release", Server: core.Server{Name: "vm-release"}}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("generated key still exists: %v", err)
	}
}

func TestReleaseRetainedVMPreservesClaimAndKey(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig(t)
	cfg.KubeVirt.SSHKey = ""
	cfg.KubeVirt.SSHPublicKey = ""
	cfg.KubeVirt.DeleteOnRelease = false
	keyPath, _, err := core.EnsureTestboxKey("cbx_retained")
	if err != nil {
		t.Fatal(err)
	}
	claimKubeVirtLease(t, cfg, "cbx_retained", "retained", t.TempDir(), cfg.IdleTimeout, false)
	runner := &recordingRunner{stdout: `{"metadata":{"name":"vm-retained","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_retained","crabbox.dev/slug":"retained"}}}`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	lease := core.LeaseTarget{LeaseID: "cbx_retained", Server: core.Server{Name: "vm-retained"}}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("retained key missing: %v", err)
	}
	if claim, ok, err := core.ResolveLeaseClaimForProvider("retained", providerName); err != nil || !ok || claim.LeaseID != "cbx_retained" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestStatusDoesNotStartRetainedStoppedVM(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig(t)
	claimKubeVirtLease(t, cfg, "cbx_retained", "retained", t.TempDir(), time.Minute, false)
	server := core.Server{Name: "vm-retained", Labels: map[string]string{"name": "vm-retained"}}
	if err := core.UpdateLeaseClaimEndpoint("cbx_retained", server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{stdout: `{"metadata":{"name":"vm-retained","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_retained","crabbox.dev/slug":"retained"},"annotations":{"crabbox.dev/state":"ready"}},"status":{"printableStatus":"Stopped"}}`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	view, err := backend.Status(context.Background(), core.StatusRequest{ID: "retained"})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "Stopped" || view.Ready {
		t.Fatalf("status=%#v", view)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, " start ") {
			t.Fatalf("status started VM: calls=%#v", runner.calls)
		}
	}
}

func TestStatusRequiresSSHProbeBeforeReady(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig(t)
	claimKubeVirtLease(t, cfg, "cbx_running", "running", t.TempDir(), time.Minute, false)
	server := core.Server{Name: "vm-running", Labels: map[string]string{"name": "vm-running"}}
	if err := core.UpdateLeaseClaimEndpoint("cbx_running", server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{outputs: []string{
		`{"metadata":{"name":"vm-running","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_running","crabbox.dev/slug":"running"},"annotations":{"crabbox.dev/state":"ready"}},"status":{"printableStatus":"Running"}}`,
		`{"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}`,
	}}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	view, err := backend.Status(ctx, core.StatusRequest{ID: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Ready {
		t.Fatalf("status=%#v", view)
	}
	if len(runner.calls) != 2 || !strings.Contains(runner.calls[1], "get virtualmachineinstances.kubevirt.io/vm-running") {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestResolveNameUsesProviderScopedClaim(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig(t)
	repo := t.TempDir()
	if err := core.ClaimLeaseForRepoProvider("cbx_external", "shared", "external", repo, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	claimKubeVirtLease(t, cfg, "cbx_kubevirt", "shared", repo, time.Minute, false)
	runner := &recordingRunner{stdout: `{"metadata":{"name":"crabbox-shared-kubevirt","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_kubevirt","crabbox.dev/slug":"shared"}}}`}
	if claim, ok, err := core.ResolveLeaseClaimForProvider("shared", providerName); err != nil || !ok {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	} else {
		server := core.Server{Name: "crabbox-shared-kubevirt", Labels: map[string]string{"name": "crabbox-shared-kubevirt"}}
		if err := core.UpdateLeaseClaimEndpoint(claim.LeaseID, server, core.SSHTarget{}); err != nil {
			t.Fatal(err)
		}
	}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	_, leaseID, _, _, err := backend.resolveIdentity(context.Background(), "shared")
	if err != nil {
		t.Fatal(err)
	}
	if leaseID != "cbx_kubevirt" {
		t.Fatalf("leaseID=%q", leaseID)
	}
}

func TestClaimLeaseUsesKubeVirtScope(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig(t)
	claimKubeVirtLease(t, cfg, "cbx_scoped", "scoped", t.TempDir(), time.Minute, false)
	claim, err := core.ReadLeaseClaim("cbx_scoped")
	if err != nil {
		t.Fatal(err)
	}
	if claim.ProviderScope != kubeVirtClaimScope(cfg) {
		t.Fatalf("provider scope=%q, want %q", claim.ProviderScope, kubeVirtClaimScope(cfg))
	}
}

func TestClaimScopeUsesInheritedKubeconfig(t *testing.T) {
	cfg := testConfig(t)
	cfg.KubeVirt.Kubeconfig = ""
	t.Setenv("KUBECONFIG", "/cluster-a")
	scopeA := kubeVirtClaimScope(cfg)
	t.Setenv("KUBECONFIG", "/cluster-b")
	scopeB := kubeVirtClaimScope(cfg)
	if scopeA == scopeB || !strings.Contains(scopeA, "kubeconfig:/cluster-a") || !strings.Contains(scopeB, "kubeconfig:/cluster-b") {
		t.Fatalf("scopeA=%q scopeB=%q", scopeA, scopeB)
	}
}

func TestAllocateLeaseSlugIgnoresOtherKubeVirtScopes(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig(t)
	otherScope := "kubeconfig:/other|context:test-context|namespace:test-ns"
	if err := core.ClaimLeaseForRepoProviderScope("cbx_other", "shared", providerName, otherScope, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: &recordingRunner{stdout: `{"items":[]}`}}}
	slug, err := backend.allocateLeaseSlug(context.Background(), "cbx_new", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if slug != "shared" {
		t.Fatalf("slug=%q, want shared when collision is outside scope", slug)
	}
	claimKubeVirtLease(t, cfg, "cbx_current", "shared", t.TempDir(), time.Minute, false)
	slug, err = backend.allocateLeaseSlug(context.Background(), "cbx_next", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if slug == "shared" || !strings.HasPrefix(slug, "shared-") {
		t.Fatalf("slug=%q, want current-scope collision suffix", slug)
	}
}

func TestAllocateLeaseSlugChecksLiveVMsForLegacyClaims(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig(t)
	if err := core.ClaimLeaseForRepoProvider("cbx_legacy", "shared", providerName, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{stdout: `{"items":[{"metadata":{"name":"vm-legacy","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_legacy","crabbox.dev/slug":"shared"}},"status":{"printableStatus":"Stopped"}}]}`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	slug, err := backend.allocateLeaseSlug(context.Background(), "cbx_new", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if slug == "shared" || !strings.HasPrefix(slug, "shared-") {
		t.Fatalf("slug=%q, want live VM collision suffix", slug)
	}
}

func TestAllocateGeneratedSlugIgnoresMalformedClaims(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	claimsDir := filepath.Join(stateDir, "crabbox", "claims")
	if err := os.MkdirAll(claimsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claimsDir, "bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	runner := &recordingRunner{stdout: `{"items":[]}`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	slug, err := backend.allocateLeaseSlug(context.Background(), "cbx_new", "")
	if err != nil {
		t.Fatal(err)
	}
	if slug == "" {
		t.Fatal("empty generated slug")
	}
}

func TestResolveIdentityIgnoresKubeVirtClaimFromOtherScope(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig(t)
	wrongScope := "kubeconfig:/other|context:test-context|namespace:test-ns"
	if err := core.ClaimLeaseForRepoProviderScope("cbx_wrong", "shared", providerName, wrongScope, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	server := core.Server{Name: "wrong-scope-vm", Labels: map[string]string{"name": "wrong-scope-vm"}}
	if err := core.UpdateLeaseClaimEndpoint("cbx_wrong", server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{stdout: `{"items":[{"metadata":{"name":"vm-current","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_current","crabbox.dev/slug":"shared"}},"status":{"printableStatus":"Stopped"}}]}`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	name, leaseID, slug, _, err := backend.resolveIdentity(context.Background(), "shared")
	if err != nil {
		t.Fatal(err)
	}
	if name != "vm-current" || leaseID != "cbx_current" || slug != "shared" {
		t.Fatalf("identity=%q %q %q", name, leaseID, slug)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "wrong-scope-vm") {
			t.Fatalf("used wrong-scope claim: calls=%#v", runner.calls)
		}
	}
}

func TestStatusIgnoresKubeVirtClaimFromOtherScope(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig(t)
	if err := core.ClaimLeaseForRepoProviderScope("cbx_wrong", "shared", providerName, "kubeconfig:/other|context:test-context|namespace:test-ns", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	server := core.Server{Name: "wrong-scope-vm", Labels: map[string]string{"name": "wrong-scope-vm"}}
	if err := core.UpdateLeaseClaimEndpoint("cbx_wrong", server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{stdout: `{"items":[{"metadata":{"name":"vm-current","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_current","crabbox.dev/slug":"shared"}},"status":{"printableStatus":"Stopped"}}]}`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	view, err := backend.Status(context.Background(), core.StatusRequest{ID: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "cbx_current" || view.Host != "vm-current" || view.Slug != "shared" {
		t.Fatalf("status=%#v", view)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "wrong-scope-vm") {
			t.Fatalf("used wrong-scope claim: calls=%#v", runner.calls)
		}
	}
}

func TestResolveIdentityExactClaimIgnoresMalformedUnrelatedClaim(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	cfg := testConfig(t)
	claimKubeVirtLease(t, cfg, "cbx_valid", "valid", t.TempDir(), time.Minute, false)
	server := core.Server{Name: "vm-valid", Labels: map[string]string{"name": "vm-valid"}}
	if err := core.UpdateLeaseClaimEndpoint("cbx_valid", server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	claimsDir := filepath.Join(stateDir, "crabbox", "claims")
	if err := os.WriteFile(filepath.Join(claimsDir, "bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{stdout: `{"metadata":{"name":"vm-valid","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_valid","crabbox.dev/slug":"valid"}}}`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	name, leaseID, slug, _, err := backend.resolveIdentity(context.Background(), "cbx_valid")
	if err != nil {
		t.Fatal(err)
	}
	if name != "vm-valid" || leaseID != "cbx_valid" || slug != "valid" {
		t.Fatalf("identity=%q %q %q", name, leaseID, slug)
	}
}

func TestResolveIdentityUsesClaimedClusterName(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig(t)
	claimKubeVirtLease(t, cfg, "cbx_cluster", "cluster", t.TempDir(), time.Minute, false)
	server := core.Server{Name: "actual-cluster-vm", Labels: map[string]string{"name": "actual-cluster-vm"}}
	if err := core.UpdateLeaseClaimEndpoint("cbx_cluster", server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{stdout: `{"metadata":{"name":"actual-cluster-vm","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_cluster","crabbox.dev/slug":"cluster"}}}`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	name, leaseID, slug, _, err := backend.resolveIdentity(context.Background(), "cluster")
	if err != nil {
		t.Fatal(err)
	}
	if name != "actual-cluster-vm" || leaseID != "cbx_cluster" || slug != "cluster" {
		t.Fatalf("identity=%q %q %q", name, leaseID, slug)
	}
}

func TestResolveIdentityRejectsStaleClaimForReusedVMName(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig(t)
	claimKubeVirtLease(t, cfg, "cbx_original", "shared", t.TempDir(), time.Minute, false)
	server := core.Server{Name: "reused-vm", Labels: map[string]string{"name": "reused-vm"}}
	if err := core.UpdateLeaseClaimEndpoint("cbx_original", server, core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{stdout: `{"metadata":{"name":"reused-vm","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_replacement","crabbox.dev/slug":"replacement"}}}`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if _, _, _, _, err := backend.resolveIdentity(context.Background(), "shared"); err == nil || !strings.Contains(err.Error(), "lease identity changed") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveIdentityUsesVMLeaseLabels(t *testing.T) {
	cfg := testConfig(t)
	runner := &recordingRunner{stdout: `{"items":[{"metadata":{"name":"vm-one","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_original","crabbox.dev/slug":"original"}},"status":{"printableStatus":"Stopped"}}]}`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	name, leaseID, slug, keep, err := backend.resolveIdentity(context.Background(), "vm-one")
	if err != nil {
		t.Fatal(err)
	}
	if name != "vm-one" || leaseID != "cbx_original" || slug != "original" {
		t.Fatalf("identity=%q %q %q", name, leaseID, slug)
	}
	if !keep {
		t.Fatal("missing keep annotation should preserve by default")
	}
}

func TestResolveIdentityFindsRequestedSlugAndLeaseID(t *testing.T) {
	cfg := testConfig(t)
	inventory := `{"items":[{"metadata":{"name":"crabbox-custom-deadbeef","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_original","crabbox.dev/slug":"custom"}},"status":{"printableStatus":"Stopped"}}]}`
	for _, identifier := range []string{"custom", "cbx_original"} {
		t.Run(identifier, func(t *testing.T) {
			runner := &recordingRunner{stdout: inventory}
			backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
			name, leaseID, slug, keep, err := backend.resolveIdentity(context.Background(), identifier)
			if err != nil {
				t.Fatal(err)
			}
			if name != "crabbox-custom-deadbeef" || leaseID != "cbx_original" || slug != "custom" {
				t.Fatalf("identity=%q %q %q", name, leaseID, slug)
			}
			if !keep {
				t.Fatal("missing keep annotation should preserve by default")
			}
			if len(runner.calls) != 1 || !strings.Contains(runner.calls[0], "-l "+managedByLabel+"=crabbox") {
				t.Fatalf("calls=%#v", runner.calls)
			}
		})
	}
}

func TestResolveIdentityDoesNotInterpretLeaseIDAsSelector(t *testing.T) {
	cfg := testConfig(t)
	inventory := `{"items":[{"metadata":{"name":"victim","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_victim","crabbox.dev/slug":"victim"}}}]}`
	runner := &recordingRunner{stdout: inventory}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	identifier := "cbx_missing,app.kubernetes.io/managed-by=crabbox"
	if _, _, _, _, err := backend.resolveIdentity(context.Background(), identifier); err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.calls) != 1 || strings.Contains(runner.calls[0], identifier) {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestResolveIdentityPrefersExactNameOverAnotherVMsSlug(t *testing.T) {
	cfg := testConfig(t)
	inventory := `{"items":[
		{"metadata":{"name":"exact-name","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_exact","crabbox.dev/slug":"exact"}}},
		{"metadata":{"name":"other-name","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_other","crabbox.dev/slug":"exact-name"}}}
	]}`
	runner := &recordingRunner{stdout: inventory}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	name, leaseID, slug, _, err := backend.resolveIdentity(context.Background(), "exact-name")
	if err != nil {
		t.Fatal(err)
	}
	if name != "exact-name" || leaseID != "cbx_exact" || slug != "exact" {
		t.Fatalf("identity=%q %q %q", name, leaseID, slug)
	}
}

func TestResolveIdentityPreservesEphemeralKeepPolicy(t *testing.T) {
	cfg := testConfig(t)
	inventory := `{"items":[{"metadata":{"name":"vm-ephemeral","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_ephemeral","crabbox.dev/slug":"ephemeral"},"annotations":{"crabbox.dev/keep":"false"}},"status":{"printableStatus":"Stopped"}}]}`
	runner := &recordingRunner{stdout: inventory}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	_, _, _, keep, err := backend.resolveIdentity(context.Background(), "ephemeral")
	if err != nil {
		t.Fatal(err)
	}
	if keep {
		t.Fatal("ephemeral VM became keep=true")
	}
}

func TestPersistedVMLabelsPreserveTTLMetadata(t *testing.T) {
	cfg := testConfig(t)
	runner := &recordingRunner{stdout: `{"metadata":{"name":"vm-ephemeral","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_ephemeral","crabbox.dev/slug":"ephemeral"},"annotations":{"crabbox.dev/keep":"false","crabbox.dev/created_at":"100","crabbox.dev/expires_at":"200","crabbox.dev/ttl_secs":"100"}}}`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	labels, err := backend.persistedVMLabels(context.Background(), "vm-ephemeral", "cbx_ephemeral", "ephemeral")
	if err != nil {
		t.Fatal(err)
	}
	if labels["keep"] != "false" || labels["created_at"] != "100" || labels["expires_at"] != "200" || labels["ttl_secs"] != "100" {
		t.Fatalf("labels=%#v", labels)
	}
	server := backend.server("vm-ephemeral", "cbx_ephemeral", "ephemeral", false)
	for key, value := range labels {
		server.Labels[key] = value
	}
	touched := core.TouchDirectLeaseLabels(server.Labels, cfg, "ready", time.Unix(150, 0).UTC())
	if touched["created_at"] != "100" || touched["expires_at"] != "200" {
		t.Fatalf("touched=%#v", touched)
	}
}

func TestResolveSSHKeyDoesNotGenerateMissingKey(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig(t)
	cfg.KubeVirt.SSHKey = ""
	backend := &leaseBackend{cfg: cfg}
	keyPath, err := core.TestboxKeyPath("cbx_missing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.resolveSSHKey("cbx_missing"); err == nil || !strings.Contains(err.Error(), "stored SSH key") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("missing key was created: %v", err)
	}
}

func TestResolveIdentityRejectsUnmanagedVM(t *testing.T) {
	cfg := testConfig(t)
	runner := &recordingRunner{stdout: `{"items":[]}`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	if _, _, _, _, err := backend.resolveIdentity(context.Background(), "database"); err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("err=%v", err)
	}
}

func TestListParsesKubeVirtInventory(t *testing.T) {
	cfg := testConfig(t)
	runner := &recordingRunner{stdout: `{"items":[{"metadata":{"name":"vm-one","creationTimestamp":"2026-06-06T00:00:00Z","labels":{"crabbox.dev/lease-id":"cbx_123","crabbox.dev/slug":"one"},"annotations":{"crabbox.dev/keep":"false","crabbox.dev/expires_at":"1"}},"status":{"printableStatus":"Running"}}]}`}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stderr: io.Discard, Exec: runner}}
	items, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "vm-one" || items[0].Status != "Running" || items[0].Labels["lease"] != "cbx_123" || items[0].Labels["keep"] != "false" {
		t.Fatalf("items=%#v", items)
	}
}

func TestCleanupDeletesExpiredVMAndHonorsDryRun(t *testing.T) {
	cfg := testConfig(t)
	inventory := `{"items":[{"metadata":{"name":"vm-old","labels":{"crabbox.dev/lease-id":"cbx_old","crabbox.dev/slug":"old"},"annotations":{"crabbox.dev/keep":"false","crabbox.dev/state":"ready","crabbox.dev/expires_at":"1"}},"status":{"printableStatus":"Stopped"}}]}`
	for _, dryRun := range []bool{true, false} {
		t.Run(map[bool]string{true: "dry-run", false: "delete"}[dryRun], func(t *testing.T) {
			item := `{"metadata":{"name":"vm-old","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_old","crabbox.dev/slug":"old"},"annotations":{"crabbox.dev/keep":"false","crabbox.dev/state":"ready","crabbox.dev/expires_at":"1"}},"status":{"printableStatus":"Stopped"}}`
			runner := &recordingRunner{stdout: inventory, outputs: []string{inventory, item}}
			var stdout strings.Builder
			backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stdout: &stdout, Stderr: io.Discard, Exec: runner}}
			if err := backend.Cleanup(context.Background(), core.CleanupRequest{DryRun: dryRun}); err != nil {
				t.Fatal(err)
			}
			if dryRun && len(runner.calls) != 1 {
				t.Fatalf("dry-run calls=%#v", runner.calls)
			}
			if !dryRun && (len(runner.calls) != 3 || !strings.Contains(runner.calls[2], "delete virtualmachine.kubevirt.io/vm-old")) {
				t.Fatalf("delete calls=%#v", runner.calls)
			}
		})
	}
}

func TestCleanupDoesNotRemoveWrongScopeClaim(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig(t)
	if err := core.ClaimLeaseForRepoProviderScope("cbx_old", "old", providerName, "kubeconfig:/other|context:test-context|namespace:test-ns", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	inventory := `{"items":[{"metadata":{"name":"vm-old","labels":{"crabbox.dev/lease-id":"cbx_old","crabbox.dev/slug":"old"},"annotations":{"crabbox.dev/keep":"false","crabbox.dev/state":"ready","crabbox.dev/expires_at":"1"}},"status":{"printableStatus":"Stopped"}}]}`
	item := `{"metadata":{"name":"vm-old","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_old","crabbox.dev/slug":"old"},"annotations":{"crabbox.dev/keep":"false","crabbox.dev/state":"ready","crabbox.dev/expires_at":"1"}},"status":{"printableStatus":"Stopped"}}`
	runner := &recordingRunner{stdout: inventory, outputs: []string{inventory, item}}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim("cbx_old")
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID == "" {
		t.Fatal("wrong-scope claim was removed")
	}
}

func TestCleanupRemovesLegacyUnscopedClaim(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig(t)
	if err := core.ClaimLeaseForRepoProvider("cbx_old", "old", providerName, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	inventory := `{"items":[{"metadata":{"name":"vm-old","labels":{"crabbox.dev/lease-id":"cbx_old","crabbox.dev/slug":"old"},"annotations":{"crabbox.dev/keep":"false","crabbox.dev/state":"ready","crabbox.dev/expires_at":"1"}},"status":{"printableStatus":"Stopped"}}]}`
	item := `{"metadata":{"name":"vm-old","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/lease-id":"cbx_old","crabbox.dev/slug":"old"},"annotations":{"crabbox.dev/keep":"false","crabbox.dev/state":"ready","crabbox.dev/expires_at":"1"}},"status":{"printableStatus":"Stopped"}}`
	runner := &recordingRunner{stdout: inventory, outputs: []string{inventory, item}}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim("cbx_old")
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID != "" {
		t.Fatalf("legacy claim remains: %#v", claim)
	}
}

type recordingRunner struct {
	calls      []string
	stdout     string
	outputs    []string
	manifest   string
	failAt     int
	failStderr string
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req.Name+" "+strings.Join(req.Args, " "))
	stdout := r.stdout
	if index := len(r.calls) - 1; index < len(r.outputs) {
		stdout = r.outputs[index]
	}
	for i, arg := range req.Args {
		if arg == "-f" && i+1 < len(req.Args) {
			data, _ := os.ReadFile(req.Args[i+1])
			r.manifest = string(data)
		}
	}
	if r.failAt > 0 && len(r.calls) == r.failAt {
		stderr := r.failStderr
		if stderr == "" {
			stderr = "fixture failure"
		}
		return core.LocalCommandResult{ExitCode: 1, Stderr: stderr}, io.ErrUnexpectedEOF
	}
	return core.LocalCommandResult{Stdout: stdout}, nil
}
