package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// An empty source selects the unchanged coordinator wire representation.
func resolveHistorySource(source string) (string, *CoordinatorClient, error) {
	switch source {
	case "local":
		return source, nil, nil
	case "", "coordinator", "all":
	default:
		return "", nil, Exit(2, "source must be local, coordinator, or all")
	}
	cfg, err := loadConfig()
	if err != nil {
		if source == "all" {
			return source, nil, err
		}
		return "", nil, err
	}
	coord, ok, err := newCoordinatorClient(cfg)
	if err != nil {
		if source == "all" {
			return source, nil, err
		}
		return "", nil, err
	}
	if source == "all" {
		return source, coord, nil
	}
	if ok {
		return source, coord, nil
	}
	if source == "" {
		available, err := localHistoryAvailable()
		if err != nil {
			return "", nil, err
		}
		if available {
			return "local", nil, nil
		}
	}
	return "", nil, Exit(2, "command requires a configured coordinator (or existing local history)")
}

type historyReadRow struct {
	renderResults *TestResultSummary
	Source        string              `json:"source"`
	Local         *localHistoryRecord `json:"local,omitempty"`
	Coordinator   *CoordinatorRun     `json:"coordinator,omitempty"`
	Log           *string             `json:"log,omitempty"`
	Results       *TestResultSummary  `json:"results,omitempty"`
	Failed        []TestFailure       `json:"failed,omitempty"`
}

type historyReadEnvelope struct {
	Records []historyReadRow  `json:"records"`
	Errors  map[string]string `json:"errors,omitempty"`
}

func (e *historyReadEnvelope) failure(source string, err error) {
	if e.Errors == nil {
		e.Errors = make(map[string]string)
	}
	e.Errors[source] = err.Error()
}

func (a App) finishHistoryRead(e historyReadEnvelope, jsonOut bool) error {
	if jsonOut {
		if err := json.NewEncoder(a.Stdout).Encode(e); err != nil {
			return err
		}
	} else {
		for _, source := range []string{"local", "coordinator"} {
			if message, ok := e.Errors[source]; ok {
				fmt.Fprintf(a.Stderr, "%s history: %s\n", source, message)
			}
		}
	}
	if len(e.Errors) != 0 {
		return Exit(1, "one or more requested history sources failed")
	}
	return nil
}

func (a App) historyFromSources(ctx context.Context, source string, coord *CoordinatorClient, filter localHistoryFilter, owner, org string, jsonOut bool, sourceErr error) error {
	if filter.Limit <= 0 {
		return Exit(2, "limit must be positive")
	}
	e := historyReadEnvelope{Records: []historyReadRow{}}
	if source != "coordinator" {
		// Local records contain no authenticated actor attribution.
		if owner != "" || org != "" {
			e.failure("local", errors.New("owner and org filters require coordinator attribution"))
		} else {
			records, err := listLocalHistory(filter)
			if err != nil {
				e.failure("local", err)
			} else {
				for i := range records {
					e.Records = append(e.Records, historyReadRow{Source: "local", Local: &records[i]})
				}
			}
		}
	}
	if source != "local" {
		if coord == nil {
			if sourceErr == nil {
				sourceErr = errors.New("coordinator is not configured")
			}
			e.failure("coordinator", sourceErr)
		} else {
			runs, err := coord.Runs(ctx, filter.LeaseID, owner, org, filter.State, filter.Limit)
			if err != nil {
				e.failure("coordinator", err)
			} else {
				for i := range runs {
					matched := false
					for j := range e.Records {
						local := e.Records[j].Local
						if local != nil && local.CoordinatorState == "recorded" && local.CoordinatorRunID == runs[i].ID && local.CoordinatorReference != "" && local.CoordinatorReference == coordinatorHistoryReference(coord.BaseURL) {
							e.Records[j].Source = "both"
							e.Records[j].Coordinator = &runs[i]
							matched = true
							break
						}
					}
					if !matched {
						e.Records = append(e.Records, historyReadRow{Source: "coordinator", Coordinator: &runs[i]})
					}
				}
			}
		}
	}
	sort.SliceStable(e.Records, func(i, j int) bool { return historyRowBefore(e.Records[i], e.Records[j]) })
	if len(e.Records) > filter.Limit {
		e.Records = e.Records[:filter.Limit]
	}
	if !jsonOut {
		for _, row := range e.Records {
			if r := row.Local; r != nil {
				fmt.Fprintf(a.Stdout, "source=%s provenance=%s coordinator=%s %s lease=%s state=%s phase=%s exit=%s duration=%s started=%s command=%s\n", row.Source, terminalSafeResultField(r.Source), terminalSafeResultField(r.CoordinatorState), terminalSafeResultField(r.ID), terminalSafeResultField(blank(r.LeaseID, "-")), r.RecordingState, r.Phase, formatRunExit(r.ExitCode), formatMs(r.TotalMs), r.StartedAt, terminalSafeResultField(r.CommandDisplay))
				printImageEvidence(a.Stdout, r.ImageEvidence)
			} else {
				r := row.Coordinator
				fmt.Fprintf(a.Stdout, "source=coordinator %s lease=%s state=%s phase=%s exit=%s duration=%s started=%s\n", r.ID, blank(r.LeaseID, "-"), r.State, r.Phase, formatRunExit(r.ExitCode), formatMs(r.DurationMs), r.StartedAt)
			}
		}
	}
	return a.finishHistoryRead(e, jsonOut)
}

func historyRowStart(row historyReadRow) string {
	if row.Local != nil {
		return row.Local.StartedAt
	}
	return row.Coordinator.StartedAt
}

// Compare instants, not their RFC3339 spellings; invalid remote timestamps sort last.
func historyRowBefore(a, b historyReadRow) bool {
	at, ae := time.Parse(time.RFC3339Nano, historyRowStart(a))
	bt, be := time.Parse(time.RFC3339Nano, historyRowStart(b))
	if (ae == nil) != (be == nil) {
		return ae == nil
	}
	if ae == nil && !at.Equal(bt) {
		return at.After(bt)
	}
	aid, bid := historyRowID(a), historyRowID(b)
	if aid != bid {
		return aid < bid
	}
	return a.Source < b.Source
}

func historyRowID(row historyReadRow) string {
	if row.Local != nil {
		return row.Local.ID
	}
	return row.Coordinator.ID
}

func (a App) localHistoryRead(ctx context.Context, source string, coord *CoordinatorClient, id, kind string, tail int, failedOnly, jsonOut bool, sourceErr error) error {
	e := historyReadEnvelope{Records: []historyReadRow{}}
	if source != "coordinator" {
		record, log, results, err := readLocalHistory(id)
		if err != nil {
			e.failure("local", err)
		} else {
			row := historyReadRow{Source: "local", Local: &record, Results: results, renderResults: results}
			if kind == "logs" {
				if tail > 0 {
					log = tailLogLines(log, tail)
				}
				row.Log = &log
				row.Results = nil
			}
			if kind == "results" && failedOnly {
				row.Results = nil
				row.Failed = []TestFailure{}
				if results != nil {
					row.Failed = nonNilTestFailures(results.Failed)
				}
			}
			e.Records = append(e.Records, row)
		}
	}
	if source != "local" {
		if coord == nil {
			if sourceErr == nil {
				sourceErr = errors.New("coordinator is not configured")
			}
			e.failure("coordinator", sourceErr)
		} else {
			run, err := coord.Run(ctx, id)
			if err != nil {
				e.failure("coordinator", err)
			} else {
				row := historyReadRow{Source: "coordinator", Coordinator: &run, Results: run.Results, renderResults: run.Results}
				if kind == "logs" {
					log, err := coord.RunLogs(ctx, id)
					if err != nil {
						e.failure("coordinator", err)
					} else {
						if tail > 0 {
							log = tailLogLines(log, tail)
						}
						row.Log = &log
					}
					row.Results = nil
				}
				if kind == "results" && failedOnly {
					row.Results = nil
					row.Failed = []TestFailure{}
					if run.Results != nil {
						row.Failed = nonNilTestFailures(run.Results.Failed)
					}
				}
				e.Records = append(e.Records, row)
			}
		}
	}
	if !jsonOut {
		for _, row := range e.Records {
			fmt.Fprintf(a.Stderr, "source=%s run=%s\n", row.Source, terminalSafeResultField(id))
			if row.Local != nil {
				a.printLocalHistoryAvailability(*row.Local, kind)
			}
			if kind == "logs" {
				if row.Log != nil {
					fmt.Fprint(a.Stdout, *row.Log)
				}
				continue
			}
			results := row.renderResults
			if results == nil {
				fmt.Fprintf(a.Stdout, "no test results recorded for %s\n", terminalSafeResultField(id))
			} else if failedOnly {
				printTestFailuresOnly(a.Stdout, *results)
			} else {
				printTestResults(a.Stdout, *results)
			}
		}
	}
	return a.finishHistoryRead(e, jsonOut)
}

func (a App) printLocalHistoryAvailability(r localHistoryRecord, kind string) {
	fmt.Fprintf(a.Stderr, "local provenance=%s coordinator=%s\n", terminalSafeResultField(r.Source), terminalSafeResultField(r.CoordinatorState))
	if kind == "logs" {
		fmt.Fprintf(a.Stderr, "local recording=%s scope=%s stdout=%s stderr=%s truncated=%t\n", r.RecordingState, r.CaptureScope, r.Stdout.Availability, r.Stderr.Availability, r.LogTruncated)
		for _, stream := range []struct {
			name  string
			value localHistoryStream
		}{{"stdout", r.Stdout}, {"stderr", r.Stderr}} {
			if stream.value.Reason != "" {
				fmt.Fprintf(a.Stderr, "%s: %s\n", stream.name, terminalSafeResultField(stream.value.Reason))
			}
		}
	} else {
		fmt.Fprintf(a.Stderr, "local results=%s detailsTruncated=%t omittedFiles=%d omittedFailures=%d clippedFields=%d\n", r.ResultsState, r.ResultsDetailsTruncated, r.ResultsOmittedFiles, r.ResultsOmittedFailures, r.ResultsClippedFields)
	}
}

func (a App) localHistoryMaintenance(args []string) error {
	action := args[0]
	fs := newFlagSet("history "+action, a.Stderr)
	source := fs.String("source", "local", "record source (local only)")
	jsonOut := fs.Bool("json", false, "print JSON")
	id, rest := popLeadingRunID(args[1:])
	if err := parseFlags(fs, rest); err != nil {
		return err
	}
	if *source != "local" {
		return Exit(2, "history %s supports only --source local", action)
	}
	positionals := fs.Args()
	if id != "" {
		positionals = append([]string{id}, positionals...)
	}
	if len(positionals) > 1 {
		return Exit(2, "history %s accepts at most one run id", action)
	}
	if len(positionals) == 1 {
		id = positionals[0]
	}
	if action == "prune" {
		if id != "" {
			return Exit(2, "usage: crabbox history prune [--source local]")
		}
		n, err := pruneLocalHistory()
		if err != nil {
			return err
		}
		if *jsonOut {
			return json.NewEncoder(a.Stdout).Encode(map[string]any{"source": "local", "pruned": n})
		}
		fmt.Fprintf(a.Stdout, "pruned %d local history records\n", n)
		return nil
	}
	if id == "" {
		return Exit(2, "usage: crabbox history delete <run-id> [--source local]")
	}
	if err := deleteLocalHistory(id); err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(map[string]any{"source": "local", "deleted": id})
	}
	fmt.Fprintf(a.Stdout, "deleted local history %s\n", terminalSafeResultField(id))
	return nil
}
