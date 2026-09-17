package proxmox

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestServerTypeProjection(t *testing.T) {
	for _, name := range []string{"proxmox", " Proxmox "} {
		if got := core.ServerTypeForProviderClass(name, "beast"); got != "template" {
			t.Fatalf("provider=%q type=%q, want template", name, got)
		}
	}
	for _, tc := range []struct {
		id   int
		want string
	}{{0, "template"}, {-1, "template"}, {1, "template-1"}, {9000, "template-9000"}} {
		cfg := core.Config{Class: "beast", ServerType: "prior-type", ServerTypeExplicit: true, Proxmox: core.ProxmoxConfig{TemplateID: tc.id}}
		if got := (Provider{}).ServerTypeForConfig(cfg); got != tc.want {
			t.Fatalf("template ID=%d type=%q, want %q", tc.id, got, tc.want)
		}
	}
}
