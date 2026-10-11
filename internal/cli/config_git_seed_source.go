package cli

import "strings"

func effectiveGitSeedSource(cfg Config) string {
	if source := strings.TrimSpace(cfg.Sync.GitSeedSource); source != "" {
		return source
	}
	return "origin"
}

// Validate at the sync boundary, after all config and flag layers are applied.
func validateGitSeedSource(cfg Config) error {
	switch effectiveGitSeedSource(cfg) {
	case "origin":
		return nil
	case "local":
		if !cfg.Sync.GitSeed {
			return Exit(2, "sync.gitSeedSource=local requires sync.gitSeed=true")
		}
		if effectiveSyncSource(cfg) != "git" {
			return Exit(2, "sync.gitSeedSource=local requires sync.source=git")
		}
		if cfg.Sync.GitOverlay {
			return Exit(2, "sync.gitSeedSource=local cannot use sync.gitOverlay: both own repository metadata; set sync.gitOverlay=false for local seeding, or set sync.gitSeed=false and sync.gitSeedSource=origin for file-only sync")
		}
		return nil
	default:
		return Exit(2, "sync.gitSeedSource must be origin or local")
	}
}
