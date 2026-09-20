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
				write("node", "#!/bin/sh\ncase \"$1\" in\n  -p) echo "+quoteForShell(filepath.Join(tools, "node"))+" ;;\n  *) echo v24.19.0 ;;\nesac\n")
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
		script := parallelsPOSIXEnsureReadyScript("guest", "/Users/guest/crabbox", desktop, false, sshPortCandidates("22", nil))
		cmd := exec.Command("/bin/sh", "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("desktop=%v: generated script is not valid POSIX shell: %v: %s", desktop, err, out)
		}
	}
}

// The preservation arm is the compatibility fix itself, and the stanza test
// above stubs su to fail, so it never runs there. Drive it for real, including
// both mixed-path arrangements: node and npm can live in different prefixes,
// and a command already sitting at its destination must be left alone rather
// than forcing the whole guest onto the installer.
func TestParallelsMacOSNodeBaselineStanzaPreservesUserManagedRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	for _, tc := range []struct {
		name          string
		nodeAtDefault bool
		npmAtDefault  bool
	}{
		{name: "both outside the standard prefix"},
		{name: "node already at its destination, npm elsewhere", nodeAtDefault: true},
		{name: "npm already at its destination, node elsewhere", npmAtDefault: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			local := filepath.Join(root, "local")
			tools := filepath.Join(root, "tools")
			// Deliberately NOT on PATH: stands in for /opt/homebrew/bin or ~/.nvm.
			managed := filepath.Join(root, "managed")
			for _, dir := range []string{tools, managed, filepath.Join(local, "bin")} {
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

			// Place each command in whichever prefix this arrangement calls for.
			// The standard-PATH probe still fails in every case, because the
			// stubbed tools directory never carries a complete runtime.
			home := map[string]string{"node": managed, "npm": managed, "npx": managed}
			if tc.nodeAtDefault {
				home["node"] = filepath.Join(local, "bin")
			}
			if tc.npmAtDefault {
				home["npm"] = filepath.Join(local, "bin")
			}
			for name, dir := range home {
				body := "#!/bin/sh\necho stub\n"
				if name == "node" {
					// The stanza asks node where it really lives.
					body = "#!/bin/sh\ncase \"$1\" in\n  -p) echo " + quoteForShell(filepath.Join(dir, name)) + " ;;\n  *) echo v24.19.0 ;;\nesac\n"
				}
				write(dir, name, body)
			}

			su := "#!/bin/sh\ncase \"$*\" in\n"
			su += "  *'node -p process.execPath'*) exec " + quoteForShell(filepath.Join(home["node"], "node")) + " -p ;;\n"
			for _, name := range []string{"node", "npm", "npx"} {
				su += "  *'command -v " + name + "'*) echo " + quoteForShell(filepath.Join(home[name], name)) + " ;;\n"
			}
			su += "  *) exit 1 ;;\nesac\n"
			write(tools, "su", su)
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
				dest := filepath.Join(local, "bin", name)
				got, lerr := os.Readlink(dest)
				if home[name] == filepath.Join(local, "bin") {
					// Already at its destination: must be left exactly as it was,
					// never relinked onto itself.
					if lerr == nil {
						t.Fatalf("%s was already correct but got replaced by a link to %q", name, got)
					}
					continue
				}
				if lerr != nil {
					t.Fatalf("%s was not linked into the standard location: %v: %s", name, lerr, out)
				}
				if want := filepath.Join(managed, name); got != want {
					t.Fatalf("%s linked to %q, want the preserved runtime %q", name, got, want)
				}
			}
		})
	}
}

func quoteForShell(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// asdf-style managers expose shims that re-exec through the manager. The shim
// is on the guest user's login PATH and satisfies the probe, but crabbox-ready
// runs on a fixed PATH without the manager, so linking the shim produces a
// helper that fails while the probe passes. Resolve the real binary instead.
func TestParallelsMacOSNodeBaselineStanzaResolvesRuntimeManagerShims(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	root := t.TempDir()
	local := filepath.Join(root, "local")
	tools := filepath.Join(root, "tools")
	shims := filepath.Join(root, "shims")   // on the login PATH only
	mgr := filepath.Join(root, "mgr")       // the manager, login PATH only
	real := filepath.Join(root, "installs") // what the shim ultimately runs
	for _, dir := range []string{tools, shims, mgr, real, filepath.Join(local, "bin")} {
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
	write(tools, "curl", "#!/bin/sh\necho REACHED-nodejs.org >&2\nexit 22\n")
	write(mgr, "asdfmgr", "#!/bin/sh\nexit 0\n")

	// The real binaries, reachable without the manager.
	write(real, "node", "#!/bin/sh\ncase \"$1\" in\n  -p) echo "+quoteForShell(filepath.Join(real, "node"))+" ;;\n  *) echo v24.19.0 ;;\nesac\n")
	write(real, "npm", "#!/bin/sh\necho 11.17.0\n")
	write(real, "npx", "#!/bin/sh\necho npx\n")

	// The shims refuse to run unless the manager is on PATH.
	for _, name := range []string{"node", "npm", "npx"} {
		write(shims, name, "#!/bin/sh\ncommand -v asdfmgr >/dev/null 2>&1 || { echo 'asdfmgr: command not found' >&2; exit 127; }\nexec "+quoteForShell(filepath.Join(real, name))+" \"$@\"\n")
	}

	// su stands in for the guest user's login shell: manager and shims present.
	write(tools, "su", "#!/bin/sh\nPATH="+filepath.Join(root, "mgr")+":"+filepath.Join(root, "shims")+":$PATH\nexport PATH\ncase \"$*\" in\n"+
		"  *'node -p process.execPath'*) exec node -p process.execPath ;;\n"+
		"  *'command -v node'*) command -v node ;;\n"+
		"  *'command -v npm'*) command -v npm ;;\n"+
		"  *'command -v npx'*) command -v npx ;;\n"+
		"  *) exit 1 ;;\nesac\n")

	stanza := parallelsMacOSNodeBaselineStanza()
	stanza = strings.ReplaceAll(stanza, "/usr/local", local)
	stanza = strings.ReplaceAll(stanza, local+"/bin:/usr/bin:/bin:/usr/sbin:/sbin", local+"/bin:"+tools)

	script := "set -eu\nuser=guest\n" + stanza + "\necho PREPARATION-CONTINUED\n"
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = []string{"PATH=" + tools, "HOME=" + root}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim-managed runtime was not preserved: %v: %s", err, out)
	}
	if strings.Contains(string(out), "nodejs.org") {
		t.Fatalf("downloaded over a working shim-managed runtime: %s", out)
	}
	// The link must point at the real binary, not the manager's shim.
	got, lerr := os.Readlink(filepath.Join(local, "bin", "node"))
	if lerr != nil {
		t.Fatalf("node was not linked: %v: %s", lerr, out)
	}
	if want := filepath.Join(real, "node"); got != want {
		t.Fatalf("node linked to %q, want the underlying binary %q", got, want)
	}
	// The decisive check: what crabbox-ready will actually run must work on the
	// fixed PATH, with the manager absent.
	verify := exec.Command("/bin/sh", "-c", "node --version >/dev/null && npm --version >/dev/null")
	verify.Env = []string{"PATH=" + filepath.Join(local, "bin") + ":" + tools}
	if vout, verr := verify.CombinedOutput(); verr != nil {
		t.Fatalf("preserved runtime does not work on crabbox-ready's PATH: %v: %s", verr, vout)
	}
}
