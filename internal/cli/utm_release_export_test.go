//go:build darwin

package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
)

// These test-only bridges let cli_test use the actual UTM backend without an
// import cycle or adding production hooks to the release path. Tests using the
// provider override must remain serial, just like the other registry fixtures.
func PrepareUTMCoreReleaseTest(t *testing.T) { clearConfigEnv(t) }

func AutomaticUTMReleaseForTest(ctx context.Context, backend SSHLeaseBackend, cfg Config, lease LeaseTarget) error {
	var diagnostic bytes.Buffer
	_, err := (App{Stdout: io.Discard, Stderr: &diagnostic}).releaseBackendLeaseWithOutcomeBestEffort(ctx, backend, cfg, lease)
	if err != nil {
		return fmt.Errorf("%w; diagnostics: %s", err, diagnostic.String())
	}
	return err
}

func ExplicitUTMReleaseForTest(t *testing.T, ctx context.Context, backend SSHLeaseBackend, id string, afterResolve func() error) error {
	t.Helper()
	original := providerRegistry["utm"]
	providerRegistry["utm"] = utmReleaseProviderForTest{Provider: original, backend: &utmReleaseBackendForTest{SSHLeaseBackend: backend, afterResolve: afterResolve}}
	defer func() { providerRegistry["utm"] = original }()
	var diagnostic bytes.Buffer
	err := (App{Stdout: io.Discard, Stderr: &diagnostic}).stop(ctx, []string{"--provider", "utm", "--id", id})
	if err != nil {
		return fmt.Errorf("%w; diagnostics: %s", err, diagnostic.String())
	}
	return nil
}

type utmReleaseProviderForTest struct {
	Provider
	backend SSHLeaseBackend
}

func (p utmReleaseProviderForTest) Configure(Config, Runtime) (Backend, error) { return p.backend, nil }
func (p utmReleaseProviderForTest) ClaimScope(cfg Config) string {
	return p.Provider.(ProviderClaimScoper).ClaimScope(cfg)
}
func (p utmReleaseProviderForTest) ApplyConfigDefaults(cfg *Config) error {
	return p.Provider.(ProviderConfigDefaulter).ApplyConfigDefaults(cfg)
}

type utmReleaseBackendForTest struct {
	SSHLeaseBackend
	afterResolve func() error
}

func (b *utmReleaseBackendForTest) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	lease, err := b.SSHLeaseBackend.Resolve(ctx, req)
	if err == nil && b.afterResolve != nil {
		err = b.afterResolve()
	}
	return lease, err
}

func (b *utmReleaseBackendForTest) ReleaseLeaseConnectionCleanupSafe() bool {
	policy, ok := b.SSHLeaseBackend.(ReleaseLeaseConnectionCleanupPolicy)
	return !ok || policy.ReleaseLeaseConnectionCleanupSafe()
}
