package cli

import (
	"flag"
	"io"
	"reflect"
	"testing"
)

func TestGenericFlagInputAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name                string
		args                []string
		accepted, wantError bool
	}{
		{"unvisited", nil, false, false},
		{"market-same", []string{"--market=spot"}, true, false},
		{"market-invalid", []string{"--market=reserved"}, false, true},
		{"type-clear", []string{"--type="}, true, false},
		{"target-same", []string{"--target=linux"}, true, false},
		{"mode-same", []string{"--windows-mode=normal"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			fs := flag.NewFlagSet("inputs", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			target := registerTargetFlags(fs, cfg)
			market := fs.String("market", "spot", "")
			serverType := fs.String("type", "", "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			err := applyCapacityMarketFlag(&cfg, fs, *market)
			if err == nil {
				applyServerTypeFlagOverrides(&cfg, fs, *serverType)
				err = applyTargetFlagOverrides(&cfg, fs, target)
			}
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v", err)
			}
			got := cfg.inputProvenance.summary(configInputGeneric)
			if (got.state == "present") != tc.accepted || got.complete {
				t.Fatalf("summary=%#v", got)
			}
			if tc.accepted && !reflect.DeepEqual(got.sources, []string{"flag"}) {
				t.Fatalf("sources=%v", got.sources)
			}
		})
	}
	for _, values := range [][]string{{" , "}, {"NAME", " NAME "}} {
		cfg := baseConfig()
		cfg.EnvAllow = []string{"NAME"}
		applyRunEnvAllowFlags(&cfg, values)
		if (cfg.inputProvenance.summary(configInputGeneric).state == "present") != (values[0] == "NAME") {
			t.Fatal("append acceptance used changed length or blank input")
		}
		if !reflect.DeepEqual(cfg.EnvAllow, []string{"NAME"}) {
			t.Fatalf("allow list=%v", cfg.EnvAllow)
		}
	}
}

func TestGenericLeaseFlagInputAcceptance(t *testing.T) {
	p := architectureCapabilityTestProvider{}
	providerRegistry[p.Name()] = p
	t.Cleanup(func() { delete(providerRegistry, p.Name()) })
	for _, args := range [][]string{
		nil, {"--profile=default"}, {"--class=standard"}, {"--ssh-port=22"},
		{"--arch=amd64"}, {"--pond="}, {"--desktop=false"}, {"--browser=false"}, {"--code=false"},
		{"--desktop-env=xfce"}, {"--os=ubuntu:24.04"}, {"--ttl=30m"}, {"--idle-timeout=5m"},
		{"--image-min-os="}, {"--image-require-browser=false"}, {"--image-require-webview2=false"}, {"--image-require-desktop=false"},
	} {
		name := "none"
		if len(args) > 0 {
			name = args[0]
		}
		t.Run(name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Provider = p.Name()
			fs := newFlagSet("lease inputs", io.Discard)
			values := registerLeaseCreateFlags(fs, cfg)
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			if err := applyLeaseCreateFlagsForTarget(&cfg, fs, values, leaseFlagTarget{}, false); err != nil {
				t.Fatal(err)
			}
			got := cfg.inputProvenance.summary(configInputGeneric)
			if (got.state == "present") != (len(args) > 0) || got.complete {
				t.Fatalf("summary=%#v", got)
			}
		})
	}
}

func TestExtractBoolFlag(t *testing.T) {
	args, found := extractBoolFlag([]string{"run_123", "--json", "--tail"}, "json")
	if !found {
		t.Fatalf("flag not found")
	}
	if len(args) != 2 || args[0] != "run_123" || args[1] != "--tail" {
		t.Fatalf("args=%v", args)
	}
}

func TestExtractBoolFlagMissing(t *testing.T) {
	args, found := extractBoolFlag([]string{"run_123"}, "json")
	if found {
		t.Fatalf("flag should not be found")
	}
	if len(args) != 1 || args[0] != "run_123" {
		t.Fatalf("args=%v", args)
	}
}

func TestFlagWasSet(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	value := fs.String("id", "", "")
	fs.Bool("json", false, "")
	if err := fs.Parse([]string{"--id", "blue-lobster"}); err != nil {
		t.Fatal(err)
	}
	if *value != "blue-lobster" {
		t.Fatalf("id=%q", *value)
	}
	if !flagWasSet(fs, "id") {
		t.Fatal("id should be marked set")
	}
	if !FlagWasSet(fs, "id") {
		t.Fatal("exported id check should be marked set")
	}
	if flagWasSet(fs, "json") {
		t.Fatal("json should not be marked set")
	}
	if FlagWasSet(fs, "json") {
		t.Fatal("exported json check should not be marked set")
	}

	for _, tc := range []struct {
		name string
		args []string
		flag string
		want bool
	}{
		{name: "unset default", flag: "name"},
		{name: "explicit empty", args: []string{"--name="}, flag: "name", want: true},
		{name: "explicit default", args: []string{"--name=default"}, flag: "name", want: true},
		{name: "explicit false", args: []string{"--enabled=false"}, flag: "enabled", want: true},
		{name: "explicit zero", args: []string{"--count=0"}, flag: "count", want: true},
		{name: "repeated setting", args: []string{"--name=first", "--name="}, flag: "name", want: true},
		{name: "exact name", args: []string{"--name=value"}, flag: "NAME"},
		{name: "unknown name", args: []string{"--name=value"}, flag: "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet(tc.name, flag.ContinueOnError)
			fs.String("name", "default", "")
			fs.Bool("enabled", false, "")
			fs.Int("count", 0, "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			if got := FlagWasSet(fs, tc.flag); got != tc.want {
				t.Fatalf("FlagWasSet(%q)=%v, want %v", tc.flag, got, tc.want)
			}
		})
	}
}
