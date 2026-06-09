package incus

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"golang.org/x/oauth2"
)

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

func TestDoctorConnectionInfoForConfigUsesAddressTrustMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

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
	if info.Auth != "insecure_tls" {
		t.Fatalf("Auth=%q want insecure_tls", info.Auth)
	}
	if info.Project != "default" {
		t.Fatalf("Project=%q want default", info.Project)
	}
	if info.Protocol != "incus" {
		t.Fatalf("Protocol=%q want incus", info.Protocol)
	}
}

func TestDoctorConnectionInfoForConfigRejectsAddressWithoutTrustMaterial(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Address = "https://incus.example.test:8443"

	if _, err := doctorConnectionInfoForConfig(cfg); err == nil {
		t.Fatal("doctorConnectionInfoForConfig accepted address mode without trust material")
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

	args, err := connectionArgsForAddress(cfg)
	if err != nil {
		t.Fatalf("connectionArgsForAddress: %v", err)
	}
	if args.TLSClientCert != "" || args.TLSClientKey != "" {
		t.Fatalf("unexpected remote TLS client credentials for unrelated address: %#v", args)
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
	cfg.Incus.InsecureTLS = true

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

func TestDoctorConnectionInfoForConfigUsesAddressOIDCMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeIncusConfig(t, home, "default-remote: staging\nremotes:\n  staging:\n    addr: https://staging.incus.example.test:8443\n    auth_type: oidc\n    protocol: incus\n")

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Incus.Remote = "staging"
	cfg.Incus.Address = "https://staging.incus.example.test:8443"
	cfg.Incus.InsecureTLS = true

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
