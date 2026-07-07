package sealosdevbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
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

func ownedDevboxSecretJSON(name, namespace, uid, publicKey, privateKey string, encoded bool) string {
	secret := devboxSecret{
		Metadata: devboxMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []ownerReference{{
				APIVersion: devboxGroupVersion,
				Kind:       devboxKind,
				Name:       name,
				UID:        uid,
				Controller: true,
			}},
		},
	}
	if encoded {
		secret.Data = map[string]string{
			devboxPublicKeyField:  base64.StdEncoding.EncodeToString([]byte(publicKey)),
			devboxPrivateKeyField: base64.StdEncoding.EncodeToString([]byte(privateKey)),
		}
	} else {
		secret.StringData = map[string]string{
			devboxPublicKeyField:  publicKey,
			devboxPrivateKeyField: privateKey,
		}
	}
	data, err := json.Marshal(secret)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func withDevboxResourceVersion(value string) string {
	return strings.Replace(value, `"uid":"uid-test",`, `"uid":"uid-test","resourceVersion":"rv-test",`, 1)
}

func withDevboxUID(value string) string {
	return strings.Replace(value, `"namespace":"team-a",`, `"namespace":"team-a","uid":"uid-test",`, 1)
}

func assertPreconditionedDevboxDelete(t *testing.T, cfg core.Config, runner *lifecycleRunner, name string) {
	t.Helper()
	wantPath := "/apis/devbox.sealos.io/v1alpha2/namespaces/" + cfg.SealosDevbox.Namespace + "/devboxes/" + name
	for index, req := range runner.requests {
		if !strings.Contains(commandString(req), "delete --raw "+wantPath+" -f -") {
			continue
		}
		var payload struct {
			Preconditions map[string]string `json:"preconditions"`
		}
		if err := json.Unmarshal([]byte(runner.inputs[index]), &payload); err != nil {
			t.Fatalf("delete preconditions are not valid JSON: %v", err)
		}
		if payload.Preconditions["uid"] != "uid-test" || payload.Preconditions["resourceVersion"] != "rv-test" {
			t.Fatalf("delete preconditions=%#v", payload.Preconditions)
		}
		return
	}
	t.Fatalf("no UID/resourceVersion-bound delete for %s in %#v", name, flattenArgs(runner.requests))
}

type lifecycleRunner struct {
	requests []core.LocalCommandRequest
	inputs   []string
	outputs  []string
	stderrs  []string
	exitCode []int
	errors   []error
}

type hookLifecycleRunner struct {
	inner  *lifecycleRunner
	before func(core.LocalCommandRequest)
}

func (r *hookLifecycleRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if r.before != nil {
		r.before(req)
	}
	return r.inner.Run(ctx, req)
}

type stderrStreamingRunner struct {
	message string
}

func (r stderrStreamingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	_, _ = io.WriteString(req.Stderr, r.message)
	return core.LocalCommandResult{ExitCode: 1, Stderr: r.message}, errors.New("exit status 1")
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
		if isRulesReviewRequest(req) {
			result.Stdout = `{"status":{"resourceRules":[{"verbs":["*"],"apiGroups":["*"],"resources":["*"]}]}}`
		} else if isDevboxDiscoveryRequest(req) {
			result.Stdout = `{"groupVersion":"devbox.sealos.io/v1alpha2","resources":[{"name":"devboxes"}]}`
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
		sshRun:   func(context.Context, core.SSHTarget, string) error { return nil },
	}
}

func isolateSealosState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("HOME", dir)
}

func claimExactSealosTarget(t *testing.T, cfg core.Config, leaseID, slug, name, repoRoot string, target core.SSHTarget) core.Server {
	t.Helper()
	backend := lifecycleBackend(cfg, &lifecycleRunner{})
	server := releaseServer(cfg, leaseID, slug, name)
	previous, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.claimExactTarget(leaseID, slug, repoRoot, server, target, cfg.IdleTimeout, false, previous, exists); err != nil {
		t.Fatal(err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists {
		t.Fatalf("claim=%#v exists=%v err=%v", claim, exists, err)
	}
	core.SetServerLeaseClaimSnapshot(&server, claim, true)
	return server
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
	for _, key := range []string{"provider", "lease", "slug", "devbox_namespace", "devbox_name", "network", "provider-scope", "provider_scope_id", "ttl_secs", "idle_timeout_secs"} {
		if annotations[annotationBase+key] == "" {
			t.Fatalf("annotation %s missing in %#v", key, annotations)
		}
	}
	if annotations[annotationBase+"provider_scope"] != nil {
		t.Fatalf("raw provider scope annotation leaked in %#v", annotations)
	}
	for _, key := range []string{"gateway_host", "gateway_port", "node_host"} {
		if annotations[annotationBase+key] != nil {
			t.Fatalf("raw route annotation %s leaked in %#v", key, annotations)
		}
	}
	if annotations[annotationBase+"provider-scope"] != sealosClaimScopeID(cfg) || annotations[annotationBase+"provider-scope"] == sealosClaimScope(cfg) {
		t.Fatalf("provider scope annotation=%#v", annotations[annotationBase+"provider-scope"])
	}
	spec := doc["spec"].(map[string]any)
	if spec["state"] != "Running" || spec["image"] != "ubuntu:24.04" || spec["templateID"] != "tpl-devbox" || spec["storageLimit"] != "40Gi" {
		t.Fatalf("spec=%#v", spec)
	}
	resource := spec["resource"].(map[string]any)
	if resource["cpu"] != "4" || resource["memory"] != "8Gi" || resource["ephemeral-storage"] != "40Gi" {
		t.Fatalf("resource=%#v", resource)
	}
	if spec["runtimeClassName"] != devboxRuntimeClass || spec["mergeBaseImageTopLayer"] != true {
		t.Fatalf("runtime spec=%#v", spec)
	}
	tolerations := spec["tolerations"].([]any)
	toleration := tolerations[0].(map[string]any)
	if toleration["key"] != devboxSchedulingNodeKey || toleration["operator"] != "Exists" || toleration["effect"] != "NoSchedule" {
		t.Fatalf("tolerations=%#v", tolerations)
	}
	affinity := spec["affinity"].(map[string]any)
	nodeAffinity := affinity["nodeAffinity"].(map[string]any)
	required := nodeAffinity["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)
	terms := required["nodeSelectorTerms"].([]any)
	expressions := terms[0].(map[string]any)["matchExpressions"].([]any)
	expression := expressions[0].(map[string]any)
	if expression["key"] != devboxSchedulingNodeKey || expression["operator"] != "Exists" {
		t.Fatalf("affinity=%#v", affinity)
	}
	network := spec["network"].(map[string]any)
	if network["type"] != networkSSHGate {
		t.Fatalf("network=%#v", network)
	}
	config := spec["config"].(map[string]any)
	if config["user"] != "devbox" || config["workingDir"] != "/home/devbox/project" {
		t.Fatalf("config=%#v", config)
	}
	ports := config["ports"].([]any)
	port := ports[0].(map[string]any)
	if port["name"] != devboxSSHPortName || port["containerPort"] != 22 || port["protocol"] != "TCP" {
		t.Fatalf("ports=%#v", ports)
	}
}

func TestRenderDevboxManifestRequiresImage(t *testing.T) {
	cfg := lifecycleConfig()
	cfg.SealosDevbox.Image = ""
	cfg.SealosDevbox.TemplateID = "tpl-devbox"
	backend := lifecycleBackend(cfg, &lifecycleRunner{})
	_, err := backend.renderDevboxManifest("crabbox-blue", "cbx_123456abcdef", "blue", false, backend.now())
	if err == nil || !strings.Contains(err.Error(), "requires image") {
		t.Fatalf("render error=%v", err)
	}
}

func TestListDecodesContainerStatusObject(t *testing.T) {
	cfg := lifecycleConfig()
	leaseID := "cbx_statusobject"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	item := `{"items":[{"metadata":{"name":"` + name + `","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running","lastContainerStatus":{"name":"devbox","ready":true,"restartCount":0}}}]}`
	backend := lifecycleBackend(cfg, &lifecycleRunner{outputs: []string{item}})
	leases, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Labels["lease"] != leaseID {
		t.Fatalf("leases=%#v", leases)
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

func TestKubectlCapturesStderrUntilRedacted(t *testing.T) {
	const secret = "super-secret-token"
	var stderr strings.Builder
	backend := lifecycleBackend(lifecycleConfig(), &lifecycleRunner{})
	backend.rt.Exec = stderrStreamingRunner{message: "token=" + secret}
	backend.rt.Stderr = &stderr

	_, err := backend.kubectl(context.Background(), nil, true, "get", devboxResource)
	if err == nil {
		t.Fatal("expected kubectl failure")
	}
	if strings.Contains(stderr.String(), secret) {
		t.Fatalf("runtime stderr leaked kubectl diagnostic: %s", stderr.String())
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("returned error leaked kubectl diagnostic: %s", err)
	}
	if !strings.Contains(err.Error(), "token=[redacted]") {
		t.Fatalf("returned error missing redacted diagnostic: %s", err)
	}
}

func TestAcquireCreatesManifestPersistsClaimAndKey(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_123456abcdef"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	privateKey := "-----BEGIN OPENSSH PRIVATE KEY-----\nsecret-private\n-----END OPENSSH PRIVATE KEY-----\n"
	publicKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest public"
	devboxJSON := `{"metadata":{"name":"` + name + `","namespace":"team-a","uid":"uid-test","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"blue"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running"}}`
	secretJSON := ownedDevboxSecretJSON(name, "team-a", "uid-test", publicKey, privateKey, true)
	runner := &lifecycleRunner{outputs: []string{
		`{"items":[]}`,
		`devbox created`,
		devboxJSON,
		secretJSON,
	}}
	backend := lifecycleBackend(cfg, runner)
	backend.sshReady = func(_ context.Context, _ *core.SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
		if err != nil {
			t.Fatal(err)
		}
		if !exists || claim.CloudID != devboxCloudID("team-a", name) {
			t.Fatalf("acquire did not persist exact resource binding before readiness: %#v", claim)
		}
		if claim.SSHHost != "" || claim.SSHPort != 0 {
			t.Fatalf("acquire published endpoint before readiness: %#v", claim)
		}
		return nil
	}
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedLeaseID: leaseID, RequestedSlug: slug, Repo: core.Repo{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.Name != name || lease.SSH.Host != "ssh.sealos.example.test" || lease.SSH.Port != "2233" || lease.SSH.Key == "" {
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
		t.Fatalf("create stdin missing manifest: %#v", runner.inputs)
	}
	if strings.Contains(strings.Join(flattenArgs(runner.requests), " "), "secret-private") {
		t.Fatal("private key leaked into kubectl args")
	}
}

func TestAcquireRefusesExistingDevboxCollisionWithoutMutation(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_collision123"
	slug := "occupied"
	name := core.LeaseProviderName(leaseID, slug)
	runner := &lifecycleRunner{
		outputs:  []string{`{"items":[]}`, ""},
		stderrs:  []string{"", `Error from server (AlreadyExists): devboxes.devbox.sealos.io "` + name + `" already exists`},
		exitCode: []int{0, 1},
		errors:   []error{nil, errors.New("exit status 1")},
	}
	backend := lifecycleBackend(cfg, runner)

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedLeaseID: leaseID, RequestedSlug: slug, Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite existing Sealos DevBox") {
		t.Fatalf("Acquire error=%v", err)
	}
	if len(runner.requests) != 2 {
		t.Fatalf("collision issued follow-up mutation: %#v", runner.requests)
	}
	got := strings.Join(flattenArgs(runner.requests), " ")
	if !strings.Contains(got, "create -f -") || strings.Contains(got, " delete ") || strings.Contains(got, " patch ") {
		t.Fatalf("collision commands=%s", got)
	}
}

func TestAcquireRejectsExistingClaimBeforeProviderMutation(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_existingclaim"
	slug := "occupied"
	name := core.LeaseProviderName(leaseID, slug)
	claimExactSealosTarget(t, cfg, leaseID, slug, name, t.TempDir(), core.SSHTarget{})
	runner := &lifecycleRunner{}
	backend := lifecycleBackend(cfg, runner)

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedLeaseID: leaseID, RequestedSlug: slug, Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "already has a local claim") {
		t.Fatalf("Acquire error=%v", err)
	}
	if len(runner.requests) != 0 {
		t.Fatalf("existing claim reached provider: %#v", runner.requests)
	}
}

func TestAcquireReconcilesAmbiguousCreateWithExactRemoteIdentity(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_ambiguous123"
	slug := "ambiguous"
	name := core.LeaseProviderName(leaseID, slug)
	privateKey := "private\n"
	item := `{"metadata":{"name":"` + name + `","namespace":"team-a","uid":"uid-test","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running"}}`
	secret := ownedDevboxSecretJSON(name, "team-a", "uid-test", "ssh-ed25519 AAA test", privateKey, true)
	runner := &lifecycleRunner{
		outputs:  []string{`{"items":[]}`, "", item, item, secret},
		stderrs:  []string{"", "connection reset after request"},
		exitCode: []int{0, 1},
		errors:   []error{nil, io.ErrUnexpectedEOF},
	}
	backend := lifecycleBackend(cfg, runner)

	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedLeaseID: leaseID, RequestedSlug: slug, Repo: core.Repo{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.Name != name {
		t.Fatalf("lease=%#v", lease)
	}
	got := strings.Join(flattenArgs(runner.requests), " ")
	if strings.Count(got, "create -f -") != 1 || strings.Contains(got, " delete ") {
		t.Fatalf("ambiguous create commands=%s", got)
	}
}

func TestAcquireOnAcquiredErrorRollsBackBeforeLocalState(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_ackrollback"
	slug := "reject"
	name := core.LeaseProviderName(leaseID, slug)
	devboxJSON := `{"metadata":{"name":"` + name + `","namespace":"team-a","uid":"uid-test","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running"}}`
	devboxJSON = withDevboxResourceVersion(devboxJSON)
	runner := &lifecycleRunner{outputs: []string{
		`{"items":[]}`,
		`devbox created`,
		devboxJSON,
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
	assertPreconditionedDevboxDelete(t, cfg, runner, name)
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
	devboxJSON := `{"metadata":{"name":"` + name + `","namespace":"team-a","uid":"uid-test","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running"}}`
	devboxJSON = withDevboxResourceVersion(devboxJSON)
	runner := &lifecycleRunner{outputs: []string{
		`{"items":[]}`,
		`devbox created`,
		devboxJSON,
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
	assertPreconditionedDevboxDelete(t, cfg, runner, name)
}

func TestAcquireRollbackPreservesConcurrentlyAdoptedDevbox(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_concurrentadopt"
	slug := "adopted"
	name := core.LeaseProviderName(leaseID, slug)
	devboxJSON := `{"metadata":{"name":"` + name + `","namespace":"team-a","uid":"uid-test","resourceVersion":"rv-test","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `"}},"status":{"state":"Running","phase":"Running"}}`
	runner := &lifecycleRunner{outputs: []string{`{"items":[]}`, "created", devboxJSON}}
	backend := lifecycleBackend(cfg, runner)

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{
		RequestedLeaseID: leaseID,
		RequestedSlug:    slug,
		Repo:             core.Repo{Root: t.TempDir()},
		OnAcquired: func(acquired core.LeaseTarget) error {
			claimExactSealosTarget(t, cfg, leaseID, slug, name, t.TempDir(), acquired.SSH)
			return errors.New("fail after concurrent adoption")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "fail after concurrent adoption") {
		t.Fatalf("Acquire error=%v", err)
	}
	if got := strings.Join(flattenArgs(runner.requests), " "); strings.Contains(got, " delete ") {
		t.Fatalf("rollback deleted concurrently adopted DevBox: %s", got)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || claim.CloudID != devboxCloudID("team-a", name) {
		t.Fatalf("concurrent claim exists=%v err=%v claim=%#v", exists, err, claim)
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
	devboxJSON := `{"metadata":{"name":"` + name + `","namespace":"team-a","uid":"uid-test","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running"}}`
	devboxJSON = withDevboxResourceVersion(devboxJSON)
	secretJSON := ownedDevboxSecretJSON(name, "team-a", "uid-test", publicKey, privateKey, true)
	runner := &lifecycleRunner{outputs: []string{
		`{"items":[]}`,
		`devbox created`,
		devboxJSON,
		secretJSON,
		`{"items":[]}`,
		devboxJSON,
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
	assertPreconditionedDevboxDelete(t, cfg, runner, name)
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v after rollback", exists, err)
	}
	target := core.SSHTarget{}
	core.UseStoredTestboxKey(&target, leaseID)
	if target.Key != "" {
		t.Fatalf("stored key still exists after rollback: %s", target.Key)
	}
}

func TestDoctorBlocksWhenImageIsMissing(t *testing.T) {
	cfg := lifecycleConfig()
	cfg.SealosDevbox.Image = ""
	cfg.SealosDevbox.TemplateID = "tpl-devbox"
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
			if check.Status != "failed" || !strings.Contains(check.Message, "requires image") {
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
	item := `{"items":[{"metadata":{"name":"devbox-one","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"cbx_111111111111","crabbox.dev/slug":"blue"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"devbox-one","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}]}`
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
	item := `{"items":[{"metadata":{"name":"devbox-one","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"cbx_111111111111","crabbox.dev/slug":"blue"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"devbox-one","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}]}`
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

func TestResolveReleaseOnlySkipsMissingNodePortRoute(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	cfg.SealosDevbox.Network = networkNodePort
	cfg.SealosDevbox.NodeHost = "node-1.example.test"
	leaseID := "cbx_nodeportgone"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	claimExactSealosTarget(t, cfg, leaseID, slug, name, t.TempDir(), core.SSHTarget{})
	item := `{"metadata":{"name":"` + name + `","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running"}}`
	backend := lifecycleBackend(cfg, &lifecycleRunner{outputs: []string{item}})
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.Name != name {
		t.Fatalf("lease=%#v", lease)
	}
	if lease.SSH.Host != "" || lease.SSH.Port != "" || lease.SSH.Key != "" {
		t.Fatalf("release-only resolve should not require SSH route: %#v", lease.SSH)
	}
}

func TestResolveReleaseOnlyAcceptsClaimedDevboxName(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_namedrelease"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	claimExactSealosTarget(t, cfg, leaseID, slug, name, t.TempDir(), core.SSHTarget{})
	item := `{"metadata":{"name":"` + name + `","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running"}}`
	backend := lifecycleBackend(cfg, &lifecycleRunner{outputs: []string{item}})

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: name, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.Name != name {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestResolveReadOnlyDoesNotPersistSecretKey(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_readonlykey"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	item := `{"items":[{"metadata":{"name":"` + name + `","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running"}}]}`
	runner := &lifecycleRunner{outputs: []string{item}}
	backend := lifecycleBackend(cfg, runner)
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, NoLocalStateMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.SSH.Host != "ssh.sealos.example.test" || lease.SSH.Key == "" {
		t.Fatalf("lease=%#v", lease)
	}
	if got := strings.Join(flattenArgs(runner.requests), " "); strings.Contains(got, "secret/") {
		t.Fatalf("read-only resolve fetched Secret: %s", got)
	}
	target := core.SSHTarget{}
	core.UseStoredTestboxKey(&target, leaseID)
	if target.Key != "" {
		t.Fatalf("read-only resolve persisted key: %s", target.Key)
	}
}

func TestResolveReleaseOnlyRejectsUnclaimedBeforeProviderRead(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	runner := &lifecycleRunner{}
	backend := lifecycleBackend(cfg, runner)

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "cbx_unclaimed000", ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "no exact resource-bound local claim") {
		t.Fatalf("Resolve error=%v", err)
	}
	if len(runner.requests) != 0 {
		t.Fatalf("unclaimed release read or mutated provider state: %#v", runner.requests)
	}
}

func TestResolveMutableReuseRequiresExplicitReclaim(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_unclaimedreuse"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	item := `{"items":[{"metadata":{"name":"` + name + `","namespace":"team-a","uid":"uid-test","resourceVersion":"rv-test","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `"}},"status":{"state":"Paused","phase":"Paused"}}]}`
	runner := &lifecycleRunner{outputs: []string{item}}
	backend := lifecycleBackend(cfg, runner)

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "retry a mutable reuse command with --reclaim") {
		t.Fatalf("Resolve error=%v", err)
	}
	commands := strings.Join(flattenArgs(runner.requests), " ")
	if strings.Contains(commands, " patch ") || strings.Contains(commands, "secret/") {
		t.Fatalf("unclaimed reuse mutated resource or read Secret: %s", commands)
	}
}

func TestResolveReclaimRejectsResourceBoundToAnotherLease(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	name := "shared-devbox"
	claimExactSealosTarget(t, cfg, "cbx_existing000", "existing", name, t.TempDir(), core.SSHTarget{})
	leaseID := "cbx_candidate000"
	slug := "candidate"
	item := `{"items":[{"metadata":{"name":"` + name + `","namespace":"team-a","uid":"uid-test","resourceVersion":"rv-test","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `"}},"status":{"state":"Paused","phase":"Paused"}}]}`
	runner := &lifecycleRunner{outputs: []string{item}}
	backend := lifecycleBackend(cfg, runner)

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, Repo: core.Repo{Root: t.TempDir()}, Reclaim: true})
	if err == nil || !strings.Contains(err.Error(), "already bound to lease cbx_existing000") {
		t.Fatalf("Resolve error=%v", err)
	}
	commands := strings.Join(flattenArgs(runner.requests), " ")
	if strings.Contains(commands, " patch ") || strings.Contains(commands, "secret/") {
		t.Fatalf("conflicting reclaim mutated resource or read Secret: %s", commands)
	}
}

func TestResolveChecksRepoClaimBeforeResumeOrSecretRead(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_claimed00000"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	claimedRepo := t.TempDir()
	claimExactSealosTarget(t, cfg, leaseID, slug, name, claimedRepo, core.SSHTarget{})
	pausedItem := `{"metadata":{"name":"` + name + `","namespace":"team-a","uid":"uid-test","resourceVersion":"rv-test","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Paused","phase":"Paused"}}`
	runner := &lifecycleRunner{outputs: []string{pausedItem}}
	backend := lifecycleBackend(cfg, runner)

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "claimed by repo") {
		t.Fatalf("Resolve error=%v", err)
	}
	commands := strings.Join(flattenArgs(runner.requests), " ")
	if strings.Contains(commands, " patch ") || strings.Contains(commands, "secret/") {
		t.Fatalf("claim rejection mutated resource or read Secret: %s", commands)
	}
	target := core.SSHTarget{}
	core.UseStoredTestboxKey(&target, leaseID)
	if target.Key != "" {
		t.Fatalf("claim rejection persisted SSH key: %s", target.Key)
	}
}

func TestResolveRestoresClaimAfterPostClaimFailure(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_claimrollback"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	item := `{"metadata":{"name":"` + name + `","namespace":"team-a","uid":"uid-test","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running"}}`
	runner := &lifecycleRunner{outputs: []string{
		`{"items":[` + item + `]}`,
		ownedDevboxSecretJSON(name, "team-a", "uid-other", "ssh-ed25519 AAA test", "private", false),
	}}
	backend := lifecycleBackend(cfg, runner)

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, Repo: core.Repo{Root: t.TempDir()}, Reclaim: true})
	if err == nil || !strings.Contains(err.Error(), "exact DevBox controller owner") {
		t.Fatalf("Resolve error=%v", err)
	}
	if _, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr != nil || exists {
		t.Fatalf("failed resolve left preflight claim exists=%v err=%v", exists, readErr)
	}
}

func TestResolveResumesPausedDevboxBeforeSSHReuse(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_resume12345"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	repoRoot := t.TempDir()
	claimExactSealosTarget(t, cfg, leaseID, slug, name, repoRoot, core.SSHTarget{})
	pausedItem := `{"metadata":{"name":"` + name + `","namespace":"team-a","uid":"uid-test","resourceVersion":"rv-test","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Paused","phase":"Paused"}}`
	runningItem := `{"metadata":{"name":"` + name + `","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running"}}`
	runningItem = withDevboxUID(runningItem)
	secretJSON := ownedDevboxSecretJSON(name, "team-a", "uid-test", "ssh-ed25519 AAA test", "private", false)
	runner := &lifecycleRunner{outputs: []string{
		pausedItem,
		"patched",
		runningItem,
		secretJSON,
	}}
	backend := lifecycleBackend(cfg, runner)
	probed := false
	backend.sshReady = func(_ context.Context, target *core.SSHTarget, _ io.Writer, phase string, _ time.Duration) error {
		probed = true
		if target.Host != "ssh.sealos.example.test" || target.Key == "" || phase != "Sealos DevBox SSH" {
			t.Fatalf("unexpected SSH target=%#v phase=%q", target, phase)
		}
		return nil
	}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, Repo: core.Repo{Root: repoRoot}})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.Status != "Running" {
		t.Fatalf("lease=%#v", lease)
	}
	if !probed {
		t.Fatal("SSH wait was not called")
	}
	commands := []string{}
	for _, req := range runner.requests {
		commands = append(commands, strings.Join(req.Args, " "))
	}
	if len(commands) < 4 {
		t.Fatalf("commands=%#v", commands)
	}
	if !strings.Contains(commands[1], "patch "+devboxResource+"/"+name) || !strings.Contains(commands[1], `"state":"Running"`) {
		t.Fatalf("resume patch missing: %#v", commands)
	}
	if !strings.Contains(commands[2], "get "+devboxResource+"/"+name) || !strings.Contains(commands[3], "get secret/"+name) {
		t.Fatalf("resume did not refresh before secret read: %#v", commands)
	}
}

func TestResolveResumesPausedNodePortDevboxBeforeRouteLookup(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	cfg.SealosDevbox.Network = networkNodePort
	cfg.SealosDevbox.NodeHost = "node-1.example.test"
	leaseID := "cbx_nodeportres"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	repoRoot := t.TempDir()
	claimExactSealosTarget(t, cfg, leaseID, slug, name, repoRoot, core.SSHTarget{})
	pausedItem := `{"metadata":{"name":"` + name + `","namespace":"team-a","uid":"uid-test","resourceVersion":"rv-test","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Paused","phase":"Paused"}}`
	runningItem := `{"metadata":{"name":"` + name + `","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running","network":{"type":"NodePort","nodePort":32022}}}`
	runningItem = withDevboxUID(runningItem)
	secretJSON := ownedDevboxSecretJSON(name, "team-a", "uid-test", "ssh-ed25519 AAA test", "private", false)
	runner := &lifecycleRunner{outputs: []string{
		pausedItem,
		"patched",
		runningItem,
		secretJSON,
	}}
	backend := lifecycleBackend(cfg, runner)
	backend.sshReady = func(_ context.Context, target *core.SSHTarget, _ io.Writer, phase string, _ time.Duration) error {
		if target.Host != "node-1.example.test" || target.Port != "32022" || target.Key == "" || phase != "Sealos DevBox SSH" {
			t.Fatalf("unexpected SSH target=%#v phase=%q", target, phase)
		}
		return nil
	}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, Repo: core.Repo{Root: repoRoot}})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Host != "node-1.example.test" || lease.SSH.Port != "32022" {
		t.Fatalf("lease=%#v", lease)
	}
	commands := strings.Join(flattenArgs(runner.requests), " ")
	if !strings.Contains(commands, "patch "+devboxResource+"/"+name) || !strings.Contains(commands, "get secret/"+name) {
		t.Fatalf("missing resume or secret commands: %s", commands)
	}
}

func TestWaitForDevboxPreparedWaitsForNodePortRoute(t *testing.T) {
	cfg := lifecycleConfig()
	cfg.SealosDevbox.Network = networkNodePort
	name := "crabbox-blue-12345678"
	runner := &lifecycleRunner{outputs: []string{
		`{"metadata":{"name":"` + name + `"},"status":{"state":"Running","phase":"Running","network":{"type":"NodePort"}}}`,
		`{"metadata":{"name":"` + name + `"},"status":{"state":"Running","phase":"Running","network":{"type":"NodePort","nodePort":32022}}}`,
	}}
	backend := lifecycleBackend(cfg, runner)
	backend.pollIntervalOverride = time.Millisecond
	item, err := backend.waitForDevboxPrepared(context.Background(), name, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	port, ok := devboxSSHNodePort(item)
	if !ok || port != 32022 {
		t.Fatalf("NodePort route=%d ok=%t item=%#v", port, ok, item)
	}
	if len(runner.requests) != 2 {
		t.Fatalf("requests=%#v", runner.requests)
	}
}

func TestResolveRejectsSecretOwnedByAnotherDevbox(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_wrongowner"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	repoRoot := t.TempDir()
	claimExactSealosTarget(t, cfg, leaseID, slug, name, repoRoot, core.SSHTarget{})
	item := `{"metadata":{"name":"` + name + `","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running"}}`
	item = withDevboxUID(item)
	runner := &lifecycleRunner{outputs: []string{
		item,
		ownedDevboxSecretJSON(name, "team-a", "uid-other", "ssh-ed25519 AAA test", "private", false),
	}}
	backend := lifecycleBackend(cfg, runner)
	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, Repo: core.Repo{Root: repoRoot}})
	if err == nil || !strings.Contains(err.Error(), "exact DevBox controller owner") {
		t.Fatalf("Resolve error=%v", err)
	}
	target := core.SSHTarget{}
	core.UseStoredTestboxKey(&target, leaseID)
	if target.Key != "" {
		t.Fatalf("unowned Secret key persisted: %s", target.Key)
	}
}

func TestResolveDoesNotReplaceKeyAfterClaimTransfer(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_resolvekeyrace"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	repoRoot := t.TempDir()
	server := claimExactSealosTarget(t, cfg, leaseID, slug, name, repoRoot, core.SSHTarget{Host: "winner.example.test", Port: "22"})
	keyPath, err := persistDevboxKey(leaseID, devboxSecretKeys{PublicKey: "ssh-ed25519 AAA winner", PrivateKey: "winner-private\n"})
	if err != nil {
		t.Fatal(err)
	}
	item := `{"metadata":{"name":"` + name + `","namespace":"team-a","uid":"uid-test","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running"}}`
	inner := &lifecycleRunner{outputs: []string{
		item,
		ownedDevboxSecretJSON(name, "team-a", "uid-test", "ssh-ed25519 AAA loser", "loser-private\n", false),
	}}
	transferred := false
	backend := lifecycleBackend(cfg, inner)
	backend.rt.Exec = &hookLifecycleRunner{inner: inner, before: func(req core.LocalCommandRequest) {
		if transferred || !strings.Contains(commandString(req), "get secret/") {
			return
		}
		transferred = true
		if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{Host: "winner.example.test", Port: "22"}, t.TempDir(), cfg.IdleTimeout, true); err != nil {
			t.Fatal(err)
		}
	}}

	_, err = backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, Repo: core.Repo{Root: repoRoot}})
	if err == nil || !strings.Contains(err.Error(), "claim changed; retry") {
		t.Fatalf("Resolve error=%v", err)
	}
	if !transferred {
		t.Fatal("test did not transfer claim during Secret read")
	}
	privateKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(privateKey) != "winner-private\n" {
		t.Fatalf("stale resolve replaced winning key: %q", privateKey)
	}
}

func TestTouchRefreshesRemoteLeaseAnnotationsWithResourceVersion(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_touch123456"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	claimedServer := claimExactSealosTarget(t, cfg, leaseID, slug, name, t.TempDir(), core.SSHTarget{Host: "ssh.sealos.example.test", User: "devbox", Port: "2222", Key: "/tmp/key"})
	item := `{"metadata":{"name":"` + name + `","namespace":"team-a","uid":"uid-test","resourceVersion":"rv-touch","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a","crabbox.dev/last_touched_at":"2026-06-01T00:00:00Z"}},"status":{"state":"Running","phase":"Running"}}`
	runner := &lifecycleRunner{outputs: []string{item, "patched"}}
	backend := lifecycleBackend(cfg, runner)
	server, err := backend.Touch(context.Background(), core.TouchRequest{
		Lease: core.LeaseTarget{
			LeaseID: leaseID,
			Server:  claimedServer,
			SSH:     core.SSHTarget{Host: "ssh.sealos.example.test", User: "devbox", Port: "2222", Key: "/tmp/key"},
		},
		State: "running",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantTouched := strconv.FormatInt(backend.now().Unix(), 10)
	if server.Labels["last_touched_at"] != wantTouched {
		t.Fatalf("last_touched_at=%q", server.Labels["last_touched_at"])
	}
	if claim, exists, set := core.ServerLeaseClaimSnapshot(server); !set || !exists || claim.Labels["last_touched_at"] != wantTouched {
		t.Fatalf("touch claim snapshot set=%v exists=%v claim=%#v", set, exists, claim)
	}
	if len(runner.requests) != 2 || !strings.Contains(commandString(runner.requests[1]), "patch "+devboxResource+"/"+name) {
		t.Fatalf("requests=%#v", runner.requests)
	}
	args := runner.requests[1].Args
	patchIndex := -1
	for i := range args {
		if args[i] == "-p" && i+1 < len(args) {
			patchIndex = i + 1
			break
		}
	}
	if patchIndex < 0 {
		t.Fatalf("patch payload missing: %#v", args)
	}
	var payload struct {
		Metadata struct {
			ResourceVersion string         `json:"resourceVersion"`
			Annotations     map[string]any `json:"annotations"`
		} `json:"metadata"`
		Spec map[string]any `json:"spec"`
	}
	if err := json.Unmarshal([]byte(args[patchIndex]), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Metadata.ResourceVersion != "rv-touch" || payload.Metadata.Annotations[annotationBase+"last_touched_at"] != wantTouched {
		t.Fatalf("touch patch=%#v", payload)
	}
	wantScopeID := sealosClaimScopeID(cfg)
	if len(wantScopeID) != 64 {
		t.Fatalf("scope fingerprint length=%d", len(wantScopeID))
	}
	if payload.Metadata.Annotations[annotationBase+"provider-scope"] != wantScopeID || payload.Metadata.Annotations[annotationBase+"provider_scope_id"] != wantScopeID {
		t.Fatalf("touch truncated ownership annotations: %#v", payload.Metadata.Annotations)
	}
	if server.Labels["provider-scope"] != wantScopeID || server.Labels["provider_scope_id"] != wantScopeID {
		t.Fatalf("touch truncated ownership labels: %#v", server.Labels)
	}
	if payload.Spec != nil {
		t.Fatalf("touch must not overwrite desired state: %#v", payload.Spec)
	}
}

func TestTouchRejectsTransferredClaimSnapshotBeforeMutation(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_transferredtouch"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	server := claimExactSealosTarget(t, cfg, leaseID, slug, name, t.TempDir(), core.SSHTarget{Host: "old.example.test", Port: "22"})
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{Host: "new.example.test", Port: "22"}, t.TempDir(), cfg.IdleTimeout, true); err != nil {
		t.Fatal(err)
	}
	item := `{"metadata":{"name":"` + name + `","namespace":"team-a","uid":"uid-test","resourceVersion":"rv-touch","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running"}}`
	runner := &lifecycleRunner{outputs: []string{item}}
	backend := lifecycleBackend(cfg, runner)

	_, err := backend.Touch(context.Background(), core.TouchRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}, State: "running"})
	if err == nil || !strings.Contains(err.Error(), "claim changed; retry") {
		t.Fatalf("Touch error=%v", err)
	}
	if got := strings.Join(flattenArgs(runner.requests), " "); strings.Contains(got, " patch ") {
		t.Fatalf("transferred touch mutated provider: %s", got)
	}
}

func TestResolveClaimRejectsDevboxOutsideActiveScope(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_wrongscope"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	backend := lifecycleBackend(cfg, &lifecycleRunner{})
	claimExactSealosTarget(t, cfg, leaseID, slug, name, t.TempDir(), core.SSHTarget{})
	item := `{"metadata":{"name":"` + name + `","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider_scope":"other-scope","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running"}}`
	backend.rt.Exec = &lifecycleRunner{outputs: []string{item}}
	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "outside the active provider scope") {
		t.Fatalf("Resolve error=%v", err)
	}
}

func TestResolveClaimRejectsDevboxMissingProviderScope(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_noscope123"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	backend := lifecycleBackend(cfg, &lifecycleRunner{})
	claimExactSealosTarget(t, cfg, leaseID, slug, name, t.TempDir(), core.SSHTarget{})
	item := `{"metadata":{"name":"` + name + `","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running"}}`
	backend.rt.Exec = &lifecycleRunner{outputs: []string{item}}
	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "outside the active provider scope") {
		t.Fatalf("Resolve error=%v", err)
	}
}

func TestItemMatchesScopeAcceptsLegacyRawProviderScope(t *testing.T) {
	cfg := lifecycleConfig()
	backend := lifecycleBackend(cfg, &lifecycleRunner{})
	item := devboxItem{}
	item.Metadata.Labels = map[string]string{
		managedByLabel:     "crabbox",
		providerLabel:      providerName,
		providerScopeLabel: "legacy-scope-label",
	}
	item.Metadata.Annotations = map[string]string{
		annotationBase + "provider_scope": sealosClaimScope(cfg),
	}
	if !backend.itemMatchesScope(item) {
		t.Fatalf("legacy raw provider scope did not match")
	}
}

func TestWaitForDevboxSecretRefreshesDevboxIdentity(t *testing.T) {
	cfg := lifecycleConfig()
	name := "crabbox-blue-12345678"
	initial := devboxItem{
		Metadata: devboxMeta{Name: name},
		Status:   devboxStatus{State: "Running", Phase: "Running"},
	}
	refreshedItem := `{"metadata":{"name":"` + name + `","namespace":"team-a","uid":"uid-test"},"status":{"state":"Running","phase":"Running"}}`
	secretJSON := ownedDevboxSecretJSON(name, "team-a", "uid-test", "ssh-ed25519 AAA test", "private", false)
	runner := &lifecycleRunner{
		outputs:  []string{"", refreshedItem, secretJSON},
		stderrs:  []string{`Error from server (NotFound): secrets "` + name + `" not found`},
		exitCode: []int{1},
		errors:   []error{errors.New("exit status 1")},
	}
	backend := lifecycleBackend(cfg, runner)
	secret, err := backend.waitForDevboxSecret(context.Background(), initial, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := parseDevboxSecretKeys(secret)
	if err != nil {
		t.Fatal(err)
	}
	if keys.PrivateKey != "private\n" {
		t.Fatalf("keys=%#v", keys)
	}
	if len(runner.requests) != 3 {
		t.Fatalf("requests=%#v", runner.requests)
	}
	commands := []string{}
	for _, req := range runner.requests {
		commands = append(commands, strings.Join(req.Args, " "))
	}
	if !strings.Contains(commands[0], "get secret/"+name) || !strings.Contains(commands[1], "get "+devboxResource+"/"+name) || !strings.Contains(commands[2], "get secret/"+name) {
		t.Fatalf("commands=%#v", commands)
	}
}

func flattenArgs(requests []core.LocalCommandRequest) []string {
	out := []string{}
	for _, req := range requests {
		out = append(out, req.Args...)
	}
	return out
}

func isRulesReviewRequest(req core.LocalCommandRequest) bool {
	args := strings.Join(req.Args, " ")
	return strings.Contains(args, "create --raw /apis/authorization.k8s.io/v1/selfsubjectrulesreviews -f -")
}

func isDevboxDiscoveryRequest(req core.LocalCommandRequest) bool {
	args := strings.Join(req.Args, " ")
	return strings.Contains(args, "get --raw /apis/"+devboxGroupVersion)
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
