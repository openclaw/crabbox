package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"google.golang.org/api/option"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestRootDiskDefaultsRemainUnset(t *testing.T) {
	cfg := BaseConfig()
	if cfg.AWSRootGB != 0 || cfg.GCP.RootGB != 0 {
		t.Fatalf("implicit root sizes must remain unset: aws=%d gcp=%d", cfg.AWSRootGB, cfg.GCP.RootGB)
	}
}

func TestAWSRootDiskClassAndMinimum(t *testing.T) {
	for _, tc := range []struct {
		class                   string
		minimum, explicit, want int
	}{
		{"tiny", 10, 0, 40}, {"small", 10, 0, 80}, {"standard", 10, 0, 150},
		{"fast", 10, 0, 150}, {"large", 10, 0, 250}, {"beast", 10, 0, 400},
		{"tiny", 100, 0, 100}, {"beast", 500, 0, 500}, {"tiny", 100, 8, 8}, {"tiny", 10, 400, 400},
	} {
		t.Run(fmt.Sprintf("%s/min%d/explicit%d", tc.class, tc.minimum, tc.explicit), func(t *testing.T) {
			var got string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				switch r.Form.Get("Action") {
				case "DescribeImages":
					writeEC2XML(w, fmt.Sprintf(`<DescribeImagesResponse><imagesSet><item><imageId>ami-test</imageId><rootDeviceName>/dev/xvda</rootDeviceName><blockDeviceMapping><item><deviceName>/dev/xvda</deviceName><ebs><volumeSize>%d</volumeSize></ebs></item></blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`, tc.minimum))
				case "RunInstances":
					got = r.Form.Get("BlockDeviceMapping.1.Ebs.VolumeSize")
					writeEC2XML(w, `<RunInstancesResponse><instancesSet><item><instanceId>i-test</instanceId><instanceState><name>running</name></instanceState></item></instancesSet></RunInstancesResponse>`)
				default:
					t.Errorf("unexpected action %s", r.Form.Get("Action"))
				}
			}))
			defer server.Close()
			cfg := BaseConfig()
			cfg.Class, cfg.AWSRootGB = tc.class, int32(tc.explicit)
			_, err := testAWSClient(server.URL).createServer(context.Background(), cfg, "ssh-ed25519 test", "cbx_abcdef123456", "test", false, "ami-test", "sg-test", false, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got != strconv.Itoa(tc.want) {
				t.Fatalf("root=%s want=%d", got, tc.want)
			}
		})
	}
}

func TestGCPRootDiskClassAndMinimum(t *testing.T) {
	for _, tc := range []struct {
		class                   string
		minimum, explicit, want int64
	}{
		{"tiny", 10, 0, 40}, {"small", 10, 0, 80}, {"standard", 10, 0, 150},
		{"fast", 10, 0, 150}, {"large", 10, 0, 250}, {"beast", 10, 0, 400},
		{"tiny", 100, 0, 100}, {"beast", 500, 0, 500}, {"tiny", 100, 8, 8},
	} {
		t.Run(fmt.Sprintf("%s/min%d/explicit%d", tc.class, tc.minimum, tc.explicit), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/compute/v1/projects/source/global/images/family/test" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"name":"resolved","diskSizeGb":"%d"}`, tc.minimum)
			}))
			defer server.Close()
			client := &GCPClient{Image: "projects/source/global/images/family/test", RootGB: tc.explicit, clientOptions: []option.ClientOption{option.WithoutAuthentication(), option.WithEndpoint(server.URL)}}
			image, got, err := client.resolveRootDisk(context.Background(), tc.class)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("root=%d want=%d", got, tc.want)
			}
			if tc.explicit == 0 && image != "projects/source/global/images/resolved" {
				t.Fatalf("family not pinned: %s", image)
			}
		})
	}
}

func TestCoordinatorAWSRootSizeIntent(t *testing.T) {
	for _, size := range []int32{0, 8, 400} {
		t.Run(strconv.Itoa(int(size)), func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"lease":{"id":"cbx_123","provider":"aws","state":"active","host":"192.0.2.10"}}`)
			}))
			defer server.Close()
			cfg := BaseConfig()
			cfg.Provider, cfg.AWSRootGB = "aws", size
			client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
			if _, err := client.CreateLease(context.Background(), cfg, "ssh-ed25519 test", false, "cbx_123", "test"); err != nil {
				t.Fatal(err)
			}
			if body["awsRootGB"] != float64(size) {
				t.Fatalf("sent root %v, want %d", body["awsRootGB"], size)
			}
		})
	}
}

func TestGCPRootDiskProjectRelativeImage(t *testing.T) {
	for _, source := range []string{"global/images/test", "global/images/family/test"} {
		t.Run(source, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/compute/v1/projects/selected/"+source {
					t.Errorf("unexpected source lookup: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"name":"resolved","diskSizeGb":"100"}`)
			}))
			defer server.Close()
			client := &GCPClient{Project: "selected", Image: source, clientOptions: []option.ClientOption{option.WithoutAuthentication(), option.WithEndpoint(server.URL)}}
			image, size, err := client.resolveRootDisk(context.Background(), "tiny")
			if err != nil {
				t.Fatal(err)
			}
			if image != "projects/selected/global/images/resolved" || size != 100 {
				t.Fatalf("image=%s root=%d", image, size)
			}
		})
	}
}
