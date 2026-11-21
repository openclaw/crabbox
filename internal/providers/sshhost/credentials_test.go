package sshhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/serve"
	"golang.org/x/crypto/ssh"
)

func preparedHostFixture(t *testing.T, coordinator string) hostState {
	t.Helper()
	private, _, err := generateHostKey()
	if err != nil {
		t.Fatal(err)
	}
	state := hostState{ID: "11111111-1111-4111-8111-111111111111", Coordinator: coordinator, FrontDoorKey: private, AuthMode: "host"}
	public, err := publicFrontDoorKey(state)
	if err != nil {
		t.Fatal(err)
	}
	state.Descriptor = &hostDescriptor{Version: 1, Coordinator: coordinator, ID: state.ID, Name: "test-builder", Target: "linux", Architecture: "amd64", Runtime: "native-user", Template: "test-v1", Capabilities: []string{"build"}, HostKey: public}
	return state
}

func credentialFixture(state hostState) hostCredentialEnvelope {
	c := hostCredentialEnvelope{Purpose: "agent", Version: 1, Descriptor: *state.Descriptor, Token: "cbxh_" + state.ID + "_1_" + strings.Repeat("ab", 32), Generation: 1, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339)}
	c.Identity = &hostCredentialIdentity{Purpose: "agent", ID: state.ID, Owner: "operator@example.com", Org: "example-org", HostKey: state.Descriptor.HostKey, Generation: c.Generation, ExpiresAt: c.ExpiresAt}
	return c
}

func TestHostEnrollmentPersistsSecretBeforePostAndRetriesSameCredential(t *testing.T) {
	var output bytes.Buffer
	dir := t.TempDir()
	path := filepath.Join(dir, "credential.json")
	calls := 0
	var firstHash string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer synthetic-admin" || r.URL.Path != "/v1/ssh-hosts" {
			t.Error("unexpected enrollment request")
		}
		saved, err := readPrivateHostFile(path)
		if err != nil {
			t.Error("credential not durable before POST", err)
			w.WriteHeader(500)
			return
		}
		var credential hostCredentialEnvelope
		if json.Unmarshal(saved, &credential) != nil {
			t.Error("invalid private credential")
		}
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(credential.Token)) {
			t.Error("secret sent to coordinator")
		}
		var input struct {
			Credential struct {
				TokenHash string `json:"tokenHash"`
			} `json:"credential"`
		}
		if json.Unmarshal(body, &input) != nil {
			t.Error("invalid enrollment")
		}
		hash := sha256.Sum256([]byte(credential.Token))
		expected := hex.EncodeToString(hash[:])
		if input.Credential.TokenHash != expected {
			t.Error("wrong credential digest")
		}
		if calls == 1 {
			firstHash = expected
			w.WriteHeader(503)
			return
		}
		if firstHash != expected {
			t.Error("retry rotated credential")
		}
		identity := credentialFixture(hostState{ID: credential.Descriptor.ID, Descriptor: &credential.Descriptor}).Identity
		identity.ExpiresAt = credential.ExpiresAt
		_ = json.NewEncoder(w).Encode(map[string]any{"identity": identity})
	}))
	defer server.Close()
	state := preparedHostFixture(t, server.URL)
	descriptor := filepath.Join(dir, "descriptor.json")
	data, _ := json.Marshal(state.Descriptor)
	if err := os.WriteFile(descriptor, data, 0600); err != nil {
		t.Fatal(err)
	}
	o := hostCredentialOptions{action: "enroll", descriptor: descriptor, output: path, ttl: time.Hour}
	cfg := core.Config{Coordinator: server.URL, CoordToken: "synthetic-user", CoordAdminToken: "synthetic-admin", CoordTokenCommand: []string{"must-not-run"}}
	rt := core.Runtime{Stdout: &output, Stderr: &output}
	if err := runCredentialCommand(t.Context(), cfg, rt, o, nil); err == nil {
		t.Fatal("expected first enrollment failure")
	}
	if err := runCredentialCommand(t.Context(), cfg, rt, o, nil); err != nil {
		t.Fatal(err)
	}
	saved, err := readPrivateHostFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var credential hostCredentialEnvelope
	if json.Unmarshal(saved, &credential) != nil || credential.Identity == nil {
		t.Fatal("receipt not saved")
	}
	if strings.Contains(output.String(), credential.Token) || strings.Contains(output.String(), firstHash) {
		t.Fatal("credential disclosed in output")
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
	if err := createPrivateHostJSON(path, map[string]bool{"replaced": true}); err == nil {
		t.Fatal("overwrote existing credential")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(saved, after) {
		t.Fatal("existing credential changed")
	}
}

func TestHostManagementSelectsAdminAuthWithoutChangingList(t *testing.T) {
	const hostID = "11111111-1111-4111-8111-111111111111"
	tests := []struct {
		name, action, method, path, token string
		adminToken, denied                bool
	}{
		{"list stays on user login", "list", http.MethodGet, "/v1/ssh-hosts", "user-token", true, false},
		{"legacy enroll", "enroll", http.MethodPost, "/v1/ssh-hosts", "admin-token", true, false},
		{"enable", "enable", http.MethodPatch, "/v1/ssh-hosts/" + hostID, "admin-token", true, false},
		{"disable", "disable", http.MethodPatch, "/v1/ssh-hosts/" + hostID, "admin-token", true, false},
		{"drain", "drain", http.MethodPatch, "/v1/ssh-hosts/" + hostID, "admin-token", true, false},
		{"resume", "resume", http.MethodPatch, "/v1/ssh-hosts/" + hostID, "admin-token", true, false},
		{"remove", "remove", http.MethodDelete, "/v1/ssh-hosts/" + hostID, "admin-token", true, false},
		{"revoke", "revoke", http.MethodPost, "/v1/ssh-hosts/" + hostID + "/credential/revoke", "admin-token", true, false},
		{"normal admin login fallback", "enable", http.MethodPatch, "/v1/ssh-hosts/" + hostID, "user-token", false, false},
		{"denied admin token does not fall back", "enable", http.MethodPatch, "/v1/ssh-hosts/" + hostID, "admin-token", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != tt.method || r.URL.Path != tt.path || r.Header.Get("Authorization") != "Bearer "+tt.token {
					t.Errorf("unexpected host request: %s %s, auth matched=%t", r.Method, r.URL.Path, r.Header.Get("Authorization") == "Bearer "+tt.token)
				}
				if tt.denied {
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, `{"error":"forbidden"}`)
					return
				}
				_, _ = io.WriteString(w, `{"hosts":[]}`)
			}))
			defer server.Close()
			cfg := core.Config{Coordinator: server.URL, CoordToken: "user-token", CoordTokenCommand: []string{"must-not-run"}}
			if tt.adminToken {
				cfg.CoordAdminToken = "admin-token"
			}
			if !tt.adminToken || tt.action == "list" {
				cfg.CoordTokenCommand = nil
			}
			args := []string{tt.action}
			switch tt.action {
			case "enroll":
				dir := t.TempDir()
				if err := os.Chmod(dir, 0700); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--state-dir", dir, "--name", "builder", "--template", "tools-v1")
			case "list":
				// Listing remains available to ordinary users.
			default:
				args = append(args, "--id", hostID)
			}
			err := (Provider{}).RunAgent(t.Context(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}, args)
			if tt.denied {
				var httpErr core.CoordinatorHTTPError
				if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusForbidden {
					t.Fatalf("err=%v, want coordinator denial", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("calls=%d, want one request", calls)
			}
		})
	}
}

func TestHostInstallUsesOnlyBoundCredentialAndVerifiesServerIdentity(t *testing.T) {
	var credential hostCredentialEnvelope
	var badIdentity bool
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer "+credential.Token {
			t.Error("did not use host credential")
		}
		if r.URL.Path != "/v1/ssh-hosts/"+credential.Descriptor.ID+"/heartbeat" {
			t.Error("unexpected route")
		}
		identity := *credential.Identity
		if badIdentity {
			identity.Owner = "other@example.com"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"identity": identity, "assignment": nil})
	}))
	defer server.Close()
	state := preparedHostFixture(t, server.URL)
	credential = credentialFixture(state)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	if err := writePrivateJSON(path, state); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	rt := core.Runtime{Stdout: &output, Stderr: &output}
	o := hostCredentialOptions{action: "install", dir: dir, stdin: true}
	cfg := core.Config{Coordinator: "https://wrong.example.com", CoordToken: "synthetic-admin", CoordAdminToken: "unrelated-admin", CoordTokenCommand: []string{"must-not-run"}}
	data, _ := json.Marshal(credential)
	badIdentity = true
	if err := runCredentialCommand(t.Context(), cfg, rt, o, bytes.NewReader(data)); err == nil {
		t.Fatal("accepted mismatched server identity")
	}
	saved, _ := readPrivateHostFile(path)
	var prior hostState
	_ = json.Unmarshal(saved, &prior)
	if prior.Credential != nil {
		t.Fatal("persisted unverified credential")
	}
	badIdentity = false
	if err := runCredentialCommand(t.Context(), cfg, rt, o, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	saved, _ = readPrivateHostFile(path)
	var installed hostState
	_ = json.Unmarshal(saved, &installed)
	if installed.Credential == nil || installed.Credential.Token != credential.Token {
		t.Fatal("credential not installed")
	}
	if strings.Contains(output.String(), credential.Token) {
		t.Fatal("credential disclosed")
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestHostInstallRejectsCustodyAndWrongDescriptorWithoutNetwork(t *testing.T) {
	state := preparedHostFixture(t, "https://broker.example.com")
	credential := credentialFixture(state)
	tests := []struct {
		name   string
		mutate func(*hostState, *hostCredentialEnvelope)
	}{
		{"assignment", func(s *hostState, _ *hostCredentialEnvelope) {
			s.Assignment = &hostAssignment{LeaseID: "cbx_000000000001"}
		}},
		{"runtime", func(s *hostState, _ *hostCredentialEnvelope) { s.Runtime = &runtimeState{User: "retained"} }},
		{"wrong host", func(_ *hostState, c *hostCredentialEnvelope) {
			c.Descriptor.ID = "22222222-2222-4222-8222-222222222222"
		}},
		{"wrong key", func(_ *hostState, c *hostCredentialEnvelope) {
			c.Descriptor.HostKey = preparedHostFixture(t, "https://broker.example.com").Descriptor.HostKey
		}},
		{"wrong coordinator", func(_ *hostState, c *hostCredentialEnvelope) { c.Descriptor.Coordinator = "https://other.example.com" }},
		{"missing receipt", func(_ *hostState, c *hostCredentialEnvelope) { c.Identity = nil }},
		{"expired", func(_ *hostState, c *hostCredentialEnvelope) {
			c.ExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, c := state, credential
			tc.mutate(&s, &c)
			dir := t.TempDir()
			_ = os.Chmod(dir, 0700)
			path := filepath.Join(dir, "state.json")
			if err := writePrivateJSON(path, s); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			data, _ := json.Marshal(c)
			err := runCredentialCommand(t.Context(), core.Config{}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}, hostCredentialOptions{action: "install", dir: dir, stdin: true}, bytes.NewReader(data))
			if err == nil {
				t.Fatal("accepted invalid installation")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("changed retained host state")
			}
		})
	}
}

func TestScopedHostClientNeverFallsBackAndAllowsExpiredCleanup(t *testing.T) {
	state := preparedHostFixture(t, "https://broker.example.com")
	cfg := core.Config{Coordinator: "https://other.example.com", CoordToken: "synthetic-admin", CoordAdminToken: "unrelated-admin", CoordTokenCommand: []string{"must-not-run"}}
	if _, err := hostCoordinator(cfg, state); err == nil {
		t.Fatal("missing scoped credential fell back")
	}
	credential := credentialFixture(state)
	state.Credential = &credential
	for _, mode := range []string{"", "unknown"} {
		invalid := state
		invalid.AuthMode = mode
		if _, err := hostCoordinator(cfg, invalid); err == nil {
			t.Fatal("invalid scoped mode used operator credentials")
		}
	}
	c, err := hostCoordinator(cfg, state)
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != state.Coordinator || c.Token != credential.Token || len(c.TokenCommand) != 0 {
		t.Fatal("inherited operator credentials")
	}
	state.Credential.ExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if _, err = hostCoordinator(cfg, state); err != nil {
		t.Fatal("expired credential blocked local cleanup before serving", err)
	}
	state.Credential.Descriptor.HostKey = "invalid"
	if _, err = hostCoordinator(cfg, state); err == nil {
		t.Fatal("accepted key mismatch")
	}
}

func TestCleanupCredentialInstallPreservesExactRetainedCustody(t *testing.T) {
	var credential hostCredentialEnvelope
	var assignment *hostAssignment
	var completed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+credential.Token {
			t.Error("wrong cleanup credential")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"identity": credential.Identity, "assignment": assignment, "cleanupComplete": completed})
	}))
	defer server.Close()
	state := preparedHostFixture(t, server.URL)
	state.Assignment = &hostAssignment{HostID: state.ID, LeaseID: "cbx_000000000001", Generation: 4, AccessGeneration: 2, Operation: "prepare", Deadline: time.Now().Add(time.Hour).UnixMilli()}
	state.Runtime = &runtimeState{User: "retained-user", Marker: "retained-marker"}
	state.Report = &hostReport{LeaseID: state.Assignment.LeaseID, Generation: 4, AccessGeneration: 2, State: "failed"}
	credential = credentialFixture(state)
	credential.Purpose = "cleanup"
	credential.Cleanup = &hostCleanupBinding{LeaseID: state.Assignment.LeaseID, Generation: 4, AccessGeneration: 2}
	credential.Identity.Purpose = "cleanup"
	credential.Identity.Cleanup = credential.Cleanup
	assignment = &hostAssignment{HostID: state.ID, LeaseID: state.Assignment.LeaseID, Generation: 4, AccessGeneration: 2, Operation: "clean", Deadline: state.Assignment.Deadline}
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	path := filepath.Join(dir, "state.json")
	if err := writePrivateJSON(path, state); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(credential)
	o := hostCredentialOptions{action: "install", dir: dir, stdin: true}
	rt := core.Runtime{Stdout: io.Discard, Stderr: io.Discard}
	if err := runCredentialCommand(t.Context(), core.Config{}, rt, o, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	saved, _ := readPrivateHostFile(path)
	var installed hostState
	_ = json.Unmarshal(saved, &installed)
	before, _ := json.Marshal([]any{state.Assignment, state.Runtime, state.Report})
	after, _ := json.Marshal([]any{installed.Assignment, installed.Runtime, installed.Report})
	if !bytes.Equal(before, after) {
		t.Fatal("cleanup install changed retained custody")
	}
	assignment = nil
	completed = true
	credential.Generation = 2
	credential.Token = strings.Replace(credential.Token, "_1_", "_2_", 1)
	credential.Identity.Generation = 2
	data, _ = json.Marshal(credential)
	if err := runCredentialCommand(t.Context(), core.Config{}, rt, o, bytes.NewReader(data)); err != nil {
		t.Fatal("completed cleanup acknowledgement installation", err)
	}
	assignment = &hostAssignment{HostID: state.ID, LeaseID: state.Assignment.LeaseID, Generation: 4, AccessGeneration: 2, Operation: "clean", Deadline: state.Assignment.Deadline}
	completed = false
	credential.Cleanup.AccessGeneration++
	data, _ = json.Marshal(credential)
	if err := runCredentialCommand(t.Context(), core.Config{}, rt, o, bytes.NewReader(data)); err == nil {
		t.Fatal("accepted wrong retained generation")
	}
	credential.Cleanup.AccessGeneration--
	assignment.Operation = "prepare"
	data, _ = json.Marshal(credential)
	if err := runCredentialCommand(t.Context(), core.Config{}, rt, o, bytes.NewReader(data)); err == nil {
		t.Fatal("accepted non-clean recovery assignment")
	}
}

func TestCleanupAgentRejectsWorkloadAssignmentAndRelays(t *testing.T) {
	var response hostHeartbeat
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(response) }))
	defer server.Close()
	state := preparedHostFixture(t, server.URL)
	credential := credentialFixture(state)
	credential.Purpose = "cleanup"
	credential.Cleanup = &hostCleanupBinding{LeaseID: "cbx_000000000001", Generation: 2, AccessGeneration: 3}
	state.Credential = &credential
	state.Assignment = &hostAssignment{HostID: state.ID, LeaseID: credential.Cleanup.LeaseID, Generation: 2, AccessGeneration: 3, Operation: "clean", Deadline: time.Now().Add(time.Hour).UnixMilli()}
	coord, err := hostCoordinator(core.Config{}, state)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(state.FrontDoorKey))
	if err != nil {
		t.Fatal(err)
	}
	front := serve.NewHostFrontDoor(signer)
	defer front.Close()
	a := &hostAgent{coord: coord, state: state, front: front, path: filepath.Join(t.TempDir(), "state.json")}
	for _, operation := range []string{"prepare", "idle"} {
		next := *state.Assignment
		next.Operation = operation
		response = hostHeartbeat{Assignment: &next}
		if err := a.tick(t.Context()); err == nil {
			t.Fatal("cleanup agent accepted", operation)
		}
		if a.state.Runtime != nil || a.state.Assignment.Operation != "clean" {
			t.Fatal("changed cleanup custody")
		}
	}
	response = hostHeartbeat{Assignment: state.Assignment, Relays: []relayAssignment{{SessionID: "forbidden"}}}
	if err := a.tick(t.Context()); err == nil {
		t.Fatal("cleanup agent accepted relay")
	}
}

func TestCleanupAcknowledgementSurvivesDroppedResponseAndFailedSave(t *testing.T) {
	var accepted hostCredentialEnvelope
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer "+accepted.Token {
			t.Error("wrong credential")
		}
		if calls == 1 {
			// The coordinator settled cleanup, but the HTTP response was lost.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"identity": accepted.Identity, "assignment": nil, "cleanupComplete": true, "relays": []relayAssignment{}})
	}))
	defer server.Close()
	state := preparedHostFixture(t, server.URL)
	accepted = credentialFixture(state)
	accepted.Purpose = "cleanup"
	accepted.Cleanup = &hostCleanupBinding{LeaseID: "cbx_000000000001", Generation: 1, AccessGeneration: 0}
	accepted.Identity.Purpose = "cleanup"
	accepted.Identity.Cleanup = accepted.Cleanup
	state.Credential = &accepted
	state.Assignment = &hostAssignment{HostID: state.ID, LeaseID: accepted.Cleanup.LeaseID, Generation: 1, AccessGeneration: 0, Operation: "clean", Deadline: time.Now().Add(time.Hour).UnixMilli()}
	state.Report = &hostReport{LeaseID: state.Assignment.LeaseID, Generation: 1, AccessGeneration: 0, State: "clean"}
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	path := filepath.Join(dir, "state.json")
	if err := writePrivateJSON(path, state); err != nil {
		t.Fatal(err)
	}
	coord, err := hostCoordinator(core.Config{}, state)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(state.FrontDoorKey))
	if err != nil {
		t.Fatal(err)
	}
	front := serve.NewHostFrontDoor(signer)
	defer front.Close()
	a := &hostAgent{coord: coord, state: state, front: front, path: path}
	if err := a.tick(t.Context()); err == nil {
		t.Fatal("expected dropped response")
	}
	if a.state.Assignment == nil {
		t.Fatal("lost custody after failed request")
	}
	a.path = filepath.Join(dir, "missing", "state.json")
	if err := a.tick(t.Context()); err == nil {
		t.Fatal("expected failed local save")
	}
	data, err := readPrivateHostFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var restarted hostState
	if err = json.Unmarshal(data, &restarted); err != nil {
		t.Fatal(err)
	}
	if restarted.Assignment == nil {
		t.Fatal("test lost retained restart state")
	}
	a = &hostAgent{coord: coord, state: restarted, front: front, path: path}
	if err := a.tick(t.Context()); err != nil {
		t.Fatal("completion acknowledgement retry failed", err)
	}
	data, _ = readPrivateHostFile(path)
	var cleaned hostState
	_ = json.Unmarshal(data, &cleaned)
	if cleaned.Assignment != nil || cleaned.Runtime != nil || cleaned.Report != nil {
		t.Fatal("local custody did not settle")
	}
	accepted = credentialFixture(state)
	accepted.Generation = 2
	accepted.Token = strings.Replace(accepted.Token, "_1_", "_2_", 1)
	accepted.Identity.Generation = 2
	data, _ = json.Marshal(accepted)
	if err := runCredentialCommand(t.Context(), core.Config{}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}, hostCredentialOptions{action: "install", dir: dir, stdin: true}, bytes.NewReader(data)); err != nil {
		t.Fatal("normal installation after cleanup acknowledgement", err)
	}
}

func TestHostCredentialInputBoundedAndPrivate(t *testing.T) {
	var out hostCredentialEnvelope
	if err := decodeHostJSON(strings.NewReader(strings.Repeat(" ", 65537)), &out); err == nil {
		t.Fatal("accepted oversized input")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "credential.json")
	if err := createPrivateHostJSON(path, map[string]string{"token": "synthetic"}); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateHostFile(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(dir, "symlink.json")); err == nil {
		if _, err = readPrivateHostFile(filepath.Join(dir, "symlink.json")); err == nil {
			t.Fatal("followed credential symlink")
		}
	}
}

func TestPrepareRetriesReturnOnlyPublicDescriptor(t *testing.T) {
	state := preparedHostFixture(t, "https://broker.example.com")
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	if err := writePrivateJSON(filepath.Join(dir, "state.json"), state); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runCredentialCommand(context.Background(), core.Config{}, core.Runtime{Stdout: &output, Stderr: io.Discard}, hostCredentialOptions{action: "prepare", dir: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "PRIVATE KEY") || strings.Contains(output.String(), "frontDoorKey") || strings.Contains(output.String(), "token") {
		t.Fatal("prepare exposed private state")
	}
	var descriptor hostDescriptor
	if json.Unmarshal(output.Bytes(), &descriptor) != nil || descriptor.ID != state.ID {
		t.Fatal("invalid descriptor")
	}
}
