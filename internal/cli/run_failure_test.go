package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

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
		blockedStage: "ssh",
		retryLikely:  "true",
	}, time.Second, 1)
	var buf bytes.Buffer
	if err := writeTimingJSON(&buf, report); err != nil {
		t.Fatal(err)
	}
	var got TimingReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.BlockedStage != "ssh" || got.RetryLikely != "true" {
		t.Fatalf("classification not encoded: %#v", got)
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
