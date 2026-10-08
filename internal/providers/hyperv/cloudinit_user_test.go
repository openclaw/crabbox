package hyperv

import (
	"flag"
	"io"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"gopkg.in/yaml.v3"
)

func TestLinuxCloudInitHonorsSelectedUser(t *testing.T) {
	for _, user := range []string{"bench", "alice", "crabbox", "root"} {
		t.Run(user, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.Provider, cfg.TargetOS = providerName, targetLinux
			core.MarkTargetExplicit(&cfg)
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			flags := registerFlags(fs, cfg)
			if err := fs.Parse([]string{"--hyperv-user=" + user}); err != nil {
				t.Fatal(err)
			}
			if err := applyFlags(&cfg, fs, flags); err != nil {
				t.Fatal(err)
			}
			var document struct {
				PasswordAuth bool `yaml:"ssh_pwauth"`
				DisableRoot  bool `yaml:"disable_root"`
				Users        []struct {
					Name string   `yaml:"name"`
					Keys []string `yaml:"ssh_authorized_keys"`
				} `yaml:"users"`
			}
			if err := yaml.Unmarshal([]byte(linuxUserData(cfg, "fixture-public-key")), &document); err != nil {
				t.Fatal(err)
			}
			if cfg.SSHUser != user || len(document.Users) != 1 || document.Users[0].Name != user ||
				len(document.Users[0].Keys) != 1 || document.Users[0].Keys[0] != "fixture-public-key" ||
				document.PasswordAuth || document.DisableRoot != (user != "root") {
				t.Fatal("selected SSH account cannot use its cloud-init authorized key")
			}
		})
	}
}
