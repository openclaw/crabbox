//go:build !windows

package blacksmith

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	_ "github.com/openclaw/crabbox/internal/providers/ssh"
)

func TestBlacksmithJobNoSyncAdmission(t *testing.T) {
	const freshID = "tbx_prewarm123"
	const existingID = "tbx_existing123"
	for _, tc := range []struct {
		name, provider, jobProvider, id, claimProvider, stop string
		dryRun, sync, supported                              bool
	}{
		{name: "fresh_never", provider: "blacksmith-testbox", stop: "never"},
		{name: "fresh_alias_success", provider: "blacksmith", stop: "success"},
		{name: "fresh_override", provider: "ssh", jobProvider: "blacksmith"},
		{name: "dry_run", provider: "blacksmith-testbox", dryRun: true},
		{name: "dry_override", provider: "ssh", jobProvider: "blacksmith", dryRun: true},
		{name: "existing_id_stop_always", provider: "blacksmith-testbox", id: existingID, stop: "always"},
		{name: "existing_slug_inferred", provider: "ssh", id: "existing-box"},
		{name: "dry_existing_id_inferred", provider: "ssh", id: existingID, dryRun: true},
		{name: "dry_existing_slug_inferred", provider: "ssh", id: "existing-box", dryRun: true},
		{name: "sync_control", provider: "blacksmith-testbox", stop: "never", sync: true},
		{name: "dry_sync_control", provider: "blacksmith", dryRun: true, sync: true},
		{name: "other_provider", provider: "ssh", dryRun: true, supported: true},
		{name: "override_away", provider: "blacksmith-testbox", jobProvider: "ssh", dryRun: true, supported: true},
		{name: "existing_override_away", provider: "blacksmith-testbox", jobProvider: "ssh", id: existingID, dryRun: true, supported: true},
		{name: "existing_override_to_blacksmith", provider: "ssh", jobProvider: "blacksmith", id: existingID, claimProvider: "ssh", dryRun: true},
		{name: "existing_claim_routes_away", provider: "blacksmith-testbox", id: existingID, claimProvider: "ssh", dryRun: true, supported: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stop := tc.stop
			if stop == "" {
				stop = "never"
			}
			config := fmt.Sprintf("provider: %s\nblacksmith:\n  org: example-org\n  workflow: testbox.yml\n  job: check\n  ref: main\njobs:\n  check:\n    provider: %q\n    noSync: %t\n    shell: true\n    command: echo ready\n    stop: %s\n", tc.provider, tc.jobProvider, !tc.sync, stop)
			repo, callsPath := blacksmithCallerFixture(t, config)
			claimProvider := tc.claimProvider
			if claimProvider == "" {
				claimProvider = blacksmithTestboxProvider
			}
			if err := core.ClaimLeaseForRepoProvider(existingID, "existing-box", claimProvider, repo, time.Minute, false); err != nil {
				t.Fatal(err)
			}
			before, err := readLeaseClaim(existingID)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"job", "run"}
			if tc.dryRun {
				args = append(args, "--dry-run")
			}
			if tc.id != "" {
				args = append(args, "--id", tc.id)
			}
			args = append(args, "check")
			var stdout, stderr bytes.Buffer
			runErr := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), args)
			calls, err := os.ReadFile(callsPath)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			after, err := readLeaseClaim(existingID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Error("job changed an existing claim")
			}
			fresh, err := readLeaseClaim(freshID)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.sync && !tc.supported {
				var exitErr ExitError
				if !core.AsExitError(runErr, &exitErr) || exitErr.Code != 2 {
					t.Errorf("job error=%v, want exit 2", runErr)
				}
				for _, want := range []string{`job "check"`, "--no-sync", "blacksmith-testbox", "not supported"} {
					if runErr == nil || !strings.Contains(runErr.Error(), want) {
						t.Errorf("diagnostic missing %q: %v", want, runErr)
					}
				}
				if stdout.Len() != 0 {
					t.Errorf("job admitted before rejection:\n%s", &stdout)
				}
			} else if runErr != nil {
				t.Fatalf("supported job rejected: %v\nstdout=%s stderr=%s", runErr, &stdout, &stderr)
			}
			if tc.sync && !tc.dryRun {
				if fresh.LeaseID != freshID {
					t.Errorf("sync control did not run and retain its lease: claim=%q stdout=%s", fresh.LeaseID, &stdout)
				}
				if lines := strings.Split(strings.TrimSpace(string(calls)), "\n"); len(lines) != 6 || lines[0] != "keygen" || !strings.HasPrefix(lines[1], "blacksmith testbox warmup ") || !strings.HasPrefix(lines[2], "blacksmith testbox status ") || !strings.HasPrefix(lines[3], "blacksmith testbox status ") || !strings.HasPrefix(lines[4], "blacksmith testbox status ") || !strings.HasPrefix(lines[5], "blacksmith testbox run ") {
					t.Errorf("sync control calls=%q, want keygen, warmup, exact status checks, run", calls)
				}
				return
			}
			if len(calls) != 0 || fresh.LeaseID != "" {
				t.Errorf("calls=%s retained claim=%q, want ZERO warmup/run/stop/keygen calls and no new claim", calls, fresh.LeaseID)
			}
			keyPath, err := testboxKeyPath(freshID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Dir(filepath.Dir(keyPath))); !os.IsNotExist(err) {
				t.Errorf("job created a lease key directory before admission: %v", err)
			}
			if tc.dryRun && (tc.sync || tc.supported) && !strings.Contains(stdout.String(), "crabbox run") {
				t.Errorf("supported job did not plan a run: %s", &stdout)
			}
		})
	}
}
