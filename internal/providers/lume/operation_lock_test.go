package lume

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLumeCapacityLockSerializesAndHonorsCancellation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	unlock, err := lockLumeCapacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := lockLumeCapacity(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended lock error=%v want context deadline exceeded", err)
	}

	unlock()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	secondUnlock, err := lockLumeCapacity(ctx)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	secondUnlock()
}
