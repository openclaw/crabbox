package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	runEventOutputChunkBytes = 16 * 1024
	runEventOutputMaxBytes   = 64 * 1024
	runEventOutputQueueSize  = 32
	runEventOutputPostWait   = 2 * time.Second
)

type runEventPublication struct {
	coord *CoordinatorClient
	input CoordinatorRunEventInput
}

type runEventPublisher struct {
	onError         func(string, error) bool
	outputMu        sync.Mutex
	outputBytes     int
	outputTruncated bool
	queueMu         sync.Mutex
	queueClosed     bool
	disabled        bool
	events          chan runEventPublication
	cancel          context.CancelFunc
	done            chan struct{}
}

func newRunEventPublisher(onError func(string, error) bool) *runEventPublisher {
	return &runEventPublisher{onError: onError}
}

func (q *runEventPublisher) Closed() bool {
	if q == nil {
		return true
	}
	q.outputMu.Lock()
	defer q.outputMu.Unlock()
	return q.outputTruncated
}

func (q *runEventPublisher) Enqueue(coord *CoordinatorClient, runID, stream, data string) {
	if q == nil || data == "" {
		return
	}
	for _, input := range q.eventInputs(stream, data) {
		q.append(coord, runID, input)
	}
}

func (q *runEventPublisher) eventInputs(stream, data string) []CoordinatorRunEventInput {
	q.outputMu.Lock()
	defer q.outputMu.Unlock()
	if q.outputTruncated {
		return nil
	}
	remaining := runEventOutputMaxBytes - q.outputBytes
	if remaining <= 0 {
		q.outputTruncated = true
		return []CoordinatorRunEventInput{outputTruncatedEventInput()}
	}
	truncated := false
	if len(data) > remaining {
		data = data[:remaining]
		truncated = true
	}
	q.outputBytes += len(data)
	if truncated {
		q.outputTruncated = true
	}
	events := []CoordinatorRunEventInput{{
		Type:   stream,
		Stream: stream,
		Data:   data,
	}}
	if truncated {
		events = append(events, outputTruncatedEventInput())
	}
	return events
}

func outputTruncatedEventInput() CoordinatorRunEventInput {
	return CoordinatorRunEventInput{
		Type:    "output.truncated",
		Phase:   "command",
		Message: fmt.Sprintf("stdout/stderr event capture capped at %d bytes; use crabbox logs for retained command output", runEventOutputMaxBytes),
	}
}

// One FIFO owns phase and stream publication so delayed output cannot overtake
// lease attribution or terminal recording. Every queued request is joined.
func (q *runEventPublisher) append(coord *CoordinatorClient, runID string, input CoordinatorRunEventInput) {
	if coord == nil || runID == "" {
		return
	}
	publication := runEventPublication{coord: coord, input: input}
	q.queueMu.Lock()
	if q.disabled || q.queueClosed {
		q.queueMu.Unlock()
		return
	}
	if q.events == nil {
		ctx, cancel := context.WithCancel(context.Background())
		q.cancel = cancel
		q.events = make(chan runEventPublication, runEventOutputQueueSize)
		q.done = make(chan struct{})
		go q.post(ctx, runID)
	}
	select {
	case q.events <- publication:
		q.queueMu.Unlock()
	default:
		q.queueMu.Unlock()
		err := fmt.Errorf("run event queue full; diagnostic event dropped")
		if q.onError != nil {
			q.onError(input.Type, err)
		}
		return
	}
}

// A binding owns admission, so queued diagnostics cannot consume its capacity
// or HTTP budget. Drain and join before posting, then resume the same publisher.
func (q *runEventPublisher) Bind(ctx context.Context, coord *CoordinatorClient, runID string, input CoordinatorRunEventInput) error {
	q.CloseAndWait(runEventOutputPostWait)
	if err := postRunEvent(ctx, coord, runID, input); err != nil {
		return err
	}
	q.queueMu.Lock()
	q.events, q.done, q.cancel = nil, nil, nil
	q.queueClosed, q.disabled = false, false
	q.queueMu.Unlock()
	return nil
}

func postRunEvent(ctx context.Context, coord *CoordinatorClient, runID string, input CoordinatorRunEventInput) error {
	timeout := runRecorderRequestTimeout
	if input.Stream != "" || input.Type == "output.truncated" {
		timeout = runEventOutputPostWait
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := coord.AppendRunEvent(ctx, runID, input)
	return err
}

func (q *runEventPublisher) post(ctx context.Context, runID string) {
	defer close(q.done)
	var stopped error
	for publication := range q.events {
		err := ctx.Err()
		if err == nil {
			err = stopped
		}
		if err == nil {
			err = postRunEvent(ctx, publication.coord, runID, publication.input)
		}
		if err != nil && ctx.Err() == nil && stopped == nil && q.onError != nil && !q.onError(publication.input.Type, err) {
			stopped = err
			q.queueMu.Lock()
			q.disabled = true
			q.queueMu.Unlock()
		}
	}
}

func (q *runEventPublisher) CloseAndWait(timeout time.Duration) {
	if q == nil {
		return
	}
	q.queueMu.Lock()
	if q.events == nil {
		q.queueClosed = true
		q.queueMu.Unlock()
		return
	}
	if !q.queueClosed {
		q.queueClosed = true
		close(q.events)
	}
	done, cancel := q.done, q.cancel
	q.queueMu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		cancel()
		if q.onError != nil {
			q.onError("publication", fmt.Errorf("run event drain deadline expired; remaining diagnostics dropped"))
		}
		<-done
	}
	cancel()
}

type runEventStreamWriter struct {
	recorder *runRecorder
	stream   string
	data     strings.Builder
}

func (w *runEventStreamWriter) Write(p []byte) (int, error) {
	if w == nil || w.recorder == nil || w.recorder.runID == "" || w.recorder.finished || w.recorder.publisher == nil {
		return len(p), nil
	}
	written := len(p)
	for len(p) > 0 && !w.recorder.publisher.Closed() {
		space := runEventOutputChunkBytes - w.data.Len()
		if space <= 0 {
			w.Flush()
			continue
		}
		if space > len(p) {
			space = len(p)
		}
		_, _ = w.data.Write(p[:space])
		p = p[space:]
		if w.data.Len() >= runEventOutputChunkBytes {
			w.Flush()
		}
	}
	return written, nil
}

func (w *runEventStreamWriter) Flush() {
	if w == nil || w.recorder == nil || w.recorder.runID == "" || w.recorder.finished || w.recorder.publisher == nil || w.data.Len() == 0 {
		return
	}
	data := w.data.String()
	w.data.Reset()
	w.recorder.publisher.Enqueue(w.recorder.coord, w.recorder.runID, w.stream, data)
}
