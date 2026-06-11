//go:build !windows

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExecCommandRunnerAllowsGracefulCancellation(t *testing.T) {
	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")
	cleanupPath := filepath.Join(dir, "cleanup")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := (execCommandRunner{}).Run(ctx, LocalCommandRequest{
			Name: "sh",
			Args: []string{"-c", `trap 'printf cleaned > "$2"; exit 0' INT
printf ready > "$1"
while :; do sleep 0.1; done`, "sh", readyPath, cleanupPath},
			CancelGracePeriod: 3 * time.Second,
		})
		done <- err
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("command did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled command returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled command did not exit")
	}
	if got, err := os.ReadFile(cleanupPath); err != nil {
		t.Fatalf("graceful cleanup did not run: %v", err)
	} else if string(got) != "cleaned" {
		t.Fatalf("cleanup marker=%q", got)
	}
}
