package fal

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return providerName }

func (Provider) Aliases() []string { return []string{"fal-ai"} }

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      providerName,
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return RegisterFalProviderFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return ApplyFalProviderFlags(cfg, fs, values)
}

func (Provider) PrepareLeaseClaimEndpoint(existing core.LeaseClaim, provider, slug string, server core.Server, _ bool) (core.Server, error) {
	if !isFalProviderName(provider) {
		return core.Server{}, core.Exit(2, "refusing to rewrite fal lease=%s as provider=%s", existing.LeaseID, provider)
	}
	if slug != existing.Slug || server.Labels["lease"] != existing.LeaseID || server.Labels["slug"] != existing.Slug {
		return core.Server{}, core.Exit(2, "refusing to rewrite fal lease=%s with mismatched claim identity", existing.LeaseID)
	}
	if existing.CloudID != "" && server.CloudID != "" && existing.CloudID != server.CloudID {
		return core.Server{}, core.Exit(2, "refusing to rewrite fal lease=%s with stale instance identity", existing.LeaseID)
	}
	binding := strings.TrimSpace(existing.Labels[falCredentialBindingLabel])
	if binding == "" {
		return core.Server{}, core.Exit(2, "fal lease %s has no credential binding; refusing endpoint rewrite", existing.LeaseID)
	}
	if supplied := strings.TrimSpace(server.Labels[falCredentialBindingLabel]); supplied != "" && supplied != binding {
		return core.Server{}, core.Exit(2, "refusing to rewrite fal lease=%s with a different credential binding", existing.LeaseID)
	}
	server.Labels = cloneLabels(server.Labels)
	server.Labels[falCredentialBindingLabel] = binding
	return server, nil
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, exit(2, "provider=%s managed provisioning supports target=linux only", providerName)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, exit(2, "--tailscale is not supported for provider=%s; fal Compute exposes public SSH only", providerName)
	}
	applyFalDefaults(&cfg)
	if err := validateFalSSHUser(cfg.Fal.User); err != nil {
		return nil, err
	}
	if err := validateFalSSHUser(cfg.SSHUser); err != nil {
		return nil, err
	}
	return &backend{spec: p.Spec(), cfg: cfg, rt: rt, clientFactory: newClient}, nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "fal doctor backend unavailable")
	}
	return doctor, nil
}

type backend struct {
	spec                  ProviderSpec
	cfg                   Config
	rt                    Runtime
	clientFactory         func(Config, Runtime) (computeAPI, error)
	persistRecoveredClaim func(core.LeaseClaim, Config, string) (core.LeaseClaim, error)
	waitSSH               func(context.Context, *core.SSHTarget, string, time.Duration) error
	pollInterval          time.Duration
	pollTimeout           time.Duration
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	if strings.TrimSpace(b.cfg.Fal.APIKey) == "" {
		return DoctorResult{}, exit(2, "provider=%s requires fal credentials in environment", providerName)
	}
	client, err := b.clientFactory(b.cfg, b.rt)
	if err != nil {
		return DoctorResult{}, err
	}
	count, fingerprint, err := falInventoryFingerprint(ctx, client)
	if err != nil {
		return DoctorResult{}, exit(1, "fal auth check failed: %v", err)
	}
	return DoctorResult{
		Provider: providerName,
		Message:  fmt.Sprintf("auth=ready control_plane=ready inventory=ready inventory_count=%d inventory_fingerprint=%s api=list mutation=false runtime=unchecked", count, fingerprint),
	}, nil
}

func falInventoryFingerprint(ctx context.Context, client computeAPI) (int, string, error) {
	const maxPages = 100
	ids := make([]string, 0)
	cursor := ""
	seenCursors := map[string]struct{}{}
	for page := 0; page < maxPages; page++ {
		result, err := client.ListInstances(ctx, 100, cursor)
		if err != nil {
			return 0, "", err
		}
		for _, instance := range result.Instances {
			id := strings.TrimSpace(instance.ID)
			if id == "" {
				return 0, "", exit(5, "fal inventory returned an instance without an id")
			}
			ids = append(ids, id)
		}
		if !result.HasMore {
			sort.Strings(ids)
			sum := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
			return len(ids), fmt.Sprintf("%x", sum), nil
		}
		if result.NextCursor == nil || strings.TrimSpace(*result.NextCursor) == "" {
			return 0, "", exit(5, "fal inventory pagination omitted the next cursor")
		}
		next := strings.TrimSpace(*result.NextCursor)
		if _, exists := seenCursors[next]; exists {
			return 0, "", exit(5, "fal inventory pagination repeated a cursor")
		}
		seenCursors[next] = struct{}{}
		cursor = next
	}
	return 0, "", exit(5, "fal inventory pagination exceeded %d pages", maxPages)
}

func newDiscardRuntime() Runtime {
	return Runtime{Stdout: io.Discard, Stderr: io.Discard}
}
