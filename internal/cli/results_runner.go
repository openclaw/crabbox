package cli

import (
	"context"
	"fmt"

	"github.com/openclaw/crabbox/internal/runner"
	"github.com/openclaw/crabbox/internal/runner/runnerfs"
)

type JUnitCollection struct {
	Summary  *TestResultSummary
	Warnings []error
}

// CollectJUnitResultsWithRunner shares result policy and parsing across SSH and
// API sessions; the client supplies only an authenticated runner transport.
func CollectJUnitResultsWithRunner(ctx context.Context, client *runner.Client, workdir string, cfg ResultsConfig, marker string) (JUnitCollection, error) {
	results, err := client.Collect(ctx, workdir, appendUniqueStrings(nil, cfg.JUnit...), cfg.Auto, marker)
	if err != nil {
		return JUnitCollection{}, err
	}
	files, warnings := runnerResultFiles(results)
	if len(files) == 0 {
		return JUnitCollection{Warnings: warnings}, nil
	}
	summary, parseErr := parseJUnitResults(files)
	if parseErr != nil {
		warnings = append(warnings, parseErr)
	}
	return JUnitCollection{Summary: summary, Warnings: warnings}, nil
}

func runnerResultFiles(results runnerfs.Results) (map[string]string, []error) {
	files := make(map[string]string, len(results.Files))
	for _, file := range results.Files {
		files[file.Path] = string(file.Data)
	}
	warnings := make([]error, 0, len(results.Warnings))
	for _, warning := range results.Warnings {
		warnings = append(warnings, fmt.Errorf("skip junit %s: %s", warning.Path, warning.Message))
	}
	return files, warnings
}
