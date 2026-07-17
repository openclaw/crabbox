package cli

import (
	"strings"
	"testing"
)

func TestParseCacheStats(t *testing.T) {
	entries := parseCacheStats("pnpm\t/var/cache/crabbox/pnpm\t1024\ndocker\t\tImages=1GB,Build Cache=0B\n")
	if len(entries) != 2 {
		t.Fatalf("entries=%#v", entries)
	}
	if entries[0].Kind != "pnpm" || entries[0].Bytes != 1024 {
		t.Fatalf("pnpm entry=%#v", entries[0])
	}
	if entries[1].Kind != "docker" || entries[1].Note == "" {
		t.Fatalf("docker entry=%#v", entries[1])
	}
}

func TestRemoteCacheStatsHonorsEnabledKinds(t *testing.T) {
	got := remoteCacheStats(map[string]bool{"pnpm": true, "npm": false, "docker": false, "git": true})
	if !strings.Contains(got, "pnpm:/var/cache/crabbox/pnpm") || !strings.Contains(got, "git:/var/cache/crabbox/git") {
		t.Fatalf("enabled cache kinds missing: %q", got)
	}
	for _, notWant := range []string{"npm:/var/cache/crabbox/npm", "docker system df"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("disabled cache kind appeared in stats command: %q", got)
		}
	}
}

func TestRemoteCachePurgeHonorsEnabledKinds(t *testing.T) {
	enabled := map[string]bool{"pnpm": true, "npm": false, "docker": false, "git": true}
	got := remoteCachePurge("all", enabled)
	if !strings.Contains(got, "/var/cache/crabbox/pnpm") || !strings.Contains(got, "/var/cache/crabbox/git") {
		t.Fatalf("enabled cache purge missing: %q", got)
	}
	for _, notWant := range []string{"/var/cache/crabbox/npm", "docker system prune"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("disabled cache kind appeared in purge command: %q", got)
		}
	}
	if got := remoteCachePurge("npm", enabled); got != "false" {
		t.Fatalf("disabled specific purge=%q", got)
	}
}

func TestRemoteCacheWarmCommandSourcesHydrationEnvFile(t *testing.T) {
	got := remoteCacheWarmCommand("/home/runner/work/repo/repo", map[string]string{"CI": "1"}, "/home/runner/.crabbox/actions/cbx.env.sh", []string{"pnpm", "install"})
	for _, want := range []string{
		"cd '/home/runner/work/repo/repo'",
		". '/home/runner/.crabbox/actions/cbx.env.sh'",
		"CI='1'",
		"'pnpm' 'install'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cache warm command missing %q in %q", want, got)
		}
	}
}

func TestAllowedRemoteEnvExcludesExternalDesktopPasswordForAnyTarget(t *testing.T) {
	t.Setenv("SCREEN_SHARING_PASSWORD", "operator-secret")
	t.Setenv("SCREEN_SAFE_VALUE", "preserved")
	for _, targetOS := range []string{targetLinux, targetMacOS, targetWindows} {
		t.Run(targetOS, func(t *testing.T) {
			cfg := Config{Provider: "external", TargetOS: targetOS, EnvAllow: []string{"SCREEN_*"}}
			cfg.External.Connection.Desktop.PasswordEnv = "SCREEN_SHARING_PASSWORD"
			env := allowedRemoteEnv(cfg)
			if _, found := env["SCREEN_SHARING_PASSWORD"]; found {
				t.Fatalf("remote environment retained desktop password: %#v", env)
			}
			if env["SCREEN_SAFE_VALUE"] != "preserved" {
				t.Fatalf("unrelated remote environment lost: %#v", env)
			}
			command := remoteCacheWarmCommand("/work/repo", env, "", []string{"true"})
			if strings.Contains(command, "operator-secret") || strings.Contains(command, "SCREEN_SHARING_PASSWORD") {
				t.Fatalf("cache command exposed desktop password: %s", command)
			}
		})
	}
}

func TestParseCacheVolumeSpecs(t *testing.T) {
	volumes, err := ParseCacheVolumeSpecs([]string{
		"pnpm=repo-linux-node24-lock:/var/cache/crabbox/pnpm",
		"npm-cache:/var/cache/crabbox/npm",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 2 {
		t.Fatalf("volumes=%#v", volumes)
	}
	if volumes[0].Name != "pnpm" || volumes[0].Key != "repo-linux-node24-lock" || volumes[0].Path != "/var/cache/crabbox/pnpm" {
		t.Fatalf("first volume=%#v", volumes[0])
	}
	if volumes[1].Name != "npm-cache" || volumes[1].Key != "npm-cache" || volumes[1].Path != "/var/cache/crabbox/npm" {
		t.Fatalf("second volume=%#v", volumes[1])
	}
}

func TestParseCacheVolumeSpecRequiresAbsolutePath(t *testing.T) {
	_, err := ParseCacheVolumeSpec("pnpm:relative/cache")
	if err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("err=%v, want absolute path error", err)
	}
}
