package freestyle

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestFreestyleExecExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		code     int
		timedOut bool
	}{
		{"zero", `{"statusCode":0,"stdout":"before\n","stderr":"diagnostic\n"}`, 0, false},
		{"nonzero", `{"statusCode":7,"stdout":"before\n","stderr":"diagnostic\n"}`, 7, false},
		{"null", `{"statusCode":null,"stdout":"before\n","stderr":"diagnostic\n"}`, 1, true},
		{"absent", `{"stdout":"before\n","stderr":"diagnostic\n"}`, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/vms/vm-fixture/exec-await" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()
			client := &freestyleHTTPClient{apiKey: "synthetic-test-key", apiURL: server.URL, httpClient: server.Client()}
			var stdout, stderr bytes.Buffer
			code, err := client.Exec(t.Context(), "vm-fixture", "fixture-command", &stdout, &stderr)
			if code != tc.code || (err != nil) != tc.timedOut || errors.Is(err, context.DeadlineExceeded) != tc.timedOut {
				t.Errorf("Exec code=%d err=%v, want code=%d timedOut=%t", code, err, tc.code, tc.timedOut)
			}
			if tc.timedOut && (err == nil || !strings.Contains(err.Error(), "timed out")) {
				t.Errorf("timeout error lacks diagnosis: %v", err)
			}
			if stdout.String() != "before\n" || stderr.String() != "diagnostic\n" {
				t.Fatalf("captured output lost: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestFreestyleRunHTTPExitStatusRetention(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		code     int
		status   core.RunStatus
		kind     core.RunErrorKind
	}{
		{"zero", `{"statusCode":0,"stdout":"before\n","stderr":"diagnostic\n"}`, 0, core.RunStatusSucceeded, core.RunErrorNone},
		{"nonzero", `{"statusCode":7,"stdout":"before\n","stderr":"diagnostic\n"}`, 7, core.RunStatusFailed, core.RunErrorCommandExit},
		{"null", `{"statusCode":null,"stdout":"before\n","stderr":"diagnostic\n"}`, 1, core.RunStatusTimedOut, core.RunErrorTimeout},
		{"absent", `{"stdout":"before\n","stderr":"diagnostic\n"}`, 1, core.RunStatusTimedOut, core.RunErrorTimeout},
	} {
		for _, keepOnFailure := range []bool{false, true} {
			name := tc.name + "/cleanup"
			if keepOnFailure {
				name = tc.name + "/keep-on-failure"
			}
			t.Run(name, func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				execs, deletes := 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodPost && r.URL.Path == "/v1/vms":
						_, _ = io.WriteString(w, `{"id":"vm-fixture"}`)
					case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-fixture/exec-await":
						execs++
						if execs == 1 {
							_, _ = io.WriteString(w, `{"statusCode":0}`)
						} else {
							_, _ = io.WriteString(w, tc.response)
						}
					case r.Method == http.MethodDelete && r.URL.Path == "/v1/vms/vm-fixture":
						deletes++
						w.WriteHeader(http.StatusNoContent)
					default:
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				var stdout, stderr bytes.Buffer
				backend := &freestyleBackend{
					spec: Provider{}.Spec(),
					cfg:  core.Config{Freestyle: core.FreestyleConfig{APIKey: "synthetic-test-key", APIURL: server.URL}},
					rt:   core.Runtime{Stdout: &stdout, Stderr: &stderr, HTTP: server.Client()},
				}
				result, err := backend.Run(t.Context(), core.RunRequest{
					Repo: core.Repo{Root: t.TempDir(), Name: "fixture"}, NoSync: true, KeepOnFailure: keepOnFailure,
					TimingJSON: true, Command: []string{"fixture-command"},
				})
				kept := result.Session != nil && result.Session.Kept
				t.Logf("code=%d status=%s kind=%s err=%v kept=%t deletes=%d", result.ExitCode, result.Status, result.ErrorKind, err, kept, deletes)
				if result.ExitCode != tc.code || result.Status != tc.status || result.ErrorKind != tc.kind || (err != nil) != (tc.code != 0) {
					t.Errorf("outcome=%#v err=%v, want code=%d status=%s kind=%s", result, err, tc.code, tc.status, tc.kind)
				}
				if errors.Is(err, context.DeadlineExceeded) != (tc.kind == core.RunErrorTimeout) || t.Context().Err() != nil {
					t.Errorf("timeout cause/context changed: err=%v context=%v", err, t.Context().Err())
				}
				wantKept := keepOnFailure && tc.code != 0
				wantDeletes := 1
				if wantKept {
					wantDeletes = 0
				}
				if result.Session == nil || kept != wantKept || result.Session.Reused || deletes != wantDeletes || execs != 2 {
					t.Errorf("disposition: session=%#v deletes=%d execs=%d, want kept=%t deletes=%d execs=2", result.Session, deletes, execs, wantKept, wantDeletes)
				}
				if _, exists, err := core.ReadLeaseClaimWithPresence(result.LeaseID); err != nil || exists != wantKept {
					t.Errorf("claim exists=%t err=%v, want exists=%t", exists, err, wantKept)
				}
				if stdout.String() != "before\n" || !strings.Contains(stderr.String(), "diagnostic\n") {
					t.Errorf("captured output lost: stdout=%q stderr=%q", stdout.String(), stderr.String())
				}
				assertFreestyleLifecycleTiming(t, stderr.String(), result, err)
			})
		}
	}
}
