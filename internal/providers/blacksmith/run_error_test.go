package blacksmith

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestBlacksmithPrimaryCommandFailureSurvivesCleanup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   int
		cause  error
		status core.RunStatus
		kind   core.RunErrorKind
	}{
		{"transport", 255, errors.New("connection lost token=synthetic-private-token"), core.RunStatusFailed, core.RunErrorProvider},
		{"cancellation", -1, context.Canceled, core.RunStatusCanceled, core.RunErrorCanceled},
		{"cleanup-only", 0, nil, core.RunStatusFailed, core.RunErrorProvider},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			repo := t.TempDir()
			t.Chdir(repo)
			const id = "tbx_primary_failure"
			runs, stops := 0, 0
			runner := &blacksmithFuncRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				switch req.Args[1] {
				case "warmup":
					return core.LocalCommandResult{Stdout: id + "\n"}, nil
				case "run":
					runs++
					fmt.Fprintln(req.Stderr, "CRABBOX_PHASE:build")
					return core.LocalCommandResult{ExitCode: tc.code}, tc.cause
				case "stop":
					stops++
					return core.LocalCommandResult{ExitCode: -1}, context.DeadlineExceeded
				}
				return core.LocalCommandResult{}, nil
			}}
			cfg := core.BaseConfig()
			cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
			var stderr bytes.Buffer
			backend := newTestBlacksmithBackend(cfg, runner)
			backend.rt.Stderr = &stderr
			result, err := backend.Run(t.Context(), core.RunRequest{Repo: core.Repo{Root: repo}, Command: []string{"true"}, TimingJSON: true, EmitProof: "proof"})
			result = core.FinalizeRunResult(result, err)
			wantCode := tc.code
			if wantCode == 0 {
				wantCode = 1
			}
			if runs != 1 || stops != 1 || result.ExitCode != wantCode || core.ExitCodeForError(err, 0) != wantCode {
				t.Fatalf("runs=%d stops=%d result=%+v error=%v", runs, stops, result, err)
			}
			if result.Status != tc.status || result.ErrorKind != tc.kind {
				t.Errorf("classification=%s/%s want=%s/%s", result.Status, result.ErrorKind, tc.status, tc.kind)
			}
			if !errors.Is(err, context.DeadlineExceeded) || tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Errorf("typed primary or cleanup cause lost: %v", err)
			}
			if result.Session == nil || !result.Session.Kept {
				t.Fatal("unconfirmed cleanup lost retained session")
			}
			wantStage := "build"
			if tc.cause == nil {
				wantStage = "cleanup"
			}
			checkTiming := func(data []byte) {
				t.Helper()
				var report core.TimingReport
				if err := json.Unmarshal(data, &report); err != nil {
					t.Fatal(err)
				}
				if report.ExitCode != wantCode || report.RunStatus != tc.status || report.ErrorKind != tc.kind || report.BlockedStage != wantStage {
					t.Errorf("wrong timing: %+v", report)
				}
			}
			for _, line := range strings.Split(stderr.String(), "\n") {
				if strings.HasPrefix(line, "{") {
					checkTiming([]byte(line))
				}
			}
			data, readErr := os.ReadFile(filepath.Join(repo, ".crabbox", "runs", id, "timing.json"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			checkTiming(data)
			bundles, _ := filepath.Glob(filepath.Join(repo, ".crabbox", "captures", "*.tar.gz"))
			if len(bundles) != 1 {
				t.Fatalf("failure bundles=%v", bundles)
			}
			bundle := readBlacksmithArchive(t, bundles[0])
			checkTiming([]byte(bundle["crabbox-artifacts/timings.json"]))
			if tc.name == "transport" {
				for label, diagnostic := range map[string]string{"returned": err.Error(), "console": stderr.String(), "bundle": bundle["crabbox-artifacts/stderr.log"], "excerpt": result.LogExcerpt} {
					if !strings.Contains(diagnostic, "connection lost") || strings.Contains(diagnostic, "synthetic-private-token") {
						t.Errorf("%s lost safe primary diagnostic: %q", label, diagnostic)
					}
				}
			}
		})
	}
}

func TestBlacksmithRunDiagnosticRedactsBeforeBounding(t *testing.T) {
	cause := core.Exit(99, "connection lost %s token=%s tail", strings.Repeat("x", 4050), strings.Repeat("synthetic-private-token", 100))
	err := blacksmithRunDiagnostic(255, cause)
	var display core.ExitError
	if !errors.Is(err, cause) || !core.AsExitError(err, &display) || display.Code != 255 {
		t.Fatal("typed cause or selected code lost")
	}
	if strings.Contains(display.Message, "synthetic") || !strings.Contains(display.Message, "[redacted]") || len(display.Message) > 4096+len(" [truncated]") {
		t.Fatalf("redaction/bounding failed: %q", display.Message)
	}
}

func TestBlacksmithArtifactProbePreservesFailure(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			repo := t.TempDir()
			t.Chdir(repo)
			const id = "tbx_probe_failure"
			prepareBlacksmithGuestKey(t, id)
			testOwnedBlacksmithClaim(t, id, "probe-failure", repo)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cause := errors.New("capability connection lost token=synthetic-private-token")
			base := &blacksmithFuncRunner{}
			probes := 0
			runner := ownershipRunner(func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				if len(req.Args) >= 3 && req.Args[1] == "download" && req.Args[2] == "--help" {
					probes++
					if canceled {
						cancel()
					}
					return core.LocalCommandResult{ExitCode: 255}, cause
				}
				if len(req.Args) > 1 && (req.Args[1] == "run" || req.Args[1] == "stop") {
					t.Fatalf("probe failure reached workload or cleanup: %v", req.Args)
				}
				return base.Run(ctx, req)
			})
			var stderr bytes.Buffer
			backend := newTestBlacksmithBackend(core.BaseConfig(), runner)
			backend.rt.Stderr = &stderr
			result, err := backend.Run(ctx, core.RunRequest{ID: id, Repo: core.Repo{Root: repo}, Command: []string{"true"}, ArtifactGlobs: []string{"report"}, EmitProof: "proof"})
			result = core.FinalizeRunResult(result, err)
			wantStatus, wantKind := core.RunStatusFailed, core.RunErrorProvider
			if canceled {
				wantStatus, wantKind = core.RunStatusCanceled, core.RunErrorCanceled
			}
			var native blacksmithCommandError
			if probes != 1 || !errors.Is(err, cause) || !errors.As(err, &native) || canceled && !errors.Is(err, context.Canceled) {
				t.Errorf("probe cause lost: probes=%d err=%v", probes, err)
			}
			if result.ExitCode != 255 || result.Status != wantStatus || result.ErrorKind != wantKind {
				t.Errorf("unobserved probe outcome=%+v", result)
			}
			if !strings.Contains(result.LogExcerpt, "capability connection lost") || !strings.Contains(stderr.String(), "workload exit unconfirmed") || strings.Contains(result.LogExcerpt+stderr.String(), "synthetic-private-token") {
				t.Errorf("safe probe diagnostic missing: excerpt=%q stderr=%q", result.LogExcerpt, stderr.String())
			}
		})
	}
}

type blacksmithTimingFailWriter struct {
	bytes.Buffer
	calls int
}

func (w *blacksmithTimingFailWriter) WriteTimingReport(core.TimingReport) error {
	w.calls++
	return io.ErrClosedPipe
}

func TestBlacksmithTimingFailurePreservesRetainedSession(t *testing.T) {
	for _, mode := range []string{"cleanup-failure", "keep-on-failure", "reuse"} {
		t.Run(mode, func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			repo := t.TempDir()
			t.Chdir(repo)
			const id = "tbx_reporting_failure"
			cause := errors.New("native connection lost")
			var before core.LeaseClaim
			stops := 0
			runner := &blacksmithFuncRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				switch req.Args[1] {
				case "warmup":
					return core.LocalCommandResult{Stdout: id + "\n"}, nil
				case "run":
					var err error
					before, err = core.ReadLeaseClaim(id)
					if err != nil {
						t.Fatal(err)
					}
					return core.LocalCommandResult{ExitCode: 255}, cause
				case "stop":
					stops++
					return core.LocalCommandResult{ExitCode: -1}, context.DeadlineExceeded
				}
				return core.LocalCommandResult{}, nil
			}}
			cfg := core.BaseConfig()
			cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
			backend := newTestBlacksmithBackend(cfg, runner)
			backend.rt.Stderr = &blacksmithTimingFailWriter{}
			req := core.RunRequest{Repo: core.Repo{Root: repo}, Command: []string{"true"}, TimingJSON: true, KeepOnFailure: mode == "keep-on-failure"}
			if mode == "reuse" {
				prepareBlacksmithGuestKey(t, id)
				testOwnedBlacksmithClaim(t, id, "reporting-failure", repo)
				req.ID = id
			}
			result, err := backend.Run(t.Context(), req)
			if !errors.Is(err, cause) || !errors.Is(err, io.ErrClosedPipe) || result.ExitCode != 255 || result.ErrorKind != core.RunErrorProvider {
				t.Errorf("reporting replaced primary outcome: result=%+v err=%v", result, err)
			}
			if result.Session == nil || !result.Session.Kept || result.Session.Reused != (mode == "reuse") || result.Session.CleanupCommand != "crabbox stop --provider blacksmith-testbox "+id {
				t.Errorf("reporting lost custody: %+v", result.Session)
			}
			wantStops := 0
			if mode == "cleanup-failure" {
				wantStops = 1
			}
			after, claimErr := core.ReadLeaseClaim(id)
			if stops != wantStops || claimErr != nil || !reflect.DeepEqual(before, after) {
				t.Errorf("reporting changed claim or cleanup: stops=%d claimErr=%v", stops, claimErr)
			}
		})
	}
}

func TestBlacksmithConsoleFailureStillCapturesDiagnostic(t *testing.T) {
	for _, timing := range []bool{false, true} {
		t.Run(fmt.Sprint(timing), func(t *testing.T) {
			isolateBlacksmithOwnership(t)
			repo := t.TempDir()
			t.Chdir(repo)
			const id = "tbx_console_failure"
			prepareBlacksmithGuestKey(t, id)
			testOwnedBlacksmithClaim(t, id, "console-failure", repo)
			cause := errors.New("native connection lost token=synthetic-private-token")
			runner := &blacksmithFuncRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				if req.Args[1] != "run" {
					t.Fatalf("unexpected native operation: %v", req.Args)
				}
				return core.LocalCommandResult{ExitCode: 255}, cause
			}}
			backend := newTestBlacksmithBackend(core.BaseConfig(), runner)
			backend.rt.Stderr = bookkeepingFailWriter{}
			result, err := backend.Run(t.Context(), core.RunRequest{ID: id, Repo: core.Repo{Root: repo}, Command: []string{"true"}, EmitProof: "proof", TimingJSON: timing})
			if !errors.Is(err, cause) || !errors.Is(err, io.ErrClosedPipe) || result.ExitCode != 255 || result.ErrorKind != core.RunErrorProvider {
				t.Errorf("console failure lost cause or primary: result=%+v err=%v", result, err)
			}
			bundles, _ := filepath.Glob(filepath.Join(repo, ".crabbox", "captures", "*.tar.gz"))
			if len(bundles) != 1 {
				t.Fatalf("failure bundles=%v", bundles)
			}
			proof, readErr := os.ReadFile(filepath.Join(repo, ".crabbox", "runs", id, "blacksmith.stderr.log"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			bundle := readBlacksmithArchive(t, bundles[0])
			for label, diagnostic := range map[string]string{"proof": string(proof), "bundle": bundle["crabbox-artifacts/stderr.log"], "excerpt": result.LogExcerpt} {
				if !strings.Contains(diagnostic, "native connection lost") || strings.Contains(diagnostic, "synthetic-private-token") {
					t.Errorf("%s lost safe primary diagnostic: %q", label, diagnostic)
				}
			}
		})
	}
}

func TestBlacksmithFinalizationAttemptsIndependentDestinations(t *testing.T) {
	for _, tc := range []struct {
		failure   string
		code      int
		artifacts bool
	}{
		{"timing", 0, false}, {"timing", 0, true}, {"proof", 0, false},
		{"proof", 255, false}, {"bundle", 255, false}, {"proof+bundle", 255, false},
	} {
		t.Run(fmt.Sprintf("%s/%d/artifacts=%t", tc.failure, tc.code, tc.artifacts), func(t *testing.T) {
			if tc.artifacts {
				isolateArtifactOwnership(t)
				requireBlacksmithArtifactShell(t)
			} else {
				isolateBlacksmithOwnership(t)
			}
			repo := t.TempDir()
			t.Chdir(repo)
			const id = "tbx_finalization_failure"
			if err := os.MkdirAll(filepath.Join(repo, ".crabbox"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, destination := range []struct{ failure, path string }{{"proof", "runs"}, {"bundle", "captures"}} {
				if strings.Contains(tc.failure, destination.failure) {
					if err := os.WriteFile(filepath.Join(repo, ".crabbox", destination.path), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			var primary error
			if tc.code != 0 {
				primary = errors.New("native command transport failed")
			}
			stops := 0
			runner := &blacksmithFuncRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				switch req.Args[1] {
				case "warmup":
					return core.LocalCommandResult{Stdout: id + "\n"}, nil
				case "run":
					if tc.artifacts {
						return runSyntheticBlacksmithCommand(t, t.Context(), req)
					}
					return core.LocalCommandResult{ExitCode: tc.code}, primary
				case "stop":
					stops++
				}
				return core.LocalCommandResult{}, nil
			}}
			cfg := core.BaseConfig()
			cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
			backend := newTestBlacksmithBackend(cfg, runner)
			var timing blacksmithTimingFailWriter
			if tc.failure == "timing" {
				backend.rt.Stderr = &timing
			}
			req := core.RunRequest{Repo: core.Repo{Root: repo}, Command: []string{"true"}, EmitProof: "proof", TimingJSON: true, KeepOnFailure: tc.code == 0}
			if tc.artifacts {
				testWriteBlacksmithFile(t, repo, "report", "synthetic")
				req.ArtifactGlobs = []string{"report"}
			}
			result, err := backend.Run(t.Context(), req)
			if tc.artifacts {
				assertArtifactTransferCalls(t, runner.calls, 1)
			}
			wantCode := tc.code
			if wantCode == 0 {
				wantCode = 2
				if tc.failure == "timing" {
					wantCode = 1
				}
			}
			if err == nil || result.ExitCode != wantCode || core.ExitCodeForError(err, 0) != wantCode || result.ErrorKind != core.RunErrorProvider || primary != nil && !errors.Is(err, primary) {
				t.Errorf("reporting replaced/lost primary outcome: result=%+v err=%v", result, err)
			}
			if tc.failure == "timing" && (!errors.Is(err, io.ErrClosedPipe) || timing.calls != 1) {
				t.Errorf("timing failure/call count lost: calls=%d err=%v", timing.calls, err)
			}
			for _, failure := range []string{"proof", "bundle"} {
				if strings.Contains(tc.failure, failure) && (err == nil || !strings.Contains(err.Error(), failure)) {
					t.Errorf("%s failure omitted: %v", failure, err)
				}
			}
			if stops != 1 || result.Session == nil || result.Session.Kept || result.Session.Reused {
				t.Errorf("late error changed settled custody: stops=%d session=%+v", stops, result.Session)
			}
			if _, exists, claimErr := core.ReadLeaseClaimWithPresence(id); claimErr != nil || exists {
				t.Errorf("successful cleanup retained a claim: exists=%t err=%v", exists, claimErr)
			}
			if !strings.Contains(tc.failure, "proof") {
				data, readErr := os.ReadFile(filepath.Join(repo, ".crabbox", "runs", id, "timing.json"))
				if readErr != nil {
					t.Errorf("independent proof missing: %v", readErr)
				} else {
					var report core.TimingReport
					if json.Unmarshal(data, &report) != nil || report.ExitCode != wantCode || report.RunStatus != core.RunStatusFailed || report.ErrorKind != core.RunErrorProvider {
						t.Errorf("persistent proof missed known failure: %s", data)
					}
				}
			}
			if !strings.Contains(tc.failure, "bundle") {
				bundles, _ := filepath.Glob(filepath.Join(repo, ".crabbox", "captures", "*.tar.gz"))
				if len(bundles) != 1 {
					t.Errorf("independent failure bundle missing: %v", bundles)
				}
			}
		})
	}
}

func TestBlacksmithProofFailurePreservesSuccessfulReusedSession(t *testing.T) {
	isolateBlacksmithOwnership(t)
	repo := t.TempDir()
	t.Chdir(repo)
	const id = "tbx_proof_failure"
	prepareBlacksmithGuestKey(t, id)
	testOwnedBlacksmithClaim(t, id, "proof-failure", repo)
	var before core.LeaseClaim
	// The workload succeeds; only persistent proof reporting fails.
	if err := os.MkdirAll(filepath.Join(repo, ".crabbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".crabbox", "runs"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &blacksmithFuncRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if req.Args[1] != "run" {
			t.Fatalf("reporting failure reached unexpected operation: %v", req.Args)
		}
		var err error
		before, err = core.ReadLeaseClaim(id)
		if err != nil {
			t.Fatal(err)
		}
		return core.LocalCommandResult{}, nil
	}}
	result, err := newTestBlacksmithBackend(core.BaseConfig(), runner).Run(t.Context(), core.RunRequest{ID: id, Repo: core.Repo{Root: repo}, Command: []string{"true"}, EmitProof: "proof"})
	if err == nil || result.ExitCode != 2 || result.ErrorKind != core.RunErrorProvider || result.Session == nil || !result.Session.Kept || !result.Session.Reused || result.Session.CleanupCommand != "crabbox stop --provider blacksmith-testbox "+id {
		t.Fatalf("proof reporting lost outcome/custody: result=%+v err=%v", result, err)
	}
	after, claimErr := core.ReadLeaseClaim(id)
	if claimErr != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("proof reporting changed claim: %v", claimErr)
	}
}

func BenchmarkBlacksmithRunDiagnostic(b *testing.B) {
	for _, size := range []int{64, 8192} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			cause := fmt.Errorf("connection lost token=synthetic-private-token %s", strings.Repeat("x", size))
			b.ReportAllocs()
			for b.Loop() {
				_ = blacksmithRunDiagnostic(255, cause)
			}
		})
	}
}
