package serve

import (
	"context"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type serviceRunner struct{}

func (serviceRunner) Run(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error) {
	return core.LocalCommandResult{}, nil
}
func TestInstallPreservesEffectiveEnvironmentOptions(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no supported user service")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	project := filepath.Join(dir, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	t.Setenv("CRABBOX_SERVE_MEMORY", "48g")
	t.Setenv("CRABBOX_SERVE_ADVERTISE_ADDRESS", "100.64.1.2")
	cfg := core.Config{Serve: core.ServeConfig{SSHPort: 2200}}
	err := (Provider{}).RunAgent(context.Background(), cfg, core.Runtime{Exec: serviceRunner{}, Stdout: io.Discard, Stderr: io.Discard}, []string{"install", "--state-dir", "./agent-state"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "Library", "LaunchAgents", serviceName+".plist")
	if runtime.GOOS == "linux" {
		path = filepath.Join(dir, "config", "systemd", "user", "crabbox-serve.service")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if runtime.GOOS == "darwin" {
		var plist struct {
			Dict struct {
				Array struct {
					Strings []string `xml:"string"`
				} `xml:"array"`
			} `xml:"dict"`
		}
		if err := xml.Unmarshal(data, &plist); err != nil {
			t.Fatal(err)
		}
		text = strings.Join(plist.Dict.Array.Strings, " ")
	} else {
		text = strings.ReplaceAll(text, `"`, "")
	}
	for _, want := range []string{"--memory 48g", "--advertise-address 100.64.1.2", "--ssh-port 2200", "--state-dir " + filepath.Join(project, "agent-state")} {
		if !strings.Contains(text, want) {
			t.Errorf("installed command lost %q: %s", want, text)
		}
	}
}
