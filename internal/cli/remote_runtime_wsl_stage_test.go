package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/crabbox/internal/remoteruntime"
	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

func nativeWSLStageTestRuntime(path string) *remoteNativeRuntime {
	return &remoteNativeRuntime{path: path, shell: wslStagePowerShell, target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}}
}

func TestNativeWSLStageFrame(t *testing.T) {
	for _, tc := range []struct {
		name, purpose string
		worker        time.Duration
		limit         sshCommandLimit
	}{
		{"unlimited", "command", 0, sshCommandLimit{}},
		{"control", "command", sshControlExecutionLimit, sshCommandLimit{execution: sshControlExecutionLimit, control: true}},
		{"preflight", "preflight", pythonVenvPreflightExecutionTime, sshCommandLimit{execution: functionalPreflightSupervisorAllowance}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := nativeWSLStageTestRuntime("/tmp/runtime/crabbox")
			program, err := nativeWSLStageProgram(runtime, tc.purpose, tc.worker)
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte{0, 255, 13, 10, 128}
			spool, err := newWSLStageSpoolWithProgram("printf fixture", payload, nil, int64(len(payload)), tc.limit, program)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := spool.close(); err != nil {
					t.Error(err)
				}
			}()
			reader, err := spool.input.reset()
			if err != nil {
				t.Fatal(err)
			}
			frame, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			owner, helper, command, gotPayload := decodeWSLStage(t, frame)
			if spool.nativeRuntime != runtime || helper != program.source || command != "printf fixture" || !bytes.Equal(gotPayload, payload) {
				t.Fatal("native stage changed its helper, handle, command or finite binary input")
			}
			if binary.LittleEndian.Uint64(frame[32:]) != uint64(sshTransportTiming(tc.limit).operation.Milliseconds()) {
				t.Fatal("native worker allowance replaced the Windows operation clock")
			}
			if !strings.Contains(owner, "$(if ($mode -eq 'cleanup') { Remaining 10000 } else { 0 })") || strings.Count(owner, "$clock.Restart()") != 1 {
				t.Fatal("native cleanup did not inherit the original bounded Windows clock")
			}
		})
	}
}

func TestNativeWSLStageAdapterDispatch(t *testing.T) {
	sh, err := exec.LookPath("/bin/sh")
	if err != nil {
		t.Skip("local POSIX shell unavailable")
	}
	dir := t.TempDir()
	argsPath, runtimePath := filepath.Join(dir, "args"), filepath.Join(dir, "runtime é'quoted")
	fake := "#!/bin/sh\nprintf '%s\\000' \"$@\" >" + shellQuote(argsPath) + "\nif [ \"$2\" = control ]; then exit 0; fi\ncat\nprintf 'owned stderr' >&2\nexit 23\n"
	if err := os.WriteFile(runtimePath, []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	program, err := nativeWSLStageProgram(nativeWSLStageTestRuntime(runtimePath), "preflight", 1500*time.Microsecond)
	if err != nil {
		t.Fatal(err)
	}
	nonce := strings.Repeat("a", 32)
	frame := []byte{'c', 'm', 'd', 0, 255, 13, 10}
	for _, preamble := range []int{0, 3} {
		for _, mode := range []string{"run", "cleanup"} {
			var input []byte
			if preamble != 0 {
				input = []byte{0xef, 0xbb, 0xbf}
			}
			input = append(input, []byte(program.source)...)
			if mode == "run" {
				input = append(input, frame...)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			cmd := exec.CommandContext(ctx, sh, "-c", program.bootstrap, "sh", strconv.Itoa(len(program.source)), strconv.Itoa(preamble), mode, "/tmp/crabbox-command-"+nonce, nonce, "3", "4", "15000", "5000", "7231")
			cmd.Stdin = bytes.NewReader(input)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err = cmd.Run()
			cancel()
			data, readErr := os.ReadFile(argsPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			got := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
			want := []string{remoteruntime.Command, "run", remoteruntime.Protocol, nonce, "3", "4", "15000", "5000", "2", "preflight", "watched"}
			if mode == "cleanup" {
				want = []string{remoteruntime.Command, "control", remoteruntime.Protocol, nonce, "cleanup", "7231"}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("mode=%s args=%q want=%q", mode, got, want)
			}
			if mode == "run" {
				if exitCode(err) != 23 || !bytes.Equal(stdout.Bytes(), frame) || stderr.String() != "owned stderr" {
					t.Fatalf("run exit=%d stdout=%x stderr=%q", exitCode(err), stdout.Bytes(), stderr.String())
				}
			} else if err != nil || stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("cleanup result: %v %q %q", err, stdout.String(), stderr.String())
			}
		}
	}
}

func TestNativeWSLFunctionalPreflightSelection(t *testing.T) {
	oldStage := stageWSLSpool
	t.Cleanup(func() { stageWSLSpool = oldStage })
	scope := testNativeRuntimeScope()
	scope.manifest = "synthetic selected artifact set"
	installs := 0
	scope.install = func(_ context.Context, target SSHTarget, _ *runtimeartifact.LocalSet) (*remoteNativeRuntime, error) {
		installs++
		runtime := nativeWSLStageTestRuntime("/tmp/runtime/crabbox")
		runtime.target = target
		return runtime, nil
	}
	ctx := context.WithValue(t.Context(), nativeRuntimeScopeKey{}, scope)
	target := SSHTarget{Host: "example.invalid", Port: "22", TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	staged := errors.New("owned fixture stops before remote dispatch")
	stageWSLSpool = func(spool *wslStageSpool, ctx context.Context, _ *SSHTarget, _ wslStageTiming, _, _ string, _ io.Writer) (string, error) {
		if spool.nativeRuntime == nil || ctx.Value(nativeRuntimeContextKey{}) != spool.nativeRuntime {
			t.Fatal("native preflight lost its installed runtime binding")
		}
		reader, err := spool.input.reset()
		if err != nil {
			t.Fatal(err)
		}
		frame, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		_, helper, command, payload := decodeWSLStage(t, frame)
		if !strings.Contains(helper, "90000 'preflight' watched") || !strings.Contains(command, "python3 -I -B -c") || len(payload) != 0 {
			t.Fatal("native functional stage lost worker allowance or command")
		}
		return "", staged
	}
	for range 2 {
		if _, err := runWSLFunctionalPreflight(ctx, target, "/work", nil, nil); !errors.Is(err, staged) {
			t.Fatal(err)
		}
	}
	if installs != 1 {
		t.Fatalf("preflight reinstalled runtime %d times", installs)
	}
	if err := scope.finish(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestNativeWSLCommandCleanupOutcome(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		t.Run(strconv.FormatBool(cleanupFails), func(t *testing.T) {
			// The stage lifecycle uses an owned POSIX fake SSH transport here;
			// Windows launcher bytes and handles have separate platform fixtures.
			target := nativeRuntimeTestTarget(t, false, false)
			root := t.TempDir()
			runtimePath, argsPath := filepath.Join(root, "runtime"), filepath.Join(root, "args")
			code := "0"
			if cleanupFails {
				code = "74"
			}
			if err := os.WriteFile(runtimePath, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >"+shellQuote(argsPath)+"\nexit "+code+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			native := &remoteNativeRuntime{target: target, path: runtimePath}
			scope := testNativeRuntimeScope()
			scope.install = func(context.Context, SSHTarget, *runtimeartifact.LocalSet) (*remoteNativeRuntime, error) {
				return native, nil
			}
			removed := 0
			scope.remove = func(context.Context, *remoteNativeRuntime) error { removed++; return nil }
			ctx := context.WithValue(t.Context(), nativeRuntimeScopeKey{}, scope)
			if _, err := scope.ensure(ctx, target); err != nil {
				t.Fatal(err)
			}
			program, err := nativeWSLStageProgram(nativeWSLStageTestRuntime(runtimePath), "command", 0)
			if err != nil {
				t.Fatal(err)
			}
			// Cleanup runs through the POSIX stand-in transport above.
			program.runtime = native
			spool, err := newWSLStageSpoolWithProgram("exit 23", nil, nil, 0, sshCommandLimit{}, program)
			if err != nil {
				t.Fatal(err)
			}
			defer spool.close()
			oldStage, oldBuild, oldCleanup := stageWSLSpool, buildWSLStageLauncher, cleanupPublishedWSLStage
			t.Cleanup(func() {
				stageWSLSpool, buildWSLStageLauncher, cleanupPublishedWSLStage = oldStage, oldBuild, oldCleanup
			})
			nonce := strings.Repeat("a", 32)
			stageWSLSpool = func(*wslStageSpool, context.Context, *SSHTarget, wslStageTiming, string, string, io.Writer) (string, error) {
				return nonce, nil
			}
			buildWSLStageLauncher = func(string, int64, [32]byte, wslStageShell) string { return remotePOSIXControlCommand("exit 23") }
			windowsCleanups := 0
			cleanupPublishedWSLStage = func(ctx context.Context, _ SSHTarget, _ string, _ *wslStageSpool, budget, canceled time.Duration, _ string) error {
				windowsCleanups++
				deadline, ok := ctx.Deadline()
				if !ok || budget <= 0 || budget > wslStageCleanupTimeout || canceled != 0 || time.Until(deadline) > wslStageCleanupTimeout {
					t.Fatal("cleanup phases did not share a bounded deadline")
				}
				return nil
			}
			err = spool.run(ctx, &target, "2", "1", io.Discard, io.Discard)
			if exitCode(err) != 23 || windowsCleanups != 1 {
				t.Fatalf("original result changed: %v cleanups=%d", err, windowsCleanups)
			}
			data, readErr := os.ReadFile(argsPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			args := strings.Fields(string(data))
			if len(args) != 6 || args[3] != nonce || args[4] != "cleanup" {
				t.Fatalf("cleanup args=%q", args)
			}
			allowance, parseErr := strconv.Atoi(args[5])
			if parseErr != nil || allowance <= 0 || allowance > 10000 {
				t.Fatalf("cleanup allowance %q", args[5])
			}
			finishErr := scope.finish(ctx)
			if cleanupFails {
				if removed != 0 || finishErr == nil || !strings.Contains(err.Error(), runtimePath) {
					t.Fatalf("unconfirmed stage lost runtime: removed=%d err=%v finish=%v", removed, err, finishErr)
				}
			} else if removed != 1 || finishErr != nil {
				t.Fatalf("confirmed cleanup retained runtime: %d %v", removed, finishErr)
			}
		})
	}
}
