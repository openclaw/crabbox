package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// macOS readiness requires node and npm, but nothing on the Parallels path
// installed them. Guest preparation also short-circuits on an existing
// crabbox-ready, so the baseline has to be installed before that shortcut.
func TestParallelsMacOSNodeBaselineRunsBeforeReadyShortcut(t *testing.T) {
	script := parallelsPOSIXEnsureReadyScript("guest", "/Users/guest/crabbox", false, false)

	stanza := parallelsMacOSNodeBaselineStanza()
	install := strings.Index(script, stanza)
	if install < 0 {
		t.Fatal("guest preparation does not embed the macOS Node baseline stanza")
	}
	shortcut := strings.Index(script, "if [ -x /usr/local/bin/crabbox-ready ]")
	if shortcut < 0 {
		t.Fatal("guest preparation no longer has a crabbox-ready shortcut; revisit this guard")
	}
	if install > shortcut {
		t.Fatalf("Node baseline runs after the crabbox-ready shortcut (install=%d shortcut=%d); a guest that already passes crabbox-ready would exit before installing Node", install, shortcut)
	}
	if !strings.Contains(stanza, sharedMacOSNodeInstall()) {
		t.Fatal("stanza does not carry the checksum-pinned installer verbatim")
	}
}

// Both preparation paths hand this script to /bin/sh, so the bash-only
// installer must run in its own interpreter and its exit 0 must not cut
// preparation short.
func TestParallelsMacOSNodeBaselineStanzaIsolatesInstaller(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	for _, tc := range []struct {
		name         string
		nodePresent  bool
		macOS        bool
		wantContinue bool
	}{
		{name: "healthy macOS runtime is preserved", nodePresent: true, macOS: true, wantContinue: true},
		{name: "non-macOS guest is untouched", nodePresent: false, macOS: false, wantContinue: true},
		{name: "install failure stops preparation", nodePresent: false, macOS: true, wantContinue: false},
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
			for _, name := range []string{"install", "mktemp", "rm", "rmdir", "mkdir", "cat", "uname", "sleep"} {
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
			if tc.nodePresent {
				write("node", "#!/bin/sh\necho v24.19.0\n")
				write("npm", "#!/bin/sh\necho 11.17.0\n")
			}
			// No node means the installer must download; fail that so the
			// stanza has to propagate a real failure.
			write("curl", "#!/bin/sh\nexit 22\n")

			stanza := parallelsMacOSNodeBaselineStanza()
			stanza = strings.ReplaceAll(stanza, "/usr/local", local)
			stanza = strings.ReplaceAll(stanza, local+"/bin:/usr/bin:/bin:/usr/sbin:/sbin", local+"/bin:"+tools)

			cmd := exec.Command("/bin/sh", "-c", "set -eu\n"+stanza+"\necho PREPARATION-CONTINUED\n")
			cmd.Env = []string{"PATH=" + tools, "HOME=" + root}
			out, err := cmd.CombinedOutput()

			if got := strings.Contains(string(out), "PREPARATION-CONTINUED"); got != tc.wantContinue {
				t.Fatalf("preparation continued=%v want=%v: %s", got, tc.wantContinue, out)
			}
			if tc.wantContinue && err != nil {
				t.Fatalf("stanza failed on a guest it should leave alone: %v: %s", err, out)
			}
			if !tc.wantContinue {
				if err == nil {
					t.Fatalf("stanza swallowed a failed Node install: %s", out)
				}
				if !strings.Contains(string(out), "Node baseline") {
					t.Fatalf("failure does not name the Node baseline: %s", out)
				}
			}
			// pipefail is bash-only; leaking it into /bin/sh breaks preparation.
			if strings.Contains(string(out), "pipefail") {
				t.Fatalf("installer ran in the POSIX shell instead of bash: %s", out)
			}
		})
	}
}

// crabbox-ready is what the shortcut trusts, so it has to assert the same
// baseline the readiness probe does. The managed macOS contract is the
// reference.
func TestParallelsMacOSReadyScriptMatchesReadinessContract(t *testing.T) {
	script := parallelsPOSIXEnsureReadyScript("guest", "/Users/guest/crabbox", false, false)

	want := `#!/bin/sh
set -eu
export PATH="/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
rsync --version >/dev/null
curl --version >/dev/null
node --version >/dev/null
npm --version >/dev/null
test -w '/Users/guest/crabbox'
`
	if !strings.Contains(script, want) {
		t.Fatalf("Parallels macOS crabbox-ready does not assert the Node baseline on an explicit PATH:\n%s", script)
	}
	for _, required := range []string{"node --version", "npm --version"} {
		if !strings.Contains(sshReadyCommand(SSHTarget{TargetOS: targetMacOS}), required) {
			t.Fatalf("sshReadyCommand no longer requires %q; crabbox-ready should follow", required)
		}
	}
}

// The Node baseline is macOS-only; Linux guests keep their existing contract.
func TestParallelsLinuxReadyScriptUnchanged(t *testing.T) {
	script := parallelsPOSIXEnsureReadyScript("worker", "/work/crabbox", false, false)

	want := `#!/usr/bin/env bash
set -euo pipefail
git --version >/dev/null
rsync --version >/dev/null
curl --version >/dev/null
jq --version >/dev/null
test -w '/work/crabbox'
`
	if !strings.Contains(script, want) {
		t.Fatalf("Linux crabbox-ready changed; the Node baseline must stay macOS-only:\n%s", script)
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
