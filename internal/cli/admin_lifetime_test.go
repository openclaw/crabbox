package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminLifetimeCommands(t *testing.T) {
	for _, command := range []string{"extend-lifetime", "lifetime-readback"} {
		for _, jsonOutput := range []bool{true, false} {
			t.Run(command+map[bool]string{true: "/json", false: "/human"}[jsonOutput], func(t *testing.T) {
				clearConfigEnv(t)
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.Header.Get("Authorization") != "Bearer fixture-admin" {
						t.Error("admin command did not select admin credential")
					}
					if r.URL.Path != "/v1/admin/leases/cbx_abcdef123456/"+command {
						t.Errorf("path=%s", r.URL.Path)
					}
					if command == "extend-lifetime" {
						if r.Method != http.MethodPost {
							t.Error(r.Method)
						}
						var body map[string]string
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						if len(body) != 3 || body["owner"] != "alice@example.com" || body["reason"] != "test completion" || body["expiresAt"] != "2026-10-11T11:00:00.123Z" {
							t.Errorf("body=%v", body)
						}
						w.Write([]byte(`{"lease":{"id":"cbx_abcdef123456","lifetimePolicy":{"mode":"enforce","source":"admin-canary","deadlineAt":"2026-10-11T10:00:00Z","effectiveStopAt":"2026-10-11T11:00:00Z","retainSeconds":300}}}`))
					} else {
						if r.Method != http.MethodGet {
							t.Error(r.Method)
						}
						w.Write([]byte(`{"leaseId":"cbx_abcdef123456","provider":"aws","region":"eu-west-1","instanceId":"i-fixture","instanceState":"terminated","rootVolumeId":"vol-fixture","rootVolumePresent":false,"observedAt":"2026-10-11T12:00:00Z"}`))
					}
				}))
				defer server.Close()
				t.Setenv("CRABBOX_COORDINATOR", server.URL)
				t.Setenv("CRABBOX_COORDINATOR_TOKEN", "fixture-user")
				t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "fixture-admin")
				args := []string{"admin", command, "--id", "cbx_abcdef123456"}
				if command == "extend-lifetime" {
					args = append(args, "--owner", "alice@example.com", "--reason", "test completion", "--expires-at", "2026-10-11T13:00:00.123+02:00")
				}
				if jsonOutput {
					args = append(args, "--json")
				}
				var stdout, stderr bytes.Buffer
				if err := (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args); err != nil {
					t.Fatalf("err=%v; stderr=%s", err, stderr.String())
				}
				if calls != 1 {
					t.Fatalf("calls=%d", calls)
				}
				if command == "extend-lifetime" {
					if !strings.Contains(stdout.String(), "2026-10-11T11:00:00Z") {
						t.Fatal(stdout.String())
					}
				} else {
					if jsonOutput {
						var readback CoordinatorLifetimeReadback
						if err := json.Unmarshal(stdout.Bytes(), &readback); err != nil {
							t.Fatal(err)
						}
						if readback.RootVolumePresent == nil || *readback.RootVolumePresent {
							t.Fatalf("false presence lost: %s", stdout.String())
						}
					} else if !strings.Contains(stdout.String(), "root_volume_present=false") {
						t.Fatal(stdout.String())
					}
				}
			})
		}
	}
}

func TestAdminLifetimeCommandsRejectInvalidArgumentsBeforeAuth(t *testing.T) {
	for _, args := range [][]string{
		{"lifetime-readback"},
		{"lifetime-readback", "--id", "friendly-slug"},
		{"lifetime-readback", "--id", "cbx_abcdef123456", "extra"},
		{"extend-lifetime", "--id", "cbx_abcdef123456"},
		{"extend-lifetime", "--id", "cbx_abcdef123456", "--owner", "alice@example.com", "--reason", "test", "--expires-at", "tomorrow"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			clearConfigEnv(t)
			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), append([]string{"admin"}, args...))
			if err == nil || strings.Contains(err.Error(), "configured coordinator") {
				t.Fatalf("unexpected err=%v", err)
			}
		})
	}
}

func TestAdminLifetimeCommandsDoNotRetryDeniedExplicitAdmin(t *testing.T) {
	clearConfigEnv(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer rejected-admin" {
			t.Error("fell back from explicit administrator")
		}
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "fixture-user")
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "rejected-admin")
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"admin", "lifetime-readback", "--id", "cbx_abcdef123456", "--json"})
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
