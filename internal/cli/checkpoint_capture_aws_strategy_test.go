//go:build !windows

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Exercise the real AWS adapter and SDK with only a loopback EC2/STS boundary.
// The explicit synthetic SDK credentials and empty HOME prevent ambient lookup.
func runCheckpointAWSStrategyContract(t *testing.T, repo, binary string) {
	for _, strategy := range []string{"", "auto", "image", "disk-snapshot"} {
		t.Run("direct AWS Linux retirement strategy "+blank(strategy, "default"), func(t *testing.T) {
			t.Parallel()
			f := newCheckpointCaptureFixture(t, repo, binary)
			const instanceID = "i-0123456789abcdef0"
			const imageID = "ami-0123456789abcdef0"
			const accountID = "123456789012"
			var mu sync.Mutex
			creates, removes, ready, removed := 0, 0, false, false
			var actions []string
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if err := r.ParseForm(); err != nil {
					t.Error(err)
					http.Error(w, "bad fixture request", 400)
					return
				}
				action := r.Form.Get("Action")
				actions = append(actions, action)
				w.Header().Set("Content-Type", "text/xml")
				switch action {
				case "GetCallerIdentity":
					fmt.Fprintf(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>%s</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`, accountID)
				case "DescribeInstances":
					if removed {
						fmt.Fprint(w, `<DescribeInstancesResponse><reservationSet/></DescribeInstancesResponse>`)
						return
					}
					fmt.Fprintf(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>%s</instanceId><instanceType>t3.medium</instanceType><ipAddress>127.0.0.1</ipAddress><instanceState><name>running</name></instanceState><tagSet>`, instanceID)
					for key, value := range map[string]string{"Name": "aws-strategy-proof", "crabbox": "true", "created_by": "crabbox", "provider": "aws", "lease": captureFixtureLease, "slug": "aws-strategy-proof", "provider_key": providerKeyForLease(captureFixtureLease)} {
						fmt.Fprintf(w, "<item><key>%s</key><value>%s</value></item>", key, value)
					}
					fmt.Fprint(w, `</tagSet></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`)
				case "CreateImage":
					if r.Form.Get("InstanceId") != instanceID || r.Form.Get("NoReboot") != "true" {
						t.Errorf("wrong capture request: %v", r.Form)
					}
					creates++
					fmt.Fprintf(w, `<CreateImageResponse><imageId>%s</imageId></CreateImageResponse>`, imageID)
				case "DescribeImages":
					if r.Form.Get("ImageId.1") != imageID {
						t.Errorf("wrong image inspection: %v", r.Form)
					}
					state := "pending"
					if ready {
						state = "available"
					}
					fmt.Fprintf(w, `<DescribeImagesResponse><imagesSet><item><imageId>%s</imageId><imageState>%s</imageState><architecture>x86_64</architecture></item></imagesSet></DescribeImagesResponse>`, imageID, state)
				case "TerminateInstances":
					if r.Form.Get("InstanceId.1") != instanceID || !ready {
						t.Errorf("premature or wrong source retirement: %v ready=%v", r.Form, ready)
					}
					removes++
					removed = true
					fmt.Fprint(w, `<TerminateInstancesResponse/>`)
				case "DeleteKeyPair":
					if r.Form.Get("KeyName") != providerKeyForLease(captureFixtureLease) {
						t.Errorf("wrong source key cleanup: %v", r.Form)
					}
					fmt.Fprint(w, `<DeleteKeyPairResponse><return>true</return></DeleteKeyPairResponse>`)
				default:
					t.Errorf("unexpected AWS mutation/request: %s", action)
					http.Error(w, "unsupported fixture action", 400)
				}
			}))
			t.Cleanup(endpoint.Close)
			config := "provider: aws\nnetwork: public\naws:\n  region: us-east-1\ntargetOS: linux\n"
			if err := os.WriteFile(filepath.Join(f.root, "config.yaml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			sshCalls := filepath.Join(f.root, "ssh-calls")
			sshFixture := `#!/bin/sh
for arg do remote=$arg; done
case "$remote" in
  'exit 0') printf 'probe\n' >> "$CAPTURE_AWS_SSH_CALLS";;
  "bash -lc 'if command -v cloud-init >/dev/null 2>&1; then sudo cloud-init clean --logs; fi; sync'") printf 'prepare\n' >> "$CAPTURE_AWS_SSH_CALLS";;
  *) printf 'unexpected SSH command: %s\n' "$remote" >&2; exit 97;;
esac
`
			if err := os.WriteFile(filepath.Join(f.root, "bin", "ssh"), []byte(sshFixture), 0o700); err != nil {
				t.Fatal(err)
			}
			f.env = append(f.env, "AWS_ENDPOINT_URL="+endpoint.URL, "AWS_ACCESS_KEY_ID=fixture", "AWS_SECRET_ACCESS_KEY=fixture", "AWS_EC2_METADATA_DISABLED=true", "AWS_REGION=us-east-1", "CAPTURE_AWS_SSH_CALLS="+sshCalls)
			claim := f.claim()
			claim.Provider, claim.CloudID, claim.ProviderScope, claim.Slug = "aws", instanceID, "", "aws-strategy-proof"
			claim.FixedCreateIntent = nil
			claim.Labels = map[string]string{"aws_region": "us-east-1", "aws_account_id": accountID, "provider_key": providerKeyForLease(captureFixtureLease)}
			claimPath := filepath.Join(f.root, "state", "crabbox", "claims", captureFixtureLease+".json")
			f.writeJSON(claimPath, claim)
			before, err := os.ReadFile(claimPath)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"checkpoint", "create", "--provider", "aws", "--id", captureFixtureLease, "--checkpoint-id", captureFixtureCheckpoint, "--retire-source", "--mode", "native", "--wait=false", "--json"}
			if strategy != "" {
				args = append(args, "--strategy", strategy)
			}
			admission := f.run(append(append([]string{}, args...), "--prepare-only")...)
			wantAdmission := "ready"
			if strategy == "disk-snapshot" {
				wantAdmission = "unsupported"
			}
			var receipt checkpointCaptureAdmission
			if err := json.Unmarshal(admission.stdout, &receipt); err != nil || admission.err != nil || receipt.Admission != wantAdmission || receipt.SourceID != instanceID {
				t.Fatalf("native strategy admission=%+v want=%s err=%v json=%v\n%s", receipt, wantAdmission, admission.err, err, admission.stderr)
			}
			result := f.run(args...)
			if strategy == "disk-snapshot" {
				after, readErr := os.ReadFile(claimPath)
				_, journalErr := os.Stat(filepath.Join(f.root, "state", "crabbox", "checkpoints", captureFixtureCheckpoint, checkpointMetaFile))
				if result.err == nil || readErr != nil || !bytes.Equal(before, after) || !os.IsNotExist(journalErr) {
					t.Fatalf("unsupported explicit strategy changed ownership: err=%v stderr=%s journal=%v", result.err, result.stderr, journalErr)
				}
			} else {
				for replay := 0; replay < 2; replay++ {
					if result.err != nil {
						t.Fatalf("native AMI creation/replay failed: %v\n%s", result.err, result.stderr)
					}
					record := f.record(captureFixtureCheckpoint)
					if record.Kind != checkpointKindAWSAMI || record.Native.Strategy != checkpointStrategyImage || record.Capture == nil || record.Capture.StrategyExplicit != (strategy == "image") || record.Capture.Phase != "pending" || record.Native.ImageID != imageID || !record.Native.Direct {
						t.Fatalf("AMI pending identity/explicitness lost: %+v", record)
					}
					result = f.run(args...)
				}
				mu.Lock()
				ready = true
				mu.Unlock()
				for replay := 0; replay < 3; replay++ {
					result = f.run(args...)
					if result.err != nil {
						t.Fatalf("ready retirement replay failed: %v\n%s", result.err, result.stderr)
					}
				}
				if record := f.record(captureFixtureCheckpoint); record.Capture.Phase != "retired" || record.Native.ImageID != imageID {
					t.Fatalf("retirement did not finish exact AMI: %+v", record)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			want := 1
			if strategy == "disk-snapshot" {
				want = 0
			}
			if creates != want || removes != want || (want == 0 && strings.Contains(strings.Join(actions, ","), "Create")) {
				t.Fatalf("wrong native effects: creates=%d removes=%d actions=%v", creates, removes, actions)
			}
			calls, readErr := os.ReadFile(sshCalls)
			if want == 0 {
				if !os.IsNotExist(readErr) {
					t.Fatalf("unsupported strategy reached SSH: calls=%q err=%v", calls, readErr)
				}
			} else if readErr != nil || string(calls) != "probe\nprepare\n" {
				t.Fatalf("expected one SSH probe then one preparation: calls=%q err=%v", calls, readErr)
			}
		})
	}
}
