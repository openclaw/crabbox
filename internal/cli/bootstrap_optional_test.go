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
				want = "apt-get install -y --no-install-recommends gnupg build-essential python3\n"
			}
			if string(calls) != want {
				t.Fatalf("package/network calls: got %q, want %q", calls, want)
			}
		})
	}
}
