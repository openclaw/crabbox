package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	controllerStateVersion = 5
	controllerMaxBodyBytes = 64 << 10
)

var controllerWorkspaceIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

var (
	errControllerWorkspaceStopping      = errors.New("controller workspace stopping")
	errControllerWorkspaceStillPresent  = errors.New("workspace remains in refreshed provider inventory")
	errControllerWorkspaceNeedsIdentity = errors.New("workspace remains present without an acknowledged raw provider identity")
	errControllerStateDurabilityPending = errors.New("controller state durability pending")
	errControllerSideEffectObsolete     = errors.New("controller side effect is no longer valid")
)

type controllerCapabilities struct {
	Desktop bool `json:"desktop"`
	Browser bool `json:"browser"`
	Code    bool `json:"code"`
}

type controllerWorkspaceResponseCapabilities struct {
	Terminal  bool `json:"terminal"`
	Takeover  bool `json:"takeover"`
	VNC       bool `json:"vnc"`
	Desktop   bool `json:"desktop"`
	Logs      bool `json:"logs"`
	Artifacts bool `json:"artifacts"`
}

type controllerWorkspaceRequest struct {
	ProviderRoute              string                 `json:"-"`
	ProviderScope              string                 `json:"-"`
	ProviderLeaseID            string                 `json:"-"`
	ProviderAttemptLeaseID     string                 `json:"-"`
	ProviderSlug               string                 `json:"-"`
	ProviderResourceID         string                 `json:"-"`
	CoordinatorRegistrationURL string                 `json:"-"`
	ID                         string                 `json:"id"`
	Repo                       string                 `json:"repo,omitempty"`
	Branch                     string                 `json:"branch,omitempty"`
	Runtime                    string                 `json:"runtime,omitempty"`
	Command                    string                 `json:"command,omitempty"`
	Prompt                     string                 `json:"prompt,omitempty"`
	Purpose                    string                 `json:"purpose,omitempty"`
	Summary                    string                 `json:"summary,omitempty"`
	Owner                      string                 `json:"owner,omitempty"`
	CreatedBy                  string                 `json:"createdBy,omitempty"`
	ParentSessionID            string                 `json:"parentSessionId,omitempty"`
	RootSessionID              string                 `json:"rootSessionId,omitempty"`
	Profile                    string                 `json:"profile,omitempty"`
	Class                      string                 `json:"class,omitempty"`
	ServerType                 string                 `json:"serverType,omitempty"`
	TTLSeconds                 int                    `json:"ttlSeconds,omitempty"`
	IdleTimeoutSeconds         int                    `json:"idleTimeoutSeconds,omitempty"`
	Capabilities               controllerCapabilities `json:"capabilities"`
}

type controllerWorkspaceRecord struct {
	Request                    controllerWorkspaceRequest `json:"request"`
	ProviderRoute              string                     `json:"providerRoute"`
	ProviderScope              string                     `json:"providerScope"`
	CoordinatorRegistrationURL string                     `json:"coordinatorRegistrationUrl,omitempty"`
	CreatePrepared             bool                       `json:"createPrepared,omitempty"`
	CreatePreparedAt           string                     `json:"createPreparedAt,omitempty"`
	CreateStarted              bool                       `json:"createStarted,omitempty"`
	CreateObserved             bool                       `json:"createObserved,omitempty"`
	AttemptLeaseID             string                     `json:"attemptLeaseId,omitempty"`
	FailureAfterCleanup        string                     `json:"failureAfterCleanup,omitempty"`
	StatusAfterCleanup         string                     `json:"statusAfterCleanup,omitempty"`
	ProviderStopped            bool                       `json:"providerStopped,omitempty"`
	ProviderReleaseRequested   bool                       `json:"providerReleaseRequested,omitempty"`
	ProviderAbsentSince        string                     `json:"providerAbsentSince,omitempty"`
	LocalDesktopStopped        bool                       `json:"localDesktopStopped,omitempty"`
	LocalCleanupPending        bool                       `json:"localCleanupPending,omitempty"`
	Status                     string                     `json:"status"`
	LeaseID                    string                     `json:"leaseId,omitempty"`
	Slug                       string                     `json:"slug,omitempty"`
	Provider                   string                     `json:"provider,omitempty"`
	ProviderResourceID         string                     `json:"providerResourceId,omitempty"`
	Host                       string                     `json:"host,omitempty"`
	AttachURL                  string                     `json:"attachUrl,omitempty"`
	Message                    string                     `json:"message"`
	ExpiresAt                  string                     `json:"expiresAt,omitempty"`
	ControllerExpiresAt        string                     `json:"controllerExpiresAt,omitempty"`
	ProviderExpiresAt          string                     `json:"providerExpiresAt,omitempty"`
	CreatedAt                  string                     `json:"createdAt"`
	UpdatedAt                  string                     `json:"updatedAt"`
}

type controllerWorkspaceResponse struct {
	ID                 string                                  `json:"id"`
	Status             string                                  `json:"status"`
	LeaseID            string                                  `json:"leaseId,omitempty"`
	Slug               string                                  `json:"slug,omitempty"`
	Provider           string                                  `json:"provider,omitempty"`
	ProviderResourceID string                                  `json:"providerResourceId,omitempty"`
	Host               string                                  `json:"host,omitempty"`
	AttachURL          string                                  `json:"attachUrl,omitempty"`
	Message            string                                  `json:"message"`
	Capabilities       controllerWorkspaceResponseCapabilities `json:"capabilities"`
	ExpiresAt          string                                  `json:"expiresAt,omitempty"`
	CreatedAt          string                                  `json:"createdAt"`
	UpdatedAt          string                                  `json:"updatedAt"`
}

type controllerState struct {
	Version    int                                  `json:"version"`
	Workspaces map[string]controllerWorkspaceRecord `json:"workspaces"`
}

type controllerServiceOptions struct {
	StateFile                string
	MaxConcurrent            int
	Allowed                  controllerCapabilities
	Profile                  string
	AttachURLTemplate        string
	VNCURLTemplate           string
	CreateTimeout            time.Duration
	InspectTimeout           time.Duration
	StopTimeout              time.Duration
	ConnectionTimeout        time.Duration
	RetryDelay               time.Duration
	ReadyReconcileInterval   time.Duration
	RequiredTTLSeconds       int
	RequiredIdleSeconds      int
	ForbidClassOverride      bool
	ForbidServerTypeOverride bool
}

type controllerWorkspaceRunner interface {
	ProviderIdentity(context.Context) (controllerProviderIdentity, error)
	Warmup(context.Context, string, string, controllerWorkspaceRequest, func() error, func(controllerAcquireIdentity) error) (controllerAcquireIdentity, error)
	Inspect(context.Context, string, controllerWorkspaceRequest) (StatusView, error)
	Stop(context.Context, string, controllerWorkspaceRequest) error
	ConfirmAbsent(context.Context, string, controllerWorkspaceRequest) (bool, error)
	CleanupAbsent(context.Context, string, controllerWorkspaceRequest) error
	StopLocal(context.Context, string, controllerWorkspaceRequest) error
	DesktopConnection(context.Context, string, controllerWorkspaceRequest) (string, error)
}

type controllerProviderIdentity struct {
	Route                      string
	Scope                      string
	IdempotentFixedLeaseID     bool
	CoordinatorRegistrationURL string
}

type controllerStartupRecoverer interface {
	RecoverControllerChildren(context.Context, string) error
}

type controllerService struct {
	ctx                        context.Context
	opts                       controllerServiceOptions
	runner                     controllerWorkspaceRunner
	providerRoute              string
	providerScope              string
	coordinatorRegistrationURL string
	saveState                  func(string, controllerState) error
	token                      [sha256.Size]byte
	now                        func() time.Time
	log                        io.Writer

	sideEffectGate     sync.Mutex
	sideEffects        map[uint64]*controllerSideEffect
	nextSideEffectID   uint64
	mu                 sync.Mutex
	state              controllerState
	durabilityPending  bool
	shuttingDown       bool
	active             map[string]struct{}
	pending            map[string]struct{}
	createOps          map[string]*controllerCreateOperation
	reconcileTimers    map[string]*controllerReconcileTimer
	connectionSlots    map[string]chan struct{}
	localCleanupRetry  map[string]struct{}
	terminalRevocation map[string]controllerTerminalRevocation
	desktopSetups      chan struct{}
	sem                chan struct{}
	reconcileWG        sync.WaitGroup
	stateLock          *controllerStateLock
	stateLockOnce      sync.Once
}

type controllerReconcileTimer struct {
	timer *time.Timer
}

type controllerTerminalRevocation struct {
	LeaseID         string
	ExpiresAt       string
	TerminalStatus  string
	TerminalMessage string
}

type controllerCreateOperation struct {
	cancel        context.CancelCauseFunc
	timeoutCancel context.CancelFunc
}

type controllerSideEffect struct {
	id                 uint64
	workspaceID        string
	cancel             context.CancelCauseFunc
	cancelOnBarrier    bool
	cancelOnTransition bool
}

func newControllerService(ctx context.Context, opts controllerServiceOptions, runner controllerWorkspaceRunner, token string, log io.Writer) (*controllerService, error) {
	return newControllerServiceWithStateSaver(ctx, opts, runner, token, log, saveControllerState)
}

func newControllerServiceWithStateSaver(
	ctx context.Context,
	opts controllerServiceOptions,
	runner controllerWorkspaceRunner,
	token string,
	log io.Writer,
	stateSaver func(string, controllerState) error,
) (*controllerService, error) {
	if strings.TrimSpace(opts.StateFile) == "" {
		return nil, fmt.Errorf("state file is required")
	}
	if opts.MaxConcurrent < 1 || opts.MaxConcurrent > 64 {
		return nil, fmt.Errorf("max concurrency must be between 1 and 64")
	}
	if runner == nil {
		return nil, fmt.Errorf("workspace runner is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("controller token is empty")
	}
	if stateSaver == nil {
		return nil, fmt.Errorf("controller state saver is required")
	}
	if opts.ReadyReconcileInterval <= 0 {
		opts.ReadyReconcileInterval = time.Minute
	}
	for name, value := range map[string]time.Duration{
		"create timeout":     opts.CreateTimeout,
		"inspect timeout":    opts.InspectTimeout,
		"stop timeout":       opts.StopTimeout,
		"connection timeout": opts.ConnectionTimeout,
	} {
		if value <= 0 {
			return nil, fmt.Errorf("%s must be greater than zero", name)
		}
	}
	if opts.VNCURLTemplate != "" && !opts.Allowed.Desktop {
		return nil, fmt.Errorf("VNC URL template requires desktop capability")
	}
	if opts.RetryDelay <= 0 {
		opts.RetryDelay = 2 * time.Second
	}
	if err := validateControllerTerminalURLTemplate(opts.AttachURLTemplate); err != nil {
		return nil, fmt.Errorf("attach URL template: %w", err)
	}
	if err := validateControllerURLTemplate(opts.VNCURLTemplate); err != nil {
		return nil, fmt.Errorf("VNC URL template: %w", err)
	}
	stateLock, err := acquireControllerStateLock(opts.StateFile)
	if err != nil {
		return nil, err
	}
	keepStateLock := false
	defer func() {
		if !keepStateLock {
			_ = stateLock.Unlock()
		}
	}()
	state, err := loadControllerState(opts.StateFile)
	if err != nil {
		return nil, err
	}
	// Loading proves file contents, not that the rename which installed them
	// reached stable directory storage. Reinstall and directory-sync even an
	// empty initial snapshot before child recovery can kill or reap processes.
	if err := stateSaver(opts.StateFile, state); err != nil {
		return nil, fmt.Errorf("establish controller state durability before child recovery: %w", err)
	}
	if recoverer, ok := runner.(controllerStartupRecoverer); ok {
		if err := recoverer.RecoverControllerChildren(ctx, opts.StateFile); err != nil {
			return nil, fmt.Errorf("recover controller lifecycle children: %w", err)
		}
	}
	identityCtx, cancelIdentity := context.WithTimeout(ctx, opts.InspectTimeout)
	providerIdentity, err := runner.ProviderIdentity(identityCtx)
	cancelIdentity()
	if err != nil {
		return nil, fmt.Errorf("resolve controller provider identity: %w", err)
	}
	providerIdentity.Route = strings.TrimSpace(providerIdentity.Route)
	providerIdentity.Scope = strings.TrimSpace(providerIdentity.Scope)
	providerIdentity.CoordinatorRegistrationURL = strings.TrimSpace(providerIdentity.CoordinatorRegistrationURL)
	if providerIdentity.Route == "" {
		return nil, fmt.Errorf("controller provider route is required")
	}
	if providerIdentity.Scope == "" {
		return nil, fmt.Errorf("controller provider scope is required")
	}
	if !providerIdentity.IdempotentFixedLeaseID {
		return nil, fmt.Errorf("provider=%s does not explicitly guarantee idempotent fixed lease IDs", providerIdentity.Route)
	}
	if err := validateControllerCoordinatorRegistrationURL(providerIdentity.CoordinatorRegistrationURL); err != nil {
		return nil, fmt.Errorf("controller coordinator registration binding: %w", err)
	}
	s := &controllerService{
		ctx:                        ctx,
		opts:                       opts,
		runner:                     runner,
		providerRoute:              providerIdentity.Route,
		providerScope:              providerIdentity.Scope,
		coordinatorRegistrationURL: providerIdentity.CoordinatorRegistrationURL,
		saveState:                  stateSaver,
		token:                      sha256.Sum256([]byte(token)),
		now:                        time.Now,
		log:                        log,
		state:                      state,
		durabilityPending:          false,
		sideEffects:                map[uint64]*controllerSideEffect{},
		active:                     map[string]struct{}{},
		pending:                    map[string]struct{}{},
		createOps:                  map[string]*controllerCreateOperation{},
		reconcileTimers:            map[string]*controllerReconcileTimer{},
		connectionSlots:            map[string]chan struct{}{},
		localCleanupRetry:          map[string]struct{}{},
		terminalRevocation:         map[string]controllerTerminalRevocation{},
		desktopSetups:              make(chan struct{}, 1),
		sem:                        make(chan struct{}, opts.MaxConcurrent),
		stateLock:                  stateLock,
	}
	keepStateLock = true
	go s.cancelReconcileTimersOnShutdown()
	return s, nil
}

func (s *controllerService) cancelReconcileTimersOnShutdown() {
	<-s.ctx.Done()
	s.mu.Lock()
	s.shuttingDown = true
	for id := range s.reconcileTimers {
		s.cancelReconcileTimerLocked(id)
	}
	s.mu.Unlock()
	s.reconcileWG.Wait()
}

func (s *controllerService) waitForShutdown() {
	s.mu.Lock()
	s.shuttingDown = true
	for id := range s.reconcileTimers {
		s.cancelReconcileTimerLocked(id)
	}
	s.mu.Unlock()
	s.reconcileWG.Wait()
	s.releaseStateLock()
}

func (s *controllerService) releaseStateLock() {
	s.stateLockOnce.Do(func() {
		if s.stateLock != nil {
			_ = s.stateLock.Unlock()
		}
	})
}

func (s *controllerService) beginCreateOperation(record controllerWorkspaceRecord) (context.Context, *controllerCreateOperation, bool) {
	causeCtx, cancel := context.WithCancelCause(s.ctx)
	createCtx, timeoutCancel := context.WithTimeout(causeCtx, s.provisioningOperationTimeout(record))
	op := &controllerCreateOperation{cancel: cancel, timeoutCancel: timeoutCancel}

	s.mu.Lock()
	current, ok := s.state.Workspaces[record.Request.ID]
	_, active := s.createOps[record.Request.ID]
	if !ok || current.Status != "provisioning" || current.AttemptLeaseID != record.AttemptLeaseID || active {
		s.mu.Unlock()
		cancel(context.Canceled)
		timeoutCancel()
		return nil, nil, false
	}
	s.createOps[record.Request.ID] = op
	s.mu.Unlock()
	return createCtx, op, true
}

func (s *controllerService) finishCreateOperation(id string, op *controllerCreateOperation) {
	op.timeoutCancel()
	op.cancel(context.Canceled)
	s.mu.Lock()
	if s.createOps[id] == op {
		delete(s.createOps, id)
	}
	s.mu.Unlock()
}

func (s *controllerService) cancelCreateOperationLocked(id string) {
	if op := s.createOps[id]; op != nil {
		op.cancel(errControllerWorkspaceStopping)
	}
}

func (s *controllerService) logInstalledStateWarning(id string, err error) {
	if s.log != nil {
		fmt.Fprintf(s.log, "controller state installed but directory sync failed workspace=%s: %v\n", id, err)
	}
}

func (s *controllerService) ensureDurableState(id string) bool {
	s.sideEffectGate.Lock()
	defer s.sideEffectGate.Unlock()
	return s.ensureDurableStateUnderGate(id)
}

func (s *controllerService) ensureDurableStateUnderGate(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.durabilityPending {
		return true
	}
	err := s.saveState(s.opts.StateFile, s.state)
	if err == nil {
		s.durabilityPending = false
		return true
	}
	if controllerStateInstalled(err) {
		s.logInstalledStateWarning(id, err)
	} else if s.log != nil {
		fmt.Fprintf(s.log, "controller state durability retry failed workspace=%s: %v\n", id, err)
	}
	s.cancelBarrierSideEffectsUnderGate(errControllerStateDurabilityPending)
	return false
}

func (s *controllerService) beginSideEffect(
	parent context.Context,
	id string,
	allowDurabilityPending bool,
	cancelOnBarrier bool,
	cancelOnTransition bool,
	valid func(controllerWorkspaceRecord) bool,
) (context.Context, *controllerSideEffect, error) {
	s.sideEffectGate.Lock()
	defer s.sideEffectGate.Unlock()
	if !allowDurabilityPending && !s.ensureDurableStateUnderGate(id) {
		return nil, nil, errControllerStateDurabilityPending
	}
	if valid != nil {
		s.mu.Lock()
		record, ok := s.state.Workspaces[id]
		validNow := ok && valid(record)
		s.mu.Unlock()
		if !validNow {
			return nil, nil, errControllerSideEffectObsolete
		}
	}
	effectCtx, cancel := context.WithCancelCause(parent)
	s.nextSideEffectID++
	effect := &controllerSideEffect{
		id:                 s.nextSideEffectID,
		workspaceID:        id,
		cancel:             cancel,
		cancelOnBarrier:    cancelOnBarrier,
		cancelOnTransition: cancelOnTransition,
	}
	s.sideEffects[effect.id] = effect
	return effectCtx, effect, nil
}

func (s *controllerService) finishSideEffect(effect *controllerSideEffect) {
	if effect == nil {
		return
	}
	s.sideEffectGate.Lock()
	if s.sideEffects[effect.id] == effect {
		delete(s.sideEffects, effect.id)
	}
	effect.cancel(context.Canceled)
	s.sideEffectGate.Unlock()
}

func (s *controllerService) cancelBarrierSideEffectsUnderGate(cause error) {
	for _, effect := range s.sideEffects {
		if effect.cancelOnBarrier {
			effect.cancel(cause)
		}
	}
}

func (s *controllerService) cancelTransitionSideEffectsUnderGate(id string, cause error) {
	for _, effect := range s.sideEffects {
		if effect.workspaceID == id && effect.cancelOnTransition {
			effect.cancel(cause)
		}
	}
}

// recordStateSaveResultUnderGateLocked updates the global durability barrier.
// Callers hold sideEffectGate and mu so a side effect cannot cross the state
// installation decision.
func (s *controllerService) recordStateSaveResultUnderGateLocked(id string, err error) bool {
	if err == nil {
		s.durabilityPending = false
		return true
	}
	if !controllerStateInstalled(err) {
		return false
	}
	s.durabilityPending = true
	s.logInstalledStateWarning(id, err)
	s.cancelBarrierSideEffectsUnderGate(errControllerStateDurabilityPending)
	return true
}

func (s *controllerService) startReconciliation() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.state.Workspaces))
	for id, record := range s.state.Workspaces {
		switch record.Status {
		case "provisioning", "ready", "stopping":
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.enqueue(id)
	}
}

func (s *controllerService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/healthz" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeControllerMethodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		writeControllerJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="crabbox-controller"`)
		writeControllerError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
		return
	}
	if r.URL.Path == "/v1/workspaces" {
		if r.Method != http.MethodPost {
			writeControllerMethodNotAllowed(w, http.MethodPost)
			return
		}
		s.createWorkspace(w, r)
		return
	}
	const prefix = "/v1/workspaces/"
	if !strings.HasPrefix(r.URL.Path, prefix) || len(r.URL.Path) == len(prefix) {
		writeControllerError(w, http.StatusNotFound, "not_found", "route not found")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), prefix), "/")
	id, err := url.PathUnescape(parts[0])
	if err != nil || !validControllerWorkspaceID(id) {
		writeControllerError(w, http.StatusBadRequest, "invalid_workspace_id", "workspace id must be a lowercase DNS-style name")
		return
	}
	if len(parts) == 3 && parts[1] == "connections" && parts[2] == "desktop" {
		if r.Method != http.MethodPost {
			writeControllerMethodNotAllowed(w, http.MethodPost)
			return
		}
		s.desktopConnection(w, r.Context(), id)
		return
	}
	if len(parts) != 1 {
		writeControllerError(w, http.StatusNotFound, "not_found", "route not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getWorkspace(w, id)
	case http.MethodDelete:
		s.deleteWorkspace(w, id)
	default:
		writeControllerMethodNotAllowed(w, http.MethodGet, http.MethodDelete)
	}
}

func (s *controllerService) desktopConnection(w http.ResponseWriter, requestCtx context.Context, id string) {
	record, ok := s.workspace(id)
	if !ok {
		writeControllerError(w, http.StatusNotFound, "workspace_not_found", "workspace not found")
		return
	}
	if record.Status != "ready" {
		writeControllerError(w, http.StatusConflict, "workspace_not_ready", "workspace must be ready before opening a desktop connection")
		return
	}
	if controllerWorkspaceExpired(controllerEffectiveExpiry(record, ""), s.now()) {
		s.enqueueReadyWorkspaceCleanup(record, controllerEffectiveExpiry(record, ""), "expired", "workspace expired")
		writeControllerError(w, http.StatusConflict, "workspace_expired", "workspace expired before desktop setup")
		return
	}
	_, terminalRevocationPending := s.terminalRevocationFor(id)
	if record.LocalCleanupPending || s.hasLocalCleanupRetry(id) || terminalRevocationPending {
		s.enqueue(id)
		writeControllerError(w, http.StatusServiceUnavailable, "desktop_cleanup_pending", "previous desktop bridge cleanup is still pending")
		return
	}
	if !record.Request.Capabilities.Desktop {
		writeControllerError(w, http.StatusConflict, "desktop_not_requested", "workspace was not created with desktop capability")
		return
	}
	if !s.ensureDurableState(id) {
		s.scheduleReconcile(id)
		writeControllerError(w, http.StatusServiceUnavailable, "state_durability_pending", "controller state durability is pending")
		return
	}
	connectionCtx, cancel := context.WithTimeout(requestCtx, s.opts.ConnectionTimeout)
	defer cancel()
	select {
	case s.desktopSetups <- struct{}{}:
		defer func() { <-s.desktopSetups }()
	case <-connectionCtx.Done():
		writeControllerError(w, http.StatusServiceUnavailable, "controller_busy", "desktop bridge setup is busy")
		return
	}
	connectionSlot := s.workspaceConnectionSlot(id)
	select {
	case connectionSlot <- struct{}{}:
		defer func() { <-connectionSlot }()
	case <-connectionCtx.Done():
		writeControllerError(w, http.StatusServiceUnavailable, "controller_busy", "workspace desktop connection setup is busy")
		return
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-connectionCtx.Done():
		writeControllerError(w, http.StatusServiceUnavailable, "controller_busy", "controller lifecycle capacity is busy")
		return
	}
	current, ok := s.workspace(id)
	_, terminalRevocationPending = s.terminalRevocationFor(id)
	if !ok || current.Status != "ready" || current.LeaseID != record.LeaseID || current.LocalCleanupPending || s.hasLocalCleanupRetry(id) || terminalRevocationPending {
		writeControllerError(w, http.StatusConflict, "workspace_not_ready", "workspace is no longer ready")
		return
	}
	effectiveExpiry := controllerEffectiveExpiry(current, "")
	if controllerWorkspaceExpired(effectiveExpiry, s.now()) {
		s.enqueueReadyWorkspaceCleanup(current, effectiveExpiry, "expired", "workspace expired")
		writeControllerError(w, http.StatusConflict, "workspace_expired", "workspace expired before desktop setup")
		return
	}
	record = current
	identifier := firstNonBlank(record.LeaseID, record.Slug)
	if identifier == "" {
		writeControllerError(w, http.StatusConflict, "workspace_not_ready", "workspace has no verified provider identity")
		return
	}
	effectCtx, effect, beginErr := s.beginSideEffect(
		connectionCtx,
		id,
		false,
		true,
		true,
		func(candidate controllerWorkspaceRecord) bool {
			return candidate.Status == "ready" && candidate.LeaseID == record.LeaseID &&
				!candidate.LocalCleanupPending && !terminalRevocationPending &&
				!controllerWorkspaceExpired(controllerEffectiveExpiry(candidate, ""), s.now())
		},
	)
	if beginErr != nil {
		if errors.Is(beginErr, errControllerStateDurabilityPending) {
			s.scheduleReconcile(id)
			writeControllerError(w, http.StatusServiceUnavailable, "state_durability_pending", "controller state durability is pending")
			return
		}
		writeControllerError(w, http.StatusConflict, "workspace_not_ready", "workspace lifecycle changed before desktop setup")
		return
	}
	connectionURL, err := s.runner.DesktopConnection(effectCtx, identifier, controllerRequestForRecord(record))
	effectCause := context.Cause(effectCtx)
	s.finishSideEffect(effect)
	if err == nil {
		err = effectCause
		if err == nil {
			err = context.Cause(connectionCtx)
		}
	}
	if err != nil {
		s.revokeDesktopBridge(record, identifier)
		if errors.Is(effectCause, errControllerStateDurabilityPending) {
			s.scheduleReconcile(id)
			writeControllerError(w, http.StatusServiceUnavailable, "state_durability_pending", "controller state durability became pending during desktop setup")
			return
		}
		if errors.Is(effectCause, errControllerWorkspaceStopping) {
			writeControllerError(w, http.StatusConflict, "workspace_not_ready", "workspace lifecycle changed during desktop setup")
			return
		}
		writeControllerError(w, http.StatusBadGateway, "desktop_connection_failed", "could not prepare a verified desktop connection")
		return
	}
	current, ok = s.workspace(id)
	effectiveExpiry = ""
	if ok {
		effectiveExpiry = controllerEffectiveExpiry(current, "")
	}
	expired := ok && current.Status == "ready" && current.LeaseID == record.LeaseID && controllerWorkspaceExpired(effectiveExpiry, s.now())
	_, terminalRevocationPending = s.terminalRevocationFor(id)
	if !ok || current.Status != "ready" || current.LeaseID != record.LeaseID || current.LocalCleanupPending || terminalRevocationPending || expired {
		if expired {
			s.enqueueReadyWorkspaceCleanup(current, effectiveExpiry, "expired", "workspace expired")
		}
		s.revokeDesktopBridge(record, identifier)
		writeControllerError(w, http.StatusConflict, "workspace_not_ready", "workspace lifecycle changed during desktop setup")
		return
	}
	if s.opts.VNCURLTemplate != "" {
		connectionURL = expandControllerURLTemplate(s.opts.VNCURLTemplate, current)
	}
	if err := validateControllerConnectionURL(connectionURL); err != nil {
		s.revokeDesktopBridge(record, identifier)
		writeControllerError(w, http.StatusBadGateway, "invalid_desktop_url", err.Error())
		return
	}
	// Serialize the final lifecycle check with DELETE so the response cannot be
	// committed after a concurrent stop transition.
	s.sideEffectGate.Lock()
	s.mu.Lock()
	final, ok := s.state.Workspaces[id]
	_, retryPending := s.localCleanupRetry[id]
	_, terminalRevocationPending = s.terminalRevocation[id]
	finalExpiry := ""
	if ok {
		finalExpiry = controllerEffectiveExpiry(final, "")
	}
	finalExpired := ok && final.Status == "ready" && final.LeaseID == record.LeaseID && controllerWorkspaceExpired(finalExpiry, s.now())
	requestCanceled := context.Cause(connectionCtx) != nil
	finalReady := ok && final.Status == "ready" && final.LeaseID == record.LeaseID && !final.LocalCleanupPending && !retryPending && !terminalRevocationPending && !finalExpired && !s.durabilityPending && !requestCanceled
	if finalReady {
		writeControllerJSON(w, http.StatusOK, map[string]string{"url": connectionURL})
		s.mu.Unlock()
		s.sideEffectGate.Unlock()
		return
	}
	s.mu.Unlock()
	s.sideEffectGate.Unlock()
	if finalExpired {
		s.enqueueReadyWorkspaceCleanup(final, finalExpiry, "expired", "workspace expired")
	}
	s.revokeDesktopBridge(record, identifier)
	writeControllerError(w, http.StatusConflict, "workspace_not_ready", "workspace lifecycle changed before desktop handoff")
}

func (s *controllerService) revokeDesktopBridge(record controllerWorkspaceRecord, identifier string) {
	revokeCtx, cancel := context.WithTimeout(s.ctx, s.opts.StopTimeout)
	effectCtx, effect, beginErr := s.beginSideEffect(revokeCtx, record.Request.ID, true, false, false, nil)
	if beginErr != nil {
		cancel()
		s.persistReadyLocalCleanupPending(record, beginErr)
		return
	}
	err := s.runner.StopLocal(effectCtx, identifier, controllerRequestForRecord(record))
	s.finishSideEffect(effect)
	cancel()
	if err != nil {
		if s.log != nil {
			fmt.Fprintf(s.log, "controller desktop revoke failed workspace=%s: %v\n", record.Request.ID, err)
		}
		s.persistReadyLocalCleanupPending(record, err)
		return
	}
	_ = s.updateRecord(record.Request.ID, func(current *controllerWorkspaceRecord) bool {
		if current.LeaseID != record.LeaseID {
			return false
		}
		switch current.Status {
		case "stopping":
			current.LocalDesktopStopped = true
			current.LocalCleanupPending = false
		case "ready":
			current.LocalCleanupPending = false
			current.Message = "workspace ready"
		default:
			return false
		}
		current.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
		return true
	})
}

func (s *controllerService) persistReadyLocalCleanupPending(record controllerWorkspaceRecord, cause error) {
	s.setLocalCleanupRetry(record.Request.ID, true)
	err := s.updateRecord(record.Request.ID, func(current *controllerWorkspaceRecord) bool {
		if current.Status != "ready" || current.LeaseID != record.LeaseID {
			return false
		}
		current.LocalCleanupPending = true
		current.Message = "workspace ready; local desktop cleanup pending"
		current.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
		return true
	})
	if err != nil && s.log != nil {
		fmt.Fprintf(s.log, "controller desktop cleanup state failed workspace=%s: %v\n", record.Request.ID, err)
	}
	if current, ok := s.workspace(record.Request.ID); ok && current.Status == "ready" && current.LocalCleanupPending {
		s.setLocalCleanupRetry(record.Request.ID, false)
		s.scheduleReconcile(record.Request.ID)
	} else if err != nil {
		s.scheduleReconcile(record.Request.ID)
		if cause != nil && s.log != nil {
			fmt.Fprintf(s.log, "controller desktop cleanup retry remains memory-backed workspace=%s: %v\n", record.Request.ID, cause)
		}
	} else {
		s.setLocalCleanupRetry(record.Request.ID, false)
	}
}

func (s *controllerService) setLocalCleanupRetry(id string, pending bool) {
	s.mu.Lock()
	if pending {
		s.localCleanupRetry[id] = struct{}{}
	} else {
		delete(s.localCleanupRetry, id)
	}
	s.mu.Unlock()
}

func (s *controllerService) hasLocalCleanupRetry(id string) bool {
	s.mu.Lock()
	_, pending := s.localCleanupRetry[id]
	s.mu.Unlock()
	return pending
}

func (s *controllerService) setTerminalRevocation(id string, pending controllerTerminalRevocation) {
	s.mu.Lock()
	s.terminalRevocation[id] = pending
	s.mu.Unlock()
}

func (s *controllerService) clearTerminalRevocation(id string) {
	s.mu.Lock()
	delete(s.terminalRevocation, id)
	s.mu.Unlock()
}

func (s *controllerService) terminalRevocationFor(id string) (controllerTerminalRevocation, bool) {
	s.mu.Lock()
	pending, ok := s.terminalRevocation[id]
	s.mu.Unlock()
	return pending, ok
}

func (s *controllerService) workspaceConnectionSlot(id string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot, ok := s.connectionSlots[id]
	if !ok {
		slot = make(chan struct{}, 1)
		s.connectionSlots[id] = slot
	}
	return slot
}

func (s *controllerService) authorized(r *http.Request) bool {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return subtle.ConstantTimeCompare(got[:], s.token[:]) == 1
}

func (s *controllerService) createWorkspace(w http.ResponseWriter, r *http.Request) {
	if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])); mediaType != "application/json" {
		writeControllerError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	var request controllerWorkspaceRequest
	if err := decodeControllerJSON(w, r, &request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeControllerError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 64 KiB")
			return
		}
		writeControllerError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	request = normalizeControllerWorkspaceRequest(request)
	if request.Profile == "" {
		request.Profile = s.opts.Profile
	}
	if err := validateControllerWorkspaceRequest(request, s.opts); err != nil {
		writeControllerError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	nowTime := s.now().UTC()
	now := nowTime.Format(time.RFC3339Nano)
	controllerExpiresAt := controllerRequestedExpiresAt(nowTime, request.TTLSeconds)
	provisioningSlug, err := newControllerProvisioningSlug(request.ID)
	if err != nil {
		writeControllerError(w, http.StatusInternalServerError, "identity_generation_failed", "could not generate workspace identity")
		return
	}
	record := controllerWorkspaceRecord{
		Request:                    request,
		ProviderRoute:              s.providerRoute,
		ProviderScope:              s.providerScope,
		CoordinatorRegistrationURL: s.coordinatorRegistrationURL,
		AttemptLeaseID:             newLeaseID(),
		Status:                     "provisioning",
		Slug:                       provisioningSlug,
		Message:                    "workspace provisioning",
		ExpiresAt:                  controllerExpiresAt,
		ControllerExpiresAt:        controllerExpiresAt,
		CreatedAt:                  now,
		UpdatedAt:                  now,
	}

	s.sideEffectGate.Lock()
	s.mu.Lock()
	if existing, ok := s.state.Workspaces[request.ID]; ok {
		s.mu.Unlock()
		if existing.Request != request {
			s.sideEffectGate.Unlock()
			writeControllerError(w, http.StatusConflict, "workspace_id_conflict", "workspace id already exists with a different immutable request")
			return
		}
		durable := s.ensureDurableStateUnderGate(request.ID)
		s.mu.Lock()
		existing = s.state.Workspaces[request.ID]
		s.mu.Unlock()
		s.sideEffectGate.Unlock()
		if !durable {
			s.enqueue(request.ID)
			writeControllerDurabilityPending(w)
			return
		}
		status := http.StatusOK
		if existing.Status == "provisioning" || existing.Status == "stopping" {
			status = http.StatusAccepted
		}
		writeControllerJSON(w, status, controllerResponse(existing))
		return
	}
	s.state.Workspaces[request.ID] = record
	durable := true
	if err := s.saveState(s.opts.StateFile, s.state); err != nil {
		durable = false
		if !s.recordStateSaveResultUnderGateLocked(request.ID, err) {
			delete(s.state.Workspaces, request.ID)
			s.mu.Unlock()
			s.sideEffectGate.Unlock()
			writeControllerError(w, http.StatusInternalServerError, "state_write_failed", "could not persist workspace request")
			return
		}
	} else {
		s.recordStateSaveResultUnderGateLocked(request.ID, nil)
	}
	s.mu.Unlock()
	s.sideEffectGate.Unlock()
	s.enqueue(request.ID)
	if !durable {
		writeControllerDurabilityPending(w)
		return
	}
	writeControllerJSON(w, http.StatusAccepted, controllerResponse(record))
}

func (s *controllerService) getWorkspace(w http.ResponseWriter, id string) {
	s.mu.Lock()
	record, ok := s.state.Workspaces[id]
	s.mu.Unlock()
	if !ok {
		writeControllerError(w, http.StatusNotFound, "workspace_not_found", "workspace not found")
		return
	}
	switch record.Status {
	case "provisioning", "ready", "stopping":
		s.enqueue(id)
	}
	writeControllerJSON(w, http.StatusOK, controllerResponse(record))
}

func (s *controllerService) deleteWorkspace(w http.ResponseWriter, id string) {
	s.sideEffectGate.Lock()
	s.mu.Lock()
	record, ok := s.state.Workspaces[id]
	if !ok {
		s.mu.Unlock()
		s.sideEffectGate.Unlock()
		writeControllerError(w, http.StatusNotFound, "workspace_not_found", "workspace not found")
		return
	}
	if record.Status == "stopped" {
		s.cancelReconcileTimerLocked(id)
		s.mu.Unlock()
		durable := s.ensureDurableStateUnderGate(id)
		s.sideEffectGate.Unlock()
		if !durable {
			writeControllerDurabilityPending(w)
			return
		}
		writeControllerJSON(w, http.StatusOK, controllerResponse(record))
		return
	}
	if record.Status == "stopping" {
		s.mu.Unlock()
		durable := s.ensureDurableStateUnderGate(id)
		s.mu.Lock()
		record = s.state.Workspaces[id]
		s.mu.Unlock()
		s.sideEffectGate.Unlock()
		s.enqueue(id)
		if !durable {
			writeControllerDurabilityPending(w)
			return
		}
		writeControllerJSON(w, http.StatusAccepted, controllerResponse(record))
		return
	}
	previous := record
	record.Status = "stopping"
	if record.LeaseID != "" {
		record.CreateObserved = true
	}
	record.Message = "workspace stopping"
	record.FailureAfterCleanup = ""
	record.StatusAfterCleanup = ""
	record.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
	s.state.Workspaces[id] = record
	durable := true
	if err := s.saveState(s.opts.StateFile, s.state); err != nil {
		durable = false
		if !s.recordStateSaveResultUnderGateLocked(id, err) {
			s.state.Workspaces[id] = previous
			s.mu.Unlock()
			s.sideEffectGate.Unlock()
			writeControllerError(w, http.StatusInternalServerError, "state_write_failed", "could not persist stop request")
			return
		}
	} else {
		s.recordStateSaveResultUnderGateLocked(id, nil)
	}
	s.cancelTransitionSideEffectsUnderGate(id, errControllerWorkspaceStopping)
	s.cancelCreateOperationLocked(id)
	s.cancelReconcileTimerLocked(id)
	s.mu.Unlock()
	s.sideEffectGate.Unlock()
	s.enqueue(id)
	if !durable {
		writeControllerDurabilityPending(w)
		return
	}
	writeControllerJSON(w, http.StatusAccepted, controllerResponse(record))
}

func writeControllerDurabilityPending(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	writeControllerError(w, http.StatusServiceUnavailable, "state_durability_pending", "controller state durability is pending; retry the same request")
}

func (s *controllerService) enqueue(id string) {
	s.mu.Lock()
	if s.shuttingDown || s.ctx.Err() != nil {
		s.cancelReconcileTimerLocked(id)
		s.mu.Unlock()
		return
	}
	s.cancelReconcileTimerLocked(id)
	if _, exists := s.active[id]; exists {
		s.pending[id] = struct{}{}
		s.mu.Unlock()
		return
	}
	s.active[id] = struct{}{}
	s.reconcileWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.reconcileWG.Done()
		defer func() {
			s.mu.Lock()
			delete(s.active, id)
			_, pending := s.pending[id]
			delete(s.pending, id)
			record, shouldRetry := s.state.Workspaces[id]
			shouldRetry = shouldRetry && record.Status == "stopping" && s.ctx.Err() == nil
			s.mu.Unlock()
			if pending {
				s.enqueue(id)
			} else if shouldRetry {
				s.scheduleReconcile(id)
			}
		}()
		connectionSlot := s.workspaceConnectionSlot(id)
		select {
		case connectionSlot <- struct{}{}:
			defer func() { <-connectionSlot }()
		case <-s.ctx.Done():
			return
		}
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-s.ctx.Done():
			return
		}
		s.reconcile(id)
	}()
}

func (s *controllerService) reconcile(id string) {
	record, ok := s.workspace(id)
	if !ok {
		return
	}
	if record.Status == "stopping" {
		s.setLocalCleanupRetry(id, false)
		if !record.CreatePrepared && !record.CreateStarted {
			if !s.ensureDurableState(id) {
				s.scheduleReconcile(id)
				return
			}
			_ = s.markStopped(record.Request.ID)
			return
		}
		s.stopWorkspace(record)
		return
	}
	switch record.Status {
	case "ready":
		s.reconcileReady(record)
		return
	case "provisioning":
		if !s.ensureDurableState(id) {
			s.scheduleReconcile(id)
			return
		}
		s.reconcileProvisioning(record)
		return
	default:
		return
	}
}

func (s *controllerService) reconcileReady(record controllerWorkspaceRecord) {
	if pending, ok := s.terminalRevocationFor(record.Request.ID); ok {
		transitioned, err := s.transitionReadyWorkspaceForCleanup(record, pending.ExpiresAt, pending.TerminalStatus, pending.TerminalMessage)
		if err != nil {
			s.revokeReadyDesktopAfterTransitionFailure(record, pending.ExpiresAt, pending.TerminalStatus, pending.TerminalMessage)
			return
		}
		if transitioned {
			s.clearTerminalRevocation(record.Request.ID)
			s.setLocalCleanupRetry(record.Request.ID, false)
			if current, ok := s.workspace(record.Request.ID); ok {
				s.stopWorkspace(current)
			}
			return
		}
		if current, ok := s.workspace(record.Request.ID); !ok || current.Status != "ready" || (pending.LeaseID != "" && current.LeaseID != pending.LeaseID) {
			s.clearTerminalRevocation(record.Request.ID)
			s.setLocalCleanupRetry(record.Request.ID, false)
		}
		return
	}
	controllerExpiry := controllerEffectiveExpiry(record, "")
	if controllerWorkspaceExpired(controllerExpiry, s.now()) {
		s.cleanupReadyWorkspace(record, controllerExpiry, "expired", "workspace expired")
		return
	}
	memoryCleanupPending := s.hasLocalCleanupRetry(record.Request.ID)
	if record.LocalCleanupPending || memoryCleanupPending {
		if !s.retryReadyLocalCleanup(record, memoryCleanupPending) {
			return
		}
		var ok bool
		record, ok = s.workspace(record.Request.ID)
		if !ok || record.Status != "ready" || record.LocalCleanupPending {
			return
		}
	}
	// Local credential-bridge revocation above is deliberately allowed through
	// a pending state-directory sync. Only provider inspection waits behind the
	// durability barrier, so storage failure cannot keep a bridge alive.
	if !s.ensureDurableState(record.Request.ID) {
		s.scheduleReconcile(record.Request.ID)
		return
	}
	identifier := firstNonBlank(record.LeaseID, record.Slug, record.Request.ID)
	inspectCtx, cancel := context.WithTimeout(s.ctx, s.opts.InspectTimeout)
	effectCtx, effect, beginErr := s.beginSideEffect(
		inspectCtx,
		record.Request.ID,
		false,
		true,
		true,
		func(candidate controllerWorkspaceRecord) bool {
			return candidate.Status == "ready" && candidate.LeaseID == record.LeaseID
		},
	)
	if beginErr != nil {
		cancel()
		if errors.Is(beginErr, errControllerStateDurabilityPending) {
			s.scheduleReconcile(record.Request.ID)
		}
		return
	}
	status, inspectErr := s.runner.Inspect(effectCtx, identifier, controllerRequestForRecord(record))
	s.finishSideEffect(effect)
	cancel()
	if controllerWorkspaceNotFound(inspectErr) {
		s.cleanupReadyWorkspace(record, record.ExpiresAt, "expired", "workspace no longer exists at provider")
		return
	}
	if inspectErr != nil {
		if controllerWorkspaceExpired(record.ExpiresAt, s.now()) {
			s.cleanupReadyWorkspace(record, record.ExpiresAt, "expired", "workspace expired")
			return
		}
		s.markReadyInspectionFailed(record.Request.ID)
		return
	}
	if status.ID == "" || !controllerStatusMatchesRecord(record, status) {
		s.cleanupReadyWorkspace(record, record.ExpiresAt, "failed", "provider identity mismatch during ready inspection")
		return
	}
	effectiveExpiry := controllerEffectiveExpiry(record, status.ExpiresAt)
	if controllerWorkspaceExpired(effectiveExpiry, s.now()) {
		s.cleanupReadyWorkspace(record, effectiveExpiry, "expired", "workspace expired")
		return
	}
	if statusTerminalState(status.State) {
		normalized := strings.ToLower(strings.TrimSpace(status.State))
		switch normalized {
		case "expired":
			s.cleanupReadyWorkspace(record, status.ExpiresAt, "expired", "workspace expired")
		case "failed":
			s.cleanupReadyWorkspace(record, status.ExpiresAt, "failed", "workspace failed at provider")
		default:
			s.cleanupReadyWorkspace(record, status.ExpiresAt, "stopped", "workspace stopped at provider")
		}
		return
	}
	if status.Ready {
		if err := s.markReady(record.Request.ID, status); err != nil {
			s.scheduleReconcile(record.Request.ID)
		}
		return
	}
	s.markReadyInspectionFailed(record.Request.ID)
}

func (s *controllerService) retryReadyLocalCleanup(record controllerWorkspaceRecord, memoryPending bool) bool {
	identifier := firstNonBlank(record.LeaseID, record.Slug)
	if identifier == "" {
		s.scheduleReconcile(record.Request.ID)
		return false
	}
	cleanupCtx, cancel := context.WithTimeout(s.ctx, s.opts.StopTimeout)
	effectCtx, effect, beginErr := s.beginSideEffect(
		cleanupCtx,
		record.Request.ID,
		true,
		false,
		false,
		func(candidate controllerWorkspaceRecord) bool {
			return candidate.Status == "ready" && candidate.LeaseID == record.LeaseID && (candidate.LocalCleanupPending || memoryPending)
		},
	)
	if beginErr != nil {
		cancel()
		s.scheduleReconcile(record.Request.ID)
		return false
	}
	err := s.runner.StopLocal(effectCtx, identifier, controllerRequestForRecord(record))
	s.finishSideEffect(effect)
	cancel()
	if err != nil {
		s.setLocalCleanupRetry(record.Request.ID, true)
		persistErr := s.updateRecord(record.Request.ID, func(current *controllerWorkspaceRecord) bool {
			if current.Status != "ready" || current.LeaseID != record.LeaseID {
				return false
			}
			current.LocalCleanupPending = true
			current.Message = "workspace ready; local desktop cleanup failed; retrying"
			current.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
			return true
		})
		if persistErr == nil {
			if current, ok := s.workspace(record.Request.ID); ok && current.Status == "ready" && current.LocalCleanupPending {
				s.setLocalCleanupRetry(record.Request.ID, false)
			}
		}
		s.scheduleReconcile(record.Request.ID)
		return false
	}
	cleared := false
	if err := s.updateRecord(record.Request.ID, func(current *controllerWorkspaceRecord) bool {
		if current.Status != "ready" || current.LeaseID != record.LeaseID {
			return false
		}
		if !current.LocalCleanupPending {
			cleared = true
			return false
		}
		current.LocalCleanupPending = false
		current.Message = "workspace ready"
		current.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
		cleared = true
		return true
	}); err != nil {
		s.setLocalCleanupRetry(record.Request.ID, true)
		s.scheduleReconcile(record.Request.ID)
		return false
	}
	if _, terminalPending := s.terminalRevocationFor(record.Request.ID); terminalPending {
		// The bridge is gone, but durable state still says ready. Retain the
		// terminal barrier until the ready -> stopping transition itself persists.
		s.setLocalCleanupRetry(record.Request.ID, true)
		s.scheduleReconcile(record.Request.ID)
		return false
	}
	s.setLocalCleanupRetry(record.Request.ID, false)
	return cleared
}

func (s *controllerService) markReadyInspectionFailed(id string) {
	_ = s.updateRecord(id, func(current *controllerWorkspaceRecord) bool {
		if current.Status != "ready" {
			return false
		}
		current.Message = "workspace ready; restart inspection failed"
		current.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
		return true
	})
	if record, ok := s.workspace(id); ok && record.Status == "ready" {
		s.scheduleReadyReconcile(record)
	}
}

func (s *controllerService) cleanupReadyWorkspace(record controllerWorkspaceRecord, expiresAt, terminalStatus, terminalMessage string) {
	transitioned, err := s.transitionReadyWorkspaceForCleanup(record, expiresAt, terminalStatus, terminalMessage)
	if err != nil {
		s.revokeReadyDesktopAfterTransitionFailure(record, expiresAt, terminalStatus, terminalMessage)
		return
	}
	if transitioned {
		s.clearTerminalRevocation(record.Request.ID)
		s.setLocalCleanupRetry(record.Request.ID, false)
		if current, ok := s.workspace(record.Request.ID); ok {
			s.stopWorkspace(current)
		}
	}
}

func (s *controllerService) revokeReadyDesktopAfterTransitionFailure(record controllerWorkspaceRecord, expiresAt, terminalStatus, terminalMessage string) {
	// A failed state installation must gate provider release, but it must not
	// leave a controller-owned credential bridge alive. Keep the terminal intent
	// distinct from an ordinary bridge retry so reconciliation cannot inspect the
	// provider or recreate the bridge before stopping is durably installed.
	s.setTerminalRevocation(record.Request.ID, controllerTerminalRevocation{
		LeaseID:         record.LeaseID,
		ExpiresAt:       expiresAt,
		TerminalStatus:  terminalStatus,
		TerminalMessage: terminalMessage,
	})
	s.setLocalCleanupRetry(record.Request.ID, true)
	s.retryReadyLocalCleanup(record, true)
	if current, ok := s.workspace(record.Request.ID); ok && current.Status == "ready" {
		// Keep the memory barrier even after a successful local stop. Durable state
		// still says ready, so clearing it could let a new desktop request recreate
		// the bridge before the terminal transition is persisted.
		s.setLocalCleanupRetry(record.Request.ID, true)
		s.scheduleReconcile(record.Request.ID)
	} else {
		s.setLocalCleanupRetry(record.Request.ID, false)
	}
}

func (s *controllerService) transitionReadyWorkspaceForCleanup(record controllerWorkspaceRecord, expiresAt, terminalStatus, terminalMessage string) (bool, error) {
	transitioned := false
	err := s.updateRecord(record.Request.ID, func(current *controllerWorkspaceRecord) bool {
		if current.Status != "ready" || (record.LeaseID != "" && current.LeaseID != record.LeaseID) {
			return false
		}
		current.Status = "stopping"
		if current.LeaseID != "" {
			current.CreateObserved = true
		}
		current.StatusAfterCleanup = terminalStatus
		current.FailureAfterCleanup = terminalMessage
		current.ExpiresAt = firstNonBlank(expiresAt, current.ExpiresAt)
		current.Message = terminalMessage + "; cleanup requested"
		current.Host = ""
		current.AttachURL = ""
		current.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
		transitioned = true
		return true
	})
	return transitioned, err
}

func (s *controllerService) enqueueReadyWorkspaceCleanup(record controllerWorkspaceRecord, expiresAt, terminalStatus, terminalMessage string) {
	transitioned, err := s.transitionReadyWorkspaceForCleanup(record, expiresAt, terminalStatus, terminalMessage)
	if err != nil {
		s.revokeReadyDesktopAfterTransitionFailure(record, expiresAt, terminalStatus, terminalMessage)
		return
	}
	if transitioned {
		s.clearTerminalRevocation(record.Request.ID)
		s.setLocalCleanupRetry(record.Request.ID, false)
		s.enqueue(record.Request.ID)
	}
}

func (s *controllerService) cleanupProvisioningWorkspace(record controllerWorkspaceRecord, leaseID, terminalStatus, terminalMessage string) {
	transitioned := false
	err := s.updateRecord(record.Request.ID, func(current *controllerWorkspaceRecord) bool {
		if current.Status != "provisioning" {
			return false
		}
		current.Status = "stopping"
		if leaseID != "" && current.LeaseID != leaseID {
			return false
		}
		current.StatusAfterCleanup = terminalStatus
		current.FailureAfterCleanup = terminalMessage
		current.Message = terminalMessage + "; cleanup requested"
		current.AttachURL = ""
		current.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
		transitioned = true
		return true
	})
	if err != nil {
		s.scheduleReconcile(record.Request.ID)
		return
	}
	if transitioned {
		if current, ok := s.workspace(record.Request.ID); ok {
			s.stopWorkspace(current)
		}
	}
}

func (s *controllerService) persistAcquiredIdentity(id, attemptLeaseID, slug string, identity controllerAcquireIdentity) error {
	if err := validateControllerAcquireIdentity(identity); err != nil {
		return err
	}
	accepted := false
	var identityErr error
	err := s.updateRecord(id, func(current *controllerWorkspaceRecord) bool {
		if current.Status != "provisioning" && current.Status != "stopping" {
			identityErr = errControllerSideEffectObsolete
			return false
		}
		if current.AttemptLeaseID != attemptLeaseID || current.AttemptLeaseID != identity.LeaseID ||
			current.Slug != slug || current.Slug != identity.Slug || current.ProviderRoute != identity.Provider {
			identityErr = fmt.Errorf("raw acquire identity does not match the prepared controller identity")
			return false
		}
		if current.LeaseID != "" && current.LeaseID != identity.LeaseID {
			identityErr = fmt.Errorf("raw acquire lease identity changed")
			return false
		}
		if current.ProviderResourceID != "" && current.ProviderResourceID != identity.ResourceID {
			identityErr = fmt.Errorf("raw acquire provider resource identity changed")
			return false
		}
		accepted = true
		if current.LeaseID == identity.LeaseID && current.ProviderResourceID == identity.ResourceID && current.CreateObserved {
			return false
		}
		current.LeaseID = identity.LeaseID
		current.Provider = identity.Provider
		current.ProviderResourceID = identity.ResourceID
		current.CreateObserved = true
		current.Message = "workspace provisioning; provider identity acknowledged"
		if current.Status == "stopping" {
			current.Message = "workspace stopping"
		}
		current.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
		return true
	})
	if err != nil {
		return err
	}
	if identityErr != nil {
		return identityErr
	}
	if !accepted {
		return errControllerSideEffectObsolete
	}
	if !s.ensureDurableState(id) {
		return errControllerStateDurabilityPending
	}
	return nil
}

func (s *controllerService) reconcileProvisioning(record controllerWorkspaceRecord) {
	if !s.ensureDurableState(record.Request.ID) {
		s.scheduleReconcile(record.Request.ID)
		return
	}
	if strings.TrimSpace(record.AttemptLeaseID) == "" || strings.TrimSpace(record.Slug) == "" {
		if err := s.markProvisionFailed(record.Request.ID, "fixed attempt identity unavailable; cleanup not attempted"); err != nil {
			s.scheduleReconcile(record.Request.ID)
		}
		return
	}
	if controllerWorkspaceExpired(controllerEffectiveExpiry(record, ""), s.now()) {
		s.cleanupProvisioningWorkspace(record, record.LeaseID, "expired", "workspace TTL expired during provisioning")
		return
	}
	if record.CreateObserved {
		if !controllerRecordHasAcquiredIdentity(record) {
			if err := s.markProvisionFailed(record.Request.ID, "persisted provider acquire identity is incomplete; cleanup not attempted"); err != nil {
				s.scheduleReconcile(record.Request.ID)
			}
			return
		}
		identifier := firstNonBlank(record.LeaseID, record.Slug)
		inspectCtx, cancel := context.WithTimeout(s.ctx, s.opts.InspectTimeout)
		effectCtx, effect, beginErr := s.beginSideEffect(
			inspectCtx,
			record.Request.ID,
			false,
			true,
			true,
			func(candidate controllerWorkspaceRecord) bool {
				return candidate.Status == "provisioning" && candidate.AttemptLeaseID == record.AttemptLeaseID &&
					(candidate.CreatePrepared || candidate.CreateStarted)
			},
		)
		if beginErr != nil {
			cancel()
			if errors.Is(beginErr, errControllerStateDurabilityPending) {
				s.scheduleReconcile(record.Request.ID)
			}
			return
		}
		status, inspectErr := s.runner.Inspect(effectCtx, identifier, controllerRequestForRecord(record))
		s.finishSideEffect(effect)
		cancel()
		if inspectErr == nil && status.ID != "" {
			if !controllerStatusMatchesRecord(record, status) {
				if record.AttemptLeaseID == "" {
					if err := s.markProvisionFailed(record.Request.ID, "provider identity mismatch; fixed attempt identity unavailable; cleanup not attempted"); err != nil {
						s.scheduleReconcile(record.Request.ID)
					}
					return
				}
				s.finishProvisioningFailure(record, "", "provider identity mismatch; cleanup requested")
				return
			}
			_ = s.updateRecord(record.Request.ID, func(current *controllerWorkspaceRecord) bool {
				if strings.TrimSpace(status.ExpiresAt) != "" {
					current.ProviderExpiresAt = status.ExpiresAt
					current.ExpiresAt = controllerEffectiveExpiry(*current, "")
				}
				return true
			})
			if controllerWorkspaceExpired(controllerEffectiveExpiry(record, status.ExpiresAt), s.now()) {
				s.cleanupProvisioningWorkspace(record, status.ID, "expired", "workspace TTL expired during provisioning")
				return
			}
			if status.Ready {
				if err := s.markReady(record.Request.ID, status); err != nil {
					s.scheduleReconcile(record.Request.ID)
				}
				return
			}
			if statusTerminalState(status.State) || s.provisioningExpired(record) {
				s.finishProvisioningFailure(record, status.ID, "workspace did not become ready; cleanup requested")
				return
			}
			s.markProvisioningWaiting(record.Request.ID)
			s.scheduleReconcile(record.Request.ID)
			return
		}
		if inspectErr == nil || !controllerWorkspaceNotFound(inspectErr) {
			if s.provisioningExpired(record) {
				s.finishProvisioningFailure(record, "", "workspace recovery timed out; cleanup requested")
				return
			}
			s.markProvisioningWaiting(record.Request.ID)
			s.scheduleReconcile(record.Request.ID)
			return
		}
		if record.LeaseID != "" || record.CreateObserved {
			s.finishProvisioningFailure(record, record.LeaseID, "workspace disappeared during provisioning; cleanup requested")
			return
		}
		if !s.preparedCreateRecoveryElapsed(record) {
			s.markProvisioningWaiting(record.Request.ID)
			s.scheduleReconcile(record.Request.ID)
			return
		}
	}
	if (record.CreatePrepared || record.CreateStarted) && !s.preparedCreateRecoveryElapsed(record) {
		s.markProvisioningWaiting(record.Request.ID)
		s.scheduleReconcile(record.Request.ID)
		return
	}

	prepared := false
	if err := s.updateRecord(record.Request.ID, func(current *controllerWorkspaceRecord) bool {
		if current.Status != "provisioning" {
			return false
		}
		current.CreatePrepared = true
		current.CreatePreparedAt = s.now().UTC().Format(time.RFC3339Nano)
		current.CreateStarted = false
		current.CreateObserved = false
		current.Message = "workspace provisioning"
		current.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
		prepared = true
		return true
	}); err != nil {
		s.scheduleReconcile(record.Request.ID)
		return
	}
	if !prepared {
		return
	}
	if !s.ensureDurableState(record.Request.ID) {
		s.scheduleReconcile(record.Request.ID)
		return
	}

	createCtx, createOp, ok := s.beginCreateOperation(record)
	if !ok {
		return
	}
	effectCtx, effect, beginErr := s.beginSideEffect(
		createCtx,
		record.Request.ID,
		false,
		true,
		true,
		func(candidate controllerWorkspaceRecord) bool {
			return candidate.Status == "provisioning" && candidate.AttemptLeaseID == record.AttemptLeaseID &&
				candidate.CreatePrepared
		},
	)
	if beginErr != nil {
		s.finishCreateOperation(record.Request.ID, createOp)
		if errors.Is(beginErr, errControllerStateDurabilityPending) {
			s.scheduleReconcile(record.Request.ID)
		}
		return
	}
	acquiredIdentity, err := s.runner.Warmup(effectCtx, record.AttemptLeaseID, record.Slug, controllerRequestForRecord(record), func() error {
		launched := false
		err := s.updateRecord(record.Request.ID, func(current *controllerWorkspaceRecord) bool {
			if current.Status != "provisioning" {
				return false
			}
			current.CreateStarted = true
			current.Message = "workspace provisioning; provider command started"
			current.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
			launched = true
			return true
		})
		if err != nil {
			return err
		}
		if !launched {
			return fmt.Errorf("workspace no longer exists in controller state")
		}
		if !s.ensureDurableState(record.Request.ID) {
			return fmt.Errorf("controller state durability pending after provider command start")
		}
		return nil
	}, func(identity controllerAcquireIdentity) error {
		return s.persistAcquiredIdentity(record.Request.ID, record.AttemptLeaseID, record.Slug, identity)
	})
	s.finishSideEffect(effect)
	s.finishCreateOperation(record.Request.ID, createOp)
	if err != nil {
		if s.ctx.Err() != nil {
			return
		}
		if current, ok := s.workspace(record.Request.ID); ok && controllerRecordHasAcquiredIdentity(current) {
			if controllerWorkspaceExpired(controllerEffectiveExpiry(current, ""), s.now()) {
				s.cleanupProvisioningWorkspace(current, current.LeaseID, "expired", "workspace TTL expired during provisioning")
			} else {
				s.finishProvisioningFailure(current, current.LeaseID, "workspace provisioning failed; cleanup requested")
			}
		} else if current, ok := s.workspace(record.Request.ID); ok {
			if current.Status == "provisioning" {
				s.cleanupProvisioningWorkspace(current, "", "failed", "workspace provisioning failed before provider identity acknowledgment")
			} else if current.Status == "stopping" {
				s.stopWorkspace(current)
			}
		}
		return
	}
	current, ok := s.workspace(record.Request.ID)
	if !ok || !controllerRecordHasAcquiredIdentity(current) || acquiredIdentity.LeaseID != current.LeaseID || acquiredIdentity.ResourceID != current.ProviderResourceID {
		_ = s.markProvisionFailed(record.Request.ID, "workspace provider identity was not durably acknowledged")
		return
	}
	s.markProvisioningWaiting(record.Request.ID)
	s.scheduleReconcile(record.Request.ID)
}

func (s *controllerService) stopWorkspace(record controllerWorkspaceRecord) {
	identifier := firstNonBlank(record.LeaseID, record.AttemptLeaseID, record.Slug)
	if identifier == "" {
		_ = s.updateRecord(record.Request.ID, func(current *controllerWorkspaceRecord) bool {
			if current.Status != "stopping" {
				return false
			}
			current.Status = "stopping"
			current.Message = "workspace stop blocked; provider identity unavailable"
			current.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
			return true
		})
		return
	}
	var localErr error
	if !record.LocalDesktopStopped {
		localCtx, cancel := context.WithTimeout(s.ctx, s.opts.StopTimeout)
		effectCtx, effect, beginErr := s.beginSideEffect(
			localCtx,
			record.Request.ID,
			true,
			false,
			false,
			func(candidate controllerWorkspaceRecord) bool { return candidate.Status == "stopping" },
		)
		if beginErr == nil {
			localErr = s.runner.StopLocal(effectCtx, identifier, controllerRequestForRecord(record))
			s.finishSideEffect(effect)
		} else {
			localErr = beginErr
		}
		cancel()
		if localErr == nil {
			persisted := false
			localErr = s.updateRecord(record.Request.ID, func(current *controllerWorkspaceRecord) bool {
				if current.Status != "stopping" {
					return false
				}
				current.LocalDesktopStopped = true
				current.LocalCleanupPending = false
				current.Message = "local desktop revoked; stopping workspace provider"
				current.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
				persisted = true
				return true
			})
			if localErr == nil && !persisted {
				return
			}
		}
	}

	current, ok := s.workspace(record.Request.ID)
	if !ok || current.Status != "stopping" {
		return
	}
	record = current
	if !s.ensureDurableState(record.Request.ID) {
		s.scheduleReconcile(record.Request.ID)
		return
	}
	var providerErr error
	providerPending := false
	lateCreatePending := false
	if !record.ProviderStopped {
		absent, resetAbsentSince, confirmErr := s.confirmWorkspaceStopped(record, identifier)
		if errors.Is(confirmErr, errControllerWorkspaceNeedsIdentity) {
			recovered, recoveryErr := s.recoverStoppingWorkspaceIdentity(record)
			if recovered {
				if current, ok := s.workspace(record.Request.ID); ok {
					s.stopWorkspace(current)
				}
				return
			}
			confirmErr = errors.Join(confirmErr, recoveryErr)
		}
		if confirmErr != nil || !absent {
			providerErr = confirmErr
			if providerErr == nil {
				providerErr = fmt.Errorf("workspace remains in refreshed provider inventory after stop")
			}
			if err := s.updateRecord(record.Request.ID, func(current *controllerWorkspaceRecord) bool {
				if current.Status != "stopping" {
					return false
				}
				changed := false
				if current.ProviderAbsentSince != "" {
					current.ProviderAbsentSince = ""
					changed = true
				}
				if errors.Is(providerErr, errControllerWorkspaceStillPresent) && current.ProviderReleaseRequested {
					current.ProviderReleaseRequested = false
					changed = true
				}
				if !changed {
					return false
				}
				current.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
				return true
			}); err != nil {
				providerErr = errors.Join(providerErr, err)
			}
		} else {
			persisted := false
			stableAbsent := false
			providerErr = s.updateRecord(record.Request.ID, func(current *controllerWorkspaceRecord) bool {
				if current.Status != "stopping" {
					return false
				}
				now := s.now().UTC()
				absentSince, err := time.Parse(time.RFC3339Nano, current.ProviderAbsentSince)
				if resetAbsentSince || err != nil || absentSince.After(now) {
					absentSince = now
					current.ProviderAbsentSince = now.Format(time.RFC3339Nano)
				}
				notBefore, lateCreate := s.providerAbsenceNotBefore(*current, absentSince)
				if now.Before(notBefore) {
					providerPending = true
					lateCreatePending = lateCreate
					current.Message = "workspace provider absence confirmation pending; retrying"
					current.UpdatedAt = now.Format(time.RFC3339Nano)
					persisted = true
					return true
				}
				stableAbsent = true
				current.Message = "workspace provider absence stable; cleaning local provider state"
				current.UpdatedAt = now.Format(time.RFC3339Nano)
				persisted = true
				return true
			})
			if providerErr == nil && !persisted {
				return
			}
			if providerErr == nil && stableAbsent {
				providerErr = s.cleanupStableAbsentWorkspace(record.Request.ID, identifier)
			}
		}
	}
	current, ok = s.workspace(record.Request.ID)
	if !ok || current.Status != "stopping" {
		return
	}
	if current.LocalDesktopStopped && current.ProviderStopped {
		_ = s.markStopped(record.Request.ID)
		return
	}
	_ = s.updateRecord(record.Request.ID, func(pending *controllerWorkspaceRecord) bool {
		if pending.Status != "stopping" {
			return false
		}
		switch {
		case localErr != nil && providerErr != nil:
			pending.Message = "local desktop and workspace provider cleanup failed; retrying"
		case localErr != nil && providerPending:
			pending.Message = "local WebVNC cleanup failed; provider absence confirmation pending; retrying"
		case localErr != nil:
			pending.Message = "workspace provider stopped; local WebVNC cleanup failed; retrying"
		case providerErr != nil:
			pending.Message = "local desktop revoked; workspace provider stop or absence confirmation failed; retrying"
		case lateCreatePending:
			pending.Message = "local desktop revoked; waiting for late provider creation recovery window; retrying"
		case providerPending:
			pending.Message = "local desktop revoked; provider absence confirmation pending; retrying"
		default:
			pending.Message = "workspace cleanup incomplete; retrying"
		}
		pending.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
		return true
	})
	s.scheduleReconcile(record.Request.ID)
}

func (s *controllerService) confirmWorkspaceStopped(record controllerWorkspaceRecord, identifier string) (bool, bool, error) {
	confirmAbsent := func() (bool, error) {
		confirmCtx, cancel := context.WithTimeout(s.ctx, s.opts.InspectTimeout)
		effectCtx, effect, beginErr := s.beginSideEffect(
			confirmCtx,
			record.Request.ID,
			false,
			true,
			false,
			func(candidate controllerWorkspaceRecord) bool { return candidate.Status == "stopping" },
		)
		if beginErr != nil {
			cancel()
			return false, beginErr
		}
		request := controllerRequestForRecord(record)
		absent, err := s.runner.ConfirmAbsent(effectCtx, identifier, request)
		s.finishSideEffect(effect)
		cancel()
		return absent, err
	}
	if !controllerRecordHasAcquiredIdentity(record) {
		absent, err := confirmAbsent()
		if err != nil {
			return false, false, fmt.Errorf("refresh provider inventory without complete acquire identity: %w", err)
		}
		if absent {
			return true, false, nil
		}
		return false, false, errControllerWorkspaceNeedsIdentity
	}
	if record.ProviderReleaseRequested {
		absent, err := confirmAbsent()
		if err != nil {
			return false, false, fmt.Errorf("refresh provider inventory after stop: %w", err)
		}
		if absent {
			return true, false, nil
		}
		// The exact expected resource is present after a prior release attempt.
		// One new destructive request is now safe; another inventory error is not.
	}
	if !record.ProviderReleaseRequested {
		persisted := false
		if err := s.updateRecord(record.Request.ID, func(current *controllerWorkspaceRecord) bool {
			if current.Status != "stopping" {
				return false
			}
			current.ProviderReleaseRequested = true
			current.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
			persisted = true
			return true
		}); err != nil {
			return false, false, fmt.Errorf("persist provider release request: %w", err)
		}
		if !persisted {
			return false, false, errControllerSideEffectObsolete
		}
		if current, ok := s.workspace(record.Request.ID); ok {
			record = current
		}
	}
	var stopErr error
	stopCtx, cancel := context.WithTimeout(s.ctx, s.opts.StopTimeout)
	effectCtx, effect, beginErr := s.beginSideEffect(
		stopCtx,
		record.Request.ID,
		false,
		true,
		false,
		func(candidate controllerWorkspaceRecord) bool {
			return candidate.Status == "stopping" && candidate.ProviderReleaseRequested
		},
	)
	if beginErr == nil {
		stopErr = s.runner.Stop(effectCtx, identifier, controllerRequestForRecord(record))
		s.finishSideEffect(effect)
	} else {
		stopErr = beginErr
	}
	cancel()
	resetAbsentSince := true
	absent, confirmErr := confirmAbsent()
	if confirmErr == nil && absent {
		return true, resetAbsentSince, nil
	}
	if confirmErr == nil {
		confirmErr = errControllerWorkspaceStillPresent
	} else {
		confirmErr = fmt.Errorf("refresh provider inventory after stop: %w", confirmErr)
	}
	return false, resetAbsentSince, errors.Join(stopErr, confirmErr)
}

func (s *controllerService) cleanupStableAbsentWorkspace(id, identifier string) error {
	record, ok := s.workspace(id)
	if !ok || record.Status != "stopping" || record.ProviderStopped || record.ProviderAbsentSince == "" {
		return errControllerSideEffectObsolete
	}
	absentSince := record.ProviderAbsentSince
	cleanupCtx, cancel := context.WithTimeout(s.ctx, s.opts.StopTimeout)
	effectCtx, effect, beginErr := s.beginSideEffect(
		cleanupCtx,
		id,
		false,
		true,
		false,
		func(candidate controllerWorkspaceRecord) bool {
			return candidate.Status == "stopping" && !candidate.ProviderStopped &&
				candidate.ProviderAbsentSince == absentSince
		},
	)
	if beginErr != nil {
		cancel()
		return beginErr
	}
	cleanupErr := s.runner.CleanupAbsent(effectCtx, identifier, controllerRequestForRecord(record))
	s.finishSideEffect(effect)
	cancel()
	if cleanupErr != nil {
		return fmt.Errorf("cleanup local state after stable provider absence: %w", cleanupErr)
	}
	persisted := false
	if err := s.updateRecord(id, func(current *controllerWorkspaceRecord) bool {
		if current.Status != "stopping" || current.ProviderStopped || current.ProviderAbsentSince != absentSince {
			return false
		}
		current.ProviderStopped = true
		current.ProviderAbsentSince = ""
		current.Message = "workspace provider stopped; finishing local cleanup"
		current.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
		persisted = true
		return true
	}); err != nil {
		return err
	}
	if !persisted {
		return errControllerSideEffectObsolete
	}
	return nil
}

func (s *controllerService) recoverStoppingWorkspaceIdentity(record controllerWorkspaceRecord) (bool, error) {
	if controllerRecordHasAcquiredIdentity(record) {
		return true, nil
	}
	recoveryCtx, cancel := context.WithTimeout(s.ctx, s.opts.CreateTimeout)
	defer cancel()
	effectCtx, effect, beginErr := s.beginSideEffect(
		recoveryCtx,
		record.Request.ID,
		false,
		true,
		false,
		func(candidate controllerWorkspaceRecord) bool {
			return candidate.Status == "stopping" && !controllerRecordHasAcquiredIdentity(candidate)
		},
	)
	if beginErr != nil {
		return false, beginErr
	}
	_, warmupErr := s.runner.Warmup(
		effectCtx,
		record.AttemptLeaseID,
		record.Slug,
		controllerRequestForRecord(record),
		func() error {
			started := false
			err := s.updateRecord(record.Request.ID, func(current *controllerWorkspaceRecord) bool {
				if current.Status != "stopping" || controllerRecordHasAcquiredIdentity(*current) {
					return false
				}
				current.CreateStarted = true
				current.Message = "workspace stopping; recovering raw provider identity"
				current.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
				started = true
				return true
			})
			if err != nil {
				return err
			}
			if !started {
				return errControllerSideEffectObsolete
			}
			if !s.ensureDurableState(record.Request.ID) {
				return errControllerStateDurabilityPending
			}
			return nil
		},
		func(identity controllerAcquireIdentity) error {
			return s.persistAcquiredIdentity(record.Request.ID, record.AttemptLeaseID, record.Slug, identity)
		},
	)
	s.finishSideEffect(effect)
	current, ok := s.workspace(record.Request.ID)
	if ok && controllerRecordHasAcquiredIdentity(current) {
		return true, nil
	}
	if warmupErr == nil {
		warmupErr = fmt.Errorf("identity recovery warmup returned without an acknowledged raw provider identity")
	}
	return false, warmupErr
}

func (s *controllerService) providerAbsenceNotBefore(record controllerWorkspaceRecord, absentSince time.Time) (time.Time, bool) {
	notBefore := absentSince.Add(s.opts.RetryDelay)
	lateCreate := (record.CreatePrepared || record.CreateStarted) && !record.CreateObserved
	if !lateCreate {
		return notBefore, false
	}
	createStartedAt, err := time.Parse(time.RFC3339Nano, firstNonBlank(record.CreatePreparedAt, record.CreatedAt))
	if err != nil || createStartedAt.After(absentSince) {
		createStartedAt = absentSince
	}
	recoveryDeadline := createStartedAt.Add(s.opts.CreateTimeout)
	if recoveryDeadline.After(notBefore) {
		notBefore = recoveryDeadline
	}
	return notBefore, true
}

func (s *controllerService) markStopped(id string) error {
	return s.updateRecord(id, func(record *controllerWorkspaceRecord) bool {
		if record.FailureAfterCleanup != "" {
			record.Status = firstNonBlank(record.StatusAfterCleanup, "failed")
			record.Message = record.FailureAfterCleanup
			record.FailureAfterCleanup = ""
			record.StatusAfterCleanup = ""
		} else {
			record.Status = "stopped"
			record.Message = "workspace stopped"
		}
		record.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
		record.AttachURL = ""
		return true
	})
}

func (s *controllerService) markReady(id string, status StatusView) error {
	matched := false
	err := s.updateRecord(id, func(record *controllerWorkspaceRecord) bool {
		if !controllerStatusMatchesRecord(*record, status) {
			return false
		}
		matched = true
		// A concurrent DELETE owns the lifecycle transition. Preserve stopping
		// while recording the acquired identity for the queued stop.
		stopping := record.Status == "stopping"
		record.Status = "ready"
		record.Provider = record.ProviderRoute
		record.Host = firstNonBlank(status.Host, status.SSHHost)
		if strings.TrimSpace(status.ExpiresAt) != "" {
			record.ProviderExpiresAt = status.ExpiresAt
		}
		record.ExpiresAt = controllerEffectiveExpiry(*record, "")
		record.Message = "workspace ready"
		record.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
		record.AttachURL = expandControllerURLTemplate(s.opts.AttachURLTemplate, *record)
		if stopping {
			record.Status = "stopping"
			record.Message = "workspace stopping"
			record.AttachURL = ""
		}
		return true
	})
	if err != nil {
		return err
	}
	if !matched {
		return fmt.Errorf("provider identity changed before ready adoption")
	}
	if record, ok := s.workspace(id); ok && record.Status == "ready" {
		s.scheduleReadyReconcile(record)
	}
	return nil
}

func (s *controllerService) markFailed(id, message string) error {
	return s.updateRecord(id, func(record *controllerWorkspaceRecord) bool {
		record.Status = "failed"
		record.Message = message
		record.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
		return true
	})
}

func (s *controllerService) markProvisionFailed(id, message string) error {
	return s.updateRecord(id, func(record *controllerWorkspaceRecord) bool {
		if record.Status == "stopping" {
			return false
		}
		record.Status = "failed"
		record.Message = message
		record.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
		return true
	})
}

func (s *controllerService) markProvisioningWaiting(id string) {
	_ = s.updateRecord(id, func(record *controllerWorkspaceRecord) bool {
		if record.Status != "provisioning" {
			return false
		}
		record.Message = "workspace provisioning; waiting for provider"
		record.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
		return true
	})
}

func (s *controllerService) finishProvisioningFailure(record controllerWorkspaceRecord, leaseID, message string) {
	shouldStop := false
	err := s.updateRecord(record.Request.ID, func(current *controllerWorkspaceRecord) bool {
		if leaseID != "" && current.LeaseID != leaseID {
			return false
		}
		switch current.Status {
		case "provisioning":
			current.Status = "stopping"
			current.Message = message
			current.FailureAfterCleanup = message
			current.StatusAfterCleanup = "failed"
			current.AttachURL = ""
			current.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
			shouldStop = true
			return true
		case "stopping":
			// A concurrent DELETE owns the terminal state; only carry forward
			// the provider identity needed by its cleanup.
			shouldStop = true
			return leaseID != "" && controllerRecordHasAcquiredIdentity(*current)
		default:
			return false
		}
	})
	if err != nil {
		s.scheduleReconcile(record.Request.ID)
		return
	}
	if !shouldStop {
		return
	}
	if current, ok := s.workspace(record.Request.ID); ok {
		s.stopWorkspace(current)
	}
}

func (s *controllerService) provisioningExpired(record controllerWorkspaceRecord) bool {
	if controllerWorkspaceExpired(controllerEffectiveExpiry(record, ""), s.now()) {
		return true
	}
	attemptedAt, err := time.Parse(time.RFC3339Nano, firstNonBlank(record.CreatePreparedAt, record.CreatedAt))
	return err == nil && !s.now().Before(attemptedAt.Add(s.opts.CreateTimeout))
}

func (s *controllerService) provisioningOperationTimeout(record controllerWorkspaceRecord) time.Duration {
	timeout := s.opts.CreateTimeout
	if expiresAt, ok := parseLeaseLabelTime(controllerEffectiveExpiry(record, "")); ok {
		remaining := expiresAt.Sub(s.now())
		if remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		return time.Nanosecond
	}
	return timeout
}

func (s *controllerService) preparedCreateRecoveryElapsed(record controllerWorkspaceRecord) bool {
	preparedAt, err := time.Parse(time.RFC3339Nano, firstNonBlank(record.CreatePreparedAt, record.UpdatedAt, record.CreatedAt))
	return err == nil && !s.now().Before(preparedAt.Add(s.opts.CreateTimeout))
}

func controllerWorkspaceExpired(value string, now time.Time) bool {
	expiresAt, ok := parseLeaseLabelTime(value)
	return ok && !now.Before(expiresAt)
}

func controllerRequestedExpiresAt(createdAt time.Time, ttlSeconds int) string {
	if ttlSeconds <= 0 {
		return ""
	}
	return createdAt.Add(time.Duration(ttlSeconds) * time.Second).UTC().Format(time.RFC3339Nano)
}

func controllerEffectiveExpiry(record controllerWorkspaceRecord, providerExpiresAt string) string {
	providerExpiresAt = firstNonBlank(providerExpiresAt, record.ProviderExpiresAt)
	values := []string{record.ControllerExpiresAt, providerExpiresAt}
	var earliest time.Time
	for _, value := range values {
		parsed, ok := parseLeaseLabelTime(value)
		if !ok || (!earliest.IsZero() && !parsed.Before(earliest)) {
			continue
		}
		earliest = parsed
	}
	if !earliest.IsZero() {
		return earliest.UTC().Format(time.RFC3339Nano)
	}
	return firstNonBlank(providerExpiresAt, record.ControllerExpiresAt, record.ExpiresAt)
}

func (s *controllerService) scheduleReconcile(id string) {
	s.scheduleReconcileAfter(id, s.opts.RetryDelay, "")
}

func (s *controllerService) scheduleReadyReconcile(record controllerWorkspaceRecord) {
	delay := s.opts.ReadyReconcileInterval
	if expiresAt, ok := parseLeaseLabelTime(record.ExpiresAt); ok {
		untilExpiry := expiresAt.Sub(s.now())
		if untilExpiry < delay {
			delay = untilExpiry
		}
	}
	if delay < 0 {
		delay = 0
	}
	s.scheduleReconcileAfter(record.Request.ID, delay, "ready")
}

func (s *controllerService) scheduleReconcileAfter(id string, delay time.Duration, requiredStatus string) {
	if delay < 0 {
		delay = 0
	}
	scheduled := &controllerReconcileTimer{}
	s.mu.Lock()
	s.cancelReconcileTimerLocked(id)
	record, ok := s.state.Workspaces[id]
	if s.ctx.Err() != nil || !ok || !controllerStatusNeedsReconcile(record.Status) || (requiredStatus != "" && record.Status != requiredStatus) {
		s.mu.Unlock()
		return
	}
	scheduled.timer = time.AfterFunc(delay, func() {
		s.mu.Lock()
		current, currentTimer := s.reconcileTimers[id]
		if !currentTimer || current != scheduled {
			s.mu.Unlock()
			return
		}
		delete(s.reconcileTimers, id)
		record, exists := s.state.Workspaces[id]
		shouldReconcile := exists && controllerStatusNeedsReconcile(record.Status) &&
			(requiredStatus == "" || record.Status == requiredStatus) && s.ctx.Err() == nil
		s.mu.Unlock()
		if shouldReconcile {
			s.enqueue(id)
		}
	})
	s.reconcileTimers[id] = scheduled
	s.mu.Unlock()
}

func (s *controllerService) cancelReconcileTimerLocked(id string) {
	scheduled, ok := s.reconcileTimers[id]
	if !ok {
		return
	}
	delete(s.reconcileTimers, id)
	scheduled.timer.Stop()
}

func controllerStatusNeedsReconcile(status string) bool {
	switch status {
	case "provisioning", "ready", "stopping":
		return true
	default:
		return false
	}
}

func (s *controllerService) workspace(id string) (controllerWorkspaceRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.state.Workspaces[id]
	return record, ok
}

func controllerRequestForRecord(record controllerWorkspaceRecord) controllerWorkspaceRequest {
	request := record.Request
	request.ProviderRoute = record.ProviderRoute
	request.ProviderScope = record.ProviderScope
	request.ProviderLeaseID = record.LeaseID
	request.ProviderAttemptLeaseID = record.AttemptLeaseID
	request.ProviderSlug = record.Slug
	request.ProviderResourceID = record.ProviderResourceID
	request.CoordinatorRegistrationURL = record.CoordinatorRegistrationURL
	return request
}

func (s *controllerService) updateRecord(id string, update func(*controllerWorkspaceRecord) bool) error {
	s.sideEffectGate.Lock()
	defer s.sideEffectGate.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.state.Workspaces[id]
	if !ok || !update(&record) {
		return nil
	}
	previous := s.state.Workspaces[id]
	s.state.Workspaces[id] = record
	if err := s.saveState(s.opts.StateFile, s.state); err != nil {
		if !s.recordStateSaveResultUnderGateLocked(id, err) {
			s.state.Workspaces[id] = previous
			if s.log != nil {
				fmt.Fprintf(s.log, "controller state write failed workspace=%s: %v\n", id, err)
			}
			return err
		}
	} else {
		s.recordStateSaveResultUnderGateLocked(id, nil)
	}
	if record.Status == "stopping" {
		s.cancelTransitionSideEffectsUnderGate(id, errControllerWorkspaceStopping)
		s.cancelCreateOperationLocked(id)
	}
	if !controllerStatusNeedsReconcile(record.Status) {
		s.cancelReconcileTimerLocked(id)
	}
	return nil
}

func controllerResponse(record controllerWorkspaceRecord) controllerWorkspaceResponse {
	return controllerWorkspaceResponse{
		ID:                 record.Request.ID,
		Status:             record.Status,
		LeaseID:            record.LeaseID,
		Slug:               record.Slug,
		Provider:           record.Provider,
		ProviderResourceID: record.ProviderResourceID,
		Host:               record.Host,
		AttachURL:          record.AttachURL,
		Message:            record.Message,
		Capabilities: controllerWorkspaceResponseCapabilities{
			Terminal: record.AttachURL != "",
			VNC:      record.Request.Capabilities.Desktop,
			Desktop:  record.Request.Capabilities.Desktop,
		},
		ExpiresAt: record.ExpiresAt,
		CreatedAt: record.CreatedAt,
		UpdatedAt: record.UpdatedAt,
	}
}

func normalizeControllerWorkspaceRequest(request controllerWorkspaceRequest) controllerWorkspaceRequest {
	request.ID = strings.TrimSpace(request.ID)
	request.Repo = strings.TrimSpace(request.Repo)
	request.Branch = strings.TrimSpace(request.Branch)
	request.Runtime = strings.TrimSpace(request.Runtime)
	request.Profile = strings.TrimSpace(request.Profile)
	request.Class = strings.TrimSpace(request.Class)
	request.ServerType = strings.TrimSpace(request.ServerType)
	request.Owner = strings.TrimSpace(request.Owner)
	request.CreatedBy = strings.TrimSpace(request.CreatedBy)
	request.ParentSessionID = strings.TrimSpace(request.ParentSessionID)
	request.RootSessionID = strings.TrimSpace(request.RootSessionID)
	return request
}

func validateControllerWorkspaceRequest(request controllerWorkspaceRequest, opts controllerServiceOptions) error {
	if !validControllerWorkspaceID(request.ID) {
		return fmt.Errorf("id must be a lowercase DNS-style name with at most 63 characters")
	}
	for label, value := range map[string]string{
		"repo": request.Repo, "branch": request.Branch, "runtime": request.Runtime,
		"profile": request.Profile, "class": request.Class, "serverType": request.ServerType,
		"owner": request.Owner, "createdBy": request.CreatedBy,
		"parentSessionId": request.ParentSessionID, "rootSessionId": request.RootSessionID,
	} {
		if len(value) > 512 {
			return fmt.Errorf("%s must be 512 bytes or fewer", label)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%s contains a NUL byte", label)
		}
	}
	for label, value := range map[string]string{
		"command": request.Command, "prompt": request.Prompt, "purpose": request.Purpose, "summary": request.Summary,
	} {
		if len(value) > 16<<10 {
			return fmt.Errorf("%s must be 16 KiB or fewer", label)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%s contains a NUL byte", label)
		}
	}
	if opts.Profile != "" && request.Profile != "" && request.Profile != opts.Profile {
		return fmt.Errorf("profile must be %q", opts.Profile)
	}
	if opts.Profile == "" && request.Profile != "" {
		return fmt.Errorf("profile selection is disabled by this controller")
	}
	if opts.ForbidClassOverride && request.Class != "" {
		return fmt.Errorf("class override is disabled by this controller")
	}
	if opts.ForbidServerTypeOverride && request.ServerType != "" {
		return fmt.Errorf("serverType override is disabled by this controller")
	}
	if request.Capabilities.Desktop && !opts.Allowed.Desktop {
		return fmt.Errorf("desktop capability is disabled by this controller")
	}
	if request.Capabilities.Browser && !opts.Allowed.Browser {
		return fmt.Errorf("browser capability is disabled by this controller")
	}
	if request.Capabilities.Code && !opts.Allowed.Code {
		return fmt.Errorf("code capability is disabled by this controller")
	}
	if err := validateControllerLeaseSeconds("ttlSeconds", request.TTLSeconds); err != nil {
		return err
	}
	if err := validateControllerLeaseSeconds("idleTimeoutSeconds", request.IdleTimeoutSeconds); err != nil {
		return err
	}
	if request.TTLSeconds > 0 && request.IdleTimeoutSeconds > request.TTLSeconds {
		return fmt.Errorf("idleTimeoutSeconds must not exceed ttlSeconds")
	}
	if opts.RequiredTTLSeconds > 0 && request.TTLSeconds != opts.RequiredTTLSeconds {
		return fmt.Errorf("ttlSeconds must equal controller policy value %d", opts.RequiredTTLSeconds)
	}
	if opts.RequiredIdleSeconds > 0 && request.IdleTimeoutSeconds != opts.RequiredIdleSeconds {
		return fmt.Errorf("idleTimeoutSeconds must equal controller policy value %d", opts.RequiredIdleSeconds)
	}
	return nil
}

func validateControllerLeaseSeconds(name string, value int) error {
	if value == 0 {
		return nil
	}
	if value < 60 || value > 7*24*60*60 {
		return fmt.Errorf("%s must be between 60 and 604800", name)
	}
	return nil
}

func validControllerWorkspaceID(id string) bool {
	return controllerWorkspaceIDPattern.MatchString(id)
}

func controllerStatusMatchesRecord(record controllerWorkspaceRecord, status StatusView) bool {
	if !controllerRecordHasAcquiredIdentity(record) {
		return false
	}
	return status.ID == record.LeaseID &&
		status.ID == record.AttemptLeaseID &&
		status.Slug == record.Slug &&
		status.Provider == record.ProviderRoute &&
		status.ServerID == record.ProviderResourceID
}

func controllerRecordHasAcquiredIdentity(record controllerWorkspaceRecord) bool {
	identity := controllerAcquireIdentity{
		LeaseID:    record.LeaseID,
		Slug:       record.Slug,
		Provider:   record.ProviderRoute,
		ResourceID: record.ProviderResourceID,
	}
	return record.CreateObserved && record.LeaseID == record.AttemptLeaseID && validateControllerAcquireIdentity(identity) == nil
}

func newControllerProvisioningSlug(id string) (string, error) {
	var entropy [8]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate provisioning identity: %w", err)
	}
	const prefix = "cbx-ctl-"
	suffix := "-" + hex.EncodeToString(entropy[:])
	id = normalizeLeaseSlug(id)
	maxID := maxRequestedLeaseSlugLength - len(prefix) - len(suffix)
	if len(id) > maxID {
		id = strings.TrimRight(id[:maxID], "-")
	}
	slug, err := requestedLeaseSlug(prefix + id + suffix)
	if err != nil {
		return "", fmt.Errorf("normalize provisioning identity: %w", err)
	}
	return slug, nil
}

func decodeControllerJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, controllerMaxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("request body must contain one JSON object")
		}
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}

func writeControllerJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func writeControllerError(w http.ResponseWriter, status int, code, message string) {
	writeControllerJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeControllerMethodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeControllerError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

func loadControllerState(path string) (controllerState, error) {
	state := controllerState{Version: controllerStateVersion, Workspaces: map[string]controllerWorkspaceRecord{}}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return controllerState{}, fmt.Errorf("stat controller state: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return controllerState{}, fmt.Errorf("controller state file must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return controllerState{}, fmt.Errorf("controller state file %s must not be accessible by group or others", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return controllerState{}, fmt.Errorf("read controller state: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return controllerState{}, fmt.Errorf("parse controller state: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return controllerState{}, fmt.Errorf("parse controller state: trailing data")
	}
	if state.Version != controllerStateVersion {
		return controllerState{}, fmt.Errorf("unsupported controller state version %d", state.Version)
	}
	if state.Workspaces == nil {
		state.Workspaces = map[string]controllerWorkspaceRecord{}
	}
	if err := validateControllerStateRecords(state); err != nil {
		return controllerState{}, err
	}
	return state, nil
}

func validateControllerStateRecords(state controllerState) error {
	for id, record := range state.Workspaces {
		if id != record.Request.ID || !validControllerWorkspaceID(id) {
			return fmt.Errorf("controller state contains invalid workspace %q", id)
		}
		switch record.Status {
		case "provisioning", "ready", "stopping", "failed", "expired", "stopped":
		default:
			return fmt.Errorf("controller state workspace %s has invalid status %q", id, record.Status)
		}
		validationOpts := controllerServiceOptions{
			Allowed: controllerCapabilities{Desktop: true, Browser: true, Code: true},
			Profile: record.Request.Profile,
		}
		if err := validateControllerWorkspaceRequest(record.Request, validationOpts); err != nil {
			return fmt.Errorf("controller state workspace %s request: %w", id, err)
		}
		for name, value := range map[string]string{"createdAt": record.CreatedAt, "updatedAt": record.UpdatedAt} {
			if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
				return fmt.Errorf("controller state workspace %s has invalid %s", id, name)
			}
		}
		if controllerStatusNeedsReconcile(record.Status) && (strings.TrimSpace(record.ProviderRoute) == "" || strings.TrimSpace(record.ProviderScope) == "") {
			return fmt.Errorf("controller workspace %s has no immutable provider route and scope", id)
		}
		if err := validateControllerCoordinatorRegistrationURL(record.CoordinatorRegistrationURL); err != nil {
			return fmt.Errorf("controller state workspace %s has invalid coordinator registration binding: %w", id, err)
		}
		for name, value := range map[string]string{"leaseId": record.LeaseID, "attemptLeaseId": record.AttemptLeaseID} {
			if value != "" && (value != strings.TrimSpace(value) || !validLeaseClaimID(value)) {
				return fmt.Errorf("controller state workspace %s has invalid %s", id, name)
			}
		}
		if record.Slug != "" && (record.Slug != strings.TrimSpace(record.Slug) || normalizeLeaseSlug(record.Slug) != record.Slug) {
			return fmt.Errorf("controller state workspace %s has invalid slug", id)
		}
		if record.ProviderResourceID != "" && (record.ProviderResourceID != strings.TrimSpace(record.ProviderResourceID) || !validControllerInventoryIdentity(record.ProviderResourceID)) {
			return fmt.Errorf("controller state workspace %s has invalid providerResourceId", id)
		}
		if record.AttachURL != "" {
			if err := validateControllerTerminalConnectionURL(record.AttachURL); err != nil {
				return fmt.Errorf("controller state workspace %s has invalid attachUrl: %w", id, err)
			}
		}
		if record.Status == "provisioning" && (strings.TrimSpace(record.AttemptLeaseID) == "" || strings.TrimSpace(record.Slug) == "") {
			return fmt.Errorf("controller state workspace %s provisioning identity is incomplete", id)
		}
		if record.Status == "ready" && !controllerRecordHasAcquiredIdentity(record) {
			return fmt.Errorf("controller state workspace %s ready identity is incomplete", id)
		}
		if record.StatusAfterCleanup != "" {
			switch record.StatusAfterCleanup {
			case "failed", "expired", "stopped":
			default:
				return fmt.Errorf("controller state workspace %s has invalid cleanup status %q", id, record.StatusAfterCleanup)
			}
		}
	}
	return nil
}

type controllerStateInstalledError struct {
	err error
}

func (e *controllerStateInstalledError) Error() string {
	return fmt.Sprintf("controller state installed with indeterminate durability: %v", e.err)
}

func (e *controllerStateInstalledError) Unwrap() error {
	return e.err
}

func controllerStateInstalled(err error) bool {
	var installed *controllerStateInstalledError
	return errors.As(err, &installed)
}

func saveControllerState(path string, state controllerState) error {
	return saveControllerStateWithDirectorySync(path, state, syncControllerDirectory)
}

func saveControllerStateWithDirectorySync(path string, state controllerState, syncDirectory func(string) error) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode controller state: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := ensureControllerStateDirectoryWithSync(dir, syncDirectory); err != nil {
		return fmt.Errorf("create controller state directory: %w", err)
	}
	file, err := os.CreateTemp(dir, ".controller-state-*.json")
	if err != nil {
		return fmt.Errorf("create controller state file: %w", err)
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure controller state file: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write controller state: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync controller state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close controller state: %w", err)
	}
	if err := replaceControllerFile(tmp, path); err != nil {
		return fmt.Errorf("install controller state: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return &controllerStateInstalledError{err: fmt.Errorf("sync controller state directory: %w", err)}
	}
	return nil
}

func ensureControllerStateDirectory(dir string) error {
	return ensureControllerStateDirectoryWithSync(dir, syncControllerDirectory)
}

func ensureControllerStateDirectoryWithSync(dir string, syncDirectory func(string) error) error {
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := validateControllerStateDirectoryPath(dir); err != nil {
		return err
	}
	for current := dir; ; {
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		if err := syncDirectory(parent); err != nil {
			return err
		}
		current = parent
	}
	return nil
}

func validateControllerURLTemplate(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	cleaned := value
	for _, placeholder := range []string{"{workspaceId}", "{leaseId}", "{slug}"} {
		cleaned = strings.ReplaceAll(cleaned, placeholder, "example")
	}
	if strings.Contains(cleaned, "{") || strings.Contains(cleaned, "}") {
		return fmt.Errorf("only {workspaceId}, {leaseId}, and {slug} placeholders are supported")
	}
	u, err := url.Parse(cleaned)
	if err != nil {
		return fmt.Errorf("URL template is invalid")
	}
	if u.Fragment != "" {
		return fmt.Errorf("URL template must not contain a fragment")
	}
	if u.RawQuery != "" {
		return fmt.Errorf("URL template must not contain a query string")
	}
	if err := validateControllerConnectionURL(cleaned); err != nil {
		return err
	}
	return nil
}

func validateControllerTerminalURLTemplate(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	cleaned := value
	for _, placeholder := range []string{"{workspaceId}", "{leaseId}", "{slug}"} {
		cleaned = strings.ReplaceAll(cleaned, placeholder, "example")
	}
	if strings.Contains(cleaned, "{") || strings.Contains(cleaned, "}") {
		return fmt.Errorf("only {workspaceId}, {leaseId}, and {slug} placeholders are supported")
	}
	u, err := url.Parse(cleaned)
	if err != nil {
		return fmt.Errorf("URL template is invalid")
	}
	if u.Fragment != "" {
		return fmt.Errorf("URL template must not contain a fragment")
	}
	if u.RawQuery != "" {
		return fmt.Errorf("URL template must not contain a query string")
	}
	return validateControllerTerminalConnectionURL(cleaned)
}

func validateControllerTerminalConnectionURL(value string) error {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Host == "" {
		return fmt.Errorf("terminal connection returned an invalid URL")
	}
	if u.User != nil {
		return fmt.Errorf("terminal connection URL must not contain user information")
	}
	if u.Scheme == "wss" {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme == "ws" && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
		return nil
	}
	return fmt.Errorf("terminal connection URL must use WSS or loopback WS")
}

func validateControllerConnectionURL(value string) error {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Host == "" {
		return fmt.Errorf("desktop connection returned an invalid URL")
	}
	if u.User != nil {
		return fmt.Errorf("desktop connection URL must not contain user information")
	}
	if u.Scheme == "https" {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
		return nil
	}
	return fmt.Errorf("desktop connection URL must use HTTPS or loopback HTTP")
}

func expandControllerURLTemplate(value string, record controllerWorkspaceRecord) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	replacer := strings.NewReplacer(
		"{workspaceId}", url.PathEscape(record.Request.ID),
		"{leaseId}", url.PathEscape(record.LeaseID),
		"{slug}", url.PathEscape(record.Slug),
	)
	return replacer.Replace(value)
}
