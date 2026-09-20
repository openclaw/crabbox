package parallels

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestDesktopLeaseAllowanceAllowsOwnedParallelsMacOSManualDesktop(t *testing.T) {
	isolateLeaseClaimState(t)
	leaseID := "cbx_abcdef123456"
	server := core.Server{
		CloudID:  "vm-clone",
		Provider: "parallels",
		Name:     "crabbox-cbx-abcdef123456-live",
		Labels: map[string]string{
			"provider": "parallels",
			"lease":    leaseID,
			"target":   core.TargetMacOS,
			"host":     "local",
		},
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "live", "parallels", "", "", "/repo", time.Minute, false, server, core.SSHTarget{Port: "22"}); err != nil {
		t.Fatal(err)
	}
	allowed, err := (Provider{}).DesktopLeaseWithoutLabel(
		core.Config{Desktop: true, Provider: "parallels", TargetOS: core.TargetMacOS},
		server,
		leaseID,
	)
	if err != nil || !allowed {
		t.Fatalf("owned Parallels macOS clone without desktop label should allow Screen Sharing reuse: %v", err)
	}
}

func TestDesktopLeaseAllowanceRejectsParallelsSourceAndUnownedDesktop(t *testing.T) {
	isolateLeaseClaimState(t)
	leaseID := "cbx_abcdef123456"
	owned := core.Server{
		CloudID:  "vm-clone",
		Provider: "parallels",
		Name:     "crabbox-cbx-abcdef123456-live",
		Labels: map[string]string{
			"provider": "parallels",
			"lease":    leaseID,
			"target":   core.TargetMacOS,
			"host":     "local",
		},
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "live", "parallels", "", "", "/repo", time.Minute, false, owned, core.SSHTarget{Port: "22"}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		cfg    core.Config
		server core.Server
		id     string
	}{
		{
			name: "source-vm",
			cfg:  core.Config{Desktop: true, Provider: "parallels", TargetOS: core.TargetMacOS},
			server: core.Server{
				CloudID:  "source-id",
				Provider: "parallels",
				Name:     "source-vm",
				Labels: map[string]string{
					"provider": "parallels",
					"target":   core.TargetMacOS,
					"host":     "local",
				},
			},
			id: "source-vm",
		},
		{
			name: "claimed-source-vm",
			cfg:  core.Config{Desktop: true, Provider: "parallels", TargetOS: core.TargetMacOS},
			server: core.Server{
				CloudID:  "vm-clone",
				Provider: "parallels",
				Name:     "source-vm",
				Labels: map[string]string{
					"provider": "parallels",
					"lease":    leaseID,
					"target":   core.TargetMacOS,
					"host":     "local",
				},
			},
			id: leaseID,
		},
		{
			name: "unowned-clone",
			cfg:  core.Config{Desktop: true, Provider: "parallels", TargetOS: core.TargetMacOS},
			server: core.Server{
				CloudID:  "vm-other",
				Provider: "parallels",
				Name:     "crabbox-cbx-ffffffffffff-live",
				Labels: map[string]string{
					"provider": "parallels",
					"lease":    "cbx_ffffffffffff",
					"target":   core.TargetMacOS,
					"host":     "local",
				},
			},
			id: "cbx_ffffffffffff",
		},
		{
			name: "claim-bound-to-other-vm",
			cfg:  core.Config{Desktop: true, Provider: "parallels", TargetOS: core.TargetMacOS},
			server: core.Server{
				CloudID:  "vm-other",
				Provider: "parallels",
				Name:     "crabbox-cbx-abcdef123456-live",
				Labels: map[string]string{
					"provider": "parallels",
					"lease":    leaseID,
					"target":   core.TargetMacOS,
					"host":     "local",
				},
			},
			id: leaseID,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			allowed, err := (Provider{}).DesktopLeaseWithoutLabel(test.cfg, test.server, test.id)
			if err != nil || allowed {
				t.Fatalf("err=%v, want source/unowned desktop rejection", err)
			}
		})
	}
}

func TestDesktopLeaseAllowanceRequiresDesktopLabelForNonMacAndOtherProviders(t *testing.T) {
	isolateLeaseClaimState(t)
	leaseID := "cbx_abcdef123456"
	linuxClone := core.Server{
		CloudID:  "vm-linux",
		Provider: "parallels",
		Name:     "crabbox-cbx-abcdef123456-live",
		Labels: map[string]string{
			"provider": "parallels",
			"lease":    leaseID,
			"target":   core.TargetLinux,
			"host":     "local",
		},
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "live", "parallels", "", "", "/repo", time.Minute, false, linuxClone, core.SSHTarget{Port: "22"}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		cfg    core.Config
		server core.Server
		id     string
	}{
		{
			name:   "parallels-linux",
			cfg:    core.Config{Desktop: true, Provider: "parallels", TargetOS: core.TargetLinux},
			server: linuxClone,
			id:     leaseID,
		},
		{
			name: "tart-macos",
			cfg:  core.Config{Desktop: true, Provider: "tart", TargetOS: core.TargetMacOS},
			server: core.Server{
				Provider: "tart",
				Name:     "crabbox-cbx-abcdef123456-live",
				Labels:   map[string]string{"target": core.TargetMacOS},
			},
			id: leaseID,
		},
		{
			name: "local-container",
			cfg:  core.Config{Desktop: true, Provider: "local-container", TargetOS: core.TargetLinux},
			server: core.Server{
				Provider: "local-container",
				Labels:   map[string]string{"target": core.TargetLinux},
			},
			id: leaseID,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			allowed, err := (Provider{}).DesktopLeaseWithoutLabel(test.cfg, test.server, test.id)
			if err != nil || allowed {
				t.Fatalf("err=%v, want desktop label required", err)
			}
		})
	}
}

func isolateLeaseClaimState(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	if err := os.MkdirAll(filepath.Join(root, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopLeaseAllowanceRejectsChangedIdentity(t *testing.T) {
	isolateLeaseClaimState(t)
	const leaseID = "cbx_abcdef123456"
	server := core.Server{CloudID: "vm-clone", Provider: "parallels", Name: "crabbox-cbx-abcdef123456-live",
		Labels: map[string]string{"lease": leaseID, "target": core.TargetMacOS, "host": "local"}}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "live", "parallels", "", "", "/repo", time.Minute, false, server, core.SSHTarget{Port: "22"}); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"wrong host", "missing host", "missing cloud ID", "conflicting lease label", "missing lease ID"} {
		t.Run(mutation, func(t *testing.T) {
			candidate := server
			candidate.Labels = map[string]string{"lease": leaseID, "target": core.TargetMacOS, "host": "local"}
			id := leaseID
			switch mutation {
			case "wrong host":
				candidate.Labels["host"] = "different-host"
			case "missing host":
				delete(candidate.Labels, "host")
			case "missing cloud ID":
				candidate.CloudID = ""
			case "conflicting lease label":
				candidate.Labels["lease"] = "cbx_ffffffffffff"
			case "missing lease ID":
				id = ""
			}
			allowed, err := (Provider{}).DesktopLeaseWithoutLabel(core.Config{Provider: "parallels", TargetOS: core.TargetMacOS}, candidate, id)
			if err != nil || allowed {
				t.Fatalf("allowed=%t error=%v", allowed, err)
			}
		})
	}
}
