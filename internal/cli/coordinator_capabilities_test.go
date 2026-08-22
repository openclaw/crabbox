package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestValidateCoordinatorLeaseCapabilitiesRejectsOldCoordinatorForRequiredCache(t *testing.T) {
	err := validateCoordinatorLeaseCapabilities(
		Config{Cache: CacheConfig{Volumes: []CacheVolumeConfig{{
			Name: "build", Key: "repo-build", Path: "/var/cache/build", Required: true,
		}}}},
		CoordinatorLease{ID: "cbx_test"},
	)
	if err == nil || !strings.Contains(err.Error(), "cache volume protocol v1") {
		t.Fatalf("expected required cache protocol failure, got %v", err)
	}
}

func TestValidateCoordinatorLeaseCapabilitiesAllowsExplicitOptionalCacheIgnore(t *testing.T) {
	err := validateCoordinatorLeaseCapabilities(
		Config{Cache: CacheConfig{Volumes: []CacheVolumeConfig{{
			Name: "build", Key: "repo-build", Path: "/var/cache/build",
		}}}},
		CoordinatorLease{ID: "cbx_test"},
	)
	if err != nil {
		t.Fatalf("optional cache should allow an explicit ignored result: %v", err)
	}
}

func TestValidateCoordinatorLeaseCapabilitiesRequiresResolvedCacheBindings(t *testing.T) {
	cfg := Config{Cache: CacheConfig{Volumes: []CacheVolumeConfig{{
		Name: "build", Key: "repo-build", Path: "/var/cache/build", Required: true,
	}}}}
	if err := validateCoordinatorLeaseCapabilities(cfg, CoordinatorLease{
		ID:                  "cbx_test",
		CacheVolumeProtocol: AWSCacheVolumeProtocolVersion,
	}); err == nil || !strings.Contains(err.Error(), "incomplete cache volume bindings") {
		t.Fatalf("expected incomplete bindings failure, got %v", err)
	}
	if err := validateCoordinatorLeaseCapabilities(cfg, CoordinatorLease{
		ID:                  "cbx_test",
		CacheVolumeProtocol: AWSCacheVolumeProtocolVersion,
		CacheVolumeBindings: []AWSCacheVolumeBinding{{
			Name: "build", Path: "/var/cache/build", VolumeID: "vol-1", Generation: 1, ABI: "ext4-v1",
		}},
	}); err != nil {
		t.Fatalf("expected cache protocol echo to pass: %v", err)
	}
}

func TestCoordinatorFixedCacheLeaseUsesFailClosedRoute(t *testing.T) {
	const leaseID = "cbx_abcdef123456"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/leases/cache-volume-aware/"+leaseID {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["cacheVolumeProtocol"] != float64(AWSCacheVolumeProtocolVersion) {
			t.Fatalf("cacheVolumeProtocol=%v", request["cacheVolumeProtocol"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"` + leaseID + `","provider":"aws","state":"active","cacheVolumeProtocol":1,"cacheVolumeBindings":[{"name":"build","path":"/var/cache/build","volumeID":"vol-1","generation":1,"abi":"ext4-v1"}]}}`))
	}))
	defer server.Close()

	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	_, err := client.EnsureLease(context.Background(), Config{
		Provider: "aws",
		Cache: CacheConfig{Volumes: []CacheVolumeConfig{{
			Name: "build", Key: "repo-build", Path: "/var/cache/build", Required: true,
		}}},
	}, "ssh-ed25519 test", false, leaseID, "fixed-cache")
	if err != nil {
		t.Fatal(err)
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
