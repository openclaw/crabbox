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
