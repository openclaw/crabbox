//go:build !windows

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOptionalPackagesRefreshStaleIndexes(t *testing.T) {
	for _, tc := range []struct {
		name, mode, want string
		fails            bool
	}{
		{"stale image", "stale", "update\ninstall\n", false},
		{"mirror rollover", "rollover", "update\ninstall\nupdate\ninstall\n", false},
		{"partial update", "update-fails", "update\nupdate\nupdate\n", true},
		{"install fails", "install-fails", strings.Repeat("update\ninstall\n", 3), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Execute the generated fragment with no real package-manager access.
			script := `set -eu
dpkg-query() { return 1; }
sleep() { :; }
timeout() { shift; "$@"; }
updated=0
apt-get() {
  case " $* " in
    *" update "*)
      case " $* " in *" APT::Update::Error-Mode=any "*) ;; *) return 99 ;; esac
      echo update
      [ "$MODE" != update-fails ] || return 42
      updated=$((updated + 1)) ;;
    *" install "*)
      echo install
      [ "$updated" -gt 0 ] || return 99
      [ "$MODE" != install-fails ] || return 43
      [ "$MODE" != rollover ] || [ "$updated" -gt 1 ] ;;
    *) return 99 ;;
  esac
}
` + sharedLinuxOptionalPackages() + "\ncrabbox_install_packages labwc wayvnc libinput10\n"
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-c", script)
			cmd.Env = []string{"PATH=/usr/bin:/bin", "MODE=" + tc.mode}
			output, err := cmd.CombinedOutput()
			if (err != nil) != tc.fails || string(output) != tc.want {
				t.Fatalf("got %q, %v; want %q, fails=%t", output, err, tc.want, tc.fails)
			}
		})
	}
}

func TestPreparedBrowserBootstrapAvoidsPackageAndNetworkWork(t *testing.T) {
	cfg := baseConfig()
	cfg.Browser = true
	script, _, found := strings.Cut(cloudInitOptionalBootstrap(cfg), "    if [ -n \"$browser_path\" ]; then\n")
	if !found {
		t.Fatal("missing browser wrapper setup after package preparation")
	}
	fixture, err := os.ReadFile("../../testdata/bootstrap/installed-browser-fixture.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		env      []string
		wantAPT  bool
		wantCode int
	}{
		{name: "prepared image"},
		{name: "missing build helper", env: []string{"MISSING_PACKAGE=build-essential", "INSTALL_ALLOWED=1"}, wantAPT: true},
		{name: "unconfigured build helper", env: []string{"BROKEN_PACKAGE=build-essential", "INSTALL_ALLOWED=1"}, wantAPT: true},
		{name: "install failure", env: []string{"MISSING_PACKAGE=build-essential", "INSTALL_ALLOWED=1", "INSTALL_FAIL=1"}, wantAPT: true, wantCode: 47},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-c", string(fixture)+"\n"+script+"\nprintf '%s\\n' \"$browser_path\"\n")
			cmd.Env = append([]string{"PATH=/usr/bin:/bin", "FIXTURE_BIN=" + filepath.Join(dir, "bin"), "FIXTURE_LOG=" + filepath.Join(dir, "calls")}, tc.env...)
			cmd.WaitDelay = time.Second
			output, _ := cmd.CombinedOutput()
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != tc.wantCode {
				t.Fatalf("prepared browser bootstrap: got %v, want exit %d\n%s", cmd.ProcessState, tc.wantCode, output)
			}
			if tc.wantCode == 0 && strings.TrimSpace(string(output)) != filepath.Join(dir, "bin", "google-chrome") {
				t.Fatalf("wrong prepared browser: %s", output)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			want := ""
			if tc.wantAPT {
				want = "apt-get -o APT::Update::Error-Mode=any -o Acquire::Retries=2 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 -o Acquire::Languages=none -o Acquire::IndexTargets::deb::DEP-11::DefaultEnabled=false -o Acquire::IndexTargets::deb::CNF::DefaultEnabled=false update\napt-get -o Acquire::Retries=2 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 install -y --no-install-recommends gnupg build-essential python3\n"
				if tc.wantCode != 0 {
					want = strings.Repeat(want, 3)
				}
			}
			if string(calls) != want {
				t.Fatalf("package/network calls: got %q, want %q", calls, want)
			}
		})
	}
}
