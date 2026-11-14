package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const checkpointPreSubmitEvidenceMetadata = "crabbox_pre_submit_failure_sha256"

func (a App) checkpointAbandon(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("checkpoint abandon", a.Stderr)
	provider := registerProviderSelectionFlag(fs, defaults, providerHelpSSH())
	leaseID := fs.String("id", "", "exact source lease ID to dispose of")
	expectedID := fs.String("expected-provider-resource-id", "", "exact immutable source resource ID")
	preSubmitErrorFile := fs.String("pre-submit-error-file", "", "CLI preparation failure evidence for adopting an existing retirement")
	jsonOut := fs.Bool("json", false, "print the retained checkpoint record as JSON")
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	providerFlags := registerProviderFlags(fs, defaults)
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 || !canonicalLeaseIDPattern.MatchString(*leaseID) || strings.TrimSpace(*expectedID) == "" || *expectedID != strings.TrimSpace(*expectedID) {
		return Exit(2, "usage: crabbox checkpoint abandon <checkpoint-id> --id <canonical-lease-id> --expected-provider-resource-id <immutable-source-id>")
	}
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *leaseID})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	repo, err := findCommandRepo(&cfg)
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
		if !isNativeCheckpointKind(record.Kind) || record.coordinatorManaged() || record.nativeDeleteID() != "" || record.Native.ImageID != "" || record.Native.Resource != "" || record.LeaseID != *leaseID || record.Provider != cfg.Provider || !ClaimRepositoryMatches(record.Repo.Root, repo.Root, repo.LegacyRoot) {
			return Exit(2, "checkpoint abandon requires an unresolved ordinary native record for this exact source lease, provider, and repository")
		}
		backend, err := loadBackend(cfg, runtimeForApp(operation))
		if err != nil {
			return err
		}
		ssh, ok := backend.(SSHLeaseBackend)
		if !ok {
			return Exit(2, "provider=%s cannot dispose of a checkpoint source", cfg.Provider)
		}
		if record.Capture == nil {
			preparer, ok := backend.(NativeCheckpointAbandonProvider)
			if !ok {
				return Exit(2, "provider=%s cannot attest source-only checkpoint abandonment", cfg.Provider)
			}
			if err := operation.prepareCheckpointAbandonment(ctx, cfg, repo, store, &record, *expectedID, ssh, preparer); err != nil {
				return err
			}
		} else if record.Capture.SourceDisposition == "retire" {
			if err := operation.adoptPreSubmitCheckpointAbandonment(ctx, cfg, repo, store, &record, *expectedID, *preSubmitErrorFile, ssh); err != nil {
				return err
			}
		} else if strings.TrimSpace(*preSubmitErrorFile) != "" {
			digest, err := checkpointPreSubmitEvidenceDigest(*preSubmitErrorFile)
			if err != nil {
				return err
			}
			if record.Native.Metadata[checkpointPreSubmitEvidenceMetadata] != digest {
				return Exit(2, "checkpoint pre-submit recovery evidence conflicts with its retained audit")
			}
			if err := operation.restoreCheckpointAbandonSourceScope(store, &record, repo, backend); err != nil {
				return err
			}
		}
		capture := record.Capture
		if capture.SourceDisposition != "abandon" || capture.SourceID != *expectedID || capture.DiscardFailed {
			return Exit(2, "checkpoint %s abandonment identity conflicts with the original operation", record.ID)
		}
		switch capture.Phase {
		case "prepared":
			claim, err := bindCheckpointCapture(record)
			if err != nil {
				return err
			}
			if err := WithLeaseClaimUnchanged(record.LeaseID, claim, func() error {
				capture.Phase = "retiring"
				return store.Write(record)
			}); err != nil {
				return err
			}
		case "retiring", "abandoned":
		default:
			return Exit(2, "checkpoint %s has an unknown abandonment phase; retain the record", record.ID)
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
	claim, exists, err := ReadLeaseClaimWithPresence(record.LeaseID)
	if err != nil {
		return err
	}
	if !exists || claim.CloudID != expectedID || claim.Revision == "" || !ClaimRepositoryMatches(claim.RepoRoot, repo.Root, repo.LegacyRoot) || canonicalClaimProvider(claim.Provider) != cfg.Provider || claim.CheckpointCapture != nil {
		return Exit(2, "checkpoint abandonment requires the exact current unbound source claim in this repository")
	}
	expected := ProviderIdentityExpectation{LeaseID: record.LeaseID, ResourceID: expectedID}
	lease, err := backend.Resolve(ctx, ResolveRequest{Repo: repo, ID: record.LeaseID, ReleaseOnly: true, NoLocalStateMutations: true, ExpectedProviderIdentity: expected})
	if err != nil {
		return err
	}
	if lease.LeaseID != record.LeaseID || lease.Server.CloudID != expectedID || lease.Server.Name == "" {
		return Exit(2, "checkpoint source identity changed during abandonment admission")
	}
	capability, ok := nativeModeCheckpointCapability(cfg, lease.Server, lease.SSH, checkpointStrategyImage)
	if !ok || capability.Kind != record.Kind || !capability.RetireSource || capability.RetireUnsupported != "" {
		return Exit(2, "provider=%s cannot abandon this checkpoint source: %s", cfg.Provider, firstNonBlank(capability.RetireUnsupported, "source capability changed"))
	}
	return WithLeaseClaimUnchanged(record.LeaseID, claim, func() error {
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

func (a App) adoptPreSubmitCheckpointAbandonment(ctx context.Context, cfg Config, repo Repo, store checkpointStore, record *checkpointRecord, expectedID, evidencePath string, backend SSHLeaseBackend) error {
	capture := record.Capture
	if capture == nil || capture.SourceDisposition != "retire" || capture.Phase != "submitting" ||
		record.Native.ImageID != "" || record.Native.Resource != "" || record.nativeDeleteID() != "" {
		return Exit(2, "checkpoint %s is not an unresolved pre-submit retirement", record.ID)
	}
	claim, exists, err := ReadLeaseClaimWithPresence(record.LeaseID)
	if err != nil {
		return err
	}
	if !exists || claim.CloudID != expectedID || claim.Revision == "" || !ClaimRepositoryMatches(claim.RepoRoot, repo.Root, repo.LegacyRoot) || canonicalClaimProvider(claim.Provider) != cfg.Provider {
		return Exit(2, "checkpoint pre-submit recovery requires the exact current source claim in this repository")
	}
	if err := ValidateCheckpointCaptureClaim(claim, record.ID, capture); err != nil {
		return err
	}
	expected := ProviderIdentityExpectation{LeaseID: record.LeaseID, ResourceID: expectedID}
	lease, err := backend.Resolve(ctx, ResolveRequest{Repo: repo, ID: record.LeaseID, ReleaseOnly: true, NoLocalStateMutations: true, ExpectedProviderIdentity: expected})
	if err != nil {
		return err
	}
	if lease.LeaseID != record.LeaseID || lease.Server.CloudID != expectedID || lease.Server.Name != capture.SourceName {
		return Exit(2, "checkpoint source identity changed during pre-submit recovery")
	}
	capability, ok := nativeModeCheckpointCapability(cfg, lease.Server, lease.SSH, record.Native.Strategy)
	if !ok || capability.Kind != record.Kind || !capability.RetireSource || capability.RetireUnsupported != "" {
		return Exit(2, "provider=%s cannot abandon this checkpoint source: %s", cfg.Provider, firstNonBlank(capability.RetireUnsupported, "source capability changed"))
	}
	digest, err := checkpointPreSubmitEvidenceDigest(evidencePath)
	if err != nil {
		return err
	}
	return WithLeaseClaimUnchanged(record.LeaseID, claim, func() error {
		if err := requireResolvedSourceCheckpointsExcept(store, record.LeaseID, record.ID); err != nil {
			return err
		}
		if record.Native.Metadata == nil {
			record.Native.Metadata = make(map[string]string)
		}
		if prior := record.Native.Metadata[checkpointPreSubmitEvidenceMetadata]; prior != "" && prior != digest {
			return Exit(2, "checkpoint pre-submit recovery evidence conflicts with its retained audit")
		}
		record.Native.Metadata[checkpointPreSubmitEvidenceMetadata] = digest
		if err := setCheckpointAbandonSourceScope(record, &claim, backend); err != nil {
			return err
		}
		capture.SourceDisposition = "abandon"
		capture.Phase = "prepared"
		capture.Error = "source disposal pending; CLI pre-submit failure evidence sha256=" + digest + "; original image submission remains unresolved; retain this checkpoint record"
		return store.Write(*record)
	})
}

func checkpointPreSubmitEvidenceDigest(path string) (string, error) {
	evidence, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return "", Exit(2, "read --pre-submit-error-file: %v", err)
	}
	if len(evidence) == 0 || len(evidence) > 64*1024 || !strings.HasPrefix(strings.TrimSpace(string(evidence)), "prepare native checkpoint source:") || strings.IndexByte(string(evidence), 0) >= 0 {
		return "", Exit(2, "--pre-submit-error-file must contain the CLI's bounded prepare native checkpoint source failure")
	}
	return fmt.Sprintf("%x", sha256.Sum256(evidence)), nil
}

func (a App) restoreCheckpointAbandonSourceScope(store checkpointStore, record *checkpointRecord, repo Repo, backend any) error {
	if _, ok := backend.(NativeCheckpointAbandonScopeProvider); !ok {
		return nil
	}
	claim, exists, err := ReadLeaseClaimWithPresence(record.LeaseID)
	if err != nil {
		return err
	}
	capture := record.Capture
	if capture == nil || capture.SourceDisposition != "abandon" {
		return Exit(2, "checkpoint abandonment cleanup scope no longer matches its exact source receipt")
	}
	if !exists {
		if capture.SourceIntent != "" {
			return Exit(2, "checkpoint abandonment cleanup scope no longer matches its exact source receipt")
		}
		// Do not reconstruct missing ordinary authority. The provider validates
		// retained coordinates; retirement still proves exact absence and fences
		// the absent claim before publishing the terminal journal.
		return setCheckpointAbandonSourceScope(record, nil, backend)
	}
	if claim.CloudID != capture.SourceID || claim.ProviderScope != capture.SourceScope || claim.ClaimedAt != capture.SourceClaimedAt ||
		!ClaimRepositoryMatches(claim.RepoRoot, repo.Root, repo.LegacyRoot) || canonicalClaimProvider(claim.Provider) != record.Provider ||
		(capture.SourceIntent != "" && (claim.FixedCreateIntent == nil || claim.FixedCreateIntent.Fingerprint != capture.SourceIntent)) ||
		(capture.SourceIntent == "" && claim.FixedCreateIntent != nil) {
		return Exit(2, "checkpoint abandonment cleanup scope no longer matches its exact source receipt")
	}
	if claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "released" {
		if err := ValidateCheckpointCaptureClaim(claim, record.ID, capture); err != nil {
			return err
		}
	}
	return WithLeaseClaimUnchanged(record.LeaseID, claim, func() error {
		if err := setCheckpointAbandonSourceScope(record, &claim, backend); err != nil {
			return err
		}
		return store.Write(*record)
	})
}

func setCheckpointAbandonSourceScope(record *checkpointRecord, claim *leaseClaim, backend any) error {
	attester, ok := backend.(NativeCheckpointAbandonScopeProvider)
	if !ok {
		return nil
	}
	scope, err := attester.NativeCheckpointAbandonScope(claim, *record.Capture, NativeCheckpointImage{AccountID: record.Native.AccountID, Region: record.Native.Region})
	if err != nil {
		return err
	}
	record.Native.AccountID, record.Native.Region = scope.AccountID, scope.Region
	return nil
}
