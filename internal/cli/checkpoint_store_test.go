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
	store := checkpointStore{root: t.TempDir()}
	first, err := store.Create(checkpointRecord{
		ID:        "chk_first",
		Name:      "first",
		Kind:      checkpointKindArchive,
		CreatedAt: "2026-05-09T10:00:00Z",
		Workdir:   "/work/cbx_1/my-app",
	})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	if first.ID != "chk_first" {
		t.Fatalf("id=%q", first.ID)
	}
	second, err := store.Create(checkpointRecord{
		ID:        "chk_second",
		Name:      "second",
		Kind:      checkpointKindArchive,
		CreatedAt: "2026-05-09T11:00:00Z",
		Workdir:   "/work/cbx_2/my-app",
	})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}

	got, _, err := store.Read(second.ID)
	if err != nil {
		t.Fatalf("read second: %v", err)
	}
	if got.ID != "chk_second" || got.Workdir != "/work/cbx_2/my-app" {
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

	data, err := os.ReadFile(filepath.Join(store.root, "chk_second", checkpointMetaFile))
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

func TestCheckpointStoreReserveWritesMetadata(t *testing.T) {
	store := checkpointStore{root: t.TempDir()}
	record, paths, err := store.Reserve(checkpointRecord{
		ID:        "chk_pending",
		Kind:      checkpointKindAWSAMI,
		CreatedAt: "2026-05-09T10:00:00Z",
		Workdir:   "/work/cbx_1/my-app",
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := os.Stat(paths.Meta); err != nil {
		t.Fatalf("stat metadata: %v", err)
	}

	got, _, err := store.Read("chk_pending")
	if err != nil {
		t.Fatalf("read reserved checkpoint: %v", err)
	}
	if got.ID != record.ID || got.Kind != checkpointKindAWSAMI {
		t.Fatalf("unexpected reserved checkpoint: %#v", got)
	}

	records, err := store.List()
	if err != nil {
		t.Fatalf("list reserved checkpoint: %v", err)
	}
	if len(records) != 1 || records[0].ID != "chk_pending" {
		t.Fatalf("records=%#v", records)
	}
}

func TestCheckpointStoreRejectsDuplicatesAndUnsafeIDs(t *testing.T) {
	store := checkpointStore{root: t.TempDir()}
	if _, err := store.Create(checkpointRecord{ID: "chk_ok", CreatedAt: "2026-05-09T10:00:00Z"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Create(checkpointRecord{ID: "chk_ok", CreatedAt: "2026-05-09T10:01:00Z"}); err == nil {
		t.Fatal("duplicate checkpoint succeeded")
	}
	if _, err := store.Create(checkpointRecord{ID: "../bad", CreatedAt: "2026-05-09T10:01:00Z"}); err == nil {
		t.Fatal("unsafe checkpoint id succeeded")
	}
	if _, err := store.Create(checkpointRecord{ID: "chk_bad/slash", CreatedAt: "2026-05-09T10:01:00Z"}); err == nil {
		t.Fatal("slash checkpoint id succeeded")
	}
}

func TestCheckpointStoreRejectsMetadataIDMismatch(t *testing.T) {
	store := checkpointStore{root: t.TempDir()}
	record, err := store.Create(checkpointRecord{
		ID:        "chk_source",
		Kind:      checkpointKindArchive,
		CreatedAt: "2026-05-09T10:00:00Z",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	source, err := store.Paths(record.ID)
	if err != nil {
		t.Fatalf("source paths: %v", err)
	}
	target, err := store.Paths("chk_copy")
	if err != nil {
		t.Fatalf("target paths: %v", err)
	}
	if err := os.MkdirAll(target.Dir, 0o700); err != nil {
		t.Fatalf("create copied dir: %v", err)
	}
	data, err := os.ReadFile(source.Meta)
	if err != nil {
		t.Fatalf("read source metadata: %v", err)
	}
	if err := os.WriteFile(target.Meta, data, 0o600); err != nil {
		t.Fatalf("write copied metadata: %v", err)
	}

	if _, _, err := store.Read("chk_copy"); err == nil {
		t.Fatal("expected metadata id mismatch")
	}
	if err := store.Delete("chk_copy"); err != nil {
		t.Fatalf("delete copied dir: %v", err)
	}
	if _, _, err := store.Read("chk_source"); err != nil {
		t.Fatalf("source checkpoint removed: %v", err)
	}
}

func TestCheckpointStoreConcurrentSameIDAllowsOneWriter(t *testing.T) {
	store := checkpointStore{root: t.TempDir()}
	const workers = 64
	start := make(chan struct{})
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			<-start
			_, err := store.Create(checkpointRecord{
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
	got, _, err := store.Read("chk_race")
	if err != nil {
		t.Fatalf("read raced checkpoint: %v", err)
	}
	if got.ID != "chk_race" || got.CreatedAt != "2026-05-09T10:00:00Z" {
		t.Fatalf("unexpected raced checkpoint: %#v", got)
	}
}

func TestCheckpointStoreFillsIDAndCreatedAt(t *testing.T) {
	store := checkpointStore{root: t.TempDir()}
	record, err := store.Create(checkpointRecord{Name: "generated"})
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
