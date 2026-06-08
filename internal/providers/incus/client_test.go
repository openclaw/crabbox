package incus

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
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
