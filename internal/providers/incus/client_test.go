package incus

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/cliconfig"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"golang.org/x/oauth2"
)

type fakeOIDCTokenSource struct {
	tokens *oidc.Tokens[*oidc.IDTokenClaims]
}

func (s fakeOIDCTokenSource) GetOIDCTokens() *oidc.Tokens[*oidc.IDTokenClaims] {
	return s.tokens
}

func TestDoctorConnectionInfoForConfigUsesNamedRemoteMetadata(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := filepath.Join(home, ".config", "incus")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	config := "default-remote: lab\nremotes:\n  lab:\n    addr: https://incus.example.test:8443\n    auth_type: oidc\n    protocol: incus\n    project: qa\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.yml"), []byte(config), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Remote = "lab"
	cfg.Incus.Project = ""

	info, err := doctorConnectionInfoForConfig(cfg)
	if err != nil {
		t.Fatalf("doctorConnectionInfoForConfig: %v", err)
	}
	if info.Mode != "remote" {
		t.Fatalf("Mode=%q want remote", info.Mode)
	}
	if info.Remote != "lab" {
		t.Fatalf("Remote=%q want lab", info.Remote)
	}
	if info.Endpoint != "https://incus.example.test:8443" {
		t.Fatalf("Endpoint=%q want https://incus.example.test:8443", info.Endpoint)
	}
	if info.Auth != "oidc" {
		t.Fatalf("Auth=%q want oidc", info.Auth)
	}
	if info.Project != "qa" {
		t.Fatalf("Project=%q want qa", info.Project)
	}
	if info.Protocol != "incus" {
		t.Fatalf("Protocol=%q want incus", info.Protocol)
	}
}

func TestDoctorConnectionInfoForConfigUsesAuthenticatedAddressMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := writeIncusConfig(t, home, "default-remote: trusted\nremotes:\n  trusted:\n    addr: https://incus.example.test:8443\n    protocol: incus\n")
	writeClientCertificateFiles(t, configDir)

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Address = "https://incus.example.test:8443"
	cfg.Incus.InsecureTLS = true

	info, err := doctorConnectionInfoForConfig(cfg)
	if err != nil {
		t.Fatalf("doctorConnectionInfoForConfig: %v", err)
	}
	if info.Mode != "address" {
		t.Fatalf("Mode=%q want address", info.Mode)
	}
	if info.Endpoint != "https://incus.example.test:8443" {
		t.Fatalf("Endpoint=%q want https://incus.example.test:8443", info.Endpoint)
	}
	if info.Auth != "tls_client_cert_insecure_tls" {
		t.Fatalf("Auth=%q want tls_client_cert_insecure_tls", info.Auth)
	}
	if info.Project != "default" {
		t.Fatalf("Project=%q want default", info.Project)
	}
	if info.Protocol != "incus" {
		t.Fatalf("Protocol=%q want incus", info.Protocol)
	}
}

func TestDoctorConnectionInfoForConfigRejectsAddressWithoutClientAuthentication(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Address = "https://incus.example.test:8443"
	cfg.Incus.InsecureTLS = true

	if _, err := doctorConnectionInfoForConfig(cfg); err == nil {
		t.Fatal("doctorConnectionInfoForConfig accepted address mode without client authentication")
	}
}

func TestConnectionArgsForAddressDoesNotReuseRemoteTLSClientCertForDifferentAddress(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := writeIncusConfig(t, home, "default-remote: trusted\nremotes:\n  trusted:\n    addr: https://trusted.example.test:8443\n    protocol: incus\n")
	writeClientCertificateFiles(t, configDir)

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Remote = "trusted"
	cfg.Incus.Address = "https://attacker.example.test:8443"
	cfg.Incus.InsecureTLS = true

	_, err := connectionArgsForAddress(cfg)
	exitErr := requireExitCode(t, err, 2)
	if !strings.Contains(exitErr.Message, "matching authenticated Incus remote") {
		t.Fatalf("unexpected error: %v", exitErr)
	}
}

func TestConnectionArgsForAddressUsesMatchingRemoteTLSClientCert(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := writeIncusConfig(t, home, "default-remote: trusted\nremotes:\n  trusted:\n    addr: https://trusted.example.test:8443\n    protocol: incus\n")
	writeClientCertificateFiles(t, configDir)

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Remote = "trusted"
	cfg.Incus.Address = "trusted.example.test:8443"

	args, err := connectionArgsForAddress(cfg)
	if err != nil {
		t.Fatalf("connectionArgsForAddress: %v", err)
	}
	if args.TLSClientCert == "" || args.TLSClientKey == "" {
		t.Fatalf("expected matching remote TLS credentials, got %#v", args)
	}
}

func TestConnectionArgsForAddressUsesMatchingRemoteOIDCTokens(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := writeIncusConfig(t, home, "default-remote: staging\nremotes:\n  staging:\n    addr: https://staging.incus.example.test:8443\n    auth_type: oidc\n    protocol: incus\n")
	writeOIDCTokenFile(t, configDir, "staging", oidc.Tokens[*oidc.IDTokenClaims]{
		Token: &oauth2.Token{
			AccessToken:  "forged-access-token",
			TokenType:    "Bearer",
			RefreshToken: "forged-refresh-token",
			Expiry:       time.Now().Add(time.Hour).UTC(),
		},
		IDToken: "forged-id-token",
	})

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Remote = "staging"
	cfg.Incus.Address = "https://staging.incus.example.test:8443"

	args, err := connectionArgsForAddress(cfg)
	if err != nil {
		t.Fatalf("connectionArgsForAddress: %v", err)
	}
	if args.AuthType != "oidc" {
		t.Fatalf("AuthType=%q want oidc", args.AuthType)
	}
	if args.OIDCTokens == nil || args.OIDCTokens.AccessToken != "forged-access-token" || args.OIDCTokens.IDToken != "forged-id-token" {
		t.Fatalf("unexpected OIDC tokens: %#v", args.OIDCTokens)
	}
}

func TestConnectionArgsForAddressPersistsRefreshedOIDCTokens(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := writeIncusConfig(t, home, "default-remote: staging\nremotes:\n  staging:\n    addr: https://staging.incus.example.test:8443\n    auth_type: oidc\n    protocol: incus\n")
	writeOIDCTokenFile(t, configDir, "staging", oidc.Tokens[*oidc.IDTokenClaims]{
		Token: &oauth2.Token{AccessToken: "old-access-token"},
	})

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Remote = "staging"
	cfg.Incus.Address = "https://staging.incus.example.test:8443"

	args, tokenPath, err := connectionArgsForAddressWithTokenPath(cfg)
	if err != nil {
		t.Fatalf("connectionArgsForAddressWithTokenPath: %v", err)
	}
	args.OIDCTokens.AccessToken = "refreshed-access-token"
	_, save, err := oidcTokenCallbacks(fakeOIDCTokenSource{tokens: args.OIDCTokens}, tokenPath)
	if err != nil {
		t.Fatalf("oidcTokenCallbacks: %v", err)
	}
	if save == nil {
		t.Fatal("OIDC save callback is nil")
	}
	if err := save(); err != nil {
		t.Fatalf("save OIDC tokens: %v", err)
	}

	content, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var saved oidc.Tokens[*oidc.IDTokenClaims]
	if err := json.Unmarshal(content, &saved); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if saved.AccessToken != "refreshed-access-token" {
		t.Fatalf("AccessToken=%q want refreshed-access-token", saved.AccessToken)
	}
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode=%#o want 0600", info.Mode().Perm())
	}
}

func TestOIDCTokenCallbacksReloadLatestTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := writeOIDCTokens(path, &oidc.Tokens[*oidc.IDTokenClaims]{
		Token: &oauth2.Token{AccessToken: "latest-access-token"},
	}); err != nil {
		t.Fatalf("writeOIDCTokens: %v", err)
	}
	current := &oidc.Tokens[*oidc.IDTokenClaims]{
		Token: &oauth2.Token{AccessToken: "stale-access-token"},
	}
	reload, _, err := oidcTokenCallbacks(fakeOIDCTokenSource{tokens: current}, path)
	if err != nil {
		t.Fatalf("oidcTokenCallbacks: %v", err)
	}
	if err := reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if current.AccessToken != "latest-access-token" {
		t.Fatalf("AccessToken=%q want latest-access-token", current.AccessToken)
	}
}

func TestSDKClientPersistsTokensAfterOperation(t *testing.T) {
	calls := 0
	client := &sdkClient{
		saveOIDCTokens: func() error {
			calls++
			return nil
		},
	}
	if err := client.persistResult(nil); err != nil {
		t.Fatalf("persistResult: %v", err)
	}
	if calls != 1 {
		t.Fatalf("save calls=%d want 1", calls)
	}
}

func TestSDKClientPreservesCommittedMutationWhenTokenSaveFails(t *testing.T) {
	calls := 0
	client := &sdkClient{
		saveOIDCTokens: func() error {
			calls++
			return errors.New("disk full")
		},
	}
	client.persistCommittedMutation()
	if calls != 1 {
		t.Fatalf("save calls=%d want 1", calls)
	}
}

func TestDisableOIDCKeepAlive(t *testing.T) {
	clientConfig := &cliconfig.Config{
		Remotes: map[string]cliconfig.Remote{
			"oidc": {AuthType: "oidc", KeepAlive: 30},
			"tls":  {AuthType: "tls", KeepAlive: 30},
		},
	}

	disableOIDCKeepAlive(clientConfig, "oidc")
	disableOIDCKeepAlive(clientConfig, "tls")

	if got := clientConfig.Remotes["oidc"].KeepAlive; got != 0 {
		t.Fatalf("OIDC KeepAlive=%d want 0", got)
	}
	if got := clientConfig.Remotes["tls"].KeepAlive; got != 30 {
		t.Fatalf("TLS KeepAlive=%d want 30", got)
	}
}

func TestDoctorConnectionInfoForConfigUsesAddressOIDCMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeIncusConfig(t, home, "default-remote: staging\nremotes:\n  staging:\n    addr: https://staging.incus.example.test:8443\n    auth_type: oidc\n    protocol: incus\n")

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Remote = "staging"
	cfg.Incus.Address = "https://staging.incus.example.test:8443"

	info, err := doctorConnectionInfoForConfig(cfg)
	if err != nil {
		t.Fatalf("doctorConnectionInfoForConfig: %v", err)
	}
	if info.Auth != "oidc" {
		t.Fatalf("Auth=%q want oidc", info.Auth)
	}
}

func TestConnectInstanceServerRejectsLocalUnixRemoteOnNonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("local unix remote is valid on linux")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := filepath.Join(home, ".config", "incus")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	config := "default-remote: local\nremotes:\n  local:\n    addr: unix://\n    protocol: incus\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.yml"), []byte(config), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Remote = "local"
	cfg.Incus.Project = ""

	if _, err := connectInstanceServer(cfg); err == nil || !strings.Contains(err.Error(), "not configured for a reachable Linux Incus daemon") {
		t.Fatalf("connectInstanceServer err=%v", err)
	}
}

func TestDoctorConnectionInfoRejectsLocalUnixRemoteOnNonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("local unix remote is valid on linux")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := filepath.Join(home, ".config", "incus")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	config := "default-remote: local\nremotes:\n  local:\n    addr: unix://\n    protocol: incus\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.yml"), []byte(config), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Remote = "local"
	cfg.Incus.Project = ""

	if _, err := doctorConnectionInfoForConfig(cfg); err == nil || !strings.Contains(err.Error(), "not configured for a reachable Linux Incus daemon") {
		t.Fatalf("doctorConnectionInfoForConfig err=%v", err)
	}
}

func TestConnectInstanceServerRejectsMalformedConfigWithExitCodeTwo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := filepath.Join(home, ".config", "incus")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yml"), []byte("default-remote: [\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := core.BaseConfig()
	cfg.Provider = providerName

	_, err := connectInstanceServer(cfg)
	exitErr := requireExitCode(t, err, 2)
	if !strings.Contains(exitErr.Message, "load incus client config") {
		t.Fatalf("unexpected error: %v", exitErr)
	}
}

func TestDoctorConnectionInfoRejectsMalformedConfigWithExitCodeTwo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := filepath.Join(home, ".config", "incus")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yml"), []byte("default-remote: [\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := core.BaseConfig()
	cfg.Provider = providerName

	_, err := doctorConnectionInfoForConfig(cfg)
	exitErr := requireExitCode(t, err, 2)
	if !strings.Contains(exitErr.Message, "load incus client config") {
		t.Fatalf("unexpected error: %v", exitErr)
	}
}

func TestDoctorConnectionInfoRejectsMissingRemoteWithExitCodeTwo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeIncusConfig(t, home, "default-remote: missing\nremotes: {}\n")

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Remote = ""

	_, err := doctorConnectionInfoForConfig(cfg)
	exitErr := requireExitCode(t, err, 2)
	if !strings.Contains(exitErr.Message, "remote not found") {
		t.Fatalf("unexpected error: %v", exitErr)
	}
}

func TestConnectionArgsForAddressRejectsMalformedConfigWithExitCodeTwo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := filepath.Join(home, ".config", "incus")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yml"), []byte("default-remote: [\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Address = "https://incus.example.test:8443"

	_, err := connectionArgsForAddress(cfg)
	exitErr := requireExitCode(t, err, 2)
	if !strings.Contains(exitErr.Message, "load incus client config for TLS credentials") {
		t.Fatalf("unexpected error: %v", exitErr)
	}
}

func TestConnectionArgsForAddressRejectsMissingServerCertWithExitCodeTwo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Address = "https://incus.example.test:8443"
	cfg.Incus.TLSServerCert = filepath.Join(t.TempDir(), "missing.crt")

	_, err := connectionArgsForAddress(cfg)
	exitErr := requireExitCode(t, err, 2)
	if !strings.Contains(exitErr.Message, "read incus TLS server cert") {
		t.Fatalf("unexpected error: %v", exitErr)
	}
}

func TestConnectionArgsForAddressSkipsOIDCWhenInsecureTLSEnabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := writeIncusConfig(t, home, "default-remote: staging\nremotes:\n  staging:\n    addr: https://staging.incus.example.test:8443\n    auth_type: oidc\n    protocol: incus\n")
	tokenDir := filepath.Join(configDir, "oidctokens")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatalf("MkdirAll oidctokens: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tokenDir, "staging.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("WriteFile token: %v", err)
	}

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Remote = "staging"
	cfg.Incus.Address = "https://staging.incus.example.test:8443"
	cfg.Incus.InsecureTLS = true

	_, err := connectionArgsForAddress(cfg)
	exitErr := requireExitCode(t, err, 2)
	if !strings.Contains(exitErr.Message, "matching authenticated Incus remote") {
		t.Fatalf("unexpected error: %v", exitErr)
	}
}

func TestConfiguredRemoteAddrFallsBackToFirstAddress(t *testing.T) {
	remote := cliconfig.Remote{
		Addrs: []string{"https://first.example.test:8443", "https://second.example.test:8443"},
	}
	if got := configuredRemoteAddr(remote); got != "https://first.example.test:8443" {
		t.Fatalf("configuredRemoteAddr=%q want first address", got)
	}

	remote.LastWorkingAddr = "https://last.example.test:8443"
	if got := configuredRemoteAddr(remote); got != "https://last.example.test:8443" {
		t.Fatalf("configuredRemoteAddr=%q want last working address", got)
	}
}

func TestSelectedProjectPrefersExplicitRemoteThenDefault(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Project = "explicit"

	if got := selectedProject(cfg, &cliconfig.Remote{Project: "remote"}); got != "explicit" {
		t.Fatalf("selectedProject=%q want explicit", got)
	}

	cfg.Incus.Project = ""
	if got := selectedProject(cfg, &cliconfig.Remote{Project: "remote"}); got != "remote" {
		t.Fatalf("selectedProject=%q want remote", got)
	}

	if got := selectedProject(cfg, nil); got != "default" {
		t.Fatalf("selectedProject=%q want default", got)
	}
}

func TestSSHHostForConfigFallsBackAcrossSources(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeIncusConfig(t, home, "default-remote: lab\nremotes:\n  lab:\n    addr: https://remote.example.test:8443\n    protocol: incus\n")

	cfg := core.BaseConfig()
	cfg.Provider = providerName

	cfg.Incus.ProxyListenHost = "198.51.100.8"
	if got := sshHostForConfig(cfg); got != "198.51.100.8" {
		t.Fatalf("sshHostForConfig explicit host=%q want 198.51.100.8", got)
	}

	cfg.Incus.ProxyListenHost = "0.0.0.0"
	cfg.Incus.Socket = "/var/lib/incus/unix.socket"
	if got := sshHostForConfig(cfg); got != "127.0.0.1" {
		t.Fatalf("sshHostForConfig socket host=%q want 127.0.0.1", got)
	}

	cfg.Incus.Socket = ""
	cfg.Incus.Address = "https://address.example.test:8443"
	if got := sshHostForConfig(cfg); got != "address.example.test" {
		t.Fatalf("sshHostForConfig address host=%q want address.example.test", got)
	}

	cfg.Incus.Address = ""
	cfg.Incus.Remote = "lab"
	if got := sshHostForConfig(cfg); got != "remote.example.test" {
		t.Fatalf("sshHostForConfig remote host=%q want remote.example.test", got)
	}
}

func TestHostFromAddrHandlesEmptyBareAndInvalidValues(t *testing.T) {
	if got := hostFromAddr(""); got != "" {
		t.Fatalf("hostFromAddr(empty)=%q want empty", got)
	}
	if got := hostFromAddr("incus.example.test:8443"); got != "incus.example.test" {
		t.Fatalf("hostFromAddr(bare)=%q want incus.example.test", got)
	}
	if got := hostFromAddr("://bad"); got != "" {
		t.Fatalf("hostFromAddr(invalid)=%q want empty", got)
	}
}

func requireExitCode(t *testing.T, err error, code int) core.ExitError {
	t.Helper()
	if err == nil {
		t.Fatal("expected exit error")
	}
	var exitErr core.ExitError
	if !core.AsExitError(err, &exitErr) {
		t.Fatalf("expected ExitError, got %T: %v", err, err)
	}
	if exitErr.Code != code {
		t.Fatalf("exit code=%d want %d (err=%v)", exitErr.Code, code, exitErr)
	}
	return exitErr
}

func writeIncusConfig(t *testing.T, home string, body string) string {
	t.Helper()
	configDir := filepath.Join(home, ".config", "incus")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yml"), []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	return configDir
}

func writeClientCertificateFiles(t *testing.T, configDir string) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(configDir, "client.crt"), certPEM, 0o600); err != nil {
		t.Fatalf("WriteFile client.crt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "client.key"), keyPEM, 0o600); err != nil {
		t.Fatalf("WriteFile client.key: %v", err)
	}
}

func writeOIDCTokenFile(t *testing.T, configDir string, remote string, tokens oidc.Tokens[*oidc.IDTokenClaims]) {
	t.Helper()
	tokenDir := filepath.Join(configDir, "oidctokens")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatalf("MkdirAll oidctokens: %v", err)
	}
	content, err := json.Marshal(tokens)
	if err != nil {
		t.Fatalf("Marshal tokens: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tokenDir, remote+".json"), content, 0o600); err != nil {
		t.Fatalf("WriteFile oidc token: %v", err)
	}
}
