package cli

import (
	"os"
	"testing"
)

func TestExternalRoutingRoundTripUsesPrivateHashedPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := ExternalConfig{
		Command:  "node",
		Args:     []string{"/tmp/provider.mjs", "--token", "secret-arg"},
		Config:   map[string]any{"token": "secret-config"},
		WorkRoot: "/workspaces/crabbox",
	}
	path, err := PersistExternalRouting("../unsafe/lease", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	if path == "" || path[len(path)-5:] != ".json" {
		t.Fatalf("path=%q", path)
	}
	loaded, err := LoadExternalRouting(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Command != cfg.Command || len(loaded.Args) != 3 || loaded.Config["token"] != "secret-config" || loaded.WorkRoot != cfg.WorkRoot {
		t.Fatalf("loaded=%#v", loaded)
	}
	RemoveExternalRouting("../unsafe/lease")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("routing file still exists: %v", err)
	}
}

func TestDeclarativeExternalRoutingRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := ExternalConfig{
		Config: map[string]any{"size": "cpu16"},
		Lifecycle: ExternalLifecycleConfig{
			Acquire: ExternalLifecycleOperation{
				Steps: [][]string{
					{"devboxctl", "new", "{{name}}"},
					{"devboxctl", "setup", "{{name}}"},
				},
				RollbackOnFailure: true,
			},
			List: ExternalLifecycleOperation{
				Argv:   []string{"devboxctl", "list", "--format", "json"},
				Output: "json-name-array",
			},
			Release: ExternalLifecycleOperation{Argv: []string{"devboxctl", "rm", "{{name}}"}},
		},
		Connection: ExternalConnectionConfig{
			SSH: ExternalSSHConnectionConfig{
				User:           "{{env.DEVBOX_USER}}",
				Host:           "{{name}}",
				SSHConfigProxy: true,
			},
		},
		WorkRoot: "/home/developer/crabbox",
	}
	path, err := PersistExternalRouting("cbx_abcdef123456", cfg)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadExternalRouting(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Lifecycle.Acquire.Steps; len(got) != 2 ||
		len(got[0]) != 3 || got[0][0] != "devboxctl" || got[0][2] != "{{name}}" ||
		len(got[1]) != 3 || got[1][1] != "setup" ||
		!loaded.Lifecycle.Acquire.RollbackOnFailure {
		t.Fatalf("acquire=%#v", loaded.Lifecycle.Acquire)
	}
	if loaded.Connection.SSH.User != "{{env.DEVBOX_USER}}" || !loaded.Connection.SSH.SSHConfigProxy {
		t.Fatalf("connection=%#v", loaded.Connection)
	}
}

func TestLoadExternalRoutingRejectsBroadPermissions(t *testing.T) {
	path := t.TempDir() + "/routing.json"
	if err := os.WriteFile(path, []byte(`{"command":"provider"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0o077 == 0 {
		t.Skipf("test process umask created a private file mode=%o", info.Mode().Perm())
	}
	if _, err := LoadExternalRouting(path); err == nil {
		t.Fatal("expected insecure routing file rejection")
	}
}
