package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func machine0PrewarmFixture(t *testing.T, imageVersion string) string {
	t.Helper()
	clearCrabboxDoctorEnv(t)
	dirs := testutil.IsolateUserDirs(t)
	t.Chdir(dirs.Home)
	config := filepath.Join(dirs.Home, "config.yaml")
	if err := os.WriteFile(config, []byte("provider: machine0\nmachine0:\n  imageVersion: "+imageVersion+"\nactions:\n  workflow: hydrate.yml\n  job: hydrate\n  ref: main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", config)
	return filepath.Join(dirs.Home, "missing-machine0")
}

func TestMachine0PrewarmCreationFlagsStayOnWarmup(t *testing.T) {
	for _, joined := range []bool{false, true} {
		t.Run(map[bool]string{false: "split", true: "equals"}[joined], func(t *testing.T) {
			tool := machine0PrewarmFixture(t, "0")
			flags := []struct {
				name, value string
				creation    bool
			}{
				{"machine0-size", "gpu-h100-1", true},
				{"machine0-region", "us-east", true},
				{"machine0-image", "ubuntu", true},
				{"machine0-image-version", "7", true},
				{"machine0-desktop-image", "desktop-v2", true},
				{"machine0-key", "ci-key", true},
				{"machine0-cli", tool, false},
				{"machine0-work-root", "/tmp/fixture-work", false},
				{"machine0-release-policy", "suspend", false},
				{"machine0-create-timeout", "13m", false},
				{"machine0-poll-interval", "3s", false},
			}
			args := []string{"prewarm", "--dry-run", "--provider", "machine0", "--probe-command", "printf ready"}
			for _, flag := range flags {
				if joined {
					args = append(args, "--"+flag.name+"="+flag.value)
				} else {
					args = append(args, "--"+flag.name, flag.value)
				}
			}
			var stdout, stderr bytes.Buffer
			if err := (cli.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args); err != nil {
				t.Fatalf("dry-run: %v; stderr=%s", err, stderr.String())
			}
			lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
			if len(lines) != 3 || !strings.HasPrefix(lines[0], "crabbox warmup ") || !strings.HasPrefix(lines[1], "crabbox actions hydrate ") || !strings.HasPrefix(lines[2], "crabbox run ") {
				t.Fatalf("expected warmup, hydration and probe: %s", stdout.String())
			}
			for i, line := range lines {
				for _, flag := range flags {
					if got, want := strings.Contains(line, "--"+flag.name), i == 0 || !flag.creation; got != want {
						t.Errorf("line %d --%s present=%t want=%t: %s", i, flag.name, got, want, line)
					}
				}
			}
		})
	}
}

func TestMachine0PrewarmRejectsInvalidFollowupConfigBeforeWarmup(t *testing.T) {
	for _, mode := range []string{"probe", "hydration", "both"} {
		for _, dryRun := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "/execute", true: "/dry-run"}[dryRun], func(t *testing.T) {
				tool := machine0PrewarmFixture(t, "-1")
				args := []string{"prewarm", "--provider", "machine0", "--machine0-cli", tool, "--machine0-image-version", "1"}
				if dryRun {
					args = append(args, "--dry-run")
				}
				if mode != "hydration" {
					args = append(args, "--probe-command", "true")
				}
				if mode == "probe" {
					args = append(args, "--no-hydrate")
				}
				var stdout, stderr bytes.Buffer
				err := (cli.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args)
				var exitErr cli.ExitError
				if !cli.AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(err.Error(), "prewarm ") || !strings.Contains(err.Error(), "configuration is invalid: machine0.imageVersion must not be negative") {
					t.Fatalf("expected early projected-config rejection, got %v; stderr=%s", err, stderr.String())
				}
				if stdout.Len() != 0 {
					t.Fatalf("rejected follow-up printed a plan or started warmup: %s", stdout.String())
				}
			})
		}
	}
	t.Run("plain prewarm keeps creation override", func(t *testing.T) {
		tool := machine0PrewarmFixture(t, "-1")
		var stdout, stderr bytes.Buffer
		err := (cli.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"prewarm", "--dry-run", "--no-hydrate", "--provider", "machine0", "--machine0-cli", tool, "--machine0-image-version", "1"})
		if err != nil || strings.Count(stdout.String(), "crabbox warmup ") != 1 {
			t.Fatalf("plain creation override rejected: %v; stdout=%s stderr=%s", err, stdout.String(), stderr.String())
		}
	})
}
