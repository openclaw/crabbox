package proxmox

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

const controllerTestSecret = "controller-scope-secret-value"

func controllerTestConfig() core.Config {
	cfg := core.BaseConfig()
	cfg.Provider = "proxmox"
	cfg.TargetOS = core.TargetLinux
	cfg.Proxmox = core.ProxmoxConfig{
		APIURL:      "https://pve.example.test:8006",
		TokenID:     "runner@pve!crabbox",
		TokenSecret: controllerTestSecret,
		Node:        "pve1",
		TemplateID:  9400,
		Storage:     "local-lvm",
		Pool:        "crabbox",
		Bridge:      "vmbr0",
		User:        "crabbox",
		WorkRoot:    "/work/crabbox",
		FullClone:   true,
	}
	return cfg
}

func controllerTestScope(t *testing.T, cfg core.Config) string {
	t.Helper()
	scope, err := (Provider{}).ControllerProviderScope(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func TestProxmoxExposesControllerContract(t *testing.T) {
	cfg := controllerTestConfig()
	before := cfg
	registered, err := core.ProviderFor("proxmox")
	if err != nil {
		t.Fatal(err)
	}
	contract, ok := registered.(core.ControllerProviderContract)
	if !ok {
		t.Fatal("registered Proxmox provider has no controller contract")
	}
	scope, err := contract.ControllerProviderScope(cfg)
	if err != nil || !strings.HasPrefix(scope, proxmoxControllerScopePrefix) || len(scope) != len(proxmoxControllerScopePrefix)+64 {
		t.Fatalf("scope=%q err=%v", scope, err)
	}
	if !contract.SupportsControllerFixedLeaseID(cfg) {
		t.Fatal("complete Proxmox configuration does not advertise fixed lease IDs")
	}
	if !reflect.DeepEqual(cfg, before) {
		t.Fatal("controller contract mutated config")
	}
}

func TestProxmoxControllerScopeIsStableAndSecretFree(t *testing.T) {
	base := controllerTestScope(t, controllerTestConfig())
	for name, mutate := range map[string]func(*core.Config){
		"token secret rotation":   func(cfg *core.Config) { cfg.Proxmox.TokenSecret = "rotated-secret-value" },
		"endpoint spelling":       func(cfg *core.Config) { cfg.Proxmox.APIURL = " HTTPS://PVE.example.test:8006/api2/json/ " },
		"surrounding whitespace":  func(cfg *core.Config) { cfg.Proxmox.Node, cfg.Proxmox.TokenID = " pve1 ", " runner@pve!crabbox " },
		"per-lease timing policy": func(cfg *core.Config) { cfg.TTL, cfg.IdleTimeout = 3*time.Hour, time.Hour },
		"TLS verification mode":   func(cfg *core.Config) { cfg.Proxmox.InsecureTLS = true },
		"implicit Linux target":   func(cfg *core.Config) { cfg.TargetOS = "" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := controllerTestConfig()
			mutate(&cfg)
			if got := controllerTestScope(t, cfg); got != base {
				t.Fatalf("scope changed: %q != %q", got, base)
			}
		})
	}
	for _, private := range []string{controllerTestSecret, "runner@pve", "pve.example.test"} {
		if strings.Contains(base, private) {
			t.Fatalf("scope %q exposes %q", base, private)
		}
	}
}

func TestProxmoxControllerScopeBindsRoutePrincipalAndCloneProfile(t *testing.T) {
	base := controllerTestScope(t, controllerTestConfig())
	seen := map[string]string{base: "base"}
	for name, mutate := range map[string]func(*core.Config){
		"endpoint":   func(cfg *core.Config) { cfg.Proxmox.APIURL = "https://pve2.example.test:8006" },
		"node":       func(cfg *core.Config) { cfg.Proxmox.Node = "pve2" },
		"token ID":   func(cfg *core.Config) { cfg.Proxmox.TokenID = "runner@pve!rotated" },
		"template":   func(cfg *core.Config) { cfg.Proxmox.TemplateID = 9401 },
		"storage":    func(cfg *core.Config) { cfg.Proxmox.Storage = "ceph" },
		"pool":       func(cfg *core.Config) { cfg.Proxmox.Pool = "" },
		"bridge":     func(cfg *core.Config) { cfg.Proxmox.Bridge = "vmbr1" },
		"clone mode": func(cfg *core.Config) { cfg.Proxmox.FullClone = false },
		"guest user": func(cfg *core.Config) { cfg.Proxmox.User = "runner" },
		"work root":  func(cfg *core.Config) { cfg.Proxmox.WorkRoot = "/srv/crabbox" },
	} {
		cfg := controllerTestConfig()
		mutate(&cfg)
		scope := controllerTestScope(t, cfg)
		if previous, ok := seen[scope]; ok {
			t.Fatalf("%s change produced the same scope as %s", name, previous)
		}
		seen[scope] = name
	}
}

func TestProxmoxControllerScopeRejectsIncompleteConfig(t *testing.T) {
	for name, mutate := range map[string]func(*core.Config){
		"endpoint":           func(cfg *core.Config) { cfg.Proxmox.APIURL = " " },
		"node":               func(cfg *core.Config) { cfg.Proxmox.Node = "" },
		"token ID":           func(cfg *core.Config) { cfg.Proxmox.TokenID = "" },
		"token secret":       func(cfg *core.Config) { cfg.Proxmox.TokenSecret = "" },
		"blank token secret": func(cfg *core.Config) { cfg.Proxmox.TokenSecret = " \t" },
		"template":           func(cfg *core.Config) { cfg.Proxmox.TemplateID = 0 },
		"target":             func(cfg *core.Config) { cfg.TargetOS = core.TargetWindows },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := controllerTestConfig()
			mutate(&cfg)
			if scope, err := (Provider{}).ControllerProviderScope(cfg); err == nil || scope != "" {
				t.Fatalf("scope=%q err=%v", scope, err)
			}
			if (Provider{}).SupportsControllerFixedLeaseID(cfg) {
				t.Fatal("incomplete configuration advertised fixed lease IDs")
			}
		})
	}
}

func setControllerTestEnv(t *testing.T, cfg core.Config) {
	t.Helper()
	testutil.IsolateUserDirs(t)
	for key, value := range map[string]string{
		"CRABBOX_PROVIDER":             "proxmox",
		"CRABBOX_PROXMOX_API_URL":      cfg.Proxmox.APIURL,
		"CRABBOX_PROXMOX_TOKEN_ID":     cfg.Proxmox.TokenID,
		"CRABBOX_PROXMOX_TOKEN_SECRET": cfg.Proxmox.TokenSecret,
		"CRABBOX_PROXMOX_NODE":         cfg.Proxmox.Node,
		"CRABBOX_PROXMOX_TEMPLATE_ID":  strconv.Itoa(cfg.Proxmox.TemplateID),
		"CRABBOX_PROXMOX_STORAGE":      cfg.Proxmox.Storage,
		"CRABBOX_PROXMOX_POOL":         cfg.Proxmox.Pool,
		"CRABBOX_PROXMOX_BRIDGE":       cfg.Proxmox.Bridge,
		"CRABBOX_PROXMOX_USER":         cfg.Proxmox.User,
		"CRABBOX_PROXMOX_WORK_ROOT":    cfg.Proxmox.WorkRoot,
		"CRABBOX_PROXMOX_FULL_CLONE":   strconv.FormatBool(cfg.Proxmox.FullClone),
	} {
		t.Setenv(key, value)
	}
}

// The runtime adapter resolves this identity before it listens.
func TestProxmoxAdapterProviderIdentityCommand(t *testing.T) {
	cfg := controllerTestConfig()
	setControllerTestEnv(t, cfg)
	var stdout, stderr bytes.Buffer
	err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"config", "show", "--json", "--controller-provider-identity", "--provider", "proxmox"})
	if err != nil {
		t.Fatalf("identity command: %v: %s", err, stderr.String())
	}
	var identity struct {
		Provider          string `json:"provider"`
		ProviderScope     string `json:"providerScope"`
		IdempotentLeaseID bool   `json:"idempotentLeaseId"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &identity); err != nil {
		t.Fatal(err)
	}
	if identity.Provider != "proxmox" || identity.ProviderScope != controllerTestScope(t, cfg) || !identity.IdempotentLeaseID {
		t.Fatalf("identity=%+v", identity)
	}
	if strings.Contains(stdout.String()+stderr.String(), controllerTestSecret) {
		t.Fatal("identity command exposed the token secret")
	}
}

func TestProxmoxAdapterIdentityRequiresTokenSecret(t *testing.T) {
	cfg := controllerTestConfig()
	setControllerTestEnv(t, cfg)
	t.Setenv("CRABBOX_PROXMOX_TOKEN_SECRET", "")
	var stdout bytes.Buffer
	if err := (core.App{Stdout: &stdout, Stderr: &bytes.Buffer{}}).Run(t.Context(), []string{"config", "show", "--json", "--controller-provider-identity", "--provider", "proxmox"}); err != nil {
		t.Fatal(err)
	}
	var identity struct {
		ProviderScope     string `json:"providerScope"`
		IdempotentLeaseID bool   `json:"idempotentLeaseId"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &identity); err != nil {
		t.Fatal(err)
	}
	// adapter serve refuses to listen without both values.
	if identity.ProviderScope != "" || identity.IdempotentLeaseID {
		t.Fatalf("identity advertised without a token secret: %+v", identity)
	}
}

// An existing adapter workspace keeps the scope recorded at creation. After
// the configured token principal changes, every child command the adapter runs
// for it must stop before a Proxmox client exists or any request is sent.
func TestProxmoxAdapterRejectsChangedPrincipalBeforeProxmoxIO(t *testing.T) {
	_, fake, _ := fixedProxmoxFixture(t)
	var requests atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected Proxmox request", http.StatusInternalServerError)
	}))
	defer api.Close()
	cfg := controllerTestConfig()
	cfg.Proxmox.APIURL = api.URL
	setControllerTestEnv(t, cfg)
	persisted := controllerTestScope(t, cfg)
	t.Setenv("CRABBOX_ADAPTER_PROVIDER_SCOPE", persisted)
	var clients int
	useClient := func(connect func(core.Config) (proxmoxClient, error)) {
		newClient = func(cfg core.Config) (proxmoxClient, error) {
			clients++
			return connect(cfg)
		}
	}
	useClient(func(core.Config) (proxmoxClient, error) { return fake, nil })
	const leaseID, slug = "cbx_0123456789ab", "adapter-box"
	run := func(args ...string) error {
		var stdout, stderr bytes.Buffer
		err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args)
		for _, secret := range []string{controllerTestSecret, "rotated-principal-secret", "rotated-original-secret"} {
			if strings.Contains(stdout.String()+stderr.String(), secret) {
				t.Fatalf("%s exposed a token secret", args[0])
			}
		}
		return err
	}
	if err := run("warmup", "--keep=true", "--lease-id", leaseID, "--slug", slug); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if clients == 0 || fake.fixedCreates != 1 {
		t.Fatalf("workspace creation clients=%d clones=%d", clients, fake.fixedCreates)
	}
	// Release reads provider state only; keep best-effort guest cleanup offline.
	fake.servers[0].PublicNet.IPv4.IP = ""
	before := readFixedProxmoxClaim(t, leaseID)

	// Any client created from here on sends real HTTP to the recording API.
	useClient(func(cfg core.Config) (proxmoxClient, error) { return core.NewProxmoxClient(cfg) })
	t.Setenv("CRABBOX_PROXMOX_TOKEN_ID", "other@pve!rotated")
	t.Setenv("CRABBOX_PROXMOX_TOKEN_SECRET", "rotated-principal-secret")
	clients = 0
	// Arguments match execControllerWorkspaceRunner for an unregistered adapter.
	expected := []string{"--id", leaseID,
		"--expected-provider-lease-id", leaseID, "--expected-provider-attempt-lease-id", leaseID,
		"--expected-provider-slug", slug, "--expected-provider-resource-id", "417", "--expected-provider-scope", persisted}
	stop := append(append([]string{"stop"}, expected...), "--provider", "proxmox")
	for name, args := range map[string][]string{
		"inspect":         {"inspect", "--id", leaseID, "--json", "--provider", "proxmox"},
		"inventory":       {"list", "--json", "--refresh", "--all", "--provider", "proxmox"},
		"replay":          {"warmup", "--keep=true", "--lease-id", leaseID, "--slug", slug, "--provider", "proxmox"},
		"stop":            stop,
		"absence cleanup": append(append([]string{"stop", "--confirmed-absent-local-cleanup=true"}, expected...), "--expected-coordinator-registration-url", "", "--provider", "proxmox"),
	} {
		if err := run(args...); err == nil || !strings.Contains(err.Error(), "scope changed") {
			t.Fatalf("%s with a changed principal: %v", name, err)
		}
	}
	if clients != 0 || requests.Load() != 0 || fake.deleteCalls != 0 || fake.fixedCreates != 1 {
		t.Fatalf("changed principal reached Proxmox: clients=%d requests=%d deletes=%d clones=%d", clients, requests.Load(), fake.deleteCalls, fake.fixedCreates)
	}
	if after := readFixedProxmoxClaim(t, leaseID); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected operations changed the workspace claim")
	}

	// The original token ID manages the workspace again, even with a new secret.
	useClient(func(core.Config) (proxmoxClient, error) { return fake, nil })
	t.Setenv("CRABBOX_PROXMOX_TOKEN_ID", cfg.Proxmox.TokenID)
	t.Setenv("CRABBOX_PROXMOX_TOKEN_SECRET", "rotated-original-secret")
	if err := run(stop...); err != nil {
		t.Fatalf("stop with the original principal: %v", err)
	}
	if clients == 0 || fake.deleteCalls != 1 || readFixedProxmoxClaim(t, leaseID).FixedCreateIntent.State != "released" {
		t.Fatalf("allowed stop clients=%d deletes=%d", clients, fake.deleteCalls)
	}
}

// These are the fixed child commands that adapter serve runs for one workspace.
func TestProxmoxAdapterFixedLifecycleUnderPersistedScope(t *testing.T) {
	_, client, _ := fixedProxmoxFixture(t)
	cfg := controllerTestConfig()
	setControllerTestEnv(t, cfg)
	scope := controllerTestScope(t, cfg)
	t.Setenv("CRABBOX_ADAPTER_PROVIDER_SCOPE", scope)
	const leaseID, slug = "cbx_0123456789ab", "adapter-box"
	run := func(args ...string) error {
		var stdout, stderr bytes.Buffer
		err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args)
		if strings.Contains(stdout.String()+stderr.String(), controllerTestSecret) {
			t.Fatalf("%s exposed the token secret", args[0])
		}
		return err
	}
	for attempt := range 2 {
		if err := run("warmup", "--keep=true", "--lease-id", leaseID, "--slug", slug); err != nil {
			t.Fatalf("warmup attempt %d: %v", attempt+1, err)
		}
	}
	if client.fixedCreates != 1 {
		t.Fatalf("fixed replay cloned %d VMs", client.fixedCreates)
	}
	stop := []string{"stop", "--id", leaseID,
		"--expected-provider-lease-id", leaseID, "--expected-provider-attempt-lease-id", leaseID,
		"--expected-provider-slug", slug, "--expected-provider-resource-id", "417", "--expected-provider-scope", scope}
	absenceCleanup := func(resourceID string) []string {
		return []string{"stop", "--confirmed-absent-local-cleanup=true", "--id", leaseID,
			"--expected-provider-lease-id", leaseID, "--expected-provider-attempt-lease-id", leaseID,
			"--expected-provider-slug", slug, "--expected-provider-resource-id", resourceID, "--expected-provider-scope", scope,
			"--expected-coordinator-registration-url", "", "--provider", "proxmox"}
	}
	if err := run(absenceCleanup("417")...); err == nil || !strings.Contains(err.Error(), "invalid terminal tombstone") || readFixedProxmoxClaim(t, leaseID).FixedCreateIntent.State != "acquired" {
		t.Fatalf("absence cleanup settled a live workspace: %v", err)
	}
	// Release reads provider state only; keep best-effort guest cleanup offline.
	client.servers[0].PublicNet.IPv4.IP = ""
	wrongResource := append([]string(nil), stop...)
	wrongResource[10] = "418"
	if err := run(wrongResource...); err == nil || !strings.Contains(err.Error(), "resource ID mismatch") || client.deleteCalls != 0 {
		t.Fatalf("stop accepted another resource identity: err=%v deletes=%d", err, client.deleteCalls)
	}
	if err := run(stop...); err != nil {
		t.Fatalf("exact stop: %v", err)
	}
	claim := readFixedProxmoxClaim(t, leaseID)
	if client.deleteCalls != 1 || claim.FixedCreateIntent.State != "released" {
		t.Fatalf("deletes=%d claim=%+v", client.deleteCalls, claim)
	}
	// After the adapter confirms absence it finishes cleanup and keeps the
	// released claim as the lease ID's receipt, including on retry.
	if err := run(absenceCleanup("418")...); err == nil || !strings.Contains(err.Error(), "receipt identity changed") {
		t.Fatalf("absence cleanup accepted another resource identity: %v", err)
	}
	for attempt := range 2 {
		if err := run(absenceCleanup("417")...); err != nil {
			t.Fatalf("absence cleanup attempt %d: %v", attempt+1, err)
		}
	}
	if receipt := readFixedProxmoxClaim(t, leaseID); !reflect.DeepEqual(receipt, claim) || client.deleteCalls != 1 {
		t.Fatalf("absence cleanup changed the receipt or Proxmox: deletes=%d receipt=%+v", client.deleteCalls, receipt)
	}
}

func TestProxmoxConfirmedAbsentReceiptMatchesScopesAndIdentity(t *testing.T) {
	backend, _, req := fixedProxmoxFixture(t)
	lease, err := backend.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.releaseFixed(t.Context(), core.ReleaseLeaseRequest{Lease: lease}, false); err != nil {
		t.Fatal(err)
	}
	receipt := readFixedProxmoxClaim(t, req.RequestedLeaseID)
	scope, err := (Provider{}).ControllerProviderScope(backend.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	valid := core.ConfirmedAbsentLocalCleanupRequest{ProviderScope: scope, ExpectedProviderIdentity: core.ProviderIdentityExpectation{
		LeaseID: receipt.LeaseID, AttemptLeaseID: receipt.LeaseID, Slug: receipt.Slug, ResourceID: receipt.CloudID,
	}}
	if err := backend.ValidateConfirmedAbsentTerminalReceipt(receipt, valid); err != nil {
		t.Fatalf("released receipt: %v", err)
	}
	for name, mutate := range map[string]func(*core.LeaseClaim, *core.ConfirmedAbsentLocalCleanupRequest){
		"controller scope": func(_ *core.LeaseClaim, r *core.ConfirmedAbsentLocalCleanupRequest) {
			r.ProviderScope = proxmoxControllerScopePrefix + strings.Repeat("0", 64)
		},
		"claim scope": func(c *core.LeaseClaim, _ *core.ConfirmedAbsentLocalCleanupRequest) {
			c.ProviderScope = "endpoint:https://pve.example.test:8006|node:pve2"
			c.FixedCreateIntent.ProviderScope = c.ProviderScope
		},
		"live claim": func(c *core.LeaseClaim, _ *core.ConfirmedAbsentLocalCleanupRequest) {
			c.FixedCreateIntent.State = "acquired"
		},
		"missing receipt": func(c *core.LeaseClaim, _ *core.ConfirmedAbsentLocalCleanupRequest) { *c = core.LeaseClaim{} },
		"lease": func(_ *core.LeaseClaim, r *core.ConfirmedAbsentLocalCleanupRequest) {
			r.ExpectedProviderIdentity.LeaseID = "cbx_abcdefabcdef"
		},
		"attempt": func(_ *core.LeaseClaim, r *core.ConfirmedAbsentLocalCleanupRequest) {
			r.ExpectedProviderIdentity.AttemptLeaseID = "cbx_abcdefabcdef"
		},
		"slug": func(_ *core.LeaseClaim, r *core.ConfirmedAbsentLocalCleanupRequest) {
			r.ExpectedProviderIdentity.Slug = "other-box"
		},
		"resource": func(_ *core.LeaseClaim, r *core.ConfirmedAbsentLocalCleanupRequest) {
			r.ExpectedProviderIdentity.ResourceID = "418"
		},
		"incomplete": func(_ *core.LeaseClaim, r *core.ConfirmedAbsentLocalCleanupRequest) {
			r.ExpectedProviderIdentity.ResourceID = ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			claim, request := core.CloneLeaseClaim(receipt), valid
			mutate(&claim, &request)
			if err := backend.ValidateConfirmedAbsentTerminalReceipt(claim, request); err == nil {
				t.Fatal("receipt accepted")
			}
		})
	}
}

// A registered adapter completes the coordinator's delete with the lease's
// registration generation, which must survive the fixed release receipt.
func TestProxmoxRegisteredAdapterDeleteCompletesAfterFixedRelease(t *testing.T) {
	_, client, _ := fixedProxmoxFixture(t)
	const leaseID, slug, adapterID, workspaceID = "cbx_0123456789ab", "adapter-box", "proxmox-lab", "fleet-box"
	var mu sync.Mutex
	var registered string
	var completions, rejected []string
	coordinator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RegistrationID string `json:"runtimeAdapterRegistrationID"`
			Completion     *struct {
				AdapterID      string `json:"adapterID"`
				WorkspaceID    string `json:"workspaceID"`
				RegistrationID string `json:"registrationID"`
			} `json:"runtimeAdapterDeleteCompletion"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		state := "active"
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/leases/"+leaseID+"/registration":
			registered = body.RegistrationID
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/"+leaseID+"/release" && body.Completion != nil &&
			body.Completion.AdapterID == adapterID && body.Completion.WorkspaceID == workspaceID:
			completions = append(completions, body.Completion.RegistrationID)
			state = "released"
		default:
			// The coordinator refuses metadata-only release while a registered delete is pending.
			rejected = append(rejected, r.Method+" "+r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"runtime_adapter_delete_pending"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": map[string]any{
			"id": leaseID, "provider": "proxmox", "lifecycle": "registered", "state": state,
			"runtimeAdapterID": adapterID, "runtimeAdapterWorkspaceID": workspaceID, "runtimeAdapterRegistrationID": registered,
		}})
	}))
	defer coordinator.Close()
	cfg := controllerTestConfig()
	setControllerTestEnv(t, cfg)
	for key, value := range map[string]string{
		"CRABBOX_COORDINATOR": coordinator.URL, "CRABBOX_COORDINATOR_TOKEN": "coordinator-test-token",
		"CRABBOX_COORDINATOR_MODE": "registered", "CRABBOX_ADAPTER_ID": adapterID, "CRABBOX_ADAPTER_WORKSPACE_ID": workspaceID,
	} {
		t.Setenv(key, value)
	}
	scope := controllerTestScope(t, cfg)
	t.Setenv("CRABBOX_ADAPTER_PROVIDER_SCOPE", scope)
	run := func(args ...string) (string, error) {
		var stdout, stderr bytes.Buffer
		err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args)
		return stdout.String(), err
	}
	identity, err := run("config", "show", "--json", "--controller-provider-identity", "--provider", "proxmox")
	if err != nil {
		t.Fatal(err)
	}
	var binding struct {
		URL string `json:"coordinatorRegistrationUrl"`
	}
	if err := json.Unmarshal([]byte(identity), &binding); err != nil || binding.URL == "" {
		t.Fatalf("registration binding=%q err=%v", identity, err)
	}
	if _, err := run("warmup", "--keep=true", "--lease-id", leaseID, "--slug", slug, "--provider", "proxmox"); err != nil {
		t.Fatalf("registered warmup: %v", err)
	}
	if registered == "" || readFixedProxmoxClaim(t, leaseID).RuntimeAdapterRegistrationID != registered {
		t.Fatalf("registration generation=%q claim=%+v", registered, readFixedProxmoxClaim(t, leaseID))
	}
	// Release reads provider state only; keep best-effort guest cleanup offline.
	client.servers[0].PublicNet.IPv4.IP = ""
	identityArgs := []string{"--id", leaseID,
		"--expected-provider-lease-id", leaseID, "--expected-provider-attempt-lease-id", leaseID,
		"--expected-provider-slug", slug, "--expected-provider-resource-id", "417", "--expected-provider-scope", scope}
	if _, err := run(append(append([]string{"stop"}, identityArgs...), "--provider", "proxmox")...); err != nil {
		t.Fatalf("stop: %v", err)
	}
	receipt := readFixedProxmoxClaim(t, leaseID)
	if client.deleteCalls != 1 || receipt.FixedCreateIntent.State != "released" || len(completions) != 0 {
		t.Fatalf("release deletes=%d completions=%v receipt=%+v", client.deleteCalls, completions, receipt)
	}
	cleanup := append(append([]string{"stop", "--confirmed-absent-local-cleanup=true"}, identityArgs...),
		"--expected-coordinator-registration-url", binding.URL, "--provider", "proxmox")
	if _, err := run(cleanup...); err != nil {
		t.Fatalf("confirmed-absence cleanup: %v (rejected=%v)", err, rejected)
	}
	if len(completions) != 1 || completions[0] != registered || len(rejected) != 0 {
		t.Fatalf("delete completion generations=%v registered=%q rejected=%v", completions, registered, rejected)
	}
	if after := readFixedProxmoxClaim(t, leaseID); after.RuntimeAdapterRegistrationID != registered || after.FixedCreateIntent.State != "released" {
		t.Fatalf("receipt lost its registration generation: %+v", after)
	}
}
