package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	if err := os.WriteFile(configPath, []byte("provider: backend-that-must-not-load\n"), 0o600); err != nil {
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

func TestClaimsListScansDoNotCreateLocksOrMutateState(t *testing.T) {
	for _, test := range []struct {
		name               string
		malformedJSON      bool
		whitespaceFilename bool
		wantCode           int
		wantProblemCode    string
	}{
		{name: "clean", wantCode: 0},
		{name: "malformed", malformedJSON: true, wantCode: 2, wantProblemCode: "invalid_json"},
		{name: "whitespace_filename", whitespaceFilename: true, wantCode: 2, wantProblemCode: "invalid_filename"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			writeClaimsListFixture(t, "cbx_valid.json", leaseClaim{LeaseID: "cbx_valid", Provider: "local-container"})
			if test.malformedJSON {
				writeClaimsListRawFixture(t, "cbx_broken.json", []byte("{"))
			}
			if test.whitespaceFilename {
				writeClaimsListFixture(t, " cbx_padded.json", leaseClaim{LeaseID: " cbx_padded"})
			}

			stateDir, err := crabboxStateDir()
			if err != nil {
				t.Fatal(err)
			}
			before := captureClaimsListState(t, stateDir)
			stdout, stderr, runErr := runClaimsList(t, "--json")
			assertClaimsExitCode(t, runErr, test.wantCode)
			if stderr != "" || !strings.Contains(stdout, `"leaseId":"cbx_valid"`) {
				t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
			}
			var output localClaimsListOutput
			if err := json.Unmarshal([]byte(stdout), &output); err != nil {
				t.Fatalf("decode output: %v\n%s", err, stdout)
			}
			if len(output.Claims) != 1 || output.Claims[0].LeaseID != "cbx_valid" {
				t.Fatalf("claims=%#v", output.Claims)
			}
			if test.wantProblemCode == "" {
				if len(output.Problems) != 0 {
					t.Fatalf("problems=%#v", output.Problems)
				}
			} else if len(output.Problems) != 1 || output.Problems[0].Code != test.wantProblemCode {
				t.Fatalf("problems=%#v want code=%s", output.Problems, test.wantProblemCode)
			}
			after := captureClaimsListState(t, stateDir)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("claims scan mutated state\nbefore=%#v\nafter=%#v", before, after)
			}
			if _, err := os.Stat(filepath.Join(stateDir, "claim-locks")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("claim-locks created or unreadable: %v", err)
			}
		})
	}
}

func TestClaimsListEnforcesInclusiveClaimFileSizeLimit(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	writeClaimsListFixture(t, "cbx_partial.json", leaseClaim{LeaseID: "cbx_partial", Provider: "local-container"})
	writeClaimsListRawFixture(t, "cbx_exact.json", sizedClaimsListJSON(t, "cbx_exact", int(maxLocalClaimInventoryFileBytes)))
	writeClaimsListRawFixture(t, "cbx_large_valid.json", sizedClaimsListJSON(t, "cbx_large_valid", int(maxLocalClaimInventoryFileBytes+1)))
	writeClaimsListRawFixture(t, "cbx_large_malformed.json", bytes.Repeat([]byte("{"), int(maxLocalClaimInventoryFileBytes+1)))

	stdout, stderr, err := runClaimsList(t, "--json")
	assertClaimsExitCode(t, err, 2)
	if stderr != "" {
		t.Fatalf("stderr=%q", stderr)
	}
	var output localClaimsListOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout)
	}
	if len(output.Claims) != 2 || output.Claims[0].LeaseID != "cbx_exact" || output.Claims[1].LeaseID != "cbx_partial" {
		t.Fatalf("partial claims=%#v", output.Claims)
	}
	wantProblems := []localClaimProblem{
		{File: "cbx_large_malformed.json", Code: "claim_too_large", Message: "claim file exceeds the 1 MiB inventory limit"},
		{File: "cbx_large_valid.json", Code: "claim_too_large", Message: "claim file exceeds the 1 MiB inventory limit"},
	}
	if !reflect.DeepEqual(output.Problems, wantProblems) {
		t.Fatalf("problems=%#v want=%#v", output.Problems, wantProblems)
	}

	human, humanStderr, humanErr := runClaimsList(t)
	assertClaimsExitCode(t, humanErr, 2)
	if humanStderr != "" {
		t.Fatalf("human stderr=%q", humanStderr)
	}
	for _, want := range []string{
		`leaseId: "cbx_exact"`,
		`leaseId: "cbx_partial"`,
		`"cbx_large_malformed.json": claim file exceeds the 1 MiB inventory limit (claim_too_large)`,
		`"cbx_large_valid.json": claim file exceeds the 1 MiB inventory limit (claim_too_large)`,
	} {
		if !strings.Contains(human, want) {
			t.Fatalf("human output missing %q:\n%s", want, human)
		}
	}
	stateDir, err := crabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "claim-locks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("claim-locks created or unreadable: %v", err)
	}
}

func TestClaimsListSizeLimitDoesNotChangeRuntimeClaimReader(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_runtime_large"
	writeClaimsListRawFixture(t, leaseID+".json", sizedClaimsListJSON(t, leaseID, int(maxLocalClaimInventoryFileBytes+1)))

	stdout, _, inventoryErr := runClaimsList(t, "--json")
	assertClaimsExitCode(t, inventoryErr, 2)
	var output localClaimsListOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Claims) != 0 || len(output.Problems) != 1 || output.Problems[0].Code != "claim_too_large" {
		t.Fatalf("inventory output=%#v", output)
	}
	stateDir, err := crabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "claim-locks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inventory created claim locks: %v", err)
	}

	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatalf("runtime reader rejected compatible large claim: %v", err)
	}
	if claim.LeaseID != leaseID {
		t.Fatalf("runtime claim=%#v", claim)
	}
	snapshot, err := snapshotLeaseClaims()
	if err != nil || len(snapshot.invalid) != 0 || len(snapshot.claims) != 1 || snapshot.claims[0].LeaseID != leaseID {
		t.Fatalf("runtime snapshot rejected compatible large claim: snapshot=%#v err=%v", snapshot, err)
	}
}

func TestLocalClaimInventoryBoundsUnknownOrGrowingRead(t *testing.T) {
	reader := &claimsListRepeatingReader{remaining: 1 * 1024 * 1024 * 1024}
	data, tooLarge, err := readLocalClaimInventoryData(reader)
	if err != nil {
		t.Fatal(err)
	}
	wantRead := maxLocalClaimInventoryFileBytes + 1
	if !tooLarge || int64(len(data)) != wantRead || reader.read != wantRead {
		t.Fatalf("tooLarge=%t bytes=%d sourceRead=%d want=%d", tooLarge, len(data), reader.read, wantRead)
	}
}

func TestClaimsListClaimStoreFailureIsNotMalformedPartialOutput(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateRoot)
	if err := os.WriteFile(filepath.Join(stateRoot, "crabbox"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runClaimsList(t, "--json")
	assertClaimsExitCode(t, err, 1)
	if stdout != "" {
		t.Fatalf("fatal store error wrote partial output: %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr=%q", stderr)
	}
}

func TestRuntimeClaimsSnapshotDoesNotCreateLocks(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	writeClaimsListFixture(t, "cbx_runtime.json", leaseClaim{LeaseID: "cbx_runtime", Provider: "local-container"})

	snapshot, err := snapshotLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.claims) != 1 || snapshot.claims[0].LeaseID != "cbx_runtime" {
		t.Fatalf("claims=%#v", snapshot.claims)
	}
	stateDir, err := crabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "claim-locks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime snapshot created lock state: %v", err)
	}
}

func TestLeaseClaimsSnapshotBoundaryHandling(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	writeClaimsListRawFixture(t, ".json", []byte(`{"leaseID":"ignored"}`))
	writeClaimsListFixture(t, " cbx_leading.json", leaseClaim{LeaseID: " cbx_leading"})
	writeClaimsListFixture(t, "cbx_trailing .json", leaseClaim{LeaseID: "cbx_trailing "})
	writeClaimsListFixture(t, "   .json", leaseClaim{LeaseID: "   "})
	writeClaimsListRawFixture(t, "ignored.txt", []byte("not a claim"))
	writeClaimsListFixture(t, "cbx_empty.json", leaseClaim{})
	writeClaimsListFixture(t, "cbx_mismatch.json", leaseClaim{LeaseID: "cbx_other"})
	writeClaimsListFixture(t, "cbx_skip.json", leaseClaim{LeaseID: "cbx_skip"})

	snapshot, err := snapshotLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.claims) != 1 || snapshot.claims[0].LeaseID != "cbx_skip" {
		t.Fatalf("claims=%#v", snapshot.claims)
	}
	wantInvalidCodes := map[string]string{
		"":              "invalid_filename",
		" cbx_leading":  "invalid_filename",
		"cbx_trailing ": "invalid_filename",
		"   ":           "invalid_filename",
		"cbx_empty":     "empty_lease_id",
		"cbx_mismatch":  "lease_id_mismatch",
	}
	for leaseID, wantCode := range wantInvalidCodes {
		var fileErr *leaseClaimFileError
		if !errors.As(snapshot.invalid[leaseID], &fileErr) || fileErr.code != wantCode {
			t.Fatalf("invalid[%q]=%v want code=%s", leaseID, snapshot.invalid[leaseID], wantCode)
		}
	}
	stateDir, err := crabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "claim-locks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime snapshot created lock state: %v", err)
	}

	if claim, exists, err := readLeaseClaimRuntimeSnapshotWithPresence("bad/name"); err != nil || exists || claim.LeaseID != "" {
		t.Fatalf("invalid ID read=(%#v, %t, %v)", claim, exists, err)
	}
	if claim, exists, err := readLeaseClaimRuntimeSnapshotWithPresence("cbx_missing"); err != nil || exists || claim.LeaseID != "" {
		t.Fatalf("missing read=(%#v, %t, %v)", claim, exists, err)
	}

	readCalls := 0
	readOnly, err := snapshotLeaseClaimsReadOnlyWithReader(func(_ string, _ string, _ os.FileInfo) (leaseClaim, bool, error) {
		readCalls++
		return leaseClaim{}, false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if readCalls != 3 || len(readOnly.claims) != 0 {
		t.Fatalf("read calls=%d snapshot=%#v", readCalls, readOnly)
	}
	for _, leaseID := range []string{"", " cbx_leading", "cbx_trailing ", "   "} {
		var fileErr *leaseClaimFileError
		if !errors.As(readOnly.invalid[leaseID], &fileErr) || fileErr.code != "invalid_filename" {
			t.Fatalf("read-only invalid[%q]=%v want code=invalid_filename", leaseID, readOnly.invalid[leaseID])
		}
	}
	claimsDir := filepath.Join(stateDir, "claims")
	dirInfo, err := os.Stat(claimsDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists, err := readLeaseClaimSnapshotWithPresence(claimsDir, "cbx_directory", dirInfo); err == nil || !exists {
		t.Fatalf("directory snapshot read=(exists=%t, err=%v)", exists, err)
	}

	sentinel := errors.New("sentinel")
	if !errors.Is(&leaseClaimFileError{code: "test", err: sentinel}, sentinel) {
		t.Fatal("lease claim file error did not unwrap")
	}

	brokenStateRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(brokenStateRoot, "crabbox"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", brokenStateRoot)
	if _, err := snapshotLeaseClaims(); err == nil {
		t.Fatal("runtime snapshot accepted an unreadable claims directory")
	}
}

func TestClaimsListConcurrentDeleteAndChangeProduceDeterministicProblems(t *testing.T) {
	var want localClaimsListOutput
	for iteration := 0; iteration < 2; iteration++ {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		writeClaimsListFixture(t, "cbx_change.json", leaseClaim{LeaseID: "cbx_change", Slug: "before"})
		writeClaimsListFixture(t, "cbx_delete.json", leaseClaim{LeaseID: "cbx_delete"})
		writeClaimsListFixture(t, "cbx_grow.json", leaseClaim{LeaseID: "cbx_grow"})
		writeClaimsListFixture(t, "cbx_valid.json", leaseClaim{LeaseID: "cbx_valid", Provider: "local-container"})

		reader := func(path, leaseID string, expected os.FileInfo) (leaseClaim, bool, error) {
			var mutate func() error
			switch leaseID {
			case "cbx_change":
				mutate = func() error {
					return os.WriteFile(path, []byte(`{"leaseID":"cbx_change","slug":"changed-during-scan"}`), 0o600)
				}
			case "cbx_delete":
				mutate = func() error { return os.Remove(path) }
			case "cbx_grow":
				mutate = func() error { return os.Truncate(path, maxLocalClaimInventoryFileBytes+1) }
			}
			if mutate != nil {
				result := make(chan error, 1)
				go func() { result <- mutate() }()
				if err := <-result; err != nil {
					t.Fatalf("concurrent mutation: %v", err)
				}
			}
			return readLeaseClaimSnapshotWithPresence(path, leaseID, expected)
		}

		snapshot, err := snapshotLeaseClaimsReadOnlyWithReader(reader)
		if err != nil {
			t.Fatal(err)
		}
		output := projectLocalClaims(snapshot)
		if len(output.Claims) != 1 || output.Claims[0].LeaseID != "cbx_valid" {
			t.Fatalf("partial claims=%#v", output.Claims)
		}
		if len(output.Problems) != 3 || len(output.Problems) > maxLocalClaimProblems {
			t.Fatalf("problems=%#v", output.Problems)
		}
		for i, problem := range output.Problems {
			if problem.Code != "read_error" || problem.Message != "claim file could not be read" {
				t.Fatalf("problem[%d]=%#v", i, problem)
			}
		}
		if iteration == 0 {
			want = output
		} else if !reflect.DeepEqual(output, want) {
			t.Fatalf("nondeterministic output\nfirst=%#v\nsecond=%#v", want, output)
		}
		stateDir, err := crabboxStateDir()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(stateDir, "claim-locks")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("claim-locks created or unreadable: %v", err)
		}
	}
}

func TestClaimsListReportsMalformedRecordsWithStablePartialOutput(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	writeClaimsListFixture(t, "cbx_zulu.json", leaseClaim{LeaseID: "cbx_zulu", Provider: "aws"})
	writeClaimsListFixture(t, "cbx_alpha.json", leaseClaim{LeaseID: "cbx_alpha", Provider: "hetzner"})
	writeClaimsListRawFixture(t, ".json", []byte(`{"leaseID":"ignored"}`))
	writeClaimsListFixture(t, " cbx_leading.json", leaseClaim{LeaseID: " cbx_leading"})
	writeClaimsListFixture(t, "cbx_trailing .json", leaseClaim{LeaseID: "cbx_trailing "})
	writeClaimsListFixture(t, "   .json", leaseClaim{LeaseID: "   "})
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
	wantCodeCounts := map[string]int{
		"invalid_filename":  4,
		"invalid_json":      2,
		"empty_lease_id":    1,
		"lease_id_mismatch": 1,
		"non_regular_file":  1,
	}
	if len(output.Problems) != 9 {
		t.Fatalf("problems=%#v", output.Problems)
	}
	gotCodeCounts := make(map[string]int)
	for i, problem := range output.Problems {
		if i > 0 && localClaimProblemLess(problem, output.Problems[i-1]) {
			t.Fatalf("problems not sorted: %#v", output.Problems)
		}
		if _, ok := wantCodeCounts[problem.Code]; !ok {
			t.Fatalf("unexpected problem=%#v", problem)
		}
		gotCodeCounts[problem.Code]++
		if problem.Code == "invalid_filename" && !strings.HasPrefix(problem.File, "sha256:") {
			t.Fatalf("invalid filename was not fingerprinted: %#v", problem)
		}
		if problem.Message == "" || len(problem.File) > maxLocalClaimProblemFileBytes {
			t.Fatalf("unbounded or empty problem=%#v", problem)
		}
	}
	if !reflect.DeepEqual(gotCodeCounts, wantCodeCounts) {
		t.Fatalf("problem counts=%v want=%v: %#v", gotCodeCounts, wantCodeCounts, output.Problems)
	}
	secondStdout, secondStderr, secondErr := runClaimsList(t, "--json")
	assertClaimsExitCode(t, secondErr, 2)
	if secondStderr != "" || secondStdout != stdout {
		t.Fatalf("nondeterministic output\nfirst stdout=%q\nsecond stdout=%q\nsecond stderr=%q", stdout, secondStdout, secondStderr)
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

func sizedClaimsListJSON(t *testing.T, leaseID string, size int) []byte {
	t.Helper()
	prefix := []byte(fmt.Sprintf(`{"leaseID":%q,"padding":"`, leaseID))
	suffix := []byte(`"}`)
	if size < len(prefix)+len(suffix) {
		t.Fatalf("claim fixture size %d is too small", size)
	}
	data := make([]byte, 0, size)
	data = append(data, prefix...)
	data = append(data, bytes.Repeat([]byte("x"), size-len(prefix)-len(suffix))...)
	data = append(data, suffix...)
	return data
}

type claimsListRepeatingReader struct {
	remaining int64
	read      int64
}

func (r *claimsListRepeatingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.remaining {
		n = r.remaining
	}
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.remaining -= n
	r.read += n
	return int(n), nil
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

type claimsListStateEntry struct {
	Mode os.FileMode
	Data string
}

func captureClaimsListState(t *testing.T, root string) map[string]claimsListStateEntry {
	t.Helper()
	state := make(map[string]claimsListStateEntry)
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entry := claimsListStateEntry{Mode: info.Mode()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry.Data = string(data)
		}
		state[rel] = entry
		return nil
	}); err != nil {
		t.Fatalf("capture state: %v", err)
	}
	return state
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
