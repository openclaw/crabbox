package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type leaseClaim struct {
	LeaseID           string                    `json:"leaseID"`
	Revision          string                    `json:"revision,omitempty"`
	CheckpointCapture *CheckpointCaptureBinding `json:"checkpointCapture,omitempty"`
	Slug              string                    `json:"slug,omitempty"`
	Provider          string                    `json:"provider,omitempty"`
	CloudID           string                    `json:"cloudID,omitempty"`
	// CloudNumericID binds providers whose CloudID is a reusable resource name.
	CloudNumericID int64 `json:"cloudNumericID,omitempty"`
	// CloudImmutableID binds providers whose immutable identity is a string.
	CloudImmutableID                    string             `json:"cloudImmutableID,omitempty"`
	ProviderScope                       string             `json:"providerScope,omitempty"`
	StaticHost                          string             `json:"staticHost,omitempty"`
	StaticUser                          string             `json:"staticUser,omitempty"`
	StaticPort                          string             `json:"staticPort,omitempty"`
	StaticWorkRoot                      string             `json:"staticWorkRoot,omitempty"`
	TargetOS                            string             `json:"targetOS,omitempty"`
	WindowsMode                         string             `json:"windowsMode,omitempty"`
	Pond                                string             `json:"pond,omitempty"`
	RepoRoot                            string             `json:"repoRoot"`
	ClaimedAt                           string             `json:"claimedAt"`
	LastUsedAt                          string             `json:"lastUsedAt"`
	IdleTimeoutSeconds                  int                `json:"idleTimeoutSeconds,omitempty"`
	TailscaleIPv4                       string             `json:"tailscaleIPv4,omitempty"`
	TailscaleFQDN                       string             `json:"tailscaleFQDN,omitempty"`
	TailscaleHostname                   string             `json:"tailscaleHostname,omitempty"`
	TailscaleTags                       []string           `json:"tailscaleTags,omitempty"`
	TailscaleLoginURL                   string             `json:"tailscaleLoginURL,omitempty"`
	TailscaleExitNode                   string             `json:"tailscaleExitNode,omitempty"`
	TailscaleExitLAN                    bool               `json:"tailscaleExitLAN,omitempty"`
	SSHHost                             string             `json:"sshHost,omitempty"`
	SSHPort                             int                `json:"sshPort,omitempty"`
	BridgeURL                           string             `json:"bridgeURL,omitempty"`
	CoordinatorRegistrationURL          string             `json:"coordinatorRegistrationURL,omitempty"`
	RuntimeAdapterRegistrationID        string             `json:"runtimeAdapterRegistrationID,omitempty"`
	RuntimeAdapterPendingRegistrationID string             `json:"runtimeAdapterPendingRegistrationID,omitempty"`
	CacheVolumes                        []string           `json:"cacheVolumes,omitempty"`
	Labels                              map[string]string  `json:"labels,omitempty"`
	FixedCreateIntent                   *FixedCreateIntent `json:"fixedCreateIntent,omitempty"`
}

type CheckpointCaptureBinding struct {
	ID            string `json:"id"`
	Revision      string `json:"revision"`
	BoundRevision string `json:"boundRevision"`
}

// FixedCreateIntent is the durable, provider-neutral envelope for a
// caller-supplied lease identity. Provider adapters own the opaque attempt
// fields and their interpretation; the claim layer only persists and locks it.
type FixedCreateIntent struct {
	Version        int               `json:"version"`
	Fingerprint    string            `json:"fingerprint"`
	ProviderScope  string            `json:"providerScope"`
	CheckpointID   string            `json:"checkpointId,omitempty"`
	Slug           string            `json:"slug"`
	CreatedAt      string            `json:"createdAt"`
	State          string            `json:"state"`
	Attempt        map[string]string `json:"attempt,omitempty"`
	FailedAttempts []string          `json:"failedAttempts,omitempty"`
}

const FixedAWSClaimProvider = "aws-fixed-v1"
const FixedMachine0ClaimProvider = "machine0-fixed-v1"
const FixedLocalContainerClaimProvider = "local-container-fixed-v1"
const FixedProxmoxClaimProvider = "proxmox-fixed-v1"

const maxLocalClaimInventoryFileBytes int64 = 1 * 1024 * 1024

var claimMutationMutexes sync.Map

type invalidLeaseClaimIDError struct{ id string }

func (e invalidLeaseClaimIDError) Error() string {
	return "invalid lease claim id " + strconv.Quote(e.id)
}

type leaseClaimsSnapshot struct {
	claims  []leaseClaim
	invalid map[string]error
}

type leaseClaimFileError struct {
	code string
	err  error
}

func (e *leaseClaimFileError) Error() string { return e.err.Error() }
func (e *leaseClaimFileError) Unwrap() error { return e.err }

func snapshotLeaseClaims() (leaseClaimsSnapshot, error) {
	dir, err := crabboxStateDir()
	if err != nil {
		return leaseClaimsSnapshot{}, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "claims"))
	if errors.Is(err, os.ErrNotExist) {
		return leaseClaimsSnapshot{}, nil
	}
	if err != nil {
		return leaseClaimsSnapshot{}, exit(2, "read claims directory: %v", err)
	}
	snapshot := leaseClaimsSnapshot{invalid: make(map[string]error)}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		leaseID := strings.TrimSuffix(entry.Name(), ".json")
		if !validLeaseClaimPathID(leaseID) {
			snapshot.invalid[leaseID] = &leaseClaimFileError{
				code: "invalid_filename",
				err:  exit(2, "claim filename is not a valid lease id"),
			}
			continue
		}
		claim, exists, err := readLeaseClaimRuntimeSnapshotWithPresence(leaseID)
		if err != nil {
			snapshot.invalid[leaseID] = err
			continue
		}
		if exists {
			snapshot.claims = append(snapshot.claims, claim)
		}
	}
	return snapshot, nil
}

type leaseClaimSnapshotReader func(path, leaseID string, expected os.FileInfo) (leaseClaim, bool, error)

// snapshotLeaseClaimsReadOnly is the public inventory reader. It intentionally
// bounds file size and rejects files changed during the scan. Runtime paths use
// snapshotLeaseClaims, which accepts complete atomic publications without that limit.
func snapshotLeaseClaimsReadOnly() (leaseClaimsSnapshot, error) {
	snapshot, err := snapshotLeaseClaimsReadOnlyWithReader(readLeaseClaimSnapshotWithPresence)
	if err != nil {
		return leaseClaimsSnapshot{}, exit(1, "%v", err)
	}
	return snapshot, nil
}

func snapshotLeaseClaimsReadOnlyWithReader(read leaseClaimSnapshotReader) (leaseClaimsSnapshot, error) {
	dir, err := crabboxStateDir()
	if err != nil {
		return leaseClaimsSnapshot{}, err
	}
	claimsDir := filepath.Join(dir, "claims")
	entries, err := os.ReadDir(claimsDir)
	if errors.Is(err, os.ErrNotExist) {
		return leaseClaimsSnapshot{}, nil
	}
	if err != nil {
		return leaseClaimsSnapshot{}, exit(2, "read claims directory: %v", err)
	}

	snapshot := leaseClaimsSnapshot{invalid: make(map[string]error)}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		leaseID := strings.TrimSuffix(entry.Name(), ".json")
		if !validLeaseClaimPathID(leaseID) {
			snapshot.invalid[leaseID] = &leaseClaimFileError{
				code: "invalid_filename",
				err:  exit(2, "claim filename is not a valid lease id"),
			}
			continue
		}
		info, err := entry.Info()
		if err != nil {
			snapshot.invalid[leaseID] = leaseClaimSnapshotReadError(leaseID, "inspect", err)
			continue
		}
		if !info.Mode().IsRegular() {
			snapshot.invalid[leaseID] = &leaseClaimFileError{
				code: "non_regular_file",
				err:  exit(2, "claim file %s is not a regular file", leaseID),
			}
			continue
		}

		claim, exists, err := read(filepath.Join(claimsDir, entry.Name()), leaseID, info)
		if err != nil {
			snapshot.invalid[leaseID] = err
			continue
		}
		if !exists {
			continue
		}
		if claim.LeaseID == "" {
			snapshot.invalid[leaseID] = &leaseClaimFileError{
				code: "empty_lease_id",
				err:  exit(2, "claim file %s has an empty lease id", leaseID),
			}
			continue
		}
		snapshot.claims = append(snapshot.claims, claim)
	}
	return snapshot, nil
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

func claimLeaseForRepoProviderScopePondIfUnchanged(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool, expected leaseClaim, expectedExists bool) (leaseClaim, error) {
	var updated leaseClaim
	err := claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerScope, pond, staticClaimDetails{}, repoRoot, idleTimeout, reclaim, claimMetadata{
		guard:  unchangedLeaseClaimGuard(leaseID, expected, expectedExists),
		result: &updated,
	})
	return updated, err
}

func claimLeaseForRepoProviderScopePondWithLabels(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, labels map[string]string) (leaseClaim, error) {
	var updated leaseClaim
	err := claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerScope, pond, staticClaimDetails{}, repoRoot, idleTimeout, false, claimMetadata{
		setLabels: true,
		labels:    labels,
		guard:     unchangedLeaseClaimGuard(leaseID, leaseClaim{}, false),
		result:    &updated,
		directory: claimDirectoryDurableNamespace,
	})
	return updated, err
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

func claimLeaseForRepoProviderScopePondEndpointReservationIfUnchanged(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool, server Server, target SSHTarget, reservationLabel string, reservationDuration time.Duration, expected leaseClaim, expectedExists bool) (leaseClaim, error) {
	var updated leaseClaim
	err := claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerScope, pond, staticClaimDetails{}, repoRoot, idleTimeout, reclaim, claimMetadata{
		setEndpoint:         true,
		server:              server,
		target:              target,
		reservationLabel:    reservationLabel,
		reservationDuration: reservationDuration,
		guard:               unchangedLeaseClaimGuard(leaseID, expected, expectedExists),
		result:              &updated,
	})
	return updated, err
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
	context             context.Context
	setCacheVolumes     bool
	cacheVolumes        []string
	setEndpoint         bool
	endpointMode        leaseClaimEndpointMode
	server              Server
	target              SSHTarget
	reservationLabel    string
	reservationDuration time.Duration
	guard               func(leaseClaim, bool) error
	result              *leaseClaim
	providerMetadata    leaseClaimMetadataPolicy
	allowEmptyRepoRoot  bool
	directory           claimDirectoryPolicy
	action              func() error
	setLabels           bool
	labels              map[string]string
}

func claimLeaseForRepoProviderScopePondDetails(leaseID, slug, provider, providerScope, pond string, staticDetails staticClaimDetails, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerScope, pond, staticDetails, repoRoot, idleTimeout, reclaim, claimMetadata{})
}

func checkLeaseClaimRepositoryOwner(leaseID string, existing leaseClaim, repoRoot string, reclaim bool) error {
	if existing.LeaseID != "" && existing.RepoRoot != "" && existing.RepoRoot != repoRoot && !reclaim {
		return exit(2, "lease %s is claimed by repo %s; use --reclaim to claim it for %s", leaseID, existing.RepoRoot, repoRoot)
	}
	return nil
}

func transformLeaseClaimForRepo(existing *leaseClaim, leaseID, slug, provider, providerScope, pond string, staticDetails staticClaimDetails, repoRoot string, idleTimeout time.Duration, reclaim bool, metadata claimMetadata, now string) error {
	hadExisting := existing.LeaseID != ""
	original := cloneLeaseClaim(*existing)
	if metadata.setEndpoint && hadExisting {
		server, err := prepareLeaseClaimEndpoint(original, provider, slug, metadata.server, metadata.providerMetadata)
		if err != nil {
			return err
		}
		metadata.server = server
	}
	if err := checkLeaseClaimRepositoryOwner(leaseID, *existing, repoRoot, reclaim); err != nil {
		return err
	}
	if existing.ClaimedAt == "" || reclaim || existing.RepoRoot != repoRoot {
		existing.CheckpointCapture = nil
		existing.ClaimedAt = now
	}
	existing.LeaseID = leaseID
	existing.Slug = slug
	if provider != "" {
		if existing.FixedCreateIntent != nil && existing.Provider != "" {
			if canonicalClaimProvider(existing.Provider) != canonicalClaimProvider(provider) {
				return exit(2, "lease %s fixed claim provider changed from %s to %s", leaseID, existing.Provider, provider)
			}
		} else {
			existing.Provider = provider
		}
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
	if metadata.setLabels {
		existing.Labels = cloneStringMap(metadata.labels)
	}
	if metadata.setEndpoint {
		applyLeaseClaimEndpoint(existing, metadata.server, metadata.target, metadata.endpointMode)
		if metadata.reservationLabel != "" && metadata.reservationDuration > 0 {
			if existing.Labels == nil {
				existing.Labels = make(map[string]string)
			}
			existing.Labels[metadata.reservationLabel] = leaseLabelTime(time.Now().UTC().Add(metadata.reservationDuration))
		}
	}
	return nil
}

func claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerScope, pond string, staticDetails staticClaimDetails, repoRoot string, idleTimeout time.Duration, reclaim bool, metadata claimMetadata) error {
	if leaseID == "" || (repoRoot == "" && !metadata.allowEmptyRepoRoot) {
		return nil
	}
	guard := metadata.guard
	if metadata.setEndpoint {
		guard = endpointClaimGuard(leaseID, metadata.guard)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	directory := metadata.directory
	if directory == claimDirectoryExisting {
		directory = claimDirectoryCreate
	}
	updated, err := transactLeaseClaim(leaseID, leaseClaimTransaction{
		context:     metadata.context,
		guard:       guard,
		action:      claimTransactionAction(metadata.action),
		revision:    claimRevisionBeforeAction,
		directory:   directory,
		publication: claimSkipEmpty,
		mutate: func(existing *leaseClaim) error {
			return transformLeaseClaimForRepo(existing, leaseID, slug, provider, providerScope, pond, staticDetails, repoRoot, idleTimeout, reclaim, metadata, now)
		},
	})
	if metadata.result != nil {
		*metadata.result = updated
	}
	return err
}

func claimLeaseTargetForConfig(leaseID, slug string, cfg Config, server Server, target SSHTarget, idleTimeout time.Duration) error {
	provider, staticDetails := claimProviderDetailsForConfig(cfg)
	return claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerClaimScope(provider, cfg), cfg.Pond, staticDetails, "", idleTimeout, false, claimMetadata{
		setCacheVolumes:    true,
		cacheVolumes:       CacheVolumeStickyDiskSpecs(cfg.Cache.Volumes),
		setEndpoint:        true,
		server:             server,
		target:             target,
		allowEmptyRepoRoot: true,
	})
}

func claimLeaseTargetForConfigIfUnchanged(leaseID, slug string, cfg Config, server Server, target SSHTarget, idleTimeout time.Duration, expected leaseClaim, expectedExists bool) (leaseClaim, error) {
	provider, _ := claimProviderDetailsForConfig(cfg)
	return claimLeaseTargetForConfigScopeIfUnchanged(leaseID, slug, cfg, providerClaimScope(provider, cfg), server, target, idleTimeout, expected, expectedExists)
}

func claimLeaseTargetForConfigScopeIfUnchanged(leaseID, slug string, cfg Config, providerScope string, server Server, target SSHTarget, idleTimeout time.Duration, expected leaseClaim, expectedExists bool) (leaseClaim, error) {
	provider, staticDetails := claimProviderDetailsForConfig(cfg)
	var updated leaseClaim
	err := claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerScope, cfg.Pond, staticDetails, "", idleTimeout, false, claimMetadata{
		setCacheVolumes:    true,
		cacheVolumes:       CacheVolumeStickyDiskSpecs(cfg.Cache.Volumes),
		setEndpoint:        true,
		server:             server,
		target:             target,
		allowEmptyRepoRoot: true,
		guard:              unchangedLeaseClaimGuard(leaseID, expected, expectedExists),
		result:             &updated,
	})
	return updated, err
}

func claimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug string, cfg Config, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool, expected leaseClaim, expectedExists bool) (leaseClaim, error) {
	provider, _ := claimProviderDetailsForConfig(cfg)
	return claimLeaseTargetForRepoConfigScopeIfUnchanged(leaseID, slug, cfg, providerClaimScope(provider, cfg), server, target, repoRoot, idleTimeout, reclaim, expected, expectedExists)
}

func claimLeaseTargetForRepoConfigScopeIfUnchanged(leaseID, slug string, cfg Config, providerScope string, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool, expected leaseClaim, expectedExists bool) (leaseClaim, error) {
	return claimLeaseTargetForRepoConfigScopeIfUnchangedMode(leaseID, slug, cfg, providerScope, server, target, repoRoot, idleTimeout, reclaim, expected, expectedExists, leaseClaimTargetOptions{})
}

func claimLeaseTargetForRepoConfigScopeIfUnchangedDurable(leaseID, slug string, cfg Config, providerScope string, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool, expected leaseClaim, expectedExists bool) (leaseClaim, error) {
	return claimLeaseTargetForRepoConfigScopeIfUnchangedMode(leaseID, slug, cfg, providerScope, server, target, repoRoot, idleTimeout, reclaim, expected, expectedExists, leaseClaimTargetOptions{directory: claimDirectoryDurableNamespace})
}

func claimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfter(leaseID, slug string, cfg Config, providerScope string, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool, expected leaseClaim, expectedExists bool, action func() error) (leaseClaim, error) {
	return claimLeaseTargetForRepoConfigScopeIfUnchangedMode(leaseID, slug, cfg, providerScope, server, target, repoRoot, idleTimeout, reclaim, expected, expectedExists, leaseClaimTargetOptions{directory: claimDirectoryDurableNamespace, action: action})
}

func claimLeaseTargetForRepoConfigScopeReplacingEndpointIfUnchanged(leaseID, slug string, cfg Config, providerScope string, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool, expected leaseClaim, expectedExists bool) (leaseClaim, error) {
	return claimLeaseTargetForRepoConfigScopeIfUnchangedMode(leaseID, slug, cfg, providerScope, server, target, repoRoot, idleTimeout, reclaim, expected, expectedExists, leaseClaimTargetOptions{endpoint: claimEndpointReplace})
}

func claimLeaseTargetForRepoConfigScopeIfUnchangedMode(leaseID, slug string, cfg Config, providerScope string, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool, expected leaseClaim, expectedExists bool, options leaseClaimTargetOptions) (leaseClaim, error) {
	provider, staticDetails := claimProviderDetailsForConfig(cfg)
	var updated leaseClaim
	err := claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerScope, cfg.Pond, staticDetails, repoRoot, idleTimeout, reclaim, claimMetadata{
		context:         options.context,
		setCacheVolumes: true,
		cacheVolumes:    CacheVolumeStickyDiskSpecs(cfg.Cache.Volumes),
		setEndpoint:     true,
		endpointMode:    options.endpoint,
		server:          server,
		target:          target,
		guard:           unchangedLeaseClaimGuard(leaseID, expected, expectedExists),
		result:          &updated,
		directory:       options.directory,
		action:          options.action,
	})
	return updated, err
}

func claimLeaseForRepoConfigIfUnchanged(leaseID, slug string, cfg Config, repoRoot string, idleTimeout time.Duration, reclaim bool, expected leaseClaim, expectedExists bool) (leaseClaim, error) {
	provider, staticDetails := claimProviderDetailsForConfig(cfg)
	var updated leaseClaim
	err := claimLeaseForRepoProviderScopePondDetailsMetadata(leaseID, slug, provider, providerClaimScope(provider, cfg), cfg.Pond, staticDetails, repoRoot, idleTimeout, reclaim, claimMetadata{
		setCacheVolumes: true,
		cacheVolumes:    CacheVolumeStickyDiskSpecs(cfg.Cache.Volumes),
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
		return transformLeaseClaimEndpoint(claim, server, target, leaseClaimEndpointPolicy{})
	})
}

func prepareLeaseClaimEndpoint(existing leaseClaim, providerName, slug string, server Server, providerMetadata leaseClaimMetadataPolicy) (Server, error) {
	provider, err := ProviderFor(canonicalClaimProvider(firstNonBlank(existing.Provider, providerName)))
	if err != nil {
		return Server{}, exit(2, "lease %s claim has unavailable provider %q", existing.LeaseID, existing.Provider)
	}
	preparer, ok := provider.(LeaseClaimEndpointPreparer)
	if !ok {
		return server, nil
	}
	return preparer.PrepareLeaseClaimEndpoint(existing, providerName, slug, server, providerMetadata == claimMetadataAdmitProvider)
}

func updateLeaseClaimEndpointIfUnchanged(leaseID string, expected leaseClaim, server Server, target SSHTarget) (leaseClaim, error) {
	return updateLeaseClaimEndpointIfUnchangedMode(leaseID, expected, server, target, leaseClaimEndpointPolicy{})
}

func updateLeaseClaimEndpointIfUnchangedWithProviderMetadata(leaseID string, expected leaseClaim, server Server, target SSHTarget) (leaseClaim, error) {
	return updateLeaseClaimEndpointIfUnchangedMode(leaseID, expected, server, target, leaseClaimEndpointPolicy{metadata: claimMetadataAdmitProvider})
}

func replaceLeaseClaimEndpointIfUnchangedWithProviderMetadata(leaseID string, expected leaseClaim, server Server, target SSHTarget) (leaseClaim, error) {
	return updateLeaseClaimEndpointIfUnchangedMode(leaseID, expected, server, target, leaseClaimEndpointPolicy{mode: claimEndpointReplace, metadata: claimMetadataAdmitProvider})
}

func updateLeaseClaimEndpointIfUnchangedAfter(leaseID string, expected leaseClaim, server Server, target SSHTarget, action func() error) (leaseClaim, error) {
	if leaseID == "" {
		return leaseClaim{}, nil
	}
	return transactLeaseClaim(leaseID, leaseClaimTransaction{
		guard:    endpointClaimGuard(leaseID, unchangedLeaseClaimGuard(leaseID, expected, true)),
		action:   claimTransactionAction(action),
		revision: claimRevisionBeforeMutation,
		mutate: func(claim *leaseClaim) error {
			return transformLeaseClaimEndpoint(claim, server, target, leaseClaimEndpointPolicy{})
		},
	})
}

func withLeaseClaimUnchanged(leaseID string, expected leaseClaim, action func() error) error {
	return withLeaseClaimUnchangedContext(context.Background(), leaseID, expected, false, action)
}

func withLeaseClaimUnchangedContext(ctx context.Context, leaseID string, expected leaseClaim, shared bool, action func() error) error {
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		return err
	}
	return withLeaseClaimLockContext(ctx, path, shared, func() error {
		claim, exists, err := readLeaseClaimPathWithPresence(path)
		if err != nil {
			return err
		}
		if err := unchangedLeaseClaimGuard(leaseID, expected, true)(claim, exists); err != nil {
			return err
		}
		if action == nil {
			return nil
		}
		return action()
	})
}

func updateLeaseClaimEndpointIfUnchangedAction(
	leaseID string,
	expected leaseClaim,
	action func() (Server, SSHTarget, bool, error),
) (leaseClaim, Server, SSHTarget, error) {
	return updateLeaseClaimEndpointIfUnchangedActionMode(context.Background(), leaseID, expected, action, claimEndpointUpdate, nil)
}

func replaceLeaseClaimEndpointIfUnchangedAction(
	leaseID string,
	expected leaseClaim,
	action func() (Server, SSHTarget, bool, error),
) (leaseClaim, Server, SSHTarget, error) {
	return updateLeaseClaimEndpointIfUnchangedActionMode(context.Background(), leaseID, expected, action, claimEndpointReplace, nil)
}

type leaseClaimTouchPayload struct {
	lastUsed            time.Time
	idleTimeoutOverride *time.Duration
}

func updateLeaseClaimEndpointIfUnchangedActionMode(
	ctx context.Context,
	leaseID string,
	expected leaseClaim,
	action func() (Server, SSHTarget, bool, error),
	endpointMode leaseClaimEndpointMode,
	touch *leaseClaimTouchPayload,
) (leaseClaim, Server, SSHTarget, error) {
	if touch != nil && touch.idleTimeoutOverride != nil {
		if *touch.idleTimeoutOverride <= 0 {
			return leaseClaim{}, Server{}, SSHTarget{}, exit(2, "lease %s idle timeout override must be positive", leaseID)
		}
		if touch.idleTimeoutOverride.Round(time.Second)/time.Second <= 0 {
			return leaseClaim{}, Server{}, SSHTarget{}, exit(2, "lease %s idle timeout override must be at least one second", leaseID)
		}
	}
	var server Server
	var target SSHTarget
	updated, err := transactLeaseClaim(leaseID, leaseClaimTransaction{
		context:  ctx,
		guard:    endpointClaimGuard(leaseID, unchangedLeaseClaimGuard(leaseID, expected, true)),
		revision: claimRevisionAfterMutation,
		action: func() (claimActionDecision, error) {
			if action == nil {
				return claimActionKeepSnapshot, nil
			}
			var shouldUpdate bool
			var err error
			server, target, shouldUpdate, err = action()
			if err != nil || !shouldUpdate {
				return claimActionKeepSnapshot, err
			}
			return claimActionContinue, nil
		},
		mutate: func(claim *leaseClaim) error {
			if err := transformLeaseClaimEndpoint(claim, server, target, leaseClaimEndpointPolicy{mode: endpointMode}); err != nil {
				return err
			}
			if touch != nil {
				claim.LastUsedAt = touch.lastUsed.UTC().Format(time.RFC3339)
				if touch.idleTimeoutOverride != nil {
					claim.IdleTimeoutSeconds = int(touch.idleTimeoutOverride.Round(time.Second) / time.Second)
				}
			}
			return nil
		},
	})
	return updated, server, target, err
}

func updateLeaseClaimEndpointIfUnchangedMode(leaseID string, expected leaseClaim, server Server, target SSHTarget, policy leaseClaimEndpointPolicy) (leaseClaim, error) {
	if leaseID == "" {
		return leaseClaim{}, nil
	}
	return transactLeaseClaim(leaseID, leaseClaimTransaction{
		guard:       endpointClaimGuard(leaseID, unchangedLeaseClaimGuard(leaseID, expected, true)),
		revision:    claimRevisionBeforeMutation,
		directory:   claimDirectoryCreate,
		publication: claimSkipEmpty,
		mutate: func(claim *leaseClaim) error {
			return transformLeaseClaimEndpoint(claim, server, target, policy)
		},
	})
}

func transformLeaseClaimEndpoint(claim *leaseClaim, server Server, target SSHTarget, policy leaseClaimEndpointPolicy) error {
	provider := firstNonBlank(server.Labels["provider"], server.Provider)
	prepared, err := prepareLeaseClaimEndpoint(*claim, provider, server.Labels["slug"], server, policy.metadata)
	if err != nil {
		return err
	}
	applyLeaseClaimEndpoint(claim, prepared, target, policy.mode)
	return nil
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

func updateLeaseClaimLabelsAndLastUsedIfUnchanged(leaseID string, expected leaseClaim, labels map[string]string, lastUsed time.Time) (leaseClaim, error) {
	if leaseID == "" {
		return leaseClaim{}, nil
	}
	var updated leaseClaim
	err := mutateLeaseClaimGuarded(leaseID, unchangedLeaseClaimGuard(leaseID, expected, true), func(claim *leaseClaim) error {
		if claim.LeaseID == "" {
			return nil
		}
		claim.Labels = cloneStringMap(labels)
		claim.LastUsedAt = lastUsed.UTC().Format(time.RFC3339)
		updated = cloneLeaseClaim(*claim)
		return nil
	})
	return updated, err
}

func updateLeaseClaimTouchIfUnchanged(ctx context.Context, leaseID string, expected leaseClaim, labels map[string]string, lastUsed time.Time, idleTimeoutOverride *time.Duration) (leaseClaim, error) {
	if leaseID == "" {
		return leaseClaim{}, nil
	}
	if idleTimeoutOverride != nil && *idleTimeoutOverride <= 0 {
		return leaseClaim{}, exit(2, "lease %s idle timeout override must be positive", leaseID)
	}
	return transactLeaseClaim(leaseID, leaseClaimTransaction{
		context:     ctx,
		guard:       unchangedLeaseClaimGuard(leaseID, expected, true),
		revision:    claimRevisionBeforeAction,
		publication: claimSkipEmpty,
		directory:   claimDirectoryCreate,
		mutate: func(claim *leaseClaim) error {
			if claim.LeaseID == "" {
				return nil
			}
			// A journal can reserve this source without changing its revision.
			if err := AuthorizeCheckpointRelease(*claim, ""); err != nil {
				return err
			}
			claim.Labels = cloneStringMap(labels)
			claim.LastUsedAt = lastUsed.UTC().Format(time.RFC3339)
			if idleTimeoutOverride != nil {
				claim.IdleTimeoutSeconds = int(idleTimeoutOverride.Round(time.Second) / time.Second)
				if claim.IdleTimeoutSeconds <= 0 {
					return exit(2, "lease %s idle timeout override must be at least one second", leaseID)
				}
			}
			return nil
		},
	})
}

func updateLeaseClaimLabelsIfUnchangedAfter(leaseID string, expected leaseClaim, labels map[string]string, action func() error) (leaseClaim, error) {
	if leaseID == "" {
		return leaseClaim{}, nil
	}
	return transactLeaseClaim(leaseID, leaseClaimTransaction{
		guard:    unchangedLeaseClaimGuard(leaseID, expected, true),
		action:   claimTransactionAction(action),
		revision: claimRevisionBeforeMutation,
		mutate: func(claim *leaseClaim) error {
			claim.Labels = cloneStringMap(labels)
			return nil
		},
	})
}

func cloneLeaseClaim(claim leaseClaim) leaseClaim {
	if claim.CheckpointCapture != nil {
		binding := *claim.CheckpointCapture
		claim.CheckpointCapture = &binding
	}
	claim.Labels = cloneStringMap(claim.Labels)
	claim.TailscaleTags = append([]string(nil), claim.TailscaleTags...)
	claim.CacheVolumes = append([]string(nil), claim.CacheVolumes...)
	if claim.FixedCreateIntent != nil {
		intent := *claim.FixedCreateIntent
		intent.Attempt = cloneStringMap(intent.Attempt)
		intent.FailedAttempts = append([]string(nil), intent.FailedAttempts...)
		claim.FixedCreateIntent = &intent
	}
	return claim
}

func refreshLeaseClaimRevision(claim *leaseClaim) error {
	var revision [16]byte
	if _, err := rand.Read(revision[:]); err != nil {
		return exit(2, "generate lease claim revision: %v", err)
	}
	claim.Revision = hex.EncodeToString(revision[:])
	return nil
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

func applyLeaseClaimEndpoint(claim *leaseClaim, server Server, target SSHTarget, mode leaseClaimEndpointMode) {
	if mode == claimEndpointReplace {
		clearLeaseClaimTailscaleFields(claim)
		claim.BridgeURL = ""
	}
	if server.CloudID != "" {
		claim.CloudID = server.CloudID
	}
	if server.ID != 0 {
		claim.CloudNumericID = server.ID
	}
	if server.ImmutableID != "" {
		claim.CloudImmutableID = server.ImmutableID
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
	} else if mode == claimEndpointReplace || claimEndpointInactiveState(server.Labels["state"]) {
		claim.SSHHost = ""
	}
	if port, err := strconv.Atoi(strings.TrimSpace(target.Port)); err == nil && port > 0 {
		claim.SSHPort = port
	} else if mode == claimEndpointReplace || claimEndpointInactiveState(server.Labels["state"]) {
		claim.SSHPort = 0
	}
}

func claimEndpointInactiveState(state string) bool {
	state = strings.TrimSpace(state)
	return statusTerminalState(state) || strings.EqualFold(state, "stopped") || strings.EqualFold(state, "paused") || strings.EqualFold(state, "deleting")
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
	return mutateLeaseClaimGuardedWithWrite(leaseID, guard, mutate, writeLeaseClaimAtomic)
}

func mutateLeaseClaimGuardedDurableWithSync(leaseID string, guard func(leaseClaim, bool) error, mutate func(*leaseClaim) error, syncDirectory func(string) error) error {
	_, err := transactLeaseClaim(leaseID, leaseClaimTransaction{
		guard:         guard,
		mutate:        mutate,
		revision:      claimRevisionBeforeAction,
		publication:   claimSkipEmpty,
		directory:     claimDirectoryDurableNamespace,
		syncDirectory: syncDirectory,
	})
	return err
}

func mutateLeaseClaimGuardedWithWrite(leaseID string, guard func(leaseClaim, bool) error, mutate func(*leaseClaim) error, write func(string, leaseClaim) error) error {
	_, err := transactLeaseClaim(leaseID, leaseClaimTransaction{
		guard:       guard,
		mutate:      mutate,
		write:       write,
		revision:    claimRevisionBeforeAction,
		publication: claimSkipEmpty,
		directory:   claimDirectoryCreate,
	})
	return err
}

func claimMutationMutex(path string) *sync.Mutex {
	value, _ := claimMutationMutexes.LoadOrStore(path, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func withLeaseClaimLock(path string, fn func() error) error {
	return withLeaseClaimLockContext(context.Background(), path, false, fn)
}

func withLeaseIDOperationLock(leaseID string, action func() error) error {
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return exit(2, "create claim directory: %v", err)
	}
	return withLeaseClaimLock(path, action)
}

func withDurableLeaseClaimLock(leaseID string, action func(*leaseClaim, bool, func() error) error) error {
	return withDurableLeaseClaimLockContext(context.Background(), leaseID, action)
}

func withDurableLeaseClaimLockContext(ctx context.Context, leaseID string, action func(*leaseClaim, bool, func() error) error) error {
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	firstExistingDir, err := nearestExistingClaimDirectory(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return exit(2, "create claim directory: %v", err)
	}
	return withLeaseClaimLockContext(ctx, path, false, func() error {
		claim, exists, err := readLeaseClaimPathWithPresence(path)
		if err != nil {
			return err
		}
		if err := validateLeaseClaimFileIdentity(leaseID, claim, exists); err != nil {
			return err
		}
		persist := func() error {
			if claim.LeaseID == "" {
				claim.LeaseID = leaseID
			}
			if claim.LeaseID != leaseID {
				return invalidLeaseClaimIDError{id: claim.LeaseID}
			}
			if err := refreshLeaseClaimRevision(&claim); err != nil {
				return err
			}
			if claim.CheckpointCapture != nil && claim.CheckpointCapture.BoundRevision == "" {
				// Publish the binding and exact claim revision together. Later claim
				// mutations must not silently reauthorize this capture.
				claim.CheckpointCapture.BoundRevision = claim.Revision
			}
			if err := writeLeaseClaimAtomicDurableWithSync(path, claim, firstExistingDir, syncControllerDirectory); err != nil {
				return err
			}
			exists = true
			return nil
		}
		return action(&claim, exists, persist)
	})
}

func leaseClaimLockPath(path string) (string, error) {
	dir := filepath.Join(filepath.Dir(filepath.Dir(path)), "claim-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", exit(2, "create claim lock directory: %v", err)
	}
	return filepath.Join(dir, filepath.Base(path)+".lock"), nil
}

func writeLeaseClaimAtomic(path string, claim leaseClaim) error {
	return writeLeaseClaimAtomicWithSync(path, claim, func(dir string) error {
		fsyncDir(dir)
		return nil
	})
}

func writeLeaseClaimAtomicWithSync(path string, claim leaseClaim, syncDirectory func(string) error) error {
	data, err := json.MarshalIndent(claim, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	if err := writeStateFileAtomic(path, data, syncDirectory); err != nil {
		return exit(2, "write claim %s: %v", path, err)
	}
	return nil
}

func writeLeaseClaimAtomicDurable(path string, claim leaseClaim) error {
	return writeLeaseClaimAtomicDurableWithSync(path, claim, filepath.Dir(path), syncControllerDirectory)
}

func writeLeaseClaimAtomicDurableWithSync(path string, claim leaseClaim, firstExistingDir string, syncDirectory func(string) error) error {
	if err := writeLeaseClaimAtomicWithSync(path, claim, syncDirectory); err != nil {
		return err
	}
	return syncCreatedClaimDirectoryParentsWithSync(filepath.Dir(path), firstExistingDir, syncDirectory)
}

func nearestExistingClaimDirectory(dir string) (string, error) {
	dir = filepath.Clean(dir)
	for current := dir; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return "", exit(2, "create claim directory: %s is not a directory", current)
			}
			return current, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", exit(2, "inspect claim directory %s: %v", current, err)
		}
		if parent := filepath.Dir(current); parent == current {
			return "", exit(2, "create claim directory: no existing ancestor for %s", dir)
		}
	}
}

func syncCreatedClaimDirectoryParentsWithSync(claimDir, firstExistingDir string, syncDirectory func(string) error) error {
	claimDir = filepath.Clean(claimDir)
	firstExistingDir = filepath.Clean(firstExistingDir)
	if claimDir == firstExistingDir {
		return nil
	}
	for current := filepath.Dir(claimDir); ; current = filepath.Dir(current) {
		if err := syncDirectory(current); err != nil {
			return exit(2, "sync claim namespace parent %s: %v", current, err)
		}
		if current == firstExistingDir {
			return nil
		}
		if parent := filepath.Dir(current); parent == current {
			return exit(2, "sync claim namespace: boundary %s is not an ancestor of %s", firstExistingDir, claimDir)
		}
	}
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
	switch strings.TrimSpace(provider) {
	case FixedAWSClaimProvider:
		return "aws"
	case FixedMachine0ClaimProvider:
		return "machine0"
	case FixedLocalContainerClaimProvider:
		return "local-container"
	case FixedProxmoxClaimProvider:
		return "proxmox"
	case "exec-provider":
		return "external"
	}
	if resolved, err := ProviderFor(provider); err == nil {
		return resolved.Name()
	}
	return normalizeProviderName(provider)
}

func claimLookupSlug(identifier string) string {
	// A missing canonical ID must not select a different lease's lookalike slug.
	if isCanonicalLeaseID(identifier) {
		return ""
	}
	return normalizeLeaseSlug(identifier)
}

func claimProviderForIdentifier(identifier string) (string, bool, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", false, nil
	}
	exact, exists, err := readLeaseClaimWithPresence(identifier)
	if err != nil {
		return "", false, err
	}
	if exists {
		provider := canonicalClaimProvider(exact.Provider)
		return provider, provider != "", nil
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return "", false, err
	}
	slug := claimLookupSlug(identifier)
	provider := ""
	for _, claim := range claims {
		if claim.LeaseID != identifier && (slug == "" || normalizeLeaseSlug(claim.Slug) != slug) {
			continue
		}
		candidate := canonicalClaimProvider(claim.Provider)
		if candidate == "" {
			continue
		}
		if provider != "" && provider != candidate {
			return "", false, exit(2, "lease identifier %s matches local claims from multiple providers; use a canonical lease id or pass --provider", identifier)
		}
		provider = candidate
	}
	return provider, provider != "", nil
}

func providerClaimScope(provider string, cfg Config) string {
	if resolved, err := ProviderFor(provider); err == nil {
		if scoper, ok := resolved.(ProviderClaimScoper); ok {
			return scoper.ClaimScope(cfg)
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
	slug := claimLookupSlug(identifier)
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
	if canonicalClaimProvider(claim.Provider) == provider {
		return claim, true, nil
	}
	claim, ok, err = findLeaseClaim(identifier, func(candidate leaseClaim) bool {
		return canonicalClaimProvider(candidate.Provider) == provider
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
		if exact.LeaseID == "" || canonicalClaimProvider(exact.Provider) != provider {
			return exact, false, true, nil
		}
		return exact, true, true, nil
	}
	claim, ok, err := resolveLeaseClaimForProvider(identifier, provider)
	return claim, ok, false, err
}

func resolveLeaseClaimForProviderScopeWithExact(identifier, provider, providerScope string) (leaseClaim, bool, bool, error) {
	if identifier == "" {
		return leaseClaim{}, false, false, nil
	}
	exact, exists, err := readLeaseClaimWithPresence(identifier)
	if err != nil {
		return leaseClaim{}, false, exists, err
	}
	if exists {
		if exact.LeaseID == "" || canonicalClaimProvider(exact.Provider) != provider || exact.ProviderScope != providerScope {
			return exact, false, true, nil
		}
		return exact, true, true, nil
	}
	claim, ok, err := findUniqueLeaseClaim(identifier, func(candidate leaseClaim) bool {
		return canonicalClaimProvider(candidate.Provider) == provider && candidate.ProviderScope == providerScope
	})
	return claim, ok, false, err
}

func resolveLeaseClaimForProviderCloudID(cloudID, provider string) (leaseClaim, bool, error) {
	return resolveLeaseClaimForProviderCloudIDScope(cloudID, provider, "")
}

func resolveLeaseClaimForProviderCloudIDScope(cloudID, provider, providerScope string) (leaseClaim, bool, error) {
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
		if canonicalClaimProvider(claim.Provider) != provider || claim.CloudID != cloudID {
			continue
		}
		if providerScope != "" && claim.ProviderScope != providerScope {
			continue
		}
		if match.LeaseID != "" {
			return leaseClaim{}, false, exit(2, "multiple provider=%s scope=%s claims match cloud id %s", provider, providerScope, cloudID)
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
	slug := claimLookupSlug(identifier)
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
	slug := claimLookupSlug(identifier)
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

func findUniqueLeaseClaim(identifier string, match func(leaseClaim) bool) (leaseClaim, bool, error) {
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
	slug := claimLookupSlug(identifier)
	var found leaseClaim
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		claim, err := readLeaseClaim(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return leaseClaim{}, false, err
		}
		if (claim.LeaseID != identifier && (slug == "" || normalizeLeaseSlug(claim.Slug) != slug)) || !match(claim) {
			continue
		}
		if found.LeaseID != "" {
			return leaseClaim{}, false, exit(2, "multiple claims match identifier %s", identifier)
		}
		found = claim
	}
	return found, found.LeaseID != "", nil
}

func removeLeaseClaim(leaseID string) {
	path, err := leaseClaimPath(leaseID)
	if err == nil {
		_ = withLeaseClaimLock(path, func() error {
			err := removeControllerFile(path)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		})
	}
}

func removeLeaseClaimIfUnchanged(leaseID string, expected leaseClaim) error {
	return removeLeaseClaimIfUnchangedAfter(leaseID, expected, nil)
}

func verifyLeaseClaimUnchanged(leaseID string, expected leaseClaim) error {
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		return err
	}
	return withLeaseClaimLock(path, func() error {
		claim, exists, err := readLeaseClaimPathWithPresence(path)
		if err != nil {
			return err
		}
		if err := validateLeaseClaimFileIdentity(leaseID, claim, exists); err != nil {
			return err
		}
		if err := unchangedLeaseClaimGuard(leaseID, expected, true)(claim, exists); err != nil {
			return err
		}
		return nil
	})
}

func removeLeaseClaimIfUnchangedAfter(leaseID string, expected leaseClaim, action func() error) error {
	return removeLeaseClaimIfUnchangedAfterWithSync(leaseID, expected, action, syncControllerDirectory)
}

func resolveLeaseClaimAfterActionIfUnchanged(
	leaseID string,
	expected leaseClaim,
	action func() error,
	resolve func(error) (map[string]string, bool),
) (leaseClaim, bool, bool, error) {
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		return leaseClaim{}, false, false, err
	}
	var updated leaseClaim
	removed := false
	actionSucceeded := false
	err = withLeaseClaimLock(path, func() error {
		claim, exists, err := readLeaseClaimPathWithPresence(path)
		if err != nil {
			return err
		}
		if err := unchangedLeaseClaimGuard(leaseID, expected, true)(claim, exists); err != nil {
			return err
		}
		actionErr := action()
		actionSucceeded = actionErr == nil
		labels, shouldRemove := resolve(actionErr)
		if labels == nil {
			return actionErr
		}
		claim.Labels = cloneStringMap(labels)
		claim.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
		var publicationErr error
		if err := refreshLeaseClaimRevision(&claim); err != nil {
			publicationErr = err
		} else if err := writeLeaseClaimAtomicDurable(path, claim); err != nil {
			publicationErr = err
		}
		updated = cloneLeaseClaim(claim)
		if !shouldRemove {
			return errors.Join(actionErr, publicationErr)
		}
		// A definitive create conflict must never retain destructive ownership of
		// the pre-existing resource. Attempt removal even when publishing the
		// non-destructive conflict state failed; the removal is the safety gate.
		if err := removeControllerFile(path); err != nil {
			return errors.Join(actionErr, publicationErr, exit(2, "remove claim %s: %v", path, err))
		}
		if err := syncControllerDirectory(filepath.Dir(path)); err != nil {
			// The unlink is not a durable safety result until the directory sync
			// succeeds. Recreate the non-destructive conflict tombstone so a crash
			// cannot resurrect the original creating claim as destructive ownership.
			tombstoneErr := writeLeaseClaimAtomicDurable(path, claim)
			if tombstoneErr == nil {
				updated = cloneLeaseClaim(claim)
			}
			return errors.Join(actionErr, publicationErr, exit(2, "sync removed claim directory %s: %v", filepath.Dir(path), err), tombstoneErr)
		}
		removed = true
		return errors.Join(actionErr, publicationErr)
	})
	return updated, removed, actionSucceeded, err
}

func removeLeaseClaimIfUnchangedAfterWithSync(leaseID string, expected leaseClaim, action func() error, syncDirectory func(string) error) error {
	return cleanupLeaseClaimIfUnchangedAfterWithSync(leaseID, expected, true, action, syncDirectory)
}

func cleanupLeaseClaimIfUnchangedAfter(leaseID string, expected leaseClaim, expectedExists bool, action func() error) error {
	return cleanupLeaseClaimIfUnchangedAfterWithSync(leaseID, expected, expectedExists, action, syncControllerDirectory)
}

func cleanupLeaseClaimIfUnchangedAfterWithSync(leaseID string, expected leaseClaim, expectedExists bool, action func() error, syncDirectory func(string) error) error {
	return cleanupLeaseClaimIfUnchangedAfterContext(context.Background(), leaseID, expected, expectedExists, action, syncDirectory)
}

func cleanupLeaseClaimIfUnchangedAfterContext(ctx context.Context, leaseID string, expected leaseClaim, expectedExists bool, action func() error, syncDirectory func(string) error) error {
	return finalizeLeaseClaimIfUnchangedAfterContext(ctx, leaseID, expected, expectedExists, func() (bool, error) {
		if action != nil {
			if err := action(); err != nil {
				return false, err
			}
		}
		return true, nil
	}, syncDirectory)
}

// Finalization keeps the original claim when remote cleanup is retained or
// pending. The same fence covers admission, provider effects and local removal.
func finalizeLeaseClaimIfUnchangedAfterContext(ctx context.Context, leaseID string, expected leaseClaim, expectedExists bool, action func() (bool, error), syncDirectory func(string) error) error {
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		return err
	}
	return withLeaseClaimLockContext(ctx, path, false, func() error {
		claim, exists, err := readLeaseClaimPathWithPresence(path)
		if err != nil {
			return err
		}
		if err := unchangedLeaseClaimGuard(leaseID, expected, expectedExists)(claim, exists); err != nil {
			return err
		}
		remove, err := action()
		if err != nil || !remove {
			return err
		}
		// Even when the source is absent, Windows may still have the
		// deterministic tombstone left by an interrupted write-through remove.
		if err := removeControllerFile(path); err != nil && (exists || !errors.Is(err, os.ErrNotExist)) {
			return exit(2, "remove claim %s: %v", path, err)
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			if !exists && errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return exit(2, "sync removed claim directory %s: %v", filepath.Dir(path), err)
		}
		return nil
	})
}

func restoreLeaseClaimIfUnchanged(leaseID string, current, previous leaseClaim, previousExists bool) error {
	if !previousExists {
		return removeLeaseClaimIfUnchanged(leaseID, current)
	}
	_, err := replaceLeaseClaimIfUnchangedWithWrite(leaseID, current, previous, writeLeaseClaimAtomic)
	return err
}

func replaceLeaseClaimIfUnchanged(leaseID string, current, replacement leaseClaim) error {
	_, err := replaceLeaseClaimIfUnchangedWithWrite(leaseID, current, replacement, writeLeaseClaimAtomic)
	return err
}

func replaceLeaseClaimIfUnchangedDurableReturning(leaseID string, current, replacement leaseClaim) (leaseClaim, error) {
	return replaceLeaseClaimIfUnchangedWithWrite(leaseID, current, replacement, writeLeaseClaimAtomicDurable)
}

func replaceLeaseClaimIfUnchangedDurableAfter(leaseID string, current, replacement leaseClaim, action func() error) (leaseClaim, error) {
	// This entrypoint binds replacement identity; restore/replace retain the
	// supplied payload, including incomplete claims used by rollback.
	replacement.LeaseID = leaseID
	return replaceLeaseClaimTransaction(leaseID, current, replacement, action, writeLeaseClaimAtomicDurable)
}

func replaceLeaseClaimIfUnchangedWithWrite(leaseID string, current, replacement leaseClaim, write func(string, leaseClaim) error) (leaseClaim, error) {
	return replaceLeaseClaimTransaction(leaseID, current, replacement, nil, write)
}

func replaceLeaseClaimTransaction(leaseID string, current, replacement leaseClaim, action func() error, write func(string, leaseClaim) error) (leaseClaim, error) {
	return transactLeaseClaim(leaseID, leaseClaimTransaction{
		guard:    unchangedLeaseClaimGuard(leaseID, current, true),
		action:   claimTransactionAction(action),
		revision: claimRevisionAfterMutation,
		write:    write,
		mutate: func(claim *leaseClaim) error {
			*claim = cloneLeaseClaim(replacement)
			return nil
		},
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

// Published claim files are immutable snapshots. Reads never take the operation
// lock; guarded actions must reread and validate their snapshot under that lock.
func readLeaseClaimWithPresence(leaseID string) (leaseClaim, bool, error) {
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		var invalid invalidLeaseClaimIDError
		if errors.As(err, &invalid) {
			return leaseClaim{}, false, nil
		}
		return leaseClaim{}, false, err
	}
	claim, exists, err := readLeaseClaimPathWithPresence(path)
	if err != nil {
		return leaseClaim{}, exists, err
	}
	if err := validateLeaseClaimFileIdentity(leaseID, claim, exists); err != nil {
		return leaseClaim{}, exists, err
	}
	return claim, exists, nil
}

func readLeaseClaimRuntimeSnapshotWithPresence(leaseID string) (leaseClaim, bool, error) {
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		var invalid invalidLeaseClaimIDError
		if errors.As(err, &invalid) {
			return leaseClaim{}, false, nil
		}
		return leaseClaim{}, false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return leaseClaim{}, false, nil
	}
	if err != nil {
		return leaseClaim{}, true, leaseClaimSnapshotReadError(leaseID, "inspect", err)
	}
	if !info.Mode().IsRegular() {
		return leaseClaim{}, true, &leaseClaimFileError{
			code: "non_regular_file",
			err:  exit(2, "claim file %s is not a regular file", leaseID),
		}
	}
	claim, exists, err := readLeaseClaimWithPresence(leaseID)
	if err != nil {
		return leaseClaim{}, exists, err
	}
	if exists && claim.LeaseID == "" {
		return leaseClaim{}, true, &leaseClaimFileError{
			code: "empty_lease_id",
			err:  exit(2, "claim file %s has an empty lease id", leaseID),
		}
	}
	return claim, exists, nil
}

func readLeaseClaimSnapshotWithPresence(path, leaseID string, expected os.FileInfo) (leaseClaim, bool, error) {
	file, err := openArtifactReadOnly(path)
	if err != nil {
		return leaseClaim{}, true, leaseClaimSnapshotReadError(leaseID, "open", err)
	}
	defer file.Close()

	opened, err := file.Stat()
	if err != nil {
		return leaseClaim{}, true, leaseClaimSnapshotReadError(leaseID, "inspect opened", err)
	}
	if !opened.Mode().IsRegular() {
		return leaseClaim{}, true, leaseClaimSnapshotReadError(leaseID, "verify regular", errors.New("claim path changed during scan"))
	}
	if !sameLeaseClaimSnapshotFile(expected, opened) {
		return leaseClaim{}, true, leaseClaimSnapshotReadError(leaseID, "verify opened", errors.New("claim file changed during scan"))
	}
	if opened.Size() > maxLocalClaimInventoryFileBytes {
		return leaseClaim{}, true, leaseClaimSnapshotTooLargeError(leaseID)
	}

	data, tooLarge, err := readLocalClaimInventoryData(file)
	if err != nil {
		return leaseClaim{}, true, leaseClaimSnapshotReadError(leaseID, "read", err)
	}
	if tooLarge {
		return leaseClaim{}, true, leaseClaimSnapshotTooLargeError(leaseID)
	}
	afterRead, err := file.Stat()
	if err != nil {
		return leaseClaim{}, true, leaseClaimSnapshotReadError(leaseID, "inspect read", err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return leaseClaim{}, true, leaseClaimSnapshotReadError(leaseID, "verify current", err)
	}
	if !sameLeaseClaimSnapshotFile(opened, afterRead) || !sameLeaseClaimSnapshotFile(afterRead, current) {
		return leaseClaim{}, true, leaseClaimSnapshotReadError(leaseID, "verify stable", errors.New("claim file changed during scan"))
	}

	claim, err := decodeLeaseClaim(path, data)
	if err != nil {
		return leaseClaim{}, true, err
	}
	if err := validateLeaseClaimFileIdentity(leaseID, claim, true); err != nil {
		return leaseClaim{}, true, err
	}
	return claim, true, nil
}

func readLocalClaimInventoryData(r io.Reader) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxLocalClaimInventoryFileBytes+1))
	return data, int64(len(data)) > maxLocalClaimInventoryFileBytes, err
}

func sameLeaseClaimSnapshotFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func leaseClaimSnapshotReadError(leaseID, action string, err error) error {
	return &leaseClaimFileError{
		code: "read_error",
		err:  exit(2, "%s claim file %s: %v", action, leaseID, err),
	}
}

func leaseClaimSnapshotTooLargeError(leaseID string) error {
	return &leaseClaimFileError{
		code: "claim_too_large",
		err:  exit(2, "claim file %s exceeds the 1 MiB inventory limit", leaseID),
	}
}

func validateLeaseClaimFileIdentity(leaseID string, claim leaseClaim, exists bool) error {
	if exists && claim.LeaseID != "" && claim.LeaseID != leaseID {
		return &leaseClaimFileError{
			code: "lease_id_mismatch",
			err:  exit(2, "claim file %s contains lease id %s; refusing misfiled claim", leaseID, claim.LeaseID),
		}
	}
	return nil
}

func readLeaseClaimPathWithPresence(path string) (leaseClaim, bool, error) {
	// Share deletion on Windows; open nonblocking on Unix so a FIFO cannot hang.
	file, err := openArtifactReadOnly(path)
	if errors.Is(err, os.ErrNotExist) {
		return leaseClaim{}, false, nil
	}
	var data []byte
	if err == nil {
		defer file.Close()
		var info os.FileInfo
		info, err = file.Stat()
		if err == nil && !info.Mode().IsRegular() {
			err = errors.New("claim path is not a regular file")
		}
		if err == nil {
			data, err = io.ReadAll(file)
		}
	}
	if err != nil {
		return leaseClaim{}, true, &leaseClaimFileError{
			code: "read_error",
			err:  exit(2, "read claim %s: %v", path, err),
		}
	}
	claim, err := decodeLeaseClaim(path, data)
	return claim, true, err
}

func decodeLeaseClaim(path string, data []byte) (leaseClaim, error) {
	if !utf8.Valid(data) {
		return leaseClaim{}, &leaseClaimFileError{
			code: "invalid_json",
			err:  exit(2, "parse claim %s: invalid UTF-8", path),
		}
	}
	var claim leaseClaim
	if err := json.Unmarshal(data, &claim); err != nil {
		return leaseClaim{}, &leaseClaimFileError{
			code: "invalid_json",
			err:  exit(2, "parse claim %s: %v", path, err),
		}
	}
	return claim, nil
}

func leaseClaimPath(leaseID string) (string, error) {
	if !validLeaseClaimPathID(leaseID) {
		return "", invalidLeaseClaimIDError{id: leaseID}
	}
	dir, err := crabboxStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "claims", leaseID+".json"), nil
}

func validLeaseClaimPathID(leaseID string) bool {
	return leaseID == strings.TrimSpace(leaseID) && validLeaseClaimID(leaseID)
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

func crabboxStateRootDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Clean(dir), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", exit(2, "user state directory is unavailable")
	}
	return filepath.Clean(dir), nil
}
