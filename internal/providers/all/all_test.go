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
		"apple-container",
		"apple-machine",
		"apple-vz",
		"ascii-box",
		"aws",
		"azure",
		"azure-dynamic-sessions",
		"blacksmith-testbox",
		"cloudflare",
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
		"opencomputer",
		"opensandbox",
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
