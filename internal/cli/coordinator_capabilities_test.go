package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateCoordinatorLeaseCapabilitiesRequiresDesktopEcho(t *testing.T) {
	err := validateCoordinatorLeaseCapabilities(Config{Desktop: true}, CoordinatorLease{ID: "cbx_test"})
	if err == nil {
		t.Fatal("expected desktop capability mismatch")
	}
}

func TestValidateCoordinatorLeaseCapabilitiesRequiresBrowserEcho(t *testing.T) {
	err := validateCoordinatorLeaseCapabilities(Config{Browser: true}, CoordinatorLease{ID: "cbx_test"})
	if err == nil {
		t.Fatal("expected browser capability mismatch")
	}
}

func TestValidateCoordinatorLeaseCapabilitiesRequiresRequestedDesktopEnvEcho(t *testing.T) {
	err := validateCoordinatorLeaseCapabilities(
		Config{Desktop: true, DesktopEnv: desktopEnvWayland},
		CoordinatorLease{ID: "cbx_test", Desktop: true, DesktopEnv: desktopEnvXFCE},
	)
	if err == nil {
		t.Fatal("expected desktopEnv capability mismatch")
	}
}

func TestValidateCoordinatorLeaseCapabilitiesAllowsDefaultDesktopEnvOmission(t *testing.T) {
	err := validateCoordinatorLeaseCapabilities(
		Config{Desktop: true, DesktopEnv: desktopEnvXFCE},
		CoordinatorLease{ID: "cbx_test", Desktop: true},
	)
	if err != nil {
		t.Fatalf("validateCoordinatorLeaseCapabilities error: %v", err)
	}
}

func TestValidateCoordinatorLeaseCapabilitiesRequiresCodeEcho(t *testing.T) {
	err := validateCoordinatorLeaseCapabilities(Config{Code: true}, CoordinatorLease{ID: "cbx_test"})
	if err == nil {
		t.Fatal("expected code capability mismatch")
	}
}

func TestValidateCoordinatorLeaseCapabilitiesAcceptsRequestedCapabilities(t *testing.T) {
	err := validateCoordinatorLeaseCapabilities(Config{Desktop: true, Browser: true, Code: true}, CoordinatorLease{
		ID:      "cbx_test",
		Desktop: true,
		Browser: true,
		Code:    true,
	})
	if err != nil {
		t.Fatalf("validateCoordinatorLeaseCapabilities error: %v", err)
	}
}

func TestEnforceManagedLeaseCapabilitiesRequiresRequestedDesktopEnvLabel(t *testing.T) {
	err := enforceManagedLeaseCapabilities(
		Config{Desktop: true, DesktopEnv: desktopEnvWayland},
		Server{Labels: map[string]string{"desktop": "true", "desktop_env": desktopEnvXFCE}},
		"cbx_test",
	)
	if err == nil {
		t.Fatal("expected desktopEnv label mismatch")
	}
}

func TestEnforceManagedLeaseCapabilitiesAcceptsRequestedDesktopEnvLabel(t *testing.T) {
	err := enforceManagedLeaseCapabilities(
		Config{Desktop: true, DesktopEnv: desktopEnvWayland},
		Server{Labels: map[string]string{"desktop": "true", "desktop_env": desktopEnvWayland}},
		"cbx_test",
	)
	if err != nil {
		t.Fatalf("enforceManagedLeaseCapabilities error: %v", err)
	}
}

func TestStaticDesktopProbeCommandRequiresWaylandEnvFile(t *testing.T) {
	got := staticDesktopProbeCommand(Config{DesktopEnv: desktopEnvWayland}, SSHTarget{TargetOS: targetLinux})
	for _, want := range []string{
		desktopEnvPath,
		`CRABBOX_DESKTOP_ENV:-}`,
		`XDG_RUNTIME_DIR`,
		`WAYLAND_DISPLAY`,
		`test -S "$XDG_RUNTIME_DIR/$WAYLAND_DISPLAY"`,
		`pgrep -x labwc`,
		`pgrep -x wayvnc`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("static wayland probe missing %q:\n%s", want, got)
		}
	}
}

func TestStaticDesktopProbeCommandRequiresGnomeWhenRequested(t *testing.T) {
	got := staticDesktopProbeCommand(Config{DesktopEnv: desktopEnvGnome}, SSHTarget{TargetOS: targetLinux})
	if !strings.Contains(got, `test "${CRABBOX_DESKTOP_ENV:-}" = "gnome"`) {
		t.Fatalf("static gnome probe should require gnome env:\n%s", got)
	}
	if !strings.Contains(got, `pgrep -x labwc >/dev/null`) {
		t.Fatalf("static gnome probe should require the managed labwc compositor:\n%s", got)
	}
	if strings.Contains(got, `case "${CRABBOX_DESKTOP_ENV:-}" in wayland|gnome)`) {
		t.Fatalf("static gnome probe should not accept plain wayland env:\n%s", got)
	}
}

func TestProbeDesktopEnvCommandIncludesXAuthority(t *testing.T) {
	got := probeDesktopEnvCommand()
	for _, want := range []string{
		desktopEnvPath,
		"DISPLAY",
		"XAUTHORITY",
		"XDG_RUNTIME_DIR",
		"WAYLAND_DISPLAY",
		"GDK_BACKEND",
		"MOZ_ENABLE_WAYLAND",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("desktop env probe missing %q:\n%s", want, got)
		}
	}
}

func TestStaticDesktopProbeCommandDefaultsToX11(t *testing.T) {
	got := staticDesktopProbeCommand(Config{}, SSHTarget{TargetOS: targetLinux})
	for _, want := range []string{"Xtigervnc :99", "Xvfb :99", "x11vnc"} {
		if !strings.Contains(got, want) {
			t.Fatalf("static x11 probe missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "pgrep -x labwc") {
		t.Fatalf("default static desktop probe should not accept unmanaged labwc:\n%s", got)
	}
}

func TestEnforceManagedLeaseCapabilitiesAllowsMacOSScreenSharing(t *testing.T) {
	err := enforceManagedLeaseCapabilities(
		Config{Desktop: true},
		Server{Labels: map[string]string{"target": targetMacOS}},
		"cbx_test",
	)
	if err != nil {
		t.Fatalf("enforceManagedLeaseCapabilities error: %v", err)
	}
}

func TestEnforceManagedLeaseCapabilitiesRequiresDesktopLabelForDirectMacOSProvider(t *testing.T) {
	err := enforceManagedLeaseCapabilities(
		Config{Desktop: true, Provider: "tart"},
		Server{Provider: "tart", Labels: map[string]string{"target": targetMacOS}},
		"cbx_test",
	)
	if err == nil {
		t.Fatal("direct macOS lease without desktop label should be rejected")
	}
}

func TestEnforceManagedLeaseCapabilitiesAllowsOwnedParallelsMacOSManualDesktop(t *testing.T) {
	isolateLeaseClaimState(t)
	leaseID := "cbx_abcdef123456"
	server := Server{
		CloudID:  "vm-clone",
		Provider: "parallels",
		Name:     "crabbox-cbx-abcdef123456-live",
		Labels: map[string]string{
			"provider": "parallels",
			"lease":    leaseID,
			"target":   targetMacOS,
			"host":     "local",
		},
	}
	if err := ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "live", "parallels", "", "", "/repo", time.Minute, false, server, SSHTarget{Port: "22"}); err != nil {
		t.Fatal(err)
	}
	err := enforceManagedLeaseCapabilities(
		Config{Desktop: true, Provider: "parallels", TargetOS: targetMacOS},
		server,
		leaseID,
	)
	if err != nil {
		t.Fatalf("owned Parallels macOS clone without desktop label should allow Screen Sharing reuse: %v", err)
	}
}

func TestEnforceManagedLeaseCapabilitiesRejectsParallelsSourceAndUnownedDesktop(t *testing.T) {
	isolateLeaseClaimState(t)
	leaseID := "cbx_abcdef123456"
	owned := Server{
		CloudID:  "vm-clone",
		Provider: "parallels",
		Name:     "crabbox-cbx-abcdef123456-live",
		Labels: map[string]string{
			"provider": "parallels",
			"lease":    leaseID,
			"target":   targetMacOS,
			"host":     "local",
		},
	}
	if err := ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "live", "parallels", "", "", "/repo", time.Minute, false, owned, SSHTarget{Port: "22"}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		cfg    Config
		server Server
		id     string
	}{
		{
			name: "source-vm",
			cfg:  Config{Desktop: true, Provider: "parallels", TargetOS: targetMacOS},
			server: Server{
				CloudID:  "source-id",
				Provider: "parallels",
				Name:     "source-vm",
				Labels: map[string]string{
					"provider": "parallels",
					"target":   targetMacOS,
					"host":     "local",
				},
			},
			id: "source-vm",
		},
		{
			name: "claimed-source-vm",
			cfg:  Config{Desktop: true, Provider: "parallels", TargetOS: targetMacOS},
			server: Server{
				CloudID:  "vm-clone",
				Provider: "parallels",
				Name:     "source-vm",
				Labels: map[string]string{
					"provider": "parallels",
					"lease":    leaseID,
					"target":   targetMacOS,
					"host":     "local",
				},
			},
			id: leaseID,
		},
		{
			name: "unowned-clone",
			cfg:  Config{Desktop: true, Provider: "parallels", TargetOS: targetMacOS},
			server: Server{
				CloudID:  "vm-other",
				Provider: "parallels",
				Name:     "crabbox-cbx-ffffffffffff-live",
				Labels: map[string]string{
					"provider": "parallels",
					"lease":    "cbx_ffffffffffff",
					"target":   targetMacOS,
					"host":     "local",
				},
			},
			id: "cbx_ffffffffffff",
		},
		{
			name: "claim-bound-to-other-vm",
			cfg:  Config{Desktop: true, Provider: "parallels", TargetOS: targetMacOS},
			server: Server{
				CloudID:  "vm-other",
				Provider: "parallels",
				Name:     "crabbox-cbx-abcdef123456-live",
				Labels: map[string]string{
					"provider": "parallels",
					"lease":    leaseID,
					"target":   targetMacOS,
					"host":     "local",
				},
			},
			id: leaseID,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := enforceManagedLeaseCapabilities(test.cfg, test.server, test.id)
			if err == nil || !strings.Contains(err.Error(), "was not created with desktop=true") {
				t.Fatalf("err=%v, want source/unowned desktop rejection", err)
			}
		})
	}
}

func TestEnforceManagedLeaseCapabilitiesRequiresDesktopLabelForNonMacAndOtherProviders(t *testing.T) {
	isolateLeaseClaimState(t)
	leaseID := "cbx_abcdef123456"
	linuxClone := Server{
		CloudID:  "vm-linux",
		Provider: "parallels",
		Name:     "crabbox-cbx-abcdef123456-live",
		Labels: map[string]string{
			"provider": "parallels",
			"lease":    leaseID,
			"target":   targetLinux,
			"host":     "local",
		},
	}
	if err := ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, "live", "parallels", "", "", "/repo", time.Minute, false, linuxClone, SSHTarget{Port: "22"}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		cfg    Config
		server Server
		id     string
	}{
		{
			name:   "parallels-linux",
			cfg:    Config{Desktop: true, Provider: "parallels", TargetOS: targetLinux},
			server: linuxClone,
			id:     leaseID,
		},
		{
			name: "tart-macos",
			cfg:  Config{Desktop: true, Provider: "tart", TargetOS: targetMacOS},
			server: Server{
				Provider: "tart",
				Name:     "crabbox-cbx-abcdef123456-live",
				Labels:   map[string]string{"target": targetMacOS},
			},
			id: leaseID,
		},
		{
			name: "local-container",
			cfg:  Config{Desktop: true, Provider: "local-container", TargetOS: targetLinux},
			server: Server{
				Provider: "local-container",
				Labels:   map[string]string{"target": targetLinux},
			},
			id: leaseID,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := enforceManagedLeaseCapabilities(test.cfg, test.server, test.id)
			if err == nil || !strings.Contains(err.Error(), "was not created with desktop=true") {
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
