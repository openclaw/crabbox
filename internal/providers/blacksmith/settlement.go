package blacksmith

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"syscall"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Native status supplies the association. Never discover a run by listing,
// timestamps, workflow names, or the caller's current repository.
var blacksmithActionsURL = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_-]+)/([A-Za-z0-9_.-]+)/actions/runs/([1-9][0-9]*)$`)

const blacksmithStatusReadTimeout = 10 * time.Second

// Status is observational: it needs no claim and never finalizes local custody.
// A terminal run is useful evidence only while its native association is stable.
func (b *blacksmithBackend) statusSettlement(ctx context.Context, identity blacksmithIdentity) map[string]any {
	metadata := map[string]any{"remoteSettlement": "unknown"}
	runURL := ""
	if blacksmithObserveRunURL(&runURL, identity.RunURL) != nil || runURL == "" {
		return metadata
	}
	metadata["runURL"] = runURL
	if !identity.terminal() {
		metadata["remoteSettlement"] = "pending"
		return metadata
	}
	conclusion, err := b.githubRunConclusion(ctx, runURL)
	if err != nil {
		return metadata
	}
	if conclusion == "" {
		metadata["remoteSettlement"] = "pending"
		return metadata
	}
	confirmed, err := b.inspectTestbox(ctx, identity.ID)
	if err != nil || confirmed != identity {
		return metadata
	}
	metadata["remoteSettlement"] = "complete"
	metadata["runConclusion"] = conclusion
	return metadata
}

func blacksmithObserveRunURL(previous *string, current string) error {
	if *previous != "" && *previous != current {
		return fmt.Errorf("native GitHub run association changed or disappeared")
	}
	if current != "" {
		parts := blacksmithActionsURL.FindStringSubmatch(current)
		if parts == nil || parts[2] == "." || parts[2] == ".." {
			return fmt.Errorf("native GitHub run association is invalid")
		}
		*previous = current
	}
	return nil
}

func blacksmithSettlementError(id string, err error) error {
	return blacksmithCommandError{ExitError: core.ExitError{Code: 5, Message: fmt.Sprintf("Blacksmith GitHub settlement unresolved for %s; retaining claim/key for retry with the same organization/API: %v", id, err)}, cause: err}
}

func blacksmithCommandCause(err error) string {
	var exitErr *exec.ExitError
	var startErr *exec.Error
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &exitErr):
		return "exit"
	case errors.As(err, &startErr):
		return "start"
	default:
		return "other"
	}
}

func blacksmithCommandDiagnostic(ctx context.Context, operation string, issued time.Time, elapsed time.Duration, contextAfter error, result core.LocalCommandResult, rawErr error) string {
	remaining := "unknown"
	if deadline, ok := ctx.Deadline(); ok {
		remaining = fmt.Sprint(deadline.Sub(issued).Milliseconds())
	}
	// The runner records cancellation at the child watcher. A deadline observed
	// after return is a separate fact, not evidence that it stopped the child.
	cause, contextCause := blacksmithCommandCause(rawErr), blacksmithCommandCause(contextAfter)
	process, signal := "unknown", "unknown"
	var exitErr *exec.ExitError
	if errors.As(rawErr, &exitErr) && exitErr != nil && exitErr.ProcessState != nil {
		if status, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			process, signal = "signaled", fmt.Sprint(int(status.Signal()))
		} else if exitErr.ProcessState.Exited() {
			process, signal = "exited", "none"
		}
	}
	// Only typed, allowlisted facts cross the display boundary. Raw errors and
	// captured API output may contain private paths or credentials.
	return fmt.Sprintf("%s exit=%d cause=%s process=%s signal=%s issued_at=%s elapsed_ms=%d remaining_budget_ms=%s context_after=%s",
		operation, result.ExitCode, cause, process, signal, issued.UTC().Format(time.RFC3339Nano), elapsed.Milliseconds(), remaining, contextCause)
}

func (b *blacksmithBackend) githubCommand(ctx context.Context, args []string) (core.LocalCommandResult, error, string) {
	issued := time.Now()
	result, rawErr := b.rt.Exec.Run(ctx, core.LocalCommandRequest{
		Name: "gh", Args: args, MaxCapturedOutputBytes: 16 * 1024,
	})
	elapsed, contextAfter := time.Since(issued), ctx.Err()
	err := blacksmithContextError(ctx, rawErr)
	if err == nil && result.ExitCode == 0 {
		return result, nil, ""
	}
	if err == nil {
		err = errors.New("gh returned a nonzero exit code")
	}
	return result, err, blacksmithCommandDiagnostic(ctx, "gh", issued, elapsed, contextAfter, result, rawErr)
}

// Use only existing gh access, pinned to the exact native github.com URL.
// Missing access is uncertainty, not completion. Status remains read-only;
// claim-fenced stop can also cancel this exact associated workflow.
func (b *blacksmithBackend) githubRunConclusion(ctx context.Context, runURL string) (string, error) {
	parts := blacksmithActionsURL.FindStringSubmatch(runURL)
	if parts == nil || parts[2] == "." || parts[2] == ".." {
		return "", fmt.Errorf("native status has no valid exact GitHub run URL")
	}
	result, err, diagnostic := b.githubCommand(ctx, []string{"api", "--hostname", "github.com", "--method", "GET",
		"repos/" + parts[1] + "/" + parts[2] + "/actions/runs/" + parts[3],
		"--jq", "{id,html_url,status,conclusion}"})
	if err != nil {
		return "", blacksmithCommandError{ExitError: core.ExitError{Code: 5, Message: fmt.Sprintf("exact GitHub run read failed (%s); existing gh access must be available", diagnostic)}, cause: err}
	}
	var run struct {
		ID         json.Number `json:"id"`
		URL        string      `json:"html_url"`
		Status     string      `json:"status"`
		Conclusion string      `json:"conclusion"`
	}
	if json.Unmarshal([]byte(result.Stdout), &run) != nil || run.ID.String() != parts[3] || run.URL != runURL {
		return "", fmt.Errorf("GitHub response does not identify the exact associated run")
	}
	if run.Status != "completed" {
		switch run.Status {
		case "queued", "in_progress", "waiting", "requested", "pending":
			return "", nil
		default:
			return "", fmt.Errorf("GitHub returned an unsupported run state")
		}
	}
	switch run.Conclusion {
	case "success", "failure", "cancelled", "neutral", "skipped", "timed_out", "action_required", "stale", "startup_failure":
		return run.Conclusion, nil
	default:
		return "", fmt.Errorf("GitHub completion has no supported conclusion")
	}
}

func (b *blacksmithBackend) verifySettlement(ctx context.Context, claim core.LeaseClaim, runURL string) (string, error) {
	identity, err := b.verifyTestbox(ctx, claim)
	if err != nil {
		return "", err
	}
	if !identity.terminal() {
		return "", blacksmithSettlementError(claim.LeaseID, fmt.Errorf("native state=%s is not completed", identity.State))
	}
	if err := blacksmithObserveRunURL(&runURL, identity.RunURL); err != nil {
		return "", blacksmithSettlementError(claim.LeaseID, err)
	}
	conclusion, err := b.githubRunConclusion(ctx, runURL)
	if err != nil {
		return "", blacksmithSettlementError(claim.LeaseID, err)
	}
	if conclusion == "" {
		return "", blacksmithSettlementError(claim.LeaseID, fmt.Errorf("associated GitHub run is not completed"))
	}
	return conclusion, nil
}

// The caller holds the unchanged claim fence. Refresh native identity after the
// preceding GitHub read and native stop, immediately before requesting cancellation.
func (b *blacksmithBackend) cancelGitHubRun(ctx context.Context, claim core.LeaseClaim, runURL string) (bool, error) {
	identity, err := b.verifyTestbox(ctx, claim)
	if err != nil {
		return false, err
	}
	if !identity.terminal() {
		return false, fmt.Errorf("native Testbox is not completed; retaining claim")
	}
	if err := blacksmithObserveRunURL(&runURL, identity.RunURL); err != nil {
		return false, err
	}
	parts := blacksmithActionsURL.FindStringSubmatch(runURL)
	if parts == nil {
		return false, fmt.Errorf("native status has no exact GitHub run to cancel")
	}
	if err := context.Cause(ctx); err != nil {
		return false, err
	}
	_, err, diagnostic := b.githubCommand(ctx, []string{"api", "--hostname", "github.com", "--method", "POST",
		"repos/" + parts[1] + "/" + parts[2] + "/actions/runs/" + parts[3] + "/cancel"})
	if err == nil {
		return true, nil // Accepted is not settled: the caller still verifies terminal state.
	}
	// Native stop may already have initiated cancellation. Preserve a rejected
	// request, but let the existing bounded terminal checks reconcile that race.
	return true, blacksmithCommandError{ExitError: core.ExitError{
		Code:    core.ExitCodeForError(context.Cause(ctx), 1),
		Message: fmt.Sprintf("exact GitHub run cancellation failed (%s); existing gh access must permit cancelling the owned workflow", diagnostic),
	}, cause: err}
}
