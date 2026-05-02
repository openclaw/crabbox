package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxRunEventChunkBytes = 8 * 1024

type runEventRecorder struct {
	coord    *CoordinatorClient
	runID    string
	stderr   io.Writer
	disabled atomic.Bool
	warnOnce sync.Once
}

func newRunEventRecorder(coord *CoordinatorClient, runID string, stderr io.Writer) *runEventRecorder {
	if coord == nil || runID == "" {
		return nil
	}
	return &runEventRecorder{coord: coord, runID: runID, stderr: stderr}
}

func (r *runEventRecorder) append(ctx context.Context, eventType, stream, message string, data map[string]any) {
	if r == nil || r.disabled.Load() {
		return
	}
	if _, err := r.coord.AppendRunEvent(ctx, r.runID, eventType, stream, message, data); err != nil {
		r.disabled.Store(true)
		r.warn(err)
	}
}

func (r *runEventRecorder) appendBestEffort(eventType, stream, message string, data map[string]any) {
	if r == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r.append(ctx, eventType, stream, message, data)
}

func (r *runEventRecorder) warn(err error) {
	r.warnOnce.Do(func() {
		fmt.Fprintf(r.stderr, "warning: run event append failed for %s: %v\n", r.runID, err)
	})
}

type runEventStreamer struct {
	recorder *runEventRecorder
	stream   string
	ch       chan string
	wg       sync.WaitGroup
}

func newRunEventStreamer(recorder *runEventRecorder, stream string) *runEventStreamer {
	if recorder == nil {
		return nil
	}
	s := &runEventStreamer{
		recorder: recorder,
		stream:   stream,
		ch:       make(chan string, 128),
	}
	s.wg.Add(1)
	go s.run()
	return s
}

func (s *runEventStreamer) Write(p []byte) (int, error) {
	written := len(p)
	if s == nil || len(p) == 0 {
		return written, nil
	}
	for len(p) > 0 {
		n := len(p)
		if n > maxRunEventChunkBytes {
			n = maxRunEventChunkBytes
		}
		chunk := string(append([]byte(nil), p[:n]...))
		s.ch <- chunk
		p = p[n:]
	}
	return written, nil
}

func (s *runEventStreamer) Close() {
	if s == nil {
		return
	}
	close(s.ch)
	s.wg.Wait()
}

func (s *runEventStreamer) run() {
	defer s.wg.Done()
	for chunk := range s.ch {
		s.recorder.appendBestEffort(s.stream, s.stream, chunk, nil)
	}
}

type runEventOutputWriter struct {
	local    io.Writer
	log      *runLogBuffer
	streamer *runEventStreamer
}

func (w runEventOutputWriter) Write(p []byte) (int, error) {
	if w.local != nil {
		if _, err := w.local.Write(p); err != nil {
			return 0, err
		}
	}
	if w.log != nil {
		_, _ = w.log.Write(p)
	}
	if w.streamer != nil {
		_, _ = w.streamer.Write(p)
	}
	return len(p), nil
}

func (a App) events(ctx context.Context, args []string) error {
	args, jsonAnywhere := extractBoolFlag(args, "json")
	fs := newFlagSet("events", a.Stderr)
	runID := fs.String("id", "", "run id")
	after := fs.Int64("after", 0, "only show events after this sequence")
	limit := fs.Int("limit", 500, "maximum events")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *runID == "" && fs.NArg() > 0 {
		*runID = fs.Arg(0)
	}
	if *runID == "" {
		return exit(2, "usage: crabbox events <run-id>")
	}
	if jsonAnywhere {
		*jsonOut = true
	}
	coord, err := configuredCoordinator()
	if err != nil {
		return err
	}
	events, err := coord.RunEvents(ctx, *runID, *after, *limit)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(events)
	}
	for _, event := range events {
		printRunEvent(a.Stdout, event)
	}
	return nil
}

func (a App) attach(ctx context.Context, args []string) error {
	fs := newFlagSet("attach", a.Stderr)
	runID := fs.String("id", "", "run id")
	after := fs.Int64("after", 0, "resume after this event sequence")
	poll := fs.Duration("poll", time.Second, "poll interval")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *runID == "" && fs.NArg() > 0 {
		*runID = fs.Arg(0)
	}
	if *runID == "" {
		return exit(2, "usage: crabbox attach <run-id>")
	}
	coord, err := configuredCoordinator()
	if err != nil {
		return err
	}
	nextAfter := *after
	for {
		events, err := coord.RunEvents(ctx, *runID, nextAfter, 100)
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.Seq > nextAfter {
				nextAfter = event.Seq
			}
			printAttachEvent(a.Stdout, a.Stderr, event)
		}
		if len(events) == 0 {
			run, err := coord.Run(ctx, *runID)
			if err != nil {
				return err
			}
			if run.State != "running" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(*poll):
		}
	}
}

func printRunEvent(out io.Writer, event CoordinatorRunEvent) {
	stream := event.Stream
	if stream == "" && (event.Type == "stdout" || event.Type == "stderr") {
		stream = event.Type
	}
	message := strings.TrimSpace(event.Message)
	if message != "" {
		message = " " + firstLine(message)
	}
	if stream != "" {
		stream = " stream=" + stream
	}
	fmt.Fprintf(out, "%-6d %-24s %-18s%s%s\n", event.Seq, event.CreatedAt, event.Type, stream, message)
}

func printAttachEvent(stdout, stderr io.Writer, event CoordinatorRunEvent) {
	stream := event.Stream
	if stream == "" && (event.Type == "stdout" || event.Type == "stderr") {
		stream = event.Type
	}
	switch stream {
	case "stdout":
		fmt.Fprint(stdout, event.Message)
	case "stderr":
		fmt.Fprint(stderr, event.Message)
	default:
		printRunEvent(stderr, event)
	}
}
