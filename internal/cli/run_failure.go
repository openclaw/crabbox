package cli

import (
	"fmt"
	"strings"
)

type FailureClassification struct {
	BlockedStage string
	RetryLikely  string
}

func ClassifyRunFailure(exitCode int, text string, phases []TimingPhase) FailureClassification {
	if exitCode == 0 {
		return FailureClassification{}
	}
	lower := strings.ToLower(stripANSI(text))
	switch {
	case strings.Contains(lower, "timed out waiting for ssh"):
		return FailureClassification{BlockedStage: "ssh", RetryLikely: "true"}
	case isKnownHTMLAuthBody(lower) ||
		strings.Contains(lower, "cloudflare access") ||
		strings.Contains(lower, "provider_auth") ||
		strings.Contains(lower, "provider auth"):
		return FailureClassification{BlockedStage: "provider_auth", RetryLikely: "false"}
	case strings.Contains(lower, "exdev") ||
		strings.Contains(lower, "enomem") ||
		strings.Contains(lower, "package-import-method") ||
		strings.Contains(lower, "child-concurrency") ||
		strings.Contains(lower, "network-concurrency"):
		return FailureClassification{BlockedStage: "install", RetryLikely: "unknown"}
	case strings.Contains(lower, "model_call") ||
		strings.Contains(lower, "model call") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "context length") ||
		strings.Contains(lower, "context window") ||
		strings.Contains(lower, "tokens") && strings.Contains(lower, "maximum"):
		return FailureClassification{BlockedStage: "model_call", RetryLikely: "unknown"}
	}
	if phaseName := finalTimingPhaseName(phases); strings.Contains(phaseName, "install") || strings.Contains(phaseName, "hydrate") || strings.Contains(phaseName, "setup") {
		return FailureClassification{BlockedStage: "install", RetryLikely: "unknown"}
	}
	return FailureClassification{BlockedStage: "unknown", RetryLikely: "unknown"}
}

func ApplyFailureClassification(report *TimingReport, classification FailureClassification) {
	if report == nil {
		return
	}
	report.BlockedStage = classification.BlockedStage
	report.RetryLikely = classification.RetryLikely
}

func FormatFailureClassificationFields(classification FailureClassification) string {
	if classification.BlockedStage == "" {
		return ""
	}
	retry := classification.RetryLikely
	if retry == "" {
		retry = "unknown"
	}
	return fmt.Sprintf(" blocked_stage=%s retry_likely=%s", classification.BlockedStage, retry)
}

func RedactKnownFailureBody(text string) (string, bool) {
	trimmed := strings.TrimSpace(stripANSI(text))
	if trimmed == "" {
		return "", false
	}
	lower := strings.ToLower(trimmed)
	if !isKnownHTMLAuthBody(lower) {
		return "", false
	}
	kind := "html"
	if strings.Contains(lower, "cloudflare") {
		kind = "cloudflare_html"
	}
	if strings.Contains(lower, "access") || strings.Contains(lower, "login") || strings.Contains(lower, "challenge") {
		kind = "auth_" + kind
	}
	title := htmlTitle(trimmed)
	if title != "" {
		return fmt.Sprintf("[crabbox: redacted %s response bytes=%d title=%q]", kind, len(text), title), true
	}
	return fmt.Sprintf("[crabbox: redacted %s response bytes=%d]", kind, len(text)), true
}

func isKnownHTMLAuthBody(lower string) bool {
	hasHTML := strings.Contains(lower, "<!doctype html") ||
		strings.Contains(lower, "<html") ||
		strings.Contains(lower, "<body") ||
		strings.Contains(lower, "<head")
	if !hasHTML {
		return false
	}
	return strings.Contains(lower, "cloudflare access") ||
		strings.Contains(lower, "cf-access") ||
		strings.Contains(lower, "__cf_chl_")
}

func htmlTitle(text string) string {
	lower := strings.ToLower(text)
	start := strings.Index(lower, "<title")
	if start < 0 {
		return ""
	}
	closeStart := strings.Index(lower[start:], ">")
	if closeStart < 0 {
		return ""
	}
	titleStart := start + closeStart + 1
	end := strings.Index(lower[titleStart:], "</title>")
	if end < 0 {
		return ""
	}
	title := strings.Join(strings.Fields(text[titleStart:titleStart+end]), " ")
	if len(title) > 120 {
		title = title[:117] + "..."
	}
	return title
}

func finalTimingPhaseName(phases []TimingPhase) string {
	for i := len(phases) - 1; i >= 0; i-- {
		name := strings.ToLower(strings.TrimSpace(phases[i].Name))
		if name != "" {
			return name
		}
	}
	return ""
}
