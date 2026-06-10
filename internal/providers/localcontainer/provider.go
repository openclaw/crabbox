package localcontainer

import (
	"flag"
	"path/filepath"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

func (Provider) Name() string { return providerName }

func (Provider) Aliases() []string {
	return []string{"docker", "container", "local-docker"}
}

func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:        providerName,
		Family:      "container",
		Kind:        core.ProviderKindSSHLease,
		Targets:     []core.TargetSpec{{OS: core.TargetLinux}},
		Features:    core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureDesktop, core.FeatureBrowser, core.FeatureCacheVolume, core.FeatureCheckpoint},
		Coordinator: core.CoordinatorNever,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return registerFlags(fs, defaults)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	return applyFlags(cfg, fs, values)
}

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, core.Exit(2, "provider=%s supports target=linux only", providerName)
	}
	if cfg.Tailscale.Enabled || string(cfg.Network) == "tailscale" {
		return nil, core.Exit(2, "--tailscale is not supported for provider=%s; use a remote SSH provider when tailnet reachability is required", providerName)
	}
	return newBackend(p.Spec(), cfg, rt), nil
}

func (Provider) NativeCheckpointCapability(req core.NativeCheckpointRequest) (core.NativeCheckpointCapability, bool) {
	if req.Server.CloudID == "" {
		return core.NativeCheckpointCapability{}, false
	}
	if leaseUsesDockerSocket(req.Server, req.Config.LocalContainer.DockerSocket) {
		return core.NativeCheckpointCapability{}, false
	}
	if !isDockerRuntime(leaseRuntime(req.Server, req.Config.LocalContainer.Runtime)) {
		return core.NativeCheckpointCapability{}, false
	}
	// Docker commit is selected by native/auto mode; explicit VM snapshot
	// strategies must not silently change meaning.
	if req.StrategyExplicit {
		return core.NativeCheckpointCapability{}, false
	}
	return core.NativeCheckpointCapability{Kind: core.CheckpointKindDockerCommit, Direct: true}, true
}

// leaseHasDockerSocket reports whether a resolved lease was created with
// docker-socket mode (recorded on its labels). docker-commit checkpoints are
// skipped for those leases because the host work-root mount masks the committed
// workspace; the config flag alone misses leases whose mode is on the labels.
func leaseUsesDockerSocket(server core.Server, fallback bool) bool {
	value, ok := server.Labels["docker_socket"]
	if !ok {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func leaseRuntime(server core.Server, fallback string) string {
	if runtime := strings.TrimSpace(server.Labels["runtime"]); runtime != "" {
		return runtime
	}
	if runtime := strings.TrimSpace(fallback); runtime != "" {
		return runtime
	}
	return "docker"
}

func isDockerRuntime(runtime string) bool {
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.TrimSpace(runtime))), ".exe")
	return name == "docker"
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	backend, err := p.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "%s doctor backend unavailable", providerName)
	}
	return doctor, nil
}
