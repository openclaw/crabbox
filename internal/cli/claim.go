package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

type leaseClaim struct {
	LeaseID            string            `json:"leaseID"`
	Slug               string            `json:"slug,omitempty"`
	Provider           string            `json:"provider,omitempty"`
	CloudID            string            `json:"cloudID,omitempty"`
	ProviderScope      string            `json:"providerScope,omitempty"`
	StaticHost         string            `json:"staticHost,omitempty"`
	StaticUser         string            `json:"staticUser,omitempty"`
	StaticPort         string            `json:"staticPort,omitempty"`
	StaticWorkRoot     string            `json:"staticWorkRoot,omitempty"`
	TargetOS           string            `json:"targetOS,omitempty"`
	WindowsMode        string            `json:"windowsMode,omitempty"`
	Pond               string            `json:"pond,omitempty"`
	RepoRoot           string            `json:"repoRoot"`
	ClaimedAt          string            `json:"claimedAt"`
	LastUsedAt         string            `json:"lastUsedAt"`
	IdleTimeoutSeconds int               `json:"idleTimeoutSeconds,omitempty"`
	TailscaleIPv4      string            `json:"tailscaleIPv4,omitempty"`
	TailscaleFQDN      string            `json:"tailscaleFQDN,omitempty"`
	TailscaleHostname  string            `json:"tailscaleHostname,omitempty"`
	TailscaleTags      []string          `json:"tailscaleTags,omitempty"`
	TailscaleLoginURL  string            `json:"tailscaleLoginURL,omitempty"`
	TailscaleExitNode  string            `json:"tailscaleExitNode,omitempty"`
	TailscaleExitLAN   bool              `json:"tailscaleExitLAN,omitempty"`
	SSHHost            string            `json:"sshHost,omitempty"`
	SSHPort            int               `json:"sshPort,omitempty"`
	BridgeURL          string            `json:"bridgeURL,omitempty"`
	CacheVolumes       []string          `json:"cacheVolumes,omitempty"`
	Labels             map[string]string `json:"labels,omitempty"`
}

var claimMutationMutexes sync.Map

type invalidLeaseClaimIDError struct{ id string }

func (e invalidLeaseClaimIDError) Error() string {
	return "invalid lease claim id " + strconv.Quote(e.id)
}

func claimLeaseForRepo(leaseID, slug, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProvider(leaseID, slug, "", repoRoot, idleTimeout, reclaim)
}

func claimLeaseForRepoConfig(leaseID, slug string, cfg Config, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	provider, staticDetails := claimProviderDetailsForConfig(cfg)
	return claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerClaimScope(provider, cfg), cfg.Pond, staticDetails, repoRoot, idleTimeout, reclaim, claimMetadata{
		setCacheVolumes: true,
		cacheVolumes:    CacheVolumeStickyDiskSpecs(cfg.Cache.Volumes),
	})
}

func claimLeaseTargetForRepoConfig(leaseID, slug string, cfg Config, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	provider, staticDetails := claimProviderDetailsForConfig(cfg)
	return claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerClaimScope(provider, cfg), cfg.Pond, staticDetails, repoRoot, idleTimeout, reclaim, claimMetadata{
		setCacheVolumes: true,
		cacheVolumes:    CacheVolumeStickyDiskSpecs(cfg.Cache.Volumes),
		setEndpoint:     true,
		server:          server,
		target:          target,
	})
}

func claimProviderDetailsForConfig(cfg Config) (string, staticClaimDetails) {
	provider := canonicalClaimProvider(cfg.Provider)
	staticDetails := staticClaimDetails{}
	if isStaticProvider(provider) {
		staticDetails = staticClaimDetails{
			Present:     true,
			Host:        strings.TrimSpace(cfg.Static.Host),
			User:        strings.TrimSpace(cfg.Static.User),
			Port:        strings.TrimSpace(cfg.Static.Port),
			WorkRoot:    strings.TrimSpace(cfg.Static.WorkRoot),
			TargetOS:    strings.TrimSpace(cfg.TargetOS),
			WindowsMode: strings.TrimSpace(cfg.WindowsMode),
		}
	}
	return provider, staticDetails
}

func claimLeaseForRepoProvider(leaseID, slug, provider, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProviderScopePond(leaseID, slug, provider, "", "", repoRoot, idleTimeout, reclaim)
}

func claimLeaseForRepoProviderScope(leaseID, slug, provider, providerScope, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProviderScopePond(leaseID, slug, provider, providerScope, "", repoRoot, idleTimeout, reclaim)
}

func claimLeaseForRepoProviderWithPond(leaseID, slug, provider, pond, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProviderScopePond(leaseID, slug, provider, "", pond, repoRoot, idleTimeout, reclaim)
}

func claimLeaseForRepoProviderScopePond(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProviderScopePondDetails(leaseID, slug, provider, providerScope, pond, staticClaimDetails{}, repoRoot, idleTimeout, reclaim)
}

func claimLeaseForRepoProviderScopePondCacheVolumes(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool, cacheVolumes []string) error {
	return claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerScope, pond, staticClaimDetails{}, repoRoot, idleTimeout, reclaim, claimMetadata{
		setCacheVolumes: true,
		cacheVolumes:    cacheVolumes,
	})
}

func claimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool, server Server, target SSHTarget) error {
	return claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerScope, pond, staticClaimDetails{}, repoRoot, idleTimeout, reclaim, claimMetadata{
		setEndpoint: true,
		server:      server,
		target:      target,
	})
}

type staticClaimDetails struct {
	Present     bool
	Host        string
	User        string
	Port        string
	WorkRoot    string
	TargetOS    string
	WindowsMode string
}

type claimMetadata struct {
	setCacheVolumes       bool
	cacheVolumes          []string
	setEndpoint           bool
	server                Server
	target                SSHTarget
	guard                 func(leaseClaim, bool) error
	result                *leaseClaim
	allowProviderMetadata bool
}

func claimLeaseForRepoProviderScopePondDetails(leaseID, slug, provider, providerScope, pond string, staticDetails staticClaimDetails, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerScope, pond, staticDetails, repoRoot, idleTimeout, reclaim, claimMetadata{})
}

func claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerScope, pond string, staticDetails staticClaimDetails, repoRoot string, idleTimeout time.Duration, reclaim bool, metadata claimMetadata) error {
	if leaseID == "" || repoRoot == "" {
		return nil
	}
	guard := metadata.guard
	if metadata.setEndpoint {
		guard = endpointClaimGuard(leaseID, metadata.guard)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	return mutateLeaseClaimGuarded(leaseID, guard, func(existing *leaseClaim) error {
		hadExisting := existing.LeaseID != ""
		original := cloneLeaseClaim(*existing)
		if metadata.setEndpoint && hadExisting {
			server, err := prepareLeaseClaimEndpoint(original, provider, slug, metadata.server, metadata.allowProviderMetadata)
			if err != nil {
				return err
			}
			metadata.server = server
		}
		if existing.LeaseID != "" && existing.RepoRoot != "" && existing.RepoRoot != repoRoot && !reclaim {
			return exit(2, "lease %s is claimed by repo %s; use --reclaim to claim it for %s", leaseID, existing.RepoRoot, repoRoot)
		}
		if existing.ClaimedAt == "" || reclaim || existing.RepoRoot != repoRoot {
			existing.ClaimedAt = now
		}
		existing.LeaseID = leaseID
		existing.Slug = slug
		if provider != "" {
			existing.Provider = provider
		}
		if providerScope != "" {
			existing.ProviderScope = providerScope
		}
		if pond = normalizePondName(pond); pond != "" {
			existing.Pond = pond
		}
		if staticDetails.Present {
			existing.StaticHost = staticDetails.Host
			existing.StaticUser = staticDetails.User
			existing.StaticPort = staticDetails.Port
			existing.StaticWorkRoot = staticDetails.WorkRoot
			existing.TargetOS = staticDetails.TargetOS
			existing.WindowsMode = staticDetails.WindowsMode
		} else if provider != "" && !isStaticProvider(provider) {
			existing.StaticHost = ""
			existing.StaticUser = ""
			existing.StaticPort = ""
			existing.StaticWorkRoot = ""
			existing.TargetOS = ""
			existing.WindowsMode = ""
		}
		existing.RepoRoot = repoRoot
		existing.LastUsedAt = now
		if idleTimeout > 0 {
			existing.IdleTimeoutSeconds = int(idleTimeout.Seconds())
		}
		if metadata.setCacheVolumes {
			existing.CacheVolumes = append([]string(nil), metadata.cacheVolumes...)
		}
		if metadata.setEndpoint {
			applyLeaseClaimEndpoint(existing, metadata.server, metadata.target)
		}
		if metadata.result != nil {
			*metadata.result = cloneLeaseClaim(*existing)
		}
		return nil
	})
}

func claimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug string, cfg Config, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool, expected leaseClaim, expectedExists bool) (leaseClaim, error) {
	provider, staticDetails := claimProviderDetailsForConfig(cfg)
	var updated leaseClaim
	err := claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerClaimScope(provider, cfg), cfg.Pond, staticDetails, repoRoot, idleTimeout, reclaim, claimMetadata{
		setCacheVolumes: true,
		cacheVolumes:    CacheVolumeStickyDiskSpecs(cfg.Cache.Volumes),
		setEndpoint:     true,
		server:          server,
		target:          target,
		guard:           unchangedLeaseClaimGuard(leaseID, expected, expectedExists),
		result:          &updated,
	})
	return updated, err
}

func updateLeaseClaimEndpoint(leaseID string, server Server, target SSHTarget) error {
	if leaseID == "" {
		return nil
	}
	return mutateLeaseClaimGuarded(leaseID, endpointClaimGuard(leaseID, nil), func(claim *leaseClaim) error {
		if claim.LeaseID == "" {
			return nil
		}
		provider := firstNonBlank(server.Labels["provider"], server.Provider)
		prepared, err := prepareLeaseClaimEndpoint(*claim, provider, server.Labels["slug"], server, false)
		if err != nil {
			return err
		}
		applyLeaseClaimEndpoint(claim, prepared, target)
		return nil
	})
}

func prepareLeaseClaimEndpoint(existing leaseClaim, providerName, slug string, server Server, allowProviderMetadata bool) (Server, error) {
	provider, err := ProviderFor(existing.Provider)
	if err != nil {
		return Server{}, exit(2, "lease %s claim has unavailable provider %q", existing.LeaseID, existing.Provider)
	}
	preparer, ok := provider.(LeaseClaimEndpointPreparer)
	if !ok {
		return server, nil
	}
	return preparer.PrepareLeaseClaimEndpoint(existing, providerName, slug, server, allowProviderMetadata)
}

func updateLeaseClaimEndpointIfUnchanged(leaseID string, expected leaseClaim, server Server, target SSHTarget) (leaseClaim, error) {
	return updateLeaseClaimEndpointIfUnchangedMode(leaseID, expected, server, target, false)
}

func updateLeaseClaimEndpointIfUnchangedWithProviderMetadata(leaseID string, expected leaseClaim, server Server, target SSHTarget) (leaseClaim, error) {
	return updateLeaseClaimEndpointIfUnchangedMode(leaseID, expected, server, target, true)
}

func updateLeaseClaimEndpointIfUnchangedMode(leaseID string, expected leaseClaim, server Server, target SSHTarget, allowProviderMetadata bool) (leaseClaim, error) {
	if leaseID == "" {
		return leaseClaim{}, nil
	}
	var updated leaseClaim
	err := mutateLeaseClaimGuarded(leaseID, endpointClaimGuard(leaseID, unchangedLeaseClaimGuard(leaseID, expected, true)), func(claim *leaseClaim) error {
		if claim.LeaseID == "" {
			return nil
		}
		provider := firstNonBlank(server.Labels["provider"], server.Provider)
		prepared, err := prepareLeaseClaimEndpoint(*claim, provider, server.Labels["slug"], server, allowProviderMetadata)
		if err != nil {
			return err
		}
		applyLeaseClaimEndpoint(claim, prepared, target)
		updated = cloneLeaseClaim(*claim)
		return nil
	})
	return updated, err
}

func updateLeaseClaimLabelsIfUnchanged(leaseID string, expected leaseClaim, labels map[string]string) (leaseClaim, error) {
	if leaseID == "" {
		return leaseClaim{}, nil
	}
	var updated leaseClaim
	err := mutateLeaseClaimGuarded(leaseID, unchangedLeaseClaimGuard(leaseID, expected, true), func(claim *leaseClaim) error {
		if claim.LeaseID == "" {
			return nil
		}
		claim.Labels = cloneStringMap(labels)
		updated = cloneLeaseClaim(*claim)
		return nil
	})
	return updated, err
}

func cloneLeaseClaim(claim leaseClaim) leaseClaim {
	claim.Labels = cloneStringMap(claim.Labels)
	claim.TailscaleTags = append([]string(nil), claim.TailscaleTags...)
	claim.CacheVolumes = append([]string(nil), claim.CacheVolumes...)
	return claim
}

func unchangedLeaseClaimGuard(leaseID string, expected leaseClaim, expectedExists bool) func(leaseClaim, bool) error {
	return func(existing leaseClaim, exists bool) error {
		if exists != expectedExists || (exists && !reflect.DeepEqual(existing, expected)) {
			return exit(2, "lease %s claim changed; retry", leaseID)
		}
		return nil
	}
}

func endpointClaimGuard(leaseID string, next func(leaseClaim, bool) error) func(leaseClaim, bool) error {
	return func(existing leaseClaim, exists bool) error {
		if exists && existing.LeaseID == "" {
			return exit(2, "lease %s claim is incomplete; refusing endpoint rewrite", leaseID)
		}
		if next != nil {
			return next(existing, exists)
		}
		return nil
	}
}

func applyLeaseClaimEndpoint(claim *leaseClaim, server Server, target SSHTarget) {
	if server.CloudID != "" {
		claim.CloudID = server.CloudID
	}
	if len(server.Labels) > 0 {
		claim.Labels = cloneStringMap(server.Labels)
	}
	meta := serverTailscaleMetadata(server)
	if meta.IPv4 != "" {
		claim.TailscaleIPv4 = meta.IPv4
	}
	if meta.FQDN != "" {
		claim.TailscaleFQDN = meta.FQDN
	}
	if target.NetworkKind == NetworkTailscale && target.Host != "" && claim.TailscaleFQDN == "" && claim.TailscaleIPv4 == "" {
		claim.TailscaleFQDN = target.Host
	}
	if target.Host != "" {
		claim.SSHHost = target.Host
	} else if statusTerminalState(server.Labels["state"]) {
		claim.SSHHost = ""
	}
	if port, err := strconv.Atoi(strings.TrimSpace(target.Port)); err == nil && port > 0 {
		claim.SSHPort = port
	} else if statusTerminalState(server.Labels["state"]) {
		claim.SSHPort = 0
	}
}

// updateLeaseClaimTailscale records a tailnet endpoint on an existing claim.
// Delegated-run providers (e.g. islo) have no SSH lease and so cannot go
// through updateLeaseClaimEndpoint; they call this after joining the tailnet
// out-of-band so health, ACL, and pond discovery can report enrollment.
func updateLeaseClaimTailscale(leaseID, ipv4, fqdn string) error {
	if leaseID == "" {
		return nil
	}
	return mutateLeaseClaim(leaseID, func(claim *leaseClaim) error {
		if claim.LeaseID == "" {
			return nil
		}
		setLeaseClaimTailscale(claim, ipv4, fqdn)
		return nil
	})
}

func updateLeaseClaimTailscaleSettings(leaseID, hostname string, tags []string, loginURL, exitNode string, exitLAN bool) error {
	if leaseID == "" {
		return nil
	}
	return mutateLeaseClaim(leaseID, func(claim *leaseClaim) error {
		if claim.LeaseID == "" {
			return nil
		}
		claim.TailscaleHostname = hostname
		claim.TailscaleTags = append([]string(nil), tags...)
		claim.TailscaleLoginURL = loginURL
		claim.TailscaleExitNode = exitNode
		claim.TailscaleExitLAN = exitLAN
		return nil
	})
}

func setLeaseClaimTailscale(claim *leaseClaim, ipv4, fqdn string) {
	if ipv4 != "" {
		claim.TailscaleIPv4 = ipv4
	}
	if fqdn != "" {
		claim.TailscaleFQDN = fqdn
	}
	if claim.TailscaleIPv4 == "" && claim.TailscaleFQDN == "" {
		return
	}
	if claim.Labels == nil {
		claim.Labels = map[string]string{}
	}
	claim.Labels["tailscale"] = "true"
	claim.Labels["tailscale_state"] = "ready"
	if claim.TailscaleIPv4 != "" {
		claim.Labels["tailscale_ipv4"] = claim.TailscaleIPv4
	}
	if claim.TailscaleFQDN != "" {
		claim.Labels["tailscale_fqdn"] = claim.TailscaleFQDN
	}
}

func clearLeaseClaimTailscale(leaseID string) error {
	if leaseID == "" {
		return nil
	}
	return mutateLeaseClaim(leaseID, func(claim *leaseClaim) error {
		if claim.LeaseID == "" {
			return nil
		}
		clearLeaseClaimTailscaleFields(claim)
		return nil
	})
}

func clearLeaseClaimTailscaleFields(claim *leaseClaim) {
	claim.TailscaleIPv4 = ""
	claim.TailscaleFQDN = ""
	for _, key := range []string{"tailscale", "tailscale_state", "tailscale_ipv4", "tailscale_fqdn"} {
		delete(claim.Labels, key)
	}
}

func updateLeaseClaimCacheVolumes(leaseID string, specs []string) error {
	if leaseID == "" {
		return nil
	}
	return mutateLeaseClaim(leaseID, func(claim *leaseClaim) error {
		if claim.LeaseID == "" {
			return nil
		}
		claim.CacheVolumes = append([]string(nil), specs...)
		return nil
	})
}

func mutateLeaseClaim(leaseID string, mutate func(*leaseClaim) error) error {
	return mutateLeaseClaimGuarded(leaseID, nil, mutate)
}

func mutateLeaseClaimGuarded(leaseID string, guard func(leaseClaim, bool) error, mutate func(*leaseClaim) error) error {
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return exit(2, "create claim directory: %v", err)
	}
	return withLeaseClaimLock(path, func() error {
		claim, exists, err := readLeaseClaimPathWithPresence(path)
		if err != nil {
			return err
		}
		if err := validateLeaseClaimFileIdentity(leaseID, claim, exists); err != nil {
			return err
		}
		if guard != nil {
			if err := guard(claim, exists); err != nil {
				return err
			}
		}
		if err := mutate(&claim); err != nil {
			return err
		}
		if claim.LeaseID == "" {
			return nil
		}
		return writeLeaseClaimAtomic(path, claim)
	})
}

func claimMutationMutex(path string) *sync.Mutex {
	value, _ := claimMutationMutexes.LoadOrStore(path, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func withLeaseClaimLock(path string, fn func() error) error {
	lockPath, err := leaseClaimLockPath(path)
	if err != nil {
		return err
	}
	mu := claimMutationMutex(lockPath)
	mu.Lock()
	defer mu.Unlock()

	lock := flock.New(lockPath, flock.SetPermissions(0o600))
	if err := lock.Lock(); err != nil {
		return exit(2, "lock claim %s: %v", path, err)
	}
	defer lock.Unlock()
	return fn()
}

func leaseClaimLockPath(path string) (string, error) {
	dir := filepath.Join(filepath.Dir(filepath.Dir(path)), "claim-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", exit(2, "create claim lock directory: %v", err)
	}
	return filepath.Join(dir, filepath.Base(path)+".lock"), nil
}

func writeLeaseClaimAtomic(path string, claim leaseClaim) error {
	data, err := json.MarshalIndent(claim, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return exit(2, "write claim %s: %v", path, err)
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return exit(2, "write claim %s: %v", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return exit(2, "write claim %s: %v", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return exit(2, "write claim %s: %v", path, err)
	}
	if err := tmp.Close(); err != nil {
		return exit(2, "write claim %s: %v", path, err)
	}
	if err := replaceClaimFile(tmpPath, path); err != nil {
		return exit(2, "write claim %s: %v", path, err)
	}
	removeTemp = false
	fsyncDir(dir)
	return nil
}

func fsyncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	defer f.Close()
	_ = f.Sync()
}

func canonicalClaimProvider(provider string) string {
	if resolved, err := ProviderFor(provider); err == nil {
		return resolved.Name()
	}
	return normalizeProviderName(provider)
}

func providerClaimScope(provider string, cfg Config) string {
	switch provider {
	case "gcp":
		if cfg.GCPProject != "" {
			return "project:" + cfg.GCPProject
		}
	}
	return ""
}

func resolveLeaseClaim(identifier string) (leaseClaim, bool, error) {
	if identifier == "" {
		return leaseClaim{}, false, nil
	}
	if claim, err := readLeaseClaim(identifier); err != nil {
		return leaseClaim{}, false, err
	} else if claim.LeaseID != "" {
		return claim, true, nil
	}
	dir, err := crabboxStateDir()
	if err != nil {
		return leaseClaim{}, false, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "claims"))
	if errors.Is(err, os.ErrNotExist) {
		return leaseClaim{}, false, nil
	}
	if err != nil {
		return leaseClaim{}, false, exit(2, "read claims directory: %v", err)
	}
	slug := normalizeLeaseSlug(identifier)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		leaseID := strings.TrimSuffix(entry.Name(), ".json")
		claim, err := readLeaseClaim(leaseID)
		if err != nil {
			return leaseClaim{}, false, err
		}
		if claim.LeaseID == identifier || (slug != "" && normalizeLeaseSlug(claim.Slug) == slug) {
			return claim, true, nil
		}
	}
	return leaseClaim{}, false, nil
}

func resolveLeaseClaimForProvider(identifier, provider string) (leaseClaim, bool, error) {
	if provider == "" {
		return resolveLeaseClaim(identifier)
	}
	claim, ok, err := resolveLeaseClaim(identifier)
	if err != nil || !ok {
		return claim, ok, err
	}
	if claim.Provider == provider {
		return claim, true, nil
	}
	claim, ok, err = findLeaseClaim(identifier, func(candidate leaseClaim) bool {
		return candidate.Provider == provider
	})
	if err != nil || !ok {
		return leaseClaim{}, false, err
	}
	return claim, true, nil
}

func resolveLeaseClaimForProviderWithExact(identifier, provider string) (leaseClaim, bool, bool, error) {
	if identifier == "" {
		return leaseClaim{}, false, false, nil
	}
	exact, exists, err := readLeaseClaimWithPresence(identifier)
	if err != nil {
		return leaseClaim{}, false, exists, err
	}
	if exists {
		if exact.LeaseID == "" || exact.Provider != provider {
			return exact, false, true, nil
		}
		return exact, true, true, nil
	}
	claim, ok, err := resolveLeaseClaimForProvider(identifier, provider)
	return claim, ok, false, err
}

func resolveLeaseClaimForProviderCloudID(cloudID, provider string) (leaseClaim, bool, error) {
	if cloudID == "" || provider == "" {
		return leaseClaim{}, false, nil
	}
	dir, err := crabboxStateDir()
	if err != nil {
		return leaseClaim{}, false, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "claims"))
	if errors.Is(err, os.ErrNotExist) {
		return leaseClaim{}, false, nil
	}
	if err != nil {
		return leaseClaim{}, false, exit(2, "read claims directory: %v", err)
	}
	var match leaseClaim
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		claim, err := readLeaseClaim(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return leaseClaim{}, false, err
		}
		if claim.Provider != provider || claim.CloudID != cloudID {
			continue
		}
		if match.LeaseID != "" {
			return leaseClaim{}, false, exit(2, "multiple provider=%s claims match cloud id %s", provider, cloudID)
		}
		match = claim
	}
	return match, match.LeaseID != "", nil
}

func leaseClaimMatchesIdentifier(claim leaseClaim, identifier string) bool {
	if identifier == "" {
		return false
	}
	if claim.LeaseID == identifier || claim.CloudID == identifier {
		return true
	}
	slug := normalizeLeaseSlug(identifier)
	return slug != "" && normalizeLeaseSlug(claim.Slug) == slug
}

func findLeaseClaim(identifier string, match func(leaseClaim) bool) (leaseClaim, bool, error) {
	if identifier == "" {
		return leaseClaim{}, false, nil
	}
	dir, err := crabboxStateDir()
	if err != nil {
		return leaseClaim{}, false, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "claims"))
	if errors.Is(err, os.ErrNotExist) {
		return leaseClaim{}, false, nil
	}
	if err != nil {
		return leaseClaim{}, false, exit(2, "read claims directory: %v", err)
	}
	slug := normalizeLeaseSlug(identifier)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		leaseID := strings.TrimSuffix(entry.Name(), ".json")
		claim, err := readLeaseClaim(leaseID)
		if err != nil {
			return leaseClaim{}, false, err
		}
		if (claim.LeaseID == identifier || (slug != "" && normalizeLeaseSlug(claim.Slug) == slug)) && match(claim) {
			return claim, true, nil
		}
	}
	return leaseClaim{}, false, nil
}

func removeLeaseClaim(leaseID string) {
	path, err := leaseClaimPath(leaseID)
	if err == nil {
		_ = withLeaseClaimLock(path, func() error {
			err := os.Remove(path)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		})
	}
}

func removeLeaseClaimIfUnchanged(leaseID string, expected leaseClaim) error {
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		return err
	}
	return withLeaseClaimLock(path, func() error {
		claim, exists, err := readLeaseClaimPathWithPresence(path)
		if err != nil {
			return err
		}
		if err := unchangedLeaseClaimGuard(leaseID, expected, true)(claim, exists); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return exit(2, "remove claim %s: %v", path, err)
		}
		return nil
	})
}

func listLeaseClaims() ([]leaseClaim, error) {
	return listLeaseClaimsWithPrefix("")
}

func listLeaseClaimsWithPrefix(prefix string) ([]leaseClaim, error) {
	dir, err := crabboxStateDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "claims"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, exit(2, "read claims directory: %v", err)
	}
	claims := make([]leaseClaim, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		leaseID := strings.TrimSuffix(entry.Name(), ".json")
		if prefix != "" && !strings.HasPrefix(leaseID, prefix) {
			continue
		}
		claim, err := readLeaseClaim(leaseID)
		if err != nil {
			return nil, err
		}
		if claim.LeaseID != "" {
			claims = append(claims, claim)
		}
	}
	return claims, nil
}

func readLeaseClaim(leaseID string) (leaseClaim, error) {
	claim, _, err := readLeaseClaimWithPresence(leaseID)
	return claim, err
}

func readLeaseClaimWithPresence(leaseID string) (leaseClaim, bool, error) {
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		var invalid invalidLeaseClaimIDError
		if errors.As(err, &invalid) {
			return leaseClaim{}, false, nil
		}
		return leaseClaim{}, false, err
	}
	var claim leaseClaim
	var exists bool
	err = withLeaseClaimLock(path, func() error {
		var readErr error
		claim, exists, readErr = readLeaseClaimPathWithPresence(path)
		return readErr
	})
	if err != nil {
		return leaseClaim{}, exists, err
	}
	if err := validateLeaseClaimFileIdentity(leaseID, claim, exists); err != nil {
		return leaseClaim{}, exists, err
	}
	return claim, exists, nil
}

func validateLeaseClaimFileIdentity(leaseID string, claim leaseClaim, exists bool) error {
	if exists && claim.LeaseID != "" && claim.LeaseID != leaseID {
		return exit(2, "claim file %s contains lease id %s; refusing misfiled claim", leaseID, claim.LeaseID)
	}
	return nil
}

func leaseClaimExists(leaseID string) (bool, error) {
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		var invalid invalidLeaseClaimIDError
		if errors.As(err, &invalid) {
			return false, nil
		}
		return false, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, exit(2, "inspect claim %s: %v", path, err)
	}
	return true, nil
}

func readLeaseClaimPathWithPresence(path string) (leaseClaim, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return leaseClaim{}, false, nil
	}
	if err != nil {
		return leaseClaim{}, true, exit(2, "read claim %s: %v", path, err)
	}
	var claim leaseClaim
	if err := json.Unmarshal(data, &claim); err != nil {
		return leaseClaim{}, true, exit(2, "parse claim %s: %v", path, err)
	}
	return claim, true, nil
}

func leaseClaimPath(leaseID string) (string, error) {
	if leaseID != strings.TrimSpace(leaseID) {
		return "", invalidLeaseClaimIDError{id: leaseID}
	}
	if !validLeaseClaimID(leaseID) {
		return "", invalidLeaseClaimIDError{id: leaseID}
	}
	dir, err := crabboxStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "claims", leaseID+".json"), nil
}

func validLeaseClaimID(leaseID string) bool {
	if leaseID == "" || leaseID == "." || leaseID == ".." {
		return false
	}
	if strings.ContainsAny(leaseID, `<>:"/\|?*`) || strings.ContainsRune(leaseID, 0) || strings.HasSuffix(leaseID, ".") {
		return false
	}
	for _, r := range leaseID {
		if r < 32 {
			return false
		}
	}
	name := strings.ToUpper(leaseID)
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	switch name {
	case "CON", "PRN", "AUX", "NUL":
		return false
	}
	if len(name) == 4 && (strings.HasPrefix(name, "COM") || strings.HasPrefix(name, "LPT")) && name[3] >= '1' && name[3] <= '9' {
		return false
	}
	return true
}

func crabboxStateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "crabbox"), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", exit(2, "user state directory is unavailable")
	}
	return filepath.Join(dir, "crabbox", "state"), nil
}
