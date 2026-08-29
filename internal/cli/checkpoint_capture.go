package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrCheckpointPending is a bounded observation, never authorization to repeat
// a mutation whose response was lost.
var ErrCheckpointPending = errors.New("checkpoint source transition is pending; replay the same checkpoint operation")
var ErrNativeCheckpointAbsent = errors.New("exact native checkpoint resource is absent")

type checkpointCaptureUnsupported struct{ ExitError }

type checkpointCaptureAdmission struct {
	ID                string `json:"id"`
	LeaseID           string `json:"leaseId"`
	Provider          string `json:"provider"`
	SourceID          string `json:"sourceId"`
	SourceDisposition string `json:"sourceDisposition"`
	Admission         string `json:"admission"`
	Reason            string `json:"reason,omitempty"`
}

func (a App) checkpointRetire(ctx context.Context, cfg Config, repo Repo, leaseID, id, strategy string, noReboot, discardFailed, prepareOnly, jsonOut bool, stdout io.Writer) error {
	if !canonicalLeaseIDPattern.MatchString(leaseID) {
		return exit(2, "--retire-source requires an exact canonical --id, not a source name or slug")
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		return err
	}
	return store.WithLock(id, func() error {
		record, _, err := store.Read(id)
		if isCheckpointNotFound(err) {
			if discardFailed {
				return exit(2, "--discard-failed requires an existing failed capture")
			}
			var claim leaseClaim
			record, claim, err = a.prepareCheckpointCapture(ctx, cfg, repo, store, leaseID, id, strategy, noReboot)
			if prepareOnly {
				admission := checkpointCaptureAdmission{ID: id, LeaseID: leaseID, Provider: cfg.Provider, SourceID: claim.CloudID, SourceDisposition: "retire", Admission: "ready"}
				var unsupported checkpointCaptureUnsupported
				if errors.As(err, &unsupported) {
					admission.Admission, admission.Reason = "unsupported", unsupported.Error()
				} else if err != nil {
					return err
				}
				if jsonOut {
					return json.NewEncoder(stdout).Encode(admission)
				}
				_, err = fmt.Fprintf(stdout, "checkpoint id=%s admission=%s source=%s reason=%s\n", id, admission.Admission, admission.SourceID, blank(admission.Reason, "-"))
				return err
			}
			if err == nil {
				record, _, err = reserveSourceCheckpoint(store, record, claim)
			}
		} else if err == nil && prepareOnly {
			return exit(2, "checkpoint %s already has an operation record; inspect and replay it instead of preparing a new capture", id)
		}
		if err != nil {
			return err
		}
		if record.Capture == nil || record.Capture.SourceDisposition != "retire" || record.LeaseID != leaseID || record.Provider != cfg.Provider || record.Repo.Root != repo.Root || record.Native.NoReboot != noReboot || record.Native.Strategy != checkpointCreateStrategy("native", strategy, record.Kind) || record.Capture.StrategyExplicit != !isAutoCheckpointStrategy(strategy) {
			return exit(2, "checkpoint %s capture identity or options conflict; retain the original operation", id)
		}
		switch record.Capture.Phase {
		case "prepared", "stopping", "submitting", "pending", "ready", "retiring", "retired", "failed":
		default:
			return exit(2, "checkpoint %s has an unknown capture phase; retain its record", id)
		}
		if discardFailed && !record.Capture.DiscardFailed {
			if record.Capture.Phase != "failed" {
				return exit(2, "--discard-failed requires a verified terminal capture failure")
			}
			record.Capture.DiscardFailed = true
			if err := store.Write(record); err != nil {
				return err
			}
		}
		if err := a.advanceCheckpointCapture(ctx, cfg, repo, store, &record); err != nil {
			return err
		}
		if jsonOut {
			return json.NewEncoder(stdout).Encode(record)
		}
		_, err = fmt.Fprintf(stdout, "checkpoint id=%s source_disposition=retire phase=%s resource=%s\n", id, record.Capture.Phase, blank(record.Native.ImageID, "-"))
		return err
	})
}

// Admission is read-only. In particular, a policy refusal must not reserve a
// journal or bind the claim and thereby prevent its ordinary release policy.
func (a App) prepareCheckpointCapture(ctx context.Context, cfg Config, repo Repo, store checkpointStore, leaseID, id, strategy string, noReboot bool) (checkpointRecord, leaseClaim, error) {
	claim, exists, err := readLeaseClaimWithPresence(leaseID)
	if err != nil {
		return checkpointRecord{}, claim, err
	}
	if !exists || claim.CloudID == "" || claim.Revision == "" || claim.RepoRoot != repo.Root || canonicalClaimProvider(claim.Provider) != cfg.Provider {
		return checkpointRecord{}, claim, exit(2, "checkpoint retirement requires an exact current source claim in this repository")
	}
	server, target, resolvedID, err := a.resolveLeaseTargetWithRequestConfig(ctx, &cfg, ResolveRequest{Repo: repo, ID: leaseID, ReleaseOnly: true, NoLocalStateMutations: true})
	if err != nil {
		return checkpointRecord{}, claim, err
	}
	if resolvedID != leaseID || server.CloudID != claim.CloudID {
		return checkpointRecord{}, claim, exit(2, "checkpoint source identity changed during resolution")
	}
	capability, ok := nativeModeCheckpointCapability(cfg, server, target, strategy)
	err = withLeaseClaimUnchanged(leaseID, claim, func() error {
		if err := authorizeSourceCheckpointAdmission(store, claim); err != nil {
			return err
		}
		if !ok || !capability.RetireSource || capability.Kind == "" || capability.CreateUnsupported != "" || capability.RetireUnsupported != "" {
			reason := firstNonBlank(capability.RetireUnsupported, capability.CreateUnsupported, "provider does not support this source retirement strategy")
			return checkpointCaptureUnsupported{exit(2, "provider=%s: %s", cfg.Provider, reason)}
		}
		return nil
	})
	if err != nil {
		return checkpointRecord{}, claim, err
	}
	workdir := remoteJoin(cfg, leaseID, repo.Name)
	if provider, ok := nativeCheckpointLifecycleProvider(cfg, server); ok {
		workdir = provider.NativeCheckpointWorkdir(NativeCheckpointWorkdirRequest{Config: cfg, Server: server, LeaseID: leaseID, RepoName: repo.Name})
	}
	record, _, err := newCheckpointRecord(repo, cfg, server, target, leaseID, workdir, "")
	if err != nil {
		return checkpointRecord{}, claim, err
	}
	record.ID, record.Kind = id, capability.Kind
	record.Native.Strategy, record.Native.NoReboot = checkpointCreateStrategy("native", strategy, capability.Kind), noReboot
	record.Capture = &NativeCheckpointCapture{SourceDisposition: "retire", Phase: "prepared", StrategyExplicit: !isAutoCheckpointStrategy(strategy), SourceID: claim.CloudID, SourceName: server.Name, SourceScope: claim.ProviderScope, SourceRevision: claim.Revision, SourceClaimedAt: claim.ClaimedAt}
	if claim.FixedCreateIntent != nil {
		record.Capture.SourceIntent = claim.FixedCreateIntent.Fingerprint
	}
	return record, claim, nil
}

func reserveSourceCheckpoint(store checkpointStore, record checkpointRecord, claim leaseClaim) (checkpointRecord, checkpointPaths, error) {
	var paths checkpointPaths
	err := withLeaseClaimUnchanged(record.LeaseID, claim, func() error {
		if err := authorizeSourceCheckpointAdmission(store, claim); err != nil {
			return err
		}
		var err error
		record, paths, err = store.Reserve(record)
		return err
	})
	return record, paths, err
}

func authorizeSourceCheckpointAdmission(store checkpointStore, claim leaseClaim) error {
	if claim.CheckpointCapture != nil {
		if _, _, err := store.Read(claim.CheckpointCapture.ID); isCheckpointNotFound(err) {
			return exit(2, "checkpoint %s journal is missing but its source claim is still held; restore the journal and inspect the exact retained source and image before recovery", claim.CheckpointCapture.ID)
		}
		return exit(2, "source lease already belongs to checkpoint %s; replay that operation", claim.CheckpointCapture.ID)
	}
	return requireResolvedSourceCheckpoints(store, claim.LeaseID)
}

func requireResolvedSourceCheckpoints(store checkpointStore, leaseID string) error {
	if leaseID == "" {
		return nil
	}
	records, err := store.List()
	if err != nil {
		return err
	}
	for _, prior := range records {
		if prior.LeaseID == leaseID && unresolvedCheckpoint(prior) {
			return exit(2, "source has unresolved checkpoint %s; inspect and reconcile it before capture, release, or reuse", prior.ID)
		}
	}
	return nil
}

func bindCheckpointCapture(record checkpointRecord) (leaseClaim, error) {
	var bound leaseClaim
	err := withDurableLeaseClaimLock(record.LeaseID, func(claim *leaseClaim, exists bool, persist func() error) error {
		capture := record.Capture
		if !exists || claim.CloudID != capture.SourceID || claim.ProviderScope != capture.SourceScope || claim.ClaimedAt != capture.SourceClaimedAt || claim.RepoRoot != record.Repo.Root || canonicalClaimProvider(claim.Provider) != record.Provider {
			return exit(2, "checkpoint %s source claim changed; refusing capture or retirement", record.ID)
		}
		if claim.CheckpointCapture == nil {
			if capture.Phase != "prepared" || claim.Revision != capture.SourceRevision {
				return exit(2, "checkpoint %s source claim generation changed", record.ID)
			}
			claim.CheckpointCapture = &CheckpointCaptureBinding{ID: record.ID, Revision: capture.SourceRevision}
			if err := persist(); err != nil {
				return err
			}
		}
		if claim.CheckpointCapture.ID != record.ID || claim.CheckpointCapture.Revision != capture.SourceRevision || claim.CheckpointCapture.BoundRevision != claim.Revision {
			return exit(2, "checkpoint %s does not own this source claim generation", record.ID)
		}
		bound = cloneLeaseClaim(*claim)
		return nil
	})
	return bound, err
}

// ValidateCheckpointCaptureClaim checks the journal binding, not remote metadata.
// The provider still must freshly attest the native resource before each effect.
func ValidateCheckpointCaptureClaim(claim LeaseClaim, id string, capture *NativeCheckpointCapture) error {
	if capture == nil || claim.CheckpointCapture == nil || claim.CheckpointCapture.ID != id || claim.CheckpointCapture.Revision != capture.SourceRevision || claim.CheckpointCapture.BoundRevision != claim.Revision || claim.CloudID != capture.SourceID || claim.ProviderScope != capture.SourceScope || claim.ClaimedAt != capture.SourceClaimedAt {
		return exit(2, "checkpoint %s no longer owns the exact source claim", id)
	}
	return nil
}

// AuthorizeCheckpointRelease prevents ordinary release/cleanup from cutting
// through a source-dependent snapshot. Only the recorded retirement may release.
func AuthorizeCheckpointRelease(claim LeaseClaim, checkpointID string) error {
	if claim.CheckpointCapture == nil {
		if checkpointID != "" {
			return exit(2, "checkpoint %s source claim binding disappeared", checkpointID)
		}
		store, err := defaultCheckpointStore()
		if err != nil {
			return err
		}
		return requireResolvedSourceCheckpoints(store, claim.LeaseID)
	}
	if checkpointID == "" || claim.CheckpointCapture.ID != checkpointID {
		return exit(2, "source is held by checkpoint %s; replay checkpoint create --retire-source with that --checkpoint-id", claim.CheckpointCapture.ID)
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		return err
	}
	record, _, err := store.Read(checkpointID)
	if err != nil {
		return err
	}
	if err := ValidateCheckpointCaptureClaim(claim, checkpointID, record.Capture); err != nil {
		return err
	}
	if record.Capture.Phase != "retiring" {
		return exit(2, "checkpoint %s has not authorized source retirement", checkpointID)
	}
	return nil
}

func (a App) advanceCheckpointCapture(ctx context.Context, cfg Config, repo Repo, store checkpointStore, record *checkpointRecord) error {
	capture := record.Capture
	if capture.Phase == "retired" {
		return nil
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	ssh, ok := backend.(SSHLeaseBackend)
	if !ok {
		return exit(2, "provider=%s does not support source retirement", cfg.Provider)
	}
	if capture.Phase == "retiring" {
		return a.retireCheckpointSource(ctx, cfg, repo, store, record, ssh)
	}
	if capture.Phase == "failed" && capture.DiscardFailed {
		return a.discardFailedCheckpoint(ctx, cfg, repo, store, record, ssh)
	}
	claim, err := bindCheckpointCapture(*record)
	if err != nil {
		return err
	}
	server, target, _, err := a.resolveLeaseTargetWithRequestConfig(ctx, &cfg, ResolveRequest{Repo: repo, ID: record.LeaseID, ReleaseOnly: true, NoLocalStateMutations: true})
	if err != nil {
		return err
	}
	if server.CloudID != capture.SourceID {
		return exit(2, "checkpoint source was replaced")
	}
	capability, ok := nativeCheckpointCapability(NativeCheckpointRequest{Config: cfg, Server: server, Target: target, Strategy: record.Native.Strategy, StrategyExplicit: capture.StrategyExplicit})
	if !ok || !capability.RetireSource || capability.Kind != record.Kind || capability.CreateUnsupported != "" || capability.RetireUnsupported != "" {
		return exit(2, "checkpoint %s source retirement is no longer eligible; retain its operation: %s", record.ID, firstNonBlank(capability.RetireUnsupported, capability.CreateUnsupported, "source capability changed"))
	}
	observe := func(result NativeCheckpointCreateResult) error {
		strategy := record.Native.Strategy
		if result.Image.ID != "" {
			applyNativeImageCheckpointRecord(record, coordinatorImageFromNativeCheckpoint(result.Image), record.Native.NoReboot)
			record.Native.Strategy = strategy
		}
		if result.Metadata != nil {
			record.Native.Metadata = result.Metadata
		}
		return store.Write(*record)
	}
	if capability.ReplayCapture {
		provider, ok := nativeCheckpointLifecycleProvider(cfg, server)
		if !ok {
			return exit(2, "checkpoint capture provider is unavailable")
		}
		_, err = provider.CreateNativeCheckpoint(ctx, NativeCheckpointCreateRequest{Config: cfg, Server: server, Target: target, CheckpointID: record.ID, LeaseID: record.LeaseID, RepoName: record.Repo.Name, Workdir: record.Workdir, Strategy: record.Native.Strategy, NoReboot: record.Native.NoReboot, Stderr: a.Stderr, Capture: capture, Metadata: record.Native.Metadata, Persist: observe})
		if err != nil {
			return err
		}
	} else if capture.Phase == "prepared" {
		// Generic providers get at-most-once submission. A lost identity remains
		// held; only adapters with an attested reconciliation path may recover it.
		capture.Phase = "submitting"
		if err := store.Write(*record); err != nil {
			return err
		}
		claim, _, err = readLeaseClaimWithPresence(record.LeaseID)
		if err != nil {
			return err
		}
		if err := ValidateCheckpointCaptureClaim(claim, record.ID, capture); err != nil {
			return err
		}
		image, metadata, createErr := a.createNativeCheckpointRequest(ctx, NativeCheckpointCreateRequest{Config: cfg, Server: server, Target: target, CheckpointID: record.ID, LeaseID: record.LeaseID, RepoName: repo.Name, Workdir: record.Workdir, Strategy: record.Native.Strategy, NoReboot: record.Native.NoReboot, Stderr: a.Stderr, Capture: capture})
		if image.ID != "" {
			strategy := record.Native.Strategy
			applyNativeImageCheckpointRecord(record, image, record.Native.NoReboot)
			record.Native.Strategy = strategy
			record.Native.Metadata = metadata
			capture.Phase = "pending"
		}
		if err := store.Write(*record); err != nil {
			return err
		}
		if createErr != nil {
			return createErr
		}
	}
	if capture.Phase == "failed" || record.Native.ImageID == "" {
		return nil
	}
	audit, err := a.verifyCheckpointResource(ctx, store, *record)
	if err != nil {
		return err
	}
	if audit.Error != "" {
		return exit(5, "checkpoint %s verification pending: %s", record.ID, audit.Error)
	}
	switch audit.NextAction {
	case "fork_or_delete":
		capture.Phase = "retiring"
	case "delete":
		capture.Phase = "failed"
		capture.Error = "native capture failed; retain the source and exact image for operator recovery"
	default:
		capture.Phase = "pending"
	}
	if err := store.Write(*record); err != nil {
		return err
	}
	if capture.Phase == "retiring" {
		return a.retireCheckpointSource(ctx, cfg, repo, store, record, ssh)
	}
	return nil
}

func (a App) discardFailedCheckpoint(ctx context.Context, cfg Config, repo Repo, store checkpointStore, record *checkpointRecord, backend SSHLeaseBackend) error {
	claim, exists, err := readLeaseClaimWithPresence(record.LeaseID)
	if err != nil {
		return err
	}
	if !exists {
		return exit(2, "failed checkpoint source claim is missing; retain operation")
	}
	err = withLeaseClaimUnchanged(record.LeaseID, claim, func() error {
		if err := ValidateCheckpointCaptureClaim(claim, record.ID, record.Capture); err != nil {
			return err
		}
		audit, err := a.verifyCheckpointResource(ctx, store, *record)
		if err != nil {
			return err
		}
		if audit.Error != "" {
			return exit(5, "failed checkpoint cleanup remains held: %s", audit.Error)
		}
		if audit.ProviderState != "missing" {
			if audit.NextAction != "delete" {
				return exit(2, "checkpoint %s no longer has a verified failed image; retain operation", record.ID)
			}
			if err := deleteCheckpointResource(ctx, store, *record); err != nil {
				return err
			}
			audit, err = a.verifyCheckpointResource(ctx, store, *record)
			if err != nil {
				return err
			}
			if audit.Error != "" || audit.ProviderState != "missing" {
				return exit(5, "checkpoint %s image removal is unconfirmed; replay its cleanup", record.ID)
			}
		}
		record.Capture.Phase = "retiring"
		return store.Write(*record)
	})
	if err != nil {
		return err
	}
	return a.retireCheckpointSource(ctx, cfg, repo, store, record, backend)
}

func (a App) retireCheckpointSource(ctx context.Context, cfg Config, repo Repo, store checkpointStore, record *checkpointRecord, backend SSHLeaseBackend) error {
	claim, exists, err := readLeaseClaimWithPresence(record.LeaseID)
	if err != nil {
		return err
	}
	// Observe inventory even after an acknowledged removal or terminal claim.
	// A CLI exit, lost claim, or cache eviction alone never proves absence.
	servers, err := backend.List(ctx, ListRequest{All: true})
	if err != nil {
		return err
	}
	var source *Server
	for i := range servers {
		server := &servers[i]
		if server.Name == record.Capture.SourceName && server.CloudID != record.Capture.SourceID {
			return exit(2, "checkpoint source name now belongs to another resource")
		}
		if server.CloudID == record.Capture.SourceID {
			if source != nil {
				return exit(2, "checkpoint source inventory is ambiguous")
			}
			source = server
		}
	}
	if source == nil {
		verifier, ok := backend.(CheckpointSourceVerifier)
		if !ok {
			return exit(2, "provider=%s cannot attest exact checkpoint source absence; retain operation", record.Provider)
		}
		resource := nativeCheckpointResourceRequest(*record)
		resource.LoadConfig = func() (Config, error) { return cfg, nil }
		absent, err := verifier.CheckpointSourceAbsent(ctx, CheckpointSourceRequest{LeaseID: record.LeaseID, Capture: *record.Capture, Resource: resource, AccountID: record.Native.AccountID})
		if err != nil {
			return err
		}
		if !absent {
			return exit(5, "checkpoint source is absent from lease inventory but provider absence is unconfirmed; retain operation")
		}
		if exists && claim.CloudID != "" && (claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "released") {
			if err := ValidateCheckpointCaptureClaim(claim, record.ID, record.Capture); err != nil {
				return err
			}
			// Providers finalize their own tombstones and sidecars on replay.
			if err := backend.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: record.LeaseID, Server: Server{CloudID: record.Capture.SourceID, Name: record.Capture.SourceName, Provider: record.Provider, Labels: claim.Labels, ImmutableID: claim.CloudImmutableID, ID: claim.CloudNumericID}}, CheckpointID: record.ID}); err != nil {
				return err
			}
			claim, exists, err = readLeaseClaimWithPresence(record.LeaseID)
			if err != nil {
				return err
			}
			if exists && claim.CloudID != "" && (claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "released") {
				return exit(5, "checkpoint source claim finalization remains pending; retain operation")
			}
		}
		if exists {
			if claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "released" || claim.FixedCreateIntent.Fingerprint != record.Capture.SourceIntent || claim.ClaimedAt != record.Capture.SourceClaimedAt {
				return exit(2, "checkpoint source terminal claim changed")
			}
			if claim.CloudID != "" {
				// A provider may retain immutable identity after removing the capture
				// binding. Reuse its scoped receipt checks, never reinterpret it as live.
				if claim.CloudID != record.Capture.SourceID || claim.ProviderScope != record.Capture.SourceScope || claim.RepoRoot != record.Repo.Root || canonicalClaimProvider(claim.Provider) != record.Provider {
					return exit(2, "checkpoint source terminal resource identity changed")
				}
				retainer, ok := backend.(ReleaseLeaseClaimRetentionVerifier)
				if !ok {
					return exit(2, "provider=%s cannot attest its retained checkpoint source receipt", record.Provider)
				}
				expected := ProviderIdentityExpectation{LeaseID: record.LeaseID, ResourceID: record.Capture.SourceID}
				lease, err := backend.Resolve(ctx, ResolveRequest{Repo: repo, ID: record.LeaseID, ReleaseOnly: true, NoLocalStateMutations: true, ExpectedProviderIdentity: expected})
				if err != nil {
					return err
				}
				retained, err := retainer.RetainLeaseClaimAfterReleaseWithClaim(lease, claim)
				if err != nil {
					return err
				}
				if !retained {
					return exit(2, "checkpoint source terminal receipt was not retained")
				}
				if err := backend.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: lease, CheckpointID: record.ID, ExpectedProviderIdentity: expected}); err != nil {
					return err
				}
			}
		}
		return withDurableLeaseClaimLock(record.LeaseID, func(current *leaseClaim, currentExists bool, _ func() error) error {
			if err := unchangedLeaseClaimGuard(record.LeaseID, claim, exists)(*current, currentExists); err != nil {
				return err
			}
			record.Capture.Phase = "retired"
			return store.Write(*record)
		})
	}
	if !exists {
		return exit(2, "checkpoint source exists without its bound claim; retain operation")
	}
	if err := ValidateCheckpointCaptureClaim(claim, record.ID, record.Capture); err != nil {
		return err
	}
	if !record.Capture.DiscardFailed {
		audit, err := a.verifyCheckpointResource(ctx, store, *record)
		if err != nil {
			return err
		}
		if audit.Error != "" || audit.NextAction != "fork_or_delete" {
			return exit(5, "checkpoint %s is no longer verified ready; retain source and operation: %s", record.ID, audit.Error)
		}
	}
	lease, err := backend.Resolve(ctx, ResolveRequest{Repo: repo, ID: record.LeaseID, Options: leaseOptionsFromConfig(cfg), ReleaseOnly: true, NoLocalStateMutations: true})
	if err != nil {
		return err
	}
	if lease.Server.CloudID != record.Capture.SourceID {
		return exit(2, "checkpoint source changed before retirement")
	}
	SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
	err = backend.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: lease, CheckpointID: record.ID})
	if errors.Is(err, ErrCheckpointPending) {
		return nil
	}
	if err != nil {
		return err
	}
	// Do not loop or assume asynchronous deletion has completed. The next bounded
	// replay checks inventory and publishes retired only after proven absence.
	return nil
}

func unresolvedCheckpoint(record checkpointRecord) bool {
	return isNativeCheckpointKind(record.Kind) && (record.Capture != nil && record.Capture.Phase != "retired" || strings.TrimSpace(nativeCheckpointDeleteID(record)) == "")
}
