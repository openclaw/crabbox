package mxc

import (
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestExperimentalContainmentRequiresOptIn(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWindows
	cfg.WindowsMode = core.WindowsModeNormal
	cfg.MXC.Containment = "windows_sandbox"
	_, err := (Provider{}).Configure(cfg, core.Runtime{})
	if err == nil || !strings.Contains(err.Error(), "--mxc-experimental") {
		t.Fatalf("err=%v", err)
	}
}

func TestConfigureNormalizesContainment(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWindows
	cfg.WindowsMode = core.WindowsModeNormal
	cfg.MXC.Containment = "ProcessContainer"
	configured, err := (Provider{}).Configure(cfg, core.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if got := configured.(*backend).cfg.MXC.Containment; got != "processcontainer" {
		t.Fatalf("containment=%q", got)
	}
}

func TestConfigureAcceptsStableProcessIntent(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWindows
	cfg.WindowsMode = core.WindowsModeNormal
	cfg.MXC.Containment = "Process"

	configured, err := (Provider{}).Configure(cfg, core.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if got := configured.(*backend).cfg.MXC.Containment; got != "process" {
		t.Fatalf("containment=%q", got)
	}
}

func TestFlagsApplyPolicy(t *testing.T) {
	cfg := core.BaseConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--mxc-network", "allow", "--mxc-readwrite-paths", `C:\src,C:\cache`, "--mxc-allow-dacl-mutation", "--mxc-allow-windows-ui", "--mxc-experimental"}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.MXC.Network != "allow" || len(cfg.MXC.ReadWritePaths) != 2 || !cfg.MXC.AllowDACLMutation || !cfg.MXC.AllowWindowsUI || !cfg.MXC.Experimental {
		t.Fatalf("mxc=%+v", cfg.MXC)
	}
}

func TestParseWindowsBuild(t *testing.T) {
	build, err := parseWindowsBuild("CurrentBuildNumber    REG_SZ    26100\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if build != 26100 {
		t.Fatalf("build = %d, want 26100", build)
	}
}

func TestMXCBindingFlagContract(t *testing.T) {
	fields := map[string]string{"CLIPath": "mxc-cli", "Version": "mxc-version", "Containment": "mxc-containment", "Network": "mxc-network", "ReadOnlyPaths": "mxc-readonly-paths", "ReadWritePaths": "mxc-readwrite-paths", "AllowedHosts": "mxc-allowed-hosts", "BlockedHosts": "mxc-blocked-hosts", "AllowDACLMutation": "mxc-allow-dacl-mutation", "AllowWindowsUI": "mxc-allow-windows-ui", "Experimental": "mxc-experimental"}
	for _, provider := range []string{"mxc", "execution-container", "ssh", ""} {
		for field, name := range fields {
			for _, visit := range []string{"absent", "empty", "value"} {
				cfg := core.Config{Provider: provider}
				dst := reflect.ValueOf(&cfg.MXC).Elem().FieldByName(field)
				var want any
				raw := " fixture "
				def := "prior"
				switch dst.Kind() {
				case reflect.String:
					dst.SetString("prior")
					want = "prior"
					if visit != "absent" {
						if visit == "empty" {
							raw = ""
						}
						want = raw
					}
				case reflect.Slice:
					dst.Set(reflect.ValueOf([]string{" prior ", "none"}))
					want = []string{" prior ", "none"}
					def = " prior ,none"
					raw = " a, ,a,none "
					if visit == "empty" {
						raw = " , "
						want = []string(nil)
					} else if visit == "value" {
						want = []string{"a", "a", "none"}
					}
				case reflect.Bool:
					dst.SetBool(true)
					want = true
					def = "true"
					raw = "true"
					if visit == "empty" {
						raw = "false"
						want = false
					}
				}
				fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
				values := registerFlags(fs, cfg)
				if fs.Lookup(name).DefValue != def {
					t.Fatalf("%s default=%q", name, fs.Lookup(name).DefValue)
				}
				if visit != "absent" {
					first := "previous"
					if dst.Kind() == reflect.Bool {
						first = "true"
					}
					if err := fs.Parse([]string{"--" + name + "=" + first, "--" + name + "=" + raw}); err != nil {
						t.Fatal(err)
					}
				}
				before := cfg.MXC
				if err := applyFlags(&cfg, fs, struct{}{}); err != nil || !reflect.DeepEqual(before, cfg.MXC) {
					t.Fatal("wrong type mutated config")
				}
				if err := applyFlags(&cfg, fs, values); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(dst.Interface(), want) || cfg.Provider != provider {
					t.Fatalf("%s/%s/%s got=%#v want=%#v", provider, field, visit, dst.Interface(), want)
				}
			}
		}
	}
}
