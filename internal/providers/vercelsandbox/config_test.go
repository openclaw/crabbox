package vercelsandbox

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestVercelSandboxConfigLoadsAllLayersThenExplicitFlags(t *testing.T) {
	testutil.IsolateUserDirs(t)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "CRABBOX_") || strings.HasPrefix(key, "VERCEL_") {
			t.Setenv(key, "")
		}
	}
	t.Chdir(t.TempDir())
	userDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(userDir, "crabbox", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(userFile), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(file, body string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(runtime string) core.Config {
		t.Helper()
		cfg, err := core.LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.VercelSandbox.Runtime != runtime {
			t.Fatalf("runtime=%q, want %q", cfg.VercelSandbox.Runtime, runtime)
		}
		return cfg
	}
	write(userFile, "provider: vercel-sandbox\n")
	check("node24")
	write(userFile, "provider: vercel-sandbox\nvercelSandbox:\n  runtime: node22\n  persistent: true\n  execTimeoutSecs: 55\n  networkAllow: [user.example]\n")
	check("node22")
	write("crabbox.yaml", "provider: vercel-sandbox\nvercelSandbox:\n  runtime: python3.13\n  persistent: false\n  execTimeoutSecs: 0\n  networkAllow: []\n")
	cfg := check("python3.13")
	if cfg.VercelSandbox.Persistent || cfg.VercelSandbox.ExecTimeoutSecs != 0 || len(cfg.VercelSandbox.NetworkAllow) != 0 {
		t.Fatal("repo zero values lost")
	}
	write(".crabbox.yaml", "vercelSandbox:\n  runtime: node26\n")
	check("node26")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_RUNTIME", "node24")
	t.Setenv("CRABBOX_VERCEL_SANDBOX_PERSISTENT", "true")
	cfg = check("node24")
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := (Provider{}).RegisterFlags(fs, core.BaseConfig())
	if err := fs.Parse([]string{"--vercel-sandbox-runtime=node22", "--vercel-sandbox-persistent=false"}); err != nil {
		t.Fatal(err)
	}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.VercelSandbox.Runtime != "node22" || cfg.VercelSandbox.Persistent || cfg.VercelSandbox.ExecTimeoutSecs != 0 {
		t.Fatalf("flags did not preserve precedence: %#v", cfg.VercelSandbox)
	}
}

func TestVercelSandboxCredentialAliasesStayRuntimeOnly(t *testing.T) {
	for _, group := range [][]string{
		{"CRABBOX_VERCEL_SANDBOX_TOKEN", "CRABBOX_VERCEL_TOKEN", "VERCEL_TOKEN"},
		{"CRABBOX_VERCEL_SANDBOX_AUTH_TOKEN", "CRABBOX_VERCEL_AUTH_TOKEN", "VERCEL_AUTH_TOKEN"},
		{"CRABBOX_VERCEL_SANDBOX_OIDC_TOKEN", "VERCEL_OIDC_TOKEN"},
	} {
		for first := range group {
			t.Run(group[first], func(t *testing.T) {
				for i, key := range group {
					value := ""
					if i >= first {
						value = "fake-" + key
					}
					t.Setenv(key, value)
				}
				want := group[len(group)-1] + "=fake-" + group[first]
				env := vercelSandboxBridgeEnv(nil)
				found := false
				for _, entry := range env {
					if entry == want {
						found = true
					}
				}
				if !found {
					t.Fatal("runtime credential alias priority changed")
				}
			})
		}
	}
}
