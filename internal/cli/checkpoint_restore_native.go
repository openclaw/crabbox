package cli

import (
	"context"
	"flag"
	"fmt"
	"time"
)

// Optional native in-place restore is deliberately independent of native fork.
// Keep the checkpoint lock through the effect so deletion cannot race restoration.
func (a App) restoreNativeCheckpoint(ctx context.Context, fs *flag.FlagSet, selected string, targetFlags targetFlagValues, networkFlags networkModeFlagValues, providerFlags providerFlagValues, store checkpointStore, record checkpointRecord, id string, reclaim, dryRun bool) (bool, error) {
	if !isNativeCheckpointKind(record.Kind) {
		return false, nil
	}
	provider, err := ProviderFor(record.nativeProvider())
	if err != nil {
		return true, err
	}
	restorer, ok := provider.(NativeCheckpointRestoreProvider)
	if !ok {
		return false, nil
	}
	if reclaim {
		return true, Exit(2, "native in-place restore requires the existing repository claim; --reclaim is unsupported")
	}
	if flagWasSet(fs, "workdir") || flagWasSet(fs, "clear") {
		return true, Exit(2, "native in-place restore replaces VM state; --workdir and --clear are unsupported")
	}
	cfg, err := loadLeaseTargetConfig(fs, selected, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: id})
	if err != nil {
		return true, err
	}
	if cfg.Provider != record.nativeProvider() {
		return true, Exit(2, "checkpoint provider=%s does not match target provider=%s", record.nativeProvider(), cfg.Provider)
	}
	if err = applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return true, err
	}
	if err = requireLeaseID(id, "crabbox checkpoint restore <checkpoint-id> --id <lease-id-or-slug>", cfg); err != nil {
		return true, err
	}
	repo, err := findRepo()
	if err != nil {
		return true, err
	}
	err = store.withRecord(record.ID, true, func(current checkpointRecord) error {
		if current.nativeProvider() != record.nativeProvider() || current.Kind != record.Kind || unresolvedCheckpoint(current) {
			return Exit(2, "checkpoint identity or capture changed before restore")
		}
		server, target, leaseID, err := a.resolveLeaseTargetWithRequestConfig(ctx, &cfg, ResolveRequest{Repo: repo, ID: id, StatusOnly: true, NoLocalStateMutations: true})
		if err != nil {
			return err
		}
		claim, exists, err := ReadLeaseClaimWithPresence(leaseID)
		if err != nil {
			return err
		}
		if !exists || claim.RepoRoot != repo.Root || canonicalClaimProvider(claim.Provider) != cfg.Provider || claim.CloudID != server.CloudID {
			return Exit(4, "native restore requires the exact current source claim in this repository")
		}
		SetServerLeaseClaimSnapshot(&server, claim, true)
		lease, err := restorer.RestoreNativeCheckpoint(ctx, NativeCheckpointRestoreRequest{Config: cfg, Repo: repo, Lease: LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, Resource: nativeCheckpointResourceRequest(current), DryRun: dryRun, Stderr: a.Stderr})
		if err != nil {
			return err
		}
		if dryRun {
			_, err = fmt.Fprintf(a.Stdout, "would restore checkpoint id=%s lease=%s snapshot=%s\n", current.ID, leaseID, current.Native.ImageID)
			return err
		}
		if lease.LeaseID != leaseID || lease.Server.CloudID != server.CloudID {
			return Exit(4, "native restore returned a different lease identity")
		}
		current.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
		if err = store.Write(current); err != nil {
			return err
		}
		_, err = fmt.Fprintf(a.Stdout, "checkpoint restored id=%s lease=%s snapshot=%s\n", current.ID, leaseID, current.Native.ImageID)
		return err
	})
	return true, err
}
