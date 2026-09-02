package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeRunFailureEvidenceBackend struct {
	begin func(context.Context, RunFailureEvidenceRequest) (RunFailureEvidenceCollector, error)
}

func (b fakeRunFailureEvidenceBackend) Spec() ProviderSpec {
	return ProviderSpec{Name: "evidence-test"}
}
func (b fakeRunFailureEvidenceBackend) Acquire(context.Context, AcquireRequest) (LeaseTarget, error) {
	return LeaseTarget{}, nil
}
func (b fakeRunFailureEvidenceBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	return LeaseTarget{}, nil
}
func (b fakeRunFailureEvidenceBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}
func (b fakeRunFailureEvidenceBackend) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	return nil
}
func (b fakeRunFailureEvidenceBackend) Touch(context.Context, TouchRequest) (Server, error) {
	return Server{}, nil
}
func (b fakeRunFailureEvidenceBackend) BeginRunFailureEvidence(ctx context.Context, req RunFailureEvidenceRequest) (RunFailureEvidenceCollector, error) {
	return b.begin(ctx, req)
}

func TestClassifyRunFailureStages(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		want  string
		retry string
	}{
		{name: "ssh", text: "timed out waiting for SSH on 127.0.0.1 during before command", want: "ssh", retry: "true"},
		{name: "provider auth", text: "<!doctype html><html><title>Cloudflare Access</title><body>login</body></html>", want: "provider_auth", retry: "false"},
		{name: "install", text: "pnpm install failed with ENOMEM", want: "install", retry: "unknown"},
		{name: "model", text: "model call failed: context window maximum tokens exceeded", want: "model_call", retry: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyRunFailure(1, tt.text, nil)
			if got.BlockedStage != tt.want || got.RetryLikely != tt.retry {
				t.Fatalf("ClassifyRunFailure()=%#v, want stage=%q retry=%q", got, tt.want, tt.retry)
			}
		})
	}
}

func TestClassifyRunFailureUsesNormalizedMemoryEvidence(t *testing.T) {
	got := ClassifyRunFailureWithEvidence(255, "connection reset by peer", []TimingPhase{{Name: "test"}}, RunFailureEvidence{
		ResourceExhaustion: ResourceExhaustionMemory,
	})
	if got.BlockedStage != "resource_exhaustion" || got.ResourceExhaustion != ResourceExhaustionMemory || got.RetryLikely != "false" {
		t.Fatalf("ClassifyRunFailureWithEvidence()=%#v", got)
	}
	fields := FormatFailureClassificationFields(got)
	for _, want := range []string{"blocked_stage=resource_exhaustion", "resource_exhaustion=memory", "retry_likely=false"} {
		if !strings.Contains(fields, want) {
			t.Fatalf("classification fields missing %q: %s", want, fields)
		}
	}
}

func TestRunOutcomeFailureKeepsMemoryEvidenceAcrossSecondaryFailures(t *testing.T) {
	got := classifyRunOutcomeFailure(0, "", nil, RunFailureEvidence{
		ResourceExhaustion: ResourceExhaustionMemory,
	}, true)
	if got.BlockedStage != "resource_exhaustion" || got.ResourceExhaustion != ResourceExhaustionMemory || got.RetryLikely != "false" {
		t.Fatalf("classifyRunOutcomeFailure()=%#v, want memory exhaustion", got)
	}

	got = classifyRunOutcomeFailure(0, "", nil, RunFailureEvidence{}, true)
	if got.BlockedStage != "test" || got.ResourceExhaustion != "" || got.RetryLikely != "false" {
		t.Fatalf("classifyRunOutcomeFailure()=%#v, want test failure", got)
	}
}

func TestRunFailureEvidenceErrorsAreWarnings(t *testing.T) {
	t.Run("baseline", func(t *testing.T) {
		var warnings bytes.Buffer
		backend := fakeRunFailureEvidenceBackend{begin: func(context.Context, RunFailureEvidenceRequest) (RunFailureEvidenceCollector, error) {
			return nil, errors.New("baseline read failed")
		}}
		if collector := beginRunFailureEvidence(context.Background(), backend, LeaseTarget{}, &warnings); collector != nil {
			t.Fatal("baseline error returned a collector")
		}
		if !strings.Contains(warnings.String(), "warning: failed-run evidence baseline unavailable") {
			t.Fatalf("missing baseline warning: %s", warnings.String())
		}
	})

	t.Run("collection", func(t *testing.T) {
		var warnings bytes.Buffer
		collector := RunFailureEvidenceCollector(func(context.Context) (RunFailureEvidence, error) {
			return RunFailureEvidence{}, errors.New("post-failure read failed")
		})
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if evidence := collectRunFailureEvidence(canceled, collector, &warnings); evidence.ResourceExhaustion != "" {
			t.Fatalf("evidence=%#v, want empty", evidence)
		}
		if !strings.Contains(warnings.String(), "warning: failed-run evidence collection incomplete") {
			t.Fatalf("missing collection warning: %s", warnings.String())
		}
	})
}

func TestClassifyRunFailureBlacksmithInfraStages(t *testing.T) {
	for _, tt := range []struct {
		name  string
		text  string
		stage string
	}{
		{
			name:  "shutdown dns",
			text:  "warning: blacksmith stop failed for tbx_1: request failed: Post https://backend.blacksmith.sh/api/shutdown: dial tcp: lookup backend.blacksmith.sh: i/o timeout",
			stage: "cleanup",
		},
		{
			name:  "sync guard",
			text:  "Blacksmith Testbox sync did not print a completion marker for 10m0s; terminating local runner.",
			stage: "sync",
		},
		{
			name:  "actions cancelled",
			text:  "Testbox ready\nGitHub Actions run cancelled",
			stage: "actions_cancelled",
		},
		{
			name:  "stalled after ready",
			text:  "Blacksmith Testbox ready\nBlacksmith post-ready stall: no output after ready",
			stage: "testbox_stalled_after_ready",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyRunFailure(255, tt.text, nil)
			if got.BlockedStage != tt.stage || got.RetryLikely != "true" {
				t.Fatalf("ClassifyRunFailure()=%#v, want stage=%q retry=true", got, tt.stage)
			}
		})
	}
}

func TestClassifyRunFailureDoesNotTreatUserTimeoutsAsBlacksmithInfra(t *testing.T) {
	got := ClassifyRunFailure(1, "Testbox ready\nFAIL ui.spec.ts\nError: Test timeout of 30000ms exceeded", nil)
	if got.BlockedStage != "unknown" {
		t.Fatalf("ClassifyRunFailure()=%#v, want unknown", got)
	}
}

func TestClassifyRunFailureDoesNotTreatUserCancellationAsBlacksmithInfra(t *testing.T) {
	got := ClassifyRunFailure(1, "Testbox ready\nFAIL queue.test.ts\nexpected canceled job state", nil)
	if got.BlockedStage != "unknown" {
		t.Fatalf("ClassifyRunFailure()=%#v, want unknown", got)
	}
}

func TestClassifyRunFailureUsesFinalPhaseAfterErrorSignatures(t *testing.T) {
	got := ClassifyRunFailure(1, "pnpm install completed\nunit tests failed", []TimingPhase{
		{Name: "install"},
		{Name: "test"},
	})
	if got.BlockedStage != "unknown" {
		t.Fatalf("ClassifyRunFailure()=%#v, want unknown", got)
	}
	got = ClassifyRunFailure(1, "test failed", []TimingPhase{
		{Name: "install"},
		{Name: "test"},
	})
	if got.BlockedStage != "unknown" {
		t.Fatalf("ClassifyRunFailure()=%#v, want unknown", got)
	}
	got = ClassifyRunFailure(1, "exit status 1", []TimingPhase{
		{Name: "test"},
		{Name: "install"},
	})
	if got.BlockedStage != "install" {
		t.Fatalf("ClassifyRunFailure()=%#v, want install", got)
	}
}

func TestClassifyRunFailureDoesNotTreatApplicationConnectionErrorsAsSSH(t *testing.T) {
	got := ClassifyRunFailure(1, "dial tcp 127.0.0.1:5432: connection refused", nil)
	if got.BlockedStage != "unknown" {
		t.Fatalf("ClassifyRunFailure()=%#v, want unknown", got)
	}
}

func TestClassifyRunFailureDoesNotTreatApplicationAuthFailuresAsProviderAuth(t *testing.T) {
	got := ClassifyRunFailure(1, "expected 200, got 401 Unauthorized", nil)
	if got.BlockedStage != "unknown" {
		t.Fatalf("ClassifyRunFailure()=%#v, want unknown", got)
	}
}

func TestClassifyRunFailureDoesNotTreatApplicationProviderAuthTextAsProviderAuth(t *testing.T) {
	for _, text := range []string{
		"FAIL src/provider-auth.test.ts\nexpected area provider_auth to be rendered",
		"normal test failure in provider auth settings panel",
	} {
		got := ClassifyRunFailure(1, text, nil)
		if got.BlockedStage != "unknown" {
			t.Fatalf("ClassifyRunFailure(%q)=%#v, want unknown", text, got)
		}
	}
}

func TestFormatRunSummaryIncludesFailureClassification(t *testing.T) {
	got := formatRunSummary(runTimings{
		sync:         time.Second,
		command:      2 * time.Second,
		blockedStage: "install",
		retryLikely:  "unknown",
	}, 3*time.Second, 1)
	for _, want := range []string{"blocked_stage=install", "retry_likely=unknown"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q in %q", want, got)
		}
	}
}

func TestTimingJSONIncludesFailureClassification(t *testing.T) {
	report := timingReportFromRun("aws", "cbx_123", "slug", runTimings{
		blockedStage:       "resource_exhaustion",
		resourceExhaustion: ResourceExhaustionMemory,
		retryLikely:        "false",
	}, time.Second, 1)
	var buf bytes.Buffer
	if err := writeTimingJSON(&buf, report); err != nil {
		t.Fatal(err)
	}
	var got TimingReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.BlockedStage != "resource_exhaustion" || got.ResourceExhaustion != ResourceExhaustionMemory || got.RetryLikely != "false" {
		t.Fatalf("classification not encoded: %#v", got)
	}
}

func TestPrintRunFailureDigestIncludesMemoryGuidance(t *testing.T) {
	var buf bytes.Buffer
	printRunFailureDigest(&buf, runFailureDigestInput{
		LeaseID:        "cbx_123",
		CommandDisplay: "go test ./...",
		Classification: FailureClassification{
			BlockedStage:       "resource_exhaustion",
			ResourceExhaustion: ResourceExhaustionMemory,
			RetryLikely:        "false",
		},
	})
	out := buf.String()
	for _, want := range []string{
		"area: resource_exhaustion",
		"retryable: false",
		"resource_exhaustion: memory",
		"hint: reduce memory demand and inspect active limits and runtime capacity before retrying",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("digest missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "next: crabbox run") {
		t.Fatalf("deterministic memory exhaustion should not suggest an unchanged retry:\n%s", out)
	}
}

func TestRunFailureEvidencePresentationSnapshot(t *testing.T) {
	var evidence RunFailureEvidence
	if err := json.Unmarshal([]byte(`{"resourceExhaustion":"memory","hint":"Check the selected runtime capacity","details":{"runtime_memory_total_bytes":"8388608","bad":"\u001b[2J","bad\nkey":"value"}}`), &evidence); err != nil {
		t.Fatal(err)
	}
	got := collectRunFailureEvidence(t.Context(), func(context.Context) (RunFailureEvidence, error) { return evidence, nil }, nil)
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "8388608") || strings.Contains(string(encoded), "bad") || !strings.Contains(string(encoded), "Check the selected") {
		t.Fatalf("missing or unsafe presentation snapshot: %s", encoded)
	}
	var input runFailureDigestInput
	if err := json.Unmarshal([]byte(`{"Evidence":`+string(encoded)+`}`), &input); err != nil {
		t.Fatal(err)
	}
	input.Classification = ClassifyRunFailureWithEvidence(137, "", nil, got)
	var out bytes.Buffer
	printRunFailureDigest(&out, input)
	if !strings.Contains(out.String(), "hint: Check the selected runtime capacity") || !strings.Contains(out.String(), "runtime_memory_total_bytes=8388608") {
		t.Fatalf("digest lost evidence: %s", out.String())
	}
	var report TimingReport
	if err := json.Unmarshal([]byte(`{"exitCode":137,"failureEvidence":`+string(encoded)+`}`), &report); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := writeTimingJSON(&out, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"failureEvidence"`) || !strings.Contains(out.String(), "8388608") {
		t.Fatalf("timing lost evidence: %s", out.String())
	}
}

func TestTimingReportFromRunFailureEvidenceSnapshot(t *testing.T) {
	timings := runTimings{
		blockedStage:       "resource_exhaustion",
		resourceExhaustion: ResourceExhaustionMemory,
		retryLikely:        "false",
		failureEvidence: RunFailureEvidence{
			ResourceExhaustion: ResourceExhaustionMemory,
			Hint:               "Check active limits and runtime capacity",
			Details:            map[string]string{"memory_limit_bytes": "1024", "unsafe": "\x1b[2J"},
		},
	}
	report := timingReportFromRun("evidence-test", "cbx_test", "test", timings, time.Second, 137)
	if report.FailureEvidence == nil || report.FailureEvidence.Hint != timings.failureEvidence.Hint ||
		!reflect.DeepEqual(report.FailureEvidence.Details, map[string]string{"memory_limit_bytes": "1024"}) {
		t.Fatalf("constructor lost or failed to sanitize failure evidence: %+v", report.FailureEvidence)
	}
	if report.ExitCode != 137 || report.ResourceExhaustion != ResourceExhaustionMemory || report.BlockedStage != "resource_exhaustion" || report.RetryLikely != "false" {
		t.Fatalf("constructor changed failure classification: %+v", report)
	}
	timings.failureEvidence.Details["memory_limit_bytes"] = "2048"
	if report.FailureEvidence.Details["memory_limit_bytes"] != "1024" {
		t.Fatal("report aliases the timing input's evidence map")
	}
	rebuilt := timingReportFromRunWithActionsURL("evidence-test", "cbx_test", "test", timings, time.Second, 137, "")
	if rebuilt.FailureEvidence == nil || rebuilt.FailureEvidence.Details["memory_limit_bytes"] != "2048" {
		t.Fatal("reconstructed report did not project the timing input")
	}
	rebuilt.FailureEvidence.Details["memory_limit_bytes"] = "4096"
	if timings.failureEvidence.Details["memory_limit_bytes"] != "2048" || report.FailureEvidence.Details["memory_limit_bytes"] != "1024" {
		t.Fatal("reconstructed snapshot aliases its input or a prior report")
	}
}

func TestRunFailureEvidencePresentationBounds(t *testing.T) {
	for _, hint := range []string{"unsafe\x1b[2J", "bad\nline", "bad\u202e", string([]byte{0xff}), strings.Repeat("a", 769)} {
		evidence := RunFailureEvidence{ResourceExhaustion: ResourceExhaustionMemory, Hint: hint, Details: map[string]string{
			"good": "value", "bad/key": "value", "oversized": strings.Repeat("b", 257),
		}}
		got := collectRunFailureEvidence(t.Context(), func(context.Context) (RunFailureEvidence, error) { return evidence, nil }, nil)
		if got.ResourceExhaustion != ResourceExhaustionMemory || got.Hint != "" || !reflect.DeepEqual(got.Details, map[string]string{"good": "value"}) {
			t.Fatalf("optional fields affected classification or bounds: %+v", got)
		}
		evidence.Details["good"] = "mutated"
		if got.Details["good"] != "value" {
			t.Fatal("evidence was not cloned")
		}
	}
	details := map[string]string{}
	for i := range 30 {
		details[fmt.Sprintf("key_%02d", i)] = strings.Repeat("v", 256)
	}
	got := sanitizeRunFailureEvidence(RunFailureEvidence{Hint: strings.Repeat("h", 768), Details: details})
	size := len(got.Hint)
	for key, value := range got.Details {
		size += len(key) + len(value)
	}
	if size > 4096 || len(got.Details) > 16 || len(got.Details) == 0 {
		t.Fatalf("unbounded snapshot size=%d entries=%d", size, len(got.Details))
	}
}

func TestRunFailureEvidenceFinalization(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoRoot)
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep=%t", keep), func(t *testing.T) {
			lease, _ := setupRunClaimSnapshotTest(t)
			pathDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
			t.Setenv("PATH", pathDir+string(os.PathListSeparator)+"/usr/bin:/bin")
			installWorkspaceOwnerAwareSSH(t, filepath.Join(pathDir, "ssh"), "#!/bin/sh\ncase \"$1\" in *fixture-memory-command*) exit 137 ;; *'tar -cz'*) exit 1 ;; esac\nexit 0\n")
			begins, collects, releases := 0, 0, 0
			source := map[string]string{"runtime_memory_total_bytes": "8388608"}
			runEnvProfileTestEvidenceHook = func(context.Context, RunFailureEvidenceRequest) (RunFailureEvidenceCollector, error) {
				begins++
				return func(context.Context) (RunFailureEvidence, error) {
					collects++
					return RunFailureEvidence{ResourceExhaustion: ResourceExhaustionMemory, Hint: "Check runtime capacity and swap", Details: source}, nil
				}, nil
			}
			runEnvProfileTestReleaseRequestHook = func(req ReleaseLeaseRequest) error {
				releases++
				source["runtime_memory_total_bytes"] = "changed-after-collection"
				data, _ := json.Marshal(req.Lease.Server)
				if strings.Contains(string(data), "runtime_memory_total") {
					t.Error("reporting evidence entered release authority")
				}
				return nil
			}
			t.Cleanup(func() { runEnvProfileTestEvidenceHook = nil })
			args := []string{"--provider", runEnvProfileTestProvider{}.Name(), "--no-sync", "--no-hydrate", "--timing-json"}
			if keep {
				args = append(args, "--id", lease.LeaseID, "--keep")
			}
			args = append(args, "--", "fixture-memory-command")
			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(t.Context(), args)
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 137 {
				t.Fatalf("original exit lost: %v\n%s", err, stderr.String())
			}
			wantReleases := 1
			if keep {
				wantReleases = 0
			}
			if begins != 1 || collects != 1 || releases != wantReleases || strings.Count(stderr.String(), "failure digest\n") != 1 {
				t.Fatalf("lifecycle counts begin=%d collect=%d release=%d\n%s", begins, collects, releases, stderr.String())
			}
			var report TimingReport
			bundle := ""
			for _, line := range strings.Split(stderr.String(), "\n") {
				if strings.HasPrefix(line, "{") {
					_ = json.Unmarshal([]byte(line), &report)
				}
				if strings.HasPrefix(line, "failure-bundle local=") {
					bundle = strings.Fields(strings.TrimPrefix(line, "failure-bundle local="))[0]
				}
			}
			if report.ExitCode != 137 || report.ResourceExhaustion != ResourceExhaustionMemory || report.FailureEvidence == nil || report.FailureEvidence.Details["runtime_memory_total_bytes"] != "8388608" {
				t.Fatalf("final timing lost snapshot: %+v\n%s", report, stderr.String())
			}
			if !strings.Contains(stderr.String(), "hint: Check runtime capacity and swap") || strings.Contains(stderr.String(), "changed-after-collection") {
				t.Fatal("deferred digest did not retain sanitized snapshot")
			}
			if bundle == "" {
				t.Fatalf("no failure bundle\n%s", stderr.String())
			}
			contents := readTarGzContents(t, bundle)
			var captured TimingReport
			if err := json.Unmarshal(contents["crabbox-artifacts/timings.json"], &captured); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(captured.FailureEvidence, report.FailureEvidence) {
				t.Fatal("bundle and final evidence differ")
			}
		})
	}
}

func TestPrintRunFailureDigest(t *testing.T) {
	var buf bytes.Buffer
	printRunFailureDigest(&buf, runFailureDigestInput{
		LeaseID:        "cbx_123",
		Slug:           "blue-lobster",
		RunID:          "run_123",
		CommandDisplay: "go test ./...",
		Classification: FailureClassification{BlockedStage: "unknown", RetryLikely: "unknown"},
		Phases:         []TimingPhase{{Name: "test"}},
	})
	out := buf.String()
	for _, want := range []string{
		"failure digest",
		"phase: test",
		"area: user_command",
		"next: crabbox logs run_123 --tail 80",
		"next: crabbox doctor --from-run run_123",
		"next: crabbox run --id blue-lobster --fresh-sync -- go test ./...",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("digest missing %q:\n%s", want, out)
		}
	}
}

func TestPrintRunFailureDigestExplainsUnavailableRunHistory(t *testing.T) {
	var buf bytes.Buffer
	printRunFailureDigest(&buf, runFailureDigestInput{
		LeaseID:               "cbx_123",
		Slug:                  "blue-lobster",
		RunID:                 "run_local123",
		RunHistoryUnavailable: true,
		CommandDisplay:        "go test ./...",
		Classification:        FailureClassification{BlockedStage: "unknown", RetryLikely: "unknown"},
	})
	out := buf.String()
	for _, want := range []string{
		"run_history: unavailable",
		"next: crabbox ssh --id blue-lobster",
		"next: crabbox run --id blue-lobster --fresh-sync -- go test ./...",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("digest missing %q:\n%s", want, out)
		}
	}
	for _, unexpected := range []string{
		"crabbox logs run_",
		"crabbox events run_",
		"crabbox doctor --from-run run_",
	} {
		if strings.Contains(out, unexpected) {
			t.Fatalf("digest should not include run-based command %q:\n%s", unexpected, out)
		}
	}
}

func TestFailureDigestNextCommandsRespectRunHistoryAvailability(t *testing.T) {
	historyCommands := []string{
		"crabbox logs run_123 --tail 80",
		"crabbox events run_123 --type stderr",
		"crabbox doctor --from-run run_123",
	}
	leaseCommands := []string{
		"crabbox ssh --provider local-container --id retained-direct",
		"crabbox run --provider local-container --id retained-direct --fresh-sync -- go test ./...",
		"crabbox stop --provider local-container retained-direct",
	}
	tests := []struct {
		name               string
		runID              string
		historyUnavailable bool
		wantHistory        bool
	}{
		{name: "run ID with unavailable history", runID: "run_123", historyUnavailable: true},
		{name: "run ID with available history", runID: "run_123", wantHistory: true},
		{name: "no run ID", historyUnavailable: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			commands := failureDigestNextCommands(runFailureDigestInput{
				Provider:              "local-container",
				LeaseID:               "cbx_123",
				Slug:                  "retained-direct",
				RunID:                 tt.runID,
				RunHistoryUnavailable: tt.historyUnavailable,
				CommandDisplay:        "go test ./...",
				Classification:        FailureClassification{RetryLikely: "unknown"},
			}, "unknown")
			joined := strings.Join(commands, "\n")
			for _, command := range historyCommands {
				if got := strings.Contains(joined, command); got != tt.wantHistory {
					t.Fatalf("history command presence=%v, want %v for %q:\n%s", got, tt.wantHistory, command, joined)
				}
			}
			for _, command := range leaseCommands {
				if !strings.Contains(joined, command) {
					t.Fatalf("lease recovery command missing %q:\n%s", command, joined)
				}
			}
		})
	}
}

func TestRunFailureDigestRoutingUseExternalRoutingFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	args := CommandRoutingFor(Config{
		Provider: "external",
		External: ExternalConfig{
			Command: "provider-command",
			Args:    []string{"--token", "secret-value"},
			Config:  map[string]any{"token": "secret-config"},
		},
	}, "provider-id", CommandRoutingRetry)
	got := strings.Join(args.Args, " ")
	for _, want := range []string{"--provider external", "--external-routing-file", "--external-routing-digest"} {
		if !strings.Contains(got, want) {
			t.Fatalf("routing args missing %q: %s", want, got)
		}
	}
	for _, secret := range []string{"provider-command", "secret-value", "secret-config"} {
		if strings.Contains(got, secret) {
			t.Fatalf("routing args leaked %q: %s", secret, got)
		}
	}
}

func TestRunFailureDigestSSHRoutingUseExternalRoutingFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	args := CommandRoutingFor(Config{
		Provider: "external",
		External: ExternalConfig{
			Command: "provider-command",
			Args:    []string{"--token", "secret-value"},
			Config:  map[string]any{"token": "secret-config"},
		},
	}, "cbx_abcdef123456", CommandRoutingRetry)
	got := strings.Join(args.Args, " ")
	for _, want := range []string{"--provider external", "--external-routing-file", "--external-routing-digest"} {
		if !strings.Contains(got, want) {
			t.Fatalf("ssh routing args missing %q: %s", want, got)
		}
	}
	for _, secret := range []string{"provider-command", "secret-value", "secret-config"} {
		if strings.Contains(got, secret) {
			t.Fatalf("ssh routing args leaked %q: %s", secret, got)
		}
	}
}

func TestPrintRunFailureDigestExplainsAndChainShortCircuit(t *testing.T) {
	var buf bytes.Buffer
	printRunFailureDigest(&buf, runFailureDigestInput{
		LeaseID:        "cbx_123",
		CommandDisplay: "pnpm check && pnpm test",
		ShellMode:      true,
		Classification: FailureClassification{BlockedStage: "unknown", RetryLikely: "unknown"},
	})
	out := buf.String()
	for _, want := range []string{
		"area: user_command",
		"shell_chain: pnpm check && pnpm test",
		"would_skip_if_left_failed: pnpm test",
		"chain_semantics: && only runs later segments if all earlier segments succeed",
		"next: crabbox run --id cbx_123 --fresh-sync --shell -- 'pnpm check && pnpm test'",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("digest missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "provider_auth") {
		t.Fatalf("digest should not mention provider_auth:\n%s", out)
	}
}

func TestPrintRunFailureDigestSuppressesMixedAndOrChain(t *testing.T) {
	var buf bytes.Buffer
	printRunFailureDigest(&buf, runFailureDigestInput{
		LeaseID:        "cbx_123",
		CommandDisplay: "pnpm build && pnpm test || pnpm cleanup",
		ShellMode:      true,
		Classification: FailureClassification{BlockedStage: "unknown", RetryLikely: "unknown"},
	})
	out := buf.String()
	for _, unexpected := range []string{"shell_chain:", "would_skip_if_left_failed:", "chain_semantics:"} {
		if strings.Contains(out, unexpected) {
			t.Fatalf("digest should suppress mixed &&/|| chain note %q:\n%s", unexpected, out)
		}
	}
}

func TestPrintRunFailureDigestIncludesObservedPhases(t *testing.T) {
	var buf bytes.Buffer
	printRunFailureDigest(&buf, runFailureDigestInput{
		LeaseID:        "cbx_123",
		CommandDisplay: "pnpm verify",
		Classification: FailureClassification{BlockedStage: "unknown", RetryLikely: "unknown"},
		Phases: []TimingPhase{
			{Name: "user-command"},
			{Name: "check"},
			{Name: "test"},
		},
	})
	out := buf.String()
	for _, want := range []string{
		"phase: test",
		"failed_phase: test",
		"observed_phases: user-command,check,test",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("digest missing %q:\n%s", want, out)
		}
	}
}

func TestPrintRunFailureDigestIncludesStructuredTestFailures(t *testing.T) {
	var buf bytes.Buffer
	printRunFailureDigest(&buf, runFailureDigestInput{
		LeaseID:        "cbx_123",
		CommandDisplay: "pnpm test",
		Classification: FailureClassification{BlockedStage: "unknown", RetryLikely: "unknown"},
		Results: &TestResultSummary{
			Files:    []string{"junit.xml"},
			Tests:    2,
			Failures: 1,
			Failed: []TestFailure{{
				File:      "src/example.test.ts\x1b]0;file\x07",
				Classname: "ui\u202e",
				Name:      "renders\bhidden",
				Kind:      "failure\u0085",
				Message:   "expected true\rspoofed",
			}},
		},
	})
	out := buf.String()
	for _, want := range []string{
		"test_results: files=1 tests=2 failures=1 errors=0 skipped=0",
		`failed_test: src/example.test.ts\u001B]0;file\u0007 failure\u0085 ui\u202E.renders\u0008hidden - expected true\u000Dspoofed`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("digest missing %q:\n%s", want, out)
		}
	}
	assertTerminalSafe(t, out)
}

func TestFailureDigestSuppressesScriptRetryCommand(t *testing.T) {
	commands := failureDigestNextCommands(runFailureDigestInput{
		LeaseID:        "cbx_123",
		CommandDisplay: "'--script=./smoke test.sh' arg",
		ScriptMode:     true,
		Classification: FailureClassification{RetryLikely: "unknown"},
	}, "unknown")
	for _, command := range commands {
		if strings.Contains(command, "crabbox run") {
			t.Fatalf("script retry command should be suppressed: %v", commands)
		}
	}
}

func TestFailureDigestRoutesNextCommands(t *testing.T) {
	commands := failureDigestNextCommands(runFailureDigestInput{
		Provider:       "aws",
		TargetOS:       targetWindows,
		WindowsMode:    windowsModeWSL2,
		LeaseID:        "cbx_123",
		CommandDisplay: "go test ./...",
		Classification: FailureClassification{RetryLikely: "unknown"},
		StopCommand:    "crabbox stop --provider aws --target windows --windows-mode wsl2 cbx_123",
	}, "unknown")
	joined := strings.Join(commands, "\n")
	for _, want := range []string{
		"crabbox ssh --provider aws --target windows --windows-mode wsl2 --id cbx_123",
		"crabbox run --provider aws --target windows --windows-mode wsl2 --id cbx_123 --fresh-sync -- go test ./...",
		"crabbox stop --provider aws --target windows --windows-mode wsl2 cbx_123",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("commands missing %q:\n%s", want, joined)
		}
	}
}

func TestFailureDigestRoutesProviderArgsToSSH(t *testing.T) {
	cfg := Config{Provider: "proxmox", Proxmox: ProxmoxConfig{APIURL: "https://pve.example"}}
	commands := failureDigestNextCommands(runFailureDigestInput{
		Provider:       "proxmox",
		LeaseID:        "cbx_123",
		CommandDisplay: "go test ./...",
		Routing:        CommandRoutingFor(cfg, "cbx_123", CommandRoutingRetry),
		SSHRouting:     CommandRoutingFor(cfg, "cbx_123", CommandRoutingRetry),
		StopRouting:    CommandRoutingFor(cfg, "cbx_123", CommandRoutingStop),
		Classification: FailureClassification{RetryLikely: "unknown"},
		StopCommand:    "crabbox stop --provider proxmox --proxmox-api-url https://pve.example cbx_123",
	}, "unknown")
	if len(commands) < 3 {
		t.Fatalf("commands=%v", commands)
	}
	for _, command := range commands[:3] {
		if !strings.Contains(command, "--proxmox-api-url") {
			t.Fatalf("command lost provider routing: %q\nall=%v", command, commands)
		}
	}
}

func TestFailureDigestPreservesInheritedKubeconfigForKubeVirt(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/base.yaml:/tmp/cluster.yaml")
	cfg := Config{
		Provider: "kubevirt",
		TargetOS: targetLinux,
		KubeVirt: KubeVirtConfig{
			Kubectl:   "kubectl",
			Virtctl:   "virtctl",
			Context:   "dev=west",
			Namespace: "team-vms",
		},
	}
	commands := failureDigestNextCommands(runFailureDigestInput{
		Provider:       "kubevirt",
		TargetOS:       targetLinux,
		LeaseID:        "cbx_123",
		CommandDisplay: "go test ./...",
		Routing:        CommandRoutingFor(cfg, "cbx_123", CommandRoutingRetry),
		SSHRouting:     CommandRoutingFor(cfg, "cbx_123", CommandRoutingRetry),
		StopRouting:    CommandRoutingFor(cfg, "cbx_123", CommandRoutingStop),
		Classification: FailureClassification{RetryLikely: "unknown"},
	}, "unknown")
	joined := strings.Join(commands, "\n")
	for _, want := range []string{
		"KUBECONFIG='/tmp/base.yaml:/tmp/cluster.yaml' crabbox ssh --provider kubevirt",
		"KUBECONFIG='/tmp/base.yaml:/tmp/cluster.yaml' crabbox run --provider kubevirt",
		"KUBECONFIG='/tmp/base.yaml:/tmp/cluster.yaml' crabbox stop --provider kubevirt",
		"--kubevirt-context dev=west",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("commands missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "dev='west' crabbox") {
		t.Fatalf("context value was hoisted as env assignment:\n%s", joined)
	}
}

func TestFailureDigestPreservesSealosRouting(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/base.yaml:/tmp/cluster.yaml")
	cfg := Config{
		Provider: "sealos-devbox",
		TargetOS: targetLinux,
		SealosDevbox: SealosDevboxConfig{
			Kubectl:        "/opt/bin/kubectl",
			Context:        "dev=west",
			Namespace:      "team-devboxes",
			Network:        "SSHGate",
			SSHGatewayHost: "ssh.example.test",
			SSHGatewayPort: "2222",
			SSHUser:        "devbox",
			WorkRoot:       "/home/devbox/project",
		},
	}
	commands := failureDigestNextCommands(runFailureDigestInput{
		Provider:       "sealos-devbox",
		TargetOS:       targetLinux,
		LeaseID:        "cbx_123",
		CommandDisplay: "go test ./...",
		Routing:        CommandRoutingFor(cfg, "cbx_123", CommandRoutingRetry),
		SSHRouting:     CommandRoutingFor(cfg, "cbx_123", CommandRoutingRetry),
		StopRouting:    CommandRoutingFor(cfg, "cbx_123", CommandRoutingStop),
		Classification: FailureClassification{RetryLikely: "unknown"},
	}, "unknown")
	joined := strings.Join(commands, "\n")
	for _, want := range []string{
		"KUBECONFIG='/tmp/base.yaml:/tmp/cluster.yaml' crabbox ssh --provider sealos-devbox",
		"KUBECONFIG='/tmp/base.yaml:/tmp/cluster.yaml' crabbox run --provider sealos-devbox",
		"KUBECONFIG='/tmp/base.yaml:/tmp/cluster.yaml' crabbox stop --provider sealos-devbox",
		"--sealos-devbox-context dev=west",
		"--sealos-devbox-ssh-gateway-host ssh.example.test",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("commands missing %q:\n%s", want, joined)
		}
	}
}

func TestFailureTailRedactsHTMLAuthBody(t *testing.T) {
	tail := newStreamTailBuffer(40)
	_, _ = tail.Write([]byte("<!doctype html><html><head><title>Cloudflare Access</title></head><body>login</body></html>\n"))
	var buf bytes.Buffer
	printFailureTail(&buf, "stderr", tail, "")
	out := buf.String()
	if !strings.Contains(out, "stderr tail redacted:") || !strings.Contains(out, "redacted auth_cloudflare_html response") {
		t.Fatalf("tail was not redacted: %q", out)
	}
	if strings.Contains(out, "<html>") || strings.Contains(out, "<body>") {
		t.Fatalf("tail leaked HTML body: %q", out)
	}
}

func TestFailureTailKeepsNonAuthHTMLBody(t *testing.T) {
	tail := newStreamTailBuffer(40)
	_, _ = tail.Write([]byte("<!doctype html><html><head><title>App Output</title></head><body>rendered page</body></html>\n"))
	var buf bytes.Buffer
	printFailureTail(&buf, "stdout", tail, "")
	out := buf.String()
	if strings.Contains(out, "tail redacted") || !strings.Contains(out, "<body>rendered page</body>") {
		t.Fatalf("non-auth HTML tail was changed: %q", out)
	}
}

func TestFailureTailKeepsApplicationAuthHTMLBody(t *testing.T) {
	tail := newStreamTailBuffer(40)
	_, _ = tail.Write([]byte("<!doctype html><html><head><title>App Login</title></head><body>401 Unauthorized access denied</body></html>\n"))
	var buf bytes.Buffer
	printFailureTail(&buf, "stdout", tail, "")
	out := buf.String()
	if strings.Contains(out, "tail redacted") || !strings.Contains(out, "401 Unauthorized") {
		t.Fatalf("application auth HTML tail was changed: %q", out)
	}
}

func TestSelectProofLogExcerptRedactsHTMLAuthBody(t *testing.T) {
	got := SelectProofLogExcerpt("<!doctype html><html><head><title>Cloudflare Access</title></head><body>login</body></html>")
	if !strings.Contains(got, "redacted auth_cloudflare_html response") {
		t.Fatalf("proof excerpt was not redacted: %q", got)
	}
}

func TestFailureDigestStoppedLeaseKeepsHistoryAndLocalEvidence(t *testing.T) {
	var out bytes.Buffer
	printRunFailureDigest(&out, runFailureDigestInput{
		LeaseID: "cbx_stopped", LeaseStopped: true, RunID: "run_failed",
		CommandDisplay: "false", StopCommand: "crabbox stop --id cbx_stopped",
	})
	printFailureTail(&out, "stdout", nil, "stdout.log")
	printFailureTail(&out, "stderr", nil, "stderr.log")
	for _, want := range []string{"crabbox logs run_failed", "crabbox events run_failed", "crabbox doctor --from-run run_failed", "lease: stopped", "stderr.log", "stdout.log"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q: %s", want, out.String())
		}
	}
	for _, command := range []string{"ssh", "run", "stop"} {
		if strings.Contains(out.String(), "next: crabbox "+command+" ") {
			t.Errorf("stale recovery command %s: %s", command, out.String())
		}
	}
}
