package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestDirectAWSCheckpointDeleteRecovery(t *testing.T) {
	for _, firstRead := range []string{"error", "empty", "known subset", "pending"} {
		t.Run(firstRead, func(t *testing.T) {
			const checkpointID, imageID, accountID = "chk_0123456789abcdef", "ami-12345678", "123456789012"
			store := checkpointStore{root: t.TempDir()}
			record := checkpointRecord{ID: checkpointID, Kind: checkpointKindAWSAMI, Provider: "aws"}
			record.Native.ImageID, record.Native.Region, record.Native.AccountID, record.Native.Direct = imageID, "us-east-1", accountID, true
			if firstRead == "known subset" {
				record.Native.SnapshotIDs = []string{"snap-original"}
			}
			if _, _, err := store.Reserve(record); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			reads, deletes, failures := 0, 0, 0
			imagePresent, failSnapshot := true, true
			snapshots := map[string]bool{"snap-original": true, "snap-discovered": true}
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if err := r.ParseForm(); err != nil {
					t.Error(err)
					return
				}
				switch r.Form.Get("Action") {
				case "GetCallerIdentity":
					writeSTSXML(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>`+accountID+`</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`)
				case "DescribeImages":
					reads++
					if reads == 1 && firstRead == "error" {
						writeEC2Error(w, "RequestExpired", "injected first observation failure", http.StatusBadRequest)
					} else if reads == 1 && firstRead == "empty" {
						writeEC2XML(w, `<DescribeImagesResponse><imagesSet/></DescribeImagesResponse>`)
					} else if !imagePresent {
						writeEC2Error(w, "InvalidAMIID.NotFound", "image not found", http.StatusBadRequest)
					} else {
						writeEC2XML(w, `<DescribeImagesResponse><imagesSet><item><imageId>`+imageID+`</imageId><imageState>pending</imageState><blockDeviceMapping><item><ebs><snapshotId>snap-original</snapshotId></ebs></item><item><ebs><snapshotId>snap-discovered</snapshotId></ebs></item></blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`)
					}
				case "DeregisterImage":
					deletes++
					// The next operation destroys the only provider mapping. Both
					// backing IDs must already survive reopening the local record.
					saved, _, err := store.Read(checkpointID)
					if err != nil || !slices.Contains(saved.Native.SnapshotIDs, "snap-original") || !slices.Contains(saved.Native.SnapshotIDs, "snap-discovered") {
						t.Errorf("deregister reached before backing identities were durable: ids=%v err=%v", saved.Native.SnapshotIDs, err)
					}
					if !imagePresent {
						writeEC2Error(w, "InvalidAMIID.NotFound", "image not found", http.StatusBadRequest)
					} else {
						imagePresent = false
						writeEC2XML(w, `<DeregisterImageResponse/>`)
					}
				case "DeleteSnapshot":
					id := r.Form.Get("SnapshotId")
					if id == "snap-discovered" && failSnapshot {
						failSnapshot = false
						failures++
						writeEC2Error(w, "OperationNotPermitted", "injected deletion failure", http.StatusBadRequest)
					} else if !snapshots[id] {
						writeEC2Error(w, "InvalidSnapshot.NotFound", "snapshot not found", http.StatusBadRequest)
					} else {
						snapshots[id] = false
						writeEC2XML(w, `<DeleteSnapshotResponse/>`)
					}
				default:
					t.Errorf("unexpected action: %s", r.Form.Get("Action"))
				}
			}))
			defer endpoint.Close()
			for key, value := range map[string]string{
				"AWS_ENDPOINT_URL": endpoint.URL, "AWS_ACCESS_KEY_ID": "fixture", "AWS_SECRET_ACCESS_KEY": "fixture",
				"AWS_SESSION_TOKEN": "", "AWS_PROFILE": "", "AWS_EC2_METADATA_DISABLED": "true", "AWS_MAX_ATTEMPTS": "1",
				"AWS_CONFIG_FILE": filepath.Join(t.TempDir(), "config"), "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(t.TempDir(), "credentials"),
				"CRABBOX_CONFIG": filepath.Join(t.TempDir(), "missing.yaml"),
			} {
				t.Setenv(key, value)
			}
			for attempt := 0; attempt < 3; attempt++ {
				err := deleteCheckpoint(context.Background(), store, checkpointID, false)
				if err == nil {
					break
				}
				if _, _, readErr := store.Read(checkpointID); readErr != nil {
					t.Fatalf("failed deletion removed recovery record: %v (delete: %v)", readErr, err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if imagePresent || snapshots["snap-original"] || snapshots["snap-discovered"] || failures != 1 || deletes < 2 {
				t.Fatalf("retry lost native cleanup: image=%v snapshots=%v injectedFailures=%d deregisters=%d", imagePresent, snapshots, failures, deletes)
			}
			if _, _, err := store.Read(checkpointID); !isCheckpointNotFound(err) {
				t.Fatalf("completed deletion retained local record: %v", err)
			}
		})
	}
}

func TestDirectAWSCheckpointDeleteStopsBeforeDestruction(t *testing.T) {
	for _, reason := range []string{"persistence", "read", "pending without backing IDs", "absent without backing IDs"} {
		t.Run(reason, func(t *testing.T) {
			persistErr := errors.New("checkpoint store write failed")
			var mu sync.Mutex
			var actions []string
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if err := r.ParseForm(); err != nil {
					t.Error(err)
					return
				}
				action := r.Form.Get("Action")
				actions = append(actions, action)
				switch action {
				case "GetCallerIdentity":
					writeSTSXML(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`)
				case "DescribeImages":
					switch reason {
					case "read":
						writeEC2Error(w, "UnauthorizedOperation", "injected read failure", http.StatusBadRequest)
					case "absent without backing IDs":
						writeEC2XML(w, `<DescribeImagesResponse><imagesSet/></DescribeImagesResponse>`)
					case "pending without backing IDs":
						writeEC2XML(w, `<DescribeImagesResponse><imagesSet><item><imageId>ami-12345678</imageId><imageState>pending</imageState></item></imagesSet></DescribeImagesResponse>`)
					default:
						writeEC2XML(w, `<DescribeImagesResponse><imagesSet><item><imageId>ami-12345678</imageId><imageState>pending</imageState><blockDeviceMapping><item><ebs><snapshotId>snap-discovered</snapshotId></ebs></item></blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`)
					}
				default:
					t.Errorf("destructive request after %s failure: %s", reason, action)
				}
			}))
			defer endpoint.Close()
			persisted := false
			err := testAWSClient(endpoint.URL).DeleteImageCheckpoint(context.Background(), "ami-12345678", nil, "123456789012", func(ids []string) error {
				persisted = true
				if !slices.Equal(ids, []string{"snap-discovered"}) {
					t.Errorf("unexpected persistence identity: %v", ids)
				}
				return persistErr
			})
			if err == nil || reason == "persistence" && !errors.Is(err, persistErr) {
				t.Fatalf("missing %s failure: %v", reason, err)
			}
			if strings.Contains(reason, "without backing IDs") && !strings.Contains(err.Error(), "backing snapshot identities are unavailable") {
				t.Fatalf("missing explicit unresolved-cleanup outcome: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if persisted != (reason == "persistence") || !slices.Equal(actions, []string{"GetCallerIdentity", "DescribeImages"}) {
				t.Fatalf("unsafe cleanup ordering: persisted=%v actions=%v", persisted, actions)
			}
		})
	}
}
