package all

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestAppleContainerRegistersWithoutAliasCollision(t *testing.T) {
	for _, alias := range []string{"apple-container", "apple", "applecontainer"} {
		provider, err := core.ProviderFor(alias)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", alias, err)
		}
		if provider.Name() != "apple-container" {
			t.Fatalf("ProviderFor(%q).Name=%q want apple-container", alias, provider.Name())
		}
	}
	// The bare "container" alias must keep pointing at local-container.
	got, err := core.ProviderFor("container")
	if err != nil {
		t.Fatalf("ProviderFor(container): %v", err)
	}
	if got.Name() != "local-container" {
		t.Fatalf("'container' alias now resolves to %q; apple-container must not steal it", got.Name())
	}
}

func TestDockerSandboxRegistersWithoutAliasCollision(t *testing.T) {
	provider, err := core.ProviderFor("docker-sandbox")
	if err != nil {
		t.Fatalf("ProviderFor(docker-sandbox): %v", err)
	}
	if provider.Name() != "docker-sandbox" {
		t.Fatalf("ProviderFor(docker-sandbox).Name=%q", provider.Name())
	}
	for _, alias := range []string{"docker", "container", "local-docker"} {
		got, err := core.ProviderFor(alias)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", alias, err)
		}
		if got.Name() != "local-container" {
			t.Fatalf("%q alias now resolves to %q; docker-sandbox must not steal local-container aliases", alias, got.Name())
		}
	}
}

func TestOpenSandboxRegistersWithoutAliasCollision(t *testing.T) {
	provider, err := core.ProviderFor("opensandbox")
	if err != nil {
		t.Fatalf("ProviderFor(opensandbox): %v", err)
	}
	if provider.Name() != "opensandbox" {
		t.Fatalf("ProviderFor(opensandbox).Name=%q", provider.Name())
	}
	for _, alias := range []string{"osb", "open-sandbox"} {
		if got, err := core.ProviderFor(alias); err == nil && got.Name() == "opensandbox" {
			t.Fatalf("%q alias unexpectedly resolves to opensandbox; v1 should reserve aliases", alias)
		}
	}
}

func TestNvidiaBrevRegistersCanonicalAndAliases(t *testing.T) {
	for _, name := range []string{"nvidia-brev", "brev", "nvidia"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
		if provider.Name() != "nvidia-brev" {
			t.Fatalf("ProviderFor(%q).Name=%q want nvidia-brev", name, provider.Name())
		}
		if _, ok := provider.(core.DoctorProvider); !ok {
			t.Fatalf("ProviderFor(%q) does not expose doctor", name)
		}
	}
}

func TestAgentSandboxRegistersWithoutAliases(t *testing.T) {
	provider, err := core.ProviderFor("agent-sandbox")
	if err != nil {
		t.Fatalf("ProviderFor(agent-sandbox): %v", err)
	}
	if provider.Name() != "agent-sandbox" {
		t.Fatalf("ProviderFor(agent-sandbox).Name=%q", provider.Name())
	}
	spec := provider.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever || len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("agent-sandbox spec=%#v", spec)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("agent-sandbox features=%v", spec.Features)
	}
	for _, alias := range []string{"agentsandbox", "asb", "sandbox"} {
		if got, err := core.ProviderFor(alias); err == nil && got.Name() == "agent-sandbox" {
			t.Fatalf("%q alias unexpectedly resolves to agent-sandbox", alias)
		}
	}
}

func TestSuperserveRegistersWithoutAliases(t *testing.T) {
	provider, err := core.ProviderFor("superserve")
	if err != nil {
		t.Fatalf("ProviderFor(superserve): %v", err)
	}
	if provider.Name() != "superserve" {
		t.Fatalf("ProviderFor(superserve).Name=%q", provider.Name())
	}
	spec := provider.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever || len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("superserve spec=%#v", spec)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("superserve features=%v", spec.Features)
	}
	for _, alias := range []string{"ss", "sup", "super-serve"} {
		if got, err := core.ProviderFor(alias); err == nil && got.Name() == "superserve" {
			t.Fatalf("%q alias unexpectedly resolves to superserve", alias)
		}
	}
}

func TestScalewayRegistersWithoutAliases(t *testing.T) {
	provider, err := core.ProviderFor("scaleway")
	if err != nil {
		t.Fatalf("ProviderFor(scaleway): %v", err)
	}
	if provider.Name() != "scaleway" {
		t.Fatalf("ProviderFor(scaleway).Name=%q", provider.Name())
	}
	if provider.Aliases() != nil {
		t.Fatalf("scaleway aliases=%v, want none", provider.Aliases())
	}
	spec := provider.Spec()
	if spec.Kind != core.ProviderKindSSHLease || spec.Family != "scaleway" || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("scaleway spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("scaleway targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureTailscale} {
		if !spec.Features.Has(feature) {
			t.Fatalf("scaleway features=%v missing %s", spec.Features, feature)
		}
	}
	if _, ok := provider.(core.DoctorProvider); !ok {
		t.Fatal("scaleway does not expose doctor")
	}
}

func TestNamespaceInstanceRegistersWithoutAliasCollision(t *testing.T) {
	for _, name := range []string{"namespace-instance", "namespace-compute"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
		if provider.Name() != "namespace-instance" {
			t.Fatalf("ProviderFor(%q).Name=%q", name, provider.Name())
		}
	}
	provider, err := core.ProviderFor("namespace")
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != "namespace-devbox" {
		t.Fatalf("namespace alias resolves to %q; want namespace-devbox", provider.Name())
	}
}

func TestVercelSandboxRegistersWithoutAliases(t *testing.T) {
	provider, err := core.ProviderFor("vercel-sandbox")
	if err != nil {
		t.Fatalf("ProviderFor(vercel-sandbox): %v", err)
	}
	if provider.Name() != "vercel-sandbox" {
		t.Fatalf("ProviderFor(vercel-sandbox).Name=%q", provider.Name())
	}
	spec := provider.Spec()
	if spec.Family != "vercel" || spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("vercel-sandbox spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("vercel-sandbox targets=%#v", spec.Targets)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("vercel-sandbox features=%v", spec.Features)
	}
	for _, alias := range []string{"vercel", "vsb", "sandbox"} {
		if got, err := core.ProviderFor(alias); err == nil && got.Name() == "vercel-sandbox" {
			t.Fatalf("%q alias unexpectedly resolves to vercel-sandbox", alias)
		}
	}
}

func TestAnthropicSandboxRuntimeRegistersCanonicalAndAlias(t *testing.T) {
	for _, name := range []string{"anthropic-sandbox-runtime", "srt"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
		if provider.Name() != "anthropic-sandbox-runtime" {
			t.Fatalf("ProviderFor(%q).Name=%q", name, provider.Name())
		}
	}
	spec := mustProvider(t, "anthropic-sandbox-runtime").Spec()
	if spec.Family != "anthropic-sandbox-runtime" || spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever || len(spec.Features) != 0 {
		t.Fatalf("anthropic-sandbox-runtime spec=%#v", spec)
	}
}

func TestCloudflareDynamicWorkersRegistersCanonicalAndAliases(t *testing.T) {
	for _, name := range []string{"cloudflare-dynamic-workers", "cf-dynamic", "cfdw"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
		if provider.Name() != "cloudflare-dynamic-workers" {
			t.Fatalf("ProviderFor(%q).Name=%q", name, provider.Name())
		}
	}
	if provider := mustProvider(t, "cloudflare"); provider.Name() != "cloudflare" {
		t.Fatalf("cloudflare resolved to %q; dynamic workers must not replace Cloudflare Containers", provider.Name())
	}
	if provider := mustProvider(t, "cf"); provider.Name() != "cloudflare" {
		t.Fatalf("cf alias resolved to %q; dynamic workers must not steal it", provider.Name())
	}
	spec := mustProvider(t, "cloudflare-dynamic-workers").Spec()
	if spec.Family != "cloudflare" || spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("cloudflare-dynamic-workers spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetWorkerRuntime {
		t.Fatalf("cloudflare-dynamic-workers targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureCleanup, core.FeatureModuleRun, core.FeatureRunSession} {
		if !spec.Features.Has(feature) {
			t.Fatalf("cloudflare-dynamic-workers features=%v missing %s", spec.Features, feature)
		}
	}
}

func TestCodeSandboxRegistersCanonicalAndAliases(t *testing.T) {
	for _, name := range []string{"codesandbox", "csb", "code-sandbox"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
		if provider.Name() != "codesandbox" {
			t.Fatalf("ProviderFor(%q).Name=%q want codesandbox", name, provider.Name())
		}
		if _, ok := provider.(core.DoctorProvider); !ok {
			t.Fatalf("ProviderFor(%q) does not expose doctor", name)
		}
	}
	spec := mustProvider(t, "codesandbox").Spec()
	if spec.Family != "codesandbox" || spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("codesandbox spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("codesandbox targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureArchiveSync, core.FeatureCleanup, core.FeaturePauseResume} {
		if !spec.Features.Has(feature) {
			t.Fatalf("codesandbox features=%v missing %s", spec.Features, feature)
		}
	}
	if spec.Features.Has(core.FeatureURLBridge) {
		t.Fatalf("codesandbox features=%v must not advertise URL bridge without BridgeProvider support", spec.Features)
	}
}

func TestIncusRegistersAsBuiltInProvider(t *testing.T) {
	provider, err := core.ProviderFor("incus")
	if err != nil {
		t.Fatalf("ProviderFor(incus): %v", err)
	}
	if provider.Name() != "incus" {
		t.Fatalf("ProviderFor(incus).Name=%q", provider.Name())
	}
}

func TestAppleVZRegistersAsBuiltInProvider(t *testing.T) {
	for _, name := range []string{"apple-vz", "applevz"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
		if provider.Name() != "apple-vz" {
			t.Fatalf("ProviderFor(%q).Name=%q want apple-vz", name, provider.Name())
		}
	}
}

func TestAllBuiltInProvidersExposeDoctor(t *testing.T) {
	providers := []string{
		"agent-sandbox",
		"apple-container",
		"apple-machine",
		"apple-vz",
		"ascii-box",
		"aws",
		"azure",
		"azure-dynamic-sessions",
		"blacksmith-testbox",
		"cloudflare",
		"cloudflare-dynamic-workers",
		"codesandbox",
		"daytona",
		"docker-sandbox",
		"e2b",
		"exe-dev",
		"external",
		"freestyle",
		"gcp",
		"hetzner",
		"hostinger",
		"incus",
		"islo",
		"kubevirt",
		"local-container",
		"modal",
		"multipass",
		"mxc",
		"namespace-devbox",
		"namespace-instance",
		"nvidia-brev",
		"opencomputer",
		"opensandbox",
		"ovh",
		"parallels",
		"proxmox",
		"railway",
		"runpod",
		"anthropic-sandbox-runtime",
		"semaphore",
		"smolvm",
		"sprites",
		"ssh",
		"superserve",
		"tart",
		"tenki",
		"tensorlake",
		"upstash-box",
		"vercel-sandbox",
		"wandb",
		"windows-sandbox",
		"xcp-ng",
	}
	for _, name := range providers {
		t.Run(name, func(t *testing.T) {
			provider, err := core.ProviderFor(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := provider.(core.DoctorProvider); !ok {
				t.Fatalf("%s does not implement DoctorProvider", name)
			}
		})
	}
}

func mustProvider(t *testing.T, name string) core.Provider {
	t.Helper()
	provider, err := core.ProviderFor(name)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}
