package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSSHTransportConfigParsesWithOpenSSH(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH client is required")
	}
	t.Setenv("HOME", t.TempDir())
	session, err := newSSHTransportSession(t.Context(), SSHTarget{
		User:         `DOMAIN\alice%id`,
		Host:         "ssh.example.test",
		Port:         "2200",
		ProxyCommand: `provider-proxy --route "blue box" %h %p`,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	cmd := exec.Command(ssh, "-G", "-F", session.configPath, sshTransportHostAlias)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ssh -G: %v: %s", err, stderr.String())
	}
	got := strings.ToLower(stdout.String())
	for _, line := range []string{
		"hostname ssh.example.test",
		`user domain\alice%id`,
		"port 2200",
		`proxycommand provider-proxy --route "blue box" %h %p`,
		"exitonforwardfailure yes",
		"gatewayports no",
	} {
		if !strings.Contains(got, line) {
			t.Fatalf("ssh -G missing %q:\n%s", line, stdout.String())
		}
	}
}

func TestSSHTransportProxyCommandExecutesAsCommand(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil || os.PathSeparator == '\\' {
		t.Skip("POSIX OpenSSH client is required")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "proxy-ran")
	proxy := filepath.Join(dir, "proxy")
	script := "#!/bin/sh\nprintf ran > \"$CRABBOX_TEST_PROXY_MARKER\"\nexit 1\n"
	if err := os.WriteFile(proxy, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRABBOX_TEST_PROXY_MARKER", marker)
	session, err := newSSHTransportSession(t.Context(), SSHTarget{
		User:         "alice",
		Host:         "ssh.example.test",
		Port:         "22",
		ProxyCommand: proxy,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	cmd := exec.Command(ssh, "-F", session.configPath, sshTransportHostAlias, "exit 0")
	if err := cmd.Run(); err == nil {
		t.Fatal("SSH unexpectedly completed through failing proxy fixture")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "ran" {
		t.Fatalf("ProxyCommand did not run: data=%q err=%v", got, err)
	}
}

func TestSSHTransportSessionKeepsSecretsOutOfProcessArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	target := SSHTarget{
		User:         "token-user-secret",
		Host:         "ssh.example.test",
		Port:         "22",
		AuthSecret:   true,
		ProxyCommand: "provider proxy --session session-123 %h %p",
	}
	session, err := newSSHTransportSession(t.Context(), target, true)
	if err != nil {
		t.Fatal(err)
	}
	configPath := session.configPath
	t.Cleanup(func() { _ = session.Close() })

	copyArgs, err := resolvedSSHCopyArgs(session, target, "./input.txt", "SANDBOX:/tmp/input.txt", false, false)
	if err != nil {
		t.Fatal(err)
	}
	tunnelArgs := resolvedSSHTunnelArgs(session, "41000", "3000")
	for _, rendered := range []string{strings.Join(copyArgs, " "), strings.Join(tunnelArgs, " ")} {
		for _, secret := range []string{"token-user-secret"} {
			if strings.Contains(rendered, secret) {
				t.Fatalf("process args leaked %q: %q", secret, rendered)
			}
		}
		if !strings.Contains(rendered, configPath) || !strings.Contains(rendered, sshTransportHostAlias) {
			t.Fatalf("process args do not use private resolved config: %q", rendered)
		}
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, `User "token-user-secret"`) || !strings.Contains(got, `ProxyCommand provider proxy --session session-123 %h %p`) {
		t.Fatalf("private config did not preserve resolved transport: %q", got)
	}
	if err := verifySSHTransportPathPrivate(configPath, false); err != nil {
		t.Fatalf("private config permissions: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("private config remained after close: %v", err)
	}
}

func TestSSHTransportConfigProxyIncludesUserRouting(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX OpenSSH config fixture")
	}
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH client is required")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	identityFile := filepath.Join(sshDir, "proxy-proxy-alias identity")
	certificateFile := filepath.Join(sshDir, "proxy-routed.example.test identity-cert.pub")
	identityAgent := filepath.Join(sshDir, "agent-proxy-alias.sock")
	for path, contents := range map[string]string{identityFile: "private-key", certificateFile: "certificate"} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	identityPattern := filepath.Join(sshDir, "proxy-%n identity")
	certificatePattern := filepath.Join(sshDir, "proxy-%h identity-cert.pub")
	identityAgentPattern := filepath.Join(sshDir, "agent-%n.sock")
	userConfig := fmt.Sprintf("IgnoreUnknown CrabboxFutureOption\nCrabboxFutureOption yes\nHost proxy-alias\n  ProxyJump jump.example.test\n  HostKeyAlias edge%%blue\n  IdentityFile \"%s\"\n  IdentitiesOnly yes\n  IdentityAgent \"%s\"\n  CertificateFile \"%s\"\n  LocalForward 127.0.0.1:41001 127.0.0.1:3001\n  RemoteForward 127.0.0.1:41002 127.0.0.1:3002\n  DynamicForward 127.0.0.1:41003\n  RequestTTY force\n  RemoteCommand echo inherited\n  SessionType none\nMatch originalhost proxy-alias user alice exec \"test %%p = 2222\"\n  HostName routed.example.test\n", identityPattern, identityAgentPattern, certificatePattern)
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(userConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	target := SSHTarget{User: "alice", Host: "proxy-alias", Port: "2222", SSHConfigProxy: true}
	copySession, err := newSSHTransportSession(t.Context(), target, false)
	if err != nil {
		t.Fatalf("copy-mode route: %v", err)
	}
	if err := copySession.Close(); err != nil {
		t.Fatal(err)
	}
	session, err := newSSHTransportSession(t.Context(), target, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if session.host() != sshTransportHostAlias {
		t.Fatalf("destination=%q", session.host())
	}
	cmd := exec.Command(ssh, "-G", "-F", session.configPath, session.host())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ssh -G: %v: %s", err, output)
	}
	got := strings.ToLower(string(output))
	for _, line := range []string{"hostname routed.example.test", "hostkeyalias edge%blue", "user alice", "port 2222", "requesttty false", "identityfile " + strings.ToLower(identityFile), "identitiesonly yes", "identityagent " + strings.ToLower(identityAgent), "certificatefile " + strings.ToLower(certificateFile)} {
		if !strings.Contains(got, line) {
			t.Fatalf("ssh -G missing %q:\n%s", line, output)
		}
	}
	if !strings.Contains(got, "proxycommand ") || !strings.Contains(got, "jump.example.test") || !strings.Contains(got, "jump_config") {
		t.Fatalf("ssh -G did not preserve the configured jump route:\n%s", output)
	}
	jumpConfig, err := os.ReadFile(filepath.Join(session.dir, "jump_config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"ClearAllForwardings yes", "ControlMaster no", "PermitLocalCommand no", "BatchMode yes", filepath.Join(home, ".ssh", "config")} {
		if !strings.Contains(string(jumpConfig), value) {
			t.Fatalf("jump config missing %q:\n%s", value, jumpConfig)
		}
	}
	for _, directive := range []string{"localforward ", "remoteforward ", "dynamicforward ", "echo inherited"} {
		if strings.Contains(got, directive) {
			t.Fatalf("ssh -G inherited session directive %q:\n%s", directive, output)
		}
	}
}

func TestSSHTransportRouteParserPreservesAuthenticationFiles(t *testing.T) {
	route := parseSSHTransportConfigRoute("identityfile /home/alice/.ssh/first\nidentityfile ${HOME}/.ssh/key-%n-%k-%j-%C-%h-%%n\nidentitiesonly yes\ncertificatefile /home/alice/.ssh/id-cert-%n.pub\n", "/private/config")
	if got, want := route.identityFiles, []string{"/home/alice/.ssh/first", "${HOME}/.ssh/key-%n-%k-%j-%C-%h-%%n"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("identity files=%#v, want %#v", got, want)
	}
	if !route.identitiesOnly || !reflect.DeepEqual(route.certificateFiles, []string{"/home/alice/.ssh/id-cert-%n.pub"}) {
		t.Fatalf("route=%#v", route)
	}
	route.identityFiles = []string{"/home/alice/.ssh/first", "/home/alice/.ssh/key-proxy-alias"}
	route.certificateFiles = []string{"/home/alice/.ssh/id-cert-proxy-alias.pub"}
	config, err := renderSSHTransportConfigWithRoute(SSHTarget{User: "alice", Host: "proxy-alias", Port: "22"}, false, route)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		`IdentityFile "/home/alice/.ssh/first"`,
		`IdentityFile "/home/alice/.ssh/key-proxy-alias"`,
		"IdentitiesOnly yes",
		`CertificateFile "/home/alice/.ssh/id-cert-proxy-alias.pub"`,
	} {
		if !strings.Contains(config, value) {
			t.Fatalf("private config missing %q:\n%s", value, config)
		}
	}
}

func TestSSHTransportConfigProxyPreservesIdentityNone(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX OpenSSH config fixture")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH client is required")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte("Host no-identity\n  IdentityFile none\n  IdentitiesOnly yes\n  IdentityAgent none\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := newSSHTransportSession(t.Context(), SSHTarget{User: "alice", Host: "no-identity", Port: "22", SSHConfigProxy: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	config, err := os.ReadFile(session.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), `IdentityFile "none"`) {
		t.Fatalf("private config did not preserve IdentityFile none:\n%s", config)
	}
	if !strings.Contains(string(config), `IdentityAgent "none"`) {
		t.Fatalf("private config did not preserve IdentityAgent none:\n%s", config)
	}
}

func TestSSHTransportControlPathParserPreservesSpaces(t *testing.T) {
	path, ok := parseSSHTransportControlPath("hostname example.test\ncontrolpath /tmp/key alias-hash\n")
	if !ok || path != "/tmp/key alias-hash" {
		t.Fatalf("path=%q ok=%v", path, ok)
	}
}

func TestSSHTransportUserPercentCapabilityParser(t *testing.T) {
	for output, want := range map[string]bool{
		"user crabbox%probe\n":  true,
		"user crabbox%%probe\n": false,
	} {
		got, err := parseSSHTransportUserPercentExpansion(output)
		if err != nil || got != want {
			t.Fatalf("output=%q got=%v err=%v, want %v", output, got, err, want)
		}
	}
}

func TestExpandSSHTransportHomeTokenPreservesEscapedPercent(t *testing.T) {
	if got, want := expandSSHTransportHomeToken(`%d/.ssh/key-%%d-%%%d`, `/home/alice%ops`), `/home/alice%%ops/.ssh/key-%%d-%%/home/alice%%ops`; got != want {
		t.Fatalf("expanded=%q, want %q", got, want)
	}
}

func TestSSHTransportRouteSeedDoesNotInterpretAliasAsPattern(t *testing.T) {
	seed, err := renderSSHTransportRouteSeed(SSHTarget{User: "alice", Port: "22"}, "/tmp/user-config", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(seed, "Host ") {
		t.Fatalf("route seed contains Host pattern:\n%s", seed)
	}
	for _, value := range []string{`User "alice"`, `Port "22"`, `Include "/tmp/user-config"`} {
		if !strings.Contains(seed, value) {
			t.Fatalf("route seed missing %q:\n%s", value, seed)
		}
	}
}

func TestSSHTransportRouteParserLimitsNoneSentinelToProxySettings(t *testing.T) {
	route := parseSSHTransportConfigRoute("hostname none\nhostkeyalias none\nproxyjump none\nproxycommand none\n", "/private/config")
	if route.hostName != "none" || route.hostKeyAlias != "none" {
		t.Fatalf("route=%#v", route)
	}
	if route.proxyJump != "" || route.proxyCommand != "" {
		t.Fatalf("proxy sentinels not cleared: %#v", route)
	}
}

func TestSSHTransportRouteProbeKeepsSecretUserOutOfArgs(t *testing.T) {
	target := SSHTarget{User: "secret-token-user", Host: "proxy-alias", Port: "22", AuthSecret: true}
	args := sshTransportRouteCommandArgs("/private/seed-config", target, false, sshTransportRouteCapabilities{remoteCommand: true, sessionType: true})
	if strings.Contains(strings.Join(args, " "), target.User) {
		t.Fatalf("route probe args leaked secret user: %#v", args)
	}
	seed, err := renderSSHTransportRouteSeed(target, "/private/user-config", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seed, `User "secret-token-user"`) {
		t.Fatalf("private seed missing user: %s", seed)
	}
}

func TestSSHTransportRouteProbeMatchesSessionMode(t *testing.T) {
	target := SSHTarget{Host: "proxy-alias"}
	capabilities := sshTransportRouteCapabilities{remoteCommand: true, sessionType: true}
	execArgs := sshTransportRouteCommandArgs("/private/config", target, false, capabilities)
	for _, option := range []string{"RemoteCommand=none", "SessionType=default"} {
		if !containsString(execArgs, option) {
			t.Fatalf("exec route args missing %q: %#v", option, execArgs)
		}
	}
	if got := execArgs[len(execArgs)-1]; got != "rsync --server" {
		t.Fatalf("exec route command=%q; args=%#v", got, execArgs)
	}
	forwardArgs := sshTransportRouteCommandArgs("/private/config", target, true, capabilities)
	if !containsString(forwardArgs, "-N") || !containsString(forwardArgs, "RemoteCommand=none") || !containsString(forwardArgs, "SessionType=none") || containsString(forwardArgs, "rsync --server") {
		t.Fatalf("forward route args=%#v", forwardArgs)
	}
	wslArgs := sshTransportRouteCommandArgs("/private/config", SSHTarget{Host: "proxy-alias", TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, false, capabilities)
	if got := wslArgs[len(wslArgs)-1]; got != "wsl.exe rsync --server" {
		t.Fatalf("WSL route command=%q; args=%#v", got, wslArgs)
	}
	legacyArgs := sshTransportRouteCommandArgs("/private/config", target, false, sshTransportRouteCapabilities{})
	for _, option := range []string{"RemoteCommand=none", "SessionType=default", "IgnoreUnknown"} {
		if containsString(legacyArgs, option) {
			t.Fatalf("legacy route args contain unsupported option %q: %#v", option, legacyArgs)
		}
	}
}

func TestSSHTransportRouteCapabilitiesAreProbedWithoutUserConfig(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX executable fixture")
	}
	dir := t.TempDir()
	ssh := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\ncase \"$*\" in\n  *RemoteCommand=none*) exit 0 ;;\n  *SessionType=default*) exit 1 ;;\n  *) exit 2 ;;\nesac\n"
	if err := os.WriteFile(ssh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	capabilities := probeSSHTransportRouteCapabilities(t.Context(), SSHTarget{}, "/private/isolated-config")
	if !capabilities.remoteCommand || capabilities.sessionType {
		t.Fatalf("capabilities=%#v", capabilities)
	}
}

func TestSSHTransportRouteFailureRedactsSecretUser(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX executable fixture")
	}
	dir := t.TempDir()
	ssh := filepath.Join(dir, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nprintf 'route failed for secret-token-user\\n' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("HOME", t.TempDir())
	target := SSHTarget{User: "secret-token-user", Host: "proxy-alias", Port: "22", AuthSecret: true, SSHConfigProxy: true}
	_, err := resolveSSHTransportConfigRoute(t.Context(), target, false, false)
	if err == nil {
		t.Fatal("expected route resolution failure")
	}
	if strings.Contains(err.Error(), target.User) {
		t.Fatalf("route failure leaked secret user: %v", err)
	}
}

func TestSSHTransportRouteRejectsUnsafeAliasBeforeMatchExec(t *testing.T) {
	_, err := resolveSSHTransportConfigRoute(t.Context(), SSHTarget{User: "alice", Host: "[prod]", Port: "22", SSHConfigProxy: true}, false, false)
	if err == nil || !strings.Contains(err.Error(), "unsafe for ProxyCommand") {
		t.Fatalf("err=%v", err)
	}
}

func TestSSHTransportConfigRejectsEnvironmentExpansionInLiteral(t *testing.T) {
	_, err := renderSSHTransportConfig(SSHTarget{User: "token-${ID}", Host: "example.test", Port: "22"}, false)
	if err == nil || !strings.Contains(err.Error(), "environment expansion") {
		t.Fatalf("err=%v", err)
	}
	_, err = renderSSHTransportRouteSeed(SSHTarget{User: "alice", Port: "22"}, `/tmp/${CONFIG}`, false)
	if err == nil || !strings.Contains(err.Error(), "environment expansion") {
		t.Fatalf("seed err=%v", err)
	}
}

func TestSSHTransportRoutePreservesOriginalHostAndFDPass(t *testing.T) {
	target := SSHTarget{User: "alice", Host: "proxy-alias", Port: "22"}
	config, err := renderSSHTransportConfigWithRoute(target, false, sshTransportConfigRoute{
		hostName:       "routed.example.test",
		proxyCommand:   "broker --target %n --host %h --port %p",
		proxyUseFDPass: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"HostName \"routed.example.test\"",
		"ProxyCommand broker --target proxy-alias --host %h --port %p",
		"ProxyUseFdpass yes",
	} {
		if !strings.Contains(config, value) {
			t.Fatalf("private config missing %q:\n%s", value, config)
		}
	}
}

func TestSSHTransportLiteralFieldsEscapeOnlyTokenDirectives(t *testing.T) {
	target := SSHTarget{
		User:            `DOMAIN\user%h`,
		Host:            "host%p",
		Port:            "22",
		Key:             "/tmp/key%n",
		CertificateFile: "/tmp/cert%r",
		KnownHostsFile:  "/tmp/known%h",
	}
	config, err := renderSSHTransportConfigWithRoute(target, false, sshTransportConfigRoute{userPercentExpansion: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`DOMAIN\user%%h`, "host%%p", "key%%n", "cert%%r", "known%%h"} {
		if !strings.Contains(config, value) {
			t.Fatalf("private config did not escape %q:\n%s", value, config)
		}
	}
	legacyConfig, err := renderSSHTransportConfigWithRoute(target, false, sshTransportConfigRoute{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(legacyConfig, `User "DOMAIN\user%h"`) || strings.Contains(legacyConfig, `User "DOMAIN\user%%h"`) {
		t.Fatalf("pre-10 OpenSSH user was incorrectly escaped:\n%s", legacyConfig)
	}
}

func TestProxyJumpCommandPreservesMultiHopChainAndOwnership(t *testing.T) {
	dir := t.TempDir()
	userConfig := filepath.Join(dir, "user_config")
	if err := os.WriteFile(userConfig, []byte("Host *\n  ProxyUseFdpass yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := writeSSHTransportJumpConfig(dir, userConfig, "jump-a,jump-b,jump-c", true)
	if err != nil {
		t.Fatal(err)
	}
	got := proxyJumpCommand(sshTransportConfigRoute{proxyJump: "jump-a,jump-b,jump-c", jumpConfigPath: path})
	for _, value := range []string{"/jump_config'", "'jump-c'", "'ControlMaster=no'", "'PermitLocalCommand=no'", "'BatchMode=yes'"} {
		if !strings.Contains(got, value) {
			t.Fatalf("jump command missing %q: %s", value, got)
		}
	}
	if strings.Contains(got, "'-J'") {
		t.Fatalf("jump command delegated an unsafe implicit hop: %s", got)
	}
	for index, hop := range []string{"", "jump-a", "jump-b"} {
		name := fmt.Sprintf("jump_config_%d", index)
		if index == 2 {
			name = "jump_config"
		}
		config, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []string{"ControlMaster no", "PermitLocalCommand no", "BatchMode yes", userConfig} {
			if !strings.Contains(string(config), value) {
				t.Fatalf("%s missing %q:\n%s", name, value, config)
			}
		}
		if hop != "" && (!strings.Contains(string(config), hop) || !strings.Contains(string(config), "ProxyCommand")) {
			t.Fatalf("%s did not proxy through %s:\n%s", name, hop, config)
		}
		if hop != "" && !strings.Contains(string(config), "ProxyUseFdpass no") {
			t.Fatalf("%s allowed inherited FD passing:\n%s", name, config)
		}
	}
}

func TestSSHTransportConfigsHonorRemoteCommandCapability(t *testing.T) {
	legacyJump := renderSSHTransportJumpConfig("/tmp/user-config", "", false)
	if strings.Contains(legacyJump, "RemoteCommand") {
		t.Fatalf("legacy jump config contains unsupported RemoteCommand:\n%s", legacyJump)
	}
	modernJump := renderSSHTransportJumpConfig("/tmp/user-config", "", true)
	if !strings.Contains(modernJump, "RemoteCommand none") {
		t.Fatalf("modern jump config did not neutralize inherited RemoteCommand:\n%s", modernJump)
	}
	finalConfig, err := renderSSHTransportConfig(SSHTarget{User: "alice", Host: "example.test", Port: "22"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(finalConfig, "RemoteCommand") {
		t.Fatalf("owned final config contains an unnecessary version-specific RemoteCommand:\n%s", finalConfig)
	}
}

func TestProxyJumpCommandPreservesFinalHopUserAndPort(t *testing.T) {
	dir := t.TempDir()
	path, err := writeSSHTransportJumpConfig(dir, "", "jump-a,alice@jump.example.test:2222", true)
	if err != nil {
		t.Fatal(err)
	}
	got := proxyJumpCommand(sshTransportConfigRoute{proxyJump: "jump-a,alice@jump.example.test:2222", jumpConfigPath: path})
	for _, value := range []string{"'-p' '2222'", "'alice@jump.example.test'"} {
		if !strings.Contains(got, value) {
			t.Fatalf("jump command missing %q: %s", value, got)
		}
	}
	config, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "ProxyCommand") || !strings.Contains(string(config), "jump-a") {
		t.Fatalf("final hop config did not preserve the preceding hop:\n%s", config)
	}
}

func TestProxyJumpConfigChainGrowsLinearly(t *testing.T) {
	hops := make([]string, 100)
	for index := range hops {
		hops[index] = fmt.Sprintf("jump-%d.example.test", index)
	}
	dir := t.TempDir()
	path, err := writeSSHTransportJumpConfig(dir, "", strings.Join(hops, ","), true)
	if err != nil {
		t.Fatal(err)
	}
	command := proxyJumpCommand(sshTransportConfigRoute{proxyJump: strings.Join(hops, ","), jumpConfigPath: path})
	if len(command) > 4096 {
		t.Fatalf("outer jump command grew with chain: %d bytes", len(command))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(hops) {
		t.Fatalf("jump configs=%d, want %d", len(entries), len(hops))
	}
	total := 0
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += int(info.Size())
	}
	if total > len(hops)*2048 {
		t.Fatalf("jump config chain grew superlinearly: %d bytes for %d hops", total, len(hops))
	}
}

func TestProxyJumpCommandExpandsOriginalHost(t *testing.T) {
	config, err := renderSSHTransportConfigWithRoute(
		SSHTarget{User: "alice", Host: "original.example.test", Port: "22"},
		false,
		sshTransportConfigRoute{proxyJump: "jump-%n"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(config, "jump-original.example.test") || strings.Contains(config, "jump-%n") {
		t.Fatalf("config did not expand original host:\n%s", config)
	}
}

func TestProxyJumpExpansionQuotesResolvedTokensBeforeShell(t *testing.T) {
	expanded := expandSSHProxyJumpTokens("%r@jump-%n:%p", "[prod]", "routed.example.test", "token-user", "2222")
	if expanded != "token-user@jump-[prod]:2222" {
		t.Fatalf("expanded=%q", expanded)
	}
}

func TestSSHTransportConfigRejectsUnsafeProxyTokens(t *testing.T) {
	_, err := renderSSHTransportConfig(SSHTarget{
		User:         "alice",
		Host:         "host;touch-pwn",
		Port:         "22",
		ProxyCommand: "provider-proxy %n %p",
	}, false)
	if err == nil || !strings.Contains(err.Error(), "unsafe for ProxyCommand") {
		t.Fatalf("err=%v", err)
	}
	_, err = renderSSHTransportConfig(SSHTarget{
		User:         "token;touch-pwn",
		Host:         "example.test",
		Port:         "22",
		ProxyCommand: "provider-proxy %r %p",
	}, false)
	if err == nil || !strings.Contains(err.Error(), "unsafe for ProxyCommand") {
		t.Fatalf("user err=%v", err)
	}
}

func TestRsyncRemoteShellQuotesApostropheForRsyncParser(t *testing.T) {
	session := &sshTransportSession{configPath: `/tmp/O'Brien/ssh config`}
	if got, want := session.rsyncRemoteShell(), `'ssh' '-F' '/tmp/O''Brien/ssh config'`; got != want {
		t.Fatalf("remote shell=%q, want %q", got, want)
	}
}

func TestProxyJumpHopArgsPreservesIPv6Port(t *testing.T) {
	want := []string{"-p", "2222", "alice@2001:db8::1"}
	if got := proxyJumpHopArgs("alice@[2001:db8::1]:2222"); !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%#v, want %#v", got, want)
	}
}

func TestProxyJumpHopArgsPreservesURI(t *testing.T) {
	for _, uri := range []string{"ssh://alice@jump.example.test:2222", "ssh://alice@[2001:db8::1]:2222"} {
		if got := proxyJumpHopArgs(uri); !reflect.DeepEqual(got, []string{uri}) {
			t.Fatalf("URI %q args=%#v", uri, got)
		}
	}
}

func TestSSHTransportRouteResolutionHonorsCancellation(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX fake SSH helper")
	}
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	if err := os.WriteFile(sshPath, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := newSSHTransportSession(ctx, SSHTarget{User: "alice", Host: "proxy-alias", Port: "22", SSHConfigProxy: true}, false)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("route cancellation took %s", elapsed)
	}
}

func TestSSHTransportConfigRejectsDirectiveInjection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := renderSSHTransportConfig(SSHTarget{User: "alice\nProxyCommand leak", Host: "example.test", Port: "22"}, false)
	if err == nil || !strings.Contains(err.Error(), "control character") {
		t.Fatalf("err=%v", err)
	}
}

func TestWSLSSHTransportTargetUsesProtectedLinuxPaths(t *testing.T) {
	target := SSHTarget{
		Key:             `C:\Users\alice\.config\crabbox\id_ed25519`,
		CertificateFile: `C:\Users\alice\.config\crabbox\id_ed25519-cert.pub`,
		KnownHostsFile:  `C:\Users\alice\.config\crabbox\known_hosts`,
		ProxyCommand:    `C:\Tools\provider.exe proxy %h %p`,
	}
	got := wslSSHTransportTarget(target, "/tmp/crabbox-private", "/mnt")
	if got.Key != "/tmp/crabbox-private/identity" || got.CertificateFile != "/tmp/crabbox-private/identity-cert.pub" {
		t.Fatalf("protected identity paths: %#v", got)
	}
	if got.KnownHostsFile != "/mnt/c/Users/alice/.config/crabbox/known_hosts" {
		t.Fatalf("known hosts path=%q", got.KnownHostsFile)
	}
	if got.ProxyCommand != "/mnt/c/Tools/provider.exe proxy %h %p" {
		t.Fatalf("proxy command=%q", got.ProxyCommand)
	}
}

func TestWSLSSHTransportSessionStagesAndRemovesPrivateFiles(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX WSL fixture")
	}
	dir := t.TempDir()
	wsl := filepath.Join(dir, "wsl")
	wslScript := "#!/bin/sh\ncase \"$*\" in *mktemp*) printf 'harmless WSL warning\\n' >&2 ;; esac\nexec \"$@\"\n"
	if err := os.WriteFile(wsl, []byte(wslScript), 0o755); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "identity")
	certificate := filepath.Join(dir, "identity-cert.pub")
	knownHosts := filepath.Join(dir, "known_hosts")
	for path, data := range map[string]string{key: "private-key", certificate: "certificate", knownHosts: "host-key"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	target := SSHTarget{
		User:            "alice",
		Host:            "example.test",
		Port:            "22",
		Key:             key,
		CertificateFile: certificate,
		KnownHostsFile:  knownHosts,
	}
	session, err := newWSLSSHTransportSession(t.Context(), target, wsl, "/mnt")
	if err != nil {
		t.Fatal(err)
	}
	configPath := session.configPath
	t.Cleanup(func() { _ = session.Close() })
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), `IdentityFile "/tmp/`) || !strings.Contains(string(config), `CertificateFile "/tmp/`) {
		t.Fatalf("WSL config does not use staged identities: %s", config)
	}
	if info, err := os.Stat(configPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("WSL config mode: info=%v err=%v", info, err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("WSL private config remained: %v", err)
	}
}

func TestWSLSSHTransportSessionRejectsConfigProxyRoute(t *testing.T) {
	_, err := newWSLSSHTransportSession(t.Context(), SSHTarget{SSHConfigProxy: true}, "wsl", "/mnt")
	if err == nil || !strings.Contains(err.Error(), "require native OpenSSH") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidWSLSSHTransportDirectory(t *testing.T) {
	for _, value := range []string{"/tmp/crabbox-ssh-transport-abc123", "/tmp/crabbox-ssh-transport-1234567890"} {
		if !validWSLSSHTransportDirectory(value) {
			t.Fatalf("valid path rejected: %q", value)
		}
	}
	for _, value := range []string{"", "/tmp/crabbox-ssh-transport-short", "/tmp/other-abc123", "/tmp/crabbox-ssh-transport-abc123/child", "/tmp/crabbox-ssh-transport-abc 123", "/home/alice/crabbox-ssh-transport-abc123"} {
		if validWSLSSHTransportDirectory(value) {
			t.Fatalf("unsafe path accepted: %q", value)
		}
	}
}

func TestWSLSSHTransportSessionDoesNotCleanUpFailedDirectoryCreation(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX WSL fixture")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "cleanup-ran")
	wsl := filepath.Join(dir, "wsl")
	script := "#!/bin/sh\ncase \"$*\" in\n  *'rm -rf'*) : > \"$CRABBOX_TEST_WSL_CLEANUP_MARKER\" ;;\nesac\nexit 1\n"
	if err := os.WriteFile(wsl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_TEST_WSL_CLEANUP_MARKER", marker)
	_, err := newWSLSSHTransportSession(t.Context(), SSHTarget{}, wsl, "/mnt")
	if err == nil || !strings.Contains(err.Error(), "create private WSL SSH transport directory") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("cleanup ran without directory ownership: %v", err)
	}
}

func TestWSLSSHTransportSessionSurfacesStagedCredentialCleanupFailure(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX WSL fixture")
	}
	dir := t.TempDir()
	wslDir := "/tmp/crabbox-ssh-transport-cleanupfail"
	t.Cleanup(func() { _ = os.RemoveAll(wslDir) })
	wsl := filepath.Join(dir, "wsl")
	script := "#!/bin/sh\ncase \"$*\" in\n  *mktemp*) mkdir -p \"$CRABBOX_TEST_WSL_DIR\"; printf '%s\\n' \"$CRABBOX_TEST_WSL_DIR\" ;;\n  *'rm -rf'*) exit 42 ;;\n  *) exec \"$@\" ;;\nesac\n"
	if err := os.WriteFile(wsl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_TEST_WSL_DIR", wslDir)
	key := filepath.Join(dir, "identity")
	if err := os.WriteFile(key, []byte("private-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := SSHTarget{
		User:            "alice",
		Host:            "example.test",
		Port:            "22",
		Key:             key,
		CertificateFile: filepath.Join(dir, "missing-certificate"),
	}
	_, err := newWSLSSHTransportSession(t.Context(), target, wsl, "/mnt")
	if err == nil || !strings.Contains(err.Error(), "read SSH certificate for WSL copy transport") || !strings.Contains(err.Error(), "remove private WSL SSH transport config") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(wslDir, "identity")); statErr != nil {
		t.Fatalf("fixture did not stage the credential before cleanup failed: %v", statErr)
	}
}

func TestPrivateSSHTransportProbeKeepsSecretsOutOfArgvAndEnvironment(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX fake SSH helper")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "capture")
	sshPath := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\nset -eu\nprintf 'args=%s\\n' \"$*\" > \"$CRABBOX_TEST_SSH_CAPTURE\"\nprintf 'denied=%s\\n' \"${CRABBOX_TEST_DENIED-secret-missing}\" >> \"$CRABBOX_TEST_SSH_CAPTURE\"\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRABBOX_TEST_SSH_CAPTURE", capture)
	t.Setenv("CRABBOX_TEST_DENIED", "environment-secret")
	target := SSHTarget{
		User:             "username-secret",
		Host:             "example.test",
		Port:             "22",
		AuthSecret:       true,
		ProxyCommand:     "provider proxy --session session-123 %h %p",
		ChildEnvDenylist: []string{"CRABBOX_TEST_DENIED"},
	}
	if !probePrivateSSHTransport(t.Context(), &target, 5*time.Second) {
		t.Fatal("private SSH transport probe failed")
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, secret := range []string{"username-secret", "environment-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("probe leaked %q: %q", secret, got)
		}
	}
	if !strings.Contains(got, "secret-missing") || !strings.Contains(got, sshTransportHostAlias) {
		t.Fatalf("probe capture=%q", got)
	}
}

func TestPrivateSSHTransportProbeBudgetsEachFallbackPort(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX fake SSH helper")
	}
	dir := t.TempDir()
	attempts := filepath.Join(dir, "attempts")
	sshPath := filepath.Join(dir, "ssh")
	script := `#!/bin/sh
set -eu
config=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-F" ]; then config="$2"; shift 2; else shift; fi
done
port=$(awk '$1 == "Port" {gsub(/"/, "", $2); print $2; exit}' "$config")
printf '%s\n' "$port" >> "$CRABBOX_TEST_SSH_ATTEMPTS"
if [ "$port" = "2201" ]; then exec sleep 10; fi
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRABBOX_TEST_SSH_ATTEMPTS", attempts)
	target := SSHTarget{User: "alice", Host: "example.test", Port: "2201", FallbackPorts: []string{"2202"}}
	if !probePrivateSSHTransport(t.Context(), &target, 5*time.Second) {
		t.Fatal("fallback SSH transport probe failed")
	}
	if target.Port != "2202" {
		t.Fatalf("selected port=%q", target.Port)
	}
	data, err := os.ReadFile(attempts)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(data)); len(got) != 2 || got[0] != "2201" || got[1] != "2202" {
		t.Fatalf("probe attempts=%q", data)
	}
}
