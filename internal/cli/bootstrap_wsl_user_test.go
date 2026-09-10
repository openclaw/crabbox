package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestManagedWSLWorkerIdentity(t *testing.T) {
	for _, test := range []struct{ name, target, mode string }{
		{"linux", targetLinux, ""},
		{"native windows", targetWindows, windowsModeNormal},
		{"wsl2", targetWindows, windowsModeWSL2},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.TargetOS, cfg.WindowsMode = test.target, test.mode
			cfg.WorkRoot = "/work/custom root"
			script := cloudInit(cfg, "ssh-ed25519 test")
			if test.target == targetWindows {
				script = windowsBootstrapPowerShell(cfg, "ssh-ed25519 test")
			}
			steps := []string{
				"useradd --create-home --user-group --shell /bin/bash crabbox",
				"groupadd -f docker",
				"usermod --append --groups sudo,docker --shell /bin/bash crabbox",
				"test \"$(id -u crabbox)\" -ne 0",
				"'crabbox ALL=(ALL) NOPASSWD:ALL' >/etc/sudoers.d/crabbox",
				"chmod 0440 /etc/sudoers.d/crabbox",
				"visudo -cf /etc/sudoers.d/crabbox",
				"chown -R crabbox:crabbox '/work/custom root' /var/cache/crabbox",
				"config.set('user', 'default', 'crabbox')",
				"sudo -H -u crabbox /usr/local/bin/crabbox-ready",
				"wsl.exe -d $wslDistro --user root --exec bash /mnt/c/ProgramData/crabbox/wsl/linux-setup.sh",
				"wsl.exe --terminate $wslDistro",
				"wsl.exe -d $wslDistro --exec /usr/local/bin/crabbox-ready",
			}
			last := -1
			for _, step := range steps {
				index := strings.Index(script, step)
				if test.mode != windowsModeWSL2 {
					if index >= 0 {
						t.Fatalf("WSL worker setup leaked into %s: %s", test.name, step)
					}
					continue
				}
				if index <= last {
					t.Fatalf("missing or out-of-order worker setup: %s", step)
				}
				last = index
			}
			if test.mode == windowsModeWSL2 && strings.Count(script, "--user root") != 1 {
				t.Fatal("only Linux bootstrap may explicitly select root")
			}
		})
	}
}

func wslBootstrapHereDoc(t *testing.T, marker string) string {
	t.Helper()
	cfg := baseConfig()
	cfg.TargetOS, cfg.WindowsMode = targetWindows, windowsModeWSL2
	script := windowsBootstrapPowerShell(cfg, "ssh-ed25519 test")
	_, body, ok := strings.Cut(script, "<<'"+marker+"'\n")
	if !ok {
		t.Fatalf("missing %s heredoc", marker)
	}
	body, _, ok = strings.Cut(body, "\n"+marker+"\n")
	if !ok {
		t.Fatalf("unterminated %s heredoc", marker)
	}
	return body
}

func TestManagedWSLDefaultUserPreservesDistroSettings(t *testing.T) {
	script := wslBootstrapHereDoc(t, "WSL_USER")
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python is unavailable")
	}
	for _, test := range []struct{ name, input string }{
		{"missing", ""},
		{"existing", "[boot]\nsystemd=true\n[automount]\nmountFsTab=false\n[interop]\nappendWindowsPath=false\n[user]\ndefault=root\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wsl.conf")
			if test.input != "" {
				if err := os.WriteFile(path, []byte(test.input), 0600); err != nil {
					t.Fatal(err)
				}
			}
			command := strings.Replace(script, "Path('/etc/wsl.conf')", "Path(__import__('sys').argv[1])", 1)
			var previous string
			for pass := 0; pass < 2; pass++ {
				if out, err := exec.CommandContext(t.Context(), python, "-c", command, path).CombinedOutput(); err != nil {
					t.Fatalf("configure default user: %v: %s", err, out)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				want := "[user]\ndefault=crabbox\n\n"
				if test.input != "" {
					want = "[boot]\nsystemd=true\n\n[automount]\nmountFsTab=false\n\n[interop]\nappendWindowsPath=false\n\n" + want
				}
				if string(data) != want || (pass > 0 && string(data) != previous) {
					t.Fatalf("pass %d config=%q, want %q", pass, data, want)
				}
				previous = string(data)
			}
		})
	}
}

func TestManagedWSLReadinessRequiresWorkerIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executes POSIX readiness fixtures locally")
	}
	ready := wslBootstrapHereDoc(t, "READY")
	for _, test := range []struct{ name, uid, user, home, failingTool string }{
		{"worker", "1001", "crabbox", "worker", ""},
		{"root", "0", "crabbox", "worker", ""},
		{"wrong user", "1001", "ubuntu", "worker", ""},
		{"wrong home", "1001", "crabbox", "other", ""},
		{"sudo unavailable", "1001", "crabbox", "worker", "sudo"},
		{"node unavailable", "1001", "crabbox", "worker", "node"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			for _, tool := range []string{"id", "sudo", "git", "python3", "rsync", "curl", "jq", "trufflehog", "node", "npm", "wslpath"} {
				body := "#!/bin/sh\nexit 0\n"
				if tool == "id" {
					body = "#!/bin/sh\ncase $1 in -u) echo " + test.uid + ";; -un) echo " + test.user + ";; esac\n"
				} else if tool == test.failingTool {
					body = "#!/bin/sh\nexit 1\n"
				}
				if err := os.WriteFile(filepath.Join(bin, tool), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			home := filepath.Join(root, "worker")
			if err := os.Mkdir(home, 0700); err != nil {
				t.Fatal(err)
			}
			script := strings.NewReplacer("/home/crabbox", home, "/work/crabbox", root, "/var/cache/crabbox/npm", root, "/var/cache/crabbox/pnpm", root).Replace(ready)
			cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
			cmd.Env = []string{"PATH=" + bin, "HOME=" + filepath.Join(root, test.home)}
			out, err := cmd.CombinedOutput()
			if (err == nil) != (test.name == "worker") {
				t.Fatalf("readiness error=%v output=%s", err, out)
			}
		})
	}
}
