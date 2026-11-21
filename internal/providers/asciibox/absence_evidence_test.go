package asciibox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestBoxNativeExitHelper(t *testing.T) {
	if os.Getenv("CRABBOX_TEST_ASCII_EXIT") == "1" {
		os.Exit(1)
	}
}

func TestBoxAbsenceCommandUsesCorePolicy(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%t", force), func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			t.Chdir(t.TempDir())
			_, f, claim, _ := ownedFixture(t)
			f.getHook = func(string) (boxData, error) { return boxData{}, &boxNotFoundError{id: claim.CloudID} }
			f.listHook = func() ([]boxData, error) { return []boxData{}, nil }
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(configPath, []byte("provider: ascii-box\nasciiBox:\n  baseUrl: https://ascii.dev\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			for _, env := range []string{"CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_MODE", "CRABBOX_POND", "CRABBOX_TAILSCALE"} {
				t.Setenv(env, "")
			}
			args := []string{"stop", "--provider", "boat", "--id", claim.LeaseID}
			if force {
				args = append(args, "--force")
			}
			var output bytes.Buffer
			err := (core.App{Stdout: &output, Stderr: &output}).Run(t.Context(), args)
			if err != nil || !strings.Contains(output.String(), "forgotten locally (resource absent)") || strings.Contains(output.String(), "released") {
				t.Fatalf("err=%v output=%s", err, output.String())
			}
			if len(f.deletedIDs) != 0 || len(f.prepareIDs) != 0 {
				t.Fatalf("absence caused native mutation: deletes=%v prepare=%v", f.deletedIDs, f.prepareIDs)
			}
		})
	}
}

func TestBoxAbsenceVerifierRejectsChangedScopeAndID(t *testing.T) {
	for _, wrongScope := range []bool{false, true} {
		t.Run(fmt.Sprintf("scope=%t", wrongScope), func(t *testing.T) {
			b, f, claim, _ := ownedFixture(t)
			f.getHook = func(string) (boxData, error) { return boxData{}, &boxNotFoundError{id: "bx_wrong"} }
			f.listHook = func() ([]boxData, error) { t.Fatal("invalid binding reached inventory"); return nil, nil }
			if wrongScope {
				t.Setenv("BOX_ORG", "other-org")
				f.getHook = func(string) (boxData, error) { t.Fatal("wrong scope reached native lookup"); return boxData{}, nil }
			}
			if forgotten, err := core.ForgetAbsentLeaseClaim(t.Context(), b, claim); err == nil || forgotten {
				t.Fatalf("accepted conflicting evidence: forgotten=%t err=%v", forgotten, err)
			}
			assertClaimRetained(t, claim)
		})
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
		{"duplicate inventory fields", `{"status":404}`, `{"boxes":[{"id":"bx_1"}],"boxes":[]}`, nativeExit, false},
		{"duplicate not-found status", `{"status":403,"status":404}`, `{"boxes":[]}`, nativeExit, false},
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

func TestNativeBoxNotFoundPrefersJSONResponse(t *testing.T) {
	for _, test := range []struct {
		name, stdout, stderr string
		absent               bool
	}{
		{"rename notice", `{"status":404,"error":"not found"}`, "\x1b[38;5;245mBox is now Boat. This `box` binary keeps working but no longer updates itself\x1b[0m", true},
		{"authoritative non-404", `{"status":403}`, "box not found (404)", false},
		{"duplicate response", `{"status":403,"status":404}`, "box not found (404)", false},
		{"malformed response", `{"status":404`, "box not found (404)", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := nativeBoxNotFound(core.LocalCommandResult{Stdout: test.stdout, Stderr: test.stderr}); got != test.absent {
				t.Fatalf("nativeBoxNotFound=%t want %t", got, test.absent)
			}
		})
	}
}

func TestFixedBoxReleaseReconcilesDisappearanceDuringStop(t *testing.T) {
	nativeExit := boxNativeExit(t)
	for _, test := range []struct {
		name, inventory string
		info            commandOutcome
		inventoryErr    error
		cancelOnAction  string
		wantReleased    bool
	}{
		{name: "complete absence", inventory: `{"sandboxes":[]}`, wantReleased: true},
		{name: "incomplete inventory", inventory: `{"sandboxes":[],"pageInfo":{"hasMore":true}}`},
		{name: "inventory unavailable", inventoryErr: errors.New("inventory unavailable")},
		{name: "still in inventory", inventory: `{"sandboxes":[{"id":"bx_1","createdAt":"2026-08-30T12:00:00Z"}]}`},
		{name: "changed identity", inventory: `{"sandboxes":[{"id":"bx_1","createdAt":"2026-08-31T12:00:00Z"}]}`},
		{name: "lookup denied", info: commandOutcome{result: core.LocalCommandResult{ExitCode: 1, Stdout: `{"status":403}`}, err: nativeExit}},
		{name: "cancelled during confirmation", inventory: `{"sandboxes":[]}`, cancelOnAction: "list"},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, _, req, _ := fixedBoxFixture(t)
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			present := commandOutcome{result: core.LocalCommandResult{Stdout: `{"sandbox":{"id":"bx_1","createdAt":"2026-08-30T12:00:00Z"}}`}}
			missing := test.info
			if missing.err == nil {
				missing = commandOutcome{result: core.LocalCommandResult{ExitCode: 1, Stdout: `{"status":404}`}, err: nativeExit}
			}
			runner := &releaseCommandRunner{configPath: filepath.Join(t.TempDir(), "config.json"), outcomes: map[string][]commandOutcome{
				"info": {present, present, present, present, present, present},
				"stop": {{result: core.LocalCommandResult{}}},
				"list": {{result: core.LocalCommandResult{Stdout: test.inventory}, err: test.inventoryErr}},
			}}
			runner.onAction = func(action string) {
				if action == "stop" {
					runner.outcomes["info"] = []commandOutcome{missing, missing}
				}
				if action == test.cancelOnAction {
					cancel()
				}
			}
			withFakeAPI(t, &client{apiKey: "box_test", apiURL: "https://ascii.dev", cliPath: "box", home: t.TempDir(), runner: runner})
			outcome, err := b.ReleaseLeaseWithOutcome(ctx, core.ReleaseLeaseRequest{Lease: lease})
			if (err == nil) != test.wantReleased || outcome.Terminal != test.wantReleased {
				t.Fatalf("release err=%v terminal=%t, want released=%t", err, outcome.Terminal, test.wantReleased)
			}
			claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || (claim.FixedCreateIntent.State == "released") != test.wantReleased {
				t.Fatalf("claim state=%q err=%v", claim.FixedCreateIntent.State, err)
			}
			if claim.FixedCreateIntent.Attempt["deletion_operation_id"] != "" || claim.FixedCreateIntent.Attempt["deletion_completed"] != "" {
				t.Fatal("disappearance invented a native deletion receipt")
			}
			stops := 0
			for _, command := range runner.commands {
				if strings.Contains(command, " stop ") {
					stops++
				}
				if strings.Contains(command, " delete ") || strings.Contains(command, " extend ") {
					t.Fatalf("disappearance caused another mutation: %s", command)
				}
			}
			if stops != 1 {
				t.Fatalf("stop count=%d, want 1", stops)
			}
		})
	}
}

func TestFixedBoxReleaseRejectsAnotherBoxNotFound(t *testing.T) {
	b, f, req, _ := fixedBoxFixture(t)
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	f.releaseHook = func(string) error { return &boxNotFoundError{id: "bx_other"} }
	f.listHook = func() ([]boxData, error) { t.Fatal("wrong ID reached absence inventory"); return nil, nil }
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("another sandbox's 404 released the claim")
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent.State == "released" {
		t.Fatalf("claim state=%q err=%v", claim.FixedCreateIntent.State, err)
	}
}
