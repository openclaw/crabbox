package cli

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestImageEvidenceGenericRecords(t *testing.T) {
	evidence := &ImageEvidence{ConfiguredReference: "created:tag", RuntimeImageID: "runtime-id", RepositoryDigests: []string{"example.invalid/base@sha256:" + strings.Repeat("a", 64)}, RepositoryDigestStatus: "available"}
	server := Server{ImageEvidence: evidence, ServerType: ServerTypeInfo{Name: "created_tag"}}
	var report TimingReport
	populateRunTimingMetadata(&report, Config{}, Repo{}, server, "lease", "run", "/work", nil)
	var context strings.Builder
	printRunContextSummary(&context, nil, Config{Provider: "fixture"}, server, SSHTarget{}, "lease", "run", "", "/work", false, "")
	if !strings.Contains(context.String(), `runtime_image_id="runtime-id"`) || !strings.Contains(context.String(), "type=created_tag") {
		t.Fatalf("context=%q", context.String())
	}
	claim := cloneLeaseClaim(leaseClaim{ImageEvidence: evidence})
	evidence.RepositoryDigests[0] = "mutated-after-capture"
	if report.MachineType != "created_tag" || report.ImageEvidence.RepositoryDigests[0] == evidence.RepositoryDigests[0] || claim.ImageEvidence.RepositoryDigests[0] == evidence.RepositoryDigests[0] {
		t.Fatal("image evidence changed machineType or shares mutable digest storage")
	}
	data, err := json.Marshal(finalizeTimingReport(report))
	if err != nil || !strings.Contains(string(data), `"imageEvidence":{"configuredReference":"created:tag","runtimeImageId":"runtime-id"`) {
		t.Fatalf("timing image evidence missing: %s %v", data, err)
	}
	proof, err := renderRunProof(proofRenderInput{ImageEvidence: report.ImageEvidence, Command: "true"})
	if err != nil || !strings.Contains(proof, `runtime_image_id="runtime-id"`) {
		t.Fatalf("proof image evidence missing: %v", err)
	}
	data, err = json.Marshal(TimingReport{Provider: "other"})
	if err != nil || strings.Contains(string(data), "imageEvidence") {
		t.Fatal("other provider gained an image observation")
	}
	for _, status := range []string{"unavailable", "unknown"} {
		data, err = json.Marshal(CloneImageEvidence(&ImageEvidence{RepositoryDigestStatus: status}))
		if err != nil || !strings.Contains(string(data), `"repositoryDigests":[]`) {
			t.Fatalf("%s digest collection must be an explicit empty array", status)
		}
	}
}

func TestFinalizeRunnerPhasesNeverInflatesTotal(t *testing.T) {
	report := finalizeTimingReport(TimingReport{
		Provider:      "aws",
		RunnerTotalMs: 10,
		RunnerPhases: []RunnerPhase{
			{Name: "provider.request", Ms: 6},
			{Name: "connect.provider", Ms: 5},
			{Name: "command", Ms: 0},
			{Name: "artifacts", Ms: -1},
			{Name: "", Ms: 1},
		},
	})
	if report.RunnerTotalMs != 10 {
		t.Fatalf("runner total=%d, want 10", report.RunnerTotalMs)
	}
	want := []RunnerPhase{
		{Name: "provider.request", Ms: 6, Provider: "aws"},
		{Name: "unattributed", Ms: 4},
	}
	if len(report.RunnerPhases) != len(want) {
		t.Fatalf("runner phases=%#v", report.RunnerPhases)
	}
	for i := range want {
		if report.RunnerPhases[i] != want[i] {
			t.Fatalf("phase[%d]=%#v, want %#v", i, report.RunnerPhases[i], want[i])
		}
	}
}

func TestFinalizeRunnerPhasesUsesDelegatedOpaqueRemainder(t *testing.T) {
	report := finalizeTimingReport(TimingReport{
		Provider:      "sandbox",
		SyncDelegated: true,
		SyncMs:        200,
		CommandMs:     400,
		RunnerTotalMs: 1000,
	})
	want := []RunnerPhase{
		{Name: "workspace.sync", Ms: 200, Provider: "sandbox"},
		{Name: "command", Ms: 400, Provider: "sandbox"},
		{
			Name:     "delegated.opaque",
			Ms:       400,
			Opaque:   true,
			Reason:   "provider owns lifecycle work outside measured sync and command",
			Provider: "sandbox",
		},
	}
	if len(report.RunnerPhases) != len(want) {
		t.Fatalf("runner phases=%#v", report.RunnerPhases)
	}
	for i := range want {
		if report.RunnerPhases[i] != want[i] {
			t.Fatalf("phase[%d]=%#v, want %#v", i, report.RunnerPhases[i], want[i])
		}
	}
}

func TestRunnerPhasesKeepReplacementAttemptsSeparate(t *testing.T) {
	timings := runTimings{
		lease:       600 * time.Millisecond,
		leasePhase:  "provider.acquire",
		connect:     100 * time.Millisecond,
		syncConnect: 25 * time.Millisecond,
		bootstrap:   100 * time.Millisecond,
		sync:        300 * time.Millisecond,
		providerTiming: &runnerProviderTiming{
			TotalMs: 500,
			Phases:  []RunnerPhase{{Name: "provider.request", Ms: 500}},
		},
	}
	resetRunnerTimingsForReplacement(
		&timings,
		TimingReport{Provider: "aws", LeaseID: "cbx_old", Slug: "old-runner", MachineType: "m7i.large"},
		75*time.Millisecond,
		200*time.Millisecond,
		&runnerProviderTiming{TotalMs: 150, Phases: []RunnerPhase{{Name: "provider.request", Ms: 150}}},
	)
	timings.connect = 50 * time.Millisecond
	timings.sync = 100 * time.Millisecond
	timings.command = 200 * time.Millisecond

	report := timingReportFromRun("aws", "cbx_new", "new-runner", timings, 350*time.Millisecond, 0)
	report.MachineType = "m8i.large"
	report.RunID = "run_123"
	report.RunnerTotalMs = 1625
	report = finalizeTimingReport(report)

	if report.LeaseMs != 800 || report.BootstrapMs != 100 || report.SyncMs != 100 {
		t.Fatalf("legacy timing lease=%d bootstrap=%d sync=%d", report.LeaseMs, report.BootstrapMs, report.SyncMs)
	}
	var oldProvider, newProvider, oldCleanup *RunnerPhase
	for i := range report.RunnerPhases {
		phase := &report.RunnerPhases[i]
		if phase.Name == "cleanup" && phase.LeaseID == "cbx_old" {
			oldCleanup = phase
		}
		if phase.Name != "provider.request" {
			continue
		}
		switch phase.LeaseID {
		case "cbx_old":
			oldProvider = phase
		case "cbx_new":
			newProvider = phase
		}
	}
	if oldProvider == nil || newProvider == nil || oldCleanup == nil {
		t.Fatalf("replacement phases=%#v", report.RunnerPhases)
	}
	if oldProvider.Slug != "old-runner" || oldProvider.MachineType != "m7i.large" {
		t.Fatalf("old provider phase=%#v", oldProvider)
	}
	if newProvider.Slug != "new-runner" || newProvider.MachineType != "m8i.large" {
		t.Fatalf("new provider phase=%#v", newProvider)
	}
	if oldCleanup.Ms != 75 {
		t.Fatalf("old cleanup phase=%#v", oldCleanup)
	}
}

func TestRunnerPhasesDoNotChangeTerminalReceiptV2(t *testing.T) {
	report := finalizeTimingReport(TimingReport{
		Provider:      "aws",
		RunnerTotalMs: 10,
		RunnerPhases:  []RunnerPhase{{Name: "command", Ms: 10}},
	})
	if report.RunnerTotalMs != 10 {
		t.Fatal("runner timing setup failed")
	}
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	receipt, err := buildTerminalRunReceiptWithKey(key, terminalRunReceiptInput{
		Provider:          "aws",
		LeaseID:           "cbx_123",
		RunID:             "run_123",
		Command:           []string{"true"},
		CommandDisplay:    "true",
		StartedAt:         time.Unix(100, 0),
		EndedAt:           time.Unix(101, 0),
		LogSHA256:         sha256Digest(nil),
		RetainedLogSHA256: sha256Digest(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.SchemaVersion != 2 || strings.Contains(string(encoded), "runner") {
		t.Fatalf("receipt changed with runner telemetry: %s", encoded)
	}
}
