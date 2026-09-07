package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func (a App) checkpointAbandon(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("checkpoint abandon", a.Stderr)
	provider := registerProviderSelectionFlag(fs, defaults, providerHelpSSH())
	leaseID := fs.String("id", "", "exact source lease ID to dispose of")
	expectedID := fs.String("expected-provider-resource-id", "", "exact immutable source resource ID")
	jsonOut := fs.Bool("json", false, "print the retained checkpoint record as JSON")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	providerFlags := registerProviderFlags(fs, defaults)
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 || !canonicalLeaseIDPattern.MatchString(*leaseID) || strings.TrimSpace(*expectedID) == "" || *expectedID != strings.TrimSpace(*expectedID) {
		return exit(2, "usage: crabbox checkpoint abandon <checkpoint-id> --id <canonical-lease-id> --expected-provider-resource-id <immutable-source-id>")
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *leaseID})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	store, err := defaultCheckpointStore()
	if err != nil {
		return err
	}
	operation := a
	if *jsonOut {
		operation.Stdout = a.Stderr
	}
	return store.WithLock(fs.Arg(0), func() error {
		record, _, err := store.Read(fs.Arg(0))
		if err != nil {
			return err
		}
		if !isNativeCheckpointKind(record.Kind) || record.coordinatorManaged() || nativeCheckpointDeleteID(record) != "" || record.Native.ImageID != "" || record.Native.Resource != "" || record.LeaseID != *leaseID || record.Provider != cfg.Provider || record.Repo.Root != repo.Root {
			return exit(2, "checkpoint abandon requires an unresolved ordinary native record for this exact source lease, provider, and repository")
		}
		backend, err := loadBackend(cfg, runtimeForApp(operation))
		if err != nil {
			return err
		}
		ssh, ok := backend.(SSHLeaseBackend)
		if !ok {
			return exit(2, "provider=%s cannot dispose of a checkpoint source", cfg.Provider)
		}
		preparer, ok := backend.(NativeCheckpointAbandonProvider)
		if !ok {
			return exit(2, "provider=%s cannot attest source-only checkpoint abandonment", cfg.Provider)
		}
		if record.Capture == nil {
			if err := operation.prepareCheckpointAbandonment(ctx, cfg, repo, store, &record, *expectedID, ssh, preparer); err != nil {
				return err
			}
		}
		capture := record.Capture
		if capture.SourceDisposition != "abandon" || capture.SourceID != *expectedID || capture.DiscardFailed {
			return exit(2, "checkpoint %s abandonment identity conflicts with the original operation", record.ID)
		}
		switch capture.Phase {
		case "prepared":
			claim, err := bindCheckpointCapture(record)
			if err != nil {
				return err
			}
			if err := withLeaseClaimUnchanged(record.LeaseID, claim, func() error {
				capture.Phase = "retiring"
				return store.Write(record)
			}); err != nil {
				return err
			}
		case "retiring", "abandoned":
		default:
			return exit(2, "checkpoint %s has an unknown abandonment phase; retain the record", record.ID)
		}
		if err := operation.retireCheckpointSource(ctx, cfg, repo, store, &record, ssh); err != nil {
			return err
		}
		if *jsonOut {
			return json.NewEncoder(a.Stdout).Encode(record)
		}
		_, err = fmt.Fprintf(a.Stdout, "checkpoint id=%s source_disposition=abandon phase=%s image_submission=unresolved\n%s\n", record.ID, capture.Phase, capture.Error)
		return err
	})
}

func (a App) prepareCheckpointAbandonment(ctx context.Context, cfg Config, repo Repo, store checkpointStore, record *checkpointRecord, expectedID string, backend SSHLeaseBackend, preparer NativeCheckpointAbandonProvider) error {
	claim, exists, err := readLeaseClaimWithPresence(record.LeaseID)
	if err != nil {
		return err
	}
	if !exists || claim.CloudID != expectedID || claim.Revision == "" || claim.RepoRoot != repo.Root || canonicalClaimProvider(claim.Provider) != cfg.Provider || claim.CheckpointCapture != nil {
		return exit(2, "checkpoint abandonment requires the exact current unbound source claim in this repository")
	}
	expected := ProviderIdentityExpectation{LeaseID: record.LeaseID, ResourceID: expectedID}
	lease, err := backend.Resolve(ctx, ResolveRequest{Repo: repo, ID: record.LeaseID, ReleaseOnly: true, NoLocalStateMutations: true, ExpectedProviderIdentity: expected})
	if err != nil {
		return err
	}
	if lease.LeaseID != record.LeaseID || lease.Server.CloudID != expectedID || lease.Server.Name == "" {
		return exit(2, "checkpoint source identity changed during abandonment admission")
	}
	capability, ok := nativeModeCheckpointCapability(cfg, lease.Server, lease.SSH, checkpointStrategyImage)
	if !ok || capability.Kind != record.Kind || !capability.RetireSource || capability.RetireUnsupported != "" {
		return exit(2, "provider=%s cannot abandon this checkpoint source: %s", cfg.Provider, firstNonBlank(capability.RetireUnsupported, "source capability changed"))
	}
	return withLeaseClaimUnchanged(record.LeaseID, claim, func() error {
		if err := requireResolvedSourceCheckpointsExcept(store, record.LeaseID, record.ID); err != nil {
			return err
		}
		capture := &NativeCheckpointCapture{SourceDisposition: "abandon", Phase: "prepared", SourceID: expectedID, SourceName: lease.Server.Name, SourceScope: claim.ProviderScope, SourceRevision: claim.Revision, SourceClaimedAt: claim.ClaimedAt, Error: "source disposal pending; original image submission remains unresolved; retain this checkpoint record"}
		if claim.FixedCreateIntent != nil {
			capture.SourceIntent = claim.FixedCreateIntent.Fingerprint
		}
		metadata, err := preparer.PrepareNativeCheckpointAbandon(ctx, NativeCheckpointCreateRequest{Config: cfg, Server: lease.Server, Target: lease.SSH, CheckpointID: record.ID, LeaseID: record.LeaseID, Name: record.Name, Capture: capture, Metadata: record.Native.Metadata})
		if err != nil {
			return err
		}
		// This journal authorizes only the newly requested source disposal. The
		// original image outcome stays unknown, including after source removal.
		record.Capture, record.Native.Metadata = capture, metadata
		return store.Write(*record)
	})
}
