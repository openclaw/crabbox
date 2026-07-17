package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowResponseWriter is an http.ResponseWriter whose Write blocks briefly,
// simulating a downstream HTTP client that applies backpressure (exactly what
// eventWriter.write experiences when streaming NDJSON to a slow Cloudflare
// edge/client). It records everything written.
type slowResponseWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
	// delay is the per-Write sleep. Zero means the default 2ms. Set once
	// before the writer is handed to runCommand (i.e. before any copyPipe
	// goroutine can observe it), so no synchronization is needed to read it.
	delay time.Duration
}

func (w *slowResponseWriter) Header() http.Header { return http.Header{} }

func (w *slowResponseWriter) WriteHeader(int) {}

func (w *slowResponseWriter) Write(p []byte) (int, error) {
	// Simulate a slow consumer. This delay happens INSIDE eventWriter.write,
	// i.e. inside the copyPipe goroutine, while the child keeps producing.
	delay := w.delay
	if delay == 0 {
		delay = 2 * time.Millisecond
	}
	time.Sleep(delay)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *slowResponseWriter) body() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestRunCommandDeliversAllStdout proves the cmd.Wait()-before-wg.Wait()
// ordering bug in runCommand (main.go:204-205).
//
// The child writes exactly totalBytes of stdout and exits. Per os/exec docs,
// cmd.Wait() closes the parent read-ends of StdoutPipe/StderrPipe as soon as
// the child is reaped — "it is incorrect to call Wait before all reads from
// the pipe have completed". Because runCommand calls cmd.Wait() BEFORE
// wg.Wait(), any bytes still sitting in the kernel pipe buffer when the child
// exits are discarded; the copyPipe goroutine's next Read returns
// os.ErrClosed, which benignPipeReadError treats as EOF, so the tail of the
// output is silently dropped with no error event.
//
// With correct ordering (wg.Wait() before cmd.Wait()) this test passes.
func TestRunCommandDeliversAllStdout(t *testing.T) {
	const totalBytes = 1 << 20 // 1 MiB — far larger than any kernel pipe buffer

	rec := &slowResponseWriter{}
	writer := &eventWriter{w: rec}

	// Emit exactly totalBytes of 'x' on stdout, then exit immediately.
	req := execRequest{
		Command: fmt.Sprintf("head -c %d /dev/zero | tr '\\0' 'x'", totalBytes),
	}

	code, err := runCommand(context.Background(), req, t.TempDir(), writer)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	var got int
	var errEvents []string
	scanner := bufio.NewScanner(bytes.NewBufferString(rec.body()))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<21)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("unmarshal event %q: %v", line, err)
		}
		switch ev.Type {
		case "stdout":
			got += len(ev.Data)
		case "error":
			errEvents = append(errEvents, ev.Error)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	if len(errEvents) > 0 {
		t.Fatalf("unexpected error events: %v", errEvents)
	}
	if got != totalBytes {
		t.Fatalf("stdout bytes delivered = %d, want %d (lost %d bytes: cmd.Wait() closed the pipe before copyPipe drained it, and os.ErrClosed was masked as EOF)",
			got, totalBytes, totalBytes-got)
	}
}

// TestRunCommandDeliversAllStdoutForLongForegroundCommand proves runCommand
// does not truncate the output of a FOREGROUND command that runs LONGER than
// pipeDrainGrace and writes its result right before exiting.
//
// This is the hole the earlier "bounded select" design had: it started the
// pipeDrainGrace timer at the select, BEFORE the child exited, so the grace
// bounded TOTAL runtime. A legit foreground command running past the grace
// caused the select to fall through mid-run and call cmd.Wait, which closes the
// StdoutPipe read-ends the moment the child is reaped — racing the final copy
// and intermittently truncating the tail (the exact class of bug we fixed).
//
// The current design owns the pipe read-ends, so cmd.Wait leaves them open and
// the full output is delivered no matter how long the command runs. The grace
// is lowered here so the test stays fast while still outliving it.
func TestRunCommandDeliversAllStdoutForLongForegroundCommand(t *testing.T) {
	// A slow consumer keeps copyPipe behind the child, so if cmd.Wait ever
	// closed the read-ends there would be unread buffered bytes to lose.
	orig := pipeDrainGrace
	pipeDrainGrace = 1 * time.Second
	defer func() { pipeDrainGrace = orig }()

	const tail = 1 << 20 // 1 MiB tail emitted just before the command exits
	rec := &slowResponseWriter{}
	writer := &eventWriter{w: rec}

	// sleep 2 (> 1s grace); then emit a large tail and a marker, then exit. On
	// the old bounded-select design the grace expires at 1s while the command is
	// still sleeping, and the tail written at ~2s is truncated.
	req := execRequest{
		Command: fmt.Sprintf("sleep 2; head -c %d /dev/zero | tr '\\0' 'x'; printf 'RESULT_MARKER'", tail),
	}

	code, err := runCommand(context.Background(), req, t.TempDir(), writer)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	var gotBytes int
	var stdout strings.Builder
	var errEvents []string
	scanner := bufio.NewScanner(bytes.NewBufferString(rec.body()))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<21)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("unmarshal event %q: %v", line, err)
		}
		switch ev.Type {
		case "stdout":
			gotBytes += len(ev.Data)
			stdout.WriteString(ev.Data)
		case "error":
			errEvents = append(errEvents, ev.Error)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	if len(errEvents) > 0 {
		t.Fatalf("unexpected error events: %v", errEvents)
	}
	wantBytes := tail + len("RESULT_MARKER")
	if gotBytes != wantBytes {
		t.Fatalf("stdout bytes delivered = %d, want %d (lost %d bytes: a foreground command outlived the drain grace and its tail was truncated)",
			gotBytes, wantBytes, wantBytes-gotBytes)
	}
	if !strings.Contains(stdout.String(), "RESULT_MARKER") {
		t.Fatalf("stdout missing RESULT_MARKER: the final output of a long foreground command was truncated")
	}
}

// TestRunCommandReturnsPromptlyWithBackgroundDescendant proves runCommand
// returns PROMPTLY — near pipeDrainIdle, not the full pipeDrainGrace — for a
// background descendant that inherits the stdout/stderr write-end and
// outlives the direct child but never writes anything more itself.
//
// The script echoes "done" on the foreground and spawns `sleep 30 &`. The
// direct child (bash) exits immediately after echoing, but the backgrounded
// sleep inherits the pipe write-end, so the parent read-ends never reach EOF
// until the sleep exits ~30s later. A fixed-grace drain (wait the full
// pipeDrainGrace no matter what, then close) would therefore make every such
// request pay the full grace latency (measured: ~5.1s) even though there is
// nothing left to drain — a real regression versus the pre-owned-pipe
// baseline (~0.2s). The idle-aware drain detects that `copied` stops
// advancing almost immediately (the silent descendant writes nothing) and
// closes the read-ends after pipeDrainIdle instead of pipeDrainGrace.
//
// This assertion FAILS against the fixed-grace design (elapsed ~= grace) and
// PASSES against the idle-aware drain (elapsed ~= pipeDrainIdle).
func TestRunCommandReturnsPromptlyWithBackgroundDescendant(t *testing.T) {
	rec := &slowResponseWriter{}
	writer := &eventWriter{w: rec}

	// Foreground writes "done" and exits; the descendant sleep keeps the
	// inherited write-end open far longer than the drain grace, but (unlike
	// the noisy-descendant test below) never writes anything itself.
	req := execRequest{
		Command: "sleep 30 & echo done",
	}

	start := time.Now()
	code, err := runCommand(context.Background(), req, t.TempDir(), writer)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	// Must return WELL under pipeDrainGrace — the whole point of the
	// idle-aware drain is that a quiet descendant does not force the full
	// grace latency. Half the grace leaves generous slack for a loaded
	// machine while still failing hard against the fixed-grace regression.
	maxElapsed := pipeDrainGrace / 2
	if elapsed > maxElapsed {
		t.Fatalf("runCommand took %s, want under %s (idle-aware drain should return near pipeDrainIdle=%s, not wait out the full pipeDrainGrace=%s)",
			elapsed, maxElapsed, pipeDrainIdle, pipeDrainGrace)
	}

	var stdout strings.Builder
	var errEvents []string
	scanner := bufio.NewScanner(bytes.NewBufferString(rec.body()))
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("unmarshal event %q: %v", line, err)
		}
		switch ev.Type {
		case "stdout":
			stdout.WriteString(ev.Data)
		case "error":
			errEvents = append(errEvents, ev.Error)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	if len(errEvents) > 0 {
		t.Fatalf("unexpected error events: %v", errEvents)
	}
	if !strings.Contains(stdout.String(), "done") {
		t.Fatalf("stdout = %q, want it to contain %q (foreground output must survive the idle-aware drain)", stdout.String(), "done")
	}
}

// TestRunCommandBoundsNoisyBackgroundDescendant proves the absolute
// pipeDrainGrace cap still bounds a background descendant that writes
// CONTINUOUSLY (unlike the silent `sleep` above), so `copied` never stops
// advancing and the idle branch of the drain can never fire on its own.
//
// pipeDrainGrace is lowered so the test stays fast; the descendant's tight
// `while true; do echo x; done` loop keeps the inherited stdout write-end
// busy for the whole drain, so only the cap — not the idle timer — can end
// the wait.
func TestRunCommandBoundsNoisyBackgroundDescendant(t *testing.T) {
	origGrace := pipeDrainGrace
	pipeDrainGrace = 1 * time.Second
	defer func() { pipeDrainGrace = origGrace }()

	rec := &slowResponseWriter{}
	writer := &eventWriter{w: rec}

	// The descendant writes to the inherited stdout write-end as fast as it
	// can, so `copied` keeps advancing on every poll and the idle deadline
	// keeps getting pushed out — only the absolute cap can end the drain.
	req := execRequest{
		Command: "(while true; do echo x; done) & echo done",
	}

	start := time.Now()
	code, err := runCommand(context.Background(), req, t.TempDir(), writer)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	// Lower bound (with small slack for timer/poll granularity): a
	// continuously-writing descendant must NOT trip the idle branch early.
	minElapsed := pipeDrainGrace - 50*time.Millisecond
	if elapsed < minElapsed {
		t.Fatalf("runCommand returned in %s, want at least ~%s (a continuously-writing descendant should never satisfy the idle branch — did it return near pipeDrainIdle=%s instead?)",
			elapsed, minElapsed, pipeDrainIdle)
	}
	// Upper bound: the cap must still fire promptly once it elapses, with
	// slack for scheduling and the poll interval, not hang indefinitely.
	maxElapsed := pipeDrainGrace + 2*time.Second
	if elapsed > maxElapsed {
		t.Fatalf("runCommand took %s, want under %s (the absolute pipeDrainGrace cap should still bound a noisy descendant)", elapsed, maxElapsed)
	}

	var stdout strings.Builder
	var errEvents []string
	scanner := bufio.NewScanner(bytes.NewBufferString(rec.body()))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<21)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("unmarshal event %q: %v", line, err)
		}
		switch ev.Type {
		case "stdout":
			stdout.WriteString(ev.Data)
		case "error":
			errEvents = append(errEvents, ev.Error)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	if len(errEvents) > 0 {
		t.Fatalf("unexpected error events: %v", errEvents)
	}
	if !strings.Contains(stdout.String(), "done") {
		t.Fatalf("stdout missing %q: foreground output must survive even when a noisy descendant forces the drain to hit its cap", "done")
	}
}

// TestRunCommandIdleDrainDeliversBurstyBackgroundOutput proves the idle timer
// RESETS on genuine progress instead of using a single fixed deadline anchored
// at the start of the drain.
//
// A background descendant emits three markers separated by gaps shorter than
// pipeDrainIdle, but the TOTAL span across all three bursts is longer than
// pipeDrainIdle. If the drain used one non-resetting idle deadline (started
// when the direct child exited) instead of resetting it on every observed
// `copied` advance, the later bursts would arrive after that deadline and get
// cut off when the read-ends are closed. pipeDrainIdle/pipeDrainPoll are
// lowered so the test stays fast and deterministic; pipeDrainGrace is left
// generous so only the idle logic is under test.
func TestRunCommandIdleDrainDeliversBurstyBackgroundOutput(t *testing.T) {
	origIdle := pipeDrainIdle
	origPoll := pipeDrainPoll
	origGrace := pipeDrainGrace
	pipeDrainIdle = 150 * time.Millisecond
	pipeDrainPoll = 15 * time.Millisecond
	pipeDrainGrace = 5 * time.Second
	defer func() {
		pipeDrainIdle = origIdle
		pipeDrainPoll = origPoll
		pipeDrainGrace = origGrace
	}()

	rec := &slowResponseWriter{}
	writer := &eventWriter{w: rec}

	// The direct child exits immediately after backgrounding a descendant
	// that emits three markers separated by 90ms gaps — each gap shorter than
	// pipeDrainIdle (150ms), so every burst must reset the idle timer. The
	// TOTAL span (~270ms across the gaps) exceeds pipeDrainIdle, so a
	// non-resetting single deadline would truncate BURST_B and/or BURST_C.
	req := execRequest{
		Command: "(sleep 0.09; echo BURST_A; sleep 0.09; echo BURST_B; sleep 0.09; echo BURST_C) & echo done",
	}

	start := time.Now()
	code, err := runCommand(context.Background(), req, t.TempDir(), writer)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	// Still bounded well under the (generous) grace cap.
	if elapsed > pipeDrainGrace {
		t.Fatalf("runCommand took %s, want under the pipeDrainGrace cap %s", elapsed, pipeDrainGrace)
	}

	var stdout strings.Builder
	var errEvents []string
	scanner := bufio.NewScanner(bytes.NewBufferString(rec.body()))
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("unmarshal event %q: %v", line, err)
		}
		switch ev.Type {
		case "stdout":
			stdout.WriteString(ev.Data)
		case "error":
			errEvents = append(errEvents, ev.Error)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	if len(errEvents) > 0 {
		t.Fatalf("unexpected error events: %v", errEvents)
	}
	got := stdout.String()
	for _, want := range []string{"done", "BURST_A", "BURST_B", "BURST_C"} {
		if !strings.Contains(got, want) {
			t.Fatalf("stdout = %q, missing %q (idle timer must reset on progress, not truncate a slow-but-live bursty stream)", got, want)
		}
	}
}

// TestRunCommandNoTruncationUnderSlowResponseWrite proves the fix for the
// @clawsweeper #1097 [P1] finding: "A response write blocked for more than
// 300 ms can be mistaken for an idle pipe, recreating silent stdout/stderr
// truncation under real downstream backpressure."
//
// copyPipe only adds a chunk's length to `copied` AFTER writer.write for
// that chunk returns, so a response write that blocks longer than
// pipeDrainIdle freezes `copied` for the whole write — indistinguishable
// from a genuinely idle pipe if the drain loop watches `copied` alone. The
// fix adds writesInFlight, incremented for the full duration of each
// writer.write call, and the drain loop treats writesInFlight.Load() > 0 as
// progress too, so a slow-but-active write can never be mistaken for an
// idle pipe and the read-ends are never closed mid-flush.
//
// This test drives that exact scenario: a slow response writer (200ms per
// Write, far longer than the lowered pipeDrainIdle) receives a multi-chunk
// stdout burst, and a backgrounded descendant (`sleep 30 &`) inherits the
// pipe write-end so the read-ends can never reach a natural EOF on their
// own — forcing the drain through the idle-aware path this bug lives in.
// Without the fix, the drain's false-idle detection fires mid-write and
// closes the read-ends before every byte the child already wrote into the
// kernel pipe has been read out, silently truncating stdout.
func TestRunCommandNoTruncationUnderSlowResponseWrite(t *testing.T) {
	origIdle := pipeDrainIdle
	origPoll := pipeDrainPoll
	origGrace := pipeDrainGrace
	pipeDrainIdle = 50 * time.Millisecond
	pipeDrainPoll = 10 * time.Millisecond
	pipeDrainGrace = 15 * time.Second
	defer func() {
		pipeDrainIdle = origIdle
		pipeDrainPoll = origPoll
		pipeDrainGrace = origGrace
	}()

	const totalBytes = 256 * 1024 // several 32KiB copyPipe chunks

	// 200ms per write is far larger than the lowered pipeDrainIdle (50ms),
	// so any drain logic that ignores an in-flight write will see `copied`
	// stall well past pipeDrainIdle during every single chunk.
	rec := &slowResponseWriter{delay: 200 * time.Millisecond}
	writer := &eventWriter{w: rec}

	// Emit a multi-chunk burst on stdout, then background a descendant that
	// inherits the pipe write-end so the read-ends never reach EOF on their
	// own once the direct child exits — the drain must rely on idle/grace
	// detection instead, which is exactly the path the false-idle bug lived
	// in.
	req := execRequest{
		Command: fmt.Sprintf("head -c %d /dev/zero | tr '\\0' 'x'; sleep 30 &", totalBytes),
	}

	start := time.Now()
	code, err := runCommand(context.Background(), req, t.TempDir(), writer)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	// Must return well under the descendant's 30s lifetime: runCommand owns
	// the read-ends and drains idle-aware, so it must not wait out the
	// backgrounded sleep.
	if maxElapsed := 20 * time.Second; elapsed > maxElapsed {
		t.Fatalf("runCommand took %s, want under %s (should not wait for the 30s background descendant)", elapsed, maxElapsed)
	}

	var got int
	var errEvents []string
	scanner := bufio.NewScanner(bytes.NewBufferString(rec.body()))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<21)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("unmarshal event %q: %v", line, err)
		}
		switch ev.Type {
		case "stdout":
			got += len(ev.Data)
		case "error":
			errEvents = append(errEvents, ev.Error)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	if len(errEvents) > 0 {
		t.Fatalf("unexpected error events: %v", errEvents)
	}
	if got != totalBytes {
		t.Fatalf("stdout bytes delivered = %d, want %d (lost %d bytes: a response write blocked longer than pipeDrainIdle was mistaken for an idle pipe and the read-ends were closed mid-flush)",
			got, totalBytes, totalBytes-got)
	}
}

// TestRunCommandDrainCapPreservesBufferedForegroundOutput proves the absolute
// descendant cap cannot discard foreground bytes that were already buffered
// in the pipe when the direct child exited.
func TestRunCommandDrainCapPreservesBufferedForegroundOutput(t *testing.T) {
	origIdle := pipeDrainIdle
	origPoll := pipeDrainPoll
	origGrace := pipeDrainGrace
	pipeDrainIdle = 50 * time.Millisecond
	pipeDrainPoll = 5 * time.Millisecond
	pipeDrainGrace = 75 * time.Millisecond
	defer func() {
		pipeDrainIdle = origIdle
		pipeDrainPoll = origPoll
		pipeDrainGrace = origGrace
	}()

	const totalBytes = 512 * 1024
	rec := &slowResponseWriter{delay: 200 * time.Millisecond}
	writer := &eventWriter{w: rec}
	req := execRequest{
		Command: fmt.Sprintf("head -c %d /dev/zero | tr '\\0' 'x'", totalBytes),
	}

	code, err := runCommand(context.Background(), req, t.TempDir(), writer)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	var got int
	var errEvents []string
	scanner := bufio.NewScanner(bytes.NewBufferString(rec.body()))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<21)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("unmarshal event %q: %v", line, err)
		}
		switch ev.Type {
		case "stdout":
			got += len(ev.Data)
		case "error":
			errEvents = append(errEvents, ev.Error)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(errEvents) > 0 {
		t.Fatalf("unexpected error events: %v", errEvents)
	}
	if got != totalBytes {
		t.Fatalf("stdout bytes delivered = %d, want %d (lost %d bytes: drain cap closed the pipe while buffered foreground output remained)",
			got, totalBytes, totalBytes-got)
	}
}
