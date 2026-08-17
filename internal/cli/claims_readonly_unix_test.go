//go:build !windows

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestClaimsListReadableNonWritableStore(t *testing.T) {
	for _, test := range []struct {
		name      string
		malformed bool
		wantCode  int
	}{
		{name: "valid", wantCode: 0},
		{name: "malformed_partial", malformed: true, wantCode: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			writeClaimsListFixture(t, "cbx_valid.json", leaseClaim{LeaseID: "cbx_valid", Provider: "local-container"})
			if test.malformed {
				writeClaimsListRawFixture(t, "cbx_broken.json", []byte("{"))
			}

			stateDir, err := crabboxStateDir()
			if err != nil {
				t.Fatal(err)
			}
			claimsDir := filepath.Join(stateDir, "claims")
			entries, err := os.ReadDir(claimsDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if err := os.Chmod(filepath.Join(claimsDir, entry.Name()), 0o400); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(claimsDir, 0o500); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(stateDir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = os.Chmod(stateDir, 0o700)
				_ = os.Chmod(claimsDir, 0o700)
				for _, entry := range entries {
					_ = os.Chmod(filepath.Join(claimsDir, entry.Name()), 0o600)
				}
			})

			before := captureClaimsListState(t, stateDir)
			stdout, stderr, runErr := runClaimsList(t, "--json")
			assertClaimsExitCode(t, runErr, test.wantCode)
			if stderr != "" {
				t.Fatalf("stderr=%q", stderr)
			}
			var output localClaimsListOutput
			if err := json.Unmarshal([]byte(stdout), &output); err != nil {
				t.Fatalf("decode output: %v\n%s", err, stdout)
			}
			if len(output.Claims) != 1 || output.Claims[0].LeaseID != "cbx_valid" {
				t.Fatalf("partial claims=%#v", output.Claims)
			}
			wantProblems := 0
			if test.malformed {
				wantProblems = 1
			}
			if len(output.Problems) != wantProblems {
				t.Fatalf("problems=%#v", output.Problems)
			}
			after := captureClaimsListState(t, stateDir)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("read-only scan mutated state\nbefore=%#v\nafter=%#v", before, after)
			}
			if _, err := os.Stat(filepath.Join(stateDir, "claim-locks")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("claim-locks created or unreadable: %v", err)
			}
		})
	}
}

func TestClaimsListRejectsLargeSparseFilesWithoutCreatingState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	writeClaimsListFixture(t, "cbx_valid.json", leaseClaim{LeaseID: "cbx_valid", Provider: "local-container"})
	stateDir, err := crabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	claimsDir := filepath.Join(stateDir, "claims")
	wantSizes := map[string]int64{
		"cbx_sparse_128m.json": 128 * 1024 * 1024,
		"cbx_sparse_1g.json":   1 * 1024 * 1024 * 1024,
	}
	for name, size := range wantSizes {
		path := filepath.Join(claimsDir, name)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, size); err != nil {
			t.Fatal(err)
		}
	}

	stdout, stderr, runErr := runClaimsList(t, "--json")
	assertClaimsExitCode(t, runErr, 2)
	if stderr != "" {
		t.Fatalf("stderr=%q", stderr)
	}
	var output localClaimsListOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout)
	}
	if len(output.Claims) != 1 || output.Claims[0].LeaseID != "cbx_valid" {
		t.Fatalf("partial claims=%#v", output.Claims)
	}
	wantProblems := []localClaimProblem{
		{File: "cbx_sparse_128m.json", Code: "claim_too_large", Message: "claim file exceeds the 1 MiB inventory limit"},
		{File: "cbx_sparse_1g.json", Code: "claim_too_large", Message: "claim file exceeds the 1 MiB inventory limit"},
	}
	if !reflect.DeepEqual(output.Problems, wantProblems) {
		t.Fatalf("problems=%#v want=%#v", output.Problems, wantProblems)
	}
	for name, wantSize := range wantSizes {
		info, err := os.Stat(filepath.Join(claimsDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != wantSize {
			t.Fatalf("%s size=%d want=%d", name, info.Size(), wantSize)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "claim-locks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("claim-locks created or unreadable: %v", err)
	}
}

func TestRuntimeClaimsSnapshotChecksFileTypeInsideLock(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateDir, err := crabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	claimsDir := filepath.Join(stateDir, "claims")
	if err := os.MkdirAll(claimsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(stateDir, "outside-claim.json")
	if err := os.WriteFile(target, []byte(`{"leaseID":"cbx_link"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(claimsDir, "cbx_link.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(claimsDir, "cbx_dir.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	snapshot, err := snapshotLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.claims) != 0 {
		t.Fatalf("claims=%#v", snapshot.claims)
	}
	for _, leaseID := range []string{"cbx_dir", "cbx_link"} {
		var fileErr *leaseClaimFileError
		if !errors.As(snapshot.invalid[leaseID], &fileErr) || fileErr.code != "non_regular_file" {
			t.Fatalf("invalid[%s]=%v", leaseID, snapshot.invalid[leaseID])
		}
		if _, err := os.Stat(filepath.Join(stateDir, "claim-locks", leaseID+".json.lock")); err != nil {
			t.Fatalf("runtime lock missing for %s: %v", leaseID, err)
		}
	}
}
