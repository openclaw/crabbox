package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Both preparation paths hand this script to /bin/sh, but the pinned installer
// is bash with `set -euo pipefail`. Running it in-process would abort
// preparation on the unsupported option, and its own `exit 0` would end
// preparation on every healthy guest. Exercise the stanza rather than its text.
func TestParallelsMacOSNodeBaselineStanzaIsolatesInstaller(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	for _, tc := range []struct {
		name         string
		nodeOnPath   bool
		macOS        bool
		wantContinue bool
	}{
		{name: "healthy runtime is preserved and preparation continues", nodeOnPath: true, macOS: true, wantContinue: true},
		{name: "non-macOS guest is untouched", nodeOnPath: false, macOS: false, wantContinue: true},
		{name: "unreachable installer stops preparation", nodeOnPath: false, macOS: true, wantContinue: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			local := filepath.Join(root, "local")
			tools := filepath.Join(root, "tools")
			if err := os.MkdirAll(tools, 0o755); err != nil {
				t.Fatal(err)
			}
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(tools, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"install", "mktemp", "rm", "rmdir", "mkdir", "cat", "uname", "sleep", "ln", "sh"} {
				p, err := exec.LookPath(name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(p, filepath.Join(tools, name)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.macOS {
				write("sw_vers", "#!/bin/sh\necho ProductName:\tmacOS\n")
			}
			if tc.nodeOnPath {
				write("node", "#!/bin/sh\necho v24.19.0\n")
				write("npm", "#!/bin/sh\necho 11.17.0\n")
			}
			// With no runtime anywhere the stanza reaches the pinned installer,
			// whose download must then fail for real.
			write("curl", "#!/bin/sh\nexit 22\n")
			// su is unavailable to an unprivileged test, so the user-managed
			// lookup returns nothing and the installer arm is what runs.
			write("su", "#!/bin/sh\nexit 1\n")

			stanza := parallelsMacOSNodeBaselineStanza()
			stanza = strings.ReplaceAll(stanza, "/usr/local", local)
			// Point both the standard-PATH probe and the installer's own PATH
			// reset at the stubbed tools.
			stanza = strings.ReplaceAll(stanza, local+"/bin:/usr/bin:/bin:/usr/sbin:/sbin", local+"/bin:"+tools)

			script := "set -eu\nuser=guest\n" + stanza + "\necho PREPARATION-CONTINUED\n"
			cmd := exec.Command("/bin/sh", "-c", script)
			cmd.Env = []string{"PATH=" + tools, "HOME=" + root}
			out, err := cmd.CombinedOutput()

			if got := strings.Contains(string(out), "PREPARATION-CONTINUED"); got != tc.wantContinue {
				t.Fatalf("preparation continued=%v want=%v: %s", got, tc.wantContinue, out)
			}
			if tc.wantContinue && err != nil {
				t.Fatalf("stanza failed on a guest it should leave alone: %v: %s", err, out)
			}
			if !tc.wantContinue && err == nil {
				t.Fatalf("stanza swallowed a failed Node install: %s", out)
			}
			// A healthy guest must not reach the network at all.
			if tc.nodeOnPath && strings.Contains(string(out), "nodejs.org") {
				t.Fatalf("installer ran despite a working runtime: %s", out)
			}
			// pipefail is bash-only; leaking it into /bin/sh breaks preparation.
			if strings.Contains(string(out), "pipefail") {
				t.Fatalf("installer ran in the POSIX shell instead of bash: %s", out)
			}
		})
	}
}

// prlctl exec and the SSH fallback both run this through /bin/sh.
func TestParallelsEnsureReadyScriptParsesUnderPOSIXShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	for _, desktop := range []bool{false, true} {
		script := parallelsPOSIXEnsureReadyScript("guest", "/Users/guest/crabbox", desktop, false)
		cmd := exec.Command("/bin/sh", "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("desktop=%v: generated script is not valid POSIX shell: %v: %s", desktop, err, out)
		}
	}
}

// The preservation arm is the compatibility fix itself, and the stanza test
// above stubs su to fail, so it never runs there. Drive it for real: a runtime
// the guest user resolves through bash -lc but that is absent from the standard
// PATH must be linked, not downloaded over.
func TestParallelsMacOSNodeBaselineStanzaPreservesUserManagedRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	root := t.TempDir()
	local := filepath.Join(root, "local")
	tools := filepath.Join(root, "tools")
	// Deliberately NOT on PATH: this stands in for /opt/homebrew/bin or ~/.nvm.
	managed := filepath.Join(root, "managed")
	for _, dir := range []string{tools, managed} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"install", "mktemp", "rm", "rmdir", "mkdir", "cat", "uname", "sleep", "ln", "sh"} {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(tools, name)); err != nil {
			t.Fatal(err)
		}
	}
	write(tools, "sw_vers", "#!/bin/sh\necho ProductName:\tmacOS\n")
	for _, name := range []string{"node", "npm", "npx"} {
		write(managed, name, "#!/bin/sh\necho stub\n")
	}
	// Stand in for the guest user's login shell resolving its own runtime.
	write(tools, "su", "#!/bin/sh\ncase \"$*\" in\n  *'command -v node'*) echo "+quoteForShell(filepath.Join(managed, "node"))+" ;;\n  *'command -v npm'*) echo "+quoteForShell(filepath.Join(managed, "npm"))+" ;;\n  *'command -v npx'*) echo "+quoteForShell(filepath.Join(managed, "npx"))+" ;;\n  *) exit 1 ;;\nesac\n")
	// Any download attempt is a failure of the behaviour under test.
	write(tools, "curl", "#!/bin/sh\necho REACHED-nodejs.org >&2\nexit 22\n")

	stanza := parallelsMacOSNodeBaselineStanza()
	stanza = strings.ReplaceAll(stanza, "/usr/local", local)
	stanza = strings.ReplaceAll(stanza, local+"/bin:/usr/bin:/bin:/usr/sbin:/sbin", local+"/bin:"+tools)

	script := "set -eu\nuser=guest\n" + stanza + "\necho PREPARATION-CONTINUED\n"
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = []string{"PATH=" + tools, "HOME=" + root}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("preservation arm failed: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "PREPARATION-CONTINUED") {
		t.Fatalf("preparation did not continue: %s", out)
	}
	if strings.Contains(string(out), "nodejs.org") {
		t.Fatalf("downloaded over a working user-managed runtime: %s", out)
	}
	for _, name := range []string{"node", "npm", "npx"} {
		got, lerr := os.Readlink(filepath.Join(local, "bin", name))
		if lerr != nil {
			t.Fatalf("%s was not linked into the standard location: %v: %s", name, lerr, out)
		}
		if want := filepath.Join(managed, name); got != want {
			t.Fatalf("%s linked to %q, want the preserved runtime %q", name, got, want)
		}
	}
}

func quoteForShell(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
