//go:build !windows

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type workspaceOwnerWSLStageFixture struct {
	stages, cleanups int
	logPath          string
	marker           string
}

// Keep the real prepared envelope and execution boundary. Only the Windows
// verifier/launcher is replaced with a local POSIX stand-in; upstream tests
// separately exercise SFTP publication, verification and supervisor ownership.
func installWorkspaceOwnerWSLStageFixture(t *testing.T, home, path string) *workspaceOwnerWSLStageFixture {
	t.Helper()
	dir := t.TempDir()
	fixture := &workspaceOwnerWSLStageFixture{logPath: filepath.Join(dir, "executions")}
	commandPath, inputPath := filepath.Join(dir, "command"), filepath.Join(dir, "input")
	const launcher = "crabbox-test-staged-execution"
	nonce := strings.Repeat("d", 32)
	var staged *wslStageSpool
	var expected [sha256.Size]byte
	captureWSLStage(t, nonce, func(spool *wslStageSpool, target *SSHTarget, _ wslStageTiming, data []byte) {
		fixture.stages++
		staged, expected = spool, sha256.Sum256(data)
		_, helper, command, input := decodeWSLStage(t, data)
		if helper != wslLinuxHelper || expected != spool.digest() || int64(len(data)) != spool.size {
			t.Fatal("staged owner envelope lost integrity or helper bytes")
		}
		fixture.marker = spool.setupMarker
		for name, data := range map[string][]byte{commandPath: []byte(command), inputPath: input} {
			if err := os.WriteFile(name, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		// Let a workload deliberately emit its exact marker after handoff.
		if err := os.WriteFile(filepath.Join(home, "workload-marker"), []byte(spool.setupMarker+" failed registration\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		target.FallbackPorts, target.NoControlMaster = []string{}, true
	})
	oldLauncher, oldCleanup := buildWSLStageLauncher, cleanupPublishedWSLStage
	t.Cleanup(func() { buildWSLStageLauncher, cleanupPublishedWSLStage = oldLauncher, oldCleanup })
	buildWSLStageLauncher = func(gotNonce string, size int64, digest [sha256.Size]byte, shell wslStageShell) string {
		if staged == nil || gotNonce != nonce || size != staged.size || digest != expected || shell != wslStageCMD {
			t.Fatal("launcher lost its exact staged envelope")
		}
		return launcher
	}
	cleanupPublishedWSLStage = func(_ context.Context, _ SSHTarget, gotNonce string, spool *wslStageSpool, _, _ time.Duration, _ string) error {
		fixture.cleanups++
		if spool != staged || gotNonce != nonce || spool.digest() != expected {
			t.Fatal("cleanup lost the owned stage")
		}
		return nil
	}
	// Never record argv or command text. Readiness transport probes are fixed
	// and input-free; execution receives only the staged file's finite stdin.
	script := "#!/bin/sh\nfor arg; do [ \"$arg\" != -E ] || exit 91; remote=\"$arg\"; done\n" +
		"[ \"$remote\" != " + shellQuote(wsl2ReadinessCommand("exit 0")) + " ] || exit 0\n" +
		"[ \"$remote\" = " + shellQuote(launcher) + " ] || exit 92\n" +
		"/bin/cat > " + shellQuote(filepath.Join(dir, "received-stdin")) + "\n" +
		"[ ! -s " + shellQuote(filepath.Join(dir, "received-stdin")) + " ] || exit 93\n" +
		"printf 'execute\\n' >> " + shellQuote(fixture.logPath) + "\n" +
		"HOME=" + shellQuote(home) + "\nPATH=" + shellQuote(path) + "\nexport HOME PATH\n" +
		"exec /bin/sh " + shellQuote(commandPath) + " < " + shellQuote(inputPath) + "\n"
	writeExecutable(t, filepath.Join(dir, "ssh"), script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return fixture
}

func (f *workspaceOwnerWSLStageFixture) requireCalls(t *testing.T, stages, executions, cleanups int) {
	t.Helper()
	data, err := os.ReadFile(f.logPath)
	if err != nil && !(executions == 0 && os.IsNotExist(err)) {
		t.Fatal(err)
	}
	if f.stages != stages || strings.Count(string(data), "execute\n") != executions || f.cleanups != cleanups {
		t.Fatalf("stages=%d executions=%d cleanups=%d; want %d/%d/%d", f.stages, strings.Count(string(data), "execute\n"), f.cleanups, stages, executions, cleanups)
	}
}

func TestWorkspaceOwnerWSLControlSetupDiagnostic(t *testing.T) {
	for _, scenario := range []string{"setup-denied", "success", "workload-74", "ambiguous", "ownerless"} {
		t.Run(scenario, func(t *testing.T) {
			home, owner := workspaceOwnerSetupFixture(t)
			path := os.Getenv("PATH")
			denied := scenario == "setup-denied"
			if denied {
				path = workspaceOwnerDenySignals(t)
			}
			fixture := installWorkspaceOwnerWSLStageFixture(t, home, path)
			target := SSHTarget{Host: "127.0.0.1", User: "runner", Port: "2222", FallbackPorts: []string{"22"}, TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			if scenario != "ownerless" {
				ctx = contextWithWorkspaceOwner(ctx, owner)
			}
			marker := filepath.Join(home, "workload-started")
			remote := "touch " + shellQuote(marker) + "; printf control-output"
			wantCode := 0
			switch scenario {
			case "workload-74":
				remote += "; exit 74"
				wantCode = 74
			case "ambiguous":
				remote += "; printf %s " + shellQuote("mm_send_fd: sendmsg(1): broken pipe\n"+sshMuxDescriptorFailure+"\n") + " >&2; exit 255"
				wantCode = 7
			}
			out, err := runWSL2ControlScriptCombinedOutput(ctx, target, remote, 15*time.Second, "2", "1")
			if ctx.Err() != nil {
				t.Fatal("local staged WSL fixture exceeded its bound")
			}
			var setupErr *workspaceOwnerSetupError
			if denied {
				if !errors.As(err, &setupErr) || setupErr.phase != "registration" || out != "" || exitCode(err) != 74 {
					t.Fatal("staged setup lost typed error or leaked protocol output")
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("failed setup executed workload")
				}
			} else {
				gotCode := exitCode(err)
				var transportExit ExitError
				if AsExitError(err, &transportExit) {
					gotCode = transportExit.Code
				}
				outputOK := out == "control-output"
				if scenario == "ambiguous" {
					outputOK = strings.Count(out, "control-output") == 1 && strings.TrimSpace(strings.Replace(out, "control-output", "", 1)) == "mm_send_fd: sendmsg(1): broken pipe\n"+sshMuxDescriptorFailure
				}
				if gotCode != wantCode || errors.As(err, &setupErr) || !outputOK {
					t.Fatalf("staged workload changed: exit=%d typed=%t output-bytes=%d", gotCode, setupErr != nil, len(out))
				}
			}
			fixture.requireCalls(t, 1, 1, map[bool]int{true: 1, false: 0}[err != nil])
		})
	}
}

func TestWorkspaceOwnerWSLStagedInputAndSharedOutput(t *testing.T) {
	home, owner := workspaceOwnerSetupFixture(t)
	fixture := installWorkspaceOwnerWSLStageFixture(t, home, os.Getenv("PATH"))
	target := SSHTarget{Host: "127.0.0.1", Port: "22", FallbackPorts: []string{}, TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	ctx, cancel := context.WithTimeout(contextWithWorkspaceOwner(t.Context(), owner), 12*time.Second)
	defer cancel()
	input := bytes.Repeat([]byte{0, 255, 128, '\n'}, 1024)
	var stdout, stderr bytes.Buffer
	err := runSSHInputStream(ctx, target, "cat; cat \"$HOME/workload-marker\" >&2; exit 74", bytes.NewReader(input), &stdout, &stderr)
	var setupErr *workspaceOwnerSetupError
	if exitCode(err) != 74 || errors.As(err, &setupErr) || !bytes.Equal(stdout.Bytes(), input) || stderr.String() != fixture.marker+" failed registration\n" {
		t.Fatal("staged handoff changed binary input, exact marker-like workload stderr, or exit 74")
	}
	var combined bytes.Buffer
	remote := `(i=0; while [ "$i" -lt 2000 ]; do printf 'stdout\n'; i=$((i+1)); done) &
i=0; while [ "$i" -lt 2000 ]; do printf 'stderr\n' >&2; i=$((i+1)); done
wait`
	code, err := runSSHStreamResult(ctx, target, remote, &combined, &combined)
	if code != 0 || err != nil || strings.Count(combined.String(), "stdout\n") != 2000 || strings.Count(combined.String(), "stderr\n") != 2000 {
		t.Fatal("staged shared stdout/stderr lost serialization or output")
	}
	fixture.requireCalls(t, 2, 2, 1)
}

func TestWorkspaceOwnerWSLStagedQuietAndBoundedOutput(t *testing.T) {
	for _, bounded := range []bool{false, true} {
		t.Run(map[bool]string{false: "quiet", true: "bounded"}[bounded], func(t *testing.T) {
			home, owner := workspaceOwnerSetupFixture(t)
			fixture := installWorkspaceOwnerWSLStageFixture(t, home, workspaceOwnerDenySignals(t))
			target := SSHTarget{Host: "127.0.0.1", Port: "22", FallbackPorts: []string{}, TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
			ctx, cancel := context.WithTimeout(contextWithWorkspaceOwner(t.Context(), owner), 12*time.Second)
			defer cancel()
			var err error
			if bounded {
				var out string
				out, err = RunSSHOutputBounded(ctx, target, "printf unexpected", 16)
				if out != "" {
					t.Fatal("bounded setup failure leaked output")
				}
			} else {
				err = runSSHQuiet(ctx, target, "true")
			}
			var setupErr *workspaceOwnerSetupError
			if !errors.As(err, &setupErr) || setupErr.phase != "registration" || ctx.Err() != nil {
				t.Fatal("quiet staged execution lost setup failure")
			}
			fixture.requireCalls(t, 1, 1, 1)
		})
	}
}

func TestWaitForSSHReadyWSLStagedOwnerSetupFailure(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "proxy"}[proxy], func(t *testing.T) {
			home, owner := workspaceOwnerSetupFixture(t)
			fixture := installWorkspaceOwnerWSLStageFixture(t, home, workspaceOwnerDenySignals(t))
			target := SSHTarget{Host: "127.0.0.1", Port: "2222", FallbackPorts: []string{"22"}, TargetOS: targetWindows, WindowsMode: windowsModeWSL2, ReadyCheck: "true", SSHConfigProxy: proxy}
			oldProbe := probeWSLSFTPSubsystem
			probes := 0
			probeWSLSFTPSubsystem = func(context.Context, SSHTarget, string, string, io.Writer) error { probes++; return nil }
			t.Cleanup(func() { probeWSLSFTPSubsystem = oldProbe })
			ctx, cancel := context.WithTimeout(contextWithWorkspaceOwner(t.Context(), owner), 12*time.Second)
			defer cancel()
			var progress bytes.Buffer
			err := waitForSSHReady(ctx, &target, &progress, "before sync", 12*time.Second)
			var setupErr *workspaceOwnerSetupError
			if !errors.As(err, &setupErr) || setupErr.phase != "registration" || ctx.Err() != nil || probes != 1 || progress.Len() != 0 {
				t.Fatal("staged readiness lost setup error or retried it as transport failure")
			}
			fixture.requireCalls(t, 1, 1, 1)
		})
	}
}

func TestWorkspaceOwnerWSLStageFailureIsNotWitnessSetup(t *testing.T) {
	oldStage := stageWSLSpool
	t.Cleanup(func() { stageWSLSpool = oldStage })
	var unrelated string
	stageWSLSpool = func(s *wslStageSpool, _ context.Context, _ *SSHTarget, _ wslStageTiming, _, _ string, stderr io.Writer) (string, error) {
		// Even exact-looking lower-level diagnostics must not activate the
		// witness classifier before the staged workload has been launched.
		unrelated = s.setupMarker + " failed staging\n"
		_, _ = io.WriteString(stderr, unrelated)
		return "", errors.New("stage upload rejected")
	}
	ctx := contextWithWorkspaceOwner(t.Context(), &workspaceOwner{key: workspaceOwnerKey("stage-failure"), token: strings.Repeat("a", 64)})
	target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	var stdout, stderr bytes.Buffer
	code, err := runSSHStreamResult(ctx, target, "true", &stdout, &stderr)
	var setupErr *workspaceOwnerSetupError
	if code != 1 || errors.As(err, &setupErr) || stdout.Len() != 0 || stderr.String() != unrelated {
		t.Fatal("lower-level staging was misclassified as witness setup")
	}
}
