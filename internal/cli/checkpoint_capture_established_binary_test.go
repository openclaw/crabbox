//go:build !windows

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCheckpointCreateCaptureEstablishedBuiltBinaryContract(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary, err := builtCLITestBinary()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"direct pending", "direct available", "direct timeout", "direct snapshot error", "brokered pending", "brokered available", "legacy pending"} {
		t.Run(mode, func(t *testing.T) {
			f := newCheckpointCaptureFixture(t, repo, binary)
			const instanceID, accountID, imageID = "i-0123456789abcdef0", "123456789012", "ami-0123456789abcdef0"
			var creates, imageReads atomic.Int32
			brokered := strings.Contains(mode, "brokered") || strings.Contains(mode, "legacy")
			var managed coordinatorCheckpoint
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if brokered {
					w.Header().Set("Content-Type", "application/json")
					switch {
					case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+captureFixtureLease:
						_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: captureFixtureLease, Slug: "capture-proof", Provider: "aws", TargetOS: targetLinux, CloudID: instanceID, Host: "127.0.0.1", SSHUser: "crabbox", SSHPort: "22", State: "active"}})
					case r.URL.Path == "/v1/checkpoints" && mode == "legacy pending":
						http.NotFound(w, r)
					case r.Method == http.MethodGet && r.URL.Path == "/v1/checkpoints/"+managed.ID:
						_ = json.NewEncoder(w).Encode(map[string]any{"checkpoint": managed, "image": CoordinatorImage{ID: "snap-0001", ResourceID: "snap-0001", Provider: "aws", Kind: checkpointKindAWSEBS, State: "available"}})
					case r.Method == http.MethodPost && (r.URL.Path == "/v1/checkpoints" || r.URL.Path == "/v1/images"):
						creates.Add(1)
						var request map[string]any
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
						}
						state := "pending"
						if mode == "brokered available" {
							state = "available"
						}
						image := CoordinatorImage{ID: "snap-0001", ResourceID: "snap-0001", Provider: "aws", Kind: checkpointKindAWSEBS, State: state, Snapshots: []string{"snap-0001"}}
						response := map[string]any{"image": image}
						if r.URL.Path == "/v1/checkpoints" {
							checkpoint := managedCheckpointFixture(request["id"].(string))
							checkpoint.LeaseID, checkpoint.Image.State = captureFixtureLease, state
							managed = checkpoint
							response["checkpoint"] = checkpoint
						}
						_ = json.NewEncoder(w).Encode(response)
					default:
						t.Errorf("unexpected coordinator request: %s %s", r.Method, r.URL)
						http.NotFound(w, r)
					}
					return
				}
				if err := r.ParseForm(); err != nil {
					t.Error(err)
					return
				}
				switch r.Form.Get("Action") {
				case "GetCallerIdentity":
					writeSTSXML(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>`+accountID+`</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`)
				case "DescribeInstances":
					fmt.Fprintf(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>%s</instanceId><instanceType>t3.medium</instanceType><ipAddress>127.0.0.1</ipAddress><instanceState><name>running</name></instanceState><blockDeviceMapping><item><deviceName>/dev/sda1</deviceName><ebs><volumeId>vol-root</volumeId></ebs></item></blockDeviceMapping><tagSet>`, instanceID)
					for key, value := range map[string]string{"Name": "capture-proof", "crabbox": "true", "created_by": "crabbox", "provider": "aws", "lease": captureFixtureLease, "slug": "capture-proof", "provider_key": ProviderKeyForLease(captureFixtureLease)} {
						fmt.Fprintf(w, "<item><key>%s</key><value>%s</value></item>", key, value)
					}
					fmt.Fprint(w, `</tagSet></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`)
				case "CreateImage":
					creates.Add(1)
					writeEC2XML(w, `<CreateImageResponse><imageId>`+imageID+`</imageId></CreateImageResponse>`)
				case "DescribeImages":
					imageReads.Add(1)
					// Accepted identity must be durable before the first capture observation.
					paths, _ := filepath.Glob(filepath.Join(f.root, "state", "crabbox", "checkpoints", "*", checkpointMetaFile))
					if len(paths) != 1 {
						t.Errorf("missing durable reservation: %v", paths)
					} else {
						data, err := os.ReadFile(paths[0])
						var saved checkpointRecord
						if err != nil || json.Unmarshal(data, &saved) != nil || saved.Native.ImageID != imageID || saved.Native.AccountID != accountID || saved.CaptureEstablished {
							t.Errorf("invalid pre-barrier identity: %s err=%v", data, err)
						}
					}
					mapping := `<blockDeviceMapping><item><deviceName>/dev/sda1</deviceName><ebs><snapshotId>snap-root</snapshotId></ebs></item></blockDeviceMapping>`
					if mode == "direct timeout" {
						mapping = ""
					}
					state := "pending"
					if mode == "direct available" {
						state = "available"
					}
					writeEC2XML(w, `<DescribeImagesResponse><imagesSet><item><imageId>`+imageID+`</imageId><imageOwnerId>`+accountID+`</imageOwnerId><imageState>`+state+`</imageState>`+mapping+`</item></imagesSet></DescribeImagesResponse>`)
				case "DescribeSnapshots":
					if r.Form.Get("SnapshotId.1") != "snap-root" {
						t.Errorf("wrong snapshot lookup: %v", r.Form)
					}
					state := "pending"
					if mode == "direct snapshot error" {
						state = "error"
					}
					writeEC2XML(w, `<DescribeSnapshotsResponse><snapshotSet><item><snapshotId>snap-root</snapshotId><ownerId>`+accountID+`</ownerId><volumeId>vol-root</volumeId><status>`+state+`</status></item></snapshotSet></DescribeSnapshotsResponse>`)
				default:
					t.Errorf("unexpected EC2 action: %v", r.Form)
					http.Error(w, "unexpected action", 400)
				}
			}))
			t.Cleanup(endpoint.Close)
			config := "provider: aws\nnetwork: public\naws:\n  region: us-east-1\ntargetOS: linux\n"
			if brokered {
				config += "coordinator: " + endpoint.URL + "\n"
			}
			if err := os.WriteFile(filepath.Join(f.root, "config.yaml"), []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.root, "bin", "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			f.env = append(f.env, "AWS_ENDPOINT_URL="+endpoint.URL, "AWS_ACCESS_KEY_ID=fixture", "AWS_SECRET_ACCESS_KEY=fixture", "AWS_EC2_METADATA_DISABLED=true", "AWS_REGION=us-east-1", "CRABBOX_COORDINATOR_TOKEN=fixture", "CRABBOX_COORDINATOR_ADMIN_TOKEN=fixture-admin")
			claim := f.claim()
			claim.Provider, claim.CloudID, claim.CloudImmutableID, claim.ProviderScope, claim.Slug = "aws", instanceID, "", "", "capture-proof"
			claim.FixedCreateIntent = nil
			claim.Labels = map[string]string{"aws_region": "us-east-1", "aws_account_id": accountID, "provider_key": ProviderKeyForLease(captureFixtureLease)}
			f.writeJSON(filepath.Join(f.root, "state", "crabbox", "claims", captureFixtureLease+".json"), claim)
			wait := "false"
			if strings.Contains(mode, "available") {
				wait = "true"
			}
			timeout := "30s"
			if mode == "direct timeout" {
				timeout = "250ms"
			}
			result := f.run("checkpoint", "create", "--provider", "aws", "--id", captureFixtureLease, "--mode", "native", "--wait="+wait, "--wait-timeout", timeout, "--json")
			failure := mode == "direct timeout" || mode == "direct snapshot error"
			if creates.Load() != 1 {
				t.Fatalf("CreateImage was retried: %d", creates.Load())
			}
			if failure {
				if result.err == nil || len(result.stdout) != 0 || !strings.Contains(string(result.stderr), "uncertain") {
					t.Fatalf("capture failure misreported: err=%v stdout=%s stderr=%s", result.err, result.stdout, result.stderr)
				}
				paths, _ := filepath.Glob(filepath.Join(f.root, "state", "crabbox", "checkpoints", "*", checkpointMetaFile))
				if len(paths) != 1 {
					t.Fatalf("accepted capture lost: %v", paths)
				}
				data, _ := os.ReadFile(paths[0])
				var saved checkpointRecord
				if json.Unmarshal(data, &saved) != nil || saved.Native.ImageID != imageID || saved.CaptureEstablished {
					t.Fatalf("uncertain identity lost/attested: %s", data)
				}
				return
			}
			var record map[string]any
			if result.err != nil || json.Unmarshal(result.stdout, &record) != nil || record["captureEstablished"] != true {
				t.Fatalf("missing capture contract: err=%v stdout=%s stderr=%s", result.err, result.stdout, result.stderr)
			}
			if !brokered && imageReads.Load() != 1 {
				t.Fatalf("unexpected capture reads: %d", imageReads.Load())
			}
		})
	}
}
