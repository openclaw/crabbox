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
