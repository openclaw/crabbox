package cli

import (
	"context"
	"encoding/json"
	"fmt"
)

// A hold retains provider resources and uncertain files; it is not a release receipt.
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

func (a App) hold(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("hold", a.Stderr)
	provider := registerProviderSelectionFlag(fs, defaults, providerHelpAll())
	id := fs.String("id", "", "exact failed lease id")
	asJSON := fs.Bool("json", false, "print the durable recovery hold")
	providerFlags := registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if !flagWasSet(fs, "provider") || !IsCanonicalLeaseID(*id) || fs.NArg() != 0 {
		return Exit(2, "usage: crabbox hold --provider <provider> --id <canonical-id> [--json]")
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
	holder, ok := backend.(FailedLeaseHoldBackend)
	if !ok {
		return Exit(2, "provider=%s does not support failed-lease holds", backend.Spec().Name)
	}
	receipt, err := holder.HoldFailedLease(ctx, *id)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(a.Stdout).Encode(receipt)
	}
	fmt.Fprintf(a.Stdout, "held lease=%s provider=%s; resources and unaccepted changes retained for salvage\n", receipt.LeaseID, receipt.Provider)
	return nil
}
