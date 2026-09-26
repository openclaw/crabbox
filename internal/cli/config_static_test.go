package cli

import (
	"io"
	"reflect"
	"strings"
	"testing"
)

func staticCommandFile(start, stop []string) fileConfig {
	return fileConfig{Static: &fileStaticSection{StartCommand: start, StopCommand: stop}}
}

func TestStaticCommandsRequireApprovalFromRepositoryConfig(t *testing.T) {
	start := []string{"/opt/tools/host-power", "up"}
	stop := []string{"/opt/tools/host-power", "down"}
	for _, tc := range []struct {
		name    string
		user    *fileConfig
		env     map[string]string
		flags   []string
		wantErr string
	}{
		{name: "repository start rejected", wantErr: "repository-configured static.startCommand"},
		{
			name:    "repository stop rejected when only start approved",
			user:    &fileConfig{Static: &fileStaticSection{StartCommand: start}},
			wantErr: "repository-configured static.stopCommand",
		},
		{
			name: "identical trusted user commands approve",
			user: &fileConfig{Static: &fileStaticSection{StartCommand: start, StopCommand: stop}},
		},
		{
			name:    "different trusted user command does not approve",
			user:    &fileConfig{Static: &fileStaticSection{StartCommand: []string{"/opt/tools/host-power", "wake"}, StopCommand: stop}},
			wantErr: "repository-configured static.startCommand",
		},
		{
			name: "environment approves",
			env: map[string]string{
				"CRABBOX_STATIC_START_COMMAND": `["/opt/tools/host-power","up"]`,
				"CRABBOX_STATIC_STOP_COMMAND":  `["/opt/tools/host-power","down"]`,
			},
		},
		{
			name:  "flags approve",
			flags: []string{`--static-start-command=["/opt/tools/host-power","up"]`, `--static-stop-command=["/opt/tools/host-power","down"]`},
		},
		{
			name:  "empty flags clear repository commands",
			flags: []string{"--static-start-command=[]", "--static-stop-command=[]"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Static.Power.Dedicated = true
			cfg.Static.User = "runner"
			cfg.Static.Port = "22"
			cfg.Provider = staticProvider
			cfg.Static.Host = "buildbox.example.test"
			if tc.user != nil {
				if err := applyFileConfigWithTrust(&cfg, *tc.user, true); err != nil {
					t.Fatal(err)
				}
			}
			if err := applyFileConfigWithTrust(&cfg, staticCommandFile(start, stop), false); err != nil {
				t.Fatal(err)
			}
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if tc.flags != nil {
				fs := newFlagSet("test", io.Discard)
				values := registerTargetFlags(fs, cfg)
				if err := fs.Parse(tc.flags); err != nil {
					t.Fatal(err)
				}
				if err := applyTargetFlagOverrides(&cfg, fs, values); err != nil {
					t.Fatal(err)
				}
			}
			err := validateProviderCredentialDestination(cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("approved commands rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v want %q", err, tc.wantErr)
			}
		})
	}
}

func TestStaticCommandsFromTrustedUserConfig(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.Static.Power.Dedicated = true
	cfg.Static.User = "runner"
	cfg.Static.Port = "22"
	cfg.Provider = staticProvider
	if err := applyFileConfigWithTrust(&cfg, staticCommandFile([]string{"/opt/tools/host-power", "up"}, []string{"/opt/tools/host-power", "down"}), true); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderCredentialDestination(cfg); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Static.StartCommand, []string{"/opt/tools/host-power", "up"}) || !reflect.DeepEqual(cfg.Static.StopCommand, []string{"/opt/tools/host-power", "down"}) {
		t.Fatalf("commands=%q/%q", cfg.Static.StartCommand, cfg.Static.StopCommand)
	}
}

func TestStaticCommandInputValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   string
		value string
		want  string
	}{
		{"not JSON", "CRABBOX_STATIC_START_COMMAND", "./host-power up", "must be a JSON argv array"},
		{"blank executable", "CRABBOX_STATIC_STOP_COMMAND", `[" ","down"]`, "must start with an executable"},
		{"NUL byte", "CRABBOX_STATIC_START_COMMAND", `["/opt/tools/host-power","u\u0000p"]`, "contains a NUL byte"},
		{"dot-relative executable", "CRABBOX_STATIC_START_COMMAND", `["./host-power","up"]`, "executable \"./host-power\" must be an absolute path"},
		{"nested relative executable", "CRABBOX_STATIC_STOP_COMMAND", `["scripts/host-power","down"]`, "executable \"scripts/host-power\" must be an absolute path"},
		{"PATH command name", "CRABBOX_STATIC_START_COMMAND", `["host-power","up"]`, "executable \"host-power\" must be an absolute path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv(tc.env, tc.value)
			cfg := baseConfig()
			cfg.Static.Power.Dedicated = true
			cfg.Static.User = "runner"
			cfg.Static.Port = "22"
			err := applyEnv(&cfg)
			if err == nil || !strings.Contains(err.Error(), tc.env+" "+tc.want) {
				t.Fatalf("err=%v want %s %s", err, tc.env, tc.want)
			}
		})
	}
	cfg := baseConfig()
	cfg.Static.Power.Dedicated = true
	cfg.Static.User = "runner"
	cfg.Static.Port = "22"
	if err := applyFileConfigWithTrust(&cfg, staticCommandFile([]string{""}, nil), true); err == nil || !strings.Contains(err.Error(), "static.startCommand must start with an executable") {
		t.Fatalf("file blank executable err=%v", err)
	}
}

func TestStaticCommandRelativeExecutableRefusedFromEverySource(t *testing.T) {
	clearConfigEnv(t)
	for _, trusted := range []bool{true, false} {
		cfg := baseConfig()
		cfg.Static.Power.Dedicated = true
		cfg.Static.User = "runner"
		cfg.Static.Port = "22"
		err := applyFileConfigWithTrust(&cfg, staticCommandFile([]string{"./host-power", "up"}, nil), trusted)
		if err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
			t.Fatalf("trusted=%t err=%v", trusted, err)
		}
	}
	fs := newFlagSet("test", io.Discard)
	cfg := baseConfig()
	cfg.Static.Power.Dedicated = true
	cfg.Static.User = "runner"
	cfg.Static.Port = "22"
	values := registerTargetFlags(fs, cfg)
	if err := fs.Parse([]string{`--static-stop-command=["../host-power","down"]`}); err != nil {
		t.Fatal(err)
	}
	if err := applyTargetFlagOverrides(&cfg, fs, values); err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
		t.Fatalf("flag err=%v", err)
	}
}

func TestStaticPowerDedicatedConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		power   *StaticPowerConfig
		trusted bool
		want    string
	}{
		{"missing", nil, true, "static.power.dedicated: true"},
		{"false", &StaticPowerConfig{}, true, "static.power.dedicated: true"},
		{"repository cannot grant", &StaticPowerConfig{Dedicated: true}, false, "repository-configured static.power"},
		{"trusted", &StaticPowerConfig{Dedicated: true, HostID: "lab"}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Provider = staticProvider
			cfg.Static.Host = "127.0.0.1"
			cfg.Static.User = "root"
			cfg.Static.Port = "2222"
			approved := staticCommandFile([]string{"/bin/true"}, []string{"/bin/true"})
			if err := applyFileConfigWithTrust(&cfg, approved, true); err != nil {
				t.Fatal(err)
			}
			if tc.power != nil {
				if err := applyFileConfigWithTrust(&cfg, fileConfig{Static: &fileStaticSection{Power: tc.power}}, tc.trusted); err != nil {
					t.Fatal(err)
				}
			}
			err := validateProviderCredentialDestination(cfg)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want=%s", err, tc.want)
			}
		})
	}
}

func TestStaticPowerRouteTrustedOverrides(t *testing.T) {
	for _, method := range []string{"none", "environment", "flags", "partial"} {
		t.Run(method, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Provider = staticProvider
			trusted := fileConfig{Static: &fileStaticSection{fileStaticConfig: fileStaticConfig{Host: "127.0.0.1", User: "runner", Port: "22"}, Power: &StaticPowerConfig{Dedicated: true}, StartCommand: []string{"/usr/bin/true"}}}
			if err := applyFileConfigWithTrust(&cfg, trusted, true); err != nil {
				t.Fatal(err)
			}
			repo := fileConfig{Static: &fileStaticSection{fileStaticConfig: fileStaticConfig{User: "repo", Port: "2222"}}}
			if err := applyFileConfigWithTrust(&cfg, repo, false); err != nil {
				t.Fatal(err)
			}
			switch method {
			case "environment", "partial":
				t.Setenv("CRABBOX_STATIC_USER", "runner")
				if method != "partial" {
					t.Setenv("CRABBOX_STATIC_PORT", "22")
				}
				if err := applyEnv(&cfg); err != nil {
					t.Fatal(err)
				}
			case "flags":
				fs := newFlagSet("test", io.Discard)
				values := registerTargetFlags(fs, cfg)
				if err := fs.Parse([]string{"--static-user=runner", "--static-port=22"}); err != nil {
					t.Fatal(err)
				}
				if err := applyTargetFlagOverrides(&cfg, fs, values); err != nil {
					t.Fatal(err)
				}
			}
			err := validateProviderCredentialDestination(cfg)
			if method == "none" || method == "partial" {
				if err == nil {
					t.Fatal("repository route inherited power authority")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
