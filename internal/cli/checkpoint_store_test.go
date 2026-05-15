package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckpointStoreCreateReadList(t *testing.T) {
	store := checkpointStore{dir: t.TempDir()}
	first, err := store.Create(CheckpointRecord{
		ID:        "chk_first",
		Name:      "first",
		CreatedAt: "2026-05-09T10:00:00Z",
		Repo: CheckpointRepo{
			Root:      "/repo",
			Name:      "my-app",
			RemoteURL: "https://github.com/example-org/my-app",
			Branch:    "main",
			Head:      "abc123",
			BaseRef:   "main",
		},
		Lease: CheckpointLease{
			ID:       "cbx_123",
			Slug:     "blue-lobster",
			Provider: "aws",
			TargetOS: "linux",
			Class:    "beast",
		},
		Workspace: CheckpointWorkspace{
			ChangedFiles: 2,
			DeletedFiles: 1,
			Bytes:        42,
		},
	})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	if first.ID != "chk_first" {
		t.Fatalf("id=%q", first.ID)
	}
	second, err := store.Create(CheckpointRecord{
		ID:        "chk_second",
		ParentID:  "chk_first",
		Name:      "second",
		CreatedAt: "2026-05-09T11:00:00Z",
		Run: CheckpointRun{
			ID:         "run_123",
			DurationMs: 1500,
			Command:    "go test ./...",
		},
		Artifacts: []CheckpointArtifact{{Path: "artifacts/blue-lobster/screenshot.png", Type: "screenshot"}},
	})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}

	got, err := store.Read(second.ID)
	if err != nil {
		t.Fatalf("read second: %v", err)
	}
	if got.ParentID != "chk_first" || got.Run.ID != "run_123" || len(got.Artifacts) != 1 {
		t.Fatalf("unexpected checkpoint: %#v", got)
	}

	records, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records=%d want 2", len(records))
	}
	if records[0].ID != "chk_second" || records[1].ID != "chk_first" {
		t.Fatalf("records ordered newest first: %#v", records)
	}

	data, err := os.ReadFile(filepath.Join(store.dir, "chk_second.json"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json: %v", err)
	}
	if raw["id"] != "chk_second" {
		t.Fatalf("json id=%v", raw["id"])
	}
}

func TestCheckpointStoreRejectsDuplicatesAndUnsafeIDs(t *testing.T) {
	store := checkpointStore{dir: t.TempDir()}
	if _, err := store.Create(CheckpointRecord{ID: "chk_ok", CreatedAt: "2026-05-09T10:00:00Z"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Create(CheckpointRecord{ID: "chk_ok", CreatedAt: "2026-05-09T10:01:00Z"}); err == nil {
		t.Fatal("duplicate checkpoint succeeded")
	}
	if _, err := store.Create(CheckpointRecord{ID: "../bad", CreatedAt: "2026-05-09T10:01:00Z"}); err == nil {
		t.Fatal("unsafe checkpoint id succeeded")
	}
	if _, err := store.Create(CheckpointRecord{ID: "chk_bad/slash", CreatedAt: "2026-05-09T10:01:00Z"}); err == nil {
		t.Fatal("slash checkpoint id succeeded")
	}
	if _, err := store.Create(CheckpointRecord{ID: "chk_bad", ParentID: "../parent", CreatedAt: "2026-05-09T10:01:00Z"}); err == nil {
		t.Fatal("unsafe parent id succeeded")
	}
}

func TestCheckpointStoreConcurrentSameIDAllowsOneWriter(t *testing.T) {
	store := checkpointStore{dir: t.TempDir()}
	const workers = 64
	start := make(chan struct{})
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			<-start
			_, err := store.Create(CheckpointRecord{
				ID:        "chk_race",
				Name:      fmt.Sprintf("worker-%d", i),
				CreatedAt: "2026-05-09T10:00:00Z",
			})
			results <- err
		}(i)
	}

	close(start)
	successes := 0
	for i := 0; i < workers; i++ {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent creates=%d, want 1", successes)
	}
	got, err := store.Read("chk_race")
	if err != nil {
		t.Fatalf("read raced checkpoint: %v", err)
	}
	if got.ID != "chk_race" || got.CreatedAt != "2026-05-09T10:00:00Z" {
		t.Fatalf("unexpected raced checkpoint: %#v", got)
	}
}

func TestCheckpointStoreFillsIDAndCreatedAt(t *testing.T) {
	store := checkpointStore{dir: t.TempDir()}
	record, err := store.Create(CheckpointRecord{Name: "generated"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(record.ID, checkpointIDPrefix) {
		t.Fatalf("id=%q", record.ID)
	}
	if record.CreatedAt == "" {
		t.Fatal("createdAt not filled")
	}
}
