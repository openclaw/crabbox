package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestCheckpointManagedCreateWaitContinuesExactPendingOperation(t *testing.T) {
	const id = "chk_pending_capture"
	pending := `{"error":"checkpoint_pending","message":"checkpoint creation did not complete; durable ownership is retained for recovery","checkpointID":"` + id + `"}`
	for _, test := range []struct {
		name string
		code int
		body string
		wait bool
		ok   bool
	}{
		{"retained operation", 503, pending, true, true},
		{"published operation", 201, "", true, true},
		{"non-waiting caller", 503, pending, false, false},
		{"generic unavailable", 503, `{"error":"unavailable"}`, true, false},
		{"duplicate reservation", 409, pending, true, false},
		{"foreign identifier", 503, strings.ReplaceAll(pending, id, "chk_other"), true, false},
		{"missing identifier", 503, `{"error":"checkpoint_pending"}`, true, false},
		{"malformed response", 503, pending[:len(pending)-1], true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkpoint := managedCheckpointFixture(id)
			image := CoordinatorImage{ID: checkpoint.Image.ID, ResourceID: checkpoint.Image.ResourceID, State: "available"}
			posts, gets := 0, 0
			owned := false
			server, _ := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
				if !owned {
					t.Error("request preceded durable capture ownership")
				}
				if r.Method == http.MethodPost && r.URL.Path == "/v1/checkpoints" {
					posts++
					var request map[string]any
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["id"] != id {
						t.Errorf("wrong capture request: %v, %v", request, err)
					}
					w.WriteHeader(test.code)
					if test.body != "" {
						_, _ = io.WriteString(w, test.body)
						return
					}
				} else if r.Method == http.MethodGet && r.URL.Path == "/v1/checkpoints/"+id {
					gets++
				} else {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": checkpoint, "image": image})
			})
			cfg := defaultConfig()
			cfg.Provider, cfg.Coordinator, cfg.TargetOS = "aws", server.URL, targetWindows
			cfg.WindowsMode = windowsModeNormal
			record := checkpointRecord{ID: id, Provider: "aws", LeaseID: checkpoint.LeaseID}
			ctx := withCheckpointCreateContext(context.Background(), record, checkpointRetentionFromDuration(0), func(managed bool) error {
				owned = managed
				return nil
			})
			result, err := (coordinatorCheckpointDriver{}).Create(ctx, NativeCheckpointCreateRequest{
				Config: cfg, Server: Server{Provider: "aws", CloudID: "i-test"},
				Target:       SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal},
				CheckpointID: id, LeaseID: record.LeaseID, Strategy: checkpointStrategyImage,
				Wait: test.wait, WaitTimeout: time.Minute, Stderr: io.Discard,
			})
			if posts != 1 || !owned {
				t.Fatalf("capture ownership/replay: posts=%d owned=%v", posts, owned)
			}
			if test.ok {
				if err != nil || result.ID != image.ID || result.State != "available" || result.managedCheckpoint == nil || result.managedCheckpoint.ID != id {
					t.Fatalf("capture did not finish: %#v, %v", result, err)
				}
			} else {
				var failure CoordinatorHTTPError
				if !errors.As(err, &failure) || failure.StatusCode != test.code || failure.Message != test.body || gets != 0 {
					t.Fatalf("original failure changed: %v, gets=%d", err, gets)
				}
			}
		})
	}
}

func TestCheckpointManagedWaitObservesRecoveryBeforeImageReadiness(t *testing.T) {
	for _, outcome := range []string{"available", "deleting"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				checkpoint := managedCheckpointFixture("chk_recovering")
				reads, verifies := 0, 0
				client := &CoordinatorClient{BaseURL: "http://coordinator.test", Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.Method != http.MethodGet || r.URL.Path != "/v1/checkpoints/"+checkpoint.ID {
						t.Fatalf("recovery attempted a mutation or another checkpoint: %s %s", r.Method, r.URL)
					}
					body := map[string]any{"checkpoint": checkpoint}
					code := http.StatusOK
					if r.URL.Query().Get("verify") == "true" {
						verifies++
						if verifies == 1 {
							code = http.StatusConflict
							body = map[string]any{"error": "checkpoint_pending", "message": "checkpoint provider resource is not ready"}
						} else {
							state := "pending"
							if verifies == 3 {
								state = "available"
							}
							body["image"] = CoordinatorImage{ID: checkpoint.Image.ID, ResourceID: checkpoint.Image.ResourceID, State: state}
						}
					} else {
						reads++
						if outcome == "deleting" && reads == 4 {
							deleting := checkpoint
							deleting.State = "deleting"
							body["checkpoint"] = deleting
						} else if reads < 3 {
							state := "creating"
							if reads == 2 {
								state = "failed"
							}
							body["checkpoint"] = map[string]any{"id": checkpoint.ID, "state": state, "retryAt": time.Now().Add(time.Minute).Format(time.RFC3339)}
						}
					}
					data, _ := json.Marshal(body)
					return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(data))), Request: r}, nil
				})}}
				image, err := waitForCheckpointImage(context.Background(), client, checkpoint.ID, "", 2*time.Minute, io.Discard)
				if outcome == "deleting" {
					if err == nil || !strings.Contains(err.Error(), "deleting") || reads != 4 || verifies != 1 {
						t.Fatalf("continued after deletion raced verification: %v, reads=%d verifies=%d", err, reads, verifies)
					}
					return
				}
				if err != nil || image.ID != checkpoint.Image.ID || image.State != "available" || image.managedCheckpoint == nil || image.managedCheckpoint.State != "ready" {
					t.Fatalf("recovery result: %#v, %v", image, err)
				}
				if reads < 3 || verifies != 3 {
					t.Fatalf("skipped lifecycle or verification: reads=%d verifies=%d", reads, verifies)
				}
			})
		})
	}
}

func TestCheckpointManagedWaitStopsAtFailureOrContextBoundary(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   string
		code   int
		want   string
		verify bool
	}{
		{"terminal failure", `{"checkpoint":{"id":"chk_wait","state":"failed","lastError":"provider refused capture"}}`, 200, "provider refused capture", false},
		{"deletion", `{"checkpoint":{"id":"chk_wait","state":"deleting"}}`, 200, "deleting", false},
		{"foreign record", `{"checkpoint":{"id":"chk_other","state":"creating"}}`, 200, "requested checkpoint", false},
		{"missing image", `{"checkpoint":{"id":"chk_wait","state":"ready"}}`, 200, "identity changed or is missing", false},
		{"authorization", `{"error":"forbidden"}`, 403, "403", false},
		{"permanent verify conflict", `{"error":"checkpoint_source_mismatch"}`, 409, "checkpoint_source_mismatch", true},
		{"foreign verified record", `{"checkpoint":{"id":"chk_other"},"image":{"id":"snap-0001","state":"available"}}`, 200, "requested checkpoint", true},
		{"foreign verified image", `{"checkpoint":{"id":"chk_wait"},"image":{"id":"snap-other","state":"available"}}`, 200, "identity changed", true},
		{"poll deadline", `{"checkpoint":{"id":"chk_wait","state":"creating"}}`, 200, "deadline exceeded", false},
		{"slow request deadline", "", 0, "deadline exceeded", false},
		{"slow verification deadline", "", 0, "deadline exceeded", true},
		{"caller cancellation", "", 0, "context canceled", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				client := &CoordinatorClient{BaseURL: "http://coordinator.test", Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if test.verify && r.URL.Query().Get("verify") != "true" {
						data, _ := json.Marshal(map[string]any{"checkpoint": managedCheckpointFixture("chk_wait")})
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(data))), Request: r}, nil
					}
					if test.code == 0 {
						if test.name == "caller cancellation" {
							cancel()
						}
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					return &http.Response{StatusCode: test.code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body)), Request: r}, nil
				})}}
				_, err := waitForCheckpointImage(ctx, client, "chk_wait", "", time.Second, io.Discard)
				wantCalls := 1
				if test.verify {
					wantCalls++
				}
				if err == nil || !strings.Contains(err.Error(), test.want) || calls != wantCalls {
					t.Fatalf("wait did not stop: %v, calls=%d", err, calls)
				}
				if test.code == 0 && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
					t.Fatalf("lost context error identity: %v", err)
				}
				var failure ExitError
				if strings.Contains(test.name, "deadline") || test.name == "terminal failure" {
					if !errors.As(err, &failure) || failure.Code != 5 || !strings.Contains(failure.Message, "checkpoint inspect chk_wait --verify") {
						t.Fatalf("lost CLI exit code or recovery guidance: %v", err)
					}
				}
			})
		})
	}
}
