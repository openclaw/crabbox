package external

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type leaseBackend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime
}

func (b *leaseBackend) Spec() core.ProviderSpec { return b.spec }

func (b *leaseBackend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	leaseID := core.NewLeaseID()
	slug, err := b.allocateLeaseSlug(leaseID, req.RequestedSlug)
	if err != nil {
		return core.LeaseTarget{}, err
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
		if !req.Keep {
			_, _ = b.invoke(context.Background(), protocolRequest{Operation: "release", Lease: response.Lease})
		}
		return core.LeaseTarget{}, core.Exit(4, "external provider lease identity changed: expected %s, found %s", desired.LeaseID, response.Lease.LeaseID)
	}
	fillDesired(response.Lease, desired)
	lease := response.Lease.target(b.cfg, req.Keep)
	if err := validateLease(lease, true, true); err != nil {
		if !req.Keep {
			_, _ = b.invoke(context.Background(), protocolRequest{Operation: "release", Lease: leaseForProtocol(lease)})
		}
		return core.LeaseTarget{}, err
	}
	if _, err := core.PersistExternalRouting(lease.LeaseID, b.cfg.External); err != nil {
		if !req.Keep {
			_, _ = b.invoke(context.Background(), protocolRequest{Operation: "release", Lease: leaseForProtocol(lease)})
		}
		return core.LeaseTarget{}, core.Exit(2, "%v", err)
	}
	if err := core.WaitForSSHReady(ctx, &lease.SSH, b.rt.Stderr, "external provider SSH", core.BootstrapWaitTimeout(b.cfg)); err != nil {
		if !req.Keep {
			_, _ = b.invoke(context.Background(), protocolRequest{Operation: "release", Lease: leaseForProtocol(lease)})
			core.RemoveExternalRouting(lease.LeaseID)
		}
		return core.LeaseTarget{}, err
	}
	lease.Server.Status = "ready"
	lease.Server.Labels["state"] = "ready"
	if err := b.claimLeaseForRepo(lease.LeaseID, leaseSlugForClaim(lease, slug), req.Repo.Root, b.cfg.IdleTimeout, req.Reclaim); err != nil {
		if !req.Keep {
			_, _ = b.invoke(context.Background(), protocolRequest{Operation: "release", Lease: leaseForProtocol(lease)})
			core.RemoveExternalRouting(lease.LeaseID)
		}
		return core.LeaseTarget{}, err
	}
	if err := core.UpdateLeaseClaimEndpoint(lease.LeaseID, lease.Server, lease.SSH); err != nil {
		if !req.Keep {
			_, _ = b.invoke(context.Background(), protocolRequest{Operation: "release", Lease: leaseForProtocol(lease)})
			core.RemoveLeaseClaim(lease.LeaseID)
			core.RemoveExternalRouting(lease.LeaseID)
		}
		return core.LeaseTarget{}, err
	}
	return lease, nil
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
	return len(cfg.Lifecycle.Acquire.Argv) > 0
}

func (b *leaseBackend) claimLeaseForRepo(leaseID, slug, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProviderScope(leaseID, slug, providerName, b.claimScope(), repoRoot, idleTimeout, reclaim)
}

func (b *leaseBackend) allocateLeaseSlug(leaseID, requested string) (string, error) {
	base := core.NormalizeLeaseSlug(requested)
	checkClaims := base != ""
	if base == "" {
		base = core.NewLeaseSlug(leaseID)
	}
	slug := base
	for attempt := 0; attempt < 20; attempt++ {
		inUse := false
		var err error
		if checkClaims {
			inUse, err = b.claimSlugInUse(slug, leaseID)
		}
		if err != nil {
			return "", err
		}
		if !inUse {
			return slug, nil
		}
		slug = core.SlugWithCollisionSuffix(base, fmt.Sprintf("%s-%d", leaseID, attempt))
	}
	return core.SlugWithCollisionSuffix(base, leaseID), nil
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
	} else if externalClaimMatchesScope(claim, scope) {
		return claim, true, nil
	} else if claim.LeaseID != "" && strings.HasPrefix(identifier, "cbx_") {
		return core.LeaseClaim{}, false, nil
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	slug := core.NormalizeLeaseSlug(identifier)
	for _, claim := range claims {
		if !externalClaimMatchesScope(claim, scope) {
			continue
		}
		if claim.LeaseID == identifier || (slug != "" && core.NormalizeLeaseSlug(claim.Slug) == slug) {
			return claim, true, nil
		}
	}
	return core.LeaseClaim{}, false, nil
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
