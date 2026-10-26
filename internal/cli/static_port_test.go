package cli

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func staticPortCandidates(t *testing.T, cfg Config) []string {
	t.Helper()
	_, target, _, err := staticLease(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return sshPortCandidates(target.Port, target.FallbackPorts)
}

func TestStaticLeaseExplicitStaticPortNeverFallsBack(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost", "::1", "build.example"} {
		for _, port := range []string{"2224", "2222", "22"} {
			cfg := baseConfig()
			cfg.Provider, cfg.Static.Host, cfg.Static.Port = staticProvider, host, port
			if got, want := staticPortCandidates(t, cfg), []string{port}; !reflect.DeepEqual(got, want) {
				t.Fatalf("host=%s candidates=%v want %v", host, got, want)
			}
		}
	}
}

func TestStaticLeaseExplicitSSHPortNeverFallsBack(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = staticProvider
	cfg.Static.Host = "127.0.0.1"
	cfg.SSHPort = "2224"
	MarkSSHPortExplicit(&cfg)
	if got, want := staticPortCandidates(t, cfg), []string{"2224"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates=%v want %v", got, want)
	}
}

func TestStaticLeaseDefaultPortKeepsFallback(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = staticProvider
	cfg.Static.Host = "build.example"
	if got, want := staticPortCandidates(t, cfg), []string{"2222", "22"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates=%v want %v", got, want)
	}
}

func TestStaticLeaseExplicitPortOverridesFallbackList(t *testing.T) {
	for _, explicitSSH := range []bool{false, true} {
		cfg := baseConfig()
		cfg.Provider = staticProvider
		cfg.Static.Host = "build.example"
		if explicitSSH {
			cfg.SSHPort = "2224"
			MarkSSHPortExplicit(&cfg)
		} else {
			cfg.Static.Port = "2224"
		}
		cfg.SSHFallbackPorts = []string{"22", "2225"}
		cfg.sshFallbackPortsExplicit = true
		cfg.explicitSSHFallbackPorts = []string{"22", "2225"}
		if got, want := staticPortCandidates(t, cfg), []string{"2224"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("explicitSSH=%t candidates=%v want %v", explicitSSH, got, want)
		}
	}
}

func TestStaticLeaseDefaultPortOnLoopbackKeepsFallback(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "localhost", "build.example"} {
		cfg := baseConfig()
		cfg.Provider = staticProvider
		cfg.Static.Host = host
		if got, want := staticPortCandidates(t, cfg), []string{"2222", "22"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("host=%s candidates=%v want %v", host, got, want)
		}
		cfg.SSHFallbackPorts = []string{"2225"}
		if got, want := staticPortCandidates(t, cfg), []string{"2222", "2225"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("host=%s candidates=%v want %v", host, got, want)
		}
	}
}

func TestStaticRemoteExplicitPortInputsNeverFallBack(t *testing.T) {
	for _, source := range []string{"flag", "environment", "config"} {
		t.Run(source, func(t *testing.T) {
			isolateTestUserDirs(t)
			cfg := baseConfig()
			cfg.Provider = staticProvider
			cfg.Static.Host = "build.example"
			switch source {
			case "flag":
				fs := newFlagSet("run", io.Discard)
				flags := registerLeaseCreateFlags(fs, cfg)
				if err := parseFlags(fs, []string{"--ssh-port", "2224"}); err != nil {
					t.Fatal(err)
				}
				if err := applyLeaseCreateFlagsForLease(&cfg, fs, flags, "cbx_abcdef123456"); err != nil {
					t.Fatal(err)
				}
			case "environment":
				t.Setenv("CRABBOX_SSH_PORT", "2224")
				if err := applyEnv(&cfg); err != nil {
					t.Fatal(err)
				}
			case "config":
				configPath := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(configPath, []byte("ssh:\n  port: 2224\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				file, err := readFileConfig(configPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := applyFileConfig(&cfg, file); err != nil {
					t.Fatal(err)
				}
			}
			if !IsSSHPortExplicit(&cfg) {
				t.Fatalf("%s input did not mark the SSH port explicit", source)
			}
			if got, want := staticPortCandidates(t, cfg), []string{"2224"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("candidates=%v want %v", got, want)
			}
		})
	}
}

// openLoopbackSSHPort gives static tests a fixture-owned listening endpoint.
func openLoopbackSSHPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestStaticPortInputs(t *testing.T) {
	for _, source := range []string{"flag", "environment", "user config", "repo config"} {
		t.Run(source, func(t *testing.T) {
			clearConfigEnv(t)
			isolateTestUserDirs(t)
			cfg := baseConfig()
			cfg.Provider, cfg.Static.Host = staticProvider, "localhost"
			switch source {
			case "flag":
				fs := newFlagSet("run", io.Discard)
				flags := registerLeaseCreateFlags(fs, cfg)
				if err := parseFlags(fs, []string{"--static-port", "2224"}); err != nil {
					t.Fatal(err)
				}
				if err := applyLeaseCreateFlagsForLease(&cfg, fs, flags, "static_test"); err != nil {
					t.Fatal(err)
				}
			case "environment":
				t.Setenv("CRABBOX_STATIC_PORT", "2224")
				if err := applyEnv(&cfg); err != nil {
					t.Fatal(err)
				}
			default:
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, []byte("static:\n  port: '2224'\n"), 0600); err != nil {
					t.Fatal(err)
				}
				file, err := readFileConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				if source == "repo config" {
					err = applyFileConfigWithTrust(&cfg, file, false)
				} else {
					err = applyFileConfig(&cfg, file)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if got := staticPortCandidates(t, cfg); !reflect.DeepEqual(got, []string{"2224"}) {
				t.Fatalf("candidates=%v", got)
			}
		})
	}
}
