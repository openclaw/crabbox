package blacksmith

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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

// gh is an existing optional metadata integration. Use only its existing
// access, pinned to the exact native github.com URL; never acquire credentials
// or mutate GitHub. Missing access is uncertainty, not completion.
func (b *blacksmithBackend) githubRunConclusion(ctx context.Context, runURL string) (string, error) {
	parts := blacksmithActionsURL.FindStringSubmatch(runURL)
	if parts == nil || parts[2] == "." || parts[2] == ".." {
		return "", fmt.Errorf("native status has no valid exact GitHub run URL")
	}
	result, err := b.rt.Exec.Run(ctx, core.LocalCommandRequest{
		Name: "gh",
		Args: []string{"api", "--hostname", "github.com", "--method", "GET",
			"repos/" + parts[1] + "/" + parts[2] + "/actions/runs/" + parts[3],
			"--jq", "{id,html_url,status,conclusion}"},
		MaxCapturedOutputBytes: 16 * 1024,
	})
	if err = blacksmithContextError(ctx, err); err != nil || result.ExitCode != 0 {
		// Do not echo credential-bearing environment or arbitrary API diagnostics.
		if err == nil {
			err = errors.New("gh returned a nonzero exit code")
		}
		return "", blacksmithCommandError{ExitError: core.ExitError{Code: 5, Message: fmt.Sprintf("exact GitHub run read failed (gh exit=%d); existing gh access must be available", result.ExitCode)}, cause: err}
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
