package asciibox

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

type boxCleanupProgressKey struct{}
type boxCleanupPhaseKey struct{}

type boxCleanupProgress struct {
	mu       sync.Mutex
	writer   io.Writer
	started  time.Time
	last     time.Time
	interval time.Duration
}

func withBoxCleanupProgress(ctx context.Context, writer io.Writer) context.Context {
	if writer == nil || ctx.Value(boxCleanupProgressKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, boxCleanupProgressKey{}, &boxCleanupProgress{
		writer: writer, started: time.Now(), interval: 10 * time.Second,
	})
}

func boxCleanupPhaseContext(ctx context.Context, phase string) context.Context {
	return context.WithValue(ctx, boxCleanupPhaseKey{}, phase)
}

func boxCommandPhase(args []string) string {
	if len(args) > 0 {
		switch args[0] {
		case "stop":
			return "native-stop"
		case "delete":
			return "native-delete"
		case "extend":
			return "snapshot-recovery"
		case "deletion":
			return "deletion-operation"
		}
	}
	return "configuration"
}

func startBoxCommandProgress(ctx context.Context, phase string) func() {
	progress, _ := ctx.Value(boxCleanupProgressKey{}).(*boxCleanupProgress)
	if progress == nil {
		return func() {}
	}
	if override, ok := ctx.Value(boxCleanupPhaseKey{}).(string); ok {
		phase = override
	}
	write := func() {
		progress.mu.Lock()
		defer progress.mu.Unlock()
		now := time.Now()
		if ctx.Err() != nil || !progress.last.IsZero() && now.Sub(progress.last) < progress.interval {
			return
		}
		progress.last = now
		remaining := "deadline=none"
		if deadline, ok := ctx.Deadline(); ok {
			remaining = fmt.Sprintf("remaining=%s", max(time.Duration(0), deadline.Sub(now)).Round(time.Second))
		}
		fmt.Fprintf(progress.writer, "ascii-box cleanup phase=%s elapsed=%s %s; waiting for native CLI\n", phase, now.Sub(progress.started).Round(time.Second), remaining)
	}
	// Shared cadence also reports fast polls whose individual timers never fire.
	write()
	stop := make(chan struct{})
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		ticker := time.NewTicker(progress.interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				write()
			}
		}
	}()
	// Join before returning to core or its guarded-cleanup writer.
	return func() { close(stop); <-joined }
}
