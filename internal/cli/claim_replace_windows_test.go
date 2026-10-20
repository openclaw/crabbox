//go:build windows

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestClaimPublicationSharingRetry(t *testing.T) {
	for _, tc := range []struct {
		name          string
		blocked       int
		openBlocked   int
		openError     error
		renameError   error
		flushError    error
		closeError    error
		syncError     error
		changeClaim   bool
		removeClaim   bool
		cancel        bool
		waitError     error
		wantAttempts  int
		wantOpens     int
		wantWaits     int
		wantPublished bool
		wantError     string
	}{
		{name: "transient sharing violation", blocked: 2, wantAttempts: 3, wantWaits: 2, wantPublished: true},
		{name: "transient open sharing violation", openBlocked: 2, wantAttempts: 1, wantOpens: 3, wantWaits: 2, wantPublished: true},
		{name: "exhausted open bound", openBlocked: 6, wantOpens: 6, wantWaits: 5, wantError: "blocked after 6 attempts"},
		{name: "other open error", openError: windows.ERROR_ACCESS_DENIED, wantOpens: 1, wantError: windows.ERROR_ACCESS_DENIED.Error()},
		{name: "exhausted bound", blocked: 6, wantAttempts: 6, wantWaits: 5, wantError: "blocked after 6 attempts"},
		{name: "other rename error", renameError: windows.ERROR_ACCESS_DENIED, wantAttempts: 1, wantError: windows.ERROR_ACCESS_DENIED.Error()},
		{name: "flush sharing violation", flushError: windows.ERROR_SHARING_VIOLATION, wantAttempts: 1, wantPublished: true, wantError: "flush renamed file"},
		{name: "close after publication", closeError: windows.ERROR_SHARING_VIOLATION, wantAttempts: 1, wantPublished: true, wantError: "close namespace file"},
		{name: "close after failed rename", blocked: 1, closeError: windows.ERROR_SHARING_VIOLATION, wantAttempts: 1, wantError: "close namespace file"},
		{name: "directory sync sharing violation", syncError: windows.ERROR_SHARING_VIOLATION, wantAttempts: 1, wantPublished: true, wantError: "sync state directory"},
		{name: "revision changed", blocked: 1, changeClaim: true, wantAttempts: 1, wantWaits: 1, wantError: "claim changed"},
		{name: "claim removed", blocked: 1, removeClaim: true, wantAttempts: 1, wantWaits: 1, wantError: "claim changed"},
		{name: "cancel between attempts", blocked: 1, cancel: true, wantAttempts: 1, wantWaits: 1, wantError: "context canceled"},
		{name: "deadline between attempts", blocked: 1, waitError: context.DeadlineExceeded, wantAttempts: 1, wantWaits: 1, wantError: "deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := seedClaimContract(t)
			path, err := leaseClaimPath(original.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			opens, attempts, flushes, closes, syncs, actions, mutations := 0, 0, 0, 0, 0, 0, 0
			var delays []time.Duration
			ops := defaultWindowsRenameOps()
			realOpen := ops.open
			realRename, realFlush, realClose := ops.rename, ops.flush, ops.close
			ops.open = func(path string) (windows.Handle, error) {
				opens++
				if opens <= tc.openBlocked {
					return windows.InvalidHandle, windows.ERROR_SHARING_VIOLATION
				}
				if tc.openError != nil {
					return windows.InvalidHandle, tc.openError
				}
				return realOpen(path)
			}
			ops.rename = func(handle windows.Handle, info *byte, size uint32) error {
				attempts++
				if attempts <= tc.blocked {
					return windows.ERROR_SHARING_VIOLATION
				}
				if tc.renameError != nil {
					return tc.renameError
				}
				return realRename(handle, info, size)
			}
			ops.flush = func(handle windows.Handle) error {
				flushes++
				if tc.flushError != nil {
					return tc.flushError
				}
				return realFlush(handle)
			}
			ops.close = func(handle windows.Handle) error {
				closes++
				return errors.Join(realClose(handle), tc.closeError)
			}
			wait := func(ctx context.Context, delay time.Duration) error {
				delays = append(delays, delay)
				if deadline, ok := ctx.Deadline(); !ok || deadline.IsZero() {
					t.Fatal("retry has no deadline")
				}
				if tc.cancel {
					cancel()
				}
				if tc.changeClaim {
					changed := original
					changed.Revision = "concurrent-revision"
					if err := writeLeaseClaimAtomic(path, changed); err != nil {
						t.Fatal(err)
					}
				}
				if tc.removeClaim {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				return tc.waitError
			}
			_, err = transactLeaseClaim(original.LeaseID, leaseClaimTransaction{
				context:   ctx,
				guard:     unchangedLeaseClaimGuard(original.LeaseID, original, true),
				directory: claimDirectoryDurableNamespace,
				action: func() (claimActionDecision, error) {
					actions++
					return claimActionContinue, nil
				},
				mutate: func(claim *leaseClaim) error {
					mutations++
					claim.Slug = "updated-after-sharing-retry"
					return nil
				},
				replace: func(ctx context.Context, from, to string, unchanged func() error) error {
					return retryClaimPublication(ctx, unchanged, wait, func() (bool, error) {
						return renameWindowsFileWithOps(from, to, windows.FILE_RENAME_REPLACE_IF_EXISTS|windows.FILE_RENAME_POSIX_SEMANTICS, ops)
					})
				},
				syncDirectory: func(string) error {
					syncs++
					return tc.syncError
				},
			})
			if (err == nil) != (tc.wantError == "") || (err != nil && !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("error=%v, want %q", err, tc.wantError)
			}
			if attempts != tc.wantAttempts || len(delays) != tc.wantWaits || closes != attempts || actions != 1 || mutations != 1 {
				t.Fatalf("attempts=%d waits=%v closes=%d actions=%d mutations=%d", attempts, delays, closes, actions, mutations)
			}
			wantOpens := tc.wantOpens
			if wantOpens == 0 {
				wantOpens = tc.wantAttempts
			}
			if opens != wantOpens {
				t.Fatalf("open attempts=%d want %d", opens, wantOpens)
			}
			allDelays := []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond}
			if !reflect.DeepEqual(delays, allDelays[:tc.wantWaits]) && tc.wantWaits != 0 {
				t.Fatalf("unexpected retry schedule: %v", delays)
			}
			stored, exists, readErr := readLeaseClaimPathWithPresence(path)
			if readErr != nil || exists == tc.removeClaim {
				t.Fatalf("stored claim presence=%v err=%v", exists, readErr)
			}
			if tc.wantPublished {
				if stored.Slug != "updated-after-sharing-retry" || flushes != 1 {
					t.Fatalf("new claim not published exactly once: %#v flushes=%d", stored, flushes)
				}
			} else {
				want := original
				if tc.changeClaim {
					want.Revision = "concurrent-revision"
				}
				if !tc.removeClaim && !reflect.DeepEqual(stored, want) {
					t.Fatalf("failed publication changed claim: %#v want %#v", stored, want)
				}
				if flushes != 0 || syncs != 0 {
					t.Fatalf("unpublished failure flushed/synced: %d/%d", flushes, syncs)
				}
			}
			if tc.syncError != nil && syncs != 1 {
				t.Fatalf("directory sync replayed %d times", syncs)
			}
			temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*"))
			if err != nil || len(temps) != 0 {
				t.Fatalf("staged claim residue=%v err=%v", temps, err)
			}
		})
	}
}

func TestClaimPublicationRetryWaitHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitClaimPublicationRetry(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait=%v", err)
	}
}
