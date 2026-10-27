package ssh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestStaticRecoveryPreservesExplicitMultiplexing(t *testing.T) {
	for _, source := range []string{"environment", "config"} {
		t.Run(source, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv("XDG_CONFIG_HOME", root)
			t.Setenv("XDG_STATE_HOME", root)
			path := filepath.Join(root, "config.yaml")
			t.Setenv("CRABBOX_CONFIG", path)
			config := "provider: ssh\n"
			if source == "config" {
				config += "static:\n  noControlMaster: true\n"
			} else {
				t.Setenv("CRABBOX_STATIC_NO_CONTROL_MASTER", "1")
			}
			if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			// This is the resolved configuration after an explicit false override.
			route := core.CommandRoutingFor(core.Config{Provider: "ssh"}, "static_retry", core.CommandRoutingRetry,
				core.SSHTarget{Host: "127.0.0.1", User: "fixture", Port: "22"})
			if !strings.Contains(strings.Join(route.Args, " "), "--static-no-control-master=false") {
				t.Fatalf("retry lost explicit false: %v", route.Args)
			}
			old := waitForSSH
			t.Cleanup(func() { waitForSSH = old })
			sentinel := errors.New("stopped before remote effects")
			called := false
			waitForSSH = func(_ context.Context, target *core.SSHTarget, _ io.Writer) error {
				called = true
				if target.NoControlMaster {
					t.Fatal("retry reloaded lower-precedence true and changed transport")
				}
				return sentinel
			}
			args := append([]string{"run"}, route.Args...)
			args = append(args, "--no-sync", "--", "true")
			var output bytes.Buffer
			err := (core.App{Stdout: &output, Stderr: &output}).Run(t.Context(), args)
			if !called || !errors.Is(err, sentinel) {
				t.Fatalf("retry did not reach static transport: %v\n%s", err, &output)
			}
		})
	}
}

func TestStaticNoControlMasterLegacyClaim(t *testing.T) {
	// Pre-option claims contain only endpoint/workspace metadata, with no
	// transport policy. They must retain their identity under either setting.
	const stored = `{"leaseId":"static_legacy","provider":"ssh","staticHost":"127.0.0.1","staticUser":"fixture","staticPort":"22","staticWorkRoot":"/work/legacy","targetOS":"linux","labels":{"state":"ready"}}`
	for _, enabled := range []bool{false, true} {
		cfg := core.BaseConfig()
		cfg.Provider = "ssh"
		cfg.Static.NoControlMaster = enabled
		var claim core.LeaseClaim
		if err := json.Unmarshal([]byte(stored), &claim); err != nil {
			t.Fatal(err)
		}
		_, target, id, err := staticLeaseFromClaim(cfg, claim)
		if err != nil {
			t.Fatal(err)
		}
		if id != "static_legacy" || target.Host != "127.0.0.1" || target.User != "fixture" || target.Port != "22" || target.NoControlMaster != enabled {
			t.Fatalf("legacy claim changed: id=%q target=%+v", id, target)
		}
	}
}
