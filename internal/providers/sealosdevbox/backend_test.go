package sealosdevbox

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"gopkg.in/yaml.v3"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func noopSSHReady(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
	return nil
}

type lifecycleRunner struct {
	requests []core.LocalCommandRequest
	inputs   []string
	outputs  []string
	stderrs  []string
	exitCode []int
	errors   []error
}

func (r *lifecycleRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.requests = append(r.requests, req)
	if req.Stdin != nil {
		data, _ := io.ReadAll(req.Stdin)
		r.inputs = append(r.inputs, string(data))
	} else {
		r.inputs = append(r.inputs, "")
	}
	index := len(r.requests) - 1
	result := core.LocalCommandResult{}
	if index < len(r.outputs) {
		result.Stdout = r.outputs[index]
	}
	if index < len(r.stderrs) {
		result.Stderr = r.stderrs[index]
	}
	if index < len(r.exitCode) {
		result.ExitCode = r.exitCode[index]
	}
	if index < len(r.errors) && r.errors[index] != nil {
		return result, r.errors[index]
	}
	if result.Stdout == "" {
		if isCanIRequest(req) {
			result.Stdout = "yes"
		} else {
			result.Stdout = "ok"
		}
	}
	return result, nil
}

func lifecycleConfig() core.Config {
	cfg := testConfig()
	cfg.SealosDevbox.Image = "ubuntu:24.04"
	cfg.SealosDevbox.TemplateID = "tpl-devbox"
	cfg.SealosDevbox.CPU = "4"
	cfg.SealosDevbox.Memory = "8Gi"
	cfg.SealosDevbox.StorageLimit = "40Gi"
	cfg.IdleTimeout = time.Hour
	cfg.TTL = 2 * time.Hour
	cfg.TargetOS = core.TargetLinux
	return cfg
}

func lifecycleBackend(cfg core.Config, runner *lifecycleRunner) *backend {
	return &backend{
		spec: (Provider{}).Spec(),
		cfg:  cfg,
		rt: core.Runtime{
			Stdout: io.Discard,
			Stderr: io.Discard,
			Exec:   runner,
			Clock:  fixedClock{t: time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)},
		},
		sshReady: noopSSHReady,
	}
}

func isolateSealosState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("HOME", dir)
}

func TestRenderDevboxManifestStructured(t *testing.T) {
	cfg := lifecycleConfig()
	backend := lifecycleBackend(cfg, &lifecycleRunner{})
	manifest, err := backend.renderDevboxManifest("crabbox-blue", "cbx_123456abcdef", "blue", false, backend.now())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(manifest, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["apiVersion"] != devboxGroupVersion || doc["kind"] != devboxKind {
		t.Fatalf("manifest identity=%#v", doc)
	}
	metadata := doc["metadata"].(map[string]any)
	labels := metadata["labels"].(map[string]any)
	if labels[managedByLabel] != "crabbox" || labels[providerLabel] != providerName || labels[leaseIDLabel] != "cbx_123456abcdef" || labels[slugLabel] != "blue" {
		t.Fatalf("labels=%#v", labels)
	}
	annotations := metadata["annotations"].(map[string]any)
	for _, key := range []string{"provider", "lease", "slug", "devbox_namespace", "devbox_name", "network", "provider_scope", "ttl_secs", "idle_timeout_secs"} {
		if annotations[annotationBase+key] == "" {
			t.Fatalf("annotation %s missing in %#v", key, annotations)
		}
	}
	spec := doc["spec"].(map[string]any)
	if spec["state"] != "Running" || spec["image"] != "ubuntu:24.04" || spec["templateID"] != "tpl-devbox" || spec["storageLimit"] != "40Gi" || spec["workdir"] != "/home/devbox/project" {
		t.Fatalf("spec=%#v", spec)
	}
	resource := spec["resource"].(map[string]any)
	if resource["cpu"] != "4" || resource["memory"] != "8Gi" {
		t.Fatalf("resource=%#v", resource)
	}
	network := spec["network"].(map[string]any)
	if network["type"] != networkSSHGate {
		t.Fatalf("network=%#v", network)
	}
	config := spec["config"].(map[string]any)
	if config["user"] != "devbox" || config["workingDir"] != "/home/devbox/project" {
		t.Fatalf("config=%#v", config)
	}
}

func TestClaimScopeSeparatesSameSlugAcrossRoutes(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	other := cfg
	other.SealosDevbox.SSHGatewayHost = "other-gateway.example.test"
	if err := core.ClaimLeaseForRepoProviderScope("cbx_aaaaaaaaaaaa", "shared", providerName, sealosClaimScope(other), t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	runner := &lifecycleRunner{outputs: []string{`{"items":[]}`}}
	backend := lifecycleBackend(cfg, runner)
	slug, err := backend.allocateLeaseSlug(context.Background(), "cbx_bbbbbbbbbbbb", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if slug != "shared" {
		t.Fatalf("slug=%q; same slug in other scope should not collide", slug)
	}
	if err := core.ClaimLeaseForRepoProviderScope("cbx_cccccccccccc", "shared", providerName, sealosClaimScope(cfg), t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	runner.outputs = []string{`{"items":[]}`}
	runner.requests = nil
	slug, err = backend.allocateLeaseSlug(context.Background(), "cbx_dddddddddddd", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if slug == "shared" || !strings.HasPrefix(slug, "shared-") {
		t.Fatalf("slug=%q; same scope claim should collide", slug)
	}
}

func TestParseAndPersistDevboxSecretKeysRedactsMaterial(t *testing.T) {
	isolateSealosState(t)
	privateKey := "-----BEGIN OPENSSH PRIVATE KEY-----\nsecret-private\n-----END OPENSSH PRIVATE KEY-----\n"
	publicKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest public"
	secret := devboxSecret{Data: map[string]string{
		devboxPublicKeyField:  base64.StdEncoding.EncodeToString([]byte(publicKey)),
		devboxPrivateKeyField: base64.StdEncoding.EncodeToString([]byte(privateKey)),
	}}
	keys, err := parseDevboxSecretKeys(secret)
	if err != nil {
		t.Fatal(err)
	}
	keyPath, err := persistDevboxKey("cbx_123456abcdef", keys)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != privateKey {
		t.Fatal("private key did not round trip")
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode=%04o want 0600", info.Mode().Perm())
	}
	redacted := redactSensitive("private_key=" + strings.TrimSpace(privateKey))
	if strings.Contains(redacted, "secret-private") {
		t.Fatalf("redaction leaked private key: %s", redacted)
	}
	if _, err := os.Stat(keyPath + ".pub"); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireAppliesManifestPersistsClaimAndKey(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_123456abcdef"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	privateKey := "-----BEGIN OPENSSH PRIVATE KEY-----\nsecret-private\n-----END OPENSSH PRIVATE KEY-----\n"
	publicKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest public"
	devboxJSON := `{"metadata":{"name":"` + name + `","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"blue"},"annotations":{"crabbox.dev/provider_scope":"` + sealosClaimScope(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running","ssh":{"secretName":"` + name + `-ssh"}}}`
	secretJSON := `{"metadata":{"name":"` + name + `-ssh"},"data":{"` + devboxPublicKeyField + `":"` + base64.StdEncoding.EncodeToString([]byte(publicKey)) + `","` + devboxPrivateKeyField + `":"` + base64.StdEncoding.EncodeToString([]byte(privateKey)) + `"}}`
	runner := &lifecycleRunner{outputs: []string{
		`{"items":[]}`,
		`devbox applied`,
		devboxJSON,
		secretJSON,
	}}
	backend := lifecycleBackend(cfg, runner)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedLeaseID: leaseID, RequestedSlug: slug, Repo: core.Repo{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.Name != name || lease.SSH.Host != "ssh.sealos.example.test" || lease.SSH.Port != "2222" || lease.SSH.Key == "" {
		t.Fatalf("lease=%#v", lease)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != providerName || claim.ProviderScope != sealosClaimScope(cfg) || claim.CloudID != "team-a/"+name || claim.Labels["devbox_name"] != name {
		t.Fatalf("claim=%#v", claim)
	}
	if len(runner.inputs) < 2 || !strings.Contains(runner.inputs[1], "apiVersion: "+devboxGroupVersion) {
		t.Fatalf("apply stdin missing manifest: %#v", runner.inputs)
	}
	if strings.Contains(strings.Join(flattenArgs(runner.requests), " "), "secret-private") {
		t.Fatal("private key leaked into kubectl args")
	}
}

func TestAcquireOnAcquiredErrorRollsBackBeforeLocalState(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_ackrollback"
	slug := "reject"
	name := core.LeaseProviderName(leaseID, slug)
	devboxJSON := `{"metadata":{"name":"` + name + `","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider_scope":"` + sealosClaimScope(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running","ssh":{"secretName":"` + name + `-ssh"}}}`
	runner := &lifecycleRunner{outputs: []string{
		`{"items":[]}`,
		`devbox applied`,
		devboxJSON,
		"deleted",
	}}
	backend := lifecycleBackend(cfg, runner)
	called := false
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{
		RequestedLeaseID: leaseID,
		RequestedSlug:    slug,
		Repo:             core.Repo{Root: t.TempDir()},
		OnAcquired: func(acquired core.LeaseTarget) error {
			called = true
			if acquired.LeaseID != leaseID || acquired.Server.CloudID != "team-a/"+name || acquired.SSH.Host != "ssh.sealos.example.test" || acquired.SSH.Key == "" {
				t.Fatalf("acquired=%#v", acquired)
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
				t.Fatalf("claim exists during OnAcquired=%v err=%v", exists, err)
			}
			target := core.SSHTarget{}
			core.UseStoredTestboxKey(&target, leaseID)
			if target.Key != "" {
				t.Fatalf("stored key exists during OnAcquired: %s", target.Key)
			}
			return errors.New("controller rejected identity")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "controller rejected identity") {
		t.Fatalf("Acquire error=%v", err)
	}
	if !called {
		t.Fatal("OnAcquired was not called")
	}
	got := strings.Join(flattenArgs(runner.requests), " ")
	if !strings.Contains(got, "delete "+devboxResource+"/"+name+" --ignore-not-found=true") {
		t.Fatalf("failed acquire did not delete devbox; commands=%s", got)
	}
	if strings.Contains(got, "secret/"+name+"-ssh") {
		t.Fatalf("acquire read secret after rejected identity: %s", got)
	}
}

func TestAcquireOnAcquiredErrorRollsBackKeptDevbox(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_ackkeepfail"
	slug := "rejectkeep"
	name := core.LeaseProviderName(leaseID, slug)
	devboxJSON := `{"metadata":{"name":"` + name + `","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider_scope":"` + sealosClaimScope(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running","ssh":{"secretName":"` + name + `-ssh"}}}`
	runner := &lifecycleRunner{outputs: []string{
		`{"items":[]}`,
		`devbox applied`,
		devboxJSON,
		"deleted",
	}}
	backend := lifecycleBackend(cfg, runner)
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{
		RequestedLeaseID: leaseID,
		RequestedSlug:    slug,
		Repo:             core.Repo{Root: t.TempDir()},
		Keep:             true,
		OnAcquired: func(core.LeaseTarget) error {
			return errors.New("controller rejected kept identity")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "controller rejected kept identity") {
		t.Fatalf("Acquire error=%v", err)
	}
	got := strings.Join(flattenArgs(runner.requests), " ")
	if !strings.Contains(got, "delete "+devboxResource+"/"+name+" --ignore-not-found=true") {
		t.Fatalf("kept rejected acquire did not delete devbox; commands=%s", got)
	}
}

func TestAcquireRollsBackUnkeptDevboxAfterSSHReadinessFailure(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_rollback1234"
	slug := "failssh"
	name := core.LeaseProviderName(leaseID, slug)
	privateKey := "-----BEGIN OPENSSH PRIVATE KEY-----\nsecret-private\n-----END OPENSSH PRIVATE KEY-----\n"
	publicKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest public"
	devboxJSON := `{"metadata":{"name":"` + name + `","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider_scope":"` + sealosClaimScope(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running","ssh":{"secretName":"` + name + `-ssh"}}}`
	secretJSON := `{"metadata":{"name":"` + name + `-ssh"},"data":{"` + devboxPublicKeyField + `":"` + base64.StdEncoding.EncodeToString([]byte(publicKey)) + `","` + devboxPrivateKeyField + `":"` + base64.StdEncoding.EncodeToString([]byte(privateKey)) + `"}}`
	runner := &lifecycleRunner{outputs: []string{
		`{"items":[]}`,
		`devbox applied`,
		devboxJSON,
		secretJSON,
		`{"items":[]}`,
		"deleted",
	}}
	backend := lifecycleBackend(cfg, runner)
	backend.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return errors.New("ssh not ready")
	}
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedLeaseID: leaseID, RequestedSlug: slug, Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "ssh not ready") {
		t.Fatalf("Acquire error=%v", err)
	}
	got := strings.Join(flattenArgs(runner.requests), " ")
	if !strings.Contains(got, "delete "+devboxResource+"/"+name+" --ignore-not-found=true") {
		t.Fatalf("failed acquire did not delete devbox; commands=%s", got)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v after rollback", exists, err)
	}
	target := core.SSHTarget{}
	core.UseStoredTestboxKey(&target, leaseID)
	if target.Key != "" {
		t.Fatalf("stored key still exists after rollback: %s", target.Key)
	}
}

func TestDoctorBlocksWhenImageAndTemplateAreMissing(t *testing.T) {
	cfg := lifecycleConfig()
	cfg.SealosDevbox.Image = ""
	cfg.SealosDevbox.TemplateID = ""
	runner := &lifecycleRunner{}
	backend := lifecycleBackend(cfg, runner)
	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "blocked" {
		t.Fatalf("status=%q want blocked; checks=%#v", result.Status, result.Checks)
	}
	found := false
	for _, check := range result.Checks {
		if check.Check == "devbox.source" {
			found = true
			if check.Status != "failed" || !strings.Contains(check.Message, "requires image or templateID") {
				t.Fatalf("devbox.source check=%#v", check)
			}
		}
	}
	if !found {
		t.Fatalf("missing devbox.source check: %#v", result.Checks)
	}
}

func TestListAndStatusAreReadOnly(t *testing.T) {
	cfg := lifecycleConfig()
	item := `{"items":[{"metadata":{"name":"devbox-one","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"cbx_111111111111","crabbox.dev/slug":"blue"},"annotations":{"crabbox.dev/provider_scope":"` + sealosClaimScope(cfg) + `","crabbox.dev/devbox_name":"devbox-one","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}]}`
	runner := &lifecycleRunner{outputs: []string{item, item}}
	backend := lifecycleBackend(cfg, runner)
	views, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Name != "devbox-one" || views[0].Status != "Running" {
		t.Fatalf("views=%#v", views)
	}
	status, err := backend.Status(context.Background(), core.StatusRequest{ID: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	if status.ID != "cbx_111111111111" || status.State != "Running" || !status.Ready || status.SSHHost != "ssh.sealos.example.test" {
		t.Fatalf("status=%#v", status)
	}
	for _, req := range runner.requests {
		args := strings.Join(req.Args, " ")
		for _, verb := range []string{" apply ", " patch ", " delete ", " create ", " replace "} {
			if strings.Contains(" "+args+" ", verb) {
				t.Fatalf("read path used mutating kubectl command: %s", args)
			}
		}
		if strings.Contains(args, "secret/") {
			t.Fatalf("status/list read Secret data: %s", args)
		}
	}
}

func TestStatusWaitRequiresSSHReadiness(t *testing.T) {
	cfg := lifecycleConfig()
	item := `{"items":[{"metadata":{"name":"devbox-one","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"cbx_111111111111","crabbox.dev/slug":"blue"},"annotations":{"crabbox.dev/provider_scope":"` + sealosClaimScope(cfg) + `","crabbox.dev/devbox_name":"devbox-one","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}]}`
	runner := &lifecycleRunner{outputs: []string{item}}
	backend := lifecycleBackend(cfg, runner)
	probed := false
	backend.sshReady = func(_ context.Context, target *core.SSHTarget, _ io.Writer, phase string, _ time.Duration) error {
		probed = true
		if target.Host != "ssh.sealos.example.test" || target.User != cfg.SealosDevbox.SSHUser || phase != "Sealos DevBox status" {
			t.Fatalf("unexpected SSH probe target=%#v phase=%q", target, phase)
		}
		return errors.New("ssh not ready")
	}
	_, err := backend.Status(context.Background(), core.StatusRequest{ID: "blue", Wait: true, WaitTimeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "ssh not ready") {
		t.Fatalf("Status error=%v", err)
	}
	if !probed {
		t.Fatal("status --wait did not probe SSH readiness")
	}
}

func flattenArgs(requests []core.LocalCommandRequest) []string {
	out := []string{}
	for _, req := range requests {
		out = append(out, req.Args...)
	}
	return out
}

func isCanIRequest(req core.LocalCommandRequest) bool {
	for i := 0; i+1 < len(req.Args); i++ {
		if req.Args[i] == "auth" && req.Args[i+1] == "can-i" {
			return true
		}
	}
	return false
}

func TestPersistDevboxKeyUsesCrabboxKeyPath(t *testing.T) {
	isolateSealosState(t)
	keys := devboxSecretKeys{PublicKey: "ssh-ed25519 AAA test", PrivateKey: "private\n"}
	path, err := persistDevboxKey("cbx_abcdef123456", keys)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, filepath.Join("crabbox", "testboxes", "cbx_abcdef123456", "id_ed25519")) {
		t.Fatalf("path=%q", path)
	}
}
