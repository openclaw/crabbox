package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDirectLeasePolicy(t *testing.T) {
	for _, tc := range []struct {
		name, provider, policy, coordinator string
		allow                               []string
		registered, denied                  bool
	}{
		{name: "default", provider: "aws"},
		{name: "allow", provider: "aws", policy: "allow"},
		{name: "deny", provider: "aws", policy: "deny", denied: true},
		{name: "allowlist", provider: "aws", policy: "deny", allow: []string{"aws"}},
		{name: "other allowlist", provider: "aws", policy: "deny", allow: []string{"gcp"}, denied: true},
		{name: "broker", provider: "aws", policy: "deny", coordinator: "https://coordinator.example"},
		{name: "registered direct", provider: "aws", policy: "deny", coordinator: "https://coordinator.example", registered: true, denied: true},
		{name: "unsupported broker", provider: "run-env-profile-test", policy: "deny", coordinator: "https://coordinator.example", denied: true},
		{name: "static", provider: "ssh", policy: "deny"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Provider: tc.provider, DirectLeases: tc.policy, DirectLeaseAllow: tc.allow, Coordinator: tc.coordinator}
			if tc.registered {
				cfg.BrokerMode = BrokerModeRegistered
			}
			err := validateDirectLeaseCreation(cfg)
			if (err != nil) != tc.denied {
				t.Fatalf("err=%v denied=%t", err, tc.denied)
			}
			if tc.denied && err.Error() != "direct-provider leases are disabled (directLeases: deny); configure a coordinator or add "+tc.provider+" to directLeaseAllow" {
				t.Fatalf("error=%v", err)
			}
		})
	}
	for _, name := range []string{"ssh", "local-container", "docker-sandbox", "multipass", "firecracker", "lume", "tart", "hyperv", "windows-sandbox", "apple-container", "apple-machine", "apple-vm", "anthropic-sandbox-runtime", "mxc"} {
		if !directLeaseExempt(name) {
			t.Fatalf("missing local exemption %s", name)
		}
	}
}

func TestDirectLeaseConfigTrust(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_DIRECT_LEASES", "")
	t.Setenv("CRABBOX_DIRECT_LEASE_ALLOW", "")
	for _, trusted := range []bool{false, true} {
		cfg := baseConfig()
		cfg.DirectLeases = "deny"
		cfg.DirectLeaseAllow = []string{"aws"}
		allow := []string{"gcp"}
		if err := applyFileConfigWithTrust(&cfg, fileConfig{DirectLeases: "allow", DirectLeaseAllow: &allow}, trusted); err != nil {
			t.Fatal(err)
		}
		wantPolicy, wantAllow := "deny", []string{"aws"}
		if trusted {
			wantPolicy, wantAllow = "allow", allow
		}
		if cfg.DirectLeases != wantPolicy || !reflect.DeepEqual(cfg.DirectLeaseAllow, wantAllow) {
			t.Fatalf("trusted=%t config=%s %v", trusted, cfg.DirectLeases, cfg.DirectLeaseAllow)
		}
	}
	cfg := baseConfig()
	if err := applyFileConfigWithTrust(&cfg, fileConfig{DirectLeases: "deny"}, false); err != nil || cfg.DirectLeases != "deny" {
		t.Fatal("repository cannot tighten")
	}
	t.Setenv("CRABBOX_DIRECT_LEASES", "allow")
	t.Setenv("CRABBOX_DIRECT_LEASE_ALLOW", " aws, gcp, aws ")
	if err := applyEnv(&cfg); err != nil || cfg.DirectLeases != "allow" || !reflect.DeepEqual(cfg.DirectLeaseAllow, []string{"aws", "gcp"}) {
		t.Fatalf("env config: %v %s %v", err, cfg.DirectLeases, cfg.DirectLeaseAllow)
	}
	t.Setenv("CRABBOX_DIRECT_LEASES", "bogus")
	if err := applyEnv(&cfg); err == nil {
		t.Fatal("invalid policy accepted")
	}
}

func TestDirectDenyCommandsBeforeProviderConfigure(t *testing.T) {
	for _, command := range []string{"warmup", "run", "prewarm"} {
		t.Run(command, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("CRABBOX_DIRECT_LEASES", "deny")
			dir := t.TempDir()
			t.Chdir(dir)
			isolateRunTestUserDirs(t, dir)
			previous := runEnvProfileTestConfigureHook
			defer func() { runEnvProfileTestConfigureHook = previous }()
			called := false
			runEnvProfileTestConfigureHook = func(Config) { called = true }
			args := []string{"--provider", "run-env-profile-test", "--network", "public"}
			var out, diagnostics bytes.Buffer
			app := App{Stdout: &out, Stderr: &diagnostics}
			var err error
			switch command {
			case "warmup":
				err = app.warmup(context.Background(), args)
			case "run":
				err = app.runCommand(context.Background(), append(args, "--", "true"))
			case "prewarm":
				err = app.prewarm(context.Background(), args)
			}
			if err == nil || !strings.Contains(err.Error(), "direct-provider leases are disabled") || called {
				t.Fatalf("err=%v configure=%t diagnostics=%s", err, called, diagnostics.String())
			}
		})
	}
}

func TestDirectLeaseConfigLayering(t *testing.T) {
	clearConfigEnv(t)
	if err := os.Unsetenv("CRABBOX_DIRECT_LEASE_ALLOW"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Chdir(dir)
	isolateRunTestUserDirs(t, dir)
	user := userConfigPath()
	if err := os.MkdirAll(filepath.Dir(user), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(user, []byte("directLeases: deny\ndirectLeaseAllow: [aws]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte("directLeases: allow\ndirectLeaseAllow: [gcp]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigInputs()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DirectLeases != "deny" || !reflect.DeepEqual(cfg.DirectLeaseAllow, []string{"aws"}) {
		t.Fatalf("repository loosened policy: %s %v", cfg.DirectLeases, cfg.DirectLeaseAllow)
	}
}
