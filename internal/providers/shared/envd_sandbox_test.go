package shared

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"testing"
)

func TestEnvdSandboxControlPaginationRetainsWireQueryAndDiscardsPartialFailure(t *testing.T) {
	metadata := map[string]string{"phase": "first"}
	failure := errors.New("later page failed")
	requests := 0
	control := EnvdSandboxControl{Request: func(ctx context.Context, method, path string, query url.Values, body, out any) (http.Header, error) {
		requests++
		if method != http.MethodGet || path != "/v2/sandboxes" || query.Get("limit") != "100" || query.Get("state") != "running,paused" || body != nil {
			t.Fatalf("unexpected request: %s %s %v", method, path, query)
		}
		if requests == 1 {
			if query.Get("metadata") != "phase=first" || query.Has("nextToken") {
				t.Fatalf("first query: %v", query)
			}
			*out.(*[]EnvdSandbox) = []EnvdSandbox{{SandboxID: "first"}}
			metadata["phase"] = "second"
			return http.Header{"X-Next-Token": {" next "}}, nil
		}
		if requests != 2 || query.Get("nextToken") != " next " || query.Get("metadata") != "phase=second" {
			t.Fatalf("second query: %v", query)
		}
		return nil, failure
	}}
	got, err := control.ListSandboxes(t.Context(), metadata)
	if got != nil || err != failure || requests != 2 {
		t.Fatalf("result=%v error=%v requests=%d", got, err, requests)
	}
}

func TestEnvdSandboxControlValidatesRequestedIdentityAndConnectDefault(t *testing.T) {
	for _, operation := range []string{"get", "connect"} {
		for _, returnedID := range []string{"", "other", "id/one"} {
			t.Run(operation+"/"+returnedID, func(t *testing.T) {
				control := EnvdSandboxControl{Request: func(ctx context.Context, method, path string, query url.Values, body, out any) (http.Header, error) {
					wantMethod, wantPath := http.MethodGet, "/sandboxes/id%2Fone"
					if operation == "connect" {
						wantMethod, wantPath = http.MethodPost, wantPath+"/connect"
						if !reflect.DeepEqual(body, map[string]any{"timeout": 300}) {
							t.Fatalf("connect body=%#v", body)
						}
					}
					if method != wantMethod || path != wantPath || query != nil {
						t.Fatalf("request=%s %s %v", method, path, query)
					}
					*out.(*EnvdSandbox) = EnvdSandbox{SandboxID: returnedID}
					return nil, nil
				}}
				var result EnvdSandbox
				var err error
				if operation == "get" {
					result, err = control.GetSandbox(t.Context(), "id/one")
				} else {
					result, err = control.ConnectSandbox(t.Context(), "id/one", -1)
				}
				if returnedID != "id/one" {
					if err == nil || result.SandboxID != "" {
						t.Fatalf("unbound response accepted: %+v %v", result, err)
					}
				} else if err != nil || (operation == "get" && result.Metadata == nil) {
					t.Fatalf("bound result=%+v error=%v", result, err)
				}
			})
		}
	}
}
