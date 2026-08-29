package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCheckpointUsagePreservesCurrentRecord(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted=%t", deleted), func(t *testing.T) {
			store := checkpointStore{root: t.TempDir()}
			stale, err := store.Create(checkpointRecord{ID: "chk_usage", Kind: checkpointKindIncus})
			if err != nil {
				t.Fatal(err)
			}
			current := stale
			current.Native.ImageID, current.Native.State = "published-image", "available"
			if err := store.Write(current); err != nil {
				t.Fatal(err)
			}
			if deleted {
				if err := deleteCheckpoint(context.Background(), store, stale.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			if err := recordCheckpointUse(store, &stale); err != nil {
				t.Fatal(err)
			}
			observed, _, err := store.Read(stale.ID)
			if deleted {
				if !isCheckpointNotFound(err) {
					t.Fatalf("usage recreated a deleted checkpoint: %#v (%v)", observed, err)
				}
				return
			}
			if err != nil || observed.Native.ImageID != "published-image" || observed.Native.State != "available" {
				t.Fatalf("usage restored stale capture metadata: %#v (%v)", observed, err)
			}
		})
	}
}

func TestCheckpointRecordOperationHelper(t *testing.T) {
	root := os.Getenv("CRABBOX_TEST_CHECKPOINT_LOCK_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	store := checkpointStore{root: root}
	if err := store.withRecord("chk_locked", false, func(checkpointRecord) error {
		fmt.Println("checkpoint-lock-ready")
		_, err := io.Copy(io.Discard, os.Stdin)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointDeletionRejectsAnotherProcessCapture(t *testing.T) {
	store := checkpointStore{root: t.TempDir()}
	if _, err := store.Create(checkpointRecord{ID: "chk_locked", Kind: checkpointKindArchive}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCheckpointRecordOperationHelper$")
	command.Env = append(os.Environ(), "CRABBOX_TEST_CHECKPOINT_LOCK_ROOT="+store.root)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = command.Wait()
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "checkpoint-lock-ready" {
		_ = stdin.Close()
		_ = command.Wait()
		t.Fatalf("capture helper did not acquire its record: %q (%v), %s", line, err, stderr.String())
	}
	if err := deleteCheckpoint(ctx, store, "chk_locked", true); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("another process deleted an active capture: %v", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("capture helper: %v, %s", err, stderr.String())
	}
	if err := deleteCheckpoint(ctx, store, "chk_locked", true); err != nil {
		t.Fatalf("delete released checkpoint: %v", err)
	}
	if _, _, err := store.Read("chk_locked"); !isCheckpointNotFound(err) {
		t.Fatalf("deleted checkpoint remains: %v", err)
	}
}
