package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testReadyPoolIdentity(t *testing.T, repo, ref, commit, fingerprint string) CoordinatorReadyPoolIdentityV1 {
	t.Helper()
	seed, err := readyPoolSeedDigest(repo, ref, commit, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	return CoordinatorReadyPoolIdentityV1{
		Schema: readyPoolIdentitySchemaV1,
		Image: CoordinatorReadyPoolImageIdentity{
			Provider: "aws", Scope: "us-east-1", ID: "ami-0123456789abcdef0",
		},
		Architecture: "amd64", SeedDigest: seed, CacheCompatibility: "node-22-pnpm-10",
	}
}

func writeTestReadyPoolIdentity(t *testing.T, identity CoordinatorReadyPoolIdentityV1) string {
	t.Helper()
	encoded, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pool-identity.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadyPoolIdentityValidationFailsClosed(t *testing.T) {
	identity := testReadyPoolIdentity(t, "example-org/my-app", "main", "abc123", "setup-v2")
	if err := validateReadyPoolIdentity(identity); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*CoordinatorReadyPoolIdentityV1)
	}{
		{"unknown schema", func(value *CoordinatorReadyPoolIdentityV1) { value.Schema += "/future" }},
		{"noncanonical architecture", func(value *CoordinatorReadyPoolIdentityV1) { value.Architecture = "x86_64" }},
		{"missing provider", func(value *CoordinatorReadyPoolIdentityV1) { value.Image.Provider = "" }},
		{"missing scope", func(value *CoordinatorReadyPoolIdentityV1) { value.Image.Scope = "" }},
		{"missing image", func(value *CoordinatorReadyPoolIdentityV1) { value.Image.ID = "" }},
		{"missing cache compatibility", func(value *CoordinatorReadyPoolIdentityV1) { value.CacheCompatibility = " " }},
		{"invalid digest", func(value *CoordinatorReadyPoolIdentityV1) { value.SeedDigest = "sha256:nope" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := identity
			tc.mutate(&changed)
			if err := validateReadyPoolIdentity(changed); err == nil {
				t.Fatal("invalid identity accepted")
			}
		})
	}
	loaded, err := loadReadyPoolIdentity(writeTestReadyPoolIdentity(t, identity))
	if err != nil || !readyPoolIdentitiesEqual(loaded, identity) {
		t.Fatalf("identity round trip=%#v err=%v", loaded, err)
	}
}

func TestReadyPoolSeedDigestCrossLanguageVectors(t *testing.T) {
	for _, tc := range []struct {
		name, repo, ref, commit, fingerprint, want string
	}{
		{
			name: "basic", repo: "example-org/my-app", ref: "main", commit: "abc123", fingerprint: "setup-v2",
			want: "sha256:8b76ec429b7e084f6af6c6a2de4be7faf09f872c892513d4ce97d2f055e44e20",
		},
		{
			name: "unicode and separators", repo: "example<org>&/my-app", ref: "refs/heads/line\u2028sep\u2029end",
			commit: "café-東京", fingerprint: "finger<&>λ",
			want: "sha256:b6cf4e2e23e212fa08223dfd085f0acc58b989ab9f343d0ba6a845def25ffc70",
		},
		{name: "empty", want: "sha256:ca20f3f91bdd0a8643698abb508aa749da01297641101331aab950efd75705c8"},
		{
			name: "long", repo: "example-org/my-app", ref: "refs/heads/" + strings.Repeat("r", 500),
			commit: strings.Repeat("c", 40), fingerprint: strings.Repeat("f", 700),
			want: "sha256:9b2ffeb6f40c6883520444f395c7ebe7979a7899a98027d8371abafe132cf159",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readyPoolSeedDigest(tc.repo, tc.ref, tc.commit, tc.fingerprint)
			if err != nil || got != tc.want {
				t.Fatalf("seed=%q err=%v want=%q", got, err, tc.want)
			}
		})
	}
	if _, err := readyPoolSeedDigest("", strings.Repeat("r", readyPoolSeedFieldMaxBytes+1), "", ""); err == nil {
		t.Fatal("oversized seed accepted")
	}
	if _, err := readyPoolSeedDigest(string([]byte{0xff}), "", "", ""); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestReadyPoolIdentityRejectsMismatchedLeaseEvidence(t *testing.T) {
	identity := testReadyPoolIdentity(t, "", "", "", "")
	lease := CoordinatorLease{
		Provider: "aws", Region: "us-east-1", TargetOS: targetLinux, Architecture: "amd64",
		Image: &CoordinatorLeaseImage{ID: identity.Image.ID, Provider: "aws", Region: "us-east-1"},
	}
	if err := readyPoolIdentityMatchesLease(identity, lease); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*CoordinatorLease){
		func(value *CoordinatorLease) { value.Architecture = "arm64" },
		func(value *CoordinatorLease) { value.Image = nil },
		func(value *CoordinatorLease) { value.Region = "eu-west-1" },
		func(value *CoordinatorLease) { value.Provider = "azure" },
	} {
		changed := lease
		mutate(&changed)
		if err := readyPoolIdentityMatchesLease(identity, changed); err == nil {
			t.Fatalf("mismatched lease accepted: %#v", changed)
		}
	}
}

func TestReadyPoolIdentityGeneratorUsesCoordinatorEvidence(t *testing.T) {
	identity := testReadyPoolIdentity(t, "example-org/my-app", "main", "abc123", "setup-v2")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/ready-pools/builders/identity" {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		var input map[string]any
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if input["leaseID"] != "cbx_000000000001" || input["cacheCompatibility"] != identity.CacheCompatibility {
			t.Fatalf("generation input=%#v", input)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"identity": identity})
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "local-test-token")
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run(context.Background(), []string{
		"pool", "identity", "builders", "--id", "cbx_000000000001", "--repo", "example-org/my-app", "--ref", "main",
		"--commit", "abc123", "--fingerprint", "setup-v2", "--cache-compatibility", identity.CacheCompatibility,
	})
	if err != nil {
		t.Fatal(err)
	}
	var generated CoordinatorReadyPoolIdentityV1
	if err := json.Unmarshal(stdout.Bytes(), &generated); err != nil || !readyPoolIdentitiesEqual(generated, identity) {
		t.Fatalf("generated=%#v err=%v", generated, err)
	}
}

func TestTypedReadyPoolUnsupportedNeverFallsBack(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/v1/ready-pools/builders/borrow-identity" {
			t.Fatalf("typed request fell back to %s", request.URL.Path)
		}
		http.NotFound(w, request)
	}))
	defer server.Close()
	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	_, err := client.BorrowTypedReadyPoolLease(context.Background(), "builders", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "typed ready pools are unsupported") || requests != 1 {
		t.Fatalf("requests=%d error=%v", requests, err)
	}
}

func TestTypedReadyPoolEnsureNeverFallsBackToLegacyReconciliation(t *testing.T) {
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.URL.Path)
		http.NotFound(w, request)
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "local-test-token")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	repo, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	repoSlug := firstNonBlank(cfg.Actions.Repo, bestEffortGitHubRepoSlug(repo, cfg))
	input := readyPoolRunBorrowInput(cfg, repo, repoSlug)
	identity := testReadyPoolIdentity(t,
		readyPoolInputString(input, "repo"), readyPoolInputString(input, "ref"),
		readyPoolInputString(input, "commit"), readyPoolInputString(input, "fingerprint"),
	)
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err = app.readyPoolEnsure(context.Background(), []string{
		"builders", "--min-ready", "0", "--identity-file", writeTestReadyPoolIdentity(t, identity),
	})
	if err == nil || !strings.Contains(err.Error(), "typed ready pools are unsupported") {
		t.Fatalf("typed ensure error=%v", err)
	}
	if len(requests) != 1 || requests[0] != "/v1/ready-pools/builders/reconcile-identity" {
		t.Fatalf("typed ensure fell back: %#v", requests)
	}
	if strings.Contains(stderr.String(), "fallback") {
		t.Fatalf("typed ensure emitted legacy fallback: %s", stderr.String())
	}
}

func TestTypedPrewarmRejectsOldCoordinatorBeforeProvisioning(t *testing.T) {
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.URL.Path)
		http.NotFound(w, request)
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "local-test-token")
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.prewarm(context.Background(), []string{
		"--provider", "aws", "--pool", "builders", "--pool-cache-compatibility", "node-22", "--dry-run",
	})
	if err == nil || !strings.Contains(err.Error(), "typed ready pools are unsupported") {
		t.Fatalf("typed prewarm error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if len(requests) != 1 || requests[0] != "/v1/ready-pools/builders/identity" {
		t.Fatalf("typed prewarm contacted unexpected routes: %#v", requests)
	}
	if stdout.Len() != 0 {
		t.Fatalf("typed prewarm provisioned before support check: %s", stdout.String())
	}
}

func TestTypedReadyPoolBorrowDrainsMismatchedResponse(t *testing.T) {
	identity := testReadyPoolIdentity(t, "example-org/my-app", "main", "abc123", "")
	changed := identity
	changed.CacheCompatibility = "different-cache"
	drained := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/ready-pools/builders/borrow-identity":
			_ = json.NewEncoder(w).Encode(CoordinatorReadyPoolResponse{
				Entry: CoordinatorReadyPoolEntry{Key: "builders", LeaseID: "cbx_000000000001", BorrowToken: "borrow", Identity: &changed},
				Lease: CoordinatorLease{Provider: "aws", Region: "us-east-1", TargetOS: targetLinux, Architecture: "amd64",
					Image: &CoordinatorLeaseImage{ID: identity.Image.ID, Provider: "aws", Region: "us-east-1"}},
			})
		case "/v1/ready-pools/builders/return-identity":
			var input map[string]any
			_ = json.NewDecoder(request.Body).Decode(&input)
			drained = input["result"] == "drain"
			_ = json.NewEncoder(w).Encode(map[string]any{"entry": map[string]any{"state": "draining"}})
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	_, err := borrowValidatedTypedReadyPoolLease(context.Background(), &client, "builders", map[string]any{
		"repo": "example-org/my-app", "ref": "main", "commit": "abc123", "identity": identity,
	}, identity)
	if err == nil || !drained {
		t.Fatalf("mismatch error=%v drained=%t", err, drained)
	}
}

func TestPrewarmRejectsOrphanOrEmptyTypedFlagsBeforeConfiguration(t *testing.T) {
	for _, args := range [][]string{
		{"--pool-identity-file", "missing.json"},
		{"--pool-cache-compatibility", "node-22"},
		{"--pool", "builders", "--pool-identity-file", ""},
		{"--pool", "builders", "--pool-cache-compatibility", ""},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "invalid.yaml"))
			var stdout, stderr bytes.Buffer
			app := App{Stdout: &stdout, Stderr: &stderr}
			if err := app.prewarm(context.Background(), args); err == nil {
				t.Fatal("invalid typed flags accepted")
			}
			if stdout.Len() != 0 {
				t.Fatalf("provisioning started: %s", stdout.String())
			}
		})
	}
}

func TestReadyPoolIdentitySeedMismatchRejectsBeforeBorrow(t *testing.T) {
	identity := testReadyPoolIdentity(t, "example-org/my-app", "main", "abc123", "")
	client := CoordinatorClient{BaseURL: "http://127.0.0.1:1", Client: &http.Client{Timeout: time.Second}}
	_, err := borrowValidatedTypedReadyPoolLease(context.Background(), &client, "builders", map[string]any{
		"repo": "example-org/other", "ref": "main", "commit": "abc123", "identity": identity,
	}, identity)
	if err == nil || !strings.Contains(err.Error(), "seedDigest") {
		t.Fatalf("seed mismatch error=%v", err)
	}
}

func TestLegacyReadyPoolMatchingRejectsTypedEntries(t *testing.T) {
	identity := testReadyPoolIdentity(t, "", "", "", "")
	entry := CoordinatorReadyPoolEntry{
		State: "ready", ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339Nano), Identity: &identity,
	}
	if count := countReadyPoolEntries([]CoordinatorReadyPoolEntry{entry}, map[string]any{}); count != 0 {
		t.Fatalf("legacy fallback counted %d typed entries", count)
	}
	entry.Identity = nil
	if count := countReadyPoolEntries([]CoordinatorReadyPoolEntry{entry}, map[string]any{}); count != 1 {
		t.Fatalf("unchanged legacy entry count=%d", count)
	}
}

func TestReadyPoolIdentityProviderBinding(t *testing.T) {
	identity := testReadyPoolIdentity(t, "", "", "", "")

	t.Run("omitted selects identity provider", func(t *testing.T) {
		fs := newFlagSet("test", &bytes.Buffer{})
		provider := fs.String("provider", "", "")
		if err := parseFlags(fs, nil); err != nil {
			t.Fatal(err)
		}
		cfg := Config{}
		if err := bindReadyPoolIdentityProviderConfig(&cfg, fs, provider, identity); err != nil {
			t.Fatal(err)
		}
		if cfg.Provider != "aws" || cfg.providerSelectionSource != providerSelectionLeaseContext || *provider != "aws" {
			t.Fatalf("provider binding cfg=%+v flag=%q", cfg, *provider)
		}
	})

	t.Run("explicit mismatch", func(t *testing.T) {
		fs := newFlagSet("test", &bytes.Buffer{})
		provider := fs.String("provider", "", "")
		if err := parseFlags(fs, []string{"--provider", "gcp"}); err != nil {
			t.Fatal(err)
		}
		if err := bindReadyPoolIdentityProviderConfig(&Config{}, fs, provider, identity); err == nil {
			t.Fatal("explicit provider mismatch succeeded")
		}
	})

	t.Run("configured mismatch", func(t *testing.T) {
		cfg := Config{}
		setProviderSelection(&cfg, "gcp", providerSelectionUserConfig)
		if err := bindReadyPoolIdentityConfiguredProvider(&cfg, identity); err == nil {
			t.Fatal("configured provider mismatch succeeded")
		}
	})

	t.Run("request derives provider", func(t *testing.T) {
		input := map[string]any{}
		if err := bindReadyPoolIdentityProvider(input, identity); err != nil {
			t.Fatal(err)
		}
		if got := readyPoolInputString(input, "provider"); got != "aws" {
			t.Fatalf("provider=%q, want aws", got)
		}
	})
}

func TestReadyPoolReturnDrainsUnavailableIdentity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		result     string
		identity   string
		missing    bool
		status     int
		wantErr    string
		wantResult string
	}{
		{name: "malformed ready drains", result: "ready", identity: `{`, wantErr: "decode ready-pool identity", wantResult: "drain"},
		{name: "missing ready drains", result: "ready", missing: true, wantErr: "read ready-pool identity", wantResult: "drain"},
		{name: "explicit drain skips identity", result: "drain", identity: `{`, wantResult: "drain"},
		{name: "explicit release skips identity", result: "release", identity: `{`, wantResult: "release"},
		{name: "cleanup failure joins original error", result: "ready", identity: `{`, status: http.StatusInternalServerError, wantErr: "decode ready-pool identity", wantResult: "drain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			var received map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
					t.Fatal(err)
				}
				if tc.status != 0 {
					http.Error(w, "cleanup failed", tc.status)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"entry":{"key":"builders","leaseID":"cbx_123","state":"draining"}}`))
			}))
			defer server.Close()
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "local-test-token")

			identityPath := filepath.Join(t.TempDir(), "identity.json")
			if tc.missing {
				identityPath = filepath.Join(t.TempDir(), "missing.json")
			} else if err := os.WriteFile(identityPath, []byte(tc.identity), 0o600); err != nil {
				t.Fatal(err)
			}
			err := (App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).readyPoolReturn(
				context.Background(),
				[]string{"builders", "--id", "cbx_123", "--result", tc.result, "--reason", "caller cleanup", "--identity-file", identityPath},
			)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error=%v, want %q", err, tc.wantErr)
			}
			if tc.status != 0 && (err == nil || !strings.Contains(err.Error(), "cleanup failed")) {
				t.Fatalf("joined error=%v", err)
			}
			if received["result"] != tc.wantResult {
				t.Fatalf("result=%v, want %q", received["result"], tc.wantResult)
			}
			if tc.result == "ready" && received["identity"] != nil {
				t.Fatalf("invalid identity was sent: %#v", received["identity"])
			}
		})
	}
}
