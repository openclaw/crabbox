package cli

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func ovhImageDeleteRecord(id, imageID, region string) checkpointRecord {
	record := checkpointRecord{
		ID:        id,
		Kind:      checkpointKindOVH,
		Provider:  "ovh",
		LeaseID:   "cbx_abcdef123456",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	record.Native.Provider = "ovh"
	record.Native.ImageID = imageID
	record.Native.Resource = imageID
	record.Native.Region = region
	record.Native.Direct = true
	record.Native.Metadata = map[string]string{
		"checkpoint":      id,
		"lease":           record.LeaseID,
		"source_instance": "inst-1",
		"source_project":  "project-test",
		"source_region":   region,
		"source_type":     "instance",
	}
	return record
}

func TestDeleteOVHCheckpointImageUsesOVHRecordsOnly(t *testing.T) {
	store := checkpointStore{root: t.TempDir()}
	// A Hetzner record with the same image id must never authorize an OVH delete.
	if _, _, err := store.Reserve(hetznerImageDeleteRecord("chk_hetzner", "snap-1", "BHS5")); err != nil {
		t.Fatal(err)
	}
	lifecycle := &fakeImageDeleteLifecycle{}
	spec := nativeImageDeleteSpecs["ovh"]
	_, err := deleteNativeCheckpointImage(context.Background(), store, lifecycle, spec, "snap-1", "")
	if err == nil || !strings.Contains(err.Error(), "without an exact local ovh-snapshot checkpoint record") {
		t.Fatalf("err=%v", err)
	}
	record := ovhImageDeleteRecord("chk_owned", "snap-1", "BHS5")
	if _, _, err := store.Reserve(record); err != nil {
		t.Fatal(err)
	}
	if _, err := deleteNativeCheckpointImage(context.Background(), store, lifecycle, spec, "snap-1", "GRA11"); err == nil || !strings.Contains(err.Error(), "region mismatch") {
		t.Fatalf("err=%v", err)
	}
	if len(lifecycle.requests) != 0 {
		t.Fatal("provider deletion called on region mismatch")
	}
	deleted, err := deleteNativeCheckpointImage(context.Background(), store, lifecycle, spec, "snap-1", "BHS5")
	if err != nil {
		t.Fatal(err)
	}
	if deleted.ID != record.ID || len(lifecycle.requests) != 1 || lifecycle.requests[0].Image.Kind != checkpointKindOVH {
		t.Fatalf("deleted=%+v requests=%+v", deleted, lifecycle.requests)
	}
	if _, _, err := store.Read(record.ID); err == nil {
		t.Fatal("local record retained after remote success")
	}
}

func TestImageDeleteOVHRejectsProjectBeforeProviderAccess(t *testing.T) {
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).imageDelete(context.Background(), []string{"snap-1", "--provider", "ovh", "--project", "example-project"})
	if err == nil || !strings.Contains(err.Error(), "--project is not supported for OVH image deletion") {
		t.Fatalf("err=%v", err)
	}
}

func TestOVHCheckpointKindRouting(t *testing.T) {
	if !isNativeCheckpointKind(checkpointKindOVH) {
		t.Fatal("OVH snapshot kind is not native")
	}
	if got := checkpointProviderForKind(checkpointKindOVH); got != "ovh" {
		t.Fatalf("provider=%q", got)
	}
	if got := checkpointStrategyForKind(checkpointKindOVH); got != checkpointStrategyDiskSnapshot {
		t.Fatalf("strategy=%q", got)
	}
	image := CoordinatorImage{ID: "snap-1", Provider: "ovh", Kind: checkpointKindOVH}
	if got := checkpointKindForProviderImage(image); got != checkpointKindOVH {
		t.Fatalf("kind=%q", got)
	}
	if got := checkpointKindForProviderImage(CoordinatorImage{ID: "snap-1", Provider: "ovh"}); got != checkpointKindOVH {
		t.Fatalf("provider-only kind=%q", got)
	}
}
