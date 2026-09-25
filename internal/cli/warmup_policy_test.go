package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWarmupKeepIdleConfigPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, user, repo, envKeep, envIdle string
		flags                              []string
		keep                               bool
		idle                               time.Duration
	}{
		{name: "defaults", keep: true, idle: 30 * time.Minute},
		{name: "explicit idle", flags: []string{"--idle-timeout", "17m"}, keep: true, idle: 17 * time.Minute},
		{name: "user config", user: "warmup:\n  keep: false\nlease:\n  idleTimeout: 11m\n", idle: 11 * time.Minute},
		{name: "repo overrides user", user: "warmup:\n  keep: false\nlease:\n  idleTimeout: 11m\n", repo: "warmup:\n  keep: true\nlease:\n  idleTimeout: 13m\n", keep: true, idle: 13 * time.Minute},
		{name: "repo false overrides user true", user: "warmup:\n  keep: true\n", repo: "warmup:\n  keep: false\n", idle: 30 * time.Minute},
		{name: "env overrides config", repo: "warmup:\n  keep: false\nlease:\n  idleTimeout: 11m\n", envKeep: "true", envIdle: "19m", keep: true, idle: 19 * time.Minute},
		{name: "env false overrides config true", repo: "warmup:\n  keep: true\n", envKeep: "false", idle: 30 * time.Minute},
		{name: "flags override env", envKeep: "true", envIdle: "19m", flags: []string{"--keep=false", "--idle-timeout", "23m"}, idle: 23 * time.Minute},
		{name: "explicit true overrides config", repo: "warmup:\n  keep: false\nlease:\n  idleTimeout: 11m\n", flags: []string{"--keep=true", "--idle-timeout", "29m"}, keep: true, idle: 29 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			t.Chdir(dir)
			userConfig := userConfigPath()
			if err := os.MkdirAll(filepath.Dir(userConfig), 0700); err != nil {
				t.Fatal(err)
			}
			for path, contents := range map[string]string{userConfig: tc.user, filepath.Join(dir, ".crabbox.yaml"): tc.repo} {
				if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CRABBOX_CONFIG", "")
			t.Setenv("CRABBOX_WARMUP_KEEP", tc.envKeep)
			t.Setenv("CRABBOX_IDLE_TIMEOUT", tc.envIdle)
			var acquired AcquireRequest
			var cfg Config
			created := time.Now().UTC().Truncate(time.Second)
			runEnvProfileTestConfigureHook = func(config Config) { cfg = config }
			runEnvProfileTestAcquireLease = func(req AcquireRequest) (LeaseTarget, error) {
				acquired = req
				return LeaseTarget{
					LeaseID: "cbx_123456abcdef",
					Server:  Server{Provider: cfg.Provider, CloudID: "warmup-fixture", Labels: DirectLeaseLabels(cfg, "cbx_123456abcdef", "warmup-fixture", cfg.Provider, "", req.Keep, created)},
					SSH:     SSHTarget{User: "crabbox", Host: "192.0.2.1", Port: "22", TargetOS: targetLinux},
				}, nil
			}
			t.Cleanup(func() {
				runEnvProfileTestConfigureHook = nil
				runEnvProfileTestAcquireLease = nil
			})
			var stdout, stderr bytes.Buffer
			args := append([]string{"--provider", runEnvProfileTestProvider{}.Spec().Name, "--network", "public"}, tc.flags...)
			if err := (App{Stdout: &stdout, Stderr: &stderr}).warmup(context.Background(), args); err != nil {
				t.Fatalf("warmup: %v\n%s\n%s", err, stdout.String(), stderr.String())
			}
			if acquired.Keep != tc.keep || acquired.Options.IdleTimeout != tc.idle {
				t.Fatalf("keep=%t idle=%s, want keep=%t idle=%s", acquired.Keep, acquired.Options.IdleTimeout, tc.keep, tc.idle)
			}
			claim, err := ReadLeaseClaim("cbx_123456abcdef")
			if err != nil || claim.IdleTimeoutSeconds != int(tc.idle/time.Second) {
				t.Fatalf("persisted idle=%d, err=%v, want %s", claim.IdleTimeoutSeconds, err, tc.idle)
			}
			server := Server{Labels: claim.Labels}
			if due, reason := shouldCleanupServer(server, created.Add(tc.idle-time.Second)); due {
				t.Fatalf("cleanup before idle deadline: %s", reason)
			}
			if due, reason := shouldCleanupServer(server, created.Add(tc.idle+time.Second)); !due {
				t.Fatalf("idle lease exempt from cleanup: %s", reason)
			}
		})
	}
}
