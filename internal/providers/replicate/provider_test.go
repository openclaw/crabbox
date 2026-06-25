package replicate

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpec(t *testing.T) {
	p := Provider{}
	if p.Name() != "replicate" {
		t.Fatalf("Name=%q", p.Name())
	}
	if p.Aliases() != nil {
		t.Fatalf("Aliases=%v, want nil", p.Aliases())
	}
	spec := p.Spec()
	if spec.Name != "replicate" || spec.Family != "replicate" || spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) || !spec.Features.Has(core.FeatureRunSession) {
		t.Fatalf("features=%v", spec.Features)
	}
	if _, ok := any(replicateBackend{}).(core.DelegatedRunBackend); !ok {
		t.Fatal("replicate backend does not satisfy delegated run contract")
	}
}

func TestValidateConfigAllowsMissingRunnerTargetForExistingPredictionCommands(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  core.Config
		ok   bool
	}{
		{name: "deployment", cfg: core.Config{Provider: "replicate", Replicate: ReplicateConfig{Deployment: "owner/deploy"}}, ok: true},
		{name: "version", cfg: core.Config{Provider: "replicate", Replicate: ReplicateConfig{Version: "v1"}}, ok: true},
		{name: "missing", cfg: core.Config{Provider: "replicate"}, ok: true},
		{name: "both", cfg: core.Config{Provider: "replicate", Replicate: ReplicateConfig{Deployment: "owner/deploy", Version: "v1"}}, ok: false},
		{name: "other provider", cfg: core.Config{Provider: "modal"}, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateConfig(tc.cfg)
			if tc.ok && err != nil {
				t.Fatalf("ValidateConfig unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("ValidateConfig unexpectedly passed")
			}
		})
	}
}
