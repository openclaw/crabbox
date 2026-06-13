package nvidiabrev

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type nvidiaBrevBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func NewNvidiaBrevBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	applyNvidiaBrevDefaults(&cfg)
	cfg.Provider = providerName
	return &nvidiaBrevBackend{spec: spec, cfg: cfg, rt: rt}
}

func (b *nvidiaBrevBackend) Spec() ProviderSpec { return b.spec }

func (b *nvidiaBrevBackend) Acquire(context.Context, AcquireRequest) (LeaseTarget, error) {
	return LeaseTarget{}, unsupportedLifecycle("acquire")
}

func (b *nvidiaBrevBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	return LeaseTarget{}, unsupportedLifecycle("resolve")
}

func (b *nvidiaBrevBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, unsupportedLifecycle("list")
}

func (b *nvidiaBrevBackend) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	return unsupportedLifecycle("release")
}

func (b *nvidiaBrevBackend) Touch(_ context.Context, req TouchRequest) (Server, error) {
	return req.Lease.Server, unsupportedLifecycle("touch")
}

func (b *nvidiaBrevBackend) Cleanup(context.Context, CleanupRequest) error {
	return unsupportedLifecycle("cleanup")
}

func (b *nvidiaBrevBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	if b.rt.Exec == nil {
		return DoctorResult{}, exit(2, "provider=%s doctor requires Runtime.Exec", providerName)
	}
	if _, err := b.runBrev(ctx, "--version"); err != nil {
		return DoctorResult{}, exit(2, "nvidia-brev CLI check failed: %v", err)
	}
	result, err := b.runBrev(ctx, "ls", "--json")
	if err != nil {
		return DoctorResult{}, exit(1, "nvidia-brev auth/list check failed: %v", err)
	}
	count, err := countBrevWorkspaces(result.Stdout)
	if err != nil {
		return DoctorResult{}, exit(1, "parse nvidia-brev ls JSON: %v", err)
	}
	return cliDoctorResult(providerName, count, "unchecked"), nil
}

func (b *nvidiaBrevBackend) runBrev(ctx context.Context, args ...string) (LocalCommandResult, error) {
	cfg := b.configForRun()
	return b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name: cfg.NvidiaBrev.CLI,
		Args: args,
	})
}

func (b *nvidiaBrevBackend) configForRun() Config {
	cfg := b.cfg
	applyNvidiaBrevDefaults(&cfg)
	return cfg
}

func applyNvidiaBrevDefaults(cfg *Config) {
	cfg.Provider = providerName
	if cfg.TargetOS == "" {
		cfg.TargetOS = targetLinux
	}
	if cfg.NvidiaBrev.CLI == "" {
		cfg.NvidiaBrev.CLI = "brev"
	}
	if cfg.NvidiaBrev.GPUName == "" {
		cfg.NvidiaBrev.GPUName = "A100"
	}
	if cfg.NvidiaBrev.Mode == "" {
		cfg.NvidiaBrev.Mode = "vm"
	}
	if cfg.NvidiaBrev.ReleaseAction == "" {
		cfg.NvidiaBrev.ReleaseAction = "delete"
	}
	if cfg.NvidiaBrev.Target == "" {
		cfg.NvidiaBrev.Target = "container"
	}
	if cfg.NvidiaBrev.WorkRoot == "" {
		cfg.NvidiaBrev.WorkRoot = "/tmp/crabbox"
	}
	if cfg.NvidiaBrev.User != "" {
		cfg.SSHUser = cfg.NvidiaBrev.User
	}
	if cfg.NvidiaBrev.WorkRoot != "" {
		cfg.WorkRoot = cfg.NvidiaBrev.WorkRoot
	}
	cfg.SSHPort = ""
	cfg.SSHFallbackPorts = nil
}

type brevWorkspaceList struct {
	Workspaces json.RawMessage `json:"workspaces"`
}

func countBrevWorkspaces(stdout string) (int, error) {
	raw := strings.TrimSpace(stdout)
	if raw == "" {
		return 0, nil
	}
	if strings.HasPrefix(raw, "[") {
		var asArray []json.RawMessage
		if err := json.Unmarshal([]byte(raw), &asArray); err != nil {
			return 0, err
		}
		return len(asArray), nil
	}
	var asObject brevWorkspaceList
	if err := json.Unmarshal([]byte(raw), &asObject); err != nil {
		return 0, err
	}
	if len(asObject.Workspaces) == 0 {
		return 0, fmt.Errorf("missing workspaces field")
	}
	if strings.EqualFold(strings.TrimSpace(string(asObject.Workspaces)), "null") {
		return 0, nil
	}
	var workspaces []json.RawMessage
	if err := json.Unmarshal(asObject.Workspaces, &workspaces); err != nil {
		return 0, err
	}
	return len(workspaces), nil
}

func unsupportedLifecycle(operation string) error {
	return exit(2, "provider=%s %s is not implemented yet; lifecycle support is owned by PLAN-02", providerName, operation)
}
