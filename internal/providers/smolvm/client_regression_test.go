package smolvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUploadRejectsUnsafeStagingDirectory(t *testing.T) {
	for _, dir := range []string{
		"/", "/workspace", "/tmp/.crabbox-upload.short",
		"/tmp/.crabbox-upload.1234567890/../../workspace",
		"/tmp/../.crabbox-upload.1234567890",
		"/tmp//.crabbox-upload.1234567890",
		"/tmp/.crabbox-upload.1234567890\n/other",
		"/tmp/.crabbox-upload.1234567890\x00",
	} {
		t.Run(dir, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests != 1 || r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/exec") {
					t.Errorf("unsafe staging path triggered %s %s", r.Method, r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"stdout": dir + "\n", "exitCode": 0})
			}))
			defer server.Close()
			c := &client{base: server.URL, http: server.Client()}
			if err := c.WriteFile(t.Context(), "fixture", "/workspace/env", "value"); err == nil || !strings.Contains(err.Error(), "staging") {
				t.Fatalf("unsafe staging directory accepted: %v", err)
			}
			if requests != 1 {
				t.Fatalf("made %d requests, want only staging exec", requests)
			}
		})
	}
}

func TestUploadEncodesGuestPathAndCleansPartialUpload(t *testing.T) {
	const dir = "/work space'?#%2F+ü/.crabbox-upload.1234567890"
	const escaped = "/v1/machines/machine%2Fid/files/work%20space%27%3F%23%252F+%C3%BC/.crabbox-upload.1234567890/payload"
	const secret = "smk_synthetic-upload-test"
	for _, failure := range []string{"success", "rejected", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var uploaded, consumed, cleaned bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+secret {
					t.Error("missing upload authentication")
				}
				if r.Method == http.MethodPut {
					uploaded = true
					if r.RequestURI != escaped || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/octet-stream" || r.ContentLength != 5 {
						t.Errorf("unexpected upload: %s, type=%q length=%d", r.RequestURI, r.Header.Get("Content-Type"), r.ContentLength)
					}
					body, err := io.ReadAll(r.Body)
					if err != nil || string(body) != "value" {
						t.Errorf("payload=%q error=%v", body, err)
					}
					if failure == "rejected" {
						http.Error(w, "partial upload: "+secret, http.StatusForbidden)
					} else if failure == "canceled" {
						cancel()
						<-r.Context().Done()
					} else {
						w.WriteHeader(http.StatusOK)
					}
					return
				}
				var req struct{ Command string }
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				out := ""
				switch {
				case strings.Contains(req.Command, "mktemp -d"):
					out = dir + "\n"
				case strings.HasPrefix(req.Command, "rm -rf -- "):
					cleaned = true
					if failure == "canceled" && ctx.Err() == nil {
						t.Error("cleanup should run after caller cancellation")
					}
				case strings.Contains(req.Command, "mv -f"):
					consumed = true
				default:
					t.Errorf("unexpected command: %s", req.Command)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"stdout": out, "exitCode": 0})
			}))
			defer server.Close()
			c := &client{apiKey: secret, base: server.URL, http: server.Client()}
			err := c.WriteFile(ctx, "machine/id", "/workspace/env", "value")
			if failure == "success" {
				if err != nil || !uploaded || !consumed || cleaned {
					t.Fatalf("successful upload: err=%v uploaded=%v consumed=%v cleaned=%v", err, uploaded, consumed, cleaned)
				}
			} else {
				if err == nil || strings.Contains(err.Error(), secret) || !uploaded || consumed || !cleaned {
					t.Fatalf("failed upload: err=%v uploaded=%v consumed=%v cleaned=%v", err, uploaded, consumed, cleaned)
				}
				if failure == "canceled" && !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
				if failure == "rejected" && !strings.Contains(err.Error(), "[redacted]") {
					t.Fatalf("lost redacted provider error: %v", err)
				}
			}
		})
	}
}

func TestExecExactEmptyOutputAndStderr(t *testing.T) {
	for _, tc := range []struct {
		name      string
		response  string
		want      string
		wantError bool
	}{
		{"empty stdout", `{"stdout":"lossy","stdoutB64":""}`, "", false},
		{"binary stderr", `{"stderr":"lossy","stderrB64":"AP8=","stderrTruncated":true}`, "\x00\xff", false},
		{"truncated stderr", `{"stderr":"partial","stderrTruncated":true}`, "partial\ncrabbox: the smolvm API truncated stderr and sent no complete copy; rerun with reduced verbosity or save output to a guest file\n", false},
		{"invalid base64", `{"stdoutB64":"!"}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()
			c := &client{base: server.URL, http: server.Client()}
			var out bytes.Buffer
			_, err := c.ExecStream(t.Context(), "fixture", "true", "", &out)
			if (err != nil) != tc.wantError || out.String() != tc.want {
				t.Fatalf("output=%q err=%v, want output=%q error=%v", out.String(), err, tc.want, tc.wantError)
			}
		})
	}
}
