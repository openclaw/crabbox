package shared

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCompleteWarmupWithoutTimingJSON(t *testing.T) {
	var stdout bytes.Buffer
	err := CompleteWarmup(core.Runtime{Stdout: &stdout, Stderr: failingWriter{err: io.ErrClosedPipe}}, false, WarmupCompletion{
		Total: 1500 * time.Microsecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "warmup complete total=2ms\n"; got != want {
		t.Fatalf("stdout=%q, want %q", got, want)
	}
}

func TestCompleteWarmupWritesTimingJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := CompleteWarmup(core.Runtime{Stdout: &stdout, Stderr: &stderr}, true, WarmupCompletion{
		Provider: "nomad",
		LeaseID:  "cbx_123456789abc",
		Slug:     "timed-crab",
		Workdir:  "/workspace/crabbox",
		Total:    1500 * time.Microsecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "warmup complete total=2ms\n"; got != want {
		t.Fatalf("stdout=%q, want %q", got, want)
	}
	var report core.TimingReport
	if err := json.Unmarshal(stderr.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Provider != "nomad" || report.LeaseID != "cbx_123456789abc" || report.Slug != "timed-crab" ||
		report.TotalMs != 1 || report.ExitCode != 0 || report.Workdir != "/workspace/crabbox" ||
		report.RunStatus != core.RunStatusSucceeded || report.ErrorKind != core.RunErrorNone {
		t.Fatalf("report=%+v", report)
	}
}

func TestCompleteWarmupReturnsTimingWriterError(t *testing.T) {
	want := errors.New("timing write failed")
	err := CompleteWarmup(core.Runtime{
		Stdout: failingWriter{err: io.ErrClosedPipe},
		Stderr: failingWriter{err: want},
	}, true, WarmupCompletion{})
	if !errors.Is(err, want) {
		t.Fatalf("err=%v, want %v", err, want)
	}
}

func TestCompleteWarmupWritesTimingAfterStdoutFailure(t *testing.T) {
	writer := &recordingTimingWriter{}
	result := WarmupCompletion{
		Provider: "example-provider",
		LeaseID:  "cbx_abcdef123456",
		Slug:     "example-slug",
		Workdir:  "/workspace/example",
		Total:    2345 * time.Millisecond,
	}
	if err := CompleteWarmup(core.Runtime{
		Stdout: failingWriter{err: io.ErrClosedPipe},
		Stderr: writer,
	}, true, result); err != nil {
		t.Fatal(err)
	}
	want := core.TimingReport{
		Provider:  result.Provider,
		LeaseID:   result.LeaseID,
		Slug:      result.Slug,
		TotalMs:   result.Total.Milliseconds(),
		ExitCode:  0,
		RunStatus: core.RunStatusSucceeded,
		ErrorKind: core.RunErrorNone,
		Workdir:   result.Workdir,
	}
	if !reflect.DeepEqual(writer.report, want) {
		t.Fatalf("report=%+v, want %+v", writer.report, want)
	}
}

type failingWriter struct {
	err error
}

func (w failingWriter) Write([]byte) (int, error) {
	return 0, w.err
}

type recordingTimingWriter struct {
	report core.TimingReport
}

func (w *recordingTimingWriter) Write([]byte) (int, error) {
	return 0, errors.New("unexpected byte write")
}

func (w *recordingTimingWriter) WriteTimingReport(report core.TimingReport) error {
	w.report = report
	return nil
}
