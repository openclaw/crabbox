package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
)

func (a App) results(ctx context.Context, args []string) error {
	args, jsonAnywhere := extractBoolFlag(args, "json")
	fs := newFlagSet("results", a.Stderr)
	runIDValue, args := popLeadingRunID(args)
	runID := fs.String("id", runIDValue, "run id")
	failedOnly := fs.Bool("failed-only", false, "print only failed test cases")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *runID == "" && fs.NArg() > 0 {
		*runID = fs.Arg(0)
	}
	if *runID == "" {
		return exit(2, "usage: crabbox results <run-id>")
	}
	if jsonAnywhere {
		*jsonOut = true
	}
	coord, err := configuredCoordinator()
	if err != nil {
		return err
	}
	run, err := coord.Run(ctx, *runID)
	if err != nil {
		return err
	}
	if *jsonOut {
		if *failedOnly {
			if run.Results == nil {
				return json.NewEncoder(a.Stdout).Encode([]TestFailure{})
			}
			return json.NewEncoder(a.Stdout).Encode(nonNilTestFailures(run.Results.Failed))
		}
		return json.NewEncoder(a.Stdout).Encode(run.Results)
	}
	if run.Results == nil {
		fmt.Fprintf(a.Stdout, "no test results recorded for %s\n", *runID)
		return nil
	}
	if *failedOnly {
		printTestFailuresOnly(a.Stdout, *run.Results)
		return nil
	}
	printTestResults(a.Stdout, *run.Results)
	return nil
}

func printTestResults(out io.Writer, results TestResultSummary) {
	fmt.Fprintf(out, "results format=%s files=%d suites=%d tests=%d failures=%d errors=%d skipped=%d time=%.3fs\n",
		results.Format, len(results.Files), results.Suites, results.Tests, results.Failures, results.Errors, results.Skipped, results.TimeSeconds)
	if len(results.Failed) == 0 {
		return
	}
	fmt.Fprintln(out, "failed:")
	for _, failure := range results.Failed {
		printTestFailure(out, failure)
	}
}

func printTestFailuresOnly(out io.Writer, results TestResultSummary) {
	if len(results.Failed) == 0 {
		fmt.Fprintln(out, "no failed test cases recorded")
		return
	}
	fmt.Fprintln(out, "failed:")
	for _, failure := range results.Failed {
		printTestFailure(out, failure)
	}
}

func nonNilTestFailures(failures []TestFailure) []TestFailure {
	if failures == nil {
		return []TestFailure{}
	}
	return failures
}

func printTestFailure(out io.Writer, failure TestFailure) {
	display := terminalSafeTestFailure(failure)
	fmt.Fprintf(out, "  %s %-8s %s", display.location, display.kind, display.name)
	if display.message != "" {
		fmt.Fprintf(out, " — %s", display.message)
	}
	fmt.Fprintln(out)
}

type displayedTestFailure struct {
	location string
	kind     string
	name     string
	message  string
}

func terminalSafeTestFailure(failure TestFailure) displayedTestFailure {
	name := terminalSafeResultField(failure.Name)
	if failure.Classname != "" {
		name = terminalSafeResultField(failure.Classname) + "." + name
	}
	location := firstNonBlank(failure.File, failure.Suite, "-")
	message := strings.TrimSpace(failure.Message)
	if message != "" {
		message = terminalSafeResultField(firstLine(message))
	}
	return displayedTestFailure{
		location: terminalSafeResultField(location),
		kind:     terminalSafeResultField(failure.Kind),
		name:     name,
		message:  message,
	}
}

func terminalSafeResultField(value string) string {
	var out strings.Builder
	for _, r := range value {
		if !isTerminalControl(r) {
			out.WriteRune(r)
			continue
		}
		if r <= 0xffff {
			fmt.Fprintf(&out, `\u%04X`, r)
		} else {
			fmt.Fprintf(&out, `\U%08X`, r)
		}
	}
	return out.String()
}

func isTerminalControl(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
}

func firstLine(value string) string {
	if idx := strings.IndexByte(value, '\n'); idx >= 0 {
		return strings.TrimSpace(value[:idx])
	}
	return value
}
