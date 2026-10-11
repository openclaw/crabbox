package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var errPrecedenceBoundary = errors.New("precedence audit stopped before provider access")

type precedenceAuditProvider struct {
	architectureCapabilityTestProvider
	observed *Config
}

func (p precedenceAuditProvider) Spec() ProviderSpec {
	spec := p.architectureCapabilityTestProvider.Spec()
	spec.Name = "precedence-audit"
	spec.Features = FeatureSet{FeatureSSH, FeatureCrabboxSync, FeatureCleanup, FeatureDesktop, FeatureBrowser, FeatureCode}
	return spec
}

func (p precedenceAuditProvider) Configure(cfg Config, _ Runtime) (Backend, error) {
	*p.observed = cfg
	return nil, errPrecedenceBoundary
}

func (p precedenceAuditProvider) ConfigureDoctor(cfg Config, _ Runtime) (DoctorBackend, error) {
	*p.observed = cfg
	return nil, errPrecedenceBoundary
}

// Exercise the command parsers and their real loading paths. The provider
// boundary records the result and refuses all resource or network operations.
func TestCommandSelectorPrecedence(t *testing.T) {
	commands := []struct {
		name string
		run  func(App, context.Context, []string) error
		args []string
	}{
		{"run", App.runCommand, []string{"--", "true"}},
		{"warmup", App.warmup, nil},
		{"prewarm", App.prewarm, []string{"--no-hydrate"}},
		{"stop", App.stop, []string{"--id", "cbx_0123456789ab"}},
		{"status", App.status, []string{"--id", "cbx_0123456789ab"}},
		{"inspect", App.inspect, []string{"--id", "cbx_0123456789ab"}},
		{"list", App.list, nil},
		{"cleanup", App.cleanup, []string{"--dry-run"}},
		{"ssh", App.ssh, []string{"--id", "cbx_0123456789ab"}},
		{"connect", App.connect, []string{"--id", "cbx_0123456789ab"}},
		{"vnc", App.vnc, []string{"--id", "cbx_0123456789ab"}},
		{"webvnc", App.webvnc, []string{"--id", "cbx_0123456789ab"}},
		{"code", App.webCode, []string{"--id", "cbx_0123456789ab"}},
		{"desktop", App.desktopLaunch, []string{"--id", "cbx_0123456789ab"}},
		{"screenshot", App.screenshot, []string{"--id", "cbx_0123456789ab"}},
		{"checkpoint create", App.checkpointCreate, []string{"--id", "cbx_0123456789ab", "--name", "audit"}},
		{"checkpoint fork", App.checkpointFork, []string{"chk_audit"}},
		{"pause", App.pause, []string{"--id", "cbx_0123456789ab"}},
		{"resume", App.resume, []string{"--id", "cbx_0123456789ab"}},
		{"doctor", App.doctor, nil},
	}
	cases := []struct {
		name, config, envName, envValue string
		flags                           []string
	}{
		{"config target and mode", "target: windows\nwindows:\n  mode: wsl2\n", "", "", []string{"--target", "linux"}},
		{"env target and mode", "target: macos\n", "CRABBOX_WINDOWS_MODE", "wsl2", []string{"--target", "linux"}},
		{"invalid env target", "", "CRABBOX_TARGET", "obsolete", []string{"--target", "linux"}},
		{"invalid env mode", "target: windows\n", "CRABBOX_WINDOWS_MODE", "obsolete", []string{"--target", "linux", "--windows-mode", "normal"}},
		{"provider overrides env", "", "CRABBOX_PROVIDER", "missing-provider", []string{"--target", "linux"}},
	}
	for _, command := range commands {
		for _, tc := range cases {
			t.Run(command.name+"/"+tc.name, func(t *testing.T) {
				clearConfigEnv(t)
				root := t.TempDir()
				t.Chdir(root)
				config := filepath.Join(root, "config.yaml")
				if err := os.WriteFile(config, []byte("class: tiny\n"+tc.config), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CRABBOX_CONFIG", config)
				if tc.envName != "" {
					t.Setenv(tc.envName, tc.envValue)
				}
				var observed Config
				provider := precedenceAuditProvider{observed: &observed}
				providerRegistry[provider.Spec().Name] = provider
				t.Cleanup(func() { delete(providerRegistry, provider.Spec().Name) })
				switch command.name {
				case "code":
					// Code requires broker login before configuring a provider.
					// The provider must still stop it before any broker request.
					broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						t.Errorf("unexpected broker request: %s %s", r.Method, r.URL.Path)
						http.Error(w, "unexpected request", http.StatusInternalServerError)
					}))
					t.Cleanup(broker.Close)
					t.Setenv("CRABBOX_COORDINATOR", broker.URL)
					t.Setenv("CRABBOX_COORDINATOR_TOKEN", "fixture-token")
				case "checkpoint fork":
					store, err := defaultCheckpointStore()
					if err != nil {
						t.Fatal(err)
					}
					if _, _, err := store.Reserve(checkpointRecord{ID: "chk_audit", Kind: checkpointKindArchive}); err != nil {
						t.Fatal(err)
					}
				}
				args := append([]string{"--provider", provider.Spec().Name}, tc.flags...)
				args = append(args, command.args...)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				err := command.run(App{Stdout: io.Discard, Stderr: io.Discard}, ctx, args)
				if !errors.Is(err, errPrecedenceBoundary) && !(command.name == "doctor" && observed.Provider != "") {
					t.Fatalf("did not reach provider boundary: %v", err)
				}
				if observed.Provider != provider.Spec().Name || observed.TargetOS != targetLinux || observed.WindowsMode != windowsModeNormal || observed.Class != "tiny" {
					t.Fatalf("resolved provider=%s target=%s mode=%s", observed.Provider, observed.TargetOS, observed.WindowsMode)
				}
			})
		}
	}
}

func TestCommandLimitedSelectorPrecedence(t *testing.T) {
	for _, command := range []struct {
		name string
		run  func(App, context.Context, []string) error
		args []string
	}{
		{"hold", App.hold, []string{"--provider", "precedence-audit", "--id", "cbx_0123456789ab"}},
		{"providers sizes", App.providerSizes, []string{"precedence-audit", "--class", "small"}},
	} {
		t.Run(command.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
			t.Setenv("CRABBOX_PROVIDER", "missing-provider")
			t.Setenv("CRABBOX_DEFAULT_CLASS", "tiny")
			var observed Config
			provider := precedenceAuditProvider{observed: &observed}
			providerRegistry[provider.Spec().Name] = provider
			t.Cleanup(func() { delete(providerRegistry, provider.Spec().Name) })
			err := command.run(App{Stdout: io.Discard, Stderr: io.Discard}, t.Context(), command.args)
			if !errors.Is(err, errPrecedenceBoundary) {
				t.Fatalf("did not reach provider boundary: %v", err)
			}
			wantClass := "tiny"
			if command.name == "providers sizes" {
				wantClass = "small"
			}
			if observed.Provider != provider.Spec().Name || observed.Class != wantClass {
				t.Fatalf("provider=%s class=%s", observed.Provider, observed.Class)
			}
		})
	}
}

func TestLeaseSelectorPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, config, envName, envValue string
		args                            []string
	}{
		{"architecture", "", "CRABBOX_ARCH", "obsolete", []string{"--arch", "amd64"}},
		{"OS", "", "CRABBOX_OS", "obsolete", []string{"--os", "ubuntu:26.04"}},
		{"config architecture", "architecture: obsolete\n", "", "", []string{"--arch", "amd64"}},
		{"config OS", "os: obsolete\n", "", "", []string{"--os", "ubuntu:26.04"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			config := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(config, []byte(tc.config), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", config)
			if tc.envName != "" {
				t.Setenv(tc.envName, tc.envValue)
			}
			var observed Config
			provider := precedenceAuditProvider{observed: &observed}
			providerRegistry[provider.Spec().Name] = provider
			t.Cleanup(func() { delete(providerRegistry, provider.Spec().Name) })
			fs := flag.NewFlagSet("warmup", flag.ContinueOnError)
			flags := registerLeaseCreateFlags(fs, defaultConfig())
			if err := fs.Parse(append([]string{"--provider", provider.Spec().Name}, tc.args...)); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfigForFlags(fs)
			if err != nil {
				t.Fatal(err)
			}
			if err := applyLeaseCreateFlags(&cfg, fs, flags); err != nil {
				t.Fatal(err)
			}
			if cfg.Architecture != ArchitectureAMD64 {
				t.Fatalf("architecture=%s", cfg.Architecture)
			}
		})
	}
}

func TestTargetFlagOverridesWithoutSelectors(t *testing.T) {
	cfg := baseConfig()
	fs := newFlagSet("heartbeat", io.Discard)
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlagValues{}); err != nil {
		t.Fatal(err)
	}
	if cfg.TargetOS != targetLinux || cfg.WindowsMode != windowsModeNormal {
		t.Fatalf("target=%s mode=%s", cfg.TargetOS, cfg.WindowsMode)
	}
}
