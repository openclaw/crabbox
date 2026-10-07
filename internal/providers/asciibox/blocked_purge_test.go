package asciibox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type blockedPurgeRunner struct {
	inventoryJSON                           string
	infoFailure                             bool
	cancelInventory                         context.CancelFunc
	box                                     boxData
	configPath                              string
	nativeExit                              error
	absent                                  bool
	deleted                                 bool
	deletes, polls, exactReads, inventories int
}

func (r *blockedPurgeRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	switch action := boxCLIAction(req.Args); action {
	case "status":
		return core.LocalCommandResult{Stdout: fmt.Sprintf(`{"config":{"path":%q}}`, r.configPath)}, nil
	case "stop":
		return core.LocalCommandResult{}, nil
	case "delete":
		r.deleted = true
		r.deletes++
		return deletionOutcome(testDeletionID, r.box.ID, "box", "blocked").result, nil
	case "deletion":
		r.polls++
		return deletionOutcome(testDeletionID, r.box.ID, "box", "blocked").result, nil
	case "info":
		if r.deleted {
			r.exactReads++
			if r.infoFailure {
				return core.LocalCommandResult{}, errors.New("lost info response")
			}
			if r.absent {
				return core.LocalCommandResult{ExitCode: 1, Stderr: "box not found (404)"}, r.nativeExit
			}
		}
		data, _ := json.Marshal(map[string]any{"box": r.box})
		return core.LocalCommandResult{Stdout: string(data)}, nil
	case "list":
		r.inventories++
		if r.cancelInventory != nil {
			r.cancelInventory()
		}
		if r.inventoryJSON != "" {
			return core.LocalCommandResult{Stdout: r.inventoryJSON}, nil
		}
		boxes := []boxData{r.box}
		if r.deleted && r.absent {
			boxes = []boxData{}
		}
		data, _ := json.Marshal(map[string]any{"boxes": boxes})
		return core.LocalCommandResult{Stdout: string(data)}, nil
	default:
		return core.LocalCommandResult{}, fmt.Errorf("unexpected action %s", action)
	}
}

func TestStopBlockedPurge(t *testing.T) {
	nativeExit := boxNativeExit(t)
	for _, fixed := range []bool{false, true} {
		for _, test := range []struct {
			name                                          string
			absent, success, infoFailure, cancelInventory bool
			inventoryJSON                                 string
		}{
			{name: "present"},
			{name: "absent", absent: true, success: true},
			{name: "still listed", absent: true, inventoryJSON: `{"boxes":[{"id":"bx_1"}]}`},
			{name: "partial inventory", absent: true, inventoryJSON: `{"boxes":[],"pageInfo":{"hasMore":true}}`},
			{name: "info transport failure", absent: true, infoFailure: true},
			{name: "inventory cancellation", absent: true, cancelInventory: true},
		} {
			t.Run(fmt.Sprintf("fixed=%t/%s", fixed, test.name), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					var b *backend
					var lease core.LeaseTarget
					if fixed {
						var req core.AcquireRequest
						b, _, req, _ = fixedBoxFixture(t)
						var err error
						lease, err = b.Acquire(t.Context(), req)
						if err != nil {
							t.Fatal(err)
						}
					} else {
						b, _, _, lease = ownedFixture(t)
					}
					runner := &blockedPurgeRunner{box: testBox(), configPath: filepath.Join(t.TempDir(), "config.json"), nativeExit: nativeExit, absent: test.absent, inventoryJSON: test.inventoryJSON, infoFailure: test.infoFailure}
					c := &client{apiKey: "box_test", apiURL: "https://ascii.dev", cliPath: "box", home: t.TempDir(), runner: runner, releasePollInterval: time.Hour}
					withFakeAPI(t, c)
					ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
					defer cancel()
					if test.cancelInventory {
						runner.cancelInventory = cancel
					}
					original, err := core.ReadLeaseClaim(lease.LeaseID)
					if err != nil {
						t.Fatal(err)
					}
					err = b.ReleaseLease(ctx, core.ReleaseLeaseRequest{Lease: lease})
					if (err == nil) != test.success {
						t.Fatalf("release success=%t: %v", test.success, err)
					}
					claim, exists, readErr := core.ReadLeaseClaimWithPresence(lease.LeaseID)
					if readErr != nil {
						t.Fatal(readErr)
					}
					if !test.success {
						if !exists {
							t.Fatal("observable box lost its claim")
						}
					} else if fixed {
						if !exists || claim.FixedCreateIntent.State != "released" || claim.Labels[boxDeletionOperationLabel] != testDeletionID {
							t.Fatalf("terminal receipt lost purge operation: %+v", claim)
						}
						if retained, err := b.RetainLeaseClaimAfterReleaseWithClaim(lease, original); err != nil || !retained {
							t.Fatalf("stop rejected terminal receipt: retained=%t err=%v", retained, err)
						}
					} else if exists {
						t.Fatal("absent box retained its claim")
					}
					if runner.deletes != 1 {
						t.Fatalf("delete calls=%d", runner.deletes)
					}
					if test.success && (runner.exactReads == 0 || runner.inventories == 0 || runner.polls != 0) {
						t.Fatalf("absence not proven independently of purge: %+v", runner)
					}
				})
			})
		}
	}
}
