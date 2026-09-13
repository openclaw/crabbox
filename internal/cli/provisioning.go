package cli

import (
	"context"
	"fmt"
	"strings"
)

// ProvisioningCandidate is one adapter-selected attempt. Order, location, market,
// and diagnostics belong to the adapter; core never invents another candidate.
type ProvisioningCandidate struct {
	Config          Config
	FailureLabel    string
	FallbackMessage string
}

// ServerProvisioner separates admission from a remote create. Preparation errors
// stop immediately, without capacity fallback or rewriting the original error.
// Create must settle its own partial resources before CanRetry permits another
// attempt. Core does not infer deletion authority from a failed create response.
type ServerProvisioner struct {
	Prepare  func(context.Context, Config) error
	Create   func(context.Context, Config) (Server, error)
	CanRetry func(error) bool
}

func ProvisionServerCandidates(ctx context.Context, original Config, candidates []ProvisioningCandidate, provisioner ServerProvisioner, logf func(string, ...any)) (Server, Config, error) {
	var errs []error
	for _, candidate := range candidates {
		if candidate.FallbackMessage != "" && logf != nil {
			logf("%s", candidate.FallbackMessage)
		}
		if provisioner.Prepare != nil {
			if err := provisioner.Prepare(ctx, candidate.Config); err != nil {
				return Server{}, candidate.Config, err
			}
		}
		server, err := provisioner.Create(ctx, candidate.Config)
		if err == nil {
			return server, candidate.Config, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", candidate.FailureLabel, err))
		if provisioner.CanRetry == nil || !provisioner.CanRetry(err) {
			return Server{}, candidate.Config, joinErrors(errs)
		}
	}
	return Server{}, original, joinErrors(errs)
}

func validateProvisioningCandidates(cfg Config, candidates []string) error {
	if len(candidates) != 0 {
		return nil
	}
	provider, _ := ProviderFor(cfg.Provider)
	if provider == nil {
		return Exit(2, "provider=%s has no class profile for class=%s", cfg.Provider, cfg.Class)
	}
	if err := validateProviderClassSelector(provider, cfg); err != nil {
		return err
	}
	return Exit(2, "provider=%s has no usable provisioning candidates for class=%s", cfg.Provider, cfg.Class)
}

// provisioningMarkets preserves the requested market before its explicit
// on-demand fallback. Providers with selective fallback build their own plan.
func provisioningMarkets(cfg Config) []string {
	markets := []string{cfg.Capacity.Market}
	if strings.EqualFold(cfg.Capacity.Market, "spot") && strings.HasPrefix(cfg.Capacity.Fallback, "on-demand") {
		markets = append(markets, "on-demand")
	}
	return markets
}
