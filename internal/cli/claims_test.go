package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestClaimsListReadsTwoProvidersWithoutBackendOrCredentials(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("provider: hetzner\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("HCLOUD_TOKEN", "")
	t.Setenv("HETZNER_TOKEN", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")

	writeClaimsListFixture(t, "cbx_zulu.json", leaseClaim{
		LeaseID: "cbx_zulu", Provider: "aws", RepoRoot: "/repo/zulu", LastUsedAt: "2026-08-01T00:00:00Z",
	})
	writeClaimsListFixture(t, "cbx_alpha.json", leaseClaim{
		LeaseID: "cbx_alpha", Provider: "hetzner", RepoRoot: "/repo/alpha", LastUsedAt: "2026-08-02T00:00:00Z",
	})

	stdout, stderr, err := runClaimsList(t, "--json")
	if err != nil {
		t.Fatalf("claims list error=%v stderr=%q", err, stderr)
	}
	var output localClaimsListOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout)
	}
	if output.Version != 1 || output.Source != "local-claims" {
		t.Fatalf("metadata=%#v", output)
	}
	if len(output.Claims) != 2 || output.Claims[0].LeaseID != "cbx_alpha" || output.Claims[1].LeaseID != "cbx_zulu" {
		t.Fatalf("claims=%#v", output.Claims)
	}
	if len(output.Problems) != 0 {
		t.Fatalf("problems=%#v", output.Problems)
	}
}

func TestClaimsListKeepsStaleValidClaimAndLabelsHumanOutputUnverified(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	writeClaimsListFixture(t, "cbx_stale.json", leaseClaim{
		LeaseID:    "cbx_stale",
		Slug:       "old-lobster",
		Provider:   "local-container",
		RepoRoot:   "/repo/stale",
		ClaimedAt:  "2000-01-01T00:00:00Z",
		LastUsedAt: "2000-01-02T00:00:00Z",
	})

	stdout, stderr, err := runClaimsList(t)
	if err != nil {
		t.Fatalf("claims list error=%v stderr=%q", err, stderr)
	}
	for _, want := range []string{
		"unverified local state",
		"provider backends were not queried",
		`leaseId: "cbx_stale"`,
		`provider: "local-container"`,
		`repoRoot: "/repo/stale"`,
		`lastUsedAt: "2000-01-02T00:00:00Z"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("human output missing %q:\n%s", want, stdout)
		}
	}
}

func TestClaimsListReportsMalformedRecordsWithStablePartialOutput(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	writeClaimsListFixture(t, "cbx_zulu.json", leaseClaim{LeaseID: "cbx_zulu", Provider: "aws"})
	writeClaimsListFixture(t, "cbx_alpha.json", leaseClaim{LeaseID: "cbx_alpha", Provider: "hetzner"})
	writeClaimsListRawFixture(t, ".json", []byte(`{"leaseID":"ignored"}`))
	writeClaimsListRawFixture(t, "z-invalid.json", []byte(`{"token":"invalid-json-secret"`))
	writeClaimsListRawFixture(t, "y-invalid-utf8.json", append([]byte(`{"leaseID":"y-invalid-utf8","slug":"`), 0xff, '"', '}'))
	writeClaimsListRawFixture(t, "m-empty.json", []byte(`{"leaseID":""}`))
	writeClaimsListRawFixture(t, "a-mismatch.json", []byte(`{"leaseID":"token=payload-secret"}`))
	makeClaimsListDirectoryFixture(t, "n-non-regular.json")

	stdout, stderr, err := runClaimsList(t, "--json")
	assertClaimsExitCode(t, err, 2)
	if stderr != "" {
		t.Fatalf("stderr=%q", stderr)
	}
	if strings.Contains(stdout, "invalid-json-secret") || strings.Contains(stdout, "payload-secret") {
		t.Fatalf("malformed-record output leaked fixture content: %s", stdout)
	}
	var output localClaimsListOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout)
	}
	if got := []string{output.Claims[0].LeaseID, output.Claims[1].LeaseID}; !reflect.DeepEqual(got, []string{"cbx_alpha", "cbx_zulu"}) {
		t.Fatalf("claim order=%q", got)
	}
	wantCodes := map[string]bool{
		"invalid_filename":  false,
		"empty_lease_id":    false,
		"lease_id_mismatch": false,
		"non_regular_file":  false,
	}
	if len(output.Problems) != len(wantCodes)+2 {
		t.Fatalf("problems=%#v", output.Problems)
	}
	invalidJSONProblems := 0
	for i, problem := range output.Problems {
		if i > 0 && localClaimProblemLess(problem, output.Problems[i-1]) {
			t.Fatalf("problems not sorted: %#v", output.Problems)
		}
		if problem.Code == "invalid_json" {
			invalidJSONProblems++
		} else if _, ok := wantCodes[problem.Code]; !ok {
			t.Fatalf("unexpected problem=%#v", problem)
		} else {
			wantCodes[problem.Code] = true
		}
		if problem.Message == "" || len(problem.File) > maxLocalClaimProblemFileBytes {
			t.Fatalf("unbounded or empty problem=%#v", problem)
		}
	}
	for code, seen := range wantCodes {
		if !seen {
			t.Fatalf("missing problem code %s: %#v", code, output.Problems)
		}
	}
	if invalidJSONProblems != 2 {
		t.Fatalf("invalid JSON problems=%d want=2: %#v", invalidJSONProblems, output.Problems)
	}
}

func TestClaimsListJSONProjectionExcludesEveryPrivateSensitiveField(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	claim := leaseClaim{
		LeaseID:                             "cbx_sensitive",
		Revision:                            "sensitive-revision",
		Slug:                                "safe-slug",
		Provider:                            "external",
		CloudID:                             "sensitive-cloud-id",
		CloudNumericID:                      424242,
		CloudImmutableID:                    "sensitive-immutable-id",
		ProviderScope:                       "sensitive-provider-scope",
		StaticHost:                          "sensitive-static-host",
		StaticUser:                          "sensitive-static-user",
		StaticPort:                          "2244",
		StaticWorkRoot:                      "/sensitive/static/root",
		TargetOS:                            "windows",
		WindowsMode:                         "wsl2",
		Pond:                                "safe-pond",
		RepoRoot:                            "/safe/repo",
		ClaimedAt:                           "2026-08-01T00:00:00Z",
		LastUsedAt:                          "2026-08-02T00:00:00Z",
		IdleTimeoutSeconds:                  1800,
		TailscaleIPv4:                       "100.64.0.1",
		TailscaleFQDN:                       "sensitive.ts.net",
		TailscaleHostname:                   "sensitive-hostname",
		TailscaleTags:                       []string{"tag:sensitive"},
		TailscaleLoginURL:                   "https://login.example.test/?token=sensitive-login-token",
		TailscaleExitNode:                   "sensitive-exit-node",
		TailscaleExitLAN:                    true,
		SSHHost:                             "sensitive-ssh-host",
		SSHPort:                             2222,
		BridgeURL:                           "https://bridge.example.test/sensitive",
		CoordinatorRegistrationURL:          "https://coordinator.example.test/register/sensitive",
		RuntimeAdapterRegistrationID:        "sensitive-registration-id",
		RuntimeAdapterPendingRegistrationID: "sensitive-pending-registration-id",
		CacheVolumes:                        []string{"sensitive-cache-volume"},
		Labels:                              map[string]string{"secret": "sensitive-label"},
		FixedCreateIntent: &FixedCreateIntent{
			Version: 1, Fingerprint: "sensitive-fingerprint", ProviderScope: "sensitive-fixed-scope",
			Slug: "sensitive-fixed-slug", CreatedAt: "2026-08-03T00:00:00Z", State: "pending",
			Attempt: map[string]string{"credential": "sensitive-attempt"}, FailedAttempts: []string{"sensitive-failure"},
		},
	}
	writeClaimsListFixture(t, "cbx_sensitive.json", claim)

	stdout, stderr, err := runClaimsList(t, "--json")
	if err != nil {
		t.Fatalf("claims list error=%v stderr=%q", err, stderr)
	}
	for _, secret := range []string{
		"sensitive-revision", "sensitive-cloud-id", "sensitive-provider-scope", "sensitive-static-host",
		"sensitive-login-token", "sensitive-ssh-host", "sensitive-registration-id", "sensitive-label",
		"sensitive-fingerprint", "sensitive-attempt", "sensitive-failure",
	} {
		if strings.Contains(stdout, secret) {
			t.Fatalf("JSON leaked %q: %s", secret, stdout)
		}
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatal(err)
	}
	if got := sortedMapKeys(document); !reflect.DeepEqual(got, []string{"claims", "problems", "source", "version"}) {
		t.Fatalf("root keys=%q", got)
	}
	var claims []map[string]json.RawMessage
	if err := json.Unmarshal(document["claims"], &claims); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{
		"claimedAt", "idleTimeoutSeconds", "lastUsedAt", "leaseId", "pond", "provider", "repoRoot", "slug", "targetOS", "windowsMode",
	}
	if len(claims) != 1 || !reflect.DeepEqual(sortedMapKeys(claims[0]), wantKeys) {
		t.Fatalf("public claim keys=%q", sortedMapKeys(claims[0]))
	}
	allowlistedPrivateTags := map[string]bool{
		"leaseID": true, "slug": true, "provider": true, "repoRoot": true, "pond": true,
		"targetOS": true, "windowsMode": true, "claimedAt": true, "lastUsedAt": true, "idleTimeoutSeconds": true,
	}
	claimType := reflect.TypeOf(leaseClaim{})
	for i := 0; i < claimType.NumField(); i++ {
		name := strings.Split(claimType.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && !allowlistedPrivateTags[name] && strings.Contains(stdout, `"`+name+`"`) {
			t.Fatalf("JSON serialized sensitive private field %q: %s", name, stdout)
		}
	}
}

func TestClaimsListEmptyJSONHasStableArrays(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stdout, stderr, err := runClaimsList(t, "--json")
	if err != nil {
		t.Fatalf("claims list error=%v stderr=%q", err, stderr)
	}
	want := `{"version":1,"source":"local-claims","claims":[],"problems":[]}` + "\n"
	if stdout != want {
		t.Fatalf("output=%q want=%q", stdout, want)
	}
}

func TestClaimsListBoundsProblemEntries(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for i := 0; i < maxLocalClaimProblems+7; i++ {
		writeClaimsListRawFixture(t, fmt.Sprintf("cbx_broken_%03d.json", i), []byte("{"))
	}

	stdout, _, err := runClaimsList(t, "--json")
	assertClaimsExitCode(t, err, 2)
	var output localClaimsListOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Problems) != maxLocalClaimProblems {
		t.Fatalf("problems=%d want=%d", len(output.Problems), maxLocalClaimProblems)
	}
	last := output.Problems[len(output.Problems)-1]
	if last.Code != "problems_truncated" || last.File != "" || !strings.Contains(last.Message, "additional malformed claim files omitted") {
		t.Fatalf("truncation problem=%#v", last)
	}
}

func TestClaimsListHelpAndTopLevelHelpExposeLocalOnlyCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"--help"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "claims      Inspect unverified local lease claims without loading providers") {
		t.Fatalf("top-level help omitted claims command:\n%s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	err := app.Run(context.Background(), []string{"claims", "list", "--help"})
	assertClaimsExitCode(t, err, 0)
	if !strings.Contains(stderr.String(), "Usage of claims list:") || !strings.Contains(stderr.String(), "-json") {
		t.Fatalf("claims list help=%q", stderr.String())
	}
}

func runClaimsList(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	command := append([]string{"claims", "list"}, args...)
	err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), command)
	return stdout.String(), stderr.String(), err
}

func writeClaimsListFixture(t *testing.T, name string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeClaimsListRawFixture(t, name, data)
}

func writeClaimsListRawFixture(t *testing.T, name string, data []byte) {
	t.Helper()
	dir, err := crabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	claimsDir := filepath.Join(dir, "claims")
	if err := os.MkdirAll(claimsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claimsDir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func makeClaimsListDirectoryFixture(t *testing.T, name string) {
	t.Helper()
	dir, err := crabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "claims", name), 0o700); err != nil {
		t.Fatal(err)
	}
}

func assertClaimsExitCode(t *testing.T, err error, want int) {
	t.Helper()
	if want == 0 && err == nil {
		return
	}
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != want {
		t.Fatalf("exit error=%v want code=%d", err, want)
	}
}

func localClaimProblemLess(a, b localClaimProblem) bool {
	return a.File < b.File || (a.File == b.File && a.Code < b.Code)
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
