package mxc

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestDefaultRequestUsesStableMXCContract(t *testing.T) {
	cfg, err := buildConfig(core.BaseConfig(), core.RunRequest{Command: []string{"cmd.exe", "/d", "/c", "exit", "0"}})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Version string `json:"version"`
		Network struct {
			Egress struct {
				Default string `json:"default"`
			} `json:"egress"`
			Ingress struct {
				Default      string `json:"default"`
				HostLoopback string `json:"hostLoopback"`
			} `json:"ingress"`
		} `json:"network"`
	}
	if err := json.Unmarshal(wire, &request); err != nil {
		t.Fatal(err)
	}
	if request.Version != "1.0.0" {
		t.Errorf("default MXC request version = %q, want stable %q", request.Version, "1.0.0")
	}
	if request.Network.Egress.Default != "deny" || request.Network.Ingress.Default != "deny" || request.Network.Ingress.HostLoopback != "deny" {
		t.Errorf("default MXC request must deny egress, ingress and host loopback; got %+v", request.Network)
	}
	for _, retired := range []string{"defaultPolicy", "enforcementMode", "allowedHosts", "blockedHosts"} {
		if strings.Contains(string(wire), retired) {
			t.Fatalf("retired field %s emitted: %s", retired, wire)
		}
	}
	var process struct {
		Process map[string]json.RawMessage `json:"process"`
	}
	if err := json.Unmarshal(wire, &process); err != nil {
		t.Fatal(err)
	}
	if env, ok := process.Process["env"]; !ok || string(env) == "null" {
		t.Fatalf("explicit environment missing: %s", wire)
	}
	if string(process.Process["inheritDefaultEnv"]) != "false" {
		t.Fatalf("default environment inheritance enabled: %s", wire)
	}
}

func TestStableMXCNumericEgressRules(t *testing.T) {
	for _, policy := range []string{"block", "allow"} {
		t.Run(policy, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.MXC.Network = policy
			cfg.MXC.AllowedHosts = []string{"203.0.113.7", "2001:db8::1", "198.51.100.0/24", "2001:db8:1::/48"}
			cfg.MXC.BlockedHosts = []string{"203.0.113.7", "2001:db8::/32"}
			before, _ := json.Marshal(cfg.MXC)
			config, err := buildConfig(cfg, core.RunRequest{Command: []string{"cmd.exe"}})
			if err != nil {
				t.Fatal(err)
			}
			want := "deny"
			if policy == "allow" {
				want = "allow"
			}
			if config.Network.Egress.Default != want || config.Network.Ingress.Default != "deny" || config.Network.Ingress.HostLoopback != "deny" {
				t.Fatalf("network=%+v", config.Network)
			}
			var got []string
			for _, rule := range config.Network.Egress.Allow {
				if len(rule.To) != 1 {
					t.Fatalf("unscoped rule: %+v", rule)
				}
				got = append(got, rule.To[0].CIDR)
			}
			if !reflect.DeepEqual(got, []string{"203.0.113.7/32", "2001:db8::1/128", "198.51.100.0/24", "2001:db8:1::/48"}) {
				t.Fatalf("allow=%v", got)
			}
			if len(config.Network.Egress.Deny) != 2 || config.Network.Egress.Deny[0].To[0].CIDR != "203.0.113.7/32" || config.Network.Egress.Deny[1].To[0].CIDR != "2001:db8::/32" {
				t.Fatalf("deny=%+v", config.Network.Egress.Deny)
			}
			after, _ := json.Marshal(cfg.MXC)
			if string(before) != string(after) {
				t.Fatal("mutated source configuration")
			}
		})
	}
}

func TestStableMXCRejectsUnsupportedSettings(t *testing.T) {
	cases := []struct {
		name, want string
		change     func(*core.MXCConfig)
	}{
		{"legacy version", "--mxc-version 1.0.0", func(c *core.MXCConfig) { c.Version = "0.6.0-alpha" }},
		{"other published version", "--mxc-version 1.0.0", func(c *core.MXCConfig) { c.Version = "0.9.0-alpha" }},
		{"development version", "--mxc-version 1.0.0", func(c *core.MXCConfig) { c.Version = "1.1.0-alpha" }},
		{"invalid network", "--mxc-network block or allow", func(c *core.MXCConfig) { c.Network = "deny" }},
		{"DACL fallback", "no host-DACL mutation fallback", func(c *core.MXCConfig) { c.AllowDACLMutation = true }},
	}
	for _, containment := range []string{"windows_sandbox", "isolation_session", "wslc", "lxc", "bubblewrap", "seatbelt", "unknown"} {
		cases = append(cases, struct {
			name, want string
			change     func(*core.MXCConfig)
		}{containment, "--mxc-containment processcontainer", func(c *core.MXCConfig) { c.Containment = containment; c.Experimental = true }})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.TargetOS = core.TargetWindows
			cfg.WindowsMode = core.WindowsModeNormal
			tc.change(&cfg.MXC)
			_, err := (Provider{}).Configure(cfg, core.Runtime{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Configure err=%v", err)
			}
			_, err = buildConfig(cfg, core.RunRequest{Command: []string{"cmd.exe"}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("buildConfig err=%v", err)
			}
		})
	}
	for _, field := range []string{"allowedHosts", "blockedHosts"} {
		for _, value := range []string{"example.test", "*.example.test", "https://example.test", "203.0.113.1:443", "", "203.0.113.1/24", "203.0.113.1/33", "2001:db8::1/64", "fe80::1%eth0", "::ffff:192.0.2.1", "::ffff:192.0.2.0/120"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				cfg := core.BaseConfig()
				cfg.TargetOS = core.TargetWindows
				cfg.WindowsMode = core.WindowsModeNormal
				if field == "allowedHosts" {
					cfg.MXC.AllowedHosts = []string{value}
				} else {
					cfg.MXC.BlockedHosts = []string{value}
				}
				_, err := (Provider{}).Configure(cfg, core.Runtime{})
				if err == nil || !strings.Contains(err.Error(), "mxc."+field) {
					t.Fatalf("err=%v", err)
				}
			})
		}
	}
}

func TestStableMXCTimeoutBounds(t *testing.T) {
	for _, tc := range []struct {
		ttl   time.Duration
		valid bool
	}{
		{time.Millisecond, true}, {4294967295 * time.Millisecond, true},
		{4294967296 * time.Millisecond, false}, {time.Nanosecond, false},
	} {
		cfg := core.BaseConfig()
		got, err := buildConfig(cfg, core.RunRequest{Command: []string{"cmd.exe"}, Options: core.LeaseOptions{TTL: tc.ttl}})
		if tc.valid {
			if err != nil || got.Process.Timeout != tc.ttl.Milliseconds() {
				t.Fatalf("ttl=%v got=%+v err=%v", tc.ttl, got, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "--ttl") {
			t.Fatalf("ttl=%v err=%v", tc.ttl, err)
		}
	}
	cfg := core.BaseConfig()
	cfg.TTL = 0
	got, err := buildConfig(cfg, core.RunRequest{Command: []string{"cmd.exe"}})
	if err != nil || got.Process.Timeout != 0 {
		t.Fatalf("zero timeout: %+v %v", got, err)
	}
}

func TestStableMXCRuleErrorsDoNotEchoInput(t *testing.T) {
	for _, value := range []string{"https://user:private-token@example.test", "private-token", "203.0.113.7/24"} {
		_, err := networkRules([]string{"203.0.113.0/24", value}, "blockedHosts", "--mxc-blocked-hosts")
		if err == nil || strings.Contains(err.Error(), value) || !strings.Contains(err.Error(), "mxc.blockedHosts entry 2") {
			t.Fatalf("unsafe or unactionable error: %v", err)
		}
	}
}
