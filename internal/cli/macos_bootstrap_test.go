package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestManagedMacOSNodeBaseline(t *testing.T) {
	cfg := defaultConfig()
	cfg.TargetOS = targetMacOS
	bootstrap := macOSUserData(cfg, "ssh-ed25519 fixture")
	for _, want := range []string{"UsePAM yes", "PasswordAuthentication no", "KbdInteractiveAuthentication no"} {
		if !strings.Contains(bootstrap, want) {
			t.Errorf("macOS SSH session setup missing %q", want)
		}
	}
	for _, want := range []string{"node_version=24.19.0", "node_arch=x64", "node_arch=arm64", "https://nodejs.org/dist/", "shasum -a 256 -c -", "node --version >/dev/null", "npm --version >/dev/null"} {
		if !strings.Contains(bootstrap, want) {
			t.Errorf("macOS bootstrap missing %q", want)
		}
	}
	for _, want := range []string{"node --version", "npm --version"} {
		if !strings.Contains(sshReadyCommand(SSHTarget{TargetOS: targetMacOS}), want) {
			t.Errorf("macOS readiness missing %q", want)
		}
	}
	linux, err := os.ReadFile("../../scripts/install-linux-developer-tools.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(linux), `pinned_node_version="24.19.0"`) {
		t.Fatal("update macOS Node pin with Linux baseline")
	}
}

func TestMacOSSSHSessionBootstrap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	for _, tc := range []struct {
		name, initial string
		reject        bool
	}{
		{name: "stock disabled PAM", initial: "UsePAM no\nPasswordAuthentication yes\nKbdInteractiveAuthentication yes\nPort 2222\n"},
		{name: "existing managed block", initial: "# crabbox ssh session begin\nUsePAM no\n# crabbox ssh session end\nPort 22\n"},
		{name: "validator rejects candidate", initial: "Port 22\n", reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			config := filepath.Join(root, "sshd_config")
			if err := os.WriteFile(config, []byte(tc.initial), 0o644); err != nil {
				t.Fatal(err)
			}
			validator := filepath.Join(root, "sshd")
			body := "#!/bin/sh\nset -eu\n[ \"$1\" = -t ] && [ \"$2\" = -f ]\n[ \"$(sed -n '2p' \"$3\")\" = 'UsePAM yes' ]\n[ \"$(sed -n '3p' \"$3\")\" = 'PasswordAuthentication no' ]\n[ \"$(sed -n '4p' \"$3\")\" = 'KbdInteractiveAuthentication no' ]\n"
			if tc.reject {
				body += "exit 9\n"
			}
			if err := os.WriteFile(validator, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			script := strings.ReplaceAll(sharedMacOSSSHSession(), "/etc/ssh", root)
			script = strings.ReplaceAll(script, "/usr/sbin/sshd", shellQuote(validator))
			run := func() ([]byte, error) { return exec.Command("/bin/bash", "-c", script).CombinedOutput() }
			out, err := run()
			if tc.reject {
				if err == nil {
					t.Fatal("invalid sshd configuration was accepted")
				}
				got, readErr := os.ReadFile(config)
				if readErr != nil || string(got) != tc.initial {
					t.Fatalf("rejected configuration changed original: %q %v", got, readErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("bootstrap: %s %v", out, err)
			}
			first, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(first), "Port ") || strings.Count(string(first), "# crabbox ssh session begin") != 1 {
				t.Fatalf("lost configuration or duplicated block: %s", first)
			}
			if out, err := run(); err != nil {
				t.Fatalf("repeat: %s %v", out, err)
			}
			second, err := os.ReadFile(config)
			if err != nil || !bytes.Equal(first, second) {
				t.Fatalf("bootstrap is not idempotent: %s %v", second, err)
			}
		})
	}
}

func TestMacOSNodeInstaller(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	for _, tc := range []struct {
		name, arch                            string
		badChecksum, failDownload, brokenSlot bool
	}{
		{name: "intel", arch: "x86_64"}, {name: "arm", arch: "arm64"},
		{name: "repair incomplete managed slot", arch: "x86_64", brokenSlot: true},
		{name: "checksum failure", arch: "x86_64", badChecksum: true},
		{name: "download failure", arch: "arm64", failDownload: true},
		{name: "unsupported", arch: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			local := filepath.Join(root, "local")
			tools := filepath.Join(root, "tools")
			if err := os.MkdirAll(tools, 0o755); err != nil {
				t.Fatal(err)
			}
			write := func(path, body string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.brokenSlot {
				bin := filepath.Join(local, "lib", "crabbox", "node-v24.19.0-darwin-x64", "bin")
				if err := os.MkdirAll(bin, 0o755); err != nil {
					t.Fatal(err)
				}
				write(filepath.Join(bin, "node"), "#!/bin/sh\nexit 1\n")
			}
			for _, name := range []string{"install", "mktemp", "rm", "rmdir", "mkdir", "mv", "ln", "tar", "gzip", "shasum", "chmod", "dirname", "sleep"} {
				p, err := exec.LookPath(name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(p, filepath.Join(tools, name)); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(tools, "uname"), "#!/bin/sh\nprintf '%s\\n' "+shellQuote(tc.arch)+"\n")
			var archive bytes.Buffer
			gz := gzip.NewWriter(&archive)
			tw := tar.NewWriter(gz)
			for name, body := range map[string]string{"node": "#!/bin/sh\necho v24.19.0\n", "npm": "#!/bin/sh\nnode --version >/dev/null || exit 1\necho 11.17.0\n", "npx": "#!/bin/sh\nexit 0\n"} {
				if err := tw.WriteHeader(&tar.Header{Name: "node/bin/" + name, Mode: 0o755, Size: int64(len(body))}); err != nil {
					t.Fatal(err)
				}
				if _, err := tw.Write([]byte(body)); err != nil {
					t.Fatal(err)
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			fixture := filepath.Join(root, "node.tar.gz")
			if err := os.WriteFile(fixture, archive.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			cp, err := exec.LookPath("cp")
			if err != nil {
				t.Fatal(err)
			}
			curl := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>" + shellQuote(filepath.Join(root, "downloads")) + "\n"
			if tc.failDownload {
				curl += "exit 22\n"
			} else {
				curl += "while [ \"$1\" != --output ]; do shift; done\n" + shellQuote(cp) + " " + shellQuote(fixture) + " \"$2\"\n"
			}
			write(filepath.Join(tools, "curl"), curl)
			script := strings.ReplaceAll(sharedMacOSNodeInstall(), "/usr/local", local)
			script = strings.ReplaceAll(script, local+"/bin:/usr/bin:/bin:/usr/sbin:/sbin", local+"/bin:"+tools)
			if !tc.badChecksum {
				digest := fmt.Sprintf("%x", sha256.Sum256(archive.Bytes()))
				for _, pin := range []string{"d1b5e999db158c62fe8f7267a4476b035d8bd93b1a605bac24a3f0dd166e3316", "8294b7aa9b03997481c06babf1e8b270c859358f27da57a11509afe537ac381d"} {
					script = strings.ReplaceAll(script, pin, digest)
				}
			}
			run := func() ([]byte, error) {
				cmd := exec.Command("/bin/bash", "-c", script)
				cmd.Env = []string{"HOME=" + root}
				return cmd.CombinedOutput()
			}
			out, err := run()
			if tc.badChecksum || tc.failDownload || tc.arch == "unknown" {
				if err == nil {
					t.Fatalf("installer accepted invalid input: %s", out)
				}
				if _, err := os.Lstat(filepath.Join(local, "bin", "node")); !os.IsNotExist(err) {
					t.Fatalf("published runtime on failure: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("install: %s %v", out, err)
			}
			if out, err := run(); err != nil {
				t.Fatalf("repeat: %s %v", out, err)
			}
			downloads, err := os.ReadFile(filepath.Join(root, "downloads"))
			if err != nil {
				t.Fatal(err)
			}
			arch := "x64"
			if tc.arch == "arm64" {
				arch = "arm64"
			}
			if strings.Count(string(downloads), "https://nodejs.org/dist/v24.19.0/node-v24.19.0-darwin-"+arch+".tar.gz") != 1 {
				t.Fatalf("wrong architecture or repeated download: %s", downloads)
			}
		})
	}
}
