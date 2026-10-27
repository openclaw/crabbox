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
	recoverPath := fs.String("recover", "", "submit an original pending signed finish file without rerunning work")
	expectedFinish := fs.String("expected-finish-sha256", "", "required sha256 of the original finish request for recovery")
	expectedSigner := fs.String("expected-signer", "", "required sha256 signer fingerprint")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *runID == "" && fs.NArg() > 0 {
		*runID = fs.Arg(0)
	}
	if *runID == "" {
		return Exit(2, "usage: crabbox receipt <run-id>")
	}
	if signer := strings.TrimSpace(*expectedSigner); signer != "" && !validSHA256Digest(signer) {
		return Exit(2, "--expected-signer must be a sha256 fingerprint")
	}
	coord, err := configuredCoordinator()
	if err != nil {
		return err
	}
	var receipt terminalRunReceipt
	if *recoverPath != "" {
		receipt, err = recoverPendingRunFinish(ctx, coord, *runID, *recoverPath, *expectedSigner, *expectedFinish)
	} else {
		if *expectedFinish != "" {
			return Exit(2, "--expected-finish-sha256 requires --recover")
		}
		receipt, err = readVerifiedTerminalReceipt(ctx, coord, *runID, *expectedSigner)
	}
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(a.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(receipt); err != nil {
		return fmt.Errorf("encode receipt: %w", err)
	}
	return nil
}

func readVerifiedTerminalReceipt(ctx context.Context, coord *CoordinatorClient, runID, expectedSigner string) (terminalRunReceipt, error) {
	var empty terminalRunReceipt
	run, err := coord.Run(ctx, runID)
	if err != nil {
		return empty, err
	}
	if run.ExitCode == nil || strings.TrimSpace(run.EndedAt) == "" {
		return empty, Exit(4, "run %s has no committed terminal evidence", runID)
	}
	receipt, err := coord.RunReceipt(ctx, runID)
	if err != nil {
		if isCoordinatorNotFoundError(err) {
			return empty, Exit(4, "run %s has no committed terminal receipt; execution remains ambiguous", runID)
		}
		return empty, err
	}
	logText, err := coord.RunLogs(ctx, runID)
	if err != nil {
		return empty, err
	}
	startedAt, err := time.Parse(time.RFC3339Nano, run.StartedAt)
	if err != nil {
		return empty, Exit(2, "run %s has invalid startedAt", runID)
	}
	endedAt, err := time.Parse(time.RFC3339Nano, run.EndedAt)
	if err != nil {
		return empty, Exit(2, "run %s has invalid endedAt", runID)
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
		return empty, Exit(1, "receipt verification failed for %s: %v", runID, err)
	}
	if signer := strings.TrimSpace(expectedSigner); signer != "" && receipt.Signer != signer {
		return empty, Exit(1, "receipt signer mismatch for %s: got %s want %s", runID, receipt.Signer, signer)
	}
	return receipt, nil
}
