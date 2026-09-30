package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmptyJSONCoordinatorLists(t *testing.T) {
	for _, tc := range []struct {
		args   string
		method string
		path   string
		field  string
	}{
		{"list --provider gcp", "GET", "/v1/leases", "leases"},
		{"list --provider gcp --all", "GET", "/v1/leases", "leases"},
		{"pool list --provider gcp", "GET", "/v1/leases", "leases"},
		{"history", "GET", "/v1/runs", "runs"},
		{"events run_123", "GET", "/v1/runs/run_123/events", "events"},
		{"events run_123 --type stdout", "GET", "/v1/runs/run_123/events", "events"},
		{"events cbx_abcdef123456", "GET", "/v1/leases/cbx_abcdef123456/events", "events"},
		{"pool ready", "GET", "/v1/ready-pools", "pools"},
		{"pool ready empty", "GET", "/v1/ready-pools/empty", "pool"},
		{"admin leases", "GET", "/v1/admin/leases", "leases"},
		{"admin lease-audit", "GET", "/v1/admin/lease-audit", "audits"},
		{"admin hosts list", "GET", "/v1/admin/hosts", "hosts"},
		{"admin hosts offerings", "GET", "/v1/admin/hosts/offerings", "offerings"},
		{"admin hosts quota", "GET", "/v1/admin/hosts/quota", "quotas"},
		{"admin hosts allocate --dry-run", "POST", "/v1/admin/hosts/dry-run", "checks"},
		{"admin hosts allocate --force", "POST", "/v1/admin/hosts", "hosts"},
		{"admin hosts release h-fixture --force", "DELETE", "/v1/admin/hosts/h-fixture", "released"},
	} {
		for _, shape := range []string{"omitted", "null", "[]"} {
			t.Run(tc.args+"/"+shape, func(t *testing.T) {
				clearConfigEnv(t)
				requests := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					if r.Method != tc.method || r.URL.Path != tc.path {
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if shape == "omitted" {
						fmt.Fprint(w, `{}`)
					} else {
						fmt.Fprintf(w, `{"%s":%s}`, tc.field, shape)
					}
				}))
				defer server.Close()
				t.Setenv("CRABBOX_COORDINATOR", server.URL)
				t.Setenv("CRABBOX_COORDINATOR_TOKEN", "fixture-token")
				var stdout, stderr bytes.Buffer
				app := App{Stdout: &stdout, Stderr: &stderr}
				if err := app.Run(context.Background(), append(strings.Fields(tc.args), "--json")); err != nil {
					t.Fatalf("command failed: %v; stderr=%s", err, &stderr)
				}
				if requests == 0 {
					t.Fatal("command did not contact the fixture coordinator")
				}
				if got := stdout.String(); got != "[]\n" {
					t.Fatalf("got %q, want [] followed by newline", got)
				}
			})
		}
	}
}

func TestEmptyJSONLocalLists(t *testing.T) {
	for _, args := range []string{
		"cache volumes", "checkpoint list --local-only", "checkpoint list --local-only --verify",
	} {
		t.Run(args, func(t *testing.T) {
			clearConfigEnv(t)
			var stdout bytes.Buffer
			app := App{Stdout: &stdout, Stderr: io.Discard}
			if err := app.Run(context.Background(), append(strings.Fields(args), "--json")); err != nil {
				t.Fatal(err)
			}
			if got := stdout.String(); got != "[]\n" {
				t.Fatalf("got %q, want [] followed by newline", got)
			}
		})
	}
}

type emptyJSONListBackend struct {
	testSSHBackend
	view any
}

func (b emptyJSONListBackend) ListJSON(context.Context, ListRequest) (any, error) {
	return b.view, nil
}

func TestEmptyJSONProviderLists(t *testing.T) {
	base := testSSHBackend{spec: testAWSProvider{}.Spec()}
	for _, tc := range []struct {
		name    string
		backend SSHLeaseBackend
		want    string
	}{
		{"direct", base, "[]\n"},
		{"untyped nil", emptyJSONListBackend{testSSHBackend: base}, "[]\n"},
		{"typed nil slice", emptyJSONListBackend{base, []CoordinatorLease(nil)}, "[]\n"},
		{"typed nil map", emptyJSONListBackend{base, map[string]any(nil)}, "{}\n"},
		{"nonempty", emptyJSONListBackend{base, []map[string]any{{"id": int64(9007199254740993), "unknown": nil}}}, "[{\"id\":9007199254740993,\"unknown\":null}]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			testAWSBackendOverride = tc.backend
			t.Cleanup(func() { testAWSBackendOverride = nil })
			var stdout bytes.Buffer
			if err := (App{Stdout: &stdout, Stderr: io.Discard}).Run(context.Background(), []string{"list", "--provider", "aws", "--json"}); err != nil {
				t.Fatal(err)
			}
			if got := stdout.String(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

type emptyJSONSizeBackend struct{ testSSHBackend }

func (emptyJSONSizeBackend) SizeCatalog(context.Context, bool) ([]ProviderSize, error) {
	return nil, nil
}

func (emptyJSONSizeBackend) SizeSelection() ProviderSizeSelection {
	return ProviderSizeSelection{}
}

type emptyJSONSizeProvider struct{ testAWSProvider }

func (p emptyJSONSizeProvider) Spec() ProviderSpec {
	spec := p.testAWSProvider.Spec()
	spec.SizeSelection = "native"
	return spec
}

func TestEmptyJSONProviderSizes(t *testing.T) {
	clearConfigEnv(t)
	original := providerRegistry["aws"]
	providerRegistry["aws"] = emptyJSONSizeProvider{}
	t.Cleanup(func() { providerRegistry["aws"] = original })
	testAWSBackendOverride = emptyJSONSizeBackend{testSSHBackend{spec: testAWSProvider{}.Spec()}}
	t.Cleanup(func() { testAWSBackendOverride = nil })
	for _, withContext := range []bool{false, true} {
		t.Run(fmt.Sprintf("with-context=%t", withContext), func(t *testing.T) {
			args := []string{"providers", "sizes", "aws", "--json"}
			if withContext {
				args = append(args, "--with-context")
			}
			var stdout bytes.Buffer
			if err := (App{Stdout: &stdout, Stderr: io.Discard}).Run(context.Background(), args); err != nil {
				t.Fatal(err)
			}
			got := strings.TrimSpace(stdout.String())
			if withContext {
				var result map[string]json.RawMessage
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				got = string(result["sizes"])
			}
			if got != "[]" {
				t.Fatalf("sizes=%s, want []", got)
			}
		})
	}
}

func TestEmptyJSONReadyPoolLegacyEntries(t *testing.T) {
	for _, minReady := range []string{"0", "1"} {
		t.Run(minReady, func(t *testing.T) {
			clearConfigEnv(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" && r.URL.Path == "/v1/ready-pools/empty/reconcile" {
					http.NotFound(w, r)
					return
				}
				if r.Method != "GET" || r.URL.Path != "/v1/ready-pools/empty" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				fmt.Fprint(w, `{"pool":null}`)
			}))
			defer server.Close()
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "fixture-token")
			var stdout bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: io.Discard}).Run(context.Background(), []string{"pool", "ensure", "empty", "--min-ready", minReady, "--json"})
			if minReady == "0" && err != nil || minReady == "1" && ExitCodeForError(err, 0) != 5 {
				t.Fatalf("unexpected result: %v", err)
			}
			var result map[string]json.RawMessage
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if got := string(result["entries"]); got != "[]" {
				t.Fatalf("entries=%s, want []", got)
			}
		})
	}
}

type emptyJSONPortsBackend struct{ testSSHBackend }

func (emptyJSONPortsBackend) Ports(context.Context, PortsRequest) (string, error) {
	return "null", nil
}

func TestEmptyJSONPorts(t *testing.T) {
	clearConfigEnv(t)
	testAWSBackendOverride = emptyJSONPortsBackend{testSSHBackend{spec: testAWSProvider{}.Spec()}}
	t.Cleanup(func() { testAWSBackendOverride = nil })
	var stdout bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: io.Discard}).Run(context.Background(), []string{"ports", "--provider", "aws", "--id", "cbx_abcdef123456", "--json"}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "[]\n" {
		t.Fatalf("got %q, want [] followed by newline", got)
	}
}
