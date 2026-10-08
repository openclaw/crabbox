package hyperv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCreateNoCloudSeedUsesCidataAndCoreCloudInit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	var capturedUserData, capturedMetaData string
	var sourcePaths []string
	runner := &recordingRunner{
		onRun: func(req core.LocalCommandRequest) {
			script := req.Args[len(req.Args)-1]
			if !strings.Contains(script, "NewFileSystemLabel 'cidata'") {
				return
			}
			for _, name := range []string{"_CRABBOX_USER_DATA_PATH", "_CRABBOX_META_DATA_PATH"} {
				path := requestEnv(req, name)
				sourcePaths = append(sourcePaths, path)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Errorf("read %s: %v", name, err)
					continue
				}
				if name == "_CRABBOX_USER_DATA_PATH" {
					capturedUserData = string(data)
				} else {
					capturedMetaData = string(data)
				}
			}
		},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.TargetOS = targetLinux
	cfg.Desktop = true
	cfg.Browser = true
	cfg.Code = true
	cfg.HyperV.WorkRoot = "/work/crabbox"
	cfg.WorkRoot = "/work/crabbox"
	publicKey := "ssh-ed25519 test-public-key"
	userData := core.CloudInitUserData(cfg, publicKey)
	metaData := cloudInitMetaData("crabbox-blue-1234")
	seedPath := cloudInitSeedPath("crabbox-blue-1234")

	if err := b.createNoCloudSeed(context.Background(), seedPath, userData, metaData); err != nil {
		t.Fatal(err)
	}
	if capturedUserData != userData || capturedMetaData != metaData {
		t.Fatalf("captured NoCloud data did not match generated inputs")
	}
	for _, want := range []string{
		"#cloud-config",
		publicKey,
		"PasswordAuthentication no",
		"crabbox-desktop.service",
		"/var/lib/crabbox/browser.env",
		"/usr/local/bin/code-server",
		"/usr/local/bin/code-server --version >/dev/null",
		"crabbox-ready",
	} {
		if !strings.Contains(capturedUserData, want) {
			t.Errorf("user-data missing %q", want)
		}
	}
	for _, forbidden := range []string{"ssh_pwauth: true", "lock_passwd: false", "chpasswd:"} {
		if strings.Contains(capturedUserData, forbidden) {
			t.Errorf("user-data enables password login via %q", forbidden)
		}
	}
	if capturedMetaData != "instance-id: crabbox-blue-1234\nlocal-hostname: crabbox-blue-1234\n" {
		t.Fatalf("meta-data=%q", capturedMetaData)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%d want 1", len(runner.calls))
	}
	script := runner.calls[0].Args[len(runner.calls[0].Args)-1]
	for _, want := range []string{
		"New-VHD",
		"Mount-VHD",
		"Initialize-Disk",
		"Format-Volume",
		"-FileSystem FAT",
		"NewFileSystemLabel 'cidata'",
		"UTF8Encoding($false)",
		"'user-data'",
		"'meta-data'",
		"Dismount-VHD",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("seed script missing %q", want)
		}
	}
	if strings.Contains(script, publicKey) || strings.Contains(script, "#cloud-config") {
		t.Fatal("NoCloud payload must not be embedded in the PowerShell command line")
	}
	if strings.Contains(script, "Format-Volume -Partition") || !strings.Contains(script, "$partition | Format-Volume") {
		t.Fatalf("seed script does not bind the partition through the pipeline: %q", script)
	}
	tryIndex := strings.Index(script, "try {")
	mountIndex := strings.Index(script, "Mount-VHD")
	finallyIndex := strings.Index(script, "} finally {")
	if tryIndex < 0 || mountIndex < tryIndex || finallyIndex < mountIndex || !strings.Contains(script[finallyIndex:], "Dismount-VHD") {
		t.Fatalf("seed script does not guarantee dismount after mount: %q", script)
	}
	for _, path := range sourcePaths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("temporary NoCloud source remains at %s: %v", path, err)
		}
	}
}

func TestLinuxTailscaleReusesTemporaryNoCloudBootstrap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	const authKey = "fixture-only-invalid-value"
	var capturedUserData string
	var userDataPath string
	runner := &recordingRunner{
		onRun: func(req core.LocalCommandRequest) {
			script := req.Args[len(req.Args)-1]
			if !strings.Contains(script, "NewFileSystemLabel 'cidata'") {
				return
			}
			userDataPath = requestEnv(req, "_CRABBOX_USER_DATA_PATH")
			data, err := os.ReadFile(userDataPath)
			if err != nil {
				t.Errorf("read user-data: %v", err)
				return
			}
			capturedUserData = string(data)
		},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.TargetOS = targetLinux
	cfg.SSHUser = "runner"
	cfg.HyperV.User = "runner"
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.AuthKey = authKey
	cfg.Tailscale.Hostname = "crabbox-linux"
	cfg.Tailscale.Tags = []string{"tag:crabbox"}
	userData := core.CloudInitUserData(cfg, "ssh-ed25519 test-public-key")

	if err := b.createNoCloudSeed(context.Background(), cloudInitSeedPath("crabbox-linux"), userData, cloudInitMetaData("crabbox-linux")); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"PasswordAuthentication no",
		"tailscale up --auth-key=file:/dev/stdin",
		"/var/lib/crabbox/tailscale-ipv4",
		authKey,
	} {
		if !strings.Contains(capturedUserData, want) {
			t.Fatalf("Linux Tailscale user-data missing %q", want)
		}
	}
	command := strings.Join(runner.calls[0].Args, " ")
	if strings.Contains(command, authKey) || strings.Contains(command, "#cloud-config") {
		t.Fatal("NoCloud Tailscale payload leaked into host command argv")
	}
	if _, err := os.Stat(userDataPath); !os.IsNotExist(err) {
		t.Fatalf("temporary Tailscale user-data remains at %s: %v", userDataPath, err)
	}
}

func TestDetachAndRemoveNoCloudSeed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	name := "crabbox-blue-1234"
	path := cloudInitSeedPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	b := testBackend(runner)
	if err := b.detachAndRemoveNoCloudSeed(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("seed remains at %s: %v", path, err)
	}
	script := runner.calls[0].Args[len(runner.calls[0].Args)-1]
	if !strings.Contains(script, "Get-VMHardDiskDrive") || !strings.Contains(script, "Remove-VMHardDiskDrive") {
		t.Fatalf("detach script=%q", script)
	}
}

func TestRemoveVMStorageOwnsRootAndSeedDisks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	name := "crabbox-blue-1234"
	rootPath := filepath.Join(hypervVHDDir(), name+".vhdx")
	seedPath := cloudInitSeedPath(name)
	if err := os.MkdirAll(filepath.Dir(rootPath), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{rootPath, seedPath} {
		if err := os.WriteFile(path, []byte("disk"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := testBackend(&recordingRunner{}).removeVMStorage(name, []string{rootPath, seedPath}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{rootPath, seedPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("owned disk remains at %s: %v", path, err)
		}
	}
}

func requestEnv(req core.LocalCommandRequest, name string) string {
	prefix := name + "="
	for _, value := range req.Env {
		if strings.HasPrefix(value, prefix) {
			return strings.TrimPrefix(value, prefix)
		}
	}
	return ""
}
