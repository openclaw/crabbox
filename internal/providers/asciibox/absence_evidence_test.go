package asciibox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestBoxNativeExitHelper(t *testing.T) {
	if os.Getenv("CRABBOX_TEST_ASCII_EXIT") == "1" {
		os.Exit(1)
	}
}

func boxNativeExit(t *testing.T) error {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestBoxNativeExitHelper$")
	cmd.Env = append(os.Environ(), "CRABBOX_TEST_ASCII_EXIT=1")
	err = cmd.Run()
	if !core.IsPlainLocalCommandExit(core.LocalCommandResult{ExitCode: 1}, err) {
		t.Fatalf("expected native exit: %v", err)
	}
	return err
}

func TestReleaseNativeAbsenceEvidence(t *testing.T) {
	nativeExit := boxNativeExit(t)
	for _, test := range []struct {
		name      string
		info      string
		inventory string
		err       error
		absent    bool
	}{
		{"exact JSON 404", `{"status":404,"error":"not found"}`, `{"sandboxes":[]}`, nativeExit, true},
		{"legacy exact 404", "box not found (404)", `{"boxes":[]}`, nativeExit, true},
		{"partial inventory", `{"status":404}`, `{"boxes":[],"pageInfo":{"hasMore":true}}`, nativeExit, false},
		{"malformed inventory", `{"status":404}`, `{}`, nativeExit, false},
		{"observable ID", `{"status":404}`, `{"boxes":[{"id":"bx_1","createdAt":"2026-08-30T12:00:00Z"}]}`, nativeExit, false},
		{"replacement ID", `{"status":404}`, `{"boxes":[{"id":"bx_1","createdAt":"2026-08-31T12:00:00Z"}]}`, nativeExit, false},
		{"auth failure mentioning ID", "permission denied for bx_404", `{"boxes":[]}`, nativeExit, false},
		{"missing executable", "box: command not found", `{"boxes":[]}`, nativeExit, false},
		{"failed capture with valid 404", `{"status":404}`, `{"boxes":[]}`, errors.New("captured command output exceeded limit"), false},
		{"failed transport with valid 404", `{"status":404}`, `{"boxes":[]}`, errors.Join(nativeExit, errors.New("pipe failure")), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, _, claim, lease := ownedFixture(t)
			runner := &releaseCommandRunner{configPath: filepath.Join(t.TempDir(), "config.json"), outcomes: map[string][]commandOutcome{
				"info": {{result: core.LocalCommandResult{ExitCode: 1, Stderr: test.info}, err: test.err}},
				"list": {{result: core.LocalCommandResult{Stdout: test.inventory}}},
			}}
			withFakeAPI(t, &client{apiKey: "box_test", apiURL: "https://ascii.dev", cliPath: "box", home: t.TempDir(), runner: runner})
			teardown := false
			err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease, GuardedRemoteCleanup: func(context.Context, core.LeaseTarget) { teardown = true }})
			if (err == nil) != test.absent {
				t.Fatalf("release error=%v, want absent=%t", err, test.absent)
			}
			if teardown {
				t.Fatal("absence reconciliation attempted remote teardown")
			}
			if test.absent {
				if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
					t.Fatalf("claim remains: exists=%t err=%v", exists, err)
				}
			} else {
				assertClaimRetained(t, claim)
			}
			for _, command := range runner.commands {
				for _, mutation := range []string{" stop ", " delete ", " ssh ", " new "} {
					if strings.Contains(command, mutation) {
						t.Fatalf("absence reconciliation issued mutation: %s", command)
					}
				}
			}
		})
	}
}
