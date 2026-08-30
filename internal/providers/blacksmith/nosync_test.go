package blacksmith

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestBlacksmithRunNoSyncContract(t *testing.T) {
	const leaseID = "tbx_nosync123"
	for _, tc := range []struct {
		name string
		id   string
		slug string
	}{
		{name: "acquire"},
		{name: "reuse_id", id: leaseID},
		{name: "reuse_slug", id: "blue-lobster", slug: "blue-lobster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []struct {
				name   string
				noSync bool
			}{
				{name: "sync_control"},
				{name: "reject_no_sync", noSync: true},
			} {
				t.Run(mode.name, func(t *testing.T) {
					dirs := testutil.IsolateUserDirs(t)
					t.Setenv("TMPDIR", dirs.Root)
					t.Setenv("CRABBOX_ENV_ALLOW", "")
					t.Setenv("CRABBOX_BLACKSMITH_SYNC_TIMEOUT_MS", "0")
					t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
					t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
					repo := Repo{Name: "my-app", Root: t.TempDir()}
					t.Chdir(repo.Root)
					if output, err := exec.Command("git", "init", "--quiet").CombinedOutput(); err != nil {
						t.Fatalf("git init: %v\n%s", err, output)
					}
					if err := os.WriteFile(filepath.Join(repo.Root, "README.md"), []byte("sync fixture\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					if hidden, err := core.GitCheckoutHasHiddenOmissions(repo.Root); err != nil || hidden {
						t.Fatalf("fixture must allow delegated sync: hidden=%t err=%v", hidden, err)
					}
					if tc.id != "" {
						testOwnedBlacksmithClaim(t, leaseID, firstNonBlank(tc.slug, "jade-krill"), repo.Root)
					}
					before, err := readLeaseClaim(leaseID)
					if err != nil {
						t.Fatal(err)
					}

					var stderr bytes.Buffer
					var operations []string
					var claimDuringRun core.LeaseClaim
					runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
						if req.Name != "blacksmith" || len(req.Args) < 2 || req.Args[0] != "testbox" {
							t.Fatalf("unexpected fake command: %s %v", req.Name, req.Args)
						}
						operations = append(operations, req.Args[1])
						switch req.Args[1] {
						case "warmup":
							return LocalCommandResult{Stdout: leaseID + "\n"}, nil
						case "run":
							var err error
							claimDuringRun, err = readLeaseClaim(leaseID)
							if err != nil {
								t.Fatal(err)
							}
						}
						return LocalCommandResult{}, nil
					}}
					cfg := baseConfig()
					cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
					backend, err := (Provider{}).Configure(cfg, Runtime{
						Stdout: io.Discard, Stderr: &stderr, Clock: testClock{}, Exec: runner,
					})
					if err != nil {
						t.Fatal(err)
					}
					result, runErr := backend.(core.DelegatedRunBackend).Run(context.Background(), RunRequest{
						Repo: repo, ID: tc.id, Command: []string{"true"}, NoSync: mode.noSync,
					})
					after, err := readLeaseClaim(leaseID)
					if err != nil {
						t.Fatal(err)
					}

					if !mode.noSync {
						if runErr != nil {
							t.Fatalf("ordinary sync rejected: %v\nstderr: %s", runErr, &stderr)
						}
						wantOperations := []string{"run"}
						if tc.id == "" {
							wantOperations = []string{"warmup", "run", "stop"}
						}
						if !reflect.DeepEqual(operations, wantOperations) {
							t.Errorf("provider calls=%v, want %v", operations, wantOperations)
						}
						if result.ExitCode != 0 || result.LeaseID != leaseID || !result.SyncDelegated || result.Session == nil {
							t.Errorf("ordinary sync did not complete delegated run: %+v", result)
						}
						if claimDuringRun.LeaseID != leaseID {
							t.Error("ordinary sync did not claim the lease before execution")
						}
						if tc.id == "" && after.LeaseID != "" {
							t.Error("ordinary one-shot sync did not remove its claim after stop")
						}
						return
					}

					var exitErr ExitError
					if !core.AsExitError(runErr, &exitErr) || exitErr.Code != 2 {
						t.Errorf("Run error=%v result.ExitCode=%d, want exit 2", runErr, result.ExitCode)
					}
					message := stderr.String()
					if runErr != nil {
						message += runErr.Error()
					}
					for _, want := range []string{"--no-sync", "not supported", "blacksmith"} {
						if !strings.Contains(strings.ToLower(message), want) {
							t.Errorf("diagnostic missing %q: %q", want, message)
						}
					}
					if len(runner.calls) != 0 {
						t.Errorf("provider calls=%v, want ZERO calls before --no-sync rejection", operations)
					}
					if result.LeaseID != "" || result.Session != nil {
						t.Errorf("returned lease=%q session=%t, want no acquired or reused lease", result.LeaseID, result.Session != nil)
					}
					// One-shot cleanup must not conceal a claim created before the run.
					if claimDuringRun.LeaseID != "" && !reflect.DeepEqual(claimDuringRun, before) {
						t.Error("lease claim was created or refreshed before delegated run despite --no-sync")
					}
					if !reflect.DeepEqual(after, before) {
						t.Error("lease claim was created or refreshed despite --no-sync; want unchanged claim state")
					}
				})
			}
		})
	}
}
