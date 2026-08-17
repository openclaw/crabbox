package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type fakeImageDeleteLifecycle struct {
	requests []NativeCheckpointResourceRequest
	err      error
}

func (*fakeImageDeleteLifecycle) NativeCheckpointWorkdir(NativeCheckpointWorkdirRequest) string {
	return ""
}

func (*fakeImageDeleteLifecycle) CreateNativeCheckpoint(context.Context, NativeCheckpointCreateRequest) (NativeCheckpointCreateResult, error) {
	return NativeCheckpointCreateResult{}, errors.New("not implemented")
}

func (*fakeImageDeleteLifecycle) VerifyNativeCheckpoint(context.Context, NativeCheckpointResourceRequest) (NativeCheckpointVerifyResult, error) {
	return NativeCheckpointVerifyResult{}, errors.New("not implemented")
}

func (f *fakeImageDeleteLifecycle) DeleteNativeCheckpoint(_ context.Context, req NativeCheckpointResourceRequest) error {
	f.requests = append(f.requests, req)
	return f.err
}

func TestDeleteHetznerCheckpointImageRequiresUniqueLocalRecord(t *testing.T) {
	for name, tc := range map[string]struct {
		records []checkpointRecord
		want    string
	}{
		"zero": {want: "without an exact local"},
		"multiple": {
			records: []checkpointRecord{hetznerImageDeleteRecord("chk_one", "99", "fsn1"), hetznerImageDeleteRecord("chk_two", "99", "fsn1")},
			want:    "2 local checkpoint records",
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := checkpointStore{root: t.TempDir()}
			for _, record := range tc.records {
				if _, err := store.Create(record); err != nil {
					t.Fatal(err)
				}
			}
			lifecycle := &fakeImageDeleteLifecycle{}
			_, err := deleteHetznerCheckpointImage(context.Background(), store, lifecycle, "99", "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
			if len(lifecycle.requests) != 0 {
				t.Fatalf("provider deletion called: %+v", lifecycle.requests)
			}
		})
	}
}

func TestDeleteHetznerCheckpointImageChecksLocationAndDeletesRemoteFirst(t *testing.T) {
	store := checkpointStore{root: t.TempDir()}
	record := hetznerImageDeleteRecord("chk_owned", "99", "fsn1")
	if _, err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	lifecycle := &fakeImageDeleteLifecycle{}
	if _, err := deleteHetznerCheckpointImage(context.Background(), store, lifecycle, "99", "hel1"); err == nil || !strings.Contains(err.Error(), "location mismatch") {
		t.Fatalf("err=%v", err)
	}
	if len(lifecycle.requests) != 0 {
		t.Fatal("provider deletion called on region mismatch")
	}

	remoteErr := errors.New("remote refusal")
	lifecycle.err = remoteErr
	if _, err := deleteHetznerCheckpointImage(context.Background(), store, lifecycle, "99", "fsn1"); !errors.Is(err, remoteErr) {
		t.Fatalf("err=%v", err)
	}
	if _, _, err := store.Read(record.ID); err != nil {
		t.Fatalf("local record removed before remote success: %v", err)
	}

	lifecycle.err = nil
	deleted, err := deleteHetznerCheckpointImage(context.Background(), store, lifecycle, "99", "fsn1")
	if err != nil {
		t.Fatal(err)
	}
	if deleted.ID != record.ID || len(lifecycle.requests) != 2 {
		t.Fatalf("deleted=%+v requests=%d", deleted, len(lifecycle.requests))
	}
	req := lifecycle.requests[1]
	if req.Image.Kind != checkpointKindHetzner || req.Image.ID != "99" || req.Image.Region != "fsn1" || req.Metadata["checkpoint"] != record.ID {
		t.Fatalf("request=%+v", req)
	}
	if _, _, err := store.Read(record.ID); err == nil {
		t.Fatal("local record retained after remote success")
	}
}

func TestImageDeleteHetznerRejectsProjectBeforeProviderAccess(t *testing.T) {
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).imageDelete(context.Background(), []string{"99", "--provider", "hetzner", "--project", "example-project"})
	if err == nil || !strings.Contains(err.Error(), "--project is not supported") {
		t.Fatalf("err=%v", err)
	}
}

func TestDeleteHetznerCheckpointLocalOnlyKeepsProviderSnapshot(t *testing.T) {
	store := checkpointStore{root: t.TempDir()}
	record := hetznerImageDeleteRecord("chk_localonly", "99", "fsn1")
	if _, err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	if err := deleteCheckpoint(context.Background(), store, record.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Read(record.ID); err == nil {
		t.Fatal("local-only delete retained local metadata")
	}
}

func TestHetznerCheckpointKindProviderRouting(t *testing.T) {
	if !isNativeCheckpointKind(checkpointKindHetzner) {
		t.Fatal("Hetzner snapshot kind is not native")
	}
	if got := checkpointProviderForKind(checkpointKindHetzner); got != "hetzner" {
		t.Fatalf("provider=%q", got)
	}
	if got := checkpointStrategyForKind(checkpointKindHetzner); got != checkpointStrategyDiskSnapshot {
		t.Fatalf("strategy=%q", got)
	}
	image := CoordinatorImage{ID: "99", Provider: "hetzner", Kind: checkpointKindHetzner}
	if got := checkpointKindForProviderImage(image); got != checkpointKindHetzner {
		t.Fatalf("kind=%q", got)
	}
	if got := checkpointCreateStrategy("native", checkpointStrategyImage, checkpointKindHetzner); got != checkpointStrategyImage {
		t.Fatalf("explicit strategy=%q", got)
	}
	if got := checkpointCreateStrategy("image", checkpointStrategyAuto, checkpointKindHetzner); got != checkpointStrategyImage {
		t.Fatalf("image mode strategy=%q", got)
	}
}

func hetznerImageDeleteRecord(id, imageID, region string) checkpointRecord {
	record := checkpointRecord{
		ID:        id,
		Kind:      checkpointKindHetzner,
		Provider:  "hetzner",
		LeaseID:   "cbx_abcdef123456",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	record.Native.Provider = "hetzner"
	record.Native.ImageID = imageID
	record.Native.Resource = imageID
	record.Native.Region = region
	record.Native.Architecture = "x86"
	record.Native.Direct = true
	record.Native.Metadata = map[string]string{
		"checkpoint":          id,
		"lease":               record.LeaseID,
		"source_server":       "42",
		"source_location":     region,
		"source_architecture": "x86",
		"source_type":         "server",
	}
	return record
}
