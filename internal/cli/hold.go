package cli

import (
	"context"
	"encoding/json"
	"fmt"
)

// A hold records custody or verified absence, never proof that files were salvaged.
type LeaseRecoveryHold struct {
	Schema            string              `json:"schema"`
	Provider          string              `json:"provider"`
	LeaseID           string              `json:"leaseId"`
	Status            string              `json:"status"`
	Resources         []LeaseHeldResource `json:"resources"`
	UnacceptedChanges string              `json:"unacceptedChanges"`
}

type LeaseHeldResource struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	ImmutableID string `json:"immutableId,omitempty"`
	State       string `json:"state"`
}

type FailedLeaseHoldBackend interface {
	HoldFailedLease(context.Context, string) (LeaseRecoveryHold, error)
}

// FailedLeaseHoldFinalizerBackend optionally closes an existing hold after
// read-only absence verification, retaining a durable single-use receipt.
type FailedLeaseHoldFinalizerBackend interface {
	FinalizeFailedLeaseHold(context.Context, string) (LeaseRecoveryHold, error)
}

type failedLeaseHoldWithSlugBackend interface {
	HoldFailedLeaseWithSlug(context.Context, string, string) (LeaseRecoveryHold, error)
}

func (a App) hold(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("hold", a.Stderr)
	provider := registerProviderSelectionFlag(fs, defaults, providerHelpAll())
	id := fs.String("id", "", "exact failed lease id")
	slug := fs.String("slug", "", "original fixed lease slug for claimless Azure observation")
	finalize := fs.Bool("finalize", false, "finalize an existing hold after verifying all resources absent")
	asJSON := fs.Bool("json", false, "print the durable recovery hold receipt")
	providerFlags := registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if !flagWasSet(fs, "provider") || !IsCanonicalLeaseID(*id) || fs.NArg() != 0 {
		return Exit(2, "usage: crabbox hold --provider <provider> --id <canonical-id> [--slug <original-slug> | --finalize] [--json]")
	}
	if *finalize && flagWasSet(fs, "slug") {
		return Exit(2, "hold --finalize uses the stored original slug; --slug cannot be combined with --finalize")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := prepareProviderSelection(&cfg, *provider); err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	if err := finalizeProviderSelection(&cfg); err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	var receipt LeaseRecoveryHold
	if *finalize {
		finalizer, ok := backend.(FailedLeaseHoldFinalizerBackend)
		if !ok {
			return Exit(2, "provider=%s does not support failed-lease hold finalization", backend.Spec().Name)
		}
		receipt, err = finalizer.FinalizeFailedLeaseHold(ctx, *id)
	} else if flagWasSet(fs, "slug") {
		withSlug, ok := backend.(failedLeaseHoldWithSlugBackend)
		if !ok {
			return Exit(2, "provider=%s does not support claimless hold slug input", backend.Spec().Name)
		}
		receipt, err = withSlug.HoldFailedLeaseWithSlug(ctx, *id, *slug)
	} else {
		holder, ok := backend.(FailedLeaseHoldBackend)
		if !ok {
			return Exit(2, "provider=%s does not support failed-lease holds", backend.Spec().Name)
		}
		receipt, err = holder.HoldFailedLease(ctx, *id)
	}
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(a.Stdout).Encode(receipt)
	}
	if receipt.Status == "finalized" {
		fmt.Fprintf(a.Stdout, "finalized lease=%s provider=%s; recorded resource absence; unaccepted changes remain unknown\n", receipt.LeaseID, receipt.Provider)
	} else {
		fmt.Fprintf(a.Stdout, "held lease=%s provider=%s; resources and unaccepted changes retained for salvage\n", receipt.LeaseID, receipt.Provider)
	}
	return nil
}
