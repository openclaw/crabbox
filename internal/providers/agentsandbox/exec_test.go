package agentsandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestExecContextHonorsZeroAsNoDeadline(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.ExecTimeoutSecs = 0
	b := backend{cfg: cfg}

	ctx, cancel := b.execContext(context.Background())
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("zero exec timeout created a deadline")
	}

	b.cfg.AgentSandbox.ExecTimeoutSecs = 1
	ctx, cancel = b.execContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("positive exec timeout did not create a deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > time.Second {
		t.Fatalf("deadline remaining=%s, want within one second", remaining)
	}
}

func TestRunLiteralArgumentsSurviveNativeStdinTransport(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("POSIX stdin transport")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	for _, literalExecutable := range []bool{false, true} {
		t.Run(fmt.Sprint(literalExecutable), func(t *testing.T) {
			cfg := testAgentSandboxConfig(t)
			fake := readyFakeClient(cfg)
			b := testBackend(cfg, fake, nil, nil)
			ready, err := sandboxReadinessOnce(t.Context(), fake, cfg.AgentSandbox.Namespace, "claim-a", fakeClaimIdentity(cfg))
			if err != nil {
				t.Fatal(err)
			}
			workdir := t.TempDir()
			marker := filepath.Join(workdir, "must-not-exist")
			command := []string{"printf", "%s", ";", "touch", marker}
			literal := map[int]bool{2: true}
			want := ";touch" + marker
			wantCode := 0
			if literalExecutable {
				program := filepath.Join(workdir, "FOO=x")
				if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf 'literal:%s' \"$*\"\nexit 42\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				command = []string{"FOO=x", "argument"}
				literal = map[int]bool{0: true}
				want = "literal:argument"
				wantCode = 42
			}
			_, err = b.runCommand(t.Context(), fake, ready, RunRequest{Command: command, CommandLiteralArgs: literal}, workdir)
			if err != nil {
				t.Fatal(err)
			}
			if len(fake.execInput) != 1 {
				t.Fatalf("exec inputs=%d", len(fake.execInput))
			}
			cmd := exec.Command(sh, "-s")
			cmd.Stdin = strings.NewReader(string(fake.execInput[0]))
			cmd.Env = []string{"HOME=" + workdir, "PATH=" + workdir + ":/usr/bin:/bin", "ENV=" + os.DevNull}
			out, runErr := cmd.CombinedOutput()
			code := 0
			if runErr != nil {
				var exitErr *exec.ExitError
				if !errors.As(runErr, &exitErr) {
					t.Fatal(runErr)
				}
				code = exitErr.ExitCode()
			}
			if string(out) != want || code != wantCode {
				t.Fatalf("script=%q output=%q code=%d want=%q/%d", fake.execInput[0], out, code, want, wantCode)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("literal created marker: %v", err)
			}
		})
	}
}
