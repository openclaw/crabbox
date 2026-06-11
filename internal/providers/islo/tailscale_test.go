package islo

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsloTailscaleHostname(t *testing.T) {
	cases := []struct {
		name string
		cfg  func(*Config)
		slug string
		want string
	}{
		{
			name: "default template",
			cfg:  func(c *Config) {},
			slug: "node-1",
			want: "crabbox-node-1",
		},
		{
			name: "explicit hostname wins over template",
			cfg:  func(c *Config) { c.Tailscale.Hostname = "Build-Box" },
			slug: "node-1",
			want: "build-box",
		},
		{
			name: "template with provider token, sanitized",
			cfg:  func(c *Config) { c.Tailscale.HostnameTemplate = "{provider}_{slug}!" },
			slug: "API gw",
			want: "islo-api-gw",
		},
		{
			name: "template with lease id token",
			cfg:  func(c *Config) { c.Tailscale.HostnameTemplate = "{provider}-{id}" },
			slug: "node-1",
			want: "islo-isb-crabbox-test",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			cfg.Tailscale.HostnameTemplate = "crabbox-{slug}"
			tc.cfg(&cfg)
			if got := isloTailscaleHostname(cfg, "isb_crabbox-test", tc.slug); got != tc.want {
				t.Fatalf("hostname = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsloTailscaleIPRegex(t *testing.T) {
	out := "some preamble\nCRABBOX_TS_IP=100.105.127.13\ntrailing\n"
	m := isloTailscaleIPRe.FindStringSubmatch(out)
	if m == nil || m[1] != "100.105.127.13" {
		t.Fatalf("expected to parse 100.105.127.13, got %v", m)
	}
	if isloTailscaleIPRe.FindStringSubmatch("no ip here") != nil {
		t.Fatalf("expected no match")
	}
}

func TestIsloTailscaleBringUpScriptIncludesUserspaceProxyAndOptionalFlags(t *testing.T) {
	for _, want := range []string{
		"--tun=userspace-networking",
		"--socks5-server=127.0.0.2:1055",
		"--outbound-http-proxy-listen=127.0.0.2:1055",
		`--statedir="${TS_STATE_DIR}"`,
		`TS_AUTH_FILE="$(mktemp /tmp/crabbox-ts-auth.XXXXXX)"`,
		`--auth-key="file:${TS_AUTH_FILE}"`,
		"unset TS_AUTHKEY",
		`trap 'rm -f "${TS_AUTH_FILE}"' EXIT`,
		"--shields-up=false",
		defaultIsloTailscaleAMD64SHA256,
		defaultIsloTailscaleARM64SHA256,
		"sha256sum -c -",
		`--login-server="${TS_LOGIN_SERVER}"`,
		`--exit-node="${TS_EXIT_NODE}"`,
		"--exit-node-allow-lan-access",
		`/tmp/ts/tailscale --socket="${TS_SOCKET}" up "$@"`,
	} {
		if !strings.Contains(isloTailscaleBringUp, want) {
			t.Fatalf("bring-up script missing %q", want)
		}
	}
	if strings.Contains(isloTailscaleBringUp, `--authkey="${TS_AUTHKEY}"`) {
		t.Fatal("bring-up script must not expose the auth key in tailscale argv")
	}
	if strings.Index(isloTailscaleBringUp, "unset TS_AUTHKEY") > strings.Index(isloTailscaleBringUp, "setsid /tmp/ts/tailscaled") {
		t.Fatal("bring-up script must unset the auth key before starting tailscaled")
	}
	if strings.Contains(isloTailscaleBringUp, "--socks5-server=127.0.0.1:") || strings.Contains(isloTailscaleBringUp, "--outbound-http-proxy-listen=127.0.0.1:") {
		t.Fatal("outbound proxies must not bind the loopback address used for tailnet ingress")
	}
}

func TestIsloTailscaleArchiveVerification(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "tailscale.tgz")
	content := []byte("archive")
	if err := os.WriteFile(archive, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	run := func(expected string) error {
		cmd := exec.Command("bash", "-lc", isloTailscaleVerifyArchive)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"TS_ARCHIVE=" + archive,
			"TS_SHA256=" + expected,
		}
		return cmd.Run()
	}
	if err := run(fmt.Sprintf("%x", sum)); err != nil {
		t.Fatalf("matching checksum rejected: %v", err)
	}
	if err := run(strings.Repeat("0", 64)); err == nil {
		t.Fatal("mismatched checksum accepted")
	}
}
