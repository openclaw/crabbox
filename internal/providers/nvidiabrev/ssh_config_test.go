package nvidiabrev

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

// This runner only evaluates fixture configs; it never connects to a host.
func nativeSSHConfig(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if req.Name != "ssh" || len(req.Args) < 5 || req.Args[0] != "-G" || req.Args[1] != "-F" {
		panic("unexpected SSH fixture command")
	}
	cmd := exec.Command(req.Name, req.Args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	return core.LocalCommandResult{Stdout: string(stdout), Stderr: stderr.String()}, err
}

func TestNvidiaBrevNativeSSHConfig(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX Match exec fixture")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH client required")
	}
	for _, tt := range []struct {
		name, config, alias, override, wantUser, wantPort string
	}{
		{name: "direct certificate", config: `Match host gpu exec "true"
 HostName direct.example.test
 User brev
 Port 2222
 IdentityFile /test/cert-key
 IdentitiesOnly yes
`, alias: "gpu", wantUser: "brev", wantPort: "2222"},
		{name: "proxy certificate and user override", config: `Match host gpu exec "true"
 User brev
 IdentityFile /test/cert-key
 IdentitiesOnly yes
 ProxyCommand proxy %n %h %p
`, alias: "gpu", override: "alice", wantUser: "alice", wantPort: "22"},
		{name: "optional certificate with native first value", config: `Match host gpu exec "true"
 IdentityFile /test/cert-key
Host gpu
 HostName direct.example.test
 User brev
 IdentityFile /test/static-key
 IdentitiesOnly yes
Host gpu
 User ignored
`, alias: "gpu", wantUser: "brev", wantPort: "22"},
		{name: "optional certificate failure", config: `Match host gpu exec "false"
 IdentityFile /test/cert-key
Host gpu
 HostName direct.example.test
 User brev
 IdentityFile /test/static-key
 IdentitiesOnly yes
`, alias: "gpu", wantUser: "brev", wantPort: "22"},
		{name: "host target", config: `Host gpu
 HostName container.example.test
 User brev
 IdentityFile /test/static-key
 IdentitiesOnly yes
Host gpu-host
 HostName host.example.test
 User ubuntu
 IdentityFile /test/static-key
 IdentitiesOnly yes
`, alias: brevSSHConfigAlias("gpu", "host"), wantUser: "ubuntu", wantPort: "22"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ssh_config")
			if err := os.WriteFile(path, []byte("UserKnownHostsFile /dev/null\n"+tt.config), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := core.Config{SSHUser: "generic-default", NvidiaBrev: core.NvidiaBrevConfig{User: tt.override}}
			c := &brevClient{rt: core.Runtime{Exec: &fakeRunner{run: nativeSSHConfig}}}
			target, err := c.resolveSSHConfig(t.Context(), cfg, path, tt.alias, []byte("UserKnownHostsFile /dev/null\n"+tt.config))
			if err != nil {
				t.Fatal(err)
			}
			if target.Host != tt.alias || target.User != tt.wantUser || target.Port != tt.wantPort || target.SSHConfigFile != path || !target.SSHConfigProxy || !target.NoControlMaster || target.Key != "" || target.CertificateFile != "" || target.KnownHostsFile != "/dev/null" {
				t.Fatalf("target=%#v", target)
			}
		})
	}
}

func TestNvidiaBrevNativeSSHConfigRejectsMissingRoutes(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX Match exec fixture")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH client required")
	}
	for _, config := range []string{
		"Host other\n HostName other.example.test\n User brev\n IdentitiesOnly yes\n",
		"Match host gpu exec \"false\"\n HostName direct.example.test\n User brev\n IdentitiesOnly yes\n IdentityFile /test/key\n",
	} {
		path := filepath.Join(t.TempDir(), "ssh_config")
		if err := os.WriteFile(path, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		c := &brevClient{rt: core.Runtime{Exec: &fakeRunner{run: nativeSSHConfig}}}
		if _, err := c.resolveSSHConfig(t.Context(), core.Config{}, path, "gpu", []byte(config)); err == nil || !strings.Contains(err.Error(), "route not found") {
			t.Fatalf("err=%v", err)
		}
	}
	c := &brevClient{rt: core.Runtime{Exec: &fakeRunner{run: nativeSSHConfig}}}
	if _, err := c.resolveSSHConfig(t.Context(), core.Config{}, filepath.Join(t.TempDir(), "absent"), "gpu", nil); err == nil {
		t.Fatal("missing config accepted")
	}
}

func TestNvidiaBrevNativeSSHConfigRejectsArgumentsBeforeExec(t *testing.T) {
	for _, value := range []string{"-oProxyCommand=sh", "alice@example.test", "alice bob", "alice;id", "alice\nbob", "$(id)", "gpu%h"} {
		runner := &fakeRunner{run: func(core.LocalCommandRequest) (core.LocalCommandResult, error) {
			t.Fatal("unsafe argument reached Match exec")
			return core.LocalCommandResult{}, nil
		}}
		c := &brevClient{rt: core.Runtime{Exec: runner}}
		if _, err := c.resolveSSHConfig(context.Background(), core.Config{}, "unused", value, nil); err == nil {
			t.Fatalf("alias %q accepted", value)
		}
		if _, err := c.resolveSSHConfig(context.Background(), core.Config{NvidiaBrev: core.NvidiaBrevConfig{User: value}}, "unused", "gpu", nil); err == nil {
			t.Fatalf("user %q accepted", value)
		}
	}
}

func TestNvidiaBrevMetadataUsesCapturedConfig(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX OpenSSH fixture")
	}
	_, home := isolateNvidiaBrevState(t)
	config := "Host gpu\n HostName expected.example.test\n User brev\n IdentitiesOnly yes\n"
	writeBrevSSHConfig(t, home, config)
	c := &brevClient{rt: core.Runtime{Exec: &fakeRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		writeBrevSSHConfig(t, home, "Match host gpu exec \"exit 99\"\n HostName replaced.example.test\n User replaced\n IdentitiesOnly yes\n")
		return nativeSSHConfig(req)
	}}}}
	target, err := c.resolveSSHConfig(t.Context(), core.Config{}, defaultBrevSSHConfigPath(), "gpu", []byte(config))
	if err != nil || target.User != "brev" || string(target.SSHConfigData) != config {
		t.Fatalf("metadata followed replaced file: err=%v target=%#v", err, target)
	}
	encoded, err := json.Marshal(target)
	if err != nil || bytes.Contains(encoded, []byte("expected.example.test")) {
		t.Fatalf("provider config escaped transport: err=%v json=%s", err, encoded)
	}
}
