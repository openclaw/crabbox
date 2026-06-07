package cli

import (
	"fmt"
	"io"
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

type runFailureDigestInput struct {
	Provider       string
	TargetOS       string
	WindowsMode    string
	LeaseID        string
	Slug           string
	RunID          string
	CommandDisplay string
	ShellMode      bool
	ScriptMode     bool
	RoutingArgs    []string
	SSHRoutingArgs []string
	StopCommand    string
	Classification FailureClassification
	Phases         []TimingPhase
	Results        *TestResultSummary
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
	if input.RunID != "" {
		commands = append(commands,
			"crabbox logs "+input.RunID+" --tail 80",
			"crabbox events "+input.RunID+" --type stderr",
			"crabbox doctor --from-run "+input.RunID,
		)
	}
	leaseRef := firstNonBlank(input.Slug, input.LeaseID)
	if leaseRef != "" {
		sshRouting := append([]string(nil), input.SSHRoutingArgs...)
		if len(sshRouting) == 0 {
			sshRouting = fallbackFailureDigestRoutingArgs(input)
		}
		commands = append(commands, crabboxCommandString(append(append([]string{"ssh"}, sshRouting...), "--id", leaseRef)))
		if retry != "false" && !input.ScriptMode && canSuggestRunRetry(input.CommandDisplay) {
			routing := append([]string(nil), input.RoutingArgs...)
			if len(routing) == 0 {
				routing = fallbackFailureDigestRoutingArgs(input)
			}
			runArgs := append(append([]string{"run"}, routing...), "--id", leaseRef, "--fresh-sync")
			if input.ShellMode {
				runArgs = append(runArgs, "--shell")
			}
			runCommand := crabboxCommandString(runArgs) + " -- " + failureDigestRetryCommand(input)
			commands = append(commands, runCommand)
		}
		stopRouting := append([]string(nil), input.RoutingArgs...)
		if len(stopRouting) == 0 {
			stopRouting = fallbackFailureDigestRoutingArgs(input)
		}
		commands = append(commands, firstNonBlank(input.StopCommand, crabboxCommandString(append(append([]string{"stop"}, stopRouting...), leaseRef))))
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

func shellAndChainSegments(command string) []string {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}
	var segments []string
	var b strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	depth := 0
	flush := func() {
		part := strings.TrimSpace(b.String())
		if part != "" {
			segments = append(segments, part)
		}
		b.Reset()
	}
	for i := 0; i < len(command); i++ {
		ch := command[i]
		if escaped {
			b.WriteByte(ch)
			escaped = false
			continue
		}
		if ch == '\\' && !inSingle {
			b.WriteByte(ch)
			escaped = true
			continue
		}
		switch ch {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
			b.WriteByte(ch)
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
			b.WriteByte(ch)
		case '(', '{', '[':
			if !inSingle && !inDouble {
				depth++
			}
			b.WriteByte(ch)
		case ')', '}', ']':
			if !inSingle && !inDouble && depth > 0 {
				depth--
			}
			b.WriteByte(ch)
		case '&':
			if !inSingle && !inDouble && depth == 0 && i+1 < len(command) && command[i+1] == '&' {
				flush()
				i++
				continue
			}
			b.WriteByte(ch)
		case '|':
			if !inSingle && !inDouble && depth == 0 && i+1 < len(command) && command[i+1] == '|' {
				return nil
			}
			b.WriteByte(ch)
		default:
			b.WriteByte(ch)
		}
	}
	flush()
	if len(segments) < 2 {
		return nil
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
		failure := results.Failed[i]
		name := failure.Name
		if failure.Classname != "" {
			name = failure.Classname + "." + name
		}
		location := firstNonBlank(failure.File, failure.Suite, "-")
		message := strings.TrimSpace(firstLine(failure.Message))
		if message != "" {
			fmt.Fprintf(w, "  failed_test: %s %-8s %s - %s\n", location, failure.Kind, name, message)
			continue
		}
		fmt.Fprintf(w, "  failed_test: %s %-8s %s\n", location, failure.Kind, name)
	}
	if len(results.Failed) > limit {
		fmt.Fprintf(w, "  failed_test: +%d more\n", len(results.Failed)-limit)
	}
}

func runFailureDigestRoutingArgs(cfg Config, leaseID string) []string {
	args := []string{}
	if strings.TrimSpace(cfg.Provider) != "" {
		args = append(args, "--provider", cfg.Provider)
	}
	if strings.TrimSpace(cfg.TargetOS) != "" {
		args = append(args, "--target", cfg.TargetOS)
	}
	if cfg.TargetOS == targetWindows && strings.TrimSpace(cfg.WindowsMode) != "" {
		args = append(args, "--windows-mode", cfg.WindowsMode)
	}
	if strings.TrimSpace(cfg.Static.Host) != "" {
		args = append(args, "--static-host", cfg.Static.Host)
	}
	if strings.TrimSpace(cfg.Static.User) != "" {
		args = append(args, "--static-user", cfg.Static.User)
	}
	if strings.TrimSpace(cfg.Static.Port) != "" {
		args = append(args, "--static-port", cfg.Static.Port)
	}
	if strings.TrimSpace(cfg.Static.WorkRoot) != "" {
		args = append(args, "--static-work-root", cfg.Static.WorkRoot)
	}
	return appendProviderStopRoutingArgs(args, cfg, leaseID)
}

func runFailureDigestSSHRoutingArgs(cfg Config, leaseID string) []string {
	args := []string{}
	if strings.TrimSpace(cfg.Provider) != "" {
		args = append(args, "--provider", cfg.Provider)
	}
	if strings.TrimSpace(cfg.TargetOS) != "" {
		args = append(args, "--target", cfg.TargetOS)
	}
	if cfg.TargetOS == targetWindows && strings.TrimSpace(cfg.WindowsMode) != "" {
		args = append(args, "--windows-mode", cfg.WindowsMode)
	}
	if strings.TrimSpace(cfg.Static.Host) != "" {
		args = append(args, "--static-host", cfg.Static.Host)
	}
	if strings.TrimSpace(cfg.Static.User) != "" {
		args = append(args, "--static-user", cfg.Static.User)
	}
	if strings.TrimSpace(cfg.Static.Port) != "" {
		args = append(args, "--static-port", cfg.Static.Port)
	}
	if strings.TrimSpace(cfg.Static.WorkRoot) != "" {
		args = append(args, "--static-work-root", cfg.Static.WorkRoot)
	}
	return appendProviderStopRoutingArgs(args, cfg, leaseID)
}

func fallbackFailureDigestRoutingArgs(input runFailureDigestInput) []string {
	args := []string{}
	if strings.TrimSpace(input.Provider) != "" {
		args = append(args, "--provider", input.Provider)
	}
	if strings.TrimSpace(input.TargetOS) != "" {
		args = append(args, "--target", input.TargetOS)
	}
	if input.TargetOS == targetWindows && strings.TrimSpace(input.WindowsMode) != "" {
		args = append(args, "--windows-mode", input.WindowsMode)
	}
	return args
}

func crabboxCommandString(args []string) string {
	env := []string{}
	rest := make([]string, 0, len(args))
	index := 0
	for index < len(args) && isShellEnvAssignment(args[index]) {
		env = append(env, args[index])
		index++
	}
	if index < len(args) {
		rest = append(rest, args[index])
		index++
	}
	for index < len(args) && isShellEnvAssignment(args[index]) {
		env = append(env, args[index])
		index++
	}
	rest = append(rest, args[index:]...)
	command := append(env, "crabbox")
	command = append(command, rest...)
	return readableShellCommand(command)
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
