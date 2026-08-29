package localcontainer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrowserBootstrapPackageSelection(t *testing.T) {
	const fingerprint = "35BAA0B33E9EB396F59CA838C0BA5CE6DC6315A3"
	for _, tc := range []struct {
		name, distro, working, available, broken, failure string
		wantInstall, wantError                            string
	}{
		{name: "working Chrome untouched", distro: "ubuntu", working: "google-chrome"},
		{name: "working Chromium untouched", distro: "ubuntu", working: "chromium"},
		{name: "working ESR untouched", distro: "ubuntu", working: "firefox-esr"},
		{name: "working Firefox untouched", distro: "ubuntu", working: "firefox"},
		{name: "Ubuntu native Firefox", distro: "ubuntu", available: "firefox", wantInstall: "firefox/mozilla"},
		{name: "Ubuntu broken Chromium advances", distro: "ubuntu", available: "chromium firefox", broken: "chromium", wantInstall: "firefox/mozilla"},
		{name: "Debian Chromium", distro: "debian", available: "chromium firefox-esr", wantInstall: "chromium"},
		{name: "Debian broken Chromium advances", distro: "debian", available: "chromium firefox-esr", broken: "chromium", wantInstall: "firefox-esr"},
		{name: "Debian broken ESR advances", distro: "debian", available: "firefox-esr firefox", broken: "firefox-esr", wantInstall: "firefox"},
		{name: "Mozilla wrong key", distro: "ubuntu", available: "firefox", failure: "key", wantError: "signing key"},
		{name: "Mozilla metadata unavailable", distro: "ubuntu", available: "firefox", failure: "metadata", wantError: "Mozilla Firefox"},
		{name: "installed transition cannot downgrade", distro: "ubuntu", available: "firefox", failure: "install", wantError: "Mozilla Firefox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			write := func(name, content string) {
				t.Helper()
				if err := os.WriteFile(name, []byte(content), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(root, "os-release"), "ID="+tc.distro+"\n")
			for _, browser := range []string{"google-chrome", "chromium", "firefox-esr", "firefox"} {
				write(filepath.Join(bin, browser), "#!/bin/sh\n[ -f \"$TEST_ROOT/working-"+browser+"\" ]\n")
			}
			if tc.working != "" {
				write(filepath.Join(root, "working-"+tc.working), "original\n")
			}
			write(filepath.Join(bin, "dpkg"), "#!/bin/sh\nprintf 'arm64\\n'\n")
			write(filepath.Join(bin, "curl"), "#!/bin/sh\nprintf 'fixture public key\\n'\n")
			write(filepath.Join(bin, "gpg"), `#!/bin/sh
case "$*" in
  *--fingerprint*) printf 'fpr:::::::::%s:\n' "$TEST_FINGERPRINT" ;;
  *--export*) printf 'fixture verified keyring\n' ;;
esac
`)
			write(filepath.Join(bin, "apt-cache"), `#!/bin/sh
case " $TEST_AVAILABLE " in *" $2 "*) exit 0 ;; esac
exit 100
`)
			write(filepath.Join(bin, "apt-get"), `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$TEST_ROOT/apt-calls"
case "$*" in
  *update*)
    if [ "$TEST_FAILURE" = metadata ] && [ -f "$TEST_ROOT/apt/sources.list.d/crabbox-mozilla.sources" ]; then exit 100; fi
    exit 0 ;;
esac
for package in "$@"; do
  case "$package" in
    firefox/mozilla)
      [ "$TEST_FAILURE" != install ] || exit 100
      [ -s "$TEST_ROOT/apt/keyrings/crabbox-mozilla.gpg" ] || exit 99
      printf 'installed\n' > "$TEST_ROOT/working-firefox"
      exit 0 ;;
    chromium|firefox-esr|firefox)
      if [ "$TEST_DISTRO" = ubuntu ] && [ "$package" = firefox ]; then
        printf 'Snap transition attempted\n' >> "$TEST_ROOT/snap-attempt"
        exit 100
      fi
      case " $TEST_BROKEN " in *" $package "*) exit 100 ;; esac
      printf 'installed\n' > "$TEST_ROOT/working-$package"
      exit 0 ;;
  esac
done
`)
			start := strings.Index(bootstrapScript, `if [ "${CRABBOX_BROWSER:-0}" = "1" ] && command -v apt-get`)
			if start < 0 {
				t.Fatal("missing browser bootstrap")
			}
			end := strings.Index(bootstrapScript[start:], `if [ "${CRABBOX_DOCKER_SOCKET:-0}"`)
			if end < 0 {
				t.Fatal("missing next bootstrap section")
			}
			script := "set -eu\n" + installVerifiedAPTKeyringScript + bootstrapScript[start:start+end]
			script = strings.NewReplacer("/etc/apt/", root+"/apt/", "/etc/os-release", root+"/os-release", "/usr/local/bin/crabbox-browser", root+"/unused-browser").Replace(script)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", script)
			actualFingerprint := fingerprint
			if tc.failure == "key" {
				actualFingerprint = strings.Repeat("A", 40)
			}
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "CRABBOX_BROWSER=1", "TEST_ROOT="+root, "TEST_DISTRO="+tc.distro, "TEST_AVAILABLE="+tc.available, "TEST_BROKEN="+tc.broken, "TEST_FAILURE="+tc.failure, "TEST_FINGERPRINT="+actualFingerprint)
			output, err := cmd.CombinedOutput()
			calls, _ := os.ReadFile(filepath.Join(root, "apt-calls"))
			if tc.wantError != "" {
				if err == nil || !strings.Contains(string(output), tc.wantError) || !strings.Contains(string(output), "prebuilt") {
					t.Fatalf("err=%v output=%s, want actionable %q", err, output, tc.wantError)
				}
			} else if err != nil {
				t.Fatalf("err=%v output=%s", err, output)
			}
			if tc.working != "" {
				if len(calls) != 0 {
					t.Fatalf("working browser triggered package operations: %s", calls)
				}
				if got, _ := os.ReadFile(filepath.Join(root, "working-"+tc.working)); string(got) != "original\n" {
					t.Fatal("working browser changed")
				}
			}
			if tc.wantInstall != "" && !strings.Contains(string(calls), "install -y --no-install-recommends "+tc.wantInstall+"\n") {
				t.Fatalf("missing requested install %q: %s", tc.wantInstall, calls)
			}
			if _, err := os.Stat(filepath.Join(root, "snap-attempt")); !os.IsNotExist(err) {
				t.Fatal("attempted Ubuntu Snap transition")
			}
			if tc.distro == "ubuntu" && tc.working == "" && tc.failure != "key" {
				source, _ := os.ReadFile(filepath.Join(root, "apt/sources.list.d/crabbox-mozilla.sources"))
				for _, want := range []string{"URIs: https://packages.mozilla.org/apt", "Suites: mozilla", "Architectures: arm64", "Signed-By: " + root + "/apt/keyrings/crabbox-mozilla.gpg"} {
					if !strings.Contains(string(source), want) {
						t.Fatalf("source missing %q: %s", want, source)
					}
				}
				pins, _ := os.ReadFile(filepath.Join(root, "apt/preferences.d/crabbox-mozilla"))
				if !strings.Contains(string(pins), "Package: firefox\nPin: release o=Ubuntu\nPin-Priority: -1") || strings.Contains(string(pins), "Package: *") {
					t.Fatalf("unsafe package pin: %s", pins)
				}
			}
		})
	}
}
