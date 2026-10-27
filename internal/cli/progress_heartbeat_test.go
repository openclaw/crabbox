package cli

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedHeartbeatBuffer collects heartbeat output written from the ticker goroutine.
type lockedHeartbeatBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedHeartbeatBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *lockedHeartbeatBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// firstHeartbeatLine runs a heartbeat until it prints one line and returns that line.
func firstHeartbeatLine(t *testing.T, start func(io.Writer, time.Time, time.Duration) func(), elapsed time.Duration) string {
	t.Helper()
	var out lockedHeartbeatBuffer
	stop := start(&out, time.Now().Add(-elapsed), 5*time.Millisecond)
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if line, _, found := strings.Cut(out.String(), "\n"); found {
			return line + "\n"
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("heartbeat printed nothing; output=%q", out.String())
	return ""
}

func TestProgressHeartbeatMessages(t *testing.T) {
	tests := []struct {
		name    string
		start   func(io.Writer, time.Time, time.Duration) func()
		elapsed time.Duration
		want    string
	}{
		{
			name:    "git seed",
			start:   startGitSeedHeartbeat,
			elapsed: 30 * time.Second,
			want:    "still seeding remote Git metadata after 30s...\n",
		},
		{
			name:    "git seed has no sync watchdog hint",
			start:   startGitSeedHeartbeat,
			elapsed: 5 * time.Minute,
			want:    "still seeding remote Git metadata after 5m0s...\n",
		},
		{
			name:    "sync",
			start:   startSyncHeartbeat,
			elapsed: 30 * time.Second,
			want:    "still syncing after 30s...\n",
		},
		{
			name:    "sync quiet watchdog",
			start:   startSyncHeartbeat,
			elapsed: 5 * time.Minute,
			want:    "still syncing after 5m0s... watchdog=sync-quiet next_action=wait, or cancel and retry with --full-resync/fresh lease if no progress\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstHeartbeatLine(t, tt.start, tt.elapsed); got != tt.want {
				t.Fatalf("heartbeat line = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGitSeedHeartbeatDisabledForNonpositiveInterval(t *testing.T) {
	var out lockedHeartbeatBuffer
	stop := startGitSeedHeartbeat(&out, time.Now().Add(-time.Minute), 0)
	time.Sleep(20 * time.Millisecond)
	stop()
	if got := out.String(); got != "" {
		t.Fatalf("disabled heartbeat output = %q, want empty", got)
	}
}

func TestProgressHeartbeatDonePreferredOverReadyTick(t *testing.T) {
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	done := make(chan struct{})
	close(done)
	var out bytes.Buffer
	// Both cases are ready; repeat so a random select choice cannot hide a print.
	for range 100 {
		writeProgressHeartbeats(&out, time.Now(), ticks, done, "seeding remote Git metadata", "")
	}
	if got := out.String(); got != "" {
		t.Fatalf("heartbeat printed after stop: %q", got)
	}
}

// blockingHeartbeatWriter holds the first write until released and records any later write.
type blockingHeartbeatWriter struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	writes  int
}

func (w *blockingHeartbeatWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	w.writes++
	first := w.writes == 1
	w.mu.Unlock()
	if first {
		close(w.entered)
		<-w.release
	}
	return len(data), nil
}

func (w *blockingHeartbeatWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

func TestProgressHeartbeatStopWaitsForInFlightLine(t *testing.T) {
	w := &blockingHeartbeatWriter{entered: make(chan struct{}), release: make(chan struct{})}
	stop := startGitSeedHeartbeat(w, time.Now(), time.Millisecond)
	<-w.entered
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stop returned while a heartbeat line was still being written")
	case <-time.After(50 * time.Millisecond):
	}
	close(w.release)
	<-stopped
	after := w.count()
	time.Sleep(20 * time.Millisecond)
	if got := w.count(); got != after || got != 1 {
		t.Fatalf("heartbeat writes = %d after stop (stop saw %d), want exactly 1", got, after)
	}
}
