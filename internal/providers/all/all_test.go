package all

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestAllBuiltInProvidersExposeDoctor(t *testing.T) {
	providers := []string{
		"aws",
		"azure",
		"blacksmith-testbox",
		"cloudflare",
		"daytona",
		"e2b",
		"exe-dev",
		"gcp",
		"hetzner",
		"islo",
		"modal",
		"namespace-devbox",
		"proxmox",
		"semaphore",
		"sprites",
		"ssh",
		"tensorlake",
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
