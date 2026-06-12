package external

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type leaseBackend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

const externalSlugReservationTTL = 6 * time.Hour

func (b *leaseBackend) Spec() core.ProviderSpec { return b.spec }

func (b *leaseBackend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	leaseID := core.NewLeaseID()
	slug, reservation, err := b.allocateLeaseSlug(leaseID, req.RequestedSlug)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if reservation != nil {
		defer reservation.Release()
	}
	desired := &desiredLease{LeaseID: leaseID, Slug: slug, Name: core.LeaseProviderName(leaseID, slug)}
	response, err := b.invoke(ctx, protocolRequest{
		Operation: "acquire",
		Desired:   desired,
		Keep:      req.Keep,
		Reclaim:   req.Reclaim,
		Repo:      repoForProtocol(req.Repo),
	})
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if response.Lease == nil {
		return core.LeaseTarget{}, core.Exit(5, "external provider acquire returned no lease")
	}
	if response.Lease.LeaseID != "" && response.Lease.LeaseID != desired.LeaseID {
		var err error = core.Exit(4, "external provider lease identity changed: expected %s, found %s", desired.LeaseID, response.Lease.LeaseID)
		if !req.Keep {
			err = appendAcquireCleanupError(err, b.rollbackAcquireRelease(ctx, response.Lease))
		}
		return core.LeaseTarget{}, err
	}
	fillDesired(response.Lease, desired)
	lease := response.Lease.target(b.cfg, req.Keep)
	if err := validateLease(lease, true, true); err != nil {
		if !req.Keep {
			err = appendAcquireCleanupError(err, b.rollbackAcquireRelease(ctx, leaseForProtocol(lease)))
		}
		return core.LeaseTarget{}, err
	}
	if _, err := core.PersistExternalRouting(lease.LeaseID, b.cfg.External); err != nil {
		var acquireErr error = core.Exit(2, "%v", err)
		if !req.Keep {
			acquireErr = appendAcquireCleanupError(acquireErr, b.rollbackAcquireRelease(ctx, leaseForProtocol(lease)))
		}
		return core.LeaseTarget{}, acquireErr
	}
	if err := core.WaitForSSHReady(ctx, &lease.SSH, b.rt.Stderr, "external provider SSH", core.BootstrapWaitTimeout(b.cfg)); err != nil {
		if !req.Keep {
			err = appendAcquireCleanupError(err, b.rollbackAcquireRelease(ctx, leaseForProtocol(lease)))
			core.RemoveExternalRouting(lease.LeaseID)
		}
		return core.LeaseTarget{}, err
	}
	lease.Server.Status = "ready"
	lease.Server.Labels["state"] = "ready"
	claimSlug := leaseSlugForClaim(lease, slug)
	var claimSlugReservation *slugReservation
	if core.NormalizeLeaseSlug(claimSlug) != core.NormalizeLeaseSlug(slug) {
		inUse, err := b.claimSlugInUse(claimSlug, leaseID)
		if err != nil {
			if !req.Keep {
				err = appendAcquireCleanupError(err, b.rollbackAcquireRelease(ctx, leaseForProtocol(lease)))
				core.RemoveExternalRouting(lease.LeaseID)
			}
			return core.LeaseTarget{}, err
		}
		if inUse {
			var err error = core.Exit(4, "external provider returned slug %q which is already claimed in this lifecycle scope", claimSlug)
			if !req.Keep {
				err = appendAcquireCleanupError(err, b.rollbackAcquireRelease(ctx, leaseForProtocol(lease)))
				core.RemoveExternalRouting(lease.LeaseID)
			}
			return core.LeaseTarget{}, err
		}
		var reserved bool
		claimSlugReservation, reserved, err = b.reserveLeaseSlug(claimSlug, leaseID)
		if err != nil {
			if !req.Keep {
				err = appendAcquireCleanupError(err, b.rollbackAcquireRelease(ctx, leaseForProtocol(lease)))
				core.RemoveExternalRouting(lease.LeaseID)
			}
			return core.LeaseTarget{}, err
		}
		if !reserved {
			var err error = core.Exit(4, "external provider returned slug %q which is already reserved in this lifecycle scope", claimSlug)
			if !req.Keep {
				err = appendAcquireCleanupError(err, b.rollbackAcquireRelease(ctx, leaseForProtocol(lease)))
				core.RemoveExternalRouting(lease.LeaseID)
			}
			return core.LeaseTarget{}, err
		}
		defer claimSlugReservation.Release()
	}
	if err := b.claimLeaseForRepo(lease.LeaseID, claimSlug, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
		if !req.Keep {
			err = appendAcquireCleanupError(err, b.rollbackAcquireRelease(ctx, leaseForProtocol(lease)))
			core.RemoveExternalRouting(lease.LeaseID)
		}
		return core.LeaseTarget{}, err
	}
	if err := core.UpdateLeaseClaimEndpoint(lease.LeaseID, lease.Server, lease.SSH); err != nil {
		if !req.Keep {
			err = appendAcquireCleanupError(err, b.rollbackAcquireRelease(ctx, leaseForProtocol(lease)))
			core.RemoveLeaseClaim(lease.LeaseID)
			core.RemoveExternalRouting(lease.LeaseID)
		}
		return core.LeaseTarget{}, err
	}
	return lease, nil
}

func (b *leaseBackend) rollbackAcquireRelease(ctx context.Context, lease *protocolLease) error {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lifecycleRollbackTimeout)
	defer cancel()
	_, err := b.invoke(rollbackCtx, protocolRequest{Operation: "release", Lease: lease})
	return err
}

func appendAcquireCleanupError(primary, cleanup error) error {
	if cleanup == nil {
		return primary
	}
	return acquireCleanupError{primary: primary, cleanup: cleanup}
}

type acquireCleanupError struct {
	primary error
	cleanup error
}

func (e acquireCleanupError) Error() string {
	return fmt.Sprintf("%v; external provider cleanup failed: %v", e.primary, e.cleanup)
}

func (e acquireCleanupError) Unwrap() error {
	return e.primary
}

func (e acquireCleanupError) As(target any) bool {
	var exit core.ExitError
	if core.AsExitError(e.primary, &exit) {
		if targetExit, ok := target.(*core.ExitError); ok {
			*targetExit = core.Exit(exit.Code, "%s", e.Error())
			return true
		}
	}
	return errors.As(e.primary, target)
}

func (b *leaseBackend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	id := req.ID
	var desired *desiredLease
	var claimedLease *protocolLease
	var claimLabels map[string]string
	keep := true
	if claim, ok, err := b.resolveClaim(req.ID); err != nil {
		return core.LeaseTarget{}, err
	} else if ok {
		name := core.Blank(claim.Labels["name"], core.LeaseProviderName(claim.LeaseID, claim.Slug))
		id = name
		desired = &desiredLease{LeaseID: claim.LeaseID, Slug: claim.Slug, Name: name}
		claimLabels = claim.Labels
		if lifecycleConfigured(b.cfg.External) {
			claimedLease = &protocolLease{
				LeaseID: claim.LeaseID,
				Slug:    claim.Slug,
				Name:    name,
				Labels:  claim.Labels,
			}
		}
		keep = keepFromLabels(claim.Labels, true)
	}
	response, err := b.invoke(ctx, protocolRequest{
		Operation:   "resolve",
		ID:          id,
		Desired:     desired,
		Lease:       claimedLease,
		Reclaim:     req.Reclaim,
		ReleaseOnly: req.ReleaseOnly,
		Repo:        repoForProtocol(req.Repo),
	})
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if response.Lease == nil {
		return core.LeaseTarget{}, core.Exit(4, "external provider could not resolve %q", req.ID)
	}
	if desired != nil {
		if err := validateAndFillDesired(response.Lease, desired); err != nil {
			return core.LeaseTarget{}, err
		}
		preserveLifecycleLabels(response.Lease, claimLabels)
	} else if strings.TrimSpace(response.Lease.LeaseID) == "" {
		return core.LeaseTarget{}, core.Exit(5, "external provider resolve returned no stable leaseId for %q", req.ID)
	}
	lease := response.Lease.target(b.cfg, keep)
	if err := validateLease(lease, !req.ReleaseOnly, !req.ReleaseOnly); err != nil {
		return core.LeaseTarget{}, err
	}
	if req.ReleaseOnly {
		return lease, nil
	}
	if _, err := core.PersistExternalRouting(lease.LeaseID, b.cfg.External); err != nil {
		return core.LeaseTarget{}, core.Exit(2, "%v", err)
	}
	if err := core.WaitForSSHReady(ctx, &lease.SSH, b.rt.Stderr, "external provider SSH", core.BootstrapWaitTimeout(b.cfg)); err != nil {
		return core.LeaseTarget{}, err
	}
	if req.Repo.Root != "" {
		slug := core.NormalizeLeaseSlug(lease.Server.Labels["slug"])
		if err := b.claimLeaseForRepo(lease.LeaseID, slug, req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
			return core.LeaseTarget{}, err
		}
		if err := core.UpdateLeaseClaimEndpoint(lease.LeaseID, lease.Server, lease.SSH); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *leaseBackend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	response, err := b.invoke(ctx, protocolRequest{Operation: "list", All: req.All, Refresh: req.Refresh})
	if err != nil {
		return nil, err
	}
	servers := make([]core.Server, 0, len(response.Leases))
	for _, item := range response.Leases {
		servers = append(servers, item.target(b.cfg, true).Server)
	}
	return servers, nil
}

func (b *leaseBackend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	response, err := b.invoke(ctx, protocolRequest{Operation: "doctor"})
	if err != nil {
		return core.DoctorResult{}, err
	}
	message := core.Blank(strings.TrimSpace(response.Message), "external provider ready")
	return core.DoctorResult{Provider: providerName, Message: message}, nil
}

func (b *leaseBackend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	if err := validateExternalReleaseLeaseID(req.Lease.LeaseID); err != nil {
		return err
	}
	_, err := b.invoke(ctx, protocolRequest{
		Operation: "release",
		Lease:     leaseForProtocol(req.Lease),
		Force:     req.Force,
	})
	if err == nil {
		if externalLeaseIDSafeForClaimPath(req.Lease.LeaseID) {
			core.RemoveLeaseClaim(req.Lease.LeaseID)
		}
		core.RemoveExternalRouting(req.Lease.LeaseID)
	}
	return err
}

func (b *leaseBackend) ReleaseLeaseMessage(lease core.LeaseTarget) string {
	return fmt.Sprintf("released external lease=%s name=%s", lease.LeaseID, lease.Server.Name)
}

func (b *leaseBackend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	response, err := b.invoke(ctx, protocolRequest{
		Operation: "touch",
		Lease:     leaseForProtocol(req.Lease),
		State:     req.State,
	})
	if err != nil {
		return core.Server{}, err
	}
	if response.Lease != nil {
		desired := &desiredLease{
			LeaseID: req.Lease.LeaseID,
			Slug:    req.Lease.Server.Labels["slug"],
			Name:    req.Lease.Server.Name,
		}
		if err := validateAndFillDesired(response.Lease, desired); err != nil {
			return core.Server{}, err
		}
		preserveLifecycleLabels(response.Lease, req.Lease.Server.Labels)
		return response.Lease.target(b.cfg, keepFromLabels(req.Lease.Server.Labels, true)).Server, nil
	}
	server := req.Lease.Server
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = core.TouchDirectLeaseLabels(server.Labels, b.cfg, req.State, time.Now().UTC())
	return server, nil
}

func (b *leaseBackend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	if _, err := b.invoke(ctx, protocolRequest{Operation: "cleanup", DryRun: req.DryRun}); err != nil {
		return err
	}
	if req.DryRun {
		return nil
	}
	response, err := b.invoke(ctx, protocolRequest{Operation: "list", All: true, Refresh: true})
	if err != nil {
		return err
	}
	live := make(map[string]struct{}, len(response.Leases))
	for index, lease := range response.Leases {
		leaseID := strings.TrimSpace(lease.LeaseID)
		if leaseID == "" {
			return core.Exit(5, "external provider list lease %d is missing leaseId", index)
		}
		live[leaseID] = struct{}{}
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return err
	}
	for _, claim := range claims {
		if claim.Provider != providerName || strings.TrimSpace(claim.ProviderScope) != b.claimScope() {
			continue
		}
		if _, ok := live[claim.LeaseID]; !ok {
			core.RemoveLeaseClaim(claim.LeaseID)
			core.RemoveExternalRouting(claim.LeaseID)
		}
	}
	return nil
}

func (b *leaseBackend) invoke(ctx context.Context, request protocolRequest) (protocolResponse, error) {
	if lifecycleConfigured(b.cfg.External) {
		return b.invokeLifecycle(ctx, request)
	}
	return b.invokeProtocol(ctx, request)
}

func (b *leaseBackend) invokeProtocol(ctx context.Context, request protocolRequest) (protocolResponse, error) {
	request.ProtocolVersion = protocolVersion
	request.Config = b.cfg.External.Config
	var stdin bytes.Buffer
	if err := json.NewEncoder(&stdin).Encode(request); err != nil {
		return protocolResponse{}, fmt.Errorf("encode external provider request: %w", err)
	}
	result, err := b.rt.Exec.Run(ctx, core.LocalCommandRequest{
		Name:   strings.TrimSpace(b.cfg.External.Command),
		Args:   append([]string(nil), b.cfg.External.Args...),
		Stdin:  &stdin,
		Stderr: b.rt.Stderr,
	})
	if err != nil {
		message := strings.TrimSpace(result.Stderr)
		if message == "" {
			message = strings.TrimSpace(result.Stdout)
		}
		return protocolResponse{}, core.Exit(result.ExitCode, "external provider command failed: %v: %s", err, message)
	}
	var response protocolResponse
	if err := json.Unmarshal([]byte(result.Stdout), &response); err != nil {
		return protocolResponse{}, core.Exit(5, "external provider returned invalid JSON: %v", err)
	}
	if message := strings.TrimSpace(response.Error); message != "" {
		return protocolResponse{}, core.Exit(5, "external provider: %s", message)
	}
	if response.ProtocolVersion != protocolVersion {
		return protocolResponse{}, core.Exit(5, "external provider protocol version %d is unsupported", response.ProtocolVersion)
	}
	return response, nil
}

func (b *leaseBackend) claimScope() string {
	return externalClaimScope(b.cfg)
}

type externalClaimScopeData struct {
	Command    string                         `json:"command,omitempty"`
	Args       []string                       `json:"args,omitempty"`
	Config     map[string]any                 `json:"config,omitempty"`
	Lifecycle  *core.ExternalLifecycleConfig  `json:"lifecycle,omitempty"`
	Connection *core.ExternalConnectionConfig `json:"connection,omitempty"`
}

func externalClaimScope(cfg core.Config) string {
	scope := externalClaimScopeData{
		Command: strings.TrimSpace(cfg.External.Command),
		Args:    append([]string(nil), cfg.External.Args...),
		Config:  cfg.External.Config,
	}
	if lifecycleConfigured(cfg.External) {
		scope.Lifecycle = &cfg.External.Lifecycle
		scope.Connection = &cfg.External.Connection
	}
	data, err := json.Marshal(scope)
	if err != nil {
		data = []byte(strings.TrimSpace(cfg.External.Command) + "\x00" + strings.Join(cfg.External.Args, "\x00"))
	}
	sum := sha256.Sum256(data)
	return "sha256:" + fmt.Sprintf("%x", sum[:12])
}

func lifecycleConfigured(cfg core.ExternalConfig) bool {
	return lifecycleOperationConfigured(cfg.Lifecycle.Acquire)
}

func (b *leaseBackend) claimLeaseForRepo(leaseID, slug, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProviderScope(leaseID, slug, providerName, b.claimScope(), repoRoot, idleTimeout, reclaim)
}

func (b *leaseBackend) allocateLeaseSlug(leaseID, requested string) (string, *slugReservation, error) {
	base := core.NormalizeLeaseSlug(requested)
	if base == "" {
		base = core.NewLeaseSlug(leaseID)
	}
	for attempt := 0; attempt < 40; attempt++ {
		slug := base
		if attempt > 0 {
			slug = core.SlugWithCollisionSuffix(base, fmt.Sprintf("%s-%d", leaseID, attempt-1))
		}
		inUse := false
		var err error
		inUse, err = b.claimSlugInUse(slug, leaseID)
		if err != nil {
			return "", nil, err
		}
		if !inUse {
			reservation, reserved, err := b.reserveLeaseSlug(slug, leaseID)
			if err != nil {
				return "", nil, err
			}
			if reserved {
				return slug, reservation, nil
			}
		}
	}
	return "", nil, core.Exit(4, "could not reserve external lease slug %q in this lifecycle scope", base)
}

type slugReservation struct {
	path  string
	token string
}

type slugReservationRecord struct {
	LeaseID   string `json:"leaseID"`
	Slug      string `json:"slug"`
	CreatedAt string `json:"createdAt"`
	Token     string `json:"token"`
	PID       int    `json:"pid,omitempty"`
}

func (r *slugReservation) Release() {
	if r == nil || r.path == "" || r.token == "" {
		return
	}
	unlock, err := waitForSlugReservationLock(r.path, 2*time.Second)
	if err != nil {
		return
	}
	defer unlock()
	data, err := os.ReadFile(r.path)
	if err != nil {
		return
	}
	var record slugReservationRecord
	if err := json.Unmarshal(data, &record); err != nil || record.Token != r.token {
		return
	}
	_ = os.Remove(r.path)
}

func (b *leaseBackend) reserveLeaseSlug(slug, leaseID string) (*slugReservation, bool, error) {
	dir, err := b.slugReservationDir()
	if err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, fmt.Errorf("create external slug reservation dir: %w", err)
	}
	path := slugReservationPath(dir, slug)
	unlock, err := waitForSlugReservationLock(path, 30*time.Second)
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	inUse, err := b.claimSlugInUse(slug, leaseID)
	if err != nil {
		return nil, false, err
	}
	if inUse {
		return nil, false, nil
	}
	token, err := newSlugReservationToken()
	if err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		reclaimed, err := reclaimStaleSlugReservation(path)
		if err != nil {
			return nil, false, err
		}
		if reclaimed {
			file, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err == nil {
				if err := writeSlugReservation(file, leaseID, slug, token); err != nil {
					_ = os.Remove(path)
					return nil, false, err
				}
				return &slugReservation{path: path, token: token}, true, nil
			}
			if !errors.Is(err, os.ErrExist) {
				return nil, false, fmt.Errorf("reserve external slug %s: %w", slug, err)
			}
		}
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reserve external slug %s: %w", slug, err)
	}
	if err := writeSlugReservation(file, leaseID, slug, token); err != nil {
		_ = os.Remove(path)
		return nil, false, err
	}
	return &slugReservation{path: path, token: token}, true, nil
}

func slugReservationPath(dir, slug string) string {
	sum := sha256.Sum256([]byte(slug))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json")
}

func waitForSlugReservationLock(path string, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	for {
		unlock, locked, err := lockSlugReservation(path)
		if err != nil {
			return nil, err
		}
		if locked {
			return unlock, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out locking external slug reservation")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func reclaimStaleSlugReservation(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat external slug reservation: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read external slug reservation: %w", err)
	}
	var record slugReservationRecord
	if err := json.Unmarshal(data, &record); err != nil || strings.TrimSpace(record.CreatedAt) == "" {
		if time.Since(info.ModTime()) <= externalSlugReservationTTL {
			return false, nil
		}
		return removeStaleSlugReservation(path)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, record.CreatedAt)
	if err != nil {
		if time.Since(info.ModTime()) <= externalSlugReservationTTL {
			return false, nil
		}
		return removeStaleSlugReservation(path)
	}
	if time.Since(createdAt) <= externalSlugReservationTTL {
		return false, nil
	}
	if slugReservationOwnerActive(record.PID) {
		return false, nil
	}
	return removeStaleSlugReservation(path)
}

func writeSlugReservation(file *os.File, leaseID, slug, token string) error {
	record := slugReservationRecord{LeaseID: leaseID, Slug: slug, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Token: token, PID: os.Getpid()}
	if err := json.NewEncoder(file).Encode(record); err != nil {
		_ = file.Close()
		return fmt.Errorf("write external slug reservation: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close external slug reservation: %w", err)
	}
	return nil
}

func newSlugReservationToken() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("generate external slug reservation token: %w", err)
	}
	return hex.EncodeToString(data[:]), nil
}

func removeStaleSlugReservation(path string) (bool, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("remove stale external slug reservation: %w", err)
	}
	return true, nil
}

func (b *leaseBackend) slugReservationDir() (string, error) {
	dir, err := core.CrabboxStateDir()
	if err != nil {
		return "", err
	}
	scope := strings.TrimPrefix(b.claimScope(), "sha256:")
	return filepath.Join(dir, "external-slug-reservations", scope), nil
}

func (b *leaseBackend) claimSlugInUse(slug, leaseID string) (bool, error) {
	slug = core.NormalizeLeaseSlug(slug)
	if slug == "" {
		return false, nil
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return false, err
	}
	scope := b.claimScope()
	for _, claim := range claims {
		if !externalClaimMatchesScope(claim, scope) {
			continue
		}
		if claim.LeaseID != "" && claim.LeaseID != leaseID && core.NormalizeLeaseSlug(claim.Slug) == slug {
			return true, nil
		}
	}
	return false, nil
}

func (b *leaseBackend) resolveClaim(identifier string) (core.LeaseClaim, bool, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return core.LeaseClaim{}, false, nil
	}
	scope := b.claimScope()
	if claim, err := core.ReadLeaseClaim(identifier); err != nil {
		return core.LeaseClaim{}, false, err
	} else if b.claimMatchesScopeOrRouting(claim, scope) {
		return claim, true, nil
	} else if claim.LeaseID != "" && strings.HasPrefix(identifier, "cbx_") {
		return core.LeaseClaim{}, false, nil
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	var match core.LeaseClaim
	for _, claim := range claims {
		if !b.claimMatchesScopeOrRouting(claim, scope) {
			continue
		}
		if core.LeaseClaimMatchesIdentifier(claim, identifier) {
			if match.LeaseID != "" && match.LeaseID != claim.LeaseID {
				return core.LeaseClaim{}, false, core.Exit(4, "external provider claim %q is ambiguous in this lifecycle scope", identifier)
			}
			match = claim
		}
	}
	if match.LeaseID != "" {
		return match, true, nil
	}
	return core.LeaseClaim{}, false, nil
}

func (b *leaseBackend) claimMatchesScopeOrRouting(claim core.LeaseClaim, scope string) bool {
	if externalClaimMatchesScope(claim, scope) {
		return true
	}
	if claim.LeaseID == "" || claim.Provider != providerName || strings.TrimSpace(b.cfg.External.RoutingFile) == "" {
		return false
	}
	path, err := core.ExternalRoutingPath(claim.LeaseID)
	return err == nil && filepath.Clean(path) == filepath.Clean(b.cfg.External.RoutingFile)
}

func externalClaimMatchesScope(claim core.LeaseClaim, scope string) bool {
	return claim.LeaseID != "" && claim.Provider == providerName && strings.TrimSpace(claim.ProviderScope) == scope
}

func fillDesired(lease *protocolLease, desired *desiredLease) {
	if lease.LeaseID == "" {
		lease.LeaseID = desired.LeaseID
	}
	if lease.Slug == "" {
		lease.Slug = desired.Slug
	}
	if lease.Name == "" {
		lease.Name = desired.Name
	}
}

func validateAndFillDesired(lease *protocolLease, desired *desiredLease) error {
	if lease.LeaseID != "" && lease.LeaseID != desired.LeaseID {
		return core.Exit(4, "external provider lease identity changed: expected %s, found %s", desired.LeaseID, lease.LeaseID)
	}
	if slug := core.NormalizeLeaseSlug(lease.Slug); slug != "" && slug != core.NormalizeLeaseSlug(desired.Slug) {
		return core.Exit(4, "external provider lease slug changed: expected %s, found %s", desired.Slug, lease.Slug)
	}
	if lease.Name != "" && lease.Name != desired.Name {
		return core.Exit(4, "external provider lease name changed: expected %s, found %s", desired.Name, lease.Name)
	}
	fillDesired(lease, desired)
	return nil
}

var lifecycleLabelKeys = []string{
	externalResourceNameLabel,
	externalResourceNameFromEnv,
	"keep",
	"created_at",
	"last_touched_at",
	"idle_timeout",
	"idle_timeout_secs",
	"ttl_secs",
	"expires_at",
}

func preserveLifecycleLabels(lease *protocolLease, labels map[string]string) {
	if len(labels) == 0 {
		return
	}
	if lease.Labels == nil {
		lease.Labels = map[string]string{}
	}
	for _, key := range lifecycleLabelKeys {
		if value := strings.TrimSpace(labels[key]); value != "" {
			lease.Labels[key] = value
		}
	}
}

func keepFromLabels(labels map[string]string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(labels["keep"])) {
	case "true":
		return true
	case "false":
		return false
	default:
		return fallback
	}
}

func leaseSlugForClaim(lease core.LeaseTarget, fallback string) string {
	if slug := core.NormalizeLeaseSlug(lease.Server.Labels["slug"]); slug != "" {
		return slug
	}
	return core.NormalizeLeaseSlug(fallback)
}

func validateLease(lease core.LeaseTarget, requireSSH, requireCanonicalLeaseID bool) error {
	if requireCanonicalLeaseID {
		if err := validateExternalCanonicalLeaseID(lease.LeaseID); err != nil {
			return err
		}
	} else if err := validateExternalReleaseLeaseID(lease.LeaseID); err != nil {
		return err
	}
	if strings.TrimSpace(lease.Server.Name) == "" {
		return core.Exit(5, "external provider lease name is required")
	}
	if requireSSH {
		if strings.TrimSpace(lease.SSH.Host) == "" || strings.TrimSpace(lease.SSH.User) == "" {
			return core.Exit(5, "external provider SSH host and user are required")
		}
	}
	return nil
}

func validateExternalCanonicalLeaseID(leaseID string) error {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return core.Exit(5, "external provider leaseId is required")
	}
	if !core.IsCanonicalLeaseID(leaseID) {
		return core.Exit(5, "external provider leaseId %q must be the Crabbox-generated cbx_... id; put provider resource ids in cloudId", leaseID)
	}
	return nil
}

func validateExternalReleaseLeaseID(leaseID string) error {
	if strings.TrimSpace(leaseID) == "" {
		return core.Exit(5, "external provider leaseId is required")
	}
	return nil
}

func externalLeaseIDSafeForClaimPath(leaseID string) bool {
	leaseID = strings.TrimSpace(leaseID)
	return leaseID != "" && !strings.ContainsAny(leaseID, `/\`)
}
