package nvidiabrev

import (
	"strings"
	"testing"
)

func TestNvidiaBrevSSHConfigParsesDirectTarget(t *testing.T) {
	target, err := selectBrevSSHTarget(Config{}, `Host my-gpu-box
  HostName 10.0.0.5
  User brev
  Port 2222
  IdentityFile "/home/test/.brev/brev.pem"
  UserKnownHostsFile /dev/null
`, "my-gpu-box")
	if err != nil {
		t.Fatal(err)
	}
	if target.Host != "10.0.0.5" || target.Port != "2222" || target.User != "brev" || target.Key != "/home/test/.brev/brev.pem" {
		t.Fatalf("target=%#v", target)
	}
	if target.SSHConfigProxy || target.ProxyCommand != "" {
		t.Fatalf("direct target unexpectedly proxy-backed: %#v", target)
	}
	if target.KnownHostsFile != "/dev/null" || target.NetworkKind != networkPublic {
		t.Fatalf("target metadata=%#v", target)
	}
	for _, command := range []string{"git", "rsync", "tar"} {
		if !strings.Contains(target.ReadyCheck, "command -v "+command) {
			t.Fatalf("ready check missing %q: %q", command, target.ReadyCheck)
		}
	}
}

func TestNvidiaBrevSSHConfigParsesProxyTarget(t *testing.T) {
	target, err := selectBrevSSHTarget(Config{}, `Host my-gpu-box
  User brev
  IdentityFile "/home/test/.brev/brev.pem"
  ProxyCommand /home/test/.brev/cloudflared access ssh --hostname proxy.example
  UserKnownHostsFile /dev/null
`, "my-gpu-box")
	if err != nil {
		t.Fatal(err)
	}
	if target.Host != "my-gpu-box" || target.Port != "22" || !target.SSHConfigProxy {
		t.Fatalf("target=%#v", target)
	}
	if target.ProxyCommand != "/home/test/.brev/cloudflared access ssh --hostname proxy.example" {
		t.Fatalf("proxy command=%q", target.ProxyCommand)
	}
}

func TestNvidiaBrevSSHConfigSelectsHostAlias(t *testing.T) {
	data := `Host gpu-box
  HostName 10.0.0.5
  User brev
  Port 2222
  IdentityFile "/home/test/.brev/brev.pem"

Host gpu-box-host
  HostName 10.0.0.6
  User ubuntu
  Port 22
  IdentityFile "/home/test/.brev/brev.pem"
`
	alias := brevSSHConfigAlias("gpu-box", "host")
	target, err := selectBrevSSHTarget(Config{}, data, alias)
	if err != nil {
		t.Fatal(err)
	}
	if target.Host != "10.0.0.6" || target.User != "ubuntu" {
		t.Fatalf("target=%#v", target)
	}
}

func TestNvidiaBrevSSHConfigPrefersConfiguredUser(t *testing.T) {
	target, err := selectBrevSSHTarget(Config{NvidiaBrev: NvidiaBrevConfig{User: "alice"}}, `Host gpu-box
  HostName 10.0.0.5
  User brev
  IdentityFile "/home/test/.brev/brev.pem"
`, "gpu-box")
	if err != nil {
		t.Fatal(err)
	}
	if target.User != "alice" {
		t.Fatalf("user=%q want alice", target.User)
	}
}

func TestNvidiaBrevSSHConfigPrefersGeneratedUserOverGenericDefault(t *testing.T) {
	target, err := selectBrevSSHTarget(Config{SSHUser: "crabbox"}, `Host gpu-box
  HostName 10.0.0.5
  User brev
  IdentityFile "/home/test/.brev/brev.pem"
`, "gpu-box")
	if err != nil {
		t.Fatal(err)
	}
	if target.User != "brev" {
		t.Fatalf("user=%q want brev", target.User)
	}
}

func TestNvidiaBrevSSHConfigReportsMissingFields(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "missing user", data: `Host gpu
  HostName 10.0.0.5
  IdentityFile "/home/test/.brev/brev.pem"
`, want: "missing User"},
		{name: "missing identity", data: `Host gpu
  HostName 10.0.0.5
  User brev
`, want: "missing IdentityFile"},
		{name: "missing host and proxy", data: `Host gpu
  User brev
  IdentityFile "/home/test/.brev/brev.pem"
`, want: "missing HostName or ProxyCommand"},
		{name: "missing alias", data: `Host other
  HostName 10.0.0.5
  User brev
  IdentityFile "/home/test/.brev/brev.pem"
`, want: "not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := selectBrevSSHTarget(Config{}, tt.data, "gpu")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v, want %q", err, tt.want)
			}
		})
	}
}

func TestNvidiaBrevSSHConfigRejectsInvalidUsers(t *testing.T) {
	for _, user := range []string{"-oProxyCommand=sh", "alice@example.com", "alice bob"} {
		data := `Host gpu
  HostName 10.0.0.5
  User ` + user + `
  IdentityFile "/home/test/.brev/brev.pem"
`
		for _, cfg := range []Config{{NvidiaBrev: NvidiaBrevConfig{User: user}}, {}} {
			_, err := selectBrevSSHTarget(cfg, data, "gpu")
			if err == nil || !strings.Contains(err.Error(), "invalid User") {
				t.Fatalf("user=%q cfg=%#v err=%v", user, cfg.NvidiaBrev, err)
			}
		}
	}
	data := `Host gpu
  HostName 10.0.0.5
  User brev
  IdentityFile "/home/test/.brev/brev.pem"
`
	for _, user := range []string{"alice\nbob", "alice\tbob"} {
		_, err := selectBrevSSHTarget(Config{NvidiaBrev: NvidiaBrevConfig{User: user}}, data, "gpu")
		if err == nil || !strings.Contains(err.Error(), "invalid User") {
			t.Fatalf("configured user=%q err=%v", user, err)
		}
	}
}

func TestNvidiaBrevSSHConfigAllowsOpenSSHUsernames(t *testing.T) {
	user := "1" + strings.Repeat("a", 64)
	target, err := selectBrevSSHTarget(Config{NvidiaBrev: NvidiaBrevConfig{User: user}}, `Host gpu
  HostName 10.0.0.5
  User brev
  IdentityFile "/home/test/.brev/brev.pem"
`, "gpu")
	if err != nil {
		t.Fatal(err)
	}
	if target.User != user {
		t.Fatalf("user=%q want %q", target.User, user)
	}
}

func TestNvidiaBrevSSHConfigRejectsAmbiguousAlias(t *testing.T) {
	_, err := selectBrevSSHTarget(Config{}, `Host gpu
  HostName 10.0.0.5
  User brev
  IdentityFile "/home/test/.brev/brev.pem"

Host gpu
  HostName 10.0.0.6
  User brev
  IdentityFile "/home/test/.brev/brev.pem"
`, "gpu")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err=%v", err)
	}
}
