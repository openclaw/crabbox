package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
)

const remoteResultsMarker = "crabbox/results-start"

func collectRemoteJUnitResults(ctx context.Context, target SSHTarget, workdir string, cfg ResultsConfig, autoMarker string) (summary *TestResultSummary, err error) {
	session, err := newSSHTransportSession(ctx, target, false)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	client, err := newResolvedRunnerClient(ctx, session, target, io.Discard)
	if err != nil {
		return nil, err
	}
	collection, err := CollectJUnitResultsWithRunner(ctx, client, workdir, cfg, autoMarker)
	if err != nil {
		return nil, err
	}
	return collection.Summary, errors.Join(collection.Warnings...)
}

func remoteTouchResultsMarker(workdir string) string {
	return "cd " + shellQuote(workdir) + " && marker=.crabbox/results-start; if git_marker=$(git rev-parse --git-path " + shellQuote(remoteResultsMarker) + " 2>/dev/null); then marker=$git_marker; fi; mkdir -p \"$(dirname \"$marker\")\" && : > \"$marker\""
}

func resultSummaryLine(results *TestResultSummary) string {
	if results == nil {
		return ""
	}
	return fmt.Sprintf("test results files=%d tests=%d failures=%d errors=%d skipped=%d", len(results.Files), results.Tests, results.Failures, results.Errors, results.Skipped)
}

func failRunForTestResults(commandExitCode int, cfg ResultsConfig, results *TestResultSummary) bool {
	return commandExitCode == 0 && cfg.FailOnFailures && results != nil && (results.Failures > 0 || results.Errors > 0 || len(results.Failed) > 0)
}
