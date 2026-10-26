package parallels

import (
	"flag"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestNewBackendMacOSGuestUser(t *testing.T) {
	t.Setenv("USER", "host-user")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"omitted", nil, "host-user"},
		{"unrelated flag", []string{"--parallels-source", "source"}, "host-user"},
		{"explicit default", []string{"--parallels-user", "crabbox"}, "crabbox"},
		{"explicit nondefault", []string{"--parallels-user", "guest-user"}, "guest-user"},
		{"template default", []string{"--parallels-template", "mac"}, "crabbox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.TargetOS = core.TargetMacOS
			cfg.Parallels.Templates = map[string]core.ParallelsTemplateConfig{"mac": {User: "crabbox"}}
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			provider := Provider{}
			values := provider.RegisterFlags(fs, cfg)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			backend := NewBackend(provider.Spec(), cfg, core.Runtime{}).(*leaseBackend)
			if got := backend.Cfg.SSHUser; got != tc.want {
				t.Fatalf("guest SSH user = %q, want %q", got, tc.want)
			}
		})
	}
}
