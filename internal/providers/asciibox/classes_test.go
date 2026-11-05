package asciibox

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestBoatSizingSelection(t *testing.T) {
	for _, test := range []struct {
		name, class, native, want string
		explicitClass             bool
	}{
		{"omitted keeps native default", "large", "", "default", false},
		{"fast", "fast", "", "large", true},
		{"native type wins", "large", "small", "small", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.TargetOS, cfg.Architecture = core.TargetLinux, core.ArchitectureAMD64
			cfg.Class = test.class
			if test.explicitClass {
				core.MarkClassExplicit(&cfg)
			}
			if test.native != "" {
				cfg.ServerType, cfg.ServerTypeExplicit = test.native, true
			}
			if got := (Provider{}).ServerTypeForConfig(cfg); got != test.want {
				t.Fatalf("type=%q want %q", got, test.want)
			}
		})
	}
	for _, name := range []string{"ascii-box", "boat"} {
		cfg := testConfig()
		cfg.Provider = name
		fs := flag.NewFlagSet("boat", flag.ContinueOnError)
		fs.StringVar(&cfg.ServerType, "type", "", "")
		fs.StringVar(&cfg.Class, "class", "", "")
		values := RegisterAsciiBoxProviderFlags(fs, cfg)
		if err := fs.Parse([]string{"--type", "large"}); err != nil {
			t.Fatal(err)
		}
		if err := ApplyAsciiBoxProviderFlags(&cfg, fs, values); err != nil {
			t.Fatalf("%s rejects sizing: %v", name, err)
		}
	}
}

func TestBoatSizingReachesOrdinaryCreateAndStatus(t *testing.T) {
	fake := &fakeAPI{box: testBox()}
	fake.box.Type = "large"
	withFakeAPI(t, fake)
	stubSSHWait(t)
	cfg := testConfig()
	cfg.ServerType, cfg.ServerTypeExplicit = "large", true
	b := NewBackend(Provider{}.Spec(), cfg, testRuntime()).(*backend)
	lease, err := b.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	if fake.createReq.Type != "large" || lease.Server.ServerType.Name != "large" {
		t.Fatalf("request=%+v observed type=%q", fake.createReq, lease.Server.ServerType.Name)
	}
	view, err := b.Status(t.Context(), core.StatusRequest{ID: lease.LeaseID})
	if err != nil || view.ServerType != "large" || view.Labels["box_type"] != "large" {
		t.Fatalf("status=%+v err=%v", view, err)
	}
	fake.box.Type = ""
	view, err = b.Status(t.Context(), core.StatusRequest{ID: lease.LeaseID})
	if err != nil || view.ServerType != "" || view.Labels["box_type"] != "" {
		t.Fatalf("missing native type retained a stale size: %+v err=%v", view, err)
	}
}

func TestBoatNativeCLIReceivesSize(t *testing.T) {
	runner := &fakeCommandRunner{configPath: filepath.Join(t.TempDir(), "config.json")}
	c := &client{apiKey: "synthetic", apiURL: "https://ascii.dev", cliPath: "boat", home: t.TempDir(), runner: runner}
	if _, err := c.CreateBox(t.Context(), createRequest{Type: "large"}); err != nil {
		t.Fatal(err)
	}
	for _, request := range runner.requests {
		if strings.Contains(strings.Join(request.Args, " "), "new --type large") {
			return
		}
	}
	t.Fatal("native create did not receive --type large")
}

func TestBoatInvalidSizeFailsBeforeProviderAccess(t *testing.T) {
	cfg := testConfig()
	cfg.ServerType, cfg.ServerTypeExplicit = "unsupported", true
	cfg.AsciiBox.APIKey = ""
	b := NewBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	if _, err := b.Acquire(t.Context(), core.AcquireRequest{}); err == nil || !strings.Contains(err.Error(), "requires --type") {
		t.Fatalf("invalid type reached provider access: %v", err)
	}
}

func TestBoatRejectsARM64BeforeProviderAccess(t *testing.T) {
	cfg := testConfig()
	cfg.ServerType, cfg.ServerTypeExplicit = "small", true
	cfg.Architecture = core.ArchitectureARM64
	core.MarkArchitectureExplicit(&cfg)
	cfg.AsciiBox.APIKey = ""
	b := NewBackend(Provider{}.Spec(), cfg, testRuntime()).(*backend)
	if _, err := b.Acquire(t.Context(), core.AcquireRequest{}); err == nil || !strings.Contains(err.Error(), "only amd64") {
		t.Fatalf("unsupported architecture reached provider access: %v", err)
	}
}

func TestBoatSizingConfigAndEnvironment(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("CRABBOX_PROVIDER", "boat")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "")
	t.Setenv("CRABBOX_SERVER_TYPE", "")
	if err := os.WriteFile(".crabbox.yaml", []byte("provider: boat\nclass: fast\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ class, native, want string }{
		{"", "", "large"},
		{"standard", "", "default"},
		{"beast", "small", "small"},
	} {
		t.Setenv("CRABBOX_DEFAULT_CLASS", test.class)
		t.Setenv("CRABBOX_SERVER_TYPE", test.native)
		cfg, err := core.LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if got := (Provider{}).ServerTypeForConfig(cfg); got != test.want {
			t.Fatalf("class env=%q type env=%q: got %q want %q", test.class, test.native, got, test.want)
		}
	}
}
