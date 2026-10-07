package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestWarmupCreationRejectedTiming(t *testing.T) {
	const leaseID = "cbx_0123456789ab"
	for _, tc := range []struct {
		name, provider, body string
		status               int
		redirect             int
		flags                []string
		withoutTiming        bool
		withoutFixedID       bool
		ambiguousFirst       bool
		acceptedFirst        bool
		partialOutput        bool
		shortBody            bool
		wantRejected         bool
	}{
		{name: "fixed AWS", provider: "aws", wantRejected: true},
		{name: "fixed Azure", provider: "azure", wantRejected: true},
		{name: "resource constrained", provider: "aws", flags: []string{"--min-vcpus", "4"}, wantRejected: true},
		{name: "timing opt out", provider: "aws", withoutTiming: true},
		{name: "generated identity", provider: "aws", withoutFixedID: true},
		{name: "other rate limit", provider: "aws", body: `{"error":"rate_limited"}`},
		{name: "wrong status", provider: "aws", status: http.StatusForbidden},
		{name: "malformed body", provider: "aws", body: `{"error":"cost_limit_exceeded"`},
		{name: "duplicate code", provider: "aws", body: `{"error":"unknown","error":"cost_limit_exceeded"}`},
		{name: "case folded code override", provider: "aws", body: `{"error":"unknown","Error":"cost_limit_exceeded"}`},
		{name: "missing exact code key", provider: "aws", body: `{"Error":"cost_limit_exceeded"}`},
		{name: "redirect changes method", provider: "aws", redirect: http.StatusFound},
		{name: "redirect changes path", provider: "aws", redirect: http.StatusTemporaryRedirect},
		{name: "truncated response", provider: "aws", body: `{"error":"cost_limit_exceeded"}` + strings.Repeat(" ", 600) + `invalid`},
		{name: "incomplete response", provider: "aws", shortBody: true},
		{name: "ambiguous then denied", provider: "aws", ambiguousFirst: true},
		{name: "accepted then denied", provider: "aws", acceptedFirst: true},
		{name: "partial timing write", provider: "aws", partialOutput: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			isolateTestUserDirs(t)
			t.Chdir(t.TempDir())
			t.Setenv("CRABBOX_AWS_SSH_CIDRS", "192.0.2.1/32")
			oldInterval := coordinatorCreateLeaseRecoveryInterval
			coordinatorCreateLeaseRecoveryInterval = time.Millisecond
			t.Cleanup(func() { coordinatorCreateLeaseRecoveryInterval = oldInterval })
			var creates, reads, redirects atomic.Int32
			path := "/v1/leases/" + leaseID
			method := http.MethodPut
			if len(tc.flags) > 0 {
				path += "/resource-constrained"
			}
			if tc.withoutFixedID {
				method, path = http.MethodPost, "/v1/leases"
			}
			body := tc.body
			if body == "" {
				body = `{"error":"cost_limit_exceeded","message":"quota diagnostic sentinel"}`
			}
			status := tc.status
			if status == 0 {
				status = http.StatusTooManyRequests
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == method && r.URL.Path == path:
					n := creates.Add(1)
					if tc.redirect != 0 {
						http.Redirect(w, r, "/denied", tc.redirect)
						return
					}
					if n == 1 && tc.ambiguousFirst {
						http.Error(w, "uncertain create", http.StatusBadGateway)
						return
					}
					if n == 1 && tc.acceptedFirst {
						_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
							ID: leaseID, Provider: tc.provider, TargetOS: targetLinux, State: "provisioning",
						}})
						return
					}
				case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+leaseID && tc.acceptedFirst:
					reads.Add(1)
				case r.URL.Path == "/denied" && tc.redirect != 0:
					redirects.Add(1)
					wantMethod := http.MethodPut
					if tc.redirect == http.StatusFound {
						wantMethod = http.MethodGet
					}
					if r.Method != wantMethod {
						t.Errorf("redirect method=%s want %s", r.Method, wantMethod)
					}
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				if tc.shortBody {
					w.Header().Set("Content-Length", strconv.Itoa(len(body)+1))
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(server.Close)
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "fixture-token")
			args := []string{"--provider", tc.provider, "--network", "public"}
			if !tc.withoutFixedID {
				args = append(args, "--lease-id", leaseID)
			}
			if !tc.withoutTiming {
				args = append(args, "--timing-json")
			}
			args = append(args, tc.flags...)
			var stdout, stderr bytes.Buffer
			var timingSink io.Writer = &stderr
			if tc.partialOutput {
				timingSink = partialRejectionTimingWriter{&stderr}
			}
			err := (App{Stdout: &stdout, Stderr: timingSink}).warmup(context.Background(), args)
			var response CoordinatorHTTPError
			wantMessage := body
			if len(wantMessage) > 600 {
				wantMessage = strings.TrimSpace(wantMessage[:600])
			}
			if !errors.As(err, &response) || response.StatusCode != status || response.Message != wantMessage {
				t.Fatalf("original rejection lost: %v; stderr=%s", err, stderr.String())
			}
			wantCreates := int32(1)
			if tc.ambiguousFirst {
				wantCreates++
			}
			if creates.Load() != wantCreates || (reads.Load() > 0) != tc.acceptedFirst {
				t.Fatalf("creates=%d reads=%d", creates.Load(), reads.Load())
			}
			if (redirects.Load() == 1) != (tc.redirect != 0) {
				t.Fatalf("redirects=%d", redirects.Load())
			}
			if tc.partialOutput {
				if !errors.Is(err, io.ErrClosedPipe) || strings.HasSuffix(stderr.String(), "\n") {
					t.Fatalf("partial write lost sink error or completed record: %v; %s", err, stderr.String())
				}
				return
			}
			records := 0
			for _, line := range strings.Split(stderr.String(), "\n") {
				if !strings.HasPrefix(line, "{") {
					continue
				}
				records++
				var report map[string]json.RawMessage
				if err := json.Unmarshal([]byte(line), &report); err != nil {
					t.Fatal(err)
				}
				var rejected struct {
					Version          int    `json:"version"`
					RequestedLeaseID string `json:"requestedLeaseId"`
					Code             string `json:"code"`
				}
				if err := json.Unmarshal(report["creationRejected"], &rejected); err != nil {
					t.Fatal(err)
				}
				if rejected.Version != 1 || rejected.RequestedLeaseID != leaseID || rejected.Code != "cost_limit_exceeded" ||
					string(report["provider"]) != `"`+tc.provider+`"` || string(report["exitCode"]) != "1" {
					t.Fatalf("unexpected rejection: %s", line)
				}
				for _, key := range []string{"leaseId", "slug", "failureEvidence", "leaseStopped"} {
					if _, exists := report[key]; exists {
						t.Errorf("rejection contains allocated identity or cleanup field %s", key)
					}
				}
				if strings.Contains(line, "diagnostic sentinel") || strings.Contains(line, "fixture-token") {
					t.Fatal("timing record leaked response or credential material")
				}
			}
			if (records == 1) != tc.wantRejected || records > 1 || stdout.Len() != 0 {
				t.Fatalf("records=%d wantRejected=%t stdout=%s stderr=%s", records, tc.wantRejected, stdout.String(), stderr.String())
			}
		})
	}
}

type partialRejectionTimingWriter struct{ *bytes.Buffer }

func (w partialRejectionTimingWriter) Write(data []byte) (int, error) {
	if len(data) > 0 && data[0] == '{' {
		n, _ := w.Buffer.Write(data[:len(data)/2])
		return n, io.ErrClosedPipe
	}
	return w.Buffer.Write(data)
}

func TestCoordinatorCreationRejectionExcludesExpiredContexts(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller cancellation", true: "create deadline"}[deadline], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := baseConfig()
				cfg.Provider = "aws"
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				oldTimeout := coordinatorCreateLeaseTimeoutForConfig
				coordinatorCreateLeaseTimeoutForConfig = func(Config) time.Duration { return time.Second }
				t.Cleanup(func() { coordinatorCreateLeaseTimeoutForConfig = oldTimeout })
				coord := &CoordinatorClient{BaseURL: "http://coordinator.test", Client: &http.Client{
					Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
						if deadline {
							time.Sleep(2 * time.Second)
						} else {
							cancel()
						}
						return &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header),
							Body: io.NopCloser(strings.NewReader(`{"error":"cost_limit_exceeded"}`))}, nil
					}),
				}}
				backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord, rt: Runtime{Stderr: io.Discard}}
				_, err := backend.createCoordinatorLeaseWithProgressMode(ctx, cfg, "ssh-ed25519 fixture", true,
					"cbx_0123456789ab", "fixture", true)
				var rejected coordinatorCreationRejectedError
				if err == nil || errors.As(err, &rejected) {
					t.Fatalf("expired context yielded authoritative rejection: %v", err)
				}
				var response CoordinatorHTTPError
				if !errors.As(err, &response) {
					t.Fatalf("original response diagnostic lost: %v", err)
				}
			})
		})
	}
}
