package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (a App) receipt(ctx context.Context, args []string) error {
	fs := newFlagSet("receipt", a.Stderr)
	runIDValue, args := popLeadingRunID(args)
	runID := fs.String("id", runIDValue, "run id")
	expectedSigner := fs.String("expected-signer", "", "required sha256 signer fingerprint")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *runID == "" && fs.NArg() > 0 {
		*runID = fs.Arg(0)
	}
	if *runID == "" {
		return exit(2, "usage: crabbox receipt <run-id>")
	}
	if signer := strings.TrimSpace(*expectedSigner); signer != "" && !validSHA256Digest(signer) {
		return exit(2, "--expected-signer must be a sha256 fingerprint")
	}
	coord, err := configuredCoordinator()
	if err != nil {
		return err
	}
	run, err := coord.Run(ctx, *runID)
	if err != nil {
		return err
	}
	if run.ExitCode == nil || strings.TrimSpace(run.EndedAt) == "" {
		return exit(4, "run %s has no committed terminal evidence", *runID)
	}
	receipt, err := coord.RunReceipt(ctx, *runID)
	if err != nil {
		if isCoordinatorNotFoundError(err) {
			return exit(4, "run %s has no committed terminal receipt; execution remains ambiguous", *runID)
		}
		return err
	}
	logText, err := coord.RunLogs(ctx, *runID)
	if err != nil {
		return err
	}
	startedAt, err := time.Parse(time.RFC3339Nano, run.StartedAt)
	if err != nil {
		return exit(2, "run %s has invalid startedAt", *runID)
	}
	endedAt, err := time.Parse(time.RFC3339Nano, run.EndedAt)
	if err != nil {
		return exit(2, "run %s has invalid endedAt", *runID)
	}
	retainedDigest := sha256Digest([]byte(logText))
	fullDigest := retainedDigest
	if run.LogTruncated {
		fullDigest = ""
	}
	if err := verifyTerminalRunReceipt(receipt, terminalRunReceiptInput{
		Provider:          run.Provider,
		LeaseID:           run.LeaseID,
		Slug:              run.Slug,
		RunID:             run.ID,
		Command:           run.Command,
		ExitCode:          *run.ExitCode,
		SyncMs:            run.SyncMs,
		CommandMs:         run.CommandMs,
		StartedAt:         startedAt,
		EndedAt:           endedAt,
		LogSHA256:         fullDigest,
		RetainedLogSHA256: retainedDigest,
		LogTruncated:      run.LogTruncated,
	}); err != nil {
		return exit(1, "receipt verification failed for %s: %v", *runID, err)
	}
	if signer := strings.TrimSpace(*expectedSigner); signer != "" && receipt.Signer != signer {
		return exit(1, "receipt signer mismatch for %s: got %s want %s", *runID, receipt.Signer, signer)
	}
	encoder := json.NewEncoder(a.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(receipt); err != nil {
		return fmt.Errorf("encode receipt: %w", err)
	}
	return nil
}
