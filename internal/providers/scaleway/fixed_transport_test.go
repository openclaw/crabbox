package scaleway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fixedTransportClient struct {
	Client
	api InstanceAPI
}

func (c fixedTransportClient) Instance() InstanceAPI { return c.api }

func TestFixedScalewayRootCleanupThroughSDK(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint("foreign=", foreign), func(t *testing.T) {
			backend, fake := newTestBackend(t)
			req := fixedRequest(t)
			lease, err := backend.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			rootID := lease.Server.Labels[rootVolumeLabel]
			volume := fake.volumes[rootID]
			// Reassignment after binding must be refused even after the server is gone.
			volume.Server = nil
			if foreign {
				labels := labelsFromTags(volume.Tags)
				labels["fixed_attempt"] = "other-attempt"
				volume.Tags = tagsFromLabels(labels)
			}
			deletes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/servers/srv-1"):
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"type":"not_found","message":"server is absent"}`))
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/volumes/"+rootID):
					_ = json.NewEncoder(w).Encode(&instance.GetVolumeResponse{Volume: volume})
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/volumes/"+rootID):
					deletes++
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected SDK request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			sdk := fixedSDKAPI(t, server)
			backend.newClient = func(core.Config, core.Runtime) (Client, error) {
				return fixedTransportClient{Client: fake, api: sdk}, nil
			}
			err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
			if foreign {
				if core.ExitCodeForError(err, 1) != 4 || deletes != 0 || fake.deletedKey {
					t.Fatalf("foreign disk mutation: err=%v deletes=%d", err, deletes)
				}
			} else if err != nil || deletes != 1 || !fake.deletedKey {
				t.Fatalf("owned disk cleanup: err=%v deletes=%d", err, deletes)
			}
		})
	}
}

func TestFixedScalewayPublicImageCreateThroughSDK(t *testing.T) {
	backend, fake := newTestBackend(t)
	req := fixedRequest(t)
	// A concrete local image avoids another marketplace lookup inside the SDK.
	backend.cfg.Scaleway.Image = "11111111-1111-4111-8111-111111111111"
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/images/"):
			_ = json.NewEncoder(w).Encode(&instance.GetImageResponse{Image: &instance.Image{Zone: scw.ZoneFrPar1, RootVolume: &instance.VolumeSummary{ID: "public-snapshot", Size: 10_000_000_000}}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/servers"):
			_ = json.NewEncoder(w).Encode(&instance.ListServersResponse{})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/servers"):
			var create instance.CreateServerRequest
			if err := json.NewDecoder(r.Body).Decode(&create); err != nil {
				t.Error(err)
			}
			nonce := labelsFromTags(create.Tags)["fixed_attempt"]
			if create.Image == nil || *create.Image != backend.cfg.Scaleway.Image || len(create.Volumes) != 0 || nonce == "" {
				t.Error("server request lost image or attempt identity")
			}
			creates++
			// Model a successful create with a response the caller cannot bind.
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("public image requested forbidden snapshot/volume operation: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"type":"permissions_denied","message":"read compute_snapshots denied"}`))
		}
	}))
	defer server.Close()
	sdk := fixedSDKAPI(t, server)
	backend.newClient = func(core.Config, core.Runtime) (Client, error) {
		return fixedTransportClient{Client: fake, api: sdk}, nil
	}
	if _, err := backend.Acquire(t.Context(), req); err == nil {
		t.Fatal("expected lost response")
	} else {
		t.Logf("first acquire: %v", err)
	}
	// Even a complete empty inventory cannot authorize another server create.
	if _, err := backend.Acquire(t.Context(), req); err == nil {
		t.Fatal("expected unresolved submission")
	}
	if creates != 1 {
		t.Fatalf("server creates=%d", creates)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent.Attempt["server"] != "submitted" {
		t.Fatalf("submission not retained: %v", err)
	}
}

func fixedSDKAPI(t *testing.T, server *httptest.Server) InstanceAPI {
	t.Helper()
	client, err := scw.NewClient(scw.WithAPIURL(server.URL), scw.WithAuth(testScalewayAccessKey, testScalewaySecretKey), scw.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return instance.NewAPI(client)
}
