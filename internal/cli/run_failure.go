package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

const runFailureEvidenceTimeout = 5 * time.Second

type FailureClassification struct {
	BlockedStage       string
	ResourceExhaustion ResourceExhaustionReason
	RetryLikely        string
}

func ClassifyRunFailure(exitCode int, text string, phases []TimingPhase) FailureClassification {
	return ClassifyRunFailureWithEvidence(exitCode, text, phases, RunFailureEvidence{})
}

func ClassifyRunFailureWithEvidence(exitCode int, text string, phases []TimingPhase, evidence RunFailureEvidence) FailureClassification {
	if evidence.ResourceExhaustion == ResourceExhaustionMemory {
		return FailureClassification{
			BlockedStage:       "resource_exhaustion",
			ResourceExhaustion: ResourceExhaustionMemory,
			RetryLikely:        "false",
		}
	}
	if exitCode == 0 {
		return FailureClassification{}
	}
	lower := strings.ToLower(stripANSI(text))
	switch {
	case strings.Contains(lower, "blacksmith") &&
		strings.Contains(lower, "backend.blacksmith.sh") &&
		(strings.Contains(lower, "shutdown") || strings.Contains(lower, "lookup") || strings.Contains(lower, "no such host")):
		return FailureClassification{BlockedStage: "cleanup", RetryLikely: "true"}
	case strings.Contains(lower, "blacksmith") &&
		strings.Contains(lower, "sync did not print a completion marker"):
		return FailureClassification{BlockedStage: "sync", RetryLikely: "true"}
	case isBlacksmithActionsCancelled(lower):
		return FailureClassification{BlockedStage: "actions_cancelled", RetryLikely: "true"}
	case isBlacksmithPostReadyStall(lower):
		return FailureClassification{BlockedStage: "testbox_stalled_after_ready", RetryLikely: "true"}
	case strings.Contains(lower, "timed out waiting for ssh"):
		return FailureClassification{BlockedStage: "ssh", RetryLikely: "true"}
	case isKnownHTMLAuthBody(lower):
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

func classifyRunOutcomeFailure(exitCode int, text string, phases []TimingPhase, evidence RunFailureEvidence, testResultsFailed bool) FailureClassification {
	classification := ClassifyRunFailureWithEvidence(exitCode, text, phases, evidence)
	if testResultsFailed && classification.ResourceExhaustion == "" {
		return FailureClassification{BlockedStage: "test", RetryLikely: "false"}
	}
	return classification
}

func isBlacksmithActionsCancelled(lower string) bool {
	if !strings.Contains(lower, "testbox ready") {
		return false
	}
	return strings.Contains(lower, "github actions run cancelled") ||
		strings.Contains(lower, "github actions run canceled") ||
		strings.Contains(lower, "workflow run cancelled") ||
		strings.Contains(lower, "workflow run canceled")
}

func isBlacksmithPostReadyStall(lower string) bool {
	if !strings.Contains(lower, "blacksmith") || !strings.Contains(lower, "testbox ready") {
		return false
	}
	return strings.Contains(lower, "stalled after ready") ||
		strings.Contains(lower, "post-ready stall") ||
		strings.Contains(lower, "no output after ready")
}

func ApplyFailureClassification(report *TimingReport, classification FailureClassification) {
	if report == nil {
		return
	}
	report.BlockedStage = classification.BlockedStage
	report.ResourceExhaustion = classification.ResourceExhaustion
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
	fields := fmt.Sprintf(" blocked_stage=%s", classification.BlockedStage)
	if classification.ResourceExhaustion != "" {
		fields += fmt.Sprintf(" resource_exhaustion=%s", classification.ResourceExhaustion)
	}
	return fields + fmt.Sprintf(" retry_likely=%s", retry)
}

func beginRunFailureEvidence(ctx context.Context, backend SSHLeaseBackend, lease LeaseTarget, warnings io.Writer) RunFailureEvidenceCollector {
	capability, ok := backend.(SSHRunFailureEvidenceBackend)
	if !ok {
		return nil
	}
	bounded, cancel := context.WithTimeout(ctx, runFailureEvidenceTimeout)
	defer cancel()
	collector, err := capability.BeginRunFailureEvidence(bounded, RunFailureEvidenceRequest{Lease: lease})
	if err != nil {
		fmt.Fprintf(nonNilWriter(warnings), "warning: failed-run evidence baseline unavailable: %v\n", err)
		return nil
	}
	return collector
}

func collectRunFailureEvidence(ctx context.Context, collector RunFailureEvidenceCollector, warnings io.Writer) RunFailureEvidence {
	if collector == nil {
		return RunFailureEvidence{}
	}
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), runFailureEvidenceTimeout)
	defer cancel()
	evidence, err := collector(bounded)
	if err != nil {
		fmt.Fprintf(nonNilWriter(warnings), "warning: failed-run evidence collection incomplete: %v\n", err)
		return RunFailureEvidence{}
	}
	switch evidence.ResourceExhaustion {
	case "", ResourceExhaustionMemory:
		return evidence
	default:
		fmt.Fprintf(nonNilWriter(warnings), "warning: failed-run evidence collection returned unsupported resource exhaustion reason %q\n", evidence.ResourceExhaustion)
		return RunFailureEvidence{}
	}
}

func nonNilWriter(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
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

type runFailureDigestInput struct {
	Provider              string
	TargetOS              string
	WindowsMode           string
	LeaseID               string
	Slug                  string
	RunID                 string
	RunHistoryUnavailable bool
	CommandDisplay        string
	ShellMode             bool
	ScriptMode            bool
	Routing               CommandRouting
	SSHRouting            CommandRouting
	StopRouting           CommandRouting
	StopCommand           string
	Classification        FailureClassification
	Phases                []TimingPhase
	Results               *TestResultSummary
}

func printRunFailureDigest(w io.Writer, input runFailureDigestInput, stdoutTail, stderrTail *streamTailBuffer, stdoutCapture, stderrCapture string) {
	if w == nil {
		return
	}
	phase := failureDigestPhase(input.Classification, input.Phases)
	area := failureDigestArea(input.Classification, phase)
	retry := input.Classification.RetryLikely
	if retry == "" {
		retry = "unknown"
	}
	fmt.Fprintln(w, "failure digest")
	fmt.Fprintf(w, "  phase: %s\n", blank(phase, "unknown"))
	fmt.Fprintf(w, "  area: %s\n", area)
	fmt.Fprintf(w, "  retryable: %s\n", retry)
	if input.Classification.ResourceExhaustion != "" {
		fmt.Fprintf(w, "  resource_exhaustion: %s\n", input.Classification.ResourceExhaustion)
	}
	if input.Classification.ResourceExhaustion == ResourceExhaustionMemory {
		fmt.Fprintln(w, "  hint: increase the memory limit or reduce workload concurrency before retrying")
	}
	if input.RunHistoryUnavailable {
		fmt.Fprintln(w, "  run_history: unavailable; use lease-based recovery commands below")
	}
	printFailureDigestPhases(w, input.Phases)
	printFailureDigestShellChain(w, input)
	printFailureDigestResults(w, input.Results)
	for _, command := range failureDigestNextCommands(input, retry) {
		fmt.Fprintf(w, "  next: %s\n", command)
	}
	printFailureDigestTail(w, "stderr", stderrTail, stderrCapture)
	if stderrCapture != "" || tailLineCount(stderrTail) == 0 {
		printFailureDigestTail(w, "stdout", stdoutTail, stdoutCapture)
	}
}

func printFailureDigestPhases(w io.Writer, phases []TimingPhase) {
	names := timingPhaseNames(phases)
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(w, "  failed_phase: %s\n", names[len(names)-1])
	fmt.Fprintf(w, "  observed_phases: %s\n", strings.Join(names, ","))
}

func timingPhaseNames(phases []TimingPhase) []string {
	names := make([]string, 0, len(phases))
	for _, phase := range phases {
		name := strings.TrimSpace(phase.Name)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func failureDigestPhase(classification FailureClassification, phases []TimingPhase) string {
	if phase := finalTimingPhaseName(phases); phase != "" {
		return phase
	}
	if classification.BlockedStage != "" && classification.BlockedStage != "unknown" {
		return classification.BlockedStage
	}
	return "command"
}

func failureDigestArea(classification FailureClassification, phase string) string {
	switch classification.BlockedStage {
	case "provider_auth":
		return "provider_auth"
	case "ssh":
		return "ssh_connectivity"
	case "install":
		return "install_setup"
	case "model_call":
		return "model_tool_provider_limit"
	case "resource_exhaustion":
		return "resource_exhaustion"
	}
	switch {
	case strings.Contains(phase, "sync"):
		return "sync"
	case strings.Contains(phase, "install") || strings.Contains(phase, "setup") || strings.Contains(phase, "hydrate"):
		return "install_setup"
	case strings.Contains(phase, "ssh"):
		return "ssh_connectivity"
	default:
		return "user_command"
	}
}

func failureDigestNextCommands(input runFailureDigestInput, retry string) []string {
	var commands []string
	if input.RunID != "" && !input.RunHistoryUnavailable {
		commands = append(commands,
			"crabbox logs "+input.RunID+" --tail 80",
			"crabbox events "+input.RunID+" --type stderr",
			"crabbox doctor --from-run "+input.RunID,
		)
	}
	leaseRef := firstNonBlank(input.Slug, input.LeaseID)
	if leaseRef != "" {
		sshRouting := input.SSHRouting
		if len(sshRouting.Args) == 0 {
			sshRouting = fallbackFailureDigestRouting(input, CommandRoutingRetry)
		}
		commands = append(commands, sshRouting.ShellCommand(append(append([]string{"crabbox", "ssh"}, sshRouting.Args...), "--id", leaseRef)))
		if retry != "false" && !input.ScriptMode && canSuggestRunRetry(input.CommandDisplay) {
			routing := input.Routing
			if len(routing.Args) == 0 {
				routing = fallbackFailureDigestRouting(input, CommandRoutingRetry)
			}
			runArgs := append(append([]string{"crabbox", "run"}, routing.Args...), "--id", leaseRef, "--fresh-sync")
			if input.ShellMode {
				runArgs = append(runArgs, "--shell")
			}
			commands = append(commands, routing.ShellCommand(runArgs)+" -- "+failureDigestRetryCommand(input))
		}
		stopRouting := input.StopRouting
		if len(stopRouting.Args) == 0 {
			stopRouting = fallbackFailureDigestRouting(input, CommandRoutingStop)
		}
		commands = append(commands, firstNonBlank(input.StopCommand, stopRouting.ShellCommand(append(append([]string{"crabbox", "stop"}, stopRouting.Args...), leaseRef))))
	}
	return commands
}

func failureDigestRetryCommand(input runFailureDigestInput) string {
	if input.ShellMode {
		return strings.Join(readableShellWords([]string{input.CommandDisplay}), " ")
	}
	return input.CommandDisplay
}

func printFailureDigestShellChain(w io.Writer, input runFailureDigestInput) {
	if !input.ShellMode {
		return
	}
	segments := shellAndChainSegments(input.CommandDisplay)
	if len(segments) < 2 {
		return
	}
	fmt.Fprintf(w, "  shell_chain: %s\n", strings.Join(segments, " && "))
	fmt.Fprintf(w, "  would_skip_if_left_failed: %s\n", strings.Join(segments[1:], " && "))
	fmt.Fprintln(w, "  chain_semantics: && only runs later segments if all earlier segments succeed")
}

// Only attribute simple chains. This scanner is not a shell grammar: compound
// syntax, substitutions, lists, and pipelines make its explanation uncertain.
func shellAndChainSegments(command string) []string {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}
	var segments []string
	inSingle, inDouble, escaped := false, false, false
	start := 0
	for i := 0; i < len(command); i++ {
		ch := command[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && !inSingle {
			escaped = true
			continue
		}
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if inSingle {
			continue
		}
		if ch == '"' {
			inDouble = !inDouble
			continue
		}
		if ch == '`' || ch == '$' && i+1 < len(command) && command[i+1] == '(' {
			return nil
		}
		if inDouble {
			continue
		}
		switch ch {
		case '(', ')', '{', '}', ';', '\n', '\r', '|', '#':
			return nil
		case '[':
			if i+1 < len(command) && command[i+1] == '[' {
				return nil
			}
		case '<':
			if i+1 < len(command) && command[i+1] == '<' {
				return nil
			}
		case '&':
			if i+1 >= len(command) || command[i+1] != '&' {
				return nil
			}
			part := strings.TrimSpace(command[start:i])
			if part == "" {
				return nil
			}
			segments = append(segments, part)
			i++
			start = i + 1
		}
	}
	last := strings.TrimSpace(command[start:])
	if inSingle || inDouble || escaped || last == "" || len(segments) == 0 {
		return nil
	}
	segments = append(segments, last)
	for _, segment := range segments {
		switch strings.Fields(segment)[0] {
		case "if", "then", "else", "elif", "fi", "for", "while", "until", "do", "done", "case", "esac", "select", "function", "coproc", "!":
			return nil
		}
	}
	return segments
}

func printFailureDigestResults(w io.Writer, results *TestResultSummary) {
	if results == nil {
		return
	}
	fmt.Fprintf(w, "  test_results: files=%d tests=%d failures=%d errors=%d skipped=%d\n", len(results.Files), results.Tests, results.Failures, results.Errors, results.Skipped)
	limit := len(results.Failed)
	if limit > 5 {
		limit = 5
	}
	for i := 0; i < limit; i++ {
		display := terminalSafeTestFailure(results.Failed[i])
		if display.message != "" {
			fmt.Fprintf(w, "  failed_test: %s %-8s %s - %s\n", display.location, display.kind, display.name, display.message)
			continue
		}
		fmt.Fprintf(w, "  failed_test: %s %-8s %s\n", display.location, display.kind, display.name)
	}
	if len(results.Failed) > limit {
		fmt.Fprintf(w, "  failed_test: +%d more\n", len(results.Failed)-limit)
	}
}

func fallbackFailureDigestRouting(input runFailureDigestInput, purpose CommandRoutingPurpose) CommandRouting {
	return CommandRoutingFor(Config{Provider: input.Provider, TargetOS: input.TargetOS, WindowsMode: input.WindowsMode}, input.LeaseID, purpose)
}

func canSuggestRunRetry(commandDisplay string) bool {
	commandDisplay = strings.TrimSpace(commandDisplay)
	return commandDisplay != "" && !strings.HasPrefix(commandDisplay, "--script")
}

func printFailureDigestTail(w io.Writer, label string, tail *streamTailBuffer, capturedPath string) {
	if capturedPath != "" {
		fmt.Fprintf(w, "  tail %s: captured at %s\n", label, capturedPath)
		return
	}
	if tail == nil {
		return
	}
	lines := tail.Lines()
	if len(lines) == 0 {
		return
	}
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	text := strings.Join(lines, "\n")
	if redacted, ok := RedactKnownFailureBody(text); ok {
		fmt.Fprintf(w, "  tail %s: %s\n", label, redacted)
		return
	}
	fmt.Fprintf(w, "  tail %s:\n", label)
	for _, line := range lines {
		fmt.Fprintf(w, "    %s\n", line)
	}
}

func tailLineCount(tail *streamTailBuffer) int {
	if tail == nil {
		return 0
	}
	return len(tail.Lines())
}
