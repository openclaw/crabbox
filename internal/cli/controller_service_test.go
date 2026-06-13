package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeControllerWorkspaceRunner struct {
	mu                           sync.Mutex
	ready                        map[string]StatusView
	warmupCalls                  int
	stopCalls                    int
	confirmAbsentCalls           int
	cleanupAbsentCalls           int
	cleanupAbsentRequests        []controllerWorkspaceRequest
	stopIdentifiers              []string
	localStopCalls               int
	connectionCalls              int
	inspectCalls                 int
	inspectRoutes                []string
	inspectScopes                []string
	providerRoute                string
	providerScope                string
	idempotentFixedLeaseID       bool
	coordinatorRegistrationURL   string
	warmupErr                    error
	warmupPreAcquireFailures     int
	materializePreAcquireFailure bool
	stopErr                      error
	confirmAbsentErr             error
	confirmAbsentResults         []bool
	cleanupAbsentErr             error
	localStopErr                 error
	inspectErr                   error
	inspectFailures              int
	warmupLeaseID                string
	started                      chan string
	blockWarmup                  chan struct{}
	afterAcquireAck              func(controllerAcquireIdentity) error
	desktopURL                   string
	connectionErr                error
	connectionStarted            chan struct{}
	blockConnection              chan struct{}
	ignoreConnectionCancellation bool
}

type shutdownAwareControllerRunner struct {
	*fakeControllerWorkspaceRunner
	stopStarted  chan struct{}
	stopCanceled chan error
}

type blockingProviderIdentityRunner struct {
	*fakeControllerWorkspaceRunner
	started chan struct{}
}

type startupRecoveryControllerRunner struct {
	*fakeControllerWorkspaceRunner
	events       *[]string
	recoverCheck func(string) error
}

func (r *startupRecoveryControllerRunner) ProviderIdentity(ctx context.Context) (controllerProviderIdentity, error) {
	*r.events = append(*r.events, "provider")
	return r.fakeControllerWorkspaceRunner.ProviderIdentity(ctx)
}

func (r *startupRecoveryControllerRunner) RecoverControllerChildren(_ context.Context, stateFile string) error {
	*r.events = append(*r.events, "recover")
	if r.recoverCheck != nil {
		return r.recoverCheck(stateFile)
	}
	return nil
}

func (r *blockingProviderIdentityRunner) ProviderIdentity(ctx context.Context) (controllerProviderIdentity, error) {
	close(r.started)
	<-ctx.Done()
	return controllerProviderIdentity{}, context.Cause(ctx)
}

func (r *shutdownAwareControllerRunner) Stop(ctx context.Context, _ string, _ controllerWorkspaceRequest) error {
	r.stopStarted <- struct{}{}
	<-ctx.Done()
	err := context.Cause(ctx)
	r.stopCanceled <- err
	return err
}

func newFakeControllerWorkspaceRunner() *fakeControllerWorkspaceRunner {
	return &fakeControllerWorkspaceRunner{
		ready:                  map[string]StatusView{},
		desktopURL:             "https://portal.example.test/desktop#password=secret",
		providerRoute:          "external",
		providerScope:          "test-provider-scope",
		idempotentFixedLeaseID: true,
	}
}

func (r *fakeControllerWorkspaceRunner) ProviderIdentity(context.Context) (controllerProviderIdentity, error) {
	return controllerProviderIdentity{
		Route: r.providerRoute, Scope: r.providerScope, IdempotentFixedLeaseID: r.idempotentFixedLeaseID,
		CoordinatorRegistrationURL: r.coordinatorRegistrationURL,
	}, nil
}

func (r *fakeControllerWorkspaceRunner) Warmup(
	ctx context.Context,
	attemptLeaseID, slug string,
	request controllerWorkspaceRequest,
	onStarted func() error,
	onAcquired func(controllerAcquireIdentity) error,
) (controllerAcquireIdentity, error) {
	r.mu.Lock()
	r.warmupCalls++
	started := r.started
	block := r.blockWarmup
	warmupErr := r.warmupErr
	preAcquireFailure := r.warmupPreAcquireFailures > 0
	if preAcquireFailure {
		r.warmupPreAcquireFailures--
	}
	materializePreAcquireFailure := r.materializePreAcquireFailure
	warmupLeaseID := r.warmupLeaseID
	afterAcquireAck := r.afterAcquireAck
	r.mu.Unlock()
	if onStarted != nil {
		if err := onStarted(); err != nil {
			return controllerAcquireIdentity{}, err
		}
	}
	if started != nil {
		started <- request.ID
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return controllerAcquireIdentity{}, context.Cause(ctx)
		}
	}
	if preAcquireFailure {
		if materializePreAcquireFailure {
			r.mu.Lock()
			status := fakeControllerStatus(slug)
			status.ID = attemptLeaseID
			r.ready[request.ID] = status
			r.mu.Unlock()
		}
		if warmupErr == nil {
			warmupErr = errors.New("provider acquire failed before identity acknowledgment")
		}
		return controllerAcquireIdentity{}, warmupErr
	}
	leaseID := firstNonBlank(warmupLeaseID, attemptLeaseID)
	status := fakeControllerStatus(slug)
	status.ID = leaseID
	identity := controllerAcquireIdentity{
		LeaseID: leaseID, Slug: status.Slug, Provider: status.Provider, ResourceID: status.ServerID,
	}
	r.mu.Lock()
	r.ready[request.ID] = status
	r.mu.Unlock()
	if onAcquired != nil {
		if err := onAcquired(identity); err != nil {
			r.mu.Lock()
			delete(r.ready, request.ID)
			r.mu.Unlock()
			return identity, err
		}
	}
	if afterAcquireAck != nil {
		if err := afterAcquireAck(identity); err != nil {
			return identity, err
		}
	}
	return identity, warmupErr
}

func (r *fakeControllerWorkspaceRunner) Inspect(_ context.Context, _ string, request controllerWorkspaceRequest) (StatusView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inspectCalls++
	r.inspectRoutes = append(r.inspectRoutes, request.ProviderRoute)
	r.inspectScopes = append(r.inspectScopes, request.ProviderScope)
	if r.inspectFailures > 0 {
		r.inspectFailures--
		return StatusView{}, r.inspectErr
	}
	status, ok := r.ready[request.ID]
	if !ok {
		return StatusView{}, &controllerWorkspaceNotFoundError{Identifier: request.ID}
	}
	return status, nil
}

func (r *fakeControllerWorkspaceRunner) Stop(_ context.Context, identifier string, request controllerWorkspaceRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopCalls++
	r.stopIdentifiers = append(r.stopIdentifiers, identifier)
	if r.stopErr != nil {
		return r.stopErr
	}
	delete(r.ready, request.ID)
	return nil
}

func (r *fakeControllerWorkspaceRunner) ConfirmAbsent(_ context.Context, _ string, request controllerWorkspaceRequest) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.confirmAbsentCalls++
	if r.confirmAbsentErr != nil {
		return false, r.confirmAbsentErr
	}
	if len(r.confirmAbsentResults) > 0 {
		absent := r.confirmAbsentResults[0]
		r.confirmAbsentResults = r.confirmAbsentResults[1:]
		return absent, nil
	}
	_, exists := r.ready[request.ID]
	return !exists, nil
}

func (r *fakeControllerWorkspaceRunner) CleanupAbsent(_ context.Context, _ string, request controllerWorkspaceRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanupAbsentCalls++
	r.cleanupAbsentRequests = append(r.cleanupAbsentRequests, request)
	return r.cleanupAbsentErr
}

func (r *fakeControllerWorkspaceRunner) StopLocal(_ context.Context, _ string, _ controllerWorkspaceRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.localStopCalls++
	return r.localStopErr
}

func (r *fakeControllerWorkspaceRunner) DesktopConnection(ctx context.Context, _ string, _ controllerWorkspaceRequest) (string, error) {
	r.mu.Lock()
	r.connectionCalls++
	started := r.connectionStarted
	block := r.blockConnection
	ignoreCancellation := r.ignoreConnectionCancellation
	url := r.desktopURL
	err := r.connectionErr
	r.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if block != nil {
		if ignoreCancellation {
			<-block
		} else {
			select {
			case <-block:
			case <-ctx.Done():
				return "", context.Cause(ctx)
			}
		}
	}
	return url, err
}

func (r *fakeControllerWorkspaceRunner) counts() (int, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.warmupCalls, r.stopCalls, r.connectionCalls
}

func fakeControllerStatus(id string) StatusView {
	return StatusView{
		ID:         "cbx_abcdef123456",
		Slug:       id,
		Provider:   "external",
		TargetOS:   "linux",
		State:      "ready",
		ServerID:   "resource-123",
		ServerType: "standard",
		Host:       "192.0.2.10",
		ExpiresAt:  time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano),
		HasHost:    true,
		Ready:      true,
	}
}

func testControllerService(t *testing.T, runner controllerWorkspaceRunner, maxConcurrent int) (*controllerService, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	service, err := newControllerService(ctx, controllerServiceOptions{
		StateFile:              filepath.Join(t.TempDir(), "state", "controller.json"),
		MaxConcurrent:          maxConcurrent,
		Allowed:                controllerCapabilities{Desktop: true, Browser: true, Code: true},
		Profile:                "public-desktop",
		AttachURLTemplate:      "wss://terminal.example.test/workspaces/{workspaceId}",
		VNCURLTemplate:         "https://fleet.example.test/workspaces/{workspaceId}/desktop",
		CreateTimeout:          5 * time.Second,
		InspectTimeout:         time.Second,
		StopTimeout:            time.Second,
		ConnectionTimeout:      time.Second,
		RetryDelay:             10 * time.Millisecond,
		ReadyReconcileInterval: time.Hour,
	}, runner, "test-token", &bytes.Buffer{})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return service, func() {
		cancel()
		service.waitForShutdown()
	}
}

func TestControllerRejectsProviderWithoutExplicitFixedLeaseIDContract(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.idempotentFixedLeaseID = false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := newControllerService(ctx, controllerServiceOptions{
		StateFile: filepath.Join(t.TempDir(), "state.json"), MaxConcurrent: 1,
		CreateTimeout: time.Second, InspectTimeout: time.Second, StopTimeout: time.Second,
		ConnectionTimeout: time.Second,
	}, runner, "token", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "does not explicitly guarantee idempotent fixed lease IDs") {
		t.Fatalf("startup error=%v", err)
	}
}

func TestControllerWorkspaceLifecycleAndIdempotency(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.coordinatorRegistrationURL = "https://coordinator.example.test/root"
	service, cancel := testControllerService(t, runner, 2)
	defer cancel()

	health := controllerHTTP(service, http.MethodGet, "/healthz", "", nil)
	if health.Code != http.StatusOK {
		t.Fatalf("health status=%d body=%s", health.Code, health.Body.String())
	}
	unauthorized := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "", map[string]any{"id": "demo-box"})
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}

	request := controllerWorkspaceRequest{
		ID:      "demo-box",
		Repo:    "example/app",
		Branch:  "main",
		Runtime: "linux",
		Prompt:  "private request metadata",
		Capabilities: controllerCapabilities{
			Desktop: true,
			Browser: true,
		},
	}
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", request)
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	record := waitControllerWorkspaceStatus(t, service, "demo-box", "ready")
	if record.Request.Profile != "public-desktop" {
		t.Fatalf("profile=%q", record.Request.Profile)
	}
	if record.ProviderRoute != "external" || record.ProviderScope != "test-provider-scope" {
		t.Fatalf("provider identity was not persisted: route=%q scope=%q", record.ProviderRoute, record.ProviderScope)
	}
	if record.CoordinatorRegistrationURL != runner.coordinatorRegistrationURL {
		t.Fatalf("coordinator registration binding=%q", record.CoordinatorRegistrationURL)
	}
	if record.AttachURL != "wss://terminal.example.test/workspaces/demo-box" {
		t.Fatalf("attach URL=%q", record.AttachURL)
	}

	got := controllerHTTP(service, http.MethodGet, "/v1/workspaces/demo-box", "test-token", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", got.Code, got.Body.String())
	}
	var response controllerWorkspaceResponse
	if err := json.Unmarshal(got.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != "demo-box" || response.LeaseID != record.AttemptLeaseID || response.ProviderResourceID != "resource-123" || response.Status != "ready" {
		t.Fatalf("response=%#v", response)
	}
	if !response.Capabilities.Terminal || !response.Capabilities.VNC || !response.Capabilities.Desktop || response.Capabilities.Takeover || response.Capabilities.Logs || response.Capabilities.Artifacts {
		t.Fatalf("response capabilities=%#v", response.Capabilities)
	}

	idempotent := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", request)
	if idempotent.Code != http.StatusOK {
		t.Fatalf("idempotent status=%d body=%s", idempotent.Code, idempotent.Body.String())
	}
	request.Runtime = "other"
	conflict := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", request)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), `"code":"workspace_id_conflict"`) {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	warmups, _, _ := runner.counts()
	if warmups != 1 {
		t.Fatalf("warmup calls=%d", warmups)
	}

	connection := controllerHTTP(service, http.MethodPost, "/v1/workspaces/demo-box/connections/desktop", "test-token", nil)
	if connection.Code != http.StatusOK {
		t.Fatalf("connection status=%d body=%s", connection.Code, connection.Body.String())
	}
	var connectionBody struct {
		URL       string `json:"url"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := json.Unmarshal(connection.Body.Bytes(), &connectionBody); err != nil {
		t.Fatal(err)
	}
	wantDesktopURL := "https://fleet.example.test/workspaces/demo-box/desktop"
	if connectionBody.URL != wantDesktopURL || connectionBody.ExpiresAt != "" {
		t.Fatalf("connection=%#v", connectionBody)
	}
	stateData, err := os.ReadFile(service.opts.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stateData, []byte("password=secret")) {
		t.Fatal("credential-bearing desktop URL persisted in controller state")
	}
	if bytes.Contains(stateData, []byte("/desktop")) || bytes.Contains(got.Body.Bytes(), []byte("vncUrl")) {
		t.Fatal("desktop URL persisted or included in normal workspace response")
	}
	if info, err := os.Stat(service.opts.StateFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode info=%v err=%v", info, err)
	}

	deleted := controllerHTTP(service, http.MethodDelete, "/v1/workspaces/demo-box", "test-token", nil)
	if deleted.Code != http.StatusAccepted {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	waitControllerWorkspaceStatus(t, service, "demo-box", "stopped")
	deleted = controllerHTTP(service, http.MethodDelete, "/v1/workspaces/demo-box", "test-token", nil)
	if deleted.Code != http.StatusOK {
		t.Fatalf("idempotent delete status=%d", deleted.Code)
	}
	_, stops, connections := runner.counts()
	if stops != 1 || connections != 1 {
		t.Fatalf("stop calls=%d connection calls=%d", stops, connections)
	}
	runner.mu.Lock()
	cleanupRequests := append([]controllerWorkspaceRequest(nil), runner.cleanupAbsentRequests...)
	runner.mu.Unlock()
	if len(cleanupRequests) != 1 || cleanupRequests[0].CoordinatorRegistrationURL != runner.coordinatorRegistrationURL {
		t.Fatalf("confirmed-absence cleanup binding=%#v", cleanupRequests)
	}
}

func TestControllerResponseCapabilitiesDoNotInferTerminalFromDesktop(t *testing.T) {
	response := controllerResponse(controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{
			ID:           "desktop-only-box",
			Capabilities: controllerCapabilities{Desktop: true, Browser: true, Code: true},
		},
		Status: "ready",
	})
	want := controllerWorkspaceResponseCapabilities{VNC: true, Desktop: true}
	if response.Capabilities != want {
		t.Fatalf("capabilities=%#v want=%#v", response.Capabilities, want)
	}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"browser"`)) || bytes.Contains(data, []byte(`"code"`)) {
		t.Fatalf("response leaked non-contract capabilities: %s", data)
	}
}

func TestControllerPersistsRawAcquireIdentityBeforeAcknowledgingWarmup(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	acknowledged := make(chan controllerWorkspaceRecord, 1)
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	runner.afterAcquireAck = func(identity controllerAcquireIdentity) error {
		state, err := loadControllerState(service.opts.StateFile)
		if err != nil {
			return err
		}
		record, ok := state.Workspaces["raw-identity-box"]
		if !ok {
			return fmt.Errorf("workspace identity is not durable")
		}
		if !controllerRecordHasAcquiredIdentity(record) || record.LeaseID != identity.LeaseID ||
			record.Slug != identity.Slug || record.ProviderRoute != identity.Provider || record.ProviderResourceID != identity.ResourceID {
			return fmt.Errorf("durable raw identity mismatch: %#v", record)
		}
		acknowledged <- record
		return nil
	}
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "raw-identity-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	ready := waitControllerWorkspaceStatus(t, service, "raw-identity-box", "ready")
	select {
	case durable := <-acknowledged:
		if durable.Status != "provisioning" || durable.ProviderResourceID != "resource-123" {
			t.Fatalf("identity acknowledgment state=%#v", durable)
		}
	case <-time.After(time.Second):
		t.Fatal("warmup continued without a durable raw identity acknowledgment")
	}
	if ready.ProviderResourceID != "resource-123" {
		t.Fatalf("ready identity=%#v", ready)
	}
}

func TestControllerRejectsAcquireAcknowledgmentWhenIdentityPersistenceFails(t *testing.T) {
	service, cancel := testControllerService(t, newFakeControllerWorkspaceRunner(), 1)
	defer cancel()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "identity-write-failure"}, ProviderRoute: "external", ProviderScope: "test-provider-scope",
		CreatePrepared: true, CreateStarted: true, AttemptLeaseID: "cbx_123456789abc", Slug: "identity-write-failure",
		Status: "provisioning", Message: "workspace provisioning", CreatedAt: now, UpdatedAt: now,
	}
	service.state.Workspaces[record.Request.ID] = record
	service.saveState = func(string, controllerState) error { return errors.New("state storage unavailable") }
	identity := controllerAcquireIdentity{
		LeaseID: record.AttemptLeaseID, Slug: record.Slug, Provider: record.ProviderRoute, ResourceID: "provider/raw-resource",
	}
	if err := service.persistAcquiredIdentity(record.Request.ID, record.AttemptLeaseID, record.Slug, identity); err == nil {
		t.Fatal("raw acquire identity was acknowledged without durable persistence")
	}
	current, _ := service.workspace(record.Request.ID)
	if current.CreateObserved || current.LeaseID != "" || current.ProviderResourceID != "" {
		t.Fatalf("failed identity persistence mutated controller state: %#v", current)
	}
}

func TestControllerInspectionCannotFirstAdoptChangedProviderResource(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	service.opts.RetryDelay = 10 * time.Millisecond
	runner.afterAcquireAck = func(controllerAcquireIdentity) error {
		runner.mu.Lock()
		status := runner.ready["changed-resource-box"]
		status.ServerID = "resource-replacement"
		runner.ready["changed-resource-box"] = status
		runner.mu.Unlock()
		return nil
	}
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "changed-resource-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	failed := waitControllerWorkspaceStatus(t, service, "changed-resource-box", "failed")
	if failed.ProviderResourceID != "resource-123" || !strings.Contains(failed.Message, "identity mismatch") {
		t.Fatalf("inspection adopted replacement provider resource: %#v", failed)
	}
}

func TestControllerRequiredLeasePolicyRejectsBeforePersistence(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	service.opts.RequiredTTLSeconds = 4 * 60 * 60
	service.opts.RequiredIdleSeconds = 4 * 60 * 60
	for name, request := range map[string]controllerWorkspaceRequest{
		"omitted":       {ID: "policy-omitted-box"},
		"ttl mismatch":  {ID: "policy-ttl-box", TTLSeconds: 3600, IdleTimeoutSeconds: 3600},
		"idle mismatch": {ID: "policy-idle-box", TTLSeconds: 4 * 60 * 60, IdleTimeoutSeconds: 3600},
	} {
		t.Run(name, func(t *testing.T) {
			response := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if _, ok := service.workspace(request.ID); ok {
				t.Fatal("policy-rejected request reached controller state")
			}
		})
	}
	warmups, _, _ := runner.counts()
	if warmups != 0 {
		t.Fatalf("policy-rejected requests reached provider: warmups=%d", warmups)
	}
	accepted := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{
		ID: "policy-accepted-box", TTLSeconds: 4 * 60 * 60, IdleTimeoutSeconds: 4 * 60 * 60,
	})
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("matching policy status=%d body=%s", accepted.Code, accepted.Body.String())
	}
}

func TestControllerForbidsShapeOverridesAfterAuthenticationBeforeSideEffects(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	service.opts.ForbidClassOverride = true
	service.opts.ForbidServerTypeOverride = true

	unauthorized := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "wrong-token", controllerWorkspaceRequest{
		ID: "unauthorized-shape-box", Class: "large", ServerType: "cpu16",
	})
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
	for name, request := range map[string]controllerWorkspaceRequest{
		"class":       {ID: "class-override-box", Class: "large"},
		"server type": {ID: "server-type-override-box", ServerType: "cpu16"},
	} {
		t.Run(name, func(t *testing.T) {
			response := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if _, ok := service.workspace(request.ID); ok {
				t.Fatal("shape-policy-rejected request reached controller state")
			}
		})
	}
	warmups, _, _ := runner.counts()
	if warmups != 0 {
		t.Fatalf("shape-policy-rejected requests reached provider: warmups=%d", warmups)
	}
}

func TestControllerSerializesDesktopConnectionSetupPerWorkspace(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.connectionStarted = make(chan struct{}, 2)
	runner.blockConnection = make(chan struct{})
	service, cancel := testControllerService(t, runner, 2)
	defer cancel()
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{
		ID: "serialized-desktop-box", Capabilities: controllerCapabilities{Desktop: true},
	})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	waitControllerWorkspaceStatus(t, service, "serialized-desktop-box", "ready")
	responses := make(chan *httptest.ResponseRecorder, 2)
	openConnection := func() {
		responses <- controllerHTTP(service, http.MethodPost, "/v1/workspaces/serialized-desktop-box/connections/desktop", "test-token", nil)
	}
	go openConnection()
	select {
	case <-runner.connectionStarted:
	case <-time.After(time.Second):
		t.Fatal("first desktop connection did not start")
	}
	go openConnection()
	select {
	case <-runner.connectionStarted:
		t.Fatal("second desktop connection entered runner concurrently")
	case <-time.After(100 * time.Millisecond):
	}
	if got := len(service.sem); got != 1 {
		t.Fatalf("queued same-workspace connection consumed global lifecycle capacity: slots=%d", got)
	}
	close(runner.blockConnection)
	for range 2 {
		select {
		case response := <-responses:
			if response.Code != http.StatusOK {
				t.Fatalf("desktop connection status=%d body=%s", response.Code, response.Body.String())
			}
		case <-time.After(time.Second):
			t.Fatal("desktop connection did not finish")
		}
	}
}

func TestControllerDesktopConnectionCancellationRevokesLateBridge(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.connectionStarted = make(chan struct{}, 1)
	runner.blockConnection = make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(runner.blockConnection) }) }
	defer unblock()
	runner.ignoreConnectionCancellation = true
	service, cancelService := testControllerService(t, runner, 1)
	defer cancelService()
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{
		ID: "canceled-desktop-box", Capabilities: controllerCapabilities{Desktop: true},
	})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	waitControllerWorkspaceStatus(t, service, "canceled-desktop-box", "ready")

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	request := httptest.NewRequest(http.MethodPost, "/v1/workspaces/canceled-desktop-box/connections/desktop", nil).WithContext(requestCtx)
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.ServeHTTP(response, request)
	}()
	select {
	case <-runner.connectionStarted:
	case <-time.After(time.Second):
		t.Fatal("desktop setup did not start")
	}
	cancelRequest()
	unblock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled desktop setup did not finish")
	}
	if response.Code != http.StatusBadGateway {
		t.Fatalf("connection status=%d body=%s", response.Code, response.Body.String())
	}
	runner.mu.Lock()
	localStops := runner.localStopCalls
	runner.mu.Unlock()
	if localStops != 1 {
		t.Fatalf("canceled setup left bridge live: local revocations=%d", localStops)
	}
	if got := len(service.desktopSetups); got != 0 {
		t.Fatalf("canceled setup retained desktop slot: %d", got)
	}
	if got := len(service.workspaceConnectionSlot("canceled-desktop-box")); got != 0 {
		t.Fatalf("canceled setup retained workspace slot: %d", got)
	}
	if got := len(service.sem); got != 0 {
		t.Fatalf("canceled setup retained lifecycle slot: %d", got)
	}
}

func TestControllerSerializesDesktopDaemonSetupAcrossWorkspaces(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.connectionStarted = make(chan struct{}, 2)
	runner.blockConnection = make(chan struct{})
	service, cancel := testControllerService(t, runner, 2)
	defer cancel()
	for _, id := range []string{"desktop-port-a", "desktop-port-b"} {
		created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{
			ID: id, Capabilities: controllerCapabilities{Desktop: true},
		})
		if created.Code != http.StatusAccepted {
			t.Fatalf("create %s status=%d", id, created.Code)
		}
		waitControllerWorkspaceStatus(t, service, id, "ready")
	}
	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		responses <- controllerHTTP(service, http.MethodPost, "/v1/workspaces/desktop-port-a/connections/desktop", "test-token", nil)
	}()
	select {
	case <-runner.connectionStarted:
	case <-time.After(time.Second):
		t.Fatal("first desktop setup did not start")
	}
	go func() {
		responses <- controllerHTTP(service, http.MethodPost, "/v1/workspaces/desktop-port-b/connections/desktop", "test-token", nil)
	}()
	select {
	case <-runner.connectionStarted:
		t.Fatal("different-workspace daemon setup entered runner concurrently")
	case <-time.After(100 * time.Millisecond):
	}
	close(runner.blockConnection)
	for range 2 {
		select {
		case response := <-responses:
			if response.Code != http.StatusOK {
				t.Fatalf("desktop connection status=%d body=%s", response.Code, response.Body.String())
			}
		case <-time.After(time.Second):
			t.Fatal("desktop setup did not finish")
		}
	}
}

func TestControllerDesktopConnectionRejectsExpiredWorkspaceBeforeSetup(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	base := time.Now().UTC().Truncate(time.Second)
	service.now = func() time.Time { return base }
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{
		ID: "expired-desktop-box", TTLSeconds: 60, Capabilities: controllerCapabilities{Desktop: true},
	})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	waitControllerWorkspaceStatus(t, service, "expired-desktop-box", "ready")
	waitControllerWorkspaceInactive(t, service, "expired-desktop-box")
	expiredAt := base.Add(61 * time.Second)
	expiredTick := time.Now()
	service.now = func() time.Time { return expiredAt.Add(time.Since(expiredTick)) }
	connection := controllerHTTP(service, http.MethodPost, "/v1/workspaces/expired-desktop-box/connections/desktop", "test-token", nil)
	if connection.Code != http.StatusConflict {
		t.Fatalf("connection status=%d body=%s", connection.Code, connection.Body.String())
	}
	waitControllerWorkspaceStatus(t, service, "expired-desktop-box", "expired")
	_, _, connections := runner.counts()
	if connections != 0 {
		t.Fatalf("expired workspace entered desktop setup %d times", connections)
	}
}

func TestControllerDesktopConnectionRevokesBridgeWhenDeleteRacesSetup(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.connectionStarted = make(chan struct{}, 1)
	runner.blockConnection = make(chan struct{})
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{
		ID: "delete-race-desktop-box", Capabilities: controllerCapabilities{Desktop: true},
	})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	waitControllerWorkspaceStatus(t, service, "delete-race-desktop-box", "ready")
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- controllerHTTP(service, http.MethodPost, "/v1/workspaces/delete-race-desktop-box/connections/desktop", "test-token", nil)
	}()
	select {
	case <-runner.connectionStarted:
	case <-time.After(time.Second):
		t.Fatal("desktop setup did not start")
	}
	deleted := controllerHTTP(service, http.MethodDelete, "/v1/workspaces/delete-race-desktop-box", "test-token", nil)
	if deleted.Code != http.StatusAccepted {
		t.Fatalf("delete status=%d", deleted.Code)
	}
	close(runner.blockConnection)
	select {
	case response := <-responses:
		if response.Code != http.StatusConflict {
			t.Fatalf("connection status=%d body=%s", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("desktop connection did not finish")
	}
	waitControllerWorkspaceStatus(t, service, "delete-race-desktop-box", "stopped")
	runner.mu.Lock()
	providerStops := runner.stopCalls
	localStops := runner.localStopCalls
	runner.mu.Unlock()
	if providerStops != 1 || localStops != 1 {
		t.Fatalf("provider stops=%d local revocations=%d", providerStops, localStops)
	}
}

func TestControllerDurabilityBarrierCancelsAndRevokesDesktopSetup(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.connectionStarted = make(chan struct{}, 1)
	runner.blockConnection = make(chan struct{})
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{
		ID: "durability-race-desktop-box", Capabilities: controllerCapabilities{Desktop: true},
	})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	waitControllerWorkspaceStatus(t, service, "durability-race-desktop-box", "ready")
	waitControllerWorkspaceInactive(t, service, "durability-race-desktop-box")
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- controllerHTTP(service, http.MethodPost, "/v1/workspaces/durability-race-desktop-box/connections/desktop", "test-token", nil)
	}()
	select {
	case <-runner.connectionStarted:
	case <-time.After(time.Second):
		t.Fatal("desktop setup did not start")
	}
	dir := filepath.Dir(service.opts.StateFile)
	service.saveState = func(path string, state controllerState) error {
		return saveControllerStateWithDirectorySync(path, state, func(syncPath string) error {
			if filepath.Clean(syncPath) == filepath.Clean(dir) {
				return errors.New("persistent desktop setup sync failure")
			}
			return nil
		})
	}
	if err := service.updateRecord("durability-race-desktop-box", func(record *controllerWorkspaceRecord) bool {
		record.Message = "ready state refresh"
		record.UpdatedAt = service.now().UTC().Format(time.RFC3339Nano)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-responses:
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("connection status=%d body=%s", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("durability-canceled desktop setup did not finish")
	}
	runner.mu.Lock()
	localStops := runner.localStopCalls
	runner.mu.Unlock()
	if localStops != 1 {
		t.Fatalf("durability barrier left bridge live: local revocations=%d", localStops)
	}
	service.mu.Lock()
	pending := service.durabilityPending
	service.mu.Unlock()
	if !pending {
		t.Fatal("persistent sync failure unexpectedly cleared durability barrier")
	}
}

func TestControllerDesktopConnectionRevokesBridgeWhenExpiryRacesSetup(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.connectionStarted = make(chan struct{}, 1)
	runner.blockConnection = make(chan struct{})
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	base := time.Now().UTC().Truncate(time.Second)
	service.now = func() time.Time { return base }
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{
		ID: "expiry-race-desktop-box", TTLSeconds: 60, Capabilities: controllerCapabilities{Desktop: true},
	})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	waitControllerWorkspaceStatus(t, service, "expiry-race-desktop-box", "ready")
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- controllerHTTP(service, http.MethodPost, "/v1/workspaces/expiry-race-desktop-box/connections/desktop", "test-token", nil)
	}()
	select {
	case <-runner.connectionStarted:
	case <-time.After(time.Second):
		t.Fatal("desktop setup did not start")
	}
	expiredAt := base.Add(61 * time.Second)
	expiredTick := time.Now()
	service.now = func() time.Time { return expiredAt.Add(time.Since(expiredTick)) }
	close(runner.blockConnection)
	select {
	case response := <-responses:
		if response.Code != http.StatusConflict {
			t.Fatalf("connection status=%d body=%s", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("desktop connection did not finish")
	}
	waitControllerWorkspaceStatus(t, service, "expiry-race-desktop-box", "expired")
	runner.mu.Lock()
	localStops := runner.localStopCalls
	runner.mu.Unlock()
	if localStops != 1 {
		t.Fatalf("local revocations=%d want=1", localStops)
	}
}

func TestControllerDesktopConnectionDoesNotUseTemplateWithoutVerifiedBridge(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.connectionErr = errors.New("bridge is not connected")
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{
		ID: "unverified-desktop-box", Capabilities: controllerCapabilities{Desktop: true},
	})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	waitControllerWorkspaceStatus(t, service, "unverified-desktop-box", "ready")
	connection := controllerHTTP(service, http.MethodPost, "/v1/workspaces/unverified-desktop-box/connections/desktop", "test-token", nil)
	if connection.Code != http.StatusBadGateway {
		t.Fatalf("connection status=%d body=%s", connection.Code, connection.Body.String())
	}
	runner.mu.Lock()
	localStops := runner.localStopCalls
	runner.mu.Unlock()
	if localStops != 1 {
		t.Fatalf("failed setup local revocations=%d want=1", localStops)
	}
}

func TestControllerGetReconcilesProviderExpiry(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "expiry-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	waitControllerWorkspaceStatus(t, service, "expiry-box", "ready")
	runner.mu.Lock()
	delete(runner.ready, "expiry-box")
	runner.mu.Unlock()
	polled := controllerHTTP(service, http.MethodGet, "/v1/workspaces/expiry-box", "test-token", nil)
	if polled.Code != http.StatusOK {
		t.Fatalf("poll status=%d", polled.Code)
	}
	record := waitControllerWorkspaceStatus(t, service, "expiry-box", "expired")
	_, stops, _ := runner.counts()
	if stops != 1 {
		t.Fatalf("provider absence cleanup stops=%d", stops)
	}
	if record.AttachURL != "" {
		t.Fatalf("expired workspace retained attach URL %q", record.AttachURL)
	}
}

func TestControllerReadyWorkspaceReconcilesWithoutPolling(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	service.opts.ReadyReconcileInterval = 10 * time.Millisecond
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "autonomous-expiry-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	waitControllerWorkspaceStatus(t, service, "autonomous-expiry-box", "ready")
	runner.mu.Lock()
	status := runner.ready["autonomous-expiry-box"]
	status.ExpiresAt = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	runner.ready["autonomous-expiry-box"] = status
	runner.mu.Unlock()
	waitControllerWorkspaceStatus(t, service, "autonomous-expiry-box", "expired")
	_, stops, _ := runner.counts()
	if stops != 1 {
		t.Fatalf("autonomous expiry cleanup stops=%d", stops)
	}
}

func TestControllerReconcileSchedulingKeepsOneTimerPerWorkspace(t *testing.T) {
	service, cancel := testControllerService(t, newFakeControllerWorkspaceRunner(), 1)
	defer cancel()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "timer-box"}, Status: "ready", LeaseID: "cbx_timer123456",
		Slug: "cbx-ctl-timer-box-0000000000000000", Message: "workspace ready", CreatedAt: now, UpdatedAt: now,
	}
	service.state.Workspaces[record.Request.ID] = record
	service.opts.RetryDelay = time.Hour
	service.scheduleReconcile(record.Request.ID)
	service.mu.Lock()
	first := service.reconcileTimers[record.Request.ID]
	service.mu.Unlock()
	for range 100 {
		service.scheduleReconcile(record.Request.ID)
	}
	service.mu.Lock()
	last := service.reconcileTimers[record.Request.ID]
	timerCount := len(service.reconcileTimers)
	service.mu.Unlock()
	if timerCount != 1 || first == nil || last == nil || first == last {
		t.Fatalf("timer count=%d first=%p last=%p", timerCount, first, last)
	}
	if err := service.markFailed(record.Request.ID, "terminal"); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	timerCount = len(service.reconcileTimers)
	service.mu.Unlock()
	if timerCount != 0 {
		t.Fatalf("terminal transition retained %d reconcile timers", timerCount)
	}
}

func TestControllerGetReconcilesTerminalProviderState(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "terminal-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	record := waitControllerWorkspaceStatus(t, service, "terminal-box", "ready")
	runner.mu.Lock()
	status := runner.ready["terminal-box"]
	status.Ready = false
	status.State = "released"
	runner.ready["terminal-box"] = status
	runner.mu.Unlock()
	_ = controllerHTTP(service, http.MethodGet, "/v1/workspaces/terminal-box", "test-token", nil)
	record = waitControllerWorkspaceStatus(t, service, "terminal-box", "stopped")
	_, stops, _ := runner.counts()
	if stops != 1 {
		t.Fatalf("terminal provider state cleanup stops=%d", stops)
	}
	if record.AttachURL != "" {
		t.Fatalf("terminal workspace retained attach URL %q", record.AttachURL)
	}
}

func TestControllerFailedProviderStateCleansBeforeFailed(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "provider-failed-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	waitControllerWorkspaceStatus(t, service, "provider-failed-box", "ready")
	runner.mu.Lock()
	status := runner.ready["provider-failed-box"]
	status.Ready = false
	status.State = "failed"
	runner.ready["provider-failed-box"] = status
	runner.mu.Unlock()
	_ = controllerHTTP(service, http.MethodGet, "/v1/workspaces/provider-failed-box", "test-token", nil)
	waitControllerWorkspaceStatus(t, service, "provider-failed-box", "failed")
	_, stops, _ := runner.counts()
	if stops != 1 {
		t.Fatalf("failed provider state cleanup stops=%d", stops)
	}
}

func TestControllerReadyExpiryStopsProviderBeforeTerminalState(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "ttl-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	waitControllerWorkspaceStatus(t, service, "ttl-box", "ready")
	runner.mu.Lock()
	status := runner.ready["ttl-box"]
	status.ExpiresAt = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	runner.ready["ttl-box"] = status
	runner.mu.Unlock()
	_ = controllerHTTP(service, http.MethodGet, "/v1/workspaces/ttl-box", "test-token", nil)
	record := waitControllerWorkspaceStatus(t, service, "ttl-box", "expired")
	_, stops, _ := runner.counts()
	if stops != 1 || record.AttachURL != "" {
		t.Fatalf("expiry cleanup stops=%d record=%#v", stops, record)
	}
}

func TestControllerReadyExpiryCleanupRetries(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "ttl-retry-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	waitControllerWorkspaceStatus(t, service, "ttl-retry-box", "ready")
	runner.mu.Lock()
	status := runner.ready["ttl-retry-box"]
	status.ExpiresAt = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	runner.ready["ttl-retry-box"] = status
	runner.stopErr = errors.New("provider unavailable")
	runner.mu.Unlock()
	_ = controllerHTTP(service, http.MethodGet, "/v1/workspaces/ttl-retry-box", "test-token", nil)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		record, _ := service.workspace("ttl-retry-box")
		if record.Status == "stopping" && record.StatusAfterCleanup == "expired" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	record, _ := service.workspace("ttl-retry-box")
	if record.Status != "stopping" || record.StatusAfterCleanup != "expired" {
		t.Fatalf("expiry cleanup did not remain pending: %#v", record)
	}
	runner.mu.Lock()
	runner.stopErr = nil
	runner.mu.Unlock()
	waitControllerWorkspaceStatus(t, service, "ttl-retry-box", "expired")
}

func TestControllerFreshCreateDoesNotAdoptExistingSlug(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.ready["collision-box"] = fakeControllerStatus("collision-box")
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "collision-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	record := waitControllerWorkspaceStatus(t, service, "collision-box", "ready")
	warmups, _, _ := runner.counts()
	if warmups != 1 {
		t.Fatalf("fresh request adopted existing slug; warmups=%d", warmups)
	}
	if record.Slug == record.Request.ID || !strings.HasPrefix(record.Slug, "cbx-ctl-collision-box-") {
		t.Fatalf("unsafe provisioning slug=%q", record.Slug)
	}
}

func TestControllerProvisioningSlugHonorsRequestedLeaseLimit(t *testing.T) {
	id := strings.Repeat("a", 63)
	slug, err := newControllerProvisioningSlug(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(slug) != maxRequestedLeaseSlugLength {
		t.Fatalf("slug length=%d want=%d slug=%q", len(slug), maxRequestedLeaseSlugLength, slug)
	}
	if !strings.HasPrefix(slug, "cbx-ctl-"+strings.Repeat("a", 16)+"-") {
		t.Fatalf("long workspace ID was not deterministically budgeted: %q", slug)
	}
	second, err := newControllerProvisioningSlug(id)
	if err != nil {
		t.Fatal(err)
	}
	if second == slug || len(second) > maxRequestedLeaseSlugLength {
		t.Fatalf("collision suffix missing or over budget: first=%q second=%q", slug, second)
	}
}

func TestControllerProvisioningSlugMatchesRequestedLeaseNormalization(t *testing.T) {
	slug, err := newControllerProvisioningSlug("alpha--beta---gamma")
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := requestedLeaseSlug(slug)
	if err != nil {
		t.Fatal(err)
	}
	if slug != normalized || strings.Contains(slug, "--") {
		t.Fatalf("persisted slug differs from provider-request normalization: slug=%q normalized=%q", slug, normalized)
	}
	if !strings.HasPrefix(slug, "cbx-ctl-alpha-beta-") {
		t.Fatalf("workspace ID was not normalized before budgeting: %q", slug)
	}
}

func TestControllerCreatesMaxLengthWorkspaceIDWithinSlugBudget(t *testing.T) {
	service, cancel := testControllerService(t, newFakeControllerWorkspaceRunner(), 1)
	defer cancel()
	id := strings.Repeat("z", 63)
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: id})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	ready := waitControllerWorkspaceStatus(t, service, id, "ready")
	if len(ready.Slug) > maxRequestedLeaseSlugLength {
		t.Fatalf("provisioning slug length=%d limit=%d slug=%q", len(ready.Slug), maxRequestedLeaseSlugLength, ready.Slug)
	}
}

func TestControllerImmediateDeleteCancelsBeforeWarmup(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	service.sem <- struct{}{}
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "cancel-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	deleted := controllerHTTP(service, http.MethodDelete, "/v1/workspaces/cancel-box", "test-token", nil)
	if deleted.Code != http.StatusAccepted {
		t.Fatalf("delete status=%d", deleted.Code)
	}
	<-service.sem
	waitControllerWorkspaceStatus(t, service, "cancel-box", "stopped")
	warmups, stops, _ := runner.counts()
	if warmups != 0 || stops != 0 {
		t.Fatalf("cancel invoked provider warmup=%d stop=%d", warmups, stops)
	}
}

func TestControllerDeleteCancelsActiveWarmup(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.started = make(chan string, 1)
	runner.blockWarmup = make(chan struct{})
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	service.opts.CreateTimeout = 100 * time.Millisecond
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "active-cancel-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("warmup did not start")
	}
	deleted := controllerHTTP(service, http.MethodDelete, "/v1/workspaces/active-cancel-box", "test-token", nil)
	if deleted.Code != http.StatusAccepted {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	waitControllerWorkspaceStatus(t, service, "active-cancel-box", "stopped")
	service.mu.Lock()
	_, tracked := service.createOps["active-cancel-box"]
	service.mu.Unlock()
	if tracked {
		t.Fatal("completed warmup cancellation remained in active create map")
	}
	warmups, stops, _ := runner.counts()
	if warmups != 1 || stops != 0 {
		t.Fatalf("warmup cancellation calls warmups=%d stops=%d", warmups, stops)
	}
}

func TestControllerExpiryTransitionCancelsActiveWarmup(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.started = make(chan string, 1)
	runner.blockWarmup = make(chan struct{})
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	service.opts.CreateTimeout = 100 * time.Millisecond
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "active-expiry-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("warmup did not start")
	}
	if err := service.updateRecord("active-expiry-box", func(record *controllerWorkspaceRecord) bool {
		record.Status = "stopping"
		record.StatusAfterCleanup = "expired"
		record.FailureAfterCleanup = "workspace TTL expired during provisioning"
		record.Message = "workspace TTL expired during provisioning; cleanup requested"
		record.UpdatedAt = service.now().UTC().Format(time.RFC3339Nano)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	service.enqueue("active-expiry-box")
	waitControllerWorkspaceStatus(t, service, "active-expiry-box", "expired")
	service.mu.Lock()
	_, tracked := service.createOps["active-expiry-box"]
	service.mu.Unlock()
	if tracked {
		t.Fatal("expired warmup remained in active create map")
	}
}

func TestControllerShutdownCancelsActiveCleanup(t *testing.T) {
	runner := &shutdownAwareControllerRunner{
		fakeControllerWorkspaceRunner: newFakeControllerWorkspaceRunner(),
		stopStarted:                   make(chan struct{}, 1),
		stopCanceled:                  make(chan error, 1),
	}
	service, cancel := testControllerService(t, runner, 1)
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "shutdown-cleanup-box"})
	if created.Code != http.StatusAccepted {
		cancel()
		t.Fatalf("create status=%d", created.Code)
	}
	waitControllerWorkspaceStatus(t, service, "shutdown-cleanup-box", "ready")
	deleted := controllerHTTP(service, http.MethodDelete, "/v1/workspaces/shutdown-cleanup-box", "test-token", nil)
	if deleted.Code != http.StatusAccepted {
		cancel()
		t.Fatalf("delete status=%d", deleted.Code)
	}
	select {
	case <-runner.stopStarted:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("provider cleanup did not start")
	}
	cancel()
	service.waitForShutdown()
	select {
	case err := <-runner.stopCanceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cleanup cancellation=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("controller shutdown did not cancel provider cleanup")
	}
	waitControllerWorkspaceInactive(t, service, "shutdown-cleanup-box")
}

func TestControllerWarmupFailureCleansProvisionedResource(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.warmupErr = errors.New("bootstrap failed")
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "failed-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	record := waitControllerWorkspaceStatus(t, service, "failed-box", "failed")
	_, stops, _ := runner.counts()
	if stops != 1 || record.LeaseID == "" || !record.CreateObserved || record.ProviderResourceID == "" {
		t.Fatalf("cleanup stops=%d record=%#v", stops, record)
	}
}

func TestControllerPreAcquireAckFailureRecoversIdentityAndCleansLateResource(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.warmupErr = errors.New("provider response timed out")
	runner.warmupPreAcquireFailures = 1
	runner.materializePreAcquireFailure = true
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()

	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "pre-ack-late-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	failed := waitControllerWorkspaceStatus(t, service, "pre-ack-late-box", "failed")
	if !controllerRecordHasAcquiredIdentity(failed) || failed.LeaseID != failed.AttemptLeaseID {
		t.Fatalf("recovered raw provider identity=%#v", failed)
	}
	if failed.Message != "workspace provisioning failed before provider identity acknowledgment" {
		t.Fatalf("failure message=%q", failed.Message)
	}

	runner.mu.Lock()
	warmups := runner.warmupCalls
	stops := runner.stopCalls
	stopIdentifiers := append([]string(nil), runner.stopIdentifiers...)
	_, stillPresent := runner.ready[failed.Request.ID]
	runner.mu.Unlock()
	if warmups != 2 || stops != 1 || stillPresent {
		t.Fatalf("identity recovery warmups=%d stops=%d still present=%t", warmups, stops, stillPresent)
	}
	if len(stopIdentifiers) != 1 || stopIdentifiers[0] != failed.AttemptLeaseID {
		t.Fatalf("stop identifiers=%v attempt=%q", stopIdentifiers, failed.AttemptLeaseID)
	}
}

func TestControllerPreAcquireAckFailureRetainsStoppingUntilStableAbsence(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.warmupErr = errors.New("provider response malformed")
	runner.warmupPreAcquireFailures = 1
	runner.started = make(chan string, 1)
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	service.opts.CreateTimeout = 2 * time.Second
	base := time.Now().UTC()
	service.now = func() time.Time { return base }

	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "pre-ack-absent-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("provider command did not start")
	}
	deadline := time.Now().Add(time.Second)
	var stopping controllerWorkspaceRecord
	for time.Now().Before(deadline) {
		current, ok := service.workspace("pre-ack-absent-box")
		if ok && current.Status == "stopping" && current.FailureAfterCleanup != "" {
			stopping = current
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if stopping.Status != "stopping" || stopping.AttemptLeaseID == "" || stopping.Slug == "" || stopping.CreateObserved {
		t.Fatalf("pre-ack failure did not retain stable cleanup identity: %#v", stopping)
	}
	time.Sleep(100 * time.Millisecond)
	current, _ := service.workspace(stopping.Request.ID)
	if current.Status != "stopping" || current.AttemptLeaseID != stopping.AttemptLeaseID || current.Slug != stopping.Slug {
		t.Fatalf("workspace escaped late-materialization recovery window: %#v", current)
	}

	service.now = func() time.Time { return base.Add(3 * time.Second) }
	service.enqueue(stopping.Request.ID)
	failed := waitControllerWorkspaceStatus(t, service, stopping.Request.ID, "failed")
	if failed.Message != "workspace provisioning failed before provider identity acknowledgment" {
		t.Fatalf("failure message=%q", failed.Message)
	}
	runner.mu.Lock()
	warmups := runner.warmupCalls
	stops := runner.stopCalls
	runner.mu.Unlock()
	if warmups != 1 || stops != 0 {
		t.Fatalf("stable absence warmups=%d destructive stops=%d", warmups, stops)
	}
}

func TestControllerTransientInspectionAfterWarmupRetriesWithoutCleanup(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.inspectErr = errors.New("provider inventory temporarily unavailable")
	runner.inspectFailures = 1
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "inspect-retry-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	waitControllerWorkspaceStatus(t, service, "inspect-retry-box", "ready")
	warmups, stops, _ := runner.counts()
	if warmups != 1 || stops != 0 {
		t.Fatalf("transient inspection warmups=%d stops=%d", warmups, stops)
	}
}

func TestControllerFailedProvisionCleanupRetriesBeforeTerminalState(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.warmupErr = errors.New("bootstrap failed")
	runner.stopErr = errors.New("provider unavailable")
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "cleanup-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		record, _ := service.workspace("cleanup-box")
		if record.Status == "stopping" && record.FailureAfterCleanup != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	record, _ := service.workspace("cleanup-box")
	if record.Status != "stopping" || record.FailureAfterCleanup == "" {
		t.Fatalf("cleanup did not remain pending: %#v", record)
	}
	runner.mu.Lock()
	runner.stopErr = nil
	runner.mu.Unlock()
	waitControllerWorkspaceStatus(t, service, "cleanup-box", "failed")
}

func TestControllerStopFailureRemainsPendingAndRetries(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "retry-stop-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	waitControllerWorkspaceStatus(t, service, "retry-stop-box", "ready")
	runner.mu.Lock()
	runner.stopErr = errors.New("provider unavailable")
	runner.mu.Unlock()
	deleted := controllerHTTP(service, http.MethodDelete, "/v1/workspaces/retry-stop-box", "test-token", nil)
	if deleted.Code != http.StatusAccepted {
		t.Fatalf("delete status=%d", deleted.Code)
	}
	deadline := time.Now().Add(time.Second)
	pending := false
	for time.Now().Before(deadline) {
		if record, _ := service.workspace("retry-stop-box"); record.Status == "stopping" && strings.Contains(record.Message, "retrying") {
			pending = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !pending {
		t.Fatal("stop failure did not remain pending")
	}
	runner.mu.Lock()
	localStops := runner.localStopCalls
	runner.stopErr = nil
	runner.mu.Unlock()
	if localStops != 1 {
		t.Fatalf("provider failure did not independently revoke local desktop: local stops=%d", localStops)
	}
	waitControllerWorkspaceStatus(t, service, "retry-stop-box", "stopped")
	runner.mu.Lock()
	localStops = runner.localStopCalls
	runner.mu.Unlock()
	if localStops != 1 {
		t.Fatalf("provider retry repeated durable local cleanup: local stops=%d", localStops)
	}
}

func TestControllerConfirmedAbsenceCleanupRetriesWithoutRepeatingProviderStop(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "absence-cleanup-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	waitControllerWorkspaceStatus(t, service, "absence-cleanup-box", "ready")
	runner.mu.Lock()
	runner.cleanupAbsentErr = errors.New("local routing changed")
	runner.mu.Unlock()
	deleted := controllerHTTP(service, http.MethodDelete, "/v1/workspaces/absence-cleanup-box", "test-token", nil)
	if deleted.Code != http.StatusAccepted {
		t.Fatalf("delete status=%d", deleted.Code)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		record, _ := service.workspace("absence-cleanup-box")
		runner.mu.Lock()
		cleanupCalls := runner.cleanupAbsentCalls
		runner.mu.Unlock()
		if cleanupCalls > 0 && record.Status == "stopping" && strings.Contains(record.Message, "retrying") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	record, _ := service.workspace("absence-cleanup-box")
	if record.Status != "stopping" || record.ProviderStopped {
		t.Fatalf("failed local state cleanup was accepted: %#v", record)
	}
	runner.mu.Lock()
	providerStops := runner.stopCalls
	cleanupCallsBeforeRetry := runner.cleanupAbsentCalls
	runner.cleanupAbsentErr = nil
	runner.mu.Unlock()
	if providerStops != 1 || cleanupCallsBeforeRetry != 1 {
		t.Fatalf("provider stops=%d cleanup calls=%d before retry", providerStops, cleanupCallsBeforeRetry)
	}
	waitControllerWorkspaceStatus(t, service, "absence-cleanup-box", "stopped")
	runner.mu.Lock()
	providerStops = runner.stopCalls
	cleanupCalls := runner.cleanupAbsentCalls
	runner.mu.Unlock()
	if providerStops != 1 || cleanupCalls < 2 {
		t.Fatalf("provider stops=%d cleanup calls=%d", providerStops, cleanupCalls)
	}
}

func TestControllerPersistsProviderStoppedOnlyAfterStableRefreshedAbsence(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.confirmAbsentResults = []bool{false, true, true}
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	service.opts.RetryDelay = time.Hour
	base := time.Now().UTC()
	service.now = func() time.Time { return base }
	record := controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "stable-absence-box"}, ProviderRoute: "external", ProviderScope: "test-provider-scope",
		CreateStarted: true, CreateObserved: true, AttemptLeaseID: "cbx_stableabsence123", LeaseID: "cbx_stableabsence123",
		Slug: "stable-absence", Provider: "external", ProviderResourceID: "provider/stable-absence", Status: "stopping", LocalDesktopStopped: true,
		Message: "workspace stopping", CreatedAt: base.Format(time.RFC3339Nano), UpdatedAt: base.Format(time.RFC3339Nano),
	}
	service.state.Workspaces[record.Request.ID] = record

	service.stopWorkspace(record)
	current, _ := service.workspace(record.Request.ID)
	if current.ProviderStopped || current.ProviderAbsentSince != "" || current.Status != "stopping" {
		t.Fatalf("present refreshed inventory was accepted: %#v", current)
	}

	service.stopWorkspace(current)
	current, _ = service.workspace(record.Request.ID)
	if current.ProviderStopped || current.ProviderAbsentSince == "" || current.Status != "stopping" {
		t.Fatalf("first absence was not retained for stable confirmation: %#v", current)
	}
	runner.mu.Lock()
	cleanupCallsBeforeStable := runner.cleanupAbsentCalls
	runner.mu.Unlock()
	if cleanupCallsBeforeStable != 0 {
		t.Fatalf("local provider state cleaned after first absence: calls=%d", cleanupCallsBeforeStable)
	}

	service.now = func() time.Time { return base.Add(time.Hour) }
	service.stopWorkspace(current)
	stopped, _ := service.workspace(record.Request.ID)
	if stopped.Status != "stopped" || !stopped.ProviderStopped || stopped.ProviderAbsentSince != "" {
		t.Fatalf("stable absence did not complete stop: %#v", stopped)
	}
	runner.mu.Lock()
	stopCalls := runner.stopCalls
	confirmCalls := runner.confirmAbsentCalls
	cleanupCalls := runner.cleanupAbsentCalls
	runner.mu.Unlock()
	if stopCalls != 2 || confirmCalls != 3 || cleanupCalls != 1 {
		t.Fatalf("stop calls=%d confirm calls=%d cleanup calls=%d", stopCalls, confirmCalls, cleanupCalls)
	}
}

func TestControllerInventoryFailureDoesNotRepeatSuccessfulProviderRelease(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.confirmAbsentErr = errors.New("controller provider inventory exceeded 1048576-byte output limit")
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "inventory-overflow-box"}, ProviderRoute: "external", ProviderScope: "test-provider-scope",
		CreateStarted: true, CreateObserved: true, AttemptLeaseID: "cbx_inventory123", Status: "stopping", LeaseID: "cbx_inventory123",
		Slug: "inventory-overflow", ProviderResourceID: "provider/inventory-overflow", LocalDesktopStopped: true,
		Message: "workspace stopping", CreatedAt: now, UpdatedAt: now,
	}
	service.state.Workspaces[record.Request.ID] = record

	service.stopWorkspace(record)
	current, _ := service.workspace(record.Request.ID)
	if !current.ProviderReleaseRequested || current.ProviderStopped || current.Status != "stopping" {
		t.Fatalf("successful release was not durably retained across inventory failure: %#v", current)
	}
	service.stopWorkspace(current)
	runner.mu.Lock()
	stopCalls, confirmCalls := runner.stopCalls, runner.confirmAbsentCalls
	runner.mu.Unlock()
	if stopCalls != 1 || confirmCalls != 2 {
		t.Fatalf("provider release calls=%d inventory calls=%d", stopCalls, confirmCalls)
	}
}

func TestControllerRetainsUnobservedCreateThroughLateMaterializationWindow(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	service.opts.RetryDelay = time.Hour
	service.opts.CreateTimeout = 2 * time.Hour
	base := time.Now().UTC()
	service.now = func() time.Time { return base }
	record := controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "late-create-box"}, CreatePrepared: true, CreateStarted: true,
		CreatePreparedAt: base.Format(time.RFC3339Nano), AttemptLeaseID: "cbx_latecreate123",
		Status: "stopping", LocalDesktopStopped: true, Message: "workspace stopping",
		CreatedAt: base.Format(time.RFC3339Nano), UpdatedAt: base.Format(time.RFC3339Nano),
	}
	service.state.Workspaces[record.Request.ID] = record

	service.stopWorkspace(record)
	current, _ := service.workspace(record.Request.ID)
	if current.Status != "stopping" || current.ProviderStopped {
		t.Fatalf("unobserved create completed before recovery window: %#v", current)
	}
	service.now = func() time.Time { return base.Add(time.Hour) }
	service.stopWorkspace(current)
	current, _ = service.workspace(record.Request.ID)
	if current.Status != "stopping" || current.ProviderStopped {
		t.Fatalf("unobserved create completed inside recovery window: %#v", current)
	}

	runner.mu.Lock()
	runner.ready[record.Request.ID] = fakeControllerStatus(record.Request.ID)
	runner.mu.Unlock()
	service.now = func() time.Time { return base.Add(2 * time.Hour) }
	service.stopWorkspace(current)
	current, _ = service.workspace(record.Request.ID)
	if current.Status != "stopping" || current.ProviderStopped {
		t.Fatalf("late materialization skipped renewed stable window: %#v", current)
	}
	service.now = func() time.Time { return base.Add(3 * time.Hour) }
	service.stopWorkspace(current)
	blocked, _ := service.workspace(record.Request.ID)
	if blocked.Status != "stopping" || blocked.ProviderStopped || !strings.Contains(blocked.Message, "retrying") {
		t.Fatalf("late materialization without acknowledged raw identity was not blocked: %#v", blocked)
	}
	runner.mu.Lock()
	stopCalls := runner.stopCalls
	confirmCalls := runner.confirmAbsentCalls
	runner.mu.Unlock()
	if stopCalls != 0 || confirmCalls < 4 {
		t.Fatalf("stop calls=%d confirm calls=%d", stopCalls, confirmCalls)
	}
}

func TestControllerRetainsPreparedCreateThroughLaunchCallbackRace(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	service.opts.RetryDelay = time.Hour
	service.opts.CreateTimeout = 2 * time.Hour
	base := time.Now().UTC()
	service.now = func() time.Time { return base }
	record := controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "prepared-launch-race-box"}, CreatePrepared: true,
		CreatePreparedAt: base.Format(time.RFC3339Nano), AttemptLeaseID: "cbx_preparedrace123",
		Status: "stopping", LocalDesktopStopped: true, Message: "workspace stopping",
		CreatedAt: base.Format(time.RFC3339Nano), UpdatedAt: base.Format(time.RFC3339Nano),
	}
	service.state.Workspaces[record.Request.ID] = record

	service.stopWorkspace(record)
	current, _ := service.workspace(record.Request.ID)
	service.now = func() time.Time { return base.Add(time.Hour) }
	service.stopWorkspace(current)
	current, _ = service.workspace(record.Request.ID)
	if current.Status != "stopping" || current.ProviderStopped {
		t.Fatalf("prepared launch was released inside recovery window: %#v", current)
	}

	runner.mu.Lock()
	runner.ready[record.Request.ID] = fakeControllerStatus(record.Request.ID)
	runner.mu.Unlock()
	service.now = func() time.Time { return base.Add(2 * time.Hour) }
	service.stopWorkspace(current)
	current, _ = service.workspace(record.Request.ID)
	if current.Status != "stopping" || current.ProviderStopped {
		t.Fatalf("spawn/onStarted race skipped renewed stable window: %#v", current)
	}
	service.now = func() time.Time { return base.Add(3 * time.Hour) }
	service.stopWorkspace(current)
	blocked, _ := service.workspace(record.Request.ID)
	if blocked.Status != "stopping" || blocked.ProviderStopped || !strings.Contains(blocked.Message, "retrying") {
		t.Fatalf("prepared launch without acknowledged raw identity was not blocked: %#v", blocked)
	}
}

func TestControllerLocalCleanupRetriesWithoutRepeatingProviderStop(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "local-retry-box"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", created.Code)
	}
	waitControllerWorkspaceStatus(t, service, "local-retry-box", "ready")
	runner.mu.Lock()
	runner.localStopErr = errors.New("stale daemon pid")
	runner.mu.Unlock()
	deleted := controllerHTTP(service, http.MethodDelete, "/v1/workspaces/local-retry-box", "test-token", nil)
	if deleted.Code != http.StatusAccepted {
		t.Fatalf("delete status=%d", deleted.Code)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		record, _ := service.workspace("local-retry-box")
		if record.Status == "stopping" && record.ProviderStopped && strings.Contains(record.Message, "WebVNC cleanup failed") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	record, _ := service.workspace("local-retry-box")
	if record.Status != "stopping" || !record.ProviderStopped {
		t.Fatalf("local cleanup did not remain pending: %#v", record)
	}
	runner.mu.Lock()
	providerStops := runner.stopCalls
	runner.localStopErr = nil
	runner.mu.Unlock()
	if providerStops != 1 {
		t.Fatalf("provider stops=%d want=1", providerStops)
	}
	waitControllerWorkspaceStatus(t, service, "local-retry-box", "stopped")
	runner.mu.Lock()
	providerStops = runner.stopCalls
	localStops := runner.localStopCalls
	runner.mu.Unlock()
	if providerStops != 1 || localStops < 2 {
		t.Fatalf("provider stops=%d local stops=%d", providerStops, localStops)
	}
}

func TestControllerRestartResumesProviderStopAfterDurableLocalRevocation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	request := controllerWorkspaceRequest{ID: "restart-provider-stop-box", Profile: "public-desktop"}
	if err := saveControllerState(path, controllerState{
		Version: controllerStateVersion,
		Workspaces: map[string]controllerWorkspaceRecord{
			request.ID: {
				Request: request, ProviderRoute: "external", ProviderScope: "test-provider-scope", CreateStarted: true, CreateObserved: true,
				AttemptLeaseID: "cbx_restartstop123", Status: "stopping", LeaseID: "cbx_restartstop123",
				Slug: "cbx-ctl-restart-provider-stop-box-0000000000000000", Provider: "external", ProviderResourceID: "provider/restart-stop", LocalDesktopStopped: true,
				Message: "local desktop revoked; workspace provider stop failed; retrying", CreatedAt: now, UpdatedAt: now,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runner := newFakeControllerWorkspaceRunner()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := newControllerService(ctx, controllerServiceOptions{
		StateFile: path, MaxConcurrent: 1, Profile: request.Profile,
		CreateTimeout: time.Second, InspectTimeout: time.Second, StopTimeout: time.Second,
		ConnectionTimeout: time.Second, RetryDelay: 10 * time.Millisecond, ReadyReconcileInterval: time.Hour,
	}, runner, "token", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	service.startReconciliation()
	waitControllerWorkspaceStatus(t, service, request.ID, "stopped")
	runner.mu.Lock()
	providerStops := runner.stopCalls
	localStops := runner.localStopCalls
	runner.mu.Unlock()
	if providerStops != 1 || localStops != 0 {
		t.Fatalf("provider stops=%d local stops=%d", providerStops, localStops)
	}
}

func TestControllerStartupDurabilityBarrierPrecedesChildRecovery(t *testing.T) {
	newOptions := func(path string) controllerServiceOptions {
		return controllerServiceOptions{
			StateFile: path, MaxConcurrent: 1, Profile: "public-desktop",
			CreateTimeout: time.Second, InspectTimeout: time.Second, StopTimeout: time.Second,
			ConnectionTimeout: time.Second, RetryDelay: 5 * time.Millisecond, ReadyReconcileInterval: time.Hour,
		}
	}

	t.Run("durable snapshot before recovery", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		if err := saveControllerState(path, controllerState{Version: controllerStateVersion, Workspaces: map[string]controllerWorkspaceRecord{}}); err != nil {
			t.Fatal(err)
		}
		events := []string{}
		runner := &startupRecoveryControllerRunner{
			fakeControllerWorkspaceRunner: newFakeControllerWorkspaceRunner(),
			events:                        &events,
			recoverCheck: func(stateFile string) error {
				_, err := loadControllerState(stateFile)
				return err
			},
		}
		stateSaver := func(path string, state controllerState) error {
			events = append(events, "save")
			return saveControllerStateWithDirectorySync(path, state, func(syncPath string) error {
				if filepath.Clean(syncPath) == filepath.Clean(dir) {
					events = append(events, "directory-sync")
				}
				return nil
			})
		}
		ctx, cancel := context.WithCancel(context.Background())
		service, err := newControllerServiceWithStateSaver(ctx, newOptions(path), runner, "token", io.Discard, stateSaver)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		service.waitForShutdown()
		if got, want := strings.Join(events, ","), "save,directory-sync,recover,provider"; got != want {
			t.Fatalf("startup order=%q want=%q", got, want)
		}
	})

	t.Run("directory sync failure blocks recovery", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		if err := saveControllerState(path, controllerState{Version: controllerStateVersion, Workspaces: map[string]controllerWorkspaceRecord{}}); err != nil {
			t.Fatal(err)
		}
		events := []string{}
		runner := &startupRecoveryControllerRunner{fakeControllerWorkspaceRunner: newFakeControllerWorkspaceRunner(), events: &events}
		stateSaver := func(path string, state controllerState) error {
			events = append(events, "save")
			return saveControllerStateWithDirectorySync(path, state, func(syncPath string) error {
				if filepath.Clean(syncPath) == filepath.Clean(dir) {
					events = append(events, "directory-sync")
					return errors.New("persistent startup directory sync failure")
				}
				return nil
			})
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := newControllerServiceWithStateSaver(ctx, newOptions(path), runner, "token", io.Discard, stateSaver)
		if err == nil || !strings.Contains(err.Error(), "establish controller state durability before child recovery") {
			t.Fatalf("startup error=%v", err)
		}
		if got, want := strings.Join(events, ","), "save,directory-sync"; got != want {
			t.Fatalf("startup order=%q want=%q", got, want)
		}
	})
}

func TestControllerRestartReconcilesProvisioningWorkspace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	request := controllerWorkspaceRequest{ID: "restart-box", Profile: "public-desktop"}
	if err := saveControllerState(path, controllerState{
		Version: controllerStateVersion,
		Workspaces: map[string]controllerWorkspaceRecord{
			request.ID: {
				Request: request, ProviderRoute: "external", ProviderScope: "test-provider-scope", CreateStarted: true, CreateObserved: true,
				AttemptLeaseID: "cbx_abcdef123456", LeaseID: "cbx_abcdef123456", Status: "provisioning", Slug: "cbx-ctl-restart-box-0000000000000000", Provider: "external", ProviderResourceID: "resource-123",
				Message: "workspace provisioning", CreatedAt: "2026-06-12T00:00:00Z", UpdatedAt: "2026-06-12T00:00:00Z",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runner := newFakeControllerWorkspaceRunner()
	runner.ready[request.ID] = fakeControllerStatus("cbx-ctl-restart-box-0000000000000000")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := newControllerService(ctx, controllerServiceOptions{
		StateFile: path, MaxConcurrent: 1, Profile: request.Profile,
		CreateTimeout: time.Second, InspectTimeout: time.Second, StopTimeout: time.Second,
		ConnectionTimeout: time.Second,
	}, runner, "token", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	service.startReconciliation()
	waitControllerWorkspaceStatus(t, service, request.ID, "ready")
	warmups, _, _ := runner.counts()
	if warmups != 0 {
		t.Fatalf("restart created a duplicate workspace; warmup calls=%d", warmups)
	}
}

func TestControllerRestartDoesNotTreatUnreadyInspectionAsReady(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	request := controllerWorkspaceRequest{ID: "unready-box", Profile: "public-desktop"}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveControllerState(path, controllerState{
		Version: controllerStateVersion,
		Workspaces: map[string]controllerWorkspaceRecord{
			request.ID: {
				Request: request, ProviderRoute: "external", ProviderScope: "test-provider-scope", CreateStarted: true, CreateObserved: true,
				AttemptLeaseID: "cbx_abcdef123456", LeaseID: "cbx_abcdef123456", Status: "provisioning", Slug: "cbx-ctl-unready-box-0000000000000000", Provider: "external", ProviderResourceID: "resource-123",
				Message: "workspace provisioning", CreatedAt: now, UpdatedAt: now,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runner := newFakeControllerWorkspaceRunner()
	unready := fakeControllerStatus("cbx-ctl-unready-box-0000000000000000")
	unready.Ready = false
	runner.ready[request.ID] = unready
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := newControllerService(ctx, controllerServiceOptions{
		StateFile: path, MaxConcurrent: 1, Profile: request.Profile,
		CreateTimeout: time.Second, InspectTimeout: time.Second, StopTimeout: time.Second,
		ConnectionTimeout: time.Second,
	}, runner, "token", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	service.startReconciliation()
	time.Sleep(100 * time.Millisecond)
	warmups, _, _ := runner.counts()
	if warmups != 0 {
		t.Fatalf("unready inspection started duplicate warmup; calls=%d", warmups)
	}
	if record, _ := service.workspace(request.ID); record.Status != "provisioning" {
		t.Fatalf("status=%q want provisioning", record.Status)
	}
}

func TestControllerRestartUsesPersistedProviderIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	request := controllerWorkspaceRequest{ID: "persisted-route-box", Profile: "public-desktop"}
	status := fakeControllerStatus("cbx-ctl-persisted-route-box-0000000000000000")
	if err := saveControllerState(path, controllerState{
		Version: controllerStateVersion,
		Workspaces: map[string]controllerWorkspaceRecord{
			request.ID: {
				Request: request, ProviderRoute: "external", ProviderScope: "persisted-provider-scope", CreateStarted: true, CreateObserved: true,
				AttemptLeaseID: status.ID, Status: "ready", LeaseID: status.ID, Slug: status.Slug, Provider: "external", ProviderResourceID: status.ServerID,
				Message: "workspace ready", CreatedAt: now, UpdatedAt: now,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runner := newFakeControllerWorkspaceRunner()
	runner.providerRoute = "changed-provider"
	runner.providerScope = "changed-provider-scope"
	runner.ready[request.ID] = status
	ctx, cancel := context.WithCancel(context.Background())
	service, err := newControllerService(ctx, controllerServiceOptions{
		StateFile: path, MaxConcurrent: 1, Profile: request.Profile,
		CreateTimeout: time.Second, InspectTimeout: time.Second, StopTimeout: time.Second,
		ConnectionTimeout: time.Second, ReadyReconcileInterval: time.Hour,
	}, runner, "token", &bytes.Buffer{})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		service.waitForShutdown()
	})
	service.startReconciliation()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		runner.mu.Lock()
		if len(runner.inspectRoutes) > 0 {
			route := runner.inspectRoutes[0]
			scope := runner.inspectScopes[0]
			runner.mu.Unlock()
			if route != "external" {
				t.Fatalf("inspection provider route=%q want persisted external", route)
			}
			if scope != "persisted-provider-scope" {
				t.Fatalf("inspection provider scope=%q want persisted scope", scope)
			}
			return
		}
		runner.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("restart did not inspect workspace")
}

func TestControllerRestartRejectsActiveStateWithoutProviderRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveControllerState(path, controllerState{
		Version: controllerStateVersion,
		Workspaces: map[string]controllerWorkspaceRecord{
			"route-missing-box": {
				Request: controllerWorkspaceRequest{ID: "route-missing-box"}, Status: "provisioning",
				Message: "workspace provisioning", CreatedAt: now, UpdatedAt: now,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := newControllerService(ctx, controllerServiceOptions{
		StateFile: path, MaxConcurrent: 1, CreateTimeout: time.Second, InspectTimeout: time.Second,
		StopTimeout: time.Second, ConnectionTimeout: time.Second,
	}, newFakeControllerWorkspaceRunner(), "token", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no immutable provider route") {
		t.Fatalf("startup error=%v", err)
	}
}

func TestControllerRestartRejectsActiveStateWithoutProviderScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveControllerState(path, controllerState{
		Version: controllerStateVersion,
		Workspaces: map[string]controllerWorkspaceRecord{
			"scope-missing-box": {
				Request: controllerWorkspaceRequest{ID: "scope-missing-box"}, ProviderRoute: "external",
				AttemptLeaseID: "cbx_abcdef123456", Slug: "cbx-ctl-scope-missing-box-0000000000000000", Status: "provisioning",
				Message: "workspace provisioning", CreatedAt: now, UpdatedAt: now,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := newControllerService(ctx, controllerServiceOptions{
		StateFile: path, MaxConcurrent: 1, CreateTimeout: time.Second, InspectTimeout: time.Second,
		StopTimeout: time.Second, ConnectionTimeout: time.Second,
	}, newFakeControllerWorkspaceRunner(), "token", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no immutable provider route and scope") {
		t.Fatalf("startup error=%v", err)
	}
}

func TestControllerRestartRetriesPreparedButUnlaunchedCreate(t *testing.T) {
	service, cancel := testControllerService(t, newFakeControllerWorkspaceRunner(), 1)
	defer cancel()
	now := time.Now().UTC()
	preparedAt := now.Add(-service.opts.CreateTimeout - time.Second).Format(time.RFC3339Nano)
	record := controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "prepared-box", Profile: "public-desktop"}, ProviderRoute: "external", ProviderScope: "test-provider-scope",
		CreatePrepared:   true,
		CreatePreparedAt: preparedAt,
		AttemptLeaseID:   "cbx_123456789abc",
		Status:           "provisioning",
		Slug:             "cbx-ctl-prepared-box-0000000000000000",
		Message:          "workspace provisioning",
		CreatedAt:        now.Format(time.RFC3339Nano),
		UpdatedAt:        preparedAt,
	}
	service.state.Workspaces[record.Request.ID] = record
	service.startReconciliation()
	ready := waitControllerWorkspaceStatus(t, service, record.Request.ID, "ready")
	runner := service.runner.(*fakeControllerWorkspaceRunner)
	warmups, _, _ := runner.counts()
	if warmups != 1 || !ready.CreateStarted {
		t.Fatalf("prepared create was not safely retried: warmups=%d record=%#v", warmups, ready)
	}
}

func TestControllerRestartWaitsForPreparedCreateBeforeRetry(t *testing.T) {
	service, cancel := testControllerService(t, newFakeControllerWorkspaceRunner(), 1)
	defer cancel()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "prepared-wait-box", Profile: "public-desktop"}, ProviderRoute: "external", ProviderScope: "test-provider-scope",
		CreatePrepared:   true,
		CreateStarted:    true,
		CreatePreparedAt: now,
		AttemptLeaseID:   "cbx_123456789abc",
		Status:           "provisioning",
		Slug:             "cbx-ctl-prepared-wait-box-0000000000000000",
		Message:          "workspace provisioning",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	service.state.Workspaces[record.Request.ID] = record
	service.startReconciliation()
	time.Sleep(100 * time.Millisecond)
	runner := service.runner.(*fakeControllerWorkspaceRunner)
	warmups, _, _ := runner.counts()
	if warmups != 0 {
		t.Fatalf("recent prepared create was relaunched before recovery grace: warmups=%d", warmups)
	}
}

func TestControllerRequestedTTLAppliesWhenProviderOmitsExpiry(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	base := time.Now().UTC().Truncate(time.Second)
	service.now = func() time.Time { return base }
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "controller-ttl-box", TTLSeconds: 60})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	record := waitControllerWorkspaceStatus(t, service, "controller-ttl-box", "ready")
	wantExpiry := base.Add(time.Minute).Format(time.RFC3339Nano)
	if record.ControllerExpiresAt != wantExpiry || record.ExpiresAt != wantExpiry {
		t.Fatalf("controller expiry record=%#v want=%s", record, wantExpiry)
	}
	deadline := time.Now().Add(time.Second)
	active := true
	for time.Now().Before(deadline) {
		service.mu.Lock()
		_, active = service.active[record.Request.ID]
		service.mu.Unlock()
		if !active {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if active {
		t.Fatal("create reconciliation did not finish")
	}
	runner.mu.Lock()
	status := runner.ready[record.Request.ID]
	status.ExpiresAt = ""
	runner.ready[record.Request.ID] = status
	runner.mu.Unlock()
	expiredAt := base.Add(61 * time.Second)
	expiredTick := time.Now()
	service.now = func() time.Time { return expiredAt.Add(time.Since(expiredTick)) }
	_ = controllerHTTP(service, http.MethodGet, "/v1/workspaces/controller-ttl-box", "test-token", nil)
	waitControllerWorkspaceStatus(t, service, record.Request.ID, "expired")
	_, stops, _ := runner.counts()
	if stops != 1 {
		t.Fatalf("controller TTL cleanup stops=%d", stops)
	}
}

func TestControllerRequestedTTLAppliesDespiteMismatchedInspection(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	now := time.Now().UTC()
	record := controllerWorkspaceRecord{
		Request:             controllerWorkspaceRequest{ID: "mismatched-expired-box", TTLSeconds: 60},
		ProviderRoute:       "external",
		ProviderScope:       "test-provider-scope",
		CreatePrepared:      true,
		CreateStarted:       true,
		CreateObserved:      true,
		AttemptLeaseID:      "cbx_123456789abc",
		Status:              "ready",
		LeaseID:             "cbx_123456789abc",
		Slug:                "cbx-ctl-mismatched-expired-box-0000000000000000",
		Provider:            "external",
		ProviderResourceID:  "resource-123",
		Message:             "workspace ready",
		ExpiresAt:           now.Add(-time.Minute).Format(time.RFC3339Nano),
		ControllerExpiresAt: now.Add(-time.Minute).Format(time.RFC3339Nano),
		CreatedAt:           now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
		UpdatedAt:           now.Format(time.RFC3339Nano),
	}
	service.state.Workspaces[record.Request.ID] = record
	mismatched := fakeControllerStatus(record.Slug)
	mismatched.ID = "cbx_ffffffffffff"
	runner.ready[record.Request.ID] = mismatched
	service.reconcileReady(record)
	expired := waitControllerWorkspaceStatus(t, service, record.Request.ID, "expired")
	_, stops, _ := runner.counts()
	runner.mu.Lock()
	inspects := runner.inspectCalls
	runner.mu.Unlock()
	if stops != 1 || inspects != 0 || expired.LeaseID != record.LeaseID {
		t.Fatalf("mismatched expiry cleanup stops=%d inspections=%d record=%#v", stops, inspects, expired)
	}
}

func TestControllerRequestedTTLStopsProvisioningWorkspace(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	now := time.Now().UTC()
	record := controllerWorkspaceRecord{
		Request:             controllerWorkspaceRequest{ID: "provisioning-ttl-box", TTLSeconds: 60},
		ProviderRoute:       "external",
		ProviderScope:       "test-provider-scope",
		CreatePrepared:      true,
		CreateStarted:       true,
		CreateObserved:      true,
		AttemptLeaseID:      "cbx_123456789abc",
		Status:              "provisioning",
		LeaseID:             "cbx_123456789abc",
		Slug:                "cbx-ctl-provisioning-ttl-box-0000000000000000",
		Provider:            "external",
		ProviderResourceID:  "resource-123",
		Message:             "workspace provisioning",
		ExpiresAt:           now.Add(-time.Second).Format(time.RFC3339Nano),
		ControllerExpiresAt: now.Add(-time.Second).Format(time.RFC3339Nano),
		CreatedAt:           now.Add(-time.Minute).Format(time.RFC3339Nano),
		UpdatedAt:           now.Format(time.RFC3339Nano),
	}
	service.state.Workspaces[record.Request.ID] = record
	service.reconcileProvisioning(record)
	expired := waitControllerWorkspaceStatus(t, service, record.Request.ID, "expired")
	_, stops, _ := runner.counts()
	if stops != 1 || expired.LeaseID != record.LeaseID {
		t.Fatalf("provisioning TTL cleanup stops=%d record=%#v", stops, expired)
	}
}

func TestControllerRestartDoesNotReacquireMissingKnownLease(t *testing.T) {
	service, cancel := testControllerService(t, newFakeControllerWorkspaceRunner(), 1)
	defer cancel()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "missing-known-box", Profile: "public-desktop"}, ProviderRoute: "external", ProviderScope: "test-provider-scope",
		CreateStarted: true, CreateObserved: true, AttemptLeaseID: "cbx_missingknown123",
		Status: "provisioning", LeaseID: "cbx_missingknown123", Slug: "cbx-ctl-missing-known-box-0000000000000000",
		Provider: "external", ProviderResourceID: "provider/missing-known", Message: "workspace provisioning",
		CreatedAt: now, UpdatedAt: now,
	}
	service.state.Workspaces[record.Request.ID] = record
	service.startReconciliation()
	waitControllerWorkspaceStatus(t, service, record.Request.ID, "failed")
	runner := service.runner.(*fakeControllerWorkspaceRunner)
	warmups, stops, _ := runner.counts()
	if warmups != 0 || stops != 1 {
		t.Fatalf("missing known lease reacquired or skipped cleanup warmups=%d stops=%d", warmups, stops)
	}
}

func TestControllerRestartDoesNotReacquireExpiredMissingSlug(t *testing.T) {
	service, cancel := testControllerService(t, newFakeControllerWorkspaceRunner(), 1)
	defer cancel()
	old := time.Now().UTC().Add(-service.opts.CreateTimeout - time.Minute).Format(time.RFC3339Nano)
	record := controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "missing-expired-box", Profile: "public-desktop"}, ProviderRoute: "external", ProviderScope: "test-provider-scope",
		CreateStarted: true, CreateObserved: true, AttemptLeaseID: "cbx_missingexpired123", LeaseID: "cbx_missingexpired123",
		Status: "provisioning", Slug: "cbx-ctl-missing-expired-box-0000000000000000", Provider: "external", ProviderResourceID: "provider/missing-expired",
		Message: "workspace provisioning", CreatedAt: old, UpdatedAt: old,
	}
	service.state.Workspaces[record.Request.ID] = record
	service.startReconciliation()
	waitControllerWorkspaceStatus(t, service, record.Request.ID, "failed")
	runner := service.runner.(*fakeControllerWorkspaceRunner)
	warmups, stops, _ := runner.counts()
	if warmups != 0 || stops != 1 {
		t.Fatalf("expired missing slug reacquired or skipped cleanup warmups=%d stops=%d", warmups, stops)
	}
}

func TestControllerRestartRetriesMissingUnobservedStartedAttempt(t *testing.T) {
	service, cancel := testControllerService(t, newFakeControllerWorkspaceRunner(), 1)
	defer cancel()
	now := time.Now().UTC()
	old := now.Add(-service.opts.CreateTimeout - time.Second).Format(time.RFC3339Nano)
	record := controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "missing-started-box", Profile: "public-desktop"}, ProviderRoute: "external", ProviderScope: "test-provider-scope",
		CreatePrepared:   true,
		CreatePreparedAt: old,
		CreateStarted:    true,
		AttemptLeaseID:   "cbx_123456789abc",
		Status:           "provisioning",
		Slug:             "cbx-ctl-missing-started-box-0000000000000000",
		Message:          "workspace provisioning",
		CreatedAt:        now.Format(time.RFC3339Nano),
		UpdatedAt:        old,
	}
	service.state.Workspaces[record.Request.ID] = record
	service.startReconciliation()
	ready := waitControllerWorkspaceStatus(t, service, record.Request.ID, "ready")
	runner := service.runner.(*fakeControllerWorkspaceRunner)
	warmups, stops, _ := runner.counts()
	if warmups != 1 || stops != 0 || ready.LeaseID == "" {
		t.Fatalf("missing unobserved attempt was not retried idempotently: warmups=%d stops=%d record=%#v", warmups, stops, ready)
	}
}

func TestControllerUnacknowledgedAttemptRetriesAcquireWithoutFirstAdoptingInspection(t *testing.T) {
	service, cancel := testControllerService(t, newFakeControllerWorkspaceRunner(), 1)
	defer cancel()
	service.opts.RetryDelay = time.Hour
	now := time.Now().UTC()
	old := now.Add(-service.opts.CreateTimeout - time.Second).Format(time.RFC3339Nano)
	record := controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "identity-box", Profile: "public-desktop"}, ProviderRoute: "external", ProviderScope: "test-provider-scope",
		CreatePrepared: true, CreatePreparedAt: old, CreateStarted: true, AttemptLeaseID: "cbx_123456789abc", Status: "provisioning", Slug: "cbx-ctl-identity-box-0000000000000000",
		Message: "workspace provisioning", CreatedAt: now.Format(time.RFC3339Nano), UpdatedAt: old,
	}
	service.state.Workspaces[record.Request.ID] = record
	runner := service.runner.(*fakeControllerWorkspaceRunner)
	runner.ready[record.Request.ID] = fakeControllerStatus(record.Slug)
	service.reconcileProvisioning(record)
	current, ok := service.workspace(record.Request.ID)
	if !ok || current.Status != "provisioning" || !controllerRecordHasAcquiredIdentity(current) || current.LeaseID != record.AttemptLeaseID {
		t.Fatalf("fixed acquire retry did not persist exact raw identity: %#v", current)
	}
	warmups, stops, _ := runner.counts()
	runner.mu.Lock()
	inspects := runner.inspectCalls
	runner.mu.Unlock()
	if warmups != 1 || stops != 0 || inspects != 0 {
		t.Fatalf("unacknowledged retry warmups=%d stops=%d inspections=%d", warmups, stops, inspects)
	}
}

func TestControllerStatusMatchesEveryPersistedIdentity(t *testing.T) {
	record := controllerWorkspaceRecord{
		ProviderRoute: "external", CreateObserved: true, LeaseID: "cbx_identity123", AttemptLeaseID: "cbx_identity123",
		Slug: "stable-slug", ProviderResourceID: "provider/resource-123",
	}
	matching := StatusView{ID: record.LeaseID, Slug: record.Slug, Provider: record.ProviderRoute, ServerID: record.ProviderResourceID}
	if !controllerStatusMatchesRecord(record, matching) {
		t.Fatal("complete matching provider identity was rejected")
	}
	for name, mutate := range map[string]func(*StatusView){
		"lease":    func(status *StatusView) { status.ID = "cbx_other" },
		"slug":     func(status *StatusView) { status.Slug = "other-slug" },
		"provider": func(status *StatusView) { status.Provider = " external " },
		"resource": func(status *StatusView) { status.ServerID = "provider/other" },
	} {
		t.Run(name, func(t *testing.T) {
			status := matching
			mutate(&status)
			if controllerStatusMatchesRecord(record, status) {
				t.Fatalf("%s mismatch was accepted: %#v", name, status)
			}
		})
	}
	record.AttemptLeaseID = "cbx_different_attempt"
	if controllerStatusMatchesRecord(record, matching) {
		t.Fatal("attempt identity differing from the observed lease was accepted")
	}
}

func TestControllerReadyIdentityMismatchTransitionsToExpectedCleanup(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	service.opts.RetryDelay = time.Hour
	now := time.Now().UTC()
	record := controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "ready-identity-mismatch-box"}, ProviderRoute: "external", ProviderScope: "test-provider-scope",
		CreateStarted: true, CreateObserved: true, AttemptLeaseID: "cbx_readymismatch123", Status: "ready", LeaseID: "cbx_readymismatch123",
		Slug: "ready-identity-mismatch", Provider: "external", ProviderResourceID: "provider/ready-identity-mismatch",
		Host: "192.0.2.44", AttachURL: "wss://terminal.example.test/workspaces/ready-identity-mismatch-box", Message: "workspace ready",
		ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano), CreatedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano),
	}
	service.state.Workspaces[record.Request.ID] = record
	mismatched := fakeControllerStatus("replacement-slug")
	mismatched.ID = "cbx_replacement123"
	mismatched.ServerID = "provider/replacement"
	runner.ready[record.Request.ID] = mismatched

	service.reconcileReady(record)
	current, _ := service.workspace(record.Request.ID)
	if current.Status != "stopping" || current.Host != "" || current.AttachURL != "" {
		t.Fatalf("ready identity mismatch retained connection metadata: %#v", current)
	}
	if current.LeaseID != record.LeaseID || current.AttemptLeaseID != record.AttemptLeaseID || current.Slug != record.Slug || current.ProviderResourceID != record.ProviderResourceID {
		t.Fatalf("ready identity mismatch changed persisted identity: before=%#v after=%#v", record, current)
	}
	if current.StatusAfterCleanup != "failed" || !strings.Contains(current.FailureAfterCleanup, "identity mismatch") {
		t.Fatalf("ready identity mismatch cleanup target=%#v", current)
	}
	runner.mu.Lock()
	stopCalls, localStops := runner.stopCalls, runner.localStopCalls
	stopIdentifier := ""
	if len(runner.stopIdentifiers) > 0 {
		stopIdentifier = runner.stopIdentifiers[0]
	}
	runner.mu.Unlock()
	if stopCalls != 1 || localStops != 1 || stopIdentifier != record.LeaseID {
		t.Fatalf("cleanup provider stops=%d local stops=%d identifier=%q", stopCalls, localStops, stopIdentifier)
	}
}

func TestControllerReadyAdoptionNeverOverwritesImmutableIdentity(t *testing.T) {
	service, cancel := testControllerService(t, newFakeControllerWorkspaceRunner(), 1)
	defer cancel()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "immutable-ready-box"}, ProviderRoute: "external", ProviderScope: "scope",
		Status: "provisioning", CreateObserved: true, LeaseID: "cbx_immutable123", AttemptLeaseID: "cbx_immutable123",
		Slug: "immutable-slug", ProviderResourceID: "provider/immutable", CreatedAt: now, UpdatedAt: now,
	}
	service.state.Workspaces[record.Request.ID] = record
	status := StatusView{
		ID: record.LeaseID, Slug: "replacement-slug", Provider: "external", ServerID: "provider/replacement",
		State: "ready", Ready: true,
	}
	if err := service.markReady(record.Request.ID, status); err == nil {
		t.Fatal("ready adoption accepted replacement immutable identities")
	}
	current, _ := service.workspace(record.Request.ID)
	if current.Slug != record.Slug || current.ProviderResourceID != record.ProviderResourceID || current.Status != record.Status {
		t.Fatalf("immutable identity changed after rejected adoption: %#v", current)
	}
}

func TestControllerIdentityMismatchWithoutFixedAttemptDoesNotCleanup(t *testing.T) {
	service, cancel := testControllerService(t, newFakeControllerWorkspaceRunner(), 1)
	defer cancel()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := controllerWorkspaceRecord{
		Request:       controllerWorkspaceRequest{ID: "legacy-identity-box", Profile: "public-desktop"},
		CreateStarted: true, Status: "provisioning", Slug: "cbx-ctl-legacy-identity-box-0000000000000000",
		Message: "workspace provisioning", CreatedAt: now, UpdatedAt: now,
	}
	service.state.Workspaces[record.Request.ID] = record
	runner := service.runner.(*fakeControllerWorkspaceRunner)
	runner.ready[record.Request.ID] = fakeControllerStatus("different-slug")
	service.startReconciliation()
	failed := waitControllerWorkspaceStatus(t, service, record.Request.ID, "failed")
	warmups, stops, _ := runner.counts()
	if warmups != 0 || stops != 0 || !strings.Contains(failed.Message, "cleanup not attempted") {
		t.Fatalf("legacy identity mismatch warmups=%d stops=%d record=%#v", warmups, stops, failed)
	}
}

func TestControllerReadyTransitionPreservesConcurrentStop(t *testing.T) {
	service, cancel := testControllerService(t, newFakeControllerWorkspaceRunner(), 1)
	defer cancel()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	service.state.Workspaces["stopping-box"] = controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "stopping-box"}, ProviderRoute: "external", ProviderScope: "scope", CreateObserved: true,
		Status: "stopping", LeaseID: "cbx_abcdef123456", AttemptLeaseID: "cbx_abcdef123456", Slug: "stopping-box", ProviderResourceID: "resource-123",
		Message: "workspace stopping", CreatedAt: now, UpdatedAt: now,
	}
	_ = service.markReady("stopping-box", fakeControllerStatus("stopping-box"))
	record, ok := service.workspace("stopping-box")
	if !ok {
		t.Fatal("workspace disappeared")
	}
	if record.Status != "stopping" || record.LeaseID == "" {
		t.Fatalf("record=%#v", record)
	}
}

func TestControllerBoundsLifecycleConcurrency(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.started = make(chan string, 2)
	runner.blockWarmup = make(chan struct{})
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	for _, id := range []string{"first-box", "second-box"} {
		response := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: id})
		if response.Code != http.StatusAccepted {
			t.Fatalf("create %s status=%d body=%s", id, response.Code, response.Body.String())
		}
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("first warmup did not start")
	}
	select {
	case id := <-runner.started:
		t.Fatalf("second warmup %s started before concurrency slot was released", id)
	case <-time.After(100 * time.Millisecond):
	}
	close(runner.blockWarmup)
	waitControllerWorkspaceStatus(t, service, "first-box", "ready")
	waitControllerWorkspaceStatus(t, service, "second-box", "ready")
}

func TestControllerRejectsUnconfiguredProfileAndCapability(t *testing.T) {
	service, cancel := testControllerService(t, newFakeControllerWorkspaceRunner(), 1)
	defer cancel()
	for _, request := range []controllerWorkspaceRequest{
		{ID: "bad-profile", Profile: "other"},
		{ID: "bad-capability", Capabilities: controllerCapabilities{Desktop: true}},
	} {
		if request.ID == "bad-capability" {
			service.opts.Allowed.Desktop = false
		}
		response := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("request=%s status=%d body=%s", request.ID, response.Code, response.Body.String())
		}
	}
}

func TestControllerProviderIdentityDiscoveryUsesBoundedServiceContext(t *testing.T) {
	runner := &blockingProviderIdentityRunner{fakeControllerWorkspaceRunner: newFakeControllerWorkspaceRunner(), started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startedAt := time.Now()
	_, err := newControllerService(ctx, controllerServiceOptions{
		StateFile: filepath.Join(t.TempDir(), "controller.json"), MaxConcurrent: 1,
		CreateTimeout: time.Second, InspectTimeout: 50 * time.Millisecond,
		StopTimeout: time.Second, ConnectionTimeout: time.Second,
	}, runner, "token", io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("provider discovery error=%v", err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("provider discovery exceeded bounded startup: %s", elapsed)
	}
	select {
	case <-runner.started:
	default:
		t.Fatal("provider discovery was not invoked")
	}
}

func TestControllerRejectsUnsafeDesktopURL(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.desktopURL = "http://public.example.test/desktop"
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	service.opts.VNCURLTemplate = ""
	response := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{
		ID: "desktop-box", Capabilities: controllerCapabilities{Desktop: true},
	})
	if response.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", response.Code)
	}
	waitControllerWorkspaceStatus(t, service, "desktop-box", "ready")
	connection := controllerHTTP(service, http.MethodPost, "/v1/workspaces/desktop-box/connections/desktop", "test-token", nil)
	if connection.Code != http.StatusBadGateway {
		t.Fatalf("connection status=%d body=%s", connection.Code, connection.Body.String())
	}
}

func TestControllerRetriesFailedDesktopRevocationWhileWorkspaceRemainsReady(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.connectionErr = errors.New("desktop setup failed")
	runner.localStopErr = errors.New("local cleanup failed")
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	response := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{
		ID: "desktop-cleanup-retry-box", Capabilities: controllerCapabilities{Desktop: true},
	})
	if response.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", response.Code)
	}
	waitControllerWorkspaceStatus(t, service, "desktop-cleanup-retry-box", "ready")
	connection := controllerHTTP(service, http.MethodPost, "/v1/workspaces/desktop-cleanup-retry-box/connections/desktop", "test-token", nil)
	if connection.Code != http.StatusBadGateway {
		t.Fatalf("connection status=%d body=%s", connection.Code, connection.Body.String())
	}
	record, ok := service.workspace("desktop-cleanup-retry-box")
	if !ok || record.Status != "ready" || !record.LocalCleanupPending {
		t.Fatalf("failed desktop revocation was not persisted for retry: %#v", record)
	}
	runner.mu.Lock()
	runner.localStopErr = nil
	runner.mu.Unlock()
	service.reconcileReady(record)
	record, _ = service.workspace(record.Request.ID)
	runner.mu.Lock()
	localStops := runner.localStopCalls
	runner.mu.Unlock()
	if record.Status != "ready" || record.LocalCleanupPending || localStops < 2 {
		t.Fatalf("desktop cleanup retry record=%#v localStops=%d", record, localStops)
	}
}

func TestControllerReadyLocalCleanupRunsBeforeDurabilityBarrier(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	status := fakeControllerStatus("durability-local-cleanup")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := controllerWorkspaceRecord{
		Request:       controllerWorkspaceRequest{ID: "durability-local-cleanup", Profile: "public-desktop"},
		ProviderRoute: "external", ProviderScope: "test-provider-scope",
		CreateStarted: true, CreateObserved: true, AttemptLeaseID: status.ID,
		LeaseID: status.ID, Slug: status.Slug, ProviderResourceID: status.ServerID,
		Status: "ready", LocalCleanupPending: true, CreatedAt: now, UpdatedAt: now,
	}
	service.state.Workspaces[record.Request.ID] = record
	runner.ready[record.Request.ID] = status
	service.mu.Lock()
	service.durabilityPending = true
	service.mu.Unlock()
	service.saveState = func(string, controllerState) error {
		return &controllerStateInstalledError{err: errors.New("persistent state directory sync failure")}
	}
	service.reconcile(record.Request.ID)
	runner.mu.Lock()
	localStops := runner.localStopCalls
	inspectCalls := runner.inspectCalls
	runner.mu.Unlock()
	if localStops != 1 || inspectCalls != 0 {
		t.Fatalf("durability barrier cleanup local=%d provider inspections=%d", localStops, inspectCalls)
	}
	current, _ := service.workspace(record.Request.ID)
	if current.LocalCleanupPending {
		t.Fatalf("successful local cleanup was not retained in installed state: %#v", current)
	}
	service.mu.Lock()
	pending := service.durabilityPending
	service.mu.Unlock()
	if !pending {
		t.Fatal("persistent sync failure unexpectedly cleared durability barrier")
	}
}

func TestControllerTerminalTransitionWriteFailureRevokesDesktopBeforeProvider(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	service.opts.RetryDelay = time.Hour
	status := fakeControllerStatus("terminal-write-failure")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{
			ID: "terminal-write-failure", Profile: "public-desktop", Capabilities: controllerCapabilities{Desktop: true},
		},
		ProviderRoute: "external", ProviderScope: "test-provider-scope",
		CreateStarted: true, CreateObserved: true, AttemptLeaseID: status.ID,
		LeaseID: status.ID, Slug: status.Slug, ProviderResourceID: status.ServerID,
		Status: "ready", CreatedAt: now, UpdatedAt: now,
	}
	service.state.Workspaces[record.Request.ID] = record
	service.saveState = func(string, controllerState) error { return errors.New("state storage unavailable") }

	service.cleanupReadyWorkspace(record, now, "expired", "workspace expired")

	runner.mu.Lock()
	localStops := runner.localStopCalls
	providerStops := runner.stopCalls
	runner.mu.Unlock()
	current, _ := service.workspace(record.Request.ID)
	if localStops != 1 || providerStops != 0 {
		t.Fatalf("failed terminal write cleanup local=%d provider=%d", localStops, providerStops)
	}
	if current.Status != "ready" || current.LocalCleanupPending {
		t.Fatalf("failed terminal write mutated durable state: %#v", current)
	}
	if !service.hasLocalCleanupRetry(record.Request.ID) {
		t.Fatal("failed terminal write did not retain memory-backed local revocation retry")
	}
	response := controllerHTTP(service, http.MethodPost, "/v1/workspaces/terminal-write-failure/connections/desktop", "test-token", nil)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("desktop reopened through memory-backed terminal barrier: status=%d body=%s", response.Code, response.Body.String())
	}
	runner.mu.Lock()
	connections := runner.connectionCalls
	runner.mu.Unlock()
	if connections != 0 {
		t.Fatalf("desktop setup ran %d times after terminal write failure", connections)
	}

	service.reconcileReady(current)
	runner.mu.Lock()
	inspections := runner.inspectCalls
	connections = runner.connectionCalls
	runner.mu.Unlock()
	if inspections != 0 || connections != 0 {
		t.Fatalf("terminal barrier was abandoned after local revoke: inspections=%d connections=%d", inspections, connections)
	}
	if _, pending := service.terminalRevocationFor(record.Request.ID); !pending || !service.hasLocalCleanupRetry(record.Request.ID) {
		t.Fatal("terminal revocation intent was cleared while stopping state was not durable")
	}

	service.saveState = saveControllerState
	service.reconcileReady(current)
	persisted, _ := service.workspace(record.Request.ID)
	if persisted.Status != "stopping" {
		t.Fatalf("terminal intent was not persisted after storage recovery: %#v", persisted)
	}
	if _, pending := service.terminalRevocationFor(record.Request.ID); pending {
		t.Fatal("terminal revocation barrier remained after stopping state persisted")
	}
}

func TestControllerEnqueuesDesktopRevocationWhenPendingStateWriteFails(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	runner.connectionErr = errors.New("desktop setup failed")
	runner.localStopErr = errors.New("local cleanup failed")
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	response := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{
		ID: "desktop-cleanup-memory-retry-box", Capabilities: controllerCapabilities{Desktop: true},
	})
	if response.Code != http.StatusAccepted {
		t.Fatalf("create status=%d", response.Code)
	}
	waitControllerWorkspaceStatus(t, service, "desktop-cleanup-memory-retry-box", "ready")
	originalSave := service.saveState
	service.saveState = func(string, controllerState) error { return errors.New("state storage unavailable") }
	connection := controllerHTTP(service, http.MethodPost, "/v1/workspaces/desktop-cleanup-memory-retry-box/connections/desktop", "test-token", nil)
	if connection.Code != http.StatusBadGateway {
		t.Fatalf("connection status=%d body=%s", connection.Code, connection.Body.String())
	}
	record, _ := service.workspace("desktop-cleanup-memory-retry-box")
	if record.LocalCleanupPending || !service.hasLocalCleanupRetry(record.Request.ID) {
		t.Fatalf("failed state write did not retain memory-backed cleanup retry: %#v", record)
	}
	service.saveState = originalSave
	runner.mu.Lock()
	runner.localStopErr = nil
	runner.mu.Unlock()
	service.reconcileReady(record)
	if service.hasLocalCleanupRetry(record.Request.ID) {
		t.Fatal("memory-backed cleanup retry was not cleared")
	}
}

func TestSaveControllerStateCreatesAndReplacesNestedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "controller", "state.json")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	state := controllerState{
		Version: controllerStateVersion,
		Workspaces: map[string]controllerWorkspaceRecord{
			"durable-box": {
				Request: controllerWorkspaceRequest{ID: "durable-box"}, Status: "ready", Message: "first",
				CreatedAt: now, UpdatedAt: now,
			},
		},
	}
	if err := saveControllerState(path, state); err != nil {
		t.Fatal(err)
	}
	state.Workspaces["durable-box"] = controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "durable-box"}, Status: "stopped", Message: "second",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := saveControllerState(path, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadControllerState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Workspaces["durable-box"]; got.Status != "stopped" || got.Message != "second" {
		t.Fatalf("loaded state=%#v", got)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode info=%v err=%v", info, err)
	}
}

func TestSaveControllerStateRetriesExistingParentChainAfterSyncFailure(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "first", "second")
	path := filepath.Join(dir, "state.json")
	failedParent := filepath.Join(base, "first")
	root := filepath.VolumeName(path) + string(os.PathSeparator)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	state := controllerState{
		Version: controllerStateVersion,
		Workspaces: map[string]controllerWorkspaceRecord{
			"parent-sync-box": {
				Request: controllerWorkspaceRequest{ID: "parent-sync-box"}, Status: "stopped", Message: "durable",
				CreatedAt: now, UpdatedAt: now,
			},
		},
	}
	calls := map[string]int{}
	syncErr := errors.New("parent directory sync unavailable")
	err := saveControllerStateWithDirectorySync(path, state, func(syncPath string) error {
		syncPath = filepath.Clean(syncPath)
		calls[syncPath]++
		if syncPath == filepath.Clean(failedParent) {
			return syncErr
		}
		return nil
	})
	if err == nil || controllerStateInstalled(err) || !errors.Is(err, syncErr) {
		t.Fatalf("first save error=%v", err)
	}
	err = saveControllerStateWithDirectorySync(path, state, func(syncPath string) error {
		syncPath = filepath.Clean(syncPath)
		calls[syncPath]++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls[filepath.Clean(failedParent)] != 2 {
		t.Fatalf("failed parent sync calls=%d want=2; all=%v", calls[filepath.Clean(failedParent)], calls)
	}
	if calls[filepath.Clean(root)] == 0 {
		t.Fatalf("filesystem root was not included in parent sync chain: %v", calls)
	}
	loaded, err := loadControllerState(path)
	if err != nil || loaded.Workspaces["parent-sync-box"].Message != "durable" {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}
}

func TestSaveControllerStateReportsInstalledAfterDirectorySyncFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	state := controllerState{
		Version: controllerStateVersion,
		Workspaces: map[string]controllerWorkspaceRecord{
			"installed-box": {
				Request: controllerWorkspaceRequest{ID: "installed-box"}, Status: "stopped", Message: "installed",
				CreatedAt: now, UpdatedAt: now,
			},
		},
	}
	syncErr := errors.New("directory sync unavailable")
	err := saveControllerStateWithDirectorySync(path, state, func(syncPath string) error {
		if filepath.Clean(syncPath) == filepath.Clean(dir) {
			return syncErr
		}
		return nil
	})
	if !controllerStateInstalled(err) || !errors.Is(err, syncErr) {
		t.Fatalf("save error=%v, want installed-state error wrapping sync failure", err)
	}
	loaded, loadErr := loadControllerState(path)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got := loaded.Workspaces["installed-box"]; got.Status != "stopped" || got.Message != "installed" {
		t.Fatalf("installed state=%#v", got)
	}
}

func TestControllerKeepsMemoryAfterInstalledStateSyncFailure(t *testing.T) {
	service, cancel := testControllerService(t, newFakeControllerWorkspaceRunner(), 1)
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(service.opts.StateFile), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	service.state.Workspaces["installed-memory-box"] = controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "installed-memory-box"}, Status: "ready", Message: "ready",
		CreatedAt: now, UpdatedAt: now,
	}
	syncErr := errors.New("directory sync unavailable")
	service.saveState = func(path string, state controllerState) error {
		dir := filepath.Dir(path)
		return saveControllerStateWithDirectorySync(path, state, func(syncPath string) error {
			if filepath.Clean(syncPath) == filepath.Clean(dir) {
				return syncErr
			}
			return nil
		})
	}
	if err := service.markFailed("installed-memory-box", "failed after commit"); err != nil {
		t.Fatalf("installed state treated as uncommitted: %v", err)
	}
	record, ok := service.workspace("installed-memory-box")
	if !ok || record.Status != "failed" || record.Message != "failed after commit" {
		t.Fatalf("memory rolled back after installed state: %#v", record)
	}
	loaded, err := loadControllerState(service.opts.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Workspaces["installed-memory-box"]; got.Status != record.Status || got.Message != record.Message {
		t.Fatalf("disk=%#v memory=%#v", got, record)
	}
	service.mu.Lock()
	pending := service.durabilityPending
	service.mu.Unlock()
	if !pending {
		t.Fatal("installed state sync failure did not raise the provider-side-effect durability barrier")
	}
}

func TestControllerStateLockRejectsDuplicateBeforeStateMutation(t *testing.T) {
	service, cancel := testControllerService(t, newFakeControllerWorkspaceRunner(), 1)
	var saveCalls int
	duplicateCtx, cancelDuplicate := context.WithCancel(context.Background())
	_, err := newControllerServiceWithStateSaver(
		duplicateCtx,
		service.opts,
		newFakeControllerWorkspaceRunner(),
		"duplicate-token",
		io.Discard,
		func(string, controllerState) error {
			saveCalls++
			return nil
		},
	)
	cancelDuplicate()
	if err == nil || !strings.Contains(err.Error(), "already locked") {
		cancel()
		service.waitForShutdown()
		t.Fatalf("duplicate controller error=%v", err)
	}
	if saveCalls != 0 {
		cancel()
		service.waitForShutdown()
		t.Fatalf("duplicate controller mutated shared state: saves=%d", saveCalls)
	}
	cancel()
	service.waitForShutdown()

	restartCtx, cancelRestart := context.WithCancel(context.Background())
	restarted, err := newControllerService(restartCtx, service.opts, newFakeControllerWorkspaceRunner(), "restart-token", io.Discard)
	if err != nil {
		cancelRestart()
		t.Fatalf("state lock was not released after shutdown: %v", err)
	}
	cancelRestart()
	restarted.waitForShutdown()
}

func TestControllerBlocksProviderUntilInstalledStateBecomesDurable(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(service.opts.StateFile), 0o700); err != nil {
		t.Fatal(err)
	}
	var syncState struct {
		sync.Mutex
		fail     bool
		attempts int
	}
	syncState.fail = true
	service.saveState = func(path string, state controllerState) error {
		syncState.Lock()
		syncState.attempts++
		fail := syncState.fail
		syncState.Unlock()
		dir := filepath.Dir(path)
		return saveControllerStateWithDirectorySync(path, state, func(syncPath string) error {
			if fail && filepath.Clean(syncPath) == filepath.Clean(dir) {
				return errors.New("persistent directory sync failure")
			}
			return nil
		})
	}
	response := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "durability-barrier-box"})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Retry-After") != "1" || !strings.Contains(response.Body.String(), "state_durability_pending") {
		t.Fatalf("create durability response headers=%v body=%s", response.Header(), response.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		syncState.Lock()
		attempts := syncState.attempts
		syncState.Unlock()
		if attempts >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	syncState.Lock()
	attempts := syncState.attempts
	syncState.Unlock()
	if attempts < 3 {
		t.Fatalf("durability retry attempts=%d want at least 3", attempts)
	}
	time.Sleep(25 * time.Millisecond)
	warmups, _, _ := runner.counts()
	if warmups != 0 {
		t.Fatalf("provider warmup crossed persistent durability barrier: %d", warmups)
	}
	record, ok := service.workspace("durability-barrier-box")
	if !ok || record.Status != "provisioning" || record.CreatePrepared {
		t.Fatalf("provider intent advanced while durability pending: %#v", record)
	}
	loaded, err := loadControllerState(service.opts.StateFile)
	if err != nil || loaded.Workspaces[record.Request.ID].Status != "provisioning" {
		t.Fatalf("installed state missing from disk: %#v err=%v", loaded, err)
	}

	syncState.Lock()
	syncState.fail = false
	syncState.Unlock()
	ready := waitControllerWorkspaceStatus(t, service, record.Request.ID, "ready")
	warmups, _, _ = runner.counts()
	if warmups != 1 || !ready.CreateObserved {
		t.Fatalf("provider did not start exactly once after durability recovery: warmups=%d record=%#v", warmups, ready)
	}
	service.mu.Lock()
	pending := service.durabilityPending
	service.mu.Unlock()
	if pending {
		t.Fatal("durability barrier remained after successful retry")
	}
}

func TestControllerCreateRetryPreservesIdentityUntilDurableAcknowledgment(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	service.sem <- struct{}{}
	released := false
	t.Cleanup(func() {
		cancel()
		service.waitForShutdown()
		if !released {
			<-service.sem
		}
	})
	if err := os.MkdirAll(filepath.Dir(service.opts.StateFile), 0o700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	fail := true
	service.saveState = func(path string, state controllerState) error {
		mu.Lock()
		shouldFail := fail
		mu.Unlock()
		dir := filepath.Dir(path)
		return saveControllerStateWithDirectorySync(path, state, func(syncPath string) error {
			if shouldFail && filepath.Clean(syncPath) == filepath.Clean(dir) {
				return errors.New("directory sync unavailable")
			}
			return nil
		})
	}
	request := controllerWorkspaceRequest{ID: "create-durability-retry-box"}
	first := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", request)
	if first.Code != http.StatusServiceUnavailable {
		t.Fatalf("first create status=%d body=%s", first.Code, first.Body.String())
	}
	pending, _ := service.workspace(request.ID)
	second := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", request)
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second create status=%d body=%s", second.Code, second.Body.String())
	}
	if current, _ := service.workspace(request.ID); current.AttemptLeaseID != pending.AttemptLeaseID || current.Slug != pending.Slug {
		t.Fatalf("retry changed provider identity: before=%#v after=%#v", pending, current)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	third := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", request)
	if third.Code != http.StatusAccepted {
		t.Fatalf("durable create retry status=%d body=%s", third.Code, third.Body.String())
	}
	if current, _ := service.workspace(request.ID); current.AttemptLeaseID != pending.AttemptLeaseID || current.Slug != pending.Slug {
		t.Fatalf("durable retry changed provider identity: before=%#v after=%#v", pending, current)
	}
	<-service.sem
	released = true
	waitControllerWorkspaceStatus(t, service, request.ID, "ready")
	warmups, _, _ := runner.counts()
	if warmups != 1 {
		t.Fatalf("warmups=%d", warmups)
	}
}

func TestControllerDeleteRetryPreservesCleanupStateUntilDurableAcknowledgment(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	service.sem <- struct{}{}
	released := false
	t.Cleanup(func() {
		cancel()
		service.waitForShutdown()
		if !released {
			<-service.sem
		}
	})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	id := "delete-durability-retry-box"
	status := fakeControllerStatus("delete-durability-retry")
	status.ID = "cbx_deletedurable123"
	status.Provider = "external"
	status.ServerID = "provider/delete-durable"
	record := controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: id}, ProviderRoute: "external", ProviderScope: "test-provider-scope",
		CreateStarted: true, CreateObserved: true, AttemptLeaseID: status.ID, Status: "ready", LeaseID: status.ID,
		Slug: status.Slug, Provider: status.Provider, ProviderResourceID: status.ServerID, Host: "host.example", AttachURL: "wss://terminal.example.test",
		Message: "workspace ready", CreatedAt: now, UpdatedAt: now,
	}
	service.state.Workspaces[id] = record
	runner.ready[id] = status
	if err := saveControllerState(service.opts.StateFile, service.state); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	fail := true
	service.saveState = func(path string, state controllerState) error {
		mu.Lock()
		shouldFail := fail
		mu.Unlock()
		dir := filepath.Dir(path)
		return saveControllerStateWithDirectorySync(path, state, func(syncPath string) error {
			if shouldFail && filepath.Clean(syncPath) == filepath.Clean(dir) {
				return errors.New("directory sync unavailable")
			}
			return nil
		})
	}
	path := "/v1/workspaces/" + id
	first := controllerHTTP(service, http.MethodDelete, path, "test-token", nil)
	if first.Code != http.StatusServiceUnavailable {
		t.Fatalf("first delete status=%d body=%s", first.Code, first.Body.String())
	}
	pending, _ := service.workspace(id)
	second := controllerHTTP(service, http.MethodDelete, path, "test-token", nil)
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second delete status=%d body=%s", second.Code, second.Body.String())
	}
	if current, _ := service.workspace(id); current != pending {
		t.Fatalf("delete retry rewrote cleanup state: before=%#v after=%#v", pending, current)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	third := controllerHTTP(service, http.MethodDelete, path, "test-token", nil)
	if third.Code != http.StatusAccepted {
		t.Fatalf("durable delete retry status=%d body=%s", third.Code, third.Body.String())
	}
	if current, _ := service.workspace(id); current != pending {
		t.Fatalf("durable delete retry rewrote cleanup state: before=%#v after=%#v", pending, current)
	}
	<-service.sem
	released = true
	waitControllerWorkspaceStatus(t, service, id, "stopped")
	runner.mu.Lock()
	stopCalls, localStops := runner.stopCalls, runner.localStopCalls
	runner.mu.Unlock()
	if stopCalls != 1 || localStops != 1 {
		t.Fatalf("provider stops=%d local stops=%d", stopCalls, localStops)
	}
}

func TestControllerBlocksProviderStopAfterLocalCleanupDurabilityFailure(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	defer cancel()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := controllerWorkspaceRecord{
		Request: controllerWorkspaceRequest{ID: "cleanup-durability-box"}, ProviderRoute: "external", ProviderScope: "test-provider-scope",
		CreateStarted: true, CreateObserved: true, AttemptLeaseID: "cbx_cleanupdurable123", LeaseID: "cbx_cleanupdurable123",
		Slug: "cleanup-durability", Provider: "external", ProviderResourceID: "provider/cleanup-durability", Status: "stopping", Message: "workspace stopping",
		CreatedAt: now, UpdatedAt: now,
	}
	service.state.Workspaces[record.Request.ID] = record
	if err := saveControllerState(service.opts.StateFile, service.state); err != nil {
		t.Fatal(err)
	}
	var syncState struct {
		sync.Mutex
		fail bool
	}
	syncState.fail = true
	service.saveState = func(path string, state controllerState) error {
		syncState.Lock()
		fail := syncState.fail
		syncState.Unlock()
		dir := filepath.Dir(path)
		return saveControllerStateWithDirectorySync(path, state, func(syncPath string) error {
			if fail && filepath.Clean(syncPath) == filepath.Clean(dir) {
				return errors.New("persistent cleanup state sync failure")
			}
			return nil
		})
	}

	service.stopWorkspace(record)
	runner.mu.Lock()
	localStops := runner.localStopCalls
	providerStops := runner.stopCalls
	runner.mu.Unlock()
	current, _ := service.workspace(record.Request.ID)
	if localStops != 1 || providerStops != 0 || !current.LocalDesktopStopped || current.ProviderStopped {
		t.Fatalf("cleanup crossed durability barrier local=%d provider=%d record=%#v", localStops, providerStops, current)
	}

	syncState.Lock()
	syncState.fail = false
	syncState.Unlock()
	stopped := waitControllerWorkspaceStatus(t, service, record.Request.ID, "stopped")
	runner.mu.Lock()
	localStops = runner.localStopCalls
	providerStops = runner.stopCalls
	runner.mu.Unlock()
	if localStops != 1 || providerStops != 1 || !stopped.ProviderStopped {
		t.Fatalf("cleanup recovery local=%d provider=%d record=%#v", localStops, providerStops, stopped)
	}
}

func TestControllerDeleteRevokesDesktopAcrossPendingDurabilityBarrier(t *testing.T) {
	runner := newFakeControllerWorkspaceRunner()
	service, cancel := testControllerService(t, runner, 1)
	t.Cleanup(func() {
		cancel()
		service.waitForShutdown()
	})
	created := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{
		ID: "delete-durability-desktop-box", Capabilities: controllerCapabilities{Desktop: true},
	})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	waitControllerWorkspaceStatus(t, service, "delete-durability-desktop-box", "ready")
	waitControllerWorkspaceInactive(t, service, "delete-durability-desktop-box")
	connection := controllerHTTP(service, http.MethodPost, "/v1/workspaces/delete-durability-desktop-box/connections/desktop", "test-token", nil)
	if connection.Code != http.StatusOK {
		t.Fatalf("desktop connection status=%d body=%s", connection.Code, connection.Body.String())
	}
	dir := filepath.Dir(service.opts.StateFile)
	service.saveState = func(path string, state controllerState) error {
		return saveControllerStateWithDirectorySync(path, state, func(syncPath string) error {
			if filepath.Clean(syncPath) == filepath.Clean(dir) {
				return errors.New("persistent delete directory sync failure")
			}
			return nil
		})
	}
	deleted := controllerHTTP(service, http.MethodDelete, "/v1/workspaces/delete-durability-desktop-box", "test-token", nil)
	if deleted.Code != http.StatusServiceUnavailable {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	var current controllerWorkspaceRecord
	for time.Now().Before(deadline) {
		current, _ = service.workspace("delete-durability-desktop-box")
		if current.LocalDesktopStopped {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	runner.mu.Lock()
	localStops := runner.localStopCalls
	providerStops := runner.stopCalls
	runner.mu.Unlock()
	if localStops != 1 || providerStops != 0 || !current.LocalDesktopStopped || current.ProviderStopped {
		t.Fatalf("pending delete cleanup local=%d provider=%d record=%#v", localStops, providerStops, current)
	}
}

func TestControllerHTTPKeepsInstalledStateAfterDirectorySyncFailure(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		service, cancel := testControllerService(t, newFakeControllerWorkspaceRunner(), 1)
		if err := os.MkdirAll(filepath.Dir(service.opts.StateFile), 0o700); err != nil {
			cancel()
			t.Fatal(err)
		}
		service.sem <- struct{}{}
		service.saveState = func(path string, state controllerState) error {
			dir := filepath.Dir(path)
			return saveControllerStateWithDirectorySync(path, state, func(syncPath string) error {
				if filepath.Clean(syncPath) == filepath.Clean(dir) {
					return errors.New("directory sync unavailable")
				}
				return nil
			})
		}
		response := controllerHTTP(service, http.MethodPost, "/v1/workspaces", "test-token", controllerWorkspaceRequest{ID: "installed-create-box"})
		if response.Code != http.StatusServiceUnavailable {
			cancel()
			<-service.sem
			t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
		}
		if record, ok := service.workspace("installed-create-box"); !ok || record.Status != "provisioning" {
			cancel()
			<-service.sem
			t.Fatalf("memory record=%#v exists=%t", record, ok)
		}
		loaded, err := loadControllerState(service.opts.StateFile)
		if err != nil || loaded.Workspaces["installed-create-box"].Status != "provisioning" {
			cancel()
			<-service.sem
			t.Fatalf("disk state=%#v err=%v", loaded, err)
		}
		cancel()
		service.waitForShutdown()
		<-service.sem
	})

	t.Run("delete", func(t *testing.T) {
		service, cancel := testControllerService(t, newFakeControllerWorkspaceRunner(), 1)
		now := time.Now().UTC().Format(time.RFC3339Nano)
		service.state.Workspaces["installed-delete-box"] = controllerWorkspaceRecord{
			Request: controllerWorkspaceRequest{ID: "installed-delete-box"}, ProviderRoute: "external", ProviderScope: "test-provider-scope", CreateStarted: true,
			AttemptLeaseID: "cbx_attempt123456", Status: "provisioning", Message: "provisioning",
			Slug: "cbx-ctl-installed-delete-box-0000000000000000", CreatedAt: now, UpdatedAt: now,
		}
		if err := saveControllerState(service.opts.StateFile, service.state); err != nil {
			cancel()
			t.Fatal(err)
		}
		service.sem <- struct{}{}
		service.saveState = func(path string, state controllerState) error {
			dir := filepath.Dir(path)
			return saveControllerStateWithDirectorySync(path, state, func(syncPath string) error {
				if filepath.Clean(syncPath) == filepath.Clean(dir) {
					return errors.New("directory sync unavailable")
				}
				return nil
			})
		}
		response := controllerHTTP(service, http.MethodDelete, "/v1/workspaces/installed-delete-box", "test-token", nil)
		if response.Code != http.StatusServiceUnavailable {
			cancel()
			<-service.sem
			t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
		}
		if record, ok := service.workspace("installed-delete-box"); !ok || record.Status != "stopping" {
			cancel()
			<-service.sem
			t.Fatalf("memory record=%#v exists=%t", record, ok)
		}
		loaded, err := loadControllerState(service.opts.StateFile)
		if err != nil || loaded.Workspaces["installed-delete-box"].Status != "stopping" {
			cancel()
			<-service.sem
			t.Fatalf("disk state=%#v err=%v", loaded, err)
		}
		cancel()
		service.waitForShutdown()
		<-service.sem
	})
}

func controllerHTTP(handler http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	var reader bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&reader).Encode(body)
	}
	request := httptest.NewRequest(method, path, &reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func waitControllerWorkspaceStatus(t *testing.T, service *controllerService, id, want string) controllerWorkspaceRecord {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if record, ok := service.workspace(id); ok && record.Status == want {
			return record
		}
		time.Sleep(10 * time.Millisecond)
	}
	record, _ := service.workspace(id)
	t.Fatalf("workspace %s status=%q want=%q message=%q", id, record.Status, want, record.Message)
	return controllerWorkspaceRecord{}
}

func waitControllerWorkspaceInactive(t *testing.T, service *controllerService, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		service.mu.Lock()
		_, active := service.active[id]
		service.mu.Unlock()
		if !active {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("workspace %s reconciliation remained active", id)
}
