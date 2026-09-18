package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestDirectAWSCheckpointPersistsBeforeReadiness(t *testing.T) {
	for _, failPersist := range []bool{false, true} {
		name := "durable identity before wait"
		if failPersist {
			name = "persistence failure stops wait"
		}
		t.Run(name, func(t *testing.T) {
			const checkpointID, imageID, accountID = "chk_0123456789abcdef", "ami-12345678", "123456789012"
			store := checkpointStore{root: t.TempDir()}
			record, _, err := store.Reserve(checkpointRecord{ID: checkpointID, Kind: checkpointKindAWSAMI})
			if err != nil {
				t.Fatal(err)
			}
			var reads atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				switch r.Form.Get("Action") {
				case "GetCallerIdentity":
					writeSTSXML(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>`+accountID+`</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`)
				case "DescribeInstances":
					writeEC2XML(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>i-12345678</instanceId><instanceState><name>running</name></instanceState></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`)
				case "CreateImage":
					writeEC2XML(w, `<CreateImageResponse><imageId>`+imageID+`</imageId></CreateImageResponse>`)
				case "DescribeImages":
					reads.Add(1)
					// A killed process at this network boundary must leave a scoped,
					// recoverable AMI in the ordinary checkpoint store.
					saved, _, readErr := store.Read(checkpointID)
					if readErr != nil || saved.Native.ImageID != imageID || saved.Native.AccountID != accountID || saved.Native.Region != "us-east-1" || !saved.Native.Direct {
						t.Errorf("AMI identity was not durable before readiness: native=%+v err=%v", saved.Native, readErr)
					}
					writeEC2XML(w, `<DescribeImagesResponse><imagesSet><item><imageId>`+imageID+`</imageId><name>checkpoint</name><imageState>available</imageState></item></imagesSet></DescribeImagesResponse>`)
				default:
					http.Error(w, "unexpected operation", http.StatusBadRequest)
				}
			}))
			defer endpoint.Close()
			for key, value := range map[string]string{
				"AWS_ENDPOINT_URL": endpoint.URL, "AWS_ACCESS_KEY_ID": "fixture", "AWS_SECRET_ACCESS_KEY": "fixture",
				"AWS_SESSION_TOKEN": "", "AWS_PROFILE": "", "AWS_EC2_METADATA_DISABLED": "true",
				"AWS_CONFIG_FILE": filepath.Join(t.TempDir(), "config"), "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(t.TempDir(), "credentials"),
			} {
				t.Setenv(key, value)
			}
			persistErr := errors.New("checkpoint store unavailable")
			image, err := (directAWSAMICheckpointDriver{}).Create(context.Background(), NativeCheckpointCreateRequest{
				Config: Config{Provider: "aws", AWSRegion: "us-east-1"}, Server: Server{CloudID: "i-12345678"},
				Target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, Name: "checkpoint",
				Wait: true, WaitTimeout: time.Second, Stderr: io.Discard,
				Persist: func(result NativeCheckpointCreateResult) error {
					if failPersist {
						return persistErr
					}
					return store.WriteNativeProgress(&record, result, true)
				},
			})
			if image.ID != imageID || image.AccountID != accountID {
				t.Fatalf("creation identity lost: %+v", image)
			}
			if failPersist {
				if !errors.Is(err, persistErr) || reads.Load() != 0 {
					t.Fatalf("persistence failure reached readiness: err=%v reads=%d", err, reads.Load())
				}
			} else if err != nil || reads.Load() != 1 {
				t.Fatalf("creation failed: err=%v reads=%d", err, reads.Load())
			}
		})
	}
}
