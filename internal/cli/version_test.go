package cli

import "testing"

func TestResolveVersionPrefersInjectedRelease(t *testing.T) {
	got := resolveVersion("v1.2.3", "v9.9.9")
	if got != "1.2.3" {
		t.Fatalf("version=%q want 1.2.3", got)
	}
}

func TestResolveVersionUsesBuildInfoForDevFallback(t *testing.T) {
	got := resolveVersion("0.13.0-dev", "v0.13.0")
	if got != "0.13.0" {
		t.Fatalf("version=%q want 0.13.0", got)
	}
}

func TestResolveVersionUsesBuildInfoPrereleaseForDevFallback(t *testing.T) {
	got := resolveVersion("0.13.0-dev", "v0.14.0-rc.1")
	if got != "0.14.0-rc.1" {
		t.Fatalf("version=%q want 0.14.0-rc.1", got)
	}
}

func TestResolveVersionIgnoresDevelBuildInfo(t *testing.T) {
	got := resolveVersion("0.13.0-dev", "(devel)")
	if got != "0.13.0-dev" {
		t.Fatalf("version=%q want 0.13.0-dev", got)
	}
}

func TestResolveVersionIgnoresPseudoBuildInfo(t *testing.T) {
	got := resolveVersion("0.13.0-dev", "v0.13.1-0.20260514070813-eb9404600773")
	if got != "0.13.0-dev" {
		t.Fatalf("version=%q want 0.13.0-dev", got)
	}
}

func TestResolveVersionIgnoresDirtyBuildInfo(t *testing.T) {
	got := resolveVersion("0.13.0-dev", "v0.13.1-0.20260514070813-eb9404600773+dirty")
	if got != "0.13.0-dev" {
		t.Fatalf("version=%q want 0.13.0-dev", got)
	}
}
