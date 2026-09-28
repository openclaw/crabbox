package cli

import (
	"net"
	"strings"
)

func effectiveSyncCompression(cfg Config) string {
	if mode := strings.TrimSpace(cfg.Sync.Compression); mode != "" {
		return mode
	}
	return "auto"
}

func validateSyncCompression(cfg Config) error {
	switch effectiveSyncCompression(cfg) {
	case "auto", "always", "never":
		return nil
	default:
		return Exit(2, "sync.compression must be auto, always, or never")
	}
}

func syncCompressionEnabled(mode string, target SSHTarget) bool {
	switch mode {
	case "never":
		return false
	case "auto":
		// A proxy or SSH-config alias can carry a loopback endpoint over a WAN.
		if target.ProxyCommand != "" || target.SSHConfigProxy || target.SSHConfigFile != "" || len(target.SSHConfigData) != 0 || target.AuthSecret {
			return true
		}
		return target.Host != "localhost" && !net.ParseIP(target.Host).IsLoopback()
	default:
		// Empty retains the established compression policy for non-sync callers.
		return true
	}
}
