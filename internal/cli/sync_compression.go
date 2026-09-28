package cli

import "strings"

func effectiveSyncCompression(cfg Config) string {
	if mode := strings.TrimSpace(cfg.Sync.Compression); mode != "" {
		return mode
	}
	return "always"
}

func validateSyncCompression(cfg Config) error {
	switch effectiveSyncCompression(cfg) {
	case "always", "never":
		return nil
	default:
		return Exit(2, "sync.compression must be always or never")
	}
}

func syncCompressionEnabled(mode string) bool {
	return mode != "never"
}
