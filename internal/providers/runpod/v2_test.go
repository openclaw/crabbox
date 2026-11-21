package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func v2Client(t *testing.T, handler http.HandlerFunc) (runpodAPI, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	api, err := newRunpodClient(core.Config{Runpod: core.RunpodConfig{APIURL: server.URL + "/v2", APIKey: "fixture-token"}}, core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return api, server
}

func TestRunpodV2CreateSchema(t *testing.T) {
	for _, cpu := range []bool{false, true} {
		t.Run(fmt.Sprint(cpu), func(t *testing.T) {
			api, _ := v2Client(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v2/pods" || r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Errorf("request=%s %s", r.Method, r.URL)
				}
				var got map[string]any
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				want := map[string]any{"name": "fixture-pod", "image": "example/image", "cloud": "SECURE", "disk": float64(30), "startSsh": true, "ports": []any{"22/tcp"}, "templateId": "template-fixture", "env": map[string]any{"PUBLIC_KEY": "ssh-ed25519 fixture", "CRABBOX_FIXED_ATTEMPT": "fixture-attempt"}}
				if cpu {
					want["cpu"] = map[string]any{"id": "cpu3c", "vcpuCount": float64(8)}
				} else {
					want["gpu"] = map[string]any{"id": "NVIDIA L4", "count": float64(1)}
					want["mounts"] = map[string]any{"persistent": map[string]any{"size": float64(20), "path": "/workspace"}}
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("create payload=%#v want=%#v", got, want)
				}
				w.WriteHeader(201)
				io.WriteString(w, `{"id":"pod-fixture","name":"fixture-pod","status":"PROVISIONING","image":"example/image"}`)
			})
			instance := "NVIDIA L4"
			if cpu {
				instance = "cpu3c-8"
			}
			pod, err := api.DeployPod(t.Context(), runpodDeployInput{Name: "fixture-pod", ImageName: "example/image", InstanceID: instance, TemplateID: "template-fixture", ContainerDiskInGb: 30, Ports: "22/tcp", PublicKey: "ssh-ed25519 fixture", Env: map[string]string{"CRABBOX_FIXED_ATTEMPT": "fixture-attempt"}})
			if err != nil || pod.ID != "pod-fixture" || pod.Status != "PROVISIONING" {
				t.Fatalf("pod=%+v err=%v", pod, err)
			}
		})
	}
}

func TestRunpodV2PaginationAndOwnership(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var requests []string
	api, _ := v2Client(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RawQuery)
		if r.Method != "GET" || r.URL.Path != "/v2/pods" || r.URL.Query().Get("limit") != "1000" || len(r.URL.Query()) > 2 {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		if r.URL.Query().Get("cursor") == "" {
			io.WriteString(w, `{"pods":[{"id":"manual","name":"manual-pod","status":"RUNNING"}],"pagination":{"hasNextPage":true,"nextCursor":"next +/="}}`)
		} else {
			if r.URL.Query().Get("cursor") != "next +/=" {
				t.Error("cursor corrupted")
			}
			io.WriteString(w, `{"pods":[{"id":"owned","name":"crabbox-blue-12345678","status":"ERROR"}],"pagination":{"hasNextPage":false,"nextCursor":null}}`)
		}
	})
	b := &runpodLeaseBackend{client: api}
	views, err := b.listServersFromClient(t.Context(), api, false)
	if err != nil || len(views) != 1 || views[0].CloudID != "owned" || views[0].Status != "error" || len(requests) != 2 {
		t.Fatalf("views=%+v requests=%v err=%v", views, requests, err)
	}
}

func TestRunpodV2IncompleteInventoryFailsClosed(t *testing.T) {
	for _, last := range []string{`{}`, `{"pods":[]}`, `{"pods":[],"pagination":{}}`, `{"pods":[],"pagination":{"hasNextPage":true,"nextCursor":null}}`, `{"pods":[],"pagination":{"hasNextPage":true,"nextCursor":"next"}}`, `problem`} {
		t.Run(last, func(t *testing.T) {
			api, _ := v2Client(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("cursor") == "" {
					io.WriteString(w, `{"pods":[{"id":"owned","name":"crabbox-blue-12345678"}],"pagination":{"hasNextPage":true,"nextCursor":"next"}}`)
					return
				}
				if last == "problem" {
					w.WriteHeader(403)
					io.WriteString(w, `{"title":"Forbidden","status":403,"detail":"missing pods:read"}`)
					return
				}
				io.WriteString(w, last)
			})
			pods, err := api.ListPods(t.Context())
			if err == nil || pods != nil {
				t.Fatalf("partial inventory escaped: pods=%v err=%v", pods, err)
			}
		})
	}
}

func TestRunpodV2GetDeleteAndProblem(t *testing.T) {
	var methods []string
	api, _ := v2Client(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.URL.Path != "/v2/pods/pod-fixture" {
			t.Errorf("path=%s", r.URL.Path)
		}
		switch r.Method {
		case "GET":
			io.WriteString(w, `{"id":"pod-fixture","status":"RUNNING","desiredStatus":"EXITED","ssh":{"proxy":{"host":"ssh.runpod.io","port":22},"direct":{"host":"203.0.113.3","port":42222,"username":"root"}}}`)
		case "DELETE":
			w.WriteHeader(204)
		}
	})
	pod, err := api.GetPod(t.Context(), "pod-fixture")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := pod.SSHEndpoint()
	if pod.Status != "RUNNING" || runpodServer(pod, "lease", "slug", core.Config{}).Status != "running" || endpoint.Host != "203.0.113.3" || endpoint.Port != 42222 {
		t.Fatalf("pod=%+v endpoint=%+v", pod, endpoint)
	}
	if err := api.TerminatePod(t.Context(), pod.ID); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(methods, []string{"GET", "DELETE"}) {
		t.Fatal(methods)
	}
	for _, code := range []int{400, 402, 403, 404, 422, 429, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			api, _ := v2Client(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(code)
				fmt.Fprintf(w, `{"title":"Problem","status":%d,"detail":"bad fixture-token","errors":["invalid image"]}`, code)
			})
			_, err := api.GetPod(t.Context(), "pod-fixture")
			var apiErr *runpodAPIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != code || !strings.Contains(err.Error(), "invalid image") || !strings.Contains(err.Error(), "Problem: bad") || strings.Contains(err.Error(), "fixture-token") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestRunpodV2ReadinessUsesActualState(t *testing.T) {
	for _, state := range []string{"PROVISIONING", "STARTING", "ERROR", "EXITED", "TERMINATED", "RUNNING"} {
		t.Run(state, func(t *testing.T) {
			calls := 0
			api, _ := v2Client(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				fmt.Fprintf(w, `{"id":"pod-fixture","status":%q,"desiredStatus":"RUNNING","ssh":{"direct":{"host":"203.0.113.3","port":42222}}}`, state)
			})
			b := &runpodLeaseBackend{pollTimeoutOverride: 30 * time.Millisecond, pollInitialOverride: time.Millisecond}
			_, err := b.waitForPodSSH(t.Context(), api, "pod-fixture")
			if (err == nil) != (state == "RUNNING") {
				t.Fatalf("state=%s err=%v", state, err)
			}
			if state == "ERROR" || state == "EXITED" || state == "TERMINATED" {
				if calls != 1 || !strings.Contains(err.Error(), state) {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
			}
		})
	}
}

func TestRunpodV2Overrides(t *testing.T) {
	if core.RunpodConfigDefaultAPIURL != "https://api.runpod.io/v2" {
		t.Fatal(core.RunpodConfigDefaultAPIURL)
	}
	for _, target := range []string{"https://rest.runpod.io/v1", "https://rest.runpod.io/v1/", "https://api.runpod.io/v1"} {
		_, err := newRunpodClient(core.Config{Runpod: core.RunpodConfig{APIURL: target, APIKey: "fixture"}}, core.Runtime{})
		if err == nil || !strings.Contains(err.Error(), "2026-11-15") {
			t.Fatalf("target=%s err=%v", target, err)
		}
	}
	for _, target := range []string{"https://proxy.example/custom", "http://127.0.0.1:1234/v1"} {
		api, err := newRunpodClient(core.Config{Runpod: core.RunpodConfig{APIURL: target, APIKey: "fixture"}}, core.Runtime{})
		if err != nil || api.(*runpodClient).apiURL != target {
			t.Fatalf("target=%s err=%v", target, err)
		}
	}
}

func TestRunpodV2ReleaseFencing(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(fmt.Sprint(owned), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			deletes := 0
			api, _ := v2Client(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case "GET":
					io.WriteString(w, `{"id":"pod-owned","name":"crabbox-blue-12345678","status":"RUNNING"}`)
				case "DELETE":
					deletes++
					w.WriteHeader(204)
				}
			})
			if owned {
				claimRunpodPod(t, "rpod_12345678", "blue", runpodPod{ID: "pod-owned", Name: "crabbox-blue-12345678", Status: "RUNNING"})
			}
			b := &runpodLeaseBackend{client: api, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard}}
			lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "rpod_12345678", ReleaseOnly: true})
			if err == nil {
				err = b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease})
			}
			if owned {
				if err != nil || deletes != 1 {
					t.Fatalf("deletes=%d err=%v", deletes, err)
				}
			} else if err == nil || deletes != 0 {
				t.Fatalf("unowned deletion: deletes=%d err=%v", deletes, err)
			}
		})
	}
}

func TestRunpodV2AmbiguousLaterPageBlocksResolution(t *testing.T) {
	api, _ := v2Client(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected mutation: %s", r.Method)
		}
		if r.URL.Query().Get("cursor") == "" {
			io.WriteString(w, `{"pods":[{"id":"one","name":"same-name"}],"pagination":{"hasNextPage":true,"nextCursor":"next"}}`)
		} else {
			io.WriteString(w, `{"pods":[{"id":"two","name":"same-name"}],"pagination":{"hasNextPage":false,"nextCursor":null}}`)
		}
	})
	b := &runpodLeaseBackend{client: api}
	_, err := b.findPodByName(t.Context(), api, "same-name")
	if err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("ambiguous inventory accepted: %v", err)
	}
}

func TestRunpodV2InvalidCPUCountDoesNotSubmit(t *testing.T) {
	api, _ := v2Client(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid CPU request submitted") })
	for _, instance := range []string{"cpu3c-1", "cpu3c-3", "cpu3c-garbage", "cpu3c-8-extra"} {
		if _, err := api.DeployPod(t.Context(), runpodDeployInput{InstanceID: instance}); err == nil {
			t.Errorf("accepted %s", instance)
		}
	}
}
