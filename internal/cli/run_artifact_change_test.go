package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestArtifactChangePathValidation(t *testing.T) {
	for _, p := range []string{"", "/a", "../a", "a/../b", "./a", "a//b", "a/", "a/*", "a/?", ".git/config", "a/.crabbox/data", "a/.git/config", "-a", "a b", "a\nb", strings.Repeat("a", 1025)} {
		if err := validateArtifactChangePaths([]string{p}); err == nil {
			t.Errorf("accepted %q", p)
		}
	}
	if err := validateArtifactChangePaths([]string{"reports/proof.json", "empty"}); err != nil {
		t.Fatal(err)
	}
	if err := validateArtifactChangePaths(make([]string, maxArtifactChangePaths+1)); err == nil {
		t.Fatal("accepted too many paths")
	}
}

func TestRunArtifactChangeRejectsBlankPathBeforeNormalization(t *testing.T) {
	for _, p := range []string{"", " ", "proof "} {
		t.Run(fmt.Sprintf("path=%q", p), func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "config.yaml"))
			err := (App{Stdout: io.Discard, Stderr: io.Discard}).runCommand(context.Background(), []string{"--provider", "run-env-profile-test", "--require-artifact-change", p, "--", "true"})
			if exitCodeForError(err, 0) != 2 || !strings.Contains(fmt.Sprint(err), "--require-artifact-change") {
				t.Fatalf("invalid exact path was normalized away: %v", err)
			}
		})
	}
}

func TestArtifactChangeSnapshots(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"stale", "changed", "created", "missing", "removed", "identical", "empty"}
	for _, p := range []string{"stale", "changed", "removed", "identical"} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func() []artifactChangeSnapshot {
		t.Helper()
		cmd := exec.Command("bash", "-s")
		cmd.Stdin = strings.NewReader(artifactChangeSnapshotScript(dir, paths))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("snapshot: %v %s", err, out)
		}
		got, err := parseArtifactChangeSnapshot(string(out), paths)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	before := snapshot()
	for p, data := range map[string]string{"changed": "new", "created": "new", "identical": "old", "empty": ""} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(dir, "removed")); err != nil {
		t.Fatal(err)
	}
	after := snapshot()
	results, err := compareArtifactChanges(paths, before, after)
	if err == nil {
		t.Fatal("accepted unchanged/missing evidence")
	}
	want := []string{"unchanged", "changed", "created", "missing", "missing", "unchanged", "created"}
	for i, result := range results {
		if result.Status != want[i] {
			t.Errorf("%s=%s want %s", result.Path, result.Status, want[i])
		}
	}
	// Collection must use the accepted snapshot, even if a later producer replaces it.
	if err := os.WriteFile(filepath.Join(dir, "changed"), []byte("later"), 0600); err != nil {
		t.Fatal(err)
	}
	artifacts, err := collectChangedArtifacts(dir, "test-run", "lease", results, after)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(artifacts[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[h.Name] = string(data)
	}
	if !reflect.DeepEqual(files, map[string]string{"changed": "new", "created": "new", "empty": ""}) {
		t.Fatalf("archive=%v", files)
	}
}

func TestArtifactChangeSnapshotRejectsUnsafeFilesAndBounds(t *testing.T) {
	for _, kind := range []string{"symlink", "parent symlink", "directory", "oversized", "aggregate"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			paths := []string{"proof"}
			switch kind {
			case "symlink":
				if err := os.Symlink("absent", filepath.Join(dir, "proof")); err != nil {
					t.Fatal(err)
				}
			case "parent symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(dir, "proof")); err != nil {
					t.Fatal(err)
				}
				paths = []string{"proof/absent"}
			case "directory":
				if err := os.Mkdir(filepath.Join(dir, "proof"), 0700); err != nil {
					t.Fatal(err)
				}
			default:
				count, size := 1, int64(maxArtifactChangeFileBytes+1)
				if kind == "aggregate" {
					count, size = 5, maxArtifactChangeFileBytes
				}
				paths = nil
				for i := 0; i < count; i++ {
					p := fmt.Sprintf("proof%d", i)
					paths = append(paths, p)
					f, err := os.Create(filepath.Join(dir, p))
					if err != nil {
						t.Fatal(err)
					}
					if err := f.Truncate(size); err != nil {
						t.Fatal(err)
					}
					if err := f.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			cmd := exec.Command("bash", "-s")
			cmd.Stdin = strings.NewReader(artifactChangeSnapshotScript(dir, paths))
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err == nil {
				t.Fatal("unsafe snapshot passed")
			}
			if !strings.Contains(stderr.String(), "artifact change") {
				t.Fatalf("diagnostic=%s", stderr.String())
			}
		})
	}
}

func TestRunArtifactChangeRejectsUnsupportedRoutes(t *testing.T) {
	for _, flags := range [][]string{{"--provider", "daytona"}, {"--provider", "ssh", "--target", "macos"}, {"--provider", "ssh", "--target", "windows", "--windows-mode", "wsl2"}, {"--provider", "ssh", "--target", "windows"}, {"--provider", "aws", "--sync-only"}} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "config.yaml"))
			var stderr bytes.Buffer
			args := append(append([]string{}, flags...), "--require-artifact-change", "proof", "--", "true")
			err := (App{Stdout: io.Discard, Stderr: &stderr}).runCommand(context.Background(), args)
			if exitCodeForError(err, 0) != 2 || !strings.Contains(fmt.Sprint(err), "--require-artifact-change") {
				t.Fatalf("err=%v stderr=%s", err, stderr.String())
			}
		})
	}
}

func TestRunArtifactChangeE2E(t *testing.T) {
	runArtifactChangeE2E(t, false)
}

func TestRunArtifactChangeWithFailureDownloadsE2E(t *testing.T) {
	runArtifactChangeE2E(t, true)
}

func runArtifactChangeE2E(t *testing.T, failureDownloads bool) {
	t.Helper()
	for _, tc := range []struct {
		name, command, status string
		code                  int
		schema                bool
	}{
		{"stale", "true", "unchanged", 7, false},
		{"identical", "printf old > proof", "unchanged", 7, false},
		{"changed", "printf new > proof", "changed", 0, false},
		{"created", "printf new > created", "created", 0, false},
		{"missing", "rm proof", "missing", 7, false},
		{"failed", "exit 23", "not-evaluated", 23, false},
		{"failed with changed bytes", "printf new > proof; exit 23", "not-evaluated", 23, false},
		{"workload exit 255", "exit 255", "not-evaluated", 255, false},
		{"transport", "echo TRANSPORT_BREAK", "not-evaluated", 255, false},
		{"schema cannot rescue stale", "true", "unchanged", 7, true},
		{"schema after change", "printf '\"new\"' > proof", "changed", 0, true},
		{"changed before JUnit", "printf new > proof", "changed", 1, false},
		{"stale before JUnit", "true", "unchanged", 7, false},
		{"changed before nested JUnit", "printf new > proof", "changed", 1, false},
		{"failed before nested JUnit", "exit 23", "not-evaluated", 23, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			t.Chdir(dir)
			remoteRoot := filepath.Join(dir, "remote")
			workdir := filepath.Join(remoteRoot, "cbx_env_profile_test", filepath.Base(dir))
			if err := os.MkdirAll(workdir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workdir, "proof"), []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					_ = conn.Close()
				}
			}()
			_, port, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			ssh := `#!/bin/sh
cmd=""
for arg do cmd="$arg"; done
case "$cmd" in
  *TRANSPORT_BREAK*) exit 255 ;;
  mkdir\ -p*|cd\ *|\(cd\ *|bash\ -lc*|/bin/bash\ -lc*) exec sh -c "$cmd" ;;
esac
exit 0
`
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(ssh), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "config.yaml"))
			t.Setenv("CRABBOX_FAKE_SSH_PORT", port)
			t.Setenv("CRABBOX_WORK_ROOT", remoteRoot)
			download := filepath.Join(dir, "failed-proof")
			wantDownload := failureDownloads && tc.status == "not-evaluated" && tc.name != "transport"
			wantArchive := tc.code == 0 || tc.name == "changed before JUnit" || tc.name == "changed before nested JUnit"
			var stdout, stderr bytes.Buffer
			releases := 0
			runEnvProfileTestReleaseHook = func() error {
				releases++
				if wantDownload {
					if _, err := os.Stat(download); err != nil {
						return fmt.Errorf("failure evidence not collected before teardown: %w", err)
					}
				}
				if wantArchive {
					files, _ := filepath.Glob(filepath.Join(dir, ".crabbox", "runs", "*", "*-artifacts.tgz"))
					if len(files) != 1 {
						return fmt.Errorf("archive not collected before teardown: %v", files)
					}
				}
				return nil
			}
			t.Cleanup(func() { runEnvProfileTestReleaseHook = nil })
			p := "proof"
			if tc.name == "created" {
				p = "created"
			}
			args := []string{"--provider", "run-env-profile-test", "--no-sync", "--stop-after", "always", "--timing-json", "--require-artifact-change", p, "--artifact-glob", "*"}
			if failureDownloads {
				args = append(args, "--download-on-failure", "proof="+download)
			}
			if tc.schema {
				if err := os.WriteFile(filepath.Join(dir, "schema.json"), []byte(`true`), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--require-artifact-schema", "proof=schema.json")
			}
			command := tc.command
			receiptPath := filepath.Join(dir, "receipt.json")
			if strings.HasSuffix(tc.name, "before JUnit") {
				args = append(args, "--junit", "junit.xml", "--fail-on-test-failures")
				command += `; printf '<testsuite tests="1" failures="1"><testcase name="failed"><failure message="expected"/></testcase></testsuite>' > junit.xml`
			}
			if strings.HasSuffix(tc.name, "before nested JUnit") {
				args = append(args, "--junit", "junit.xml", "--fail-on-test-failures", "--attest", receiptPath)
				command = `printf '<testsuites><testsuite name="project"><testsuite name="leaf"><testcase name="failed"><failure message="expected"/></testcase></testsuite></testsuite></testsuites>' > junit.xml; ` + command
			}
			args = append(args, "--", "sh", "-c", command)
			err = (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), args)
			if exitCodeForError(err, 0) != tc.code {
				t.Fatalf("err=%v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
			}
			if strings.HasSuffix(tc.name, "before nested JUnit") {
				if !strings.Contains(stderr.String(), "test results files=1 tests=1 failures=1") {
					t.Fatalf("nested report not collected and parsed: %s", stderr.String())
				}
				data, err := os.ReadFile(receiptPath)
				if err != nil {
					t.Fatal(err)
				}
				receipt, err := decodeTerminalRunReceipt(data)
				if err != nil {
					t.Fatal(err)
				}
				if receipt.SchemaVersion != terminalReceiptSchemaVersion || receipt.ReceiptType != terminalReceiptType || receipt.ExitCode != tc.code {
					t.Fatalf("finalized receipt=%+v, want exit %d", receipt, tc.code)
				}
				if err := verifyTerminalRunReceiptSignature(receipt); err != nil {
					t.Fatalf("verify failure receipt: %v", err)
				}
			}
			var report timingReport
			for _, line := range strings.Split(stderr.String(), "\n") {
				if strings.HasPrefix(line, `{"provider"`) {
					if err := json.Unmarshal([]byte(line), &report); err != nil {
						t.Fatal(err)
					}
				}
			}
			if len(report.ArtifactChanges) != 1 || report.ArtifactChanges[0].Status != tc.status {
				t.Fatalf("report=%+v stderr=%s", report, stderr.String())
			}
			if tc.schema && tc.code == 0 {
				if len(report.SchemaValidations) != 1 || !report.SchemaValidations[0].Valid {
					t.Fatalf("schema not validated after change: %+v", report)
				}
			} else if len(report.SchemaValidations) != 0 {
				t.Fatalf("schema ran after stale guard: %+v", report)
			}
			if strings.HasSuffix(tc.name, "before nested JUnit") {
				if report.ExitCode != tc.code || report.RunStatus != "failed" {
					t.Fatalf("final report lost test/command failure: %+v", report)
				}
			}
			if wantArchive {
				if len(report.Artifacts) != 1 {
					t.Fatalf("artifacts=%+v", report.Artifacts)
				}
				names := tarGzNames(t, report.Artifacts[0].Path)
				if !reflect.DeepEqual(names, []string{p}) {
					t.Fatalf("archive admitted extra paths: %v", names)
				}
			} else if len(report.Artifacts) != 0 {
				t.Fatalf("failed run archived evidence: %+v", report.Artifacts)
			}
			if releases != 1 || report.LeaseStopped == nil || !*report.LeaseStopped {
				t.Fatalf("cleanup releases=%d report=%+v", releases, report)
			}
			data, readErr := os.ReadFile(download)
			if wantDownload {
				want := "old"
				if tc.name == "failed with changed bytes" {
					want = "new"
				}
				if readErr != nil || string(data) != want {
					t.Fatalf("failure evidence=%q err=%v stderr=%s", data, readErr, stderr.String())
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatalf("ineligible failure evidence exists: %q err=%v", data, readErr)
			}
			lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
			if strings.HasSuffix(tc.name, "before nested JUnit") {
				// Failure receipts finalize after the last timing record.
				if !strings.HasPrefix(lines[len(lines)-1], "artifact kind=receipt path="+receiptPath+" bytes=") {
					t.Fatalf("failure receipt is not the final artifact: %s", stderr.String())
				}
				lines = lines[:len(lines)-1]
			}
			if !strings.HasPrefix(lines[len(lines)-1], `{"provider"`) {
				t.Fatalf("timing report is not the final record: %s", stderr.String())
			}
		})
	}
}
