//go:build !windows

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunFailureTailHasOneOutputOwner(t *testing.T) {
	var longOutput strings.Builder
	for i := 0; i < 45; i++ {
		fmt.Fprintf(&longOutput, "stdout-line-%02d\n", i)
	}
	longOutput.WriteString("stdout-unterminated")
	authHTML := "<!doctype html><html><head><title>Cloudflare Access</title></head><body>login</body></html>\n"
	appHTML := "<!doctype html><html><head><title>App Login</title></head><body>401 Unauthorized access denied</body></html>\n"
	for _, test := range []struct {
		name                         string
		stdout, stderr               string
		captureStdout, captureStderr bool
		redacted                     bool
	}{
		{name: "stdout", stdout: "stdout-sentinel\n"},
		{name: "stderr", stderr: "stderr-sentinel\n"},
		{name: "both", stdout: "stdout-sentinel\n", stderr: "stderr-sentinel\n"},
		{name: "empty"},
		{name: "bounded unterminated tail", stdout: longOutput.String()},
		{name: "capture stdout", stdout: "captured-stdout\x00\xff", stderr: "stderr-sentinel\n", captureStdout: true},
		{name: "capture stderr", stdout: "stdout-sentinel\n", stderr: "captured-stderr\x00\xfe", captureStderr: true},
		{name: "capture both", stdout: "captured-stdout\x00\xff", stderr: "captured-stderr\x00\xfe", captureStdout: true, captureStderr: true},
		{name: "provider auth redaction", stderr: authHTML, redacted: true},
		{name: "application HTML", stderr: appHTML},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := setupRunCleanupWorkspaceOwnerTest(t)
			stdoutFixture, stderrFixture := filepath.Join(dir, "stdout.data"), filepath.Join(dir, "stderr.data")
			for path, data := range map[string]string{stdoutFixture: test.stdout, stderrFixture: test.stderr} {
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			installWorkspaceOwnerAwareSSH(t, filepath.Join(dir, "ssh"), `#!/bin/sh
case "$1" in
  *failure-tail-fixture*)
    cat `+shellQuote(stdoutFixture)+`
    cat `+shellQuote(stderrFixture)+` >&2
    exit 23
    ;;
  *.crabbox/capture-manifest.txt*) exit 7 ;;
esac
exit 0
`)
			releases := 0
			runEnvProfileTestReleaseHook = func() error { releases++; return nil }
			args := []string{"--provider", "run-env-profile-test", "--no-sync", "--no-hydrate", "--timing-json", "--timing-record", "off"}
			stdoutCapture, stderrCapture := filepath.Join(dir, "captured-stdout.bin"), filepath.Join(dir, "captured-stderr.bin")
			if test.captureStdout {
				args = append(args, "--capture-stdout", stdoutCapture)
			}
			if test.captureStderr {
				args = append(args, "--capture-stderr", stderrCapture)
			}
			args = append(args, "--", "failure-tail-fixture")
			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(t.Context(), args)
			assertRunCleanupExitCode(t, err, 23, stdout.String(), stderr.String())
			before, after, found := strings.Cut(stderr.String(), "failure digest\n")
			if !found {
				t.Fatalf("missing failure digest:\n%s", stderr.String())
			}
			if strings.Contains(after, "  tail stdout:") || strings.Contains(after, "  tail stderr:") {
				t.Errorf("digest duplicates the post-failure tail:\n%s", after)
			}
			for _, stream := range []struct {
				label, data, capture string
				captured             bool
			}{
				{"stdout", test.stdout, stdoutCapture, test.captureStdout},
				{"stderr", test.stderr, stderrCapture, test.captureStderr},
			} {
				if count := strings.Count(after, stream.label+" tail"); count != 1 {
					t.Errorf("%s tail sections=%d, want 1", stream.label, count)
				}
				if stream.captured {
					data, err := os.ReadFile(stream.capture)
					if err != nil || string(data) != stream.data {
						t.Errorf("%s capture=%q error=%v", stream.label, data, err)
					}
					if strings.Contains(stdout.String()+stderr.String(), stream.data) || !strings.Contains(after, stream.label+" tail: captured at "+stream.capture) {
						t.Errorf("%s capture leaked or lost its notice", stream.label)
					}
					continue
				}
				if stream.label == "stdout" && stdout.String() != stream.data {
					t.Errorf("live stdout=%q, want %q", stdout.String(), stream.data)
				}
				if stream.label == "stderr" && stream.data != "" && strings.Count(before, stream.data) != 1 {
					t.Errorf("live stderr missing or duplicated: %q", before)
				}
				if stream.data == "" {
					if !strings.Contains(after, stream.label+" tail: empty") {
						t.Errorf("missing %s empty notice", stream.label)
					}
					continue
				}
				if test.redacted && stream.label == "stderr" {
					if strings.Contains(after, "<html>") || strings.Count(after, "redacted auth_cloudflare_html response") != 1 {
						t.Errorf("provider auth tail not redacted exactly once: %s", after)
					}
					continue
				}
				lines := strings.Split(strings.TrimSuffix(stream.data, "\n"), "\n")
				for i, line := range lines {
					want := 1
					if i < len(lines)-40 {
						want = 0
					}
					count := 0
					for _, displayed := range strings.Split(after, "\n") {
						if strings.TrimSpace(displayed) == line {
							count++
						}
					}
					if count != want {
						t.Errorf("post-failure line %q count=%d, want %d", line, count, want)
					}
				}
			}
			bundles, err := filepath.Glob(filepath.Join(dir, ".crabbox", "captures", "*.tar.gz"))
			if err != nil || len(bundles) != 1 {
				t.Fatalf("bundles=%v error=%v", bundles, err)
			}
			contents := readTarGzContents(t, bundles[0])
			if string(contents["crabbox-artifacts/stdout.log"]) != test.stdout || string(contents["crabbox-artifacts/stderr.log"]) != test.stderr {
				t.Error("failure bundle changed original stream bytes")
			}
			lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
			var timing TimingReport
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &timing); err != nil {
				t.Fatalf("final timing JSON: %v", err)
			}
			if timing.ExitCode != 23 || timing.LeaseStopped == nil || !*timing.LeaseStopped || releases != 1 {
				t.Errorf("exit/cleanup changed: timing=%+v releases=%d", timing, releases)
			}
		})
	}
}
