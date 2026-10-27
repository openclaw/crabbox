package azuresandbox

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

const snapshotHTTPLease = "cbx_abcdef123456"
const snapshotHTTPGroup = "/subscriptions/subscription/resourceGroups/workers/sandboxGroups/sandboxes"
const snapshotHTTPBoxes = snapshotHTTPGroup + "/sandboxes"
const snapshotHTTPClone = snapshotHTTPBoxes + "/synthetic-clone"
const snapshotHTTPToken = "synthetic-http-proof-token"

// This is a synthetic Azure service, not Azure fault injection. The production
// CLI, lease journal, provider and HTTP client run unchanged; only newAPI supplies
// a synthetic credential and a loopback TLS endpoint. No RoundTripper is mocked.
// Service state is independent of the journal, which tests only read, never edit.
type snapshotHTTPService struct {
	mu sync.Mutex
	t  *testing.T

	binding          snapshotBinding
	box              *sandbox
	created          createRequest
	initialPolicy    snapshotLifecycle
	dropCreate       bool
	hideInventory    bool
	lifecycleOutcome string // respond, drop-applied, or drop-unapplied
	allowed          map[string]int
	records          []snapshotHTTPRecord
}

type snapshotHTTPRecord struct {
	method, path, outcome string
}

func (s *snapshotHTTPService) change(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn()
}

func (s *snapshotHTTPService) recording() []snapshotHTTPRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.records)
}

func (s *snapshotHTTPService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := snapshotHTTPRecord{method: r.Method, path: r.URL.Path}
	defer func() { s.records = append(s.records, record) }()
	respond := func(status int, value any) {
		record.outcome = fmt.Sprint(status)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if value != nil {
			if err := json.NewEncoder(w).Encode(value); err != nil {
				s.t.Error("synthetic service response write failed")
			}
		}
	}
	drop := func(outcome string) {
		// Hijack the real TLS-over-TCP connection after consuming the request.
		// Write no status line or headers: http.Client.Do must see transport loss,
		// not an invented HTTP failure or a custom transport's returned error.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			s.t.Error("synthetic service could not hijack connection")
			respond(http.StatusInternalServerError, nil)
			return
		}
		record.outcome = outcome
		if err := conn.Close(); err != nil {
			s.t.Error("synthetic service could not close connection")
		}
	}
	if r.TLS == nil || r.ProtoMajor != 1 || r.URL.Query().Get("api-version") != apiVersion || r.Header.Get("Authorization") != "Bearer "+snapshotHTTPToken {
		s.t.Error("request did not use the expected TLS, API version and synthetic authentication")
		respond(http.StatusBadRequest, nil)
		return
	}
	if r.Method != http.MethodGet {
		key := r.Method + " " + r.URL.Path
		if s.allowed[key] <= 0 {
			// Record even denied attempts. Negative tests require zero mutation
			// requests, not merely zero mutations applied by this safety fence.
			s.t.Errorf("prohibited mutation reached service: %s", key)
			respond(http.StatusForbidden, nil)
			return
		}
		s.allowed[key]--
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == snapshotHTTPGroup+"/snapshots/"+s.binding.SnapshotID:
		respond(http.StatusOK, snapshotMetadata{ID: s.binding.SnapshotID, SandboxID: s.binding.SourceSandboxID, CreatedAt: s.binding.CapturedAt})
	case r.Method == http.MethodGet && r.URL.Path == snapshotHTTPGroup+"/diskimages/"+s.binding.SourceDiskID:
		respond(http.StatusOK, map[string]any{"id": s.binding.SourceDiskID, "status": map[string]string{"state": "Ready"}})
	case r.Method == http.MethodGet && r.URL.Path == snapshotHTTPBoxes:
		boxes := []sandbox{}
		if s.box != nil && !s.hideInventory {
			boxes = append(boxes, *s.box)
		}
		respond(http.StatusOK, boxes)
	case r.Method == http.MethodPut && r.URL.Path == snapshotHTTPBoxes:
		var body createRequest
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.SourcesRef.Snapshot == nil || body.SourcesRef.Snapshot.ID != s.binding.SnapshotID || body.SourcesRef.DiskImage != nil || body.Lifecycle != nil || body.Resources != nil {
			s.t.Error("restore request did not preserve the snapshot-only contract")
			respond(http.StatusBadRequest, nil)
			return
		}
		for _, key := range []string{"crabbox_lease", "crabbox_fingerprint", "crabbox_attempt"} {
			if body.Labels[key] == "" || len(body.Labels[key]) > 63 {
				s.t.Error("restore request lacks bounded fresh ownership labels")
				respond(http.StatusBadRequest, nil)
				return
			}
		}
		s.created = body
		s.box = &sandbox{ID: "synthetic-clone", State: "Running", Labels: maps.Clone(body.Labels), Lifecycle: s.initialPolicy,
			SourcesRef: sourceReferences{Snapshot: &snapshotReference{ID: s.binding.SnapshotID}, DiskImage: &diskReference{ID: s.binding.SourceDiskID}}}
		if s.dropCreate {
			drop("dropped-after-apply")
			return
		}
		respond(http.StatusOK, s.box)
	case r.Method == http.MethodGet && r.URL.Path == snapshotHTTPClone:
		if s.box == nil {
			respond(http.StatusNotFound, nil)
		} else {
			respond(http.StatusOK, s.box)
		}
	case r.Method == http.MethodPost && r.URL.Path == snapshotHTTPClone+"/lifecycle":
		var body snapshotLifecycle
		if json.NewDecoder(r.Body).Decode(&body) != nil || body != s.binding.Lifecycle || s.box == nil {
			s.t.Error("lifecycle request differs from the pinned policy or clone is absent")
			respond(http.StatusBadRequest, nil)
			return
		}
		if s.lifecycleOutcome != "drop-unapplied" {
			s.box.Lifecycle = body
		}
		switch s.lifecycleOutcome {
		case "drop-applied":
			drop("dropped-after-apply")
		case "drop-unapplied":
			drop("dropped-without-apply")
		default:
			respond(http.StatusNoContent, nil)
		}
	case r.Method == http.MethodDelete && r.URL.Path == snapshotHTTPClone:
		s.box = nil
		respond(http.StatusNoContent, nil)
	default:
		s.t.Errorf("unexpected request reached service: %s %s", r.Method, r.URL.Path)
		respond(http.StatusNotFound, nil)
	}
}

type snapshotHTTPCLI struct {
	t          *testing.T
	service    *snapshotHTTPService
	server     *httptest.Server
	transports []*http.Transport
}

func newSnapshotHTTPCLI(t *testing.T) *snapshotHTTPCLI {
	t.Helper()
	testutil.IsolateUserDirs(t)
	t.Chdir(t.TempDir())
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	cfg := core.BaseConfig()
	cfg.Provider, cfg.TTL, cfg.IdleTimeout = providerName, 4*time.Hour, 15*time.Minute
	cfg.AzureSandbox = core.AzureSandboxConfig{Region: "westus3", Subscription: "subscription", ResourceGroup: "workers", Group: "sandboxes", Disk: "ubuntu"}
	b := &backend{cfg: cfg}
	binding := pinSnapshotFixture(t, b, nil)
	for key, value := range map[string]string{
		"CRABBOX_PROVIDER":                          providerName,
		"CRABBOX_AZURE_SANDBOX_REGION":              b.cfg.AzureSandbox.Region,
		"CRABBOX_AZURE_SANDBOX_SUBSCRIPTION_ID":     b.cfg.AzureSandbox.Subscription,
		"CRABBOX_AZURE_SANDBOX_RESOURCE_GROUP":      b.cfg.AzureSandbox.ResourceGroup,
		"CRABBOX_AZURE_SANDBOX_GROUP":               b.cfg.AzureSandbox.Group,
		"CRABBOX_AZURE_SANDBOX_CLIENT_ID":           "",
		"CRABBOX_AZURE_SANDBOX_DISK":                b.cfg.AzureSandbox.Disk,
		"CRABBOX_AZURE_SANDBOX_DISK_ID":             b.cfg.AzureSandbox.DiskID,
		"CRABBOX_AZURE_SANDBOX_SNAPSHOT_ID":         b.cfg.AzureSandbox.SnapshotID,
		"CRABBOX_AZURE_SANDBOX_SNAPSHOT_RECEIPT":    b.cfg.AzureSandbox.SnapshotReceipt,
		"CRABBOX_AZURE_SANDBOX_SNAPSHOT_GENERATION": b.cfg.AzureSandbox.SnapshotGeneration,
	} {
		t.Setenv(key, value)
	}
	h := &snapshotHTTPCLI{t: t, service: &snapshotHTTPService{t: t, binding: binding,
		allowed: map[string]int{"PUT " + snapshotHTTPBoxes: 1, "POST " + snapshotHTTPClone + "/lifecycle": 1}}}
	h.server = httptest.NewTLSServer(h.service)
	t.Cleanup(h.server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(h.server.Certificate())
	previous := newAPI
	newAPI = func(cfg core.Config, _ core.Runtime) (*client, error) {
		// Each CLI invocation gets a new backend (via Provider.Configure), client,
		// credential and connection pool. Only disk state and service state survive.
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
		h.transports = append(h.transports, transport)
		credential := credentialFunc(func(_ context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
			if !slices.Equal(opts.Scopes, []string{tokenScope}) {
				return azcore.AccessToken{}, fmt.Errorf("unexpected synthetic token scope")
			}
			return azcore.AccessToken{Token: snapshotHTTPToken, ExpiresOn: time.Now().Add(time.Hour)}, nil
		})
		c, err := newClient(cfg.AzureSandbox.Region, cfg.AzureSandbox.Subscription, cfg.AzureSandbox.ResourceGroup, cfg.AzureSandbox.Group, credential, transport)
		if err == nil {
			c.endpoint, c.http.Timeout = h.server.URL, 10*time.Second
		}
		return c, err
	}
	t.Cleanup(func() {
		newAPI = previous
		for _, transport := range h.transports {
			transport.CloseIdleConnections()
		}
	})
	return h
}

func (h *snapshotHTTPCLI) invoke(command, wantError string) {
	h.t.Helper()
	args := []string{command, "--provider", providerName}
	if command != "stop" {
		args = append(args, "--network", "public", "--tailscale=false")
	}
	switch command {
	case "warmup":
		args = append(args, "--class", "small", "--ttl", "4h", "--idle-timeout", "15m", "--lease-id", snapshotHTTPLease, "--keep")
	case "run":
		args = append(args, "--id", snapshotHTTPLease, "--keep", "--no-sync", "--", "true")
	case "stop":
		args = append(args, "--id", snapshotHTTPLease)
	default:
		h.t.Fatal("unknown test command")
	}
	start := len(h.service.recording())
	clients := len(h.transports)
	var stdout, stderr bytes.Buffer
	app := core.App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader("")}
	ctx, cancel := context.WithTimeout(h.t.Context(), 15*time.Second)
	defer cancel()
	err := app.Run(ctx, args)
	for _, transport := range h.transports[clients:] {
		transport.CloseIdleConnections()
	}
	for _, r := range h.service.recording()[start:] {
		h.t.Logf("HTTP %s %s -> %s", r.method, r.path, r.outcome)
	}
	public := fmt.Sprint(err) + stdout.String() + stderr.String()
	for _, forbidden := range []string{snapshotHTTPToken, h.server.URL, "Authorization:", "Bearer "} {
		if strings.Contains(public, forbidden) {
			h.t.Fatal("public command diagnostic exposed transport authentication or endpoint")
		}
	}
	if (wantError == "" && err != nil) || (wantError != "" && (err == nil || !strings.Contains(err.Error(), wantError))) {
		h.t.Fatalf("%s: unexpected result: %v; stderr=%s", command, err, stderr.String())
	}
	if len(h.transports) <= clients {
		h.t.Fatal("invocation did not reconstruct the provider HTTP client")
	}
	h.t.Logf("CLI %s -> %v", command, err)
}

func snapshotHTTPUncertain(operation string) string {
	return "ACA Sandbox " + operation + " request failed: transport outcome unknown (category=connection-closed)"
}

func readSnapshotHTTPClaim(t *testing.T) core.LeaseClaim {
	t.Helper()
	claim, err := core.ReadLeaseClaim(snapshotHTTPLease)
	if err != nil || claim.LeaseID != snapshotHTTPLease || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.Journal == nil {
		t.Fatalf("durable claim missing or invalid: %v", err)
	}
	i := claim.FixedCreateIntent
	if i.State != "released" && (i.Attempt["nonce"] == "" || i.Attempt["expires_at"] == "" || i.Attempt["submission"] != "submitted") {
		t.Fatal("claim lost admitted attempt identity")
	}
	t.Logf("persisted lease=%s cloud=%q immutable=%q state=%s phase=%s nonce=%s deadline=%s policySubmitted=%q",
		claim.LeaseID, claim.CloudID, claim.CloudImmutableID, i.State, i.Journal.Phase,
		i.Attempt["nonce"], i.Attempt["expires_at"], i.Attempt["snapshot_policy_submitted"])
	return claim
}

func assertSnapshotHTTPIdentity(t *testing.T, before, after core.LeaseClaim) {
	t.Helper()
	if before.LeaseID != after.LeaseID || before.Slug != after.Slug || before.ProviderScope != after.ProviderScope || before.FixedCreateIntent.Fingerprint != after.FixedCreateIntent.Fingerprint {
		t.Fatal("replay changed the durable lease identity")
	}
	for _, key := range []string{"nonce", "expires_at", "submission", "snapshot_id", "source_disk_id"} {
		if before.FixedCreateIntent.Attempt[key] != after.FixedCreateIntent.Attempt[key] {
			t.Fatalf("replay changed persisted %s", key)
		}
	}
}

func (h *snapshotHTTPCLI) assertMutations(creates, policies, deletes int) {
	h.t.Helper()
	got := map[string]int{}
	for _, r := range h.service.recording() {
		if r.method != http.MethodGet {
			got[r.method+" "+r.path]++
		}
	}
	want := map[string]int{"PUT " + snapshotHTTPBoxes: creates}
	if policies != 0 {
		want["POST "+snapshotHTTPClone+"/lifecycle"] = policies
	}
	if deletes != 0 {
		want["DELETE "+snapshotHTTPClone] = deletes
	}
	if !maps.Equal(got, want) {
		h.t.Fatalf("mutation recording differs: got=%v want=%v", got, want)
	}
	h.t.Logf("mutations create=%d lifecycle=%d delete=%d resume=0 exec=0 other=0", creates, policies, deletes)
}

func (h *snapshotHTTPCLI) stopOwned(policies int) {
	h.t.Helper()
	h.service.change(func() { h.service.allowed["DELETE "+snapshotHTTPClone] = 1 })
	h.invoke("stop", "")
	h.invoke("stop", "")
	claim := readSnapshotHTTPClaim(h.t)
	if claim.FixedCreateIntent.State != "released" || claim.FixedCreateIntent.Journal.Phase != "released" {
		h.t.Fatal("owned clone cleanup did not persist release")
	}
	h.service.change(func() {
		if h.service.box != nil {
			h.t.Error("owned clone survived successful cleanup")
		}
	})
	h.assertMutations(1, policies, 1)
}

func TestSnapshotHTTPCreateResponseLoss(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		t.Run(fmt.Sprintf("delayed-inventory=%t", delayed), func(t *testing.T) {
			h := newSnapshotHTTPCLI(t)
			h.service.change(func() {
				h.service.dropCreate, h.service.hideInventory = true, delayed
			})
			h.invoke("warmup", snapshotHTTPUncertain("create"))
			before := readSnapshotHTTPClaim(t)
			if before.CloudID != "" || before.CloudImmutableID != "" || before.FixedCreateIntent.State != "prepared" || before.FixedCreateIntent.Journal.Phase != "submitting" {
				t.Fatal("lost create response invented a completed allocation")
			}
			h.service.change(func() {
				if h.service.box == nil || h.service.created.Labels["crabbox_lease"] != before.LeaseID || h.service.created.Labels["crabbox_attempt"] != before.FixedCreateIntent.Attempt["nonce"] || h.service.created.Labels["crabbox_fingerprint"] != fingerprintLabel(before.FixedCreateIntent.Fingerprint) {
					t.Error("remote create was not applied with the persisted attempt")
				}
			})
			if delayed {
				for range 2 {
					h.invoke("warmup", "unresolved or missing resource; retain its claim for recovery")
					h.invoke("stop", "absence is unverified; claim retained")
					after := readSnapshotHTTPClaim(t)
					if !reflect.DeepEqual(before, after) {
						t.Fatal("empty inventory changed uncertain custody")
					}
					h.assertMutations(1, 0, 0)
				}
				h.service.change(func() { h.service.hideInventory = false })
			}
			h.invoke("warmup", "")
			after := readSnapshotHTTPClaim(t)
			assertSnapshotHTTPIdentity(t, before, after)
			if after.CloudID != "synthetic-clone" || after.CloudImmutableID != after.CloudID || after.FixedCreateIntent.State != "acquired" {
				t.Fatal("replay did not adopt the single remotely applied clone")
			}
			h.invoke("warmup", "")
			assertSnapshotHTTPIdentity(t, after, readSnapshotHTTPClaim(t))
			h.assertMutations(1, 1, 0)
			h.stopOwned(1)
		})
	}
}

func TestSnapshotHTTPLifecycleResponseLoss(t *testing.T) {
	for _, outcome := range []string{"drop-applied", "drop-unapplied"} {
		t.Run(outcome, func(t *testing.T) {
			h := newSnapshotHTTPCLI(t)
			h.service.change(func() { h.service.lifecycleOutcome = outcome })
			h.invoke("warmup", snapshotHTTPUncertain("snapshot-lifecycle"))
			before := readSnapshotHTTPClaim(t)
			if before.CloudID != "synthetic-clone" || before.FixedCreateIntent.State != "prepared" || before.FixedCreateIntent.Attempt["snapshot_policy_submitted"] != "true" {
				t.Fatal("lost lifecycle response did not retain bound, uncompleted custody")
			}
			want := ""
			if outcome == "drop-unapplied" {
				want = "ACA snapshot lifecycle update remains uncertain; reconcile or release this lease"
			}
			for range 2 {
				h.invoke("warmup", want)
				after := readSnapshotHTTPClaim(t)
				assertSnapshotHTTPIdentity(t, before, after)
				if (after.FixedCreateIntent.State == "acquired") != (outcome == "drop-applied") || after.CloudID != before.CloudID || after.FixedCreateIntent.Attempt["snapshot_policy_submitted"] != "true" {
					t.Fatal("replay did not reconcile only from matching lifecycle readback")
				}
				h.assertMutations(1, 1, 0)
			}
			h.stopOwned(1)
		})
	}
}

func TestSnapshotHTTPForeignOwnershipFencesMutations(t *testing.T) {
	for _, prior := range []string{"acquired", "drop-applied", "drop-unapplied"} {
		for _, label := range []string{"crabbox_lease", "crabbox_fingerprint", "crabbox_attempt"} {
			t.Run(prior+"/"+label, func(t *testing.T) {
				h := newSnapshotHTTPCLI(t)
				// Start with an acquired claim and no prior lifecycle submission.
				// This makes the ordinary replay eligible to set a mismatched policy
				// if (and only if) remote ownership still matches.
				h.service.change(func() { h.service.initialPolicy = h.service.binding.Lifecycle })
				h.invoke("warmup", "")
				initial := readSnapshotHTTPClaim(t)
				if initial.FixedCreateIntent.State != "acquired" || initial.FixedCreateIntent.Attempt["snapshot_policy_submitted"] != "" {
					t.Fatal("setup did not acquire without a lifecycle submission")
				}
				policies := 0
				if prior != "acquired" {
					h.service.change(func() {
						h.service.box.Lifecycle = snapshotLifecycle{}
						h.service.lifecycleOutcome = prior
					})
					h.invoke("warmup", snapshotHTTPUncertain("snapshot-lifecycle"))
					policies = 1
				}
				before := readSnapshotHTTPClaim(t)
				assertSnapshotHTTPIdentity(t, initial, before)
				h.service.change(func() {
					// Alter only the synthetic service's authority, never the local
					// claim. The resource ID and the other two ownership labels stay.
					h.service.box.Labels[label] = "foreign-owner"
					if prior != "drop-applied" {
						h.service.box.Lifecycle = snapshotLifecycle{}
					}
					h.service.allowed = map[string]int{}
				})
				for _, operation := range []struct{ command, state string }{
					{"warmup", "Running"}, {"warmup", "Suspended"},
					{"run", "Running"}, {"run", "Suspended"}, {"stop", "Suspended"},
				} {
					// Exercise both direct exec/lifecycle and activation paths. An
					// applied policy also must not mask foreign ownership on replay.
					h.service.change(func() { h.service.box.State = operation.state })
					t.Logf("remote fault label=%s prior=%s state=%s", label, prior, operation.state)
					start := len(h.service.recording())
					h.invoke(operation.command, "ACA Sandbox ownership does not match the exact lease attempt")
					reads := h.service.recording()[start:]
					if len(reads) == 0 {
						t.Fatal("denial did not consult remote authority")
					}
					for _, r := range reads {
						if r.method != http.MethodGet || r.path != snapshotHTTPClone || r.outcome != "200" {
							t.Fatal("foreign ownership denial performed more than exact readback")
						}
					}
					after := readSnapshotHTTPClaim(t)
					if !reflect.DeepEqual(before, after) {
						t.Fatal("foreign ownership denial rewrote durable custody")
					}
					h.assertMutations(1, policies, 0)
				}
				// Deliberately do not restore labels or delete a foreign resource.
				// The loopback service disappears on test cleanup, not via the API.
			})
		}
	}
}
