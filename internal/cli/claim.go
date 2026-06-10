package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	setCacheVolumes bool
	cacheVolumes    []string
	setEndpoint     bool
	server          Server
	target          SSHTarget
}

func claimLeaseForRepoProviderScopePondDetails(leaseID, slug, provider, providerScope, pond string, staticDetails staticClaimDetails, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerScope, pond, staticDetails, repoRoot, idleTimeout, reclaim, claimMetadata{})
}

func claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerScope, pond string, staticDetails staticClaimDetails, repoRoot string, idleTimeout time.Duration, reclaim bool, metadata claimMetadata) error {
	if leaseID == "" || repoRoot == "" {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	return mutateLeaseClaim(leaseID, func(existing *leaseClaim) error {
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
		return nil
	})
}

func updateLeaseClaimEndpoint(leaseID string, server Server, target SSHTarget) error {
	if leaseID == "" {
		return nil
	}
	return mutateLeaseClaim(leaseID, func(claim *leaseClaim) error {
		if claim.LeaseID == "" {
			return nil
		}
		applyLeaseClaimEndpoint(claim, server, target)
		return nil
	})
}

func applyLeaseClaimEndpoint(claim *leaseClaim, server Server, target SSHTarget) {
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
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return exit(2, "create claim directory: %v", err)
	}
	return withLeaseClaimLock(path, func() error {
		claim, err := readLeaseClaimPath(path)
		if err != nil {
			return err
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

func listLeaseClaims() ([]leaseClaim, error) {
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
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		var invalid invalidLeaseClaimIDError
		if errors.As(err, &invalid) {
			return leaseClaim{}, nil
		}
		return leaseClaim{}, err
	}
	var claim leaseClaim
	err = withLeaseClaimLock(path, func() error {
		var readErr error
		claim, readErr = readLeaseClaimPath(path)
		return readErr
	})
	if err != nil {
		return leaseClaim{}, err
	}
	return claim, nil
}

func readLeaseClaimPath(path string) (leaseClaim, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return leaseClaim{}, nil
	}
	if err != nil {
		return leaseClaim{}, exit(2, "read claim %s: %v", path, err)
	}
	var claim leaseClaim
	if err := json.Unmarshal(data, &claim); err != nil {
		return leaseClaim{}, exit(2, "parse claim %s: %v", path, err)
	}
	return claim, nil
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
