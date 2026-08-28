package blacksmith

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestBlacksmithRunRejectsNoSyncBeforeActivity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake provider executable requires /bin/sh")
	}
	for _, entrypoint := range []string{"backend", "cli"} {
		for _, id := range []string{"tbx_owned", ""} {
			name := id
			if name == "" {
				name = "automatic-warmup"
			}
			t.Run(entrypoint+"/"+name, func(t *testing.T) {
				dirs := testutil.IsolateUserDirs(t)
				repo := t.TempDir()
				t.Chdir(repo)
				t.Setenv("CRABBOX_CONFIG", filepath.Join(repo, "config.yaml"))
				t.Setenv("CRABBOX_COORDINATOR", "")
				t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
				t.Setenv("CRABBOX_COORDINATOR_TOKEN_COMMAND", "")
				t.Setenv("CRABBOX_ENV_ALLOW", "")
				t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
				t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
				if err := os.WriteFile(filepath.Join(repo, "config.yaml"), []byte("provider: blacksmith-testbox\nblacksmith:\n  workflow: testbox.yml\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				logPath := filepath.Join(repo, "provider-calls")
				t.Setenv("CRABBOX_TEST_NO_SYNC_LOG", logPath)
				for name, script := range map[string]string{
					"git": "#!/bin/sh\nexit 1\n",
					"blacksmith": `#!/bin/sh
printf 'provider invoked\n' >> "$CRABBOX_TEST_NO_SYNC_LOG"
case "$*" in
  *'testbox warmup'*) printf 'ready tbx_created\n' ;;
  *'testbox run'*) printf 'unexpected command execution\n' ;;
esac
`,
				} {
					if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				t.Setenv("PATH", bin+":/usr/bin:/bin")
				if id != "" {
					if err := claimLeaseForRepoProvider(id, "owned", blacksmithTestboxProvider, repo, time.Hour, false); err != nil {
						t.Fatal(err)
					}
				}
				before := blacksmithNoSyncState(t, dirs.Root)
				var stdout, stderr bytes.Buffer
				command := "git status --short; git diff HEAD --"
				var err error
				if entrypoint == "cli" {
					args := []string{"run", "--provider", blacksmithTestboxProvider, "--no-sync", "--timing-json", "--timing-record", "off", "--shell"}
					if id != "" {
						args = append(args, "--id", id)
					}
					args = append(args, "--", command)
					err = (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args)
				} else {
					runner := &blacksmithFuncRunner{fn: func(LocalCommandRequest) (LocalCommandResult, error) {
						return LocalCommandResult{Stdout: "ready tbx_created\n"}, nil
					}}
					cfg := baseConfig()
					cfg.Blacksmith.Workflow = "testbox.yml"
					backend := newTestBlacksmithBackend(cfg, runner)
					backend.rt.Stdout, backend.rt.Stderr = &stdout, &stderr
					_, err = backend.Run(t.Context(), RunRequest{Repo: Repo{Root: repo}, ID: id, NoSync: true, TimingJSON: true, ShellMode: true, Command: []string{command}})
					if len(runner.calls) != 0 {
						t.Errorf("provider runner invoked %d times before rejection", len(runner.calls))
					}
				}
				var exitErr ExitError
				if !core.AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(err.Error(), "blacksmith-testbox") || !strings.Contains(err.Error(), "--no-sync is not supported") {
					t.Errorf("error=%v, want provider-specific unsupported --no-sync exit 2", err)
				}
				if _, err := os.Stat(logPath); !os.IsNotExist(err) {
					t.Errorf("provider subprocess invoked before rejection (log stat: %v)", err)
				}
				if after := blacksmithNoSyncState(t, dirs.Root); !reflect.DeepEqual(before, after) {
					t.Error("rejected run changed local claim/key state")
				}
				if stdout.Len() != 0 || stderr.Len() != 0 {
					t.Errorf("rejected run emitted execution/timing output: stdout=%q stderr=%q", stdout.String(), stderr.String())
				}
			})
		}
	}
}

func blacksmithNoSyncState(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		files[path] = string(data)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return files
}
