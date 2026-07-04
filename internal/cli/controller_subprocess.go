package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type execControllerRunnerOptions struct {
	Binary    string
	Config    string
	Provider  string
	WorkDir   string
	StateFile string
	AdapterID string
}

type execControllerWorkspaceRunner struct {
	opts      execControllerRunnerOptions
	stateMu   sync.RWMutex
	stateFile string
}

const controllerChildIdentityVersion = 1

const controllerChildWaitDelay = 250 * time.Millisecond

const controllerDesktopRollbackTimeout = 10 * time.Second

const controllerProcessTreeOwnedEnv = "CRABBOX_CONTROLLER_PROCESS_TREE_OWNED"

type controllerChildIdentity struct {
	Version        int    `json:"version"`
	PID            int    `json:"pid"`
	ProcessStarted string `json:"processStarted"`
	BootID         string `json:"bootId,omitempty"`
	Nonce          string `json:"nonce"`
	WorkspaceID    string `json:"workspaceId"`
	Operation      string `json:"operation"`
}

func (r *execControllerWorkspaceRunner) ProviderIdentity(ctx context.Context) (controllerProviderIdentity, error) {
	var output controllerLimitedBuffer
	output.limit = 1 << 20
	args := []string{"config", "show", "--json", "--controller-provider-identity"}
	if provider := strings.TrimSpace(r.opts.Provider); provider != "" {
		args = append(args, "--provider", provider)
	}
	if err := r.runWithStarted(ctx, controllerWorkspaceRequest{}, args, &output, nil); err != nil {
		return controllerProviderIdentity{}, fmt.Errorf("resolve controller provider identity: %w", err)
	}
	if err := output.overflowError("controller provider identity"); err != nil {
		return controllerProviderIdentity{}, err
	}
	var view struct {
		Provider                   string `json:"provider"`
		ProviderScope              string `json:"providerScope"`
		IdempotentLeaseID          bool   `json:"idempotentLeaseId"`
		CoordinatorRegistrationURL string `json:"coordinatorRegistrationUrl"`
	}
	if err := json.Unmarshal(output.Bytes(), &view); err != nil {
		return controllerProviderIdentity{}, fmt.Errorf("decode controller provider identity: %w", err)
	}
	provider := strings.TrimSpace(view.Provider)
	if provider == "" {
		return controllerProviderIdentity{}, fmt.Errorf("controller provider route is empty")
	}
	if strings.TrimSpace(view.ProviderScope) == "" {
		return controllerProviderIdentity{}, fmt.Errorf("controller provider scope is empty")
	}
	registrationURL := strings.TrimSpace(view.CoordinatorRegistrationURL)
	if err := validateControllerCoordinatorRegistrationURL(registrationURL); err != nil {
		return controllerProviderIdentity{}, fmt.Errorf("decode controller coordinator registration binding: %w", err)
	}
	return controllerProviderIdentity{Route: provider, Scope: strings.TrimSpace(view.ProviderScope), IdempotentFixedLeaseID: view.IdempotentLeaseID, CoordinatorRegistrationURL: registrationURL}, nil
}

func (r *execControllerWorkspaceRunner) Warmup(
	ctx context.Context,
	attemptLeaseID, slug string,
	request controllerWorkspaceRequest,
	onStarted func() error,
	onAcquired func(controllerAcquireIdentity) error,
) (controllerAcquireIdentity, error) {
	gate, err := newControllerAcquireIdentityGate(onAcquired)
	if err != nil {
		return controllerAcquireIdentity{}, err
	}
	var output controllerLimitedBuffer
	output.limit = 1 << 20
	warmupEnv := gate.environment()
	warmupEnv[controllerCoordinatorRegistrationExpectedEnv] = "1"
	warmupEnv[controllerCoordinatorRegistrationURLEnv] = request.CoordinatorRegistrationURL
	runErr := r.runWithStartedEnv(ctx, request, r.warmupArgs(attemptLeaseID, slug, request), &output, onStarted, warmupEnv)
	gate.close()
	identityResult := gate.wait()
	return identityResult.Identity, errors.Join(runErr, output.overflowError("controller provider warmup"), identityResult.Err)
}

func (r *execControllerWorkspaceRunner) warmupArgs(attemptLeaseID, slug string, request controllerWorkspaceRequest) []string {
	args := []string{"warmup", "--keep=true", "--lease-id", attemptLeaseID, "--slug", slug}
	args = r.appendProviderArg(args, request)
	// The persisted request is the routing contract. Controller flags may
	// change across restarts, but an existing attempt's profile must not.
	profile := strings.TrimSpace(request.Profile)
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	if request.Class != "" {
		args = append(args, "--class", request.Class)
	}
	if request.ServerType != "" {
		args = append(args, "--type", request.ServerType)
	}
	if request.TTLSeconds > 0 {
		args = append(args, "--ttl", strconv.Itoa(request.TTLSeconds)+"s")
	}
	if request.IdleTimeoutSeconds > 0 {
		args = append(args, "--idle-timeout", strconv.Itoa(request.IdleTimeoutSeconds)+"s")
	}
	if request.Capabilities.Desktop {
		args = append(args, "--desktop=true")
	}
	if request.Capabilities.Browser {
		args = append(args, "--browser=true")
	}
	if request.Capabilities.Code {
		args = append(args, "--code=true")
	}
	return args
}

func (r *execControllerWorkspaceRunner) Inspect(ctx context.Context, identifier string, request controllerWorkspaceRequest) (StatusView, error) {
	args := []string{"inspect", "--id", identifier, "--json"}
	args, err := r.appendPersistedProviderRoutingArgs(args, request)
	if err != nil {
		return StatusView{}, err
	}
	var output controllerLimitedBuffer
	output.limit = 1 << 20
	if err := r.run(ctx, request, args, &output); err != nil {
		absent, confirmErr := r.workspaceAbsent(ctx, identifier, request)
		if confirmErr == nil && absent {
			return StatusView{}, &controllerWorkspaceNotFoundError{Identifier: identifier}
		}
		if confirmErr != nil {
			return StatusView{}, errors.Join(err, fmt.Errorf("confirm workspace absence: %w", confirmErr))
		}
		return StatusView{}, err
	}
	if err := output.overflowError("controller provider inspection"); err != nil {
		return StatusView{}, err
	}
	var status StatusView
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	if err := decoder.Decode(&status); err != nil {
		return StatusView{}, fmt.Errorf("decode crabbox inspect JSON: %w", err)
	}
	return status, nil
}

func (r *execControllerWorkspaceRunner) Stop(ctx context.Context, identifier string, request controllerWorkspaceRequest) error {
	args := []string{
		"stop", "--id", identifier,
		"--expected-provider-lease-id", request.ProviderLeaseID,
		"--expected-provider-attempt-lease-id", request.ProviderAttemptLeaseID,
		"--expected-provider-slug", request.ProviderSlug,
		"--expected-provider-resource-id", request.ProviderResourceID,
		"--expected-provider-scope", request.ProviderScope,
	}
	args, err := r.appendPersistedProviderRoutingArgs(args, request)
	if err != nil {
		return err
	}
	err = r.run(ctx, request, args, io.Discard)
	if err != nil {
		absent, confirmErr := r.workspaceAbsent(ctx, identifier, request)
		if confirmErr == nil && absent {
			// The controller owns local claim/routing cleanup. A single absent
			// inventory result only makes provider release idempotent; cleanup is
			// deferred until the service observes stable absence.
			err = nil
		} else if confirmErr != nil {
			err = errors.Join(err, fmt.Errorf("confirm workspace absence after stop: %w", confirmErr))
		}
	}
	if err != nil {
		return err
	}
	return nil
}

func (r *execControllerWorkspaceRunner) ConfirmAbsent(ctx context.Context, identifier string, request controllerWorkspaceRequest) (bool, error) {
	return r.workspaceAbsent(ctx, identifier, request)
}

func (r *execControllerWorkspaceRunner) CleanupAbsent(ctx context.Context, identifier string, request controllerWorkspaceRequest) error {
	args := []string{
		"stop", "--confirmed-absent-local-cleanup=true", "--id", identifier,
		"--expected-provider-lease-id", request.ProviderLeaseID,
		"--expected-provider-attempt-lease-id", request.ProviderAttemptLeaseID,
		"--expected-provider-slug", request.ProviderSlug,
		"--expected-provider-resource-id", request.ProviderResourceID,
		"--expected-provider-scope", request.ProviderScope,
		"--expected-coordinator-registration-url", request.CoordinatorRegistrationURL,
	}
	args, err := r.appendPersistedProviderRoutingArgs(args, request)
	if err != nil {
		return err
	}
	return r.run(ctx, request, args, io.Discard)
}

func (r *execControllerWorkspaceRunner) StopLocal(ctx context.Context, identifier string, request controllerWorkspaceRequest) error {
	for _, daemonID := range uniqueNonBlankStrings(identifier) {
		// Revocation must remain available while controller-state durability or
		// child-registry storage is unavailable. The fixed local-only stop command
		// verifies the WebVNC daemon identity before signaling it.
		if err := r.runWithStartedUntracked(ctx, request, []string{"webvnc", "daemon", "stop", "--id", daemonID}, io.Discard, nil); err != nil {
			return err
		}
	}
	return nil
}

func (r *execControllerWorkspaceRunner) DesktopConnection(ctx context.Context, identifier string, request controllerWorkspaceRequest) (connectionURL string, resultErr error) {
	expectedIdentityArgs, err := controllerWebVNCExpectedProviderArgs(request)
	if err != nil {
		return "", err
	}
	ownerID := r.desktopControllerOwnerID(identifier, request)
	startedHere := false
	defer func() {
		if resultErr == nil || !startedHere {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controllerDesktopRollbackTimeout)
		defer cancel()
		resultErr = errors.Join(resultErr, r.StopLocal(cleanupCtx, identifier, request))
	}()
	var daemonStatus controllerLimitedBuffer
	daemonStatus.limit = 1 << 20
	if err := r.run(ctx, request, []string{
		"webvnc", "daemon", "status", "--id", identifier,
		"--controller-owner-id", ownerID,
	}, &daemonStatus); err != nil {
		return "", err
	}
	if err := daemonStatus.overflowError("controller WebVNC daemon status"); err != nil {
		return "", err
	}
	localPort := controllerWebVNCDaemonLocalPort(daemonStatus.String())
	live := controllerWebVNCDaemonLive(daemonStatus.String())
	if live && !controllerWebVNCDaemonMatchesOwnership(daemonStatus.String()) {
		if err := r.run(ctx, request, []string{"webvnc", "daemon", "stop", "--id", identifier}, io.Discard); err != nil {
			return "", fmt.Errorf("replace WebVNC daemon with mismatched controller ownership: %w", err)
		}
		live = false
		localPort = ""
	}
	if live && localPort == "" {
		if err := r.run(ctx, request, []string{"webvnc", "daemon", "stop", "--id", identifier}, io.Discard); err != nil {
			return "", fmt.Errorf("replace WebVNC daemon without recorded local port: %w", err)
		}
		live = false
	}
	if !live {
		startArgs := r.appendProviderArg([]string{"webvnc", "daemon", "start", "--id", identifier}, request)
		startArgs = append(startArgs, "--controller-owned=true", "--controller-owner-id", ownerID)
		startArgs = append(startArgs, expectedIdentityArgs...)
		var startOutput controllerLimitedBuffer
		startOutput.limit = 1 << 20
		if err := r.run(ctx, request, startArgs, &startOutput); err != nil {
			return "", err
		}
		if err := startOutput.overflowError("controller WebVNC daemon start"); err != nil {
			return "", err
		}
		startedHere = true
		if controllerWebVNCDaemonPID(startOutput.String()) <= 0 {
			return "", fmt.Errorf("WebVNC daemon start did not report its supervisor pid")
		}
		localPort = controllerWebVNCDaemonLocalPort(startOutput.String())
		if localPort == "" {
			return "", fmt.Errorf("WebVNC daemon start did not report its reserved local port")
		}
	}
	initialDaemonPID, err := r.verifyWebVNCDaemon(ctx, identifier, localPort, ownerID, request)
	if err != nil {
		return "", err
	}
	// This subprocess output is bounded and parsed in-process; the controller needs
	// the credential-bearing URL without exposing it in human-facing command output.
	statusArgs := r.appendProviderArg([]string{"webvnc", "status", "--id", identifier, "--redact-credentials=false"}, request)
	statusArgs = append(statusArgs,
		"--local-port", localPort,
		"--expected-listener-owner-pid", strconv.Itoa(initialDaemonPID),
		"--controller-owner-id", ownerID,
	)
	statusArgs = append(statusArgs, expectedIdentityArgs...)
	var output controllerLimitedBuffer
	output.limit = 1 << 20
	if err := r.run(ctx, request, statusArgs, &output); err != nil {
		return "", err
	}
	if err := output.overflowError("controller WebVNC status"); err != nil {
		return "", err
	}
	if !controllerWebVNCReady(output.String()) {
		return "", fmt.Errorf("WebVNC status did not confirm a reachable target and connected bridge")
	}
	daemonPID, err := r.verifyWebVNCDaemon(ctx, identifier, localPort, ownerID, request)
	if err != nil {
		return "", err
	}
	if daemonPID != initialDaemonPID {
		return "", fmt.Errorf("WebVNC daemon identity changed during authenticated status probe")
	}
	if controllerDirectSSHWebVNCReady(output.String()) {
		if err := controllerVerifyDaemonOwnedListener(localPort, daemonPID); err != nil {
			return "", fmt.Errorf("verify direct SSH WebVNC listener ownership on local port %s: %w", localPort, err)
		}
	}
	if value := controllerWebVNCURL(output.String()); value != "" {
		return value, nil
	}
	return "", fmt.Errorf("crabbox webvnc status did not return a portal URL")
}

func controllerWebVNCExpectedProviderArgs(request controllerWorkspaceRequest) ([]string, error) {
	expected := ProviderIdentityExpectation{
		LeaseID:        strings.TrimSpace(request.ProviderLeaseID),
		AttemptLeaseID: strings.TrimSpace(request.ProviderAttemptLeaseID),
		Slug:           strings.TrimSpace(request.ProviderSlug),
		ResourceID:     strings.TrimSpace(request.ProviderResourceID),
	}
	scope := strings.TrimSpace(request.ProviderScope)
	route := strings.TrimSpace(request.ProviderRoute)
	if expected.LeaseID == "" || expected.AttemptLeaseID == "" || expected.Slug == "" || expected.ResourceID == "" || scope == "" || route == "" {
		return nil, fmt.Errorf("controller WebVNC requires a complete persisted provider identity")
	}
	if err := ValidateProviderIdentityExpectation(expected); err != nil {
		return nil, err
	}
	if request.ProviderScope != scope || !validControllerInventoryIdentity(scope) {
		return nil, fmt.Errorf("controller WebVNC provider scope is invalid")
	}
	if request.ProviderRoute != route || !validControllerInventoryIdentity(route) {
		return nil, fmt.Errorf("controller WebVNC provider route is invalid")
	}
	return []string{
		"--expected-provider-lease-id", expected.LeaseID,
		"--expected-provider-attempt-lease-id", expected.AttemptLeaseID,
		"--expected-provider-slug", expected.Slug,
		"--expected-provider-resource-id", expected.ResourceID,
		"--expected-provider-scope", scope,
	}, nil
}

func (r *execControllerWorkspaceRunner) verifyWebVNCDaemon(ctx context.Context, identifier, localPort, ownerID string, request controllerWorkspaceRequest) (int, error) {
	var output controllerLimitedBuffer
	output.limit = 1 << 20
	if err := r.run(ctx, request, []string{
		"webvnc", "daemon", "status", "--id", identifier,
		"--controller-owner-id", ownerID,
	}, &output); err != nil {
		return 0, fmt.Errorf("verify WebVNC daemon: %w", err)
	}
	if err := output.overflowError("controller WebVNC daemon verification"); err != nil {
		return 0, err
	}
	verifiedPort := controllerWebVNCDaemonLocalPort(output.String())
	daemonPID := controllerWebVNCDaemonPID(output.String())
	if !controllerWebVNCDaemonLive(output.String()) || daemonPID <= 0 || verifiedPort == "" || verifiedPort != localPort || !controllerWebVNCDaemonMatchesOwnership(output.String()) {
		return 0, fmt.Errorf("WebVNC daemon did not become ready on local port %s", localPort)
	}
	return daemonPID, nil
}

func (r *execControllerWorkspaceRunner) desktopControllerOwnerID(identifier string, request controllerWorkspaceRequest) string {
	values := []string{
		filepath.Clean(r.controllerStatePath()), filepath.Clean(r.opts.Config),
		request.ProviderRoute, request.ProviderScope, request.ID, identifier,
		request.ProviderLeaseID, request.ProviderAttemptLeaseID, request.ProviderSlug, request.ProviderResourceID,
	}
	hash := sha256.New()
	for _, value := range values {
		_, _ = hash.Write([]byte(strconv.Itoa(len(value))))
		_, _ = hash.Write([]byte{':'})
		_, _ = hash.Write([]byte(value))
	}
	rawOwnerToken := hex.EncodeToString(hash.Sum(nil))
	ownerIDHash := sha256.New()
	_, _ = ownerIDHash.Write([]byte("crabbox:webvnc-owner-id:v1\x00"))
	_, _ = ownerIDHash.Write([]byte(rawOwnerToken))
	return hex.EncodeToString(ownerIDHash.Sum(nil))
}

func controllerWebVNCDaemonMatchesOwnership(output string) bool {
	expected := "webvnc daemon: controller-owned=true no-provider-side-effects=true owner-match=true"
	owned := false
	commandSafe := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == expected {
			owned = true
		}
		if command, ok := strings.CutPrefix(line, "webvnc daemon: command="); ok && controllerWebVNCNoProviderSideEffectsPattern.MatchString(command) {
			commandSafe = true
		}
	}
	return owned && commandSafe
}

var controllerWebVNCNoProviderSideEffectsPattern = regexp.MustCompile(`(?:^|[[:space:]'"])--no-provider-side-effects=true(?:$|[[:space:]'"])`)

func controllerWebVNCReady(output string) bool {
	targetReachable := false
	bridgeReady := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "vnc target: reachable ") {
			targetReachable = true
		}
		if line == "portal bridge: connected=true" || strings.HasPrefix(line, "portal bridge: connected=true ") || line == "direct ssh webvnc: running" {
			bridgeReady = true
		}
	}
	return targetReachable && bridgeReady
}

func controllerDirectSSHWebVNCReady(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "direct ssh webvnc: running" {
			return true
		}
	}
	return false
}

var controllerWebVNCLocalPortPattern = regexp.MustCompile(`(?:^|[[:space:]'\"])(?:--local-port=|--local-port[[:space:]'\"]+)([0-9]{1,5})(?:$|[[:space:]'\"])`)

func controllerWebVNCDaemonPID(output string) int {
	for _, line := range strings.Split(output, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "webvnc daemon: pid=")
		if !ok {
			continue
		}
		pidText, _, _ := strings.Cut(value, " ")
		pid, err := strconv.Atoi(pidText)
		if err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}

func controllerWebVNCDaemonLocalPort(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "webvnc daemon: local-port="); ok {
			if port, err := strconv.Atoi(value); err == nil && port >= 1 && port <= 65535 {
				return value
			}
		}
		command, ok := strings.CutPrefix(strings.TrimSpace(line), "webvnc daemon: command=")
		if !ok {
			continue
		}
		match := controllerWebVNCLocalPortPattern.FindStringSubmatch(command)
		if len(match) == 2 {
			port, err := strconv.Atoi(match[1])
			if err == nil && port > 0 && port <= 65535 {
				return match[1]
			}
		}
	}
	return ""
}

func controllerWebVNCDaemonLive(output string) bool {
	hasPID := false
	hasCommand := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "webvnc daemon: pid=") {
			hasPID = true
		}
		if command, ok := strings.CutPrefix(line, "webvnc daemon: command="); ok && isWebVNCDaemonCommand(command) {
			hasCommand = true
		}
	}
	return hasPID && hasCommand
}

func controllerWebVNCURL(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "webvnc: "); ok {
			value = strings.TrimSpace(value)
			if value != "" && value != "[redacted]" && !strings.HasPrefix(value, "run ") {
				return value
			}
		}
	}
	return ""
}

func (r *execControllerWorkspaceRunner) appendProviderArg(args []string, request controllerWorkspaceRequest) []string {
	if provider := firstNonBlank(request.ProviderRoute, r.opts.Provider); provider != "" {
		args = append(args, "--provider", provider)
	}
	return args
}

func (r *execControllerWorkspaceRunner) appendPersistedProviderRoutingArgs(args []string, request controllerWorkspaceRequest) ([]string, error) {
	args = r.appendProviderArg(args, request)
	if firstNonBlank(request.ProviderRoute, r.opts.Provider) != "external" {
		return args, nil
	}
	leaseID := firstNonBlank(request.ProviderLeaseID, request.ProviderAttemptLeaseID)
	if leaseID == "" {
		return nil, fmt.Errorf("external controller lifecycle requires a persisted provider lease identity")
	}
	path, err := ExternalRoutingPath(leaseID)
	if err != nil {
		return nil, fmt.Errorf("resolve persisted external controller routing: %w", err)
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			// Acquire persists routing only after its raw identity callback and
			// lease validation. Before that point the persisted provider scope is
			// still the exact routing guard, so inventory must remain available for
			// late-create recovery instead of requiring a file that cannot exist yet.
			return args, nil
		}
		return nil, fmt.Errorf("inspect persisted external controller routing: %w", err)
	}
	return append(args, "--external-routing-file", path), nil
}

func (r *execControllerWorkspaceRunner) workspaceAbsent(ctx context.Context, identifier string, request controllerWorkspaceRequest) (bool, error) {
	args, err := r.appendPersistedProviderRoutingArgs([]string{"list", "--json", "--refresh", "--all"}, request)
	if err != nil {
		return false, err
	}
	var output controllerLimitedBuffer
	output.limit = 1 << 20
	if err := r.run(ctx, request, args, &output); err != nil {
		return false, err
	}
	if err := output.overflowError("controller provider inventory"); err != nil {
		return false, fmt.Errorf("provider absence cannot be confirmed: %w", err)
	}
	identities, err := controllerAbsenceIdentitySet(identifier, request)
	if err != nil {
		return false, err
	}
	return controllerListConfirmsAbsent(output.Bytes(), identities)
}

type controllerAbsenceIdentities struct {
	LeaseIDs    []string
	Names       []string
	ResourceIDs []string
}

type controllerInventoryIdentityKind uint8

const (
	controllerInventoryLeaseIdentity controllerInventoryIdentityKind = 1 << iota
	controllerInventoryNameIdentity
	controllerInventoryResourceIdentity
)

func controllerAbsenceIdentitySet(identifier string, request controllerWorkspaceRequest) (controllerAbsenceIdentities, error) {
	identities := controllerAbsenceIdentities{}
	addLease := func(value string) error {
		raw := value
		value = strings.TrimSpace(raw)
		if value == "" {
			return nil
		}
		if raw != value || !validLeaseClaimID(value) {
			return fmt.Errorf("invalid persisted provider lease identity %q", value)
		}
		identities.LeaseIDs = appendUniqueStrings(identities.LeaseIDs, value)
		return nil
	}
	for _, value := range []string{request.ProviderLeaseID, request.ProviderAttemptLeaseID} {
		if err := addLease(value); err != nil {
			return controllerAbsenceIdentities{}, err
		}
	}
	rawSlug := request.ProviderSlug
	slug := strings.TrimSpace(rawSlug)
	if slug != "" {
		if rawSlug != slug || normalizeLeaseSlug(slug) != slug {
			return controllerAbsenceIdentities{}, fmt.Errorf("invalid persisted provider slug identity %q", slug)
		}
		identities.Names = appendUniqueStrings(identities.Names, slug)
		for _, leaseID := range identities.LeaseIDs {
			identities.Names = appendUniqueStrings(identities.Names, leaseProviderName(leaseID, slug))
		}
	}
	rawResourceID := request.ProviderResourceID
	resourceID := strings.TrimSpace(rawResourceID)
	if resourceID != "" {
		if rawResourceID != resourceID || !validControllerInventoryIdentity(resourceID) {
			return controllerAbsenceIdentities{}, fmt.Errorf("invalid persisted provider resource identity")
		}
		identities.ResourceIDs = append(identities.ResourceIDs, resourceID)
	}
	if len(identities.LeaseIDs)+len(identities.Names)+len(identities.ResourceIDs) == 0 {
		if err := addLease(identifier); err != nil {
			return controllerAbsenceIdentities{}, err
		}
	}
	if len(identities.LeaseIDs)+len(identities.Names)+len(identities.ResourceIDs) == 0 {
		return controllerAbsenceIdentities{}, fmt.Errorf("provider absence identity set is empty")
	}
	return identities, nil
}

func (i controllerAbsenceIdentities) requiredKinds() controllerInventoryIdentityKind {
	var kinds controllerInventoryIdentityKind
	if len(i.LeaseIDs) > 0 {
		kinds |= controllerInventoryLeaseIdentity
	}
	if len(i.Names) > 0 {
		kinds |= controllerInventoryNameIdentity
	}
	if len(i.ResourceIDs) > 0 {
		kinds |= controllerInventoryResourceIdentity
	}
	return kinds
}

func (i controllerAbsenceIdentities) values() map[string]struct{} {
	values := make(map[string]struct{}, len(i.LeaseIDs)+len(i.Names)+len(i.ResourceIDs))
	for _, group := range [][]string{i.LeaseIDs, i.Names, i.ResourceIDs} {
		for _, value := range group {
			values[value] = struct{}{}
		}
	}
	return values
}

func controllerListConfirmsAbsent(data []byte, identities controllerAbsenceIdentities) (bool, error) {
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '[' {
		return false, fmt.Errorf("crabbox list JSON is not an array")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var entries []map[string]any
	if err := decoder.Decode(&entries); err != nil {
		return false, fmt.Errorf("decode crabbox list JSON: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return false, fmt.Errorf("decode crabbox list JSON: trailing data")
	}
	requiredKinds := identities.requiredKinds()
	if requiredKinds == 0 {
		return false, fmt.Errorf("provider absence identity set is empty")
	}
	expected := identities.values()
	for _, entry := range entries {
		values, kinds, _, identityErr := controllerListIdentityValues(entry)
		matchesTarget := false
		for _, value := range values {
			if _, ok := expected[value]; ok {
				matchesTarget = true
				break
			}
		}
		if !matchesTarget {
			continue
		}
		if identityErr != nil {
			return false, identityErr
		}
		if kinds&requiredKinds != requiredKinds {
			return false, fmt.Errorf("crabbox list JSON has an incomplete matching workspace identity")
		}
		return false, nil
	}
	return true, nil
}

func controllerListIdentityValues(entry map[string]any) ([]string, controllerInventoryIdentityKind, bool, error) {
	values := make([]string, 0, 8)
	var kinds controllerInventoryIdentityKind
	recognized := false
	var identityErr error
	setIdentityErr := func(err error) {
		if identityErr == nil {
			identityErr = err
		}
	}
	for key, raw := range entry {
		normalized := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
		var kind controllerInventoryIdentityKind
		switch normalized {
		case "leaseid", "lease":
			kind = controllerInventoryLeaseIdentity
		case "slug", "name":
			kind = controllerInventoryNameIdentity
		case "cloudid", "serverid", "providerid", "providerresourceid", "resourceid":
			kind = controllerInventoryResourceIdentity
		case "labels":
			recognized = true
			labels, ok := raw.(map[string]any)
			if !ok {
				setIdentityErr(fmt.Errorf("crabbox list JSON has invalid labels identity fields"))
				continue
			}
			labelValues, labelKinds, labelRecognized, err := controllerListIdentityValues(labels)
			if err != nil {
				setIdentityErr(err)
			}
			recognized = recognized || labelRecognized
			kinds |= labelKinds
			values = append(values, labelValues...)
		}
		if kind == 0 {
			continue
		}
		recognized = true
		rawValue, ok := raw.(string)
		if !ok {
			setIdentityErr(fmt.Errorf("crabbox list JSON has an empty or invalid workspace identity"))
			continue
		}
		value := strings.TrimSpace(rawValue)
		if value != "" {
			values = append(values, value)
		}
		if value != rawValue || !validControllerInventoryIdentity(value) {
			setIdentityErr(fmt.Errorf("crabbox list JSON has an empty or invalid workspace identity"))
			continue
		}
		kinds |= kind
	}
	return values, kinds, recognized, identityErr
}

func validControllerInventoryIdentity(value string) bool {
	if value == "" || len(value) > 4096 {
		return false
	}
	for _, r := range value {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}

func (r *execControllerWorkspaceRunner) run(ctx context.Context, request controllerWorkspaceRequest, args []string, stdout io.Writer) error {
	return r.runWithStarted(ctx, request, args, stdout, nil)
}

func (r *execControllerWorkspaceRunner) runWithStarted(ctx context.Context, request controllerWorkspaceRequest, args []string, stdout io.Writer, onStarted func() error) error {
	return r.runWithStartedEnv(ctx, request, args, stdout, onStarted, nil)
}

func (r *execControllerWorkspaceRunner) runWithStartedEnv(ctx context.Context, request controllerWorkspaceRequest, args []string, stdout io.Writer, onStarted func() error, extraEnv map[string]string) error {
	if strings.TrimSpace(r.controllerStatePath()) == "" {
		return r.runWithStartedUntrackedEnv(ctx, request, args, stdout, onStarted, extraEnv)
	}
	nonce, err := newWebVNCDaemonNonce()
	if err != nil {
		return fmt.Errorf("create crabbox %s child identity: %w", args[0], err)
	}
	gateReader, gateWriter, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create crabbox %s launch gate: %w", args[0], err)
	}
	defer gateWriter.Close()
	childArgs := append([]string{r.opts.Binary}, args...)
	cmdArgs := []string{"-c", controllerTrackedChildScript(), "crabbox-controller-child-" + nonce}
	cmdArgs = append(cmdArgs, childArgs...)
	cmd := exec.CommandContext(ctx, "sh", cmdArgs...)
	configureControllerCommand(cmd)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return stopDaemonProcess(cmd.Process, cmd.Process.Pid)
	}
	cmd.Dir = r.opts.WorkDir
	cmd.Stdin = gateReader
	cmd.Stdout = stdout
	cmd.Stderr = io.Discard
	cmd.Env = controllerChildEnvWithOverrides(os.Environ(), r.opts.Config, request, r.adapterChildEnv(extraEnv))
	cmd.WaitDelay = controllerChildWaitDelay
	if err := cmd.Start(); err != nil {
		_ = gateReader.Close()
		return fmt.Errorf("start crabbox %s launch gate: %w", args[0], err)
	}
	_ = gateReader.Close()
	identityPath, err := r.registerControllerChild(cmd.Process.Pid, request.ID, args[0], nonce)
	if err != nil {
		_ = cmd.Cancel()
		_ = cmd.Wait()
		groupErr := terminateControllerProcessGroup(cmd.Process.Pid)
		return errors.Join(fmt.Errorf("record crabbox %s child identity: %w", args[0], err), groupErr)
	}
	cleanupIdentity := func() error { return removeControllerChildIdentity(identityPath) }
	terminateAndCleanupIdentity := func() error {
		if err := terminateControllerProcessGroup(cmd.Process.Pid); err != nil {
			// Retain the durable identity so startup recovery can try again.
			return err
		}
		return cleanupIdentity()
	}
	if onStarted != nil {
		if err := onStarted(); err != nil {
			_ = cmd.Cancel()
			_ = cmd.Wait()
			return errors.Join(fmt.Errorf("record crabbox %s start: %w", args[0], err), terminateAndCleanupIdentity())
		}
	}
	if ctx.Err() != nil {
		_ = cmd.Cancel()
		_ = cmd.Wait()
		return errors.Join(context.Cause(ctx), terminateAndCleanupIdentity())
	}
	if _, err := io.WriteString(gateWriter, "run\n"); err != nil {
		_ = cmd.Cancel()
		_ = cmd.Wait()
		return errors.Join(fmt.Errorf("release crabbox %s launch gate: %w", args[0], err), terminateAndCleanupIdentity())
	}
	waitErr := cmd.Wait()
	_ = gateWriter.Close()
	groupErr := terminateControllerProcessGroup(cmd.Process.Pid)
	var cleanupErr error
	if groupErr == nil {
		cleanupErr = cleanupIdentity()
	}
	if errors.Is(waitErr, exec.ErrWaitDelay) && groupErr == nil {
		waitErr = nil
	}
	return errors.Join(controllerChildCommandResult(ctx, args[0], waitErr), groupErr, cleanupErr)
}

func controllerTrackedChildScript() string {
	return "exec 3<&0\n" +
		"IFS= read -r gate <&3 || exit 125\n" +
		"[ \"$gate\" = run ] || exit 125\n" +
		"( IFS= read -r _ <&3 && exit 0; /bin/kill -KILL -- -$$ || /bin/kill -KILL $$ ) >/dev/null 2>&1 &\n" +
		"\"$@\" </dev/null &\n" +
		"child=$!\n" +
		"wait \"$child\"\n" +
		"code=$?\n" +
		"exit \"$code\"\n"
}

func (r *execControllerWorkspaceRunner) runWithStartedUntracked(ctx context.Context, request controllerWorkspaceRequest, args []string, stdout io.Writer, onStarted func() error) error {
	return r.runWithStartedUntrackedEnv(ctx, request, args, stdout, onStarted, nil)
}

func (r *execControllerWorkspaceRunner) runWithStartedUntrackedEnv(ctx context.Context, request controllerWorkspaceRequest, args []string, stdout io.Writer, onStarted func() error, extraEnv map[string]string) error {
	cmd := exec.CommandContext(ctx, r.opts.Binary, args...)
	configureControllerCommand(cmd)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return stopDaemonProcess(cmd.Process, cmd.Process.Pid)
	}
	cmd.Dir = r.opts.WorkDir
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = io.Discard
	cmd.Env = controllerChildEnvWithOverrides(os.Environ(), r.opts.Config, request, r.adapterChildEnv(extraEnv))
	cmd.WaitDelay = controllerChildWaitDelay
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start crabbox %s: %w", args[0], err)
	}
	if onStarted != nil {
		if err := onStarted(); err != nil {
			_ = cmd.Cancel()
			_ = cmd.Wait()
			_ = terminateControllerProcessGroup(cmd.Process.Pid)
			return fmt.Errorf("record crabbox %s start: %w", args[0], err)
		}
	}
	waitErr := cmd.Wait()
	groupErr := terminateControllerProcessGroup(cmd.Process.Pid)
	if errors.Is(waitErr, exec.ErrWaitDelay) && groupErr == nil {
		waitErr = nil
	}
	return errors.Join(controllerChildCommandResult(ctx, args[0], waitErr), groupErr)
}

func controllerChildCommandResult(ctx context.Context, operation string, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &controllerCommandError{Command: operation, ExitCode: exitErr.ExitCode()}
	}
	return fmt.Errorf("run crabbox %s: %w", operation, err)
}

func (r *execControllerWorkspaceRunner) controllerStatePath() string {
	r.stateMu.RLock()
	stateFile := r.stateFile
	r.stateMu.RUnlock()
	if strings.TrimSpace(stateFile) != "" {
		return stateFile
	}
	return r.opts.StateFile
}

func (r *execControllerWorkspaceRunner) RecoverControllerChildren(ctx context.Context, stateFile string) error {
	stateFile = filepath.Clean(strings.TrimSpace(stateFile))
	if stateFile == "." || stateFile == "" {
		return fmt.Errorf("controller state file is required for child recovery")
	}
	if configured := strings.TrimSpace(r.opts.StateFile); configured != "" && filepath.Clean(configured) != stateFile {
		return fmt.Errorf("controller child state path does not match service state path")
	}
	r.stateMu.Lock()
	r.stateFile = stateFile
	r.stateMu.Unlock()
	dir := controllerChildStateDirectory(stateFile)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read controller child identities: %w", err)
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		path := filepath.Join(dir, entry.Name())
		if strings.HasPrefix(entry.Name(), ".controller-child-") && strings.HasSuffix(entry.Name(), ".tmp") {
			if err := removeControllerFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove incomplete controller child identity: %w", err)
			}
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		identity, err := readControllerChildIdentity(path)
		if err != nil {
			return err
		}
		sameBoot, bootErr := processBootIdentityMatches(identity.BootID)
		if bootErr != nil {
			return fmt.Errorf("inspect controller child pid %d boot identity: %w", identity.PID, bootErr)
		}
		if !sameBoot {
			// A process group cannot survive a reboot. Drop the prior-boot handle
			// without looking up or signaling a potentially recycled PID/PGID.
			if err := removeControllerChildIdentity(path); err != nil {
				return err
			}
			continue
		}
		command, alive := webVNCDaemonProcessCommand(identity.PID)
		started, startErr := webVNCDaemonProcessStartIdentity(identity.PID)
		if startErr != nil {
			if alive {
				return fmt.Errorf("inspect controller child pid %d start identity: %w", identity.PID, startErr)
			}
			if controllerProcessGroupAlive(identity.PID) {
				return fmt.Errorf("refusing to signal controller child process group %d without its recorded leader identity", identity.PID)
			}
			if err := removeControllerChildIdentity(path); err != nil {
				return err
			}
			continue
		}
		if strings.TrimSpace(started) != identity.ProcessStarted {
			// The PID was recycled. Never signal the replacement process, and
			// never discard the only durable handle while the recorded group may
			// still contain a detached lifecycle descendant.
			if controllerProcessGroupAlive(identity.PID) {
				return fmt.Errorf("controller child pid %d was recycled while its recorded process group is still active", identity.PID)
			}
			if err := removeControllerChildIdentity(path); err != nil {
				return err
			}
			continue
		}
		if !alive || !controllerChildIdentityMatchesProcess(identity, command, started) {
			if controllerProcessGroupAlive(identity.PID) {
				return fmt.Errorf("controller child pid %d does not match its recorded process identity", identity.PID)
			}
			if err := removeControllerChildIdentity(path); err != nil {
				return err
			}
			continue
		}
		process, err := os.FindProcess(identity.PID)
		if err != nil {
			return fmt.Errorf("find controller child pid %d: %w", identity.PID, err)
		}
		if err := stopDaemonProcess(process, identity.PID); err != nil {
			return fmt.Errorf("stop recovered controller child pid %d: %w", identity.PID, err)
		}
		// Usually a recovered child was reparented when the prior controller
		// exited. Tests and in-process restarts can still own it, in which case
		// this is the only process allowed to reap the terminated leader.
		_, _ = process.Wait()
		if err := terminateControllerProcessGroup(identity.PID); err != nil {
			return fmt.Errorf("stop recovered controller child process group %d: %w", identity.PID, err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			command, alive := webVNCDaemonProcessCommand(identity.PID)
			if !alive || strings.Contains(strings.ToLower(command), "<defunct>") {
				break
			}
			current, currentErr := webVNCDaemonProcessStartIdentity(identity.PID)
			if currentErr != nil || strings.TrimSpace(current) != identity.ProcessStarted {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("controller child pid %d survived recovery termination", identity.PID)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := removeControllerChildIdentity(path); err != nil {
			return err
		}
	}
	return nil
}

func terminateControllerProcessGroup(processGroupID int) error {
	if err := stopControllerProcessGroup(processGroupID); err != nil {
		// Darwin can report EPERM after the group leader was reaped even when no
		// non-zombie member remains. Verify the group before retaining the
		// durable child handle.
		if !controllerProcessGroupAlive(processGroupID) {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for controllerProcessGroupAlive(processGroupID) {
		if time.Now().After(deadline) {
			return fmt.Errorf("controller process group %d survived termination", processGroupID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func (r *execControllerWorkspaceRunner) registerControllerChild(pid int, workspaceID, operation, nonce string) (string, error) {
	if !validWebVNCDaemonNonce(nonce) {
		return "", fmt.Errorf("invalid controller child nonce")
	}
	started, err := webVNCDaemonProcessStartIdentity(pid)
	if err != nil {
		return "", err
	}
	bootID, err := processBootIdentity()
	if err != nil {
		return "", err
	}
	identity := controllerChildIdentity{
		Version:        controllerChildIdentityVersion,
		PID:            pid,
		ProcessStarted: strings.TrimSpace(started),
		BootID:         bootID,
		Nonce:          nonce,
		WorkspaceID:    workspaceID,
		Operation:      operation,
	}
	dir := controllerChildStateDirectory(r.controllerStatePath())
	if err := ensureControllerStateDirectory(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, nonce+".json")
	if err := writeControllerChildIdentity(path, identity); err != nil {
		return "", err
	}
	return path, nil
}

func controllerChildStateDirectory(stateFile string) string {
	return filepath.Clean(stateFile) + ".children"
}

func readControllerChildIdentity(path string) (controllerChildIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return controllerChildIdentity{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return controllerChildIdentity{}, fmt.Errorf("controller child identity must be a private regular file: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return controllerChildIdentity{}, err
	}
	var identity controllerChildIdentity
	if err := json.Unmarshal(data, &identity); err != nil {
		return controllerChildIdentity{}, fmt.Errorf("parse controller child identity %s: %w", path, err)
	}
	if identity.Version != controllerChildIdentityVersion || identity.PID <= 0 || identity.ProcessStarted == "" ||
		!validPersistedProcessBootIdentity(identity.BootID) || !validWebVNCDaemonNonce(identity.Nonce) || identity.Operation == "" {
		return controllerChildIdentity{}, fmt.Errorf("invalid controller child identity %s", path)
	}
	return identity, nil
}

func controllerChildIdentityMatchesProcess(identity controllerChildIdentity, command, started string) bool {
	bootMatches, err := processBootIdentityMatches(identity.BootID)
	return identity.Version == controllerChildIdentityVersion &&
		identity.PID > 0 &&
		err == nil && bootMatches &&
		identity.ProcessStarted == strings.TrimSpace(started) &&
		validWebVNCDaemonNonce(identity.Nonce) &&
		strings.Contains(command, "crabbox-controller-child-"+identity.Nonce)
}

func writeControllerChildIdentity(path string, identity controllerChildIdentity) error {
	data, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".controller-child-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceControllerFile(tmpPath, path); err != nil {
		return err
	}
	if err := syncControllerDirectory(dir); err != nil {
		return fmt.Errorf("sync controller child identity directory: %w", err)
	}
	return nil
}

func removeControllerChildIdentity(path string) error {
	if err := removeControllerFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncControllerDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync controller child identity removal: %w", err)
	}
	return nil
}

type controllerCommandError struct {
	Command  string
	ExitCode int
}

func (e *controllerCommandError) Error() string {
	return fmt.Sprintf("crabbox %s failed with exit code %d", e.Command, e.ExitCode)
}

type controllerWorkspaceNotFoundError struct {
	Identifier string
}

func (e *controllerWorkspaceNotFoundError) Error() string {
	return fmt.Sprintf("workspace %q is absent from provider inventory", e.Identifier)
}

func controllerWorkspaceNotFound(err error) bool {
	var notFound *controllerWorkspaceNotFoundError
	return errors.As(err, &notFound)
}

func uniqueNonBlankStrings(values ...string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func controllerChildEnv(base []string, config string, request controllerWorkspaceRequest) []string {
	return controllerChildEnvWithOverrides(base, config, request, nil)
}

func controllerChildEnvWithOverrides(base []string, config string, request controllerWorkspaceRequest, overrides map[string]string) []string {
	values := map[string]string{
		controllerProviderScopeEnv:          request.ProviderScope,
		controllerWorkspaceIDEnv:            request.ID,
		controllerProcessTreeOwnedEnv:       "1",
		"CRABBOX_ADAPTER_REPO":              request.Repo,
		"CRABBOX_ADAPTER_BRANCH":            request.Branch,
		"CRABBOX_ADAPTER_RUNTIME":           request.Runtime,
		"CRABBOX_ADAPTER_PROFILE":           request.Profile,
		"CRABBOX_ADAPTER_OWNER":             request.Owner,
		"CRABBOX_ADAPTER_CREATED_BY":        request.CreatedBy,
		"CRABBOX_ADAPTER_PARENT_SESSION_ID": request.ParentSessionID,
		"CRABBOX_ADAPTER_ROOT_SESSION_ID":   request.RootSessionID,
	}
	if config != "" {
		values["CRABBOX_CONFIG"] = config
	}
	blocked := map[string]struct{}{
		controllerAcquireIdentityAddressEnv:          {},
		controllerAcquireIdentityTokenEnv:            {},
		controllerCoordinatorRegistrationExpectedEnv: {},
		controllerCoordinatorRegistrationURLEnv:      {},
		"CRABBOX_ADAPTER_ID":                         {},
	}
	for name, value := range overrides {
		if strings.TrimSpace(value) == "" {
			continue
		}
		values[name] = value
	}
	out := make([]string, 0, len(base)+len(values))
	for _, item := range base {
		name, _, ok := strings.Cut(item, "=")
		if ok {
			if _, replaced := values[name]; replaced {
				continue
			}
			if _, remove := blocked[name]; remove {
				continue
			}
		}
		out = append(out, item)
	}
	for name, value := range values {
		out = append(out, name+"="+value)
	}
	return out
}

func (r *execControllerWorkspaceRunner) adapterChildEnv(overrides map[string]string) map[string]string {
	if strings.TrimSpace(r.opts.AdapterID) == "" {
		return overrides
	}
	merged := make(map[string]string, len(overrides)+1)
	for name, value := range overrides {
		merged[name] = value
	}
	merged["CRABBOX_ADAPTER_ID"] = strings.TrimSpace(r.opts.AdapterID)
	return merged
}

type controllerLimitedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *controllerLimitedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if len(data) > remaining {
			b.overflow = true
			data = data[:remaining]
		}
		_, _ = b.Buffer.Write(data)
	} else if original > 0 {
		b.overflow = true
	}
	return original, nil
}

func (b *controllerLimitedBuffer) overflowError(label string) error {
	if !b.overflow {
		return nil
	}
	return fmt.Errorf("%s exceeded %d-byte output limit", label, b.limit)
}
