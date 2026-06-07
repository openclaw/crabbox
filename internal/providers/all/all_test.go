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

func TestAllBuiltInProvidersExposeDoctor(t *testing.T) {
	providers := []string{
		"apple-container",
		"aws",
		"azure",
		"azure-dynamic-sessions",
		"blacksmith-testbox",
		"cloudflare",
		"daytona",
		"e2b",
		"exe-dev",
		"external",
		"gcp",
		"hetzner",
		"islo",
		"kubevirt",
		"local-container",
		"modal",
		"multipass",
		"namespace-devbox",
		"proxmox",
		"railway",
		"runpod",
		"semaphore",
		"sprites",
		"ssh",
		"tensorlake",
		"upstash-box",
		"ascii-box",
		"wandb",
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
