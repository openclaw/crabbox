package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"

	"golang.org/x/term"
)

var listIsTerminal = func(w io.Writer) bool {
	file, ok := w.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func renderListHeader(w io.Writer) error {
	_, err := fmt.Fprintf(w, "%-20s %-28s %-12s %-14s %-15s\n", "ID", "NAME", "STATE", "TYPE", "IP")
	return err
}

func visitListJSON(view any, visit func(any) error) error {
	if view == nil {
		return nil
	}
	entries := reflect.ValueOf(view)
	if entries.Kind() != reflect.Slice && entries.Kind() != reflect.Array {
		return fmt.Errorf("provider list response is not an array")
	}
	for i := 0; i < entries.Len(); i++ {
		if err := visit(entries.Index(i).Interface()); err != nil {
			return err
		}
	}
	return nil
}

func writeListJSON(w io.Writer, view any, lines bool) error {
	encoder := json.NewEncoder(w)
	if lines {
		return visitListJSON(view, encoder.Encode)
	}
	return encoder.Encode(nonNullJSONCollection(view))
}

func (a App) renderStreamingList(ctx context.Context, cfg Config, backend Backend, streaming StreamingListBackend, req ListRequest, pond string, jsonOut, jsonlOut bool) error {
	if !jsonOut && !jsonlOut {
		if err := renderListHeader(a.Stdout); err != nil {
			return err
		}
	}
	tty := listIsTerminal(a.Stderr)
	progress := false
	clearProgress := func() {
		if progress {
			fmt.Fprint(a.Stderr, "\r\x1b[K")
			progress = false
		}
	}
	defer clearProgress()
	rows := 0
	section := ""
	buffered := make([]any, 0)
	err := streaming.StreamList(ctx, req, func(page ListPage) error {
		clearProgress()
		if pond != "" {
			page.Servers = filterServersByPond(page.Servers, pond)
			page.JSON = filterJSONListViewByPond(page.JSON, pond)
		}
		if jsonOut {
			if err := visitListJSON(page.JSON, func(entry any) error {
				buffered = append(buffered, entry)
				return nil
			}); err != nil {
				return err
			}
		} else if jsonlOut {
			if err := writeListJSON(a.Stdout, page.JSON, true); err != nil {
				return err
			}
		} else {
			if len(page.Servers) > 0 && page.Section != "" && section != page.Section {
				if _, err := fmt.Fprintln(a.Stdout, "\n"+page.Section); err != nil {
					return err
				}
				section = page.Section
			}
			if err := renderServerList(a.Stdout, page.Servers); err != nil {
				return err
			}
		}
		rows += len(page.Servers)
		if tty && page.More {
			fmt.Fprintf(a.Stderr, "\rlist: %d rows; fetching more pages…", rows)
			progress = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	clearProgress()
	if !jsonOut && !jsonlOut && !req.Retained && listIsTerminal(a.Stdout) {
		fmt.Fprintln(a.Stderr, "\x1b[2mretained leases hidden; use --retained to include them\x1b[0m")
	}
	a.syncExternalRunnersBestEffort(ctx, cfg, backend)
	if jsonOut {
		// Buffered lease JSON keeps its newest-first contract even when the
		// live-state queries were drained in display order. Native inventory
		// payloads retain their provider's ordering.
		leaseView := true
		for _, entry := range buffered {
			if _, ok := entry.(CoordinatorLease); !ok {
				leaseView = false
				break
			}
		}
		if leaseView {
			slices.SortStableFunc(buffered, func(a, b any) int {
				return strings.Compare(b.(CoordinatorLease).CreatedAt, a.(CoordinatorLease).CreatedAt)
			})
		}
		return writeListJSON(a.Stdout, buffered, false)
	}
	return nil
}
