package scaleway

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	defaultRegion = "fr-par"
	defaultZone   = "fr-par-1"
	defaultImage  = "ubuntu_noble"
	defaultType   = "DEV1-S"
)

func regionForConfig(cfg core.Config) string {
	if value := strings.TrimSpace(cfg.Scaleway.Region); value != "" {
		return value
	}
	return defaultRegion
}

func zoneForConfig(cfg core.Config) string {
	if value := strings.TrimSpace(cfg.Scaleway.Zone); value != "" {
		return value
	}
	return defaultZone
}

func imageForConfig(cfg core.Config) string {
	if value := strings.TrimSpace(cfg.Scaleway.Image); value != "" {
		return value
	}
	return defaultImage
}

func serverTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
		return strings.TrimSpace(cfg.ServerType)
	}
	if value := strings.TrimSpace(cfg.Scaleway.Type); value != "" {
		return value
	}
	return scalewayServerTypeForClass(cfg.Class)
}

func validateFoundationConfig(cfg core.Config) error {
	if strings.TrimSpace(regionForConfig(cfg)) == "" {
		return core.Exit(2, "scaleway region is required")
	}
	if strings.TrimSpace(zoneForConfig(cfg)) == "" {
		return core.Exit(2, "scaleway zone is required")
	}
	if core.OSImageWasExplicit(cfg) && strings.TrimSpace(cfg.Scaleway.Image) == "" {
		return core.Exit(2, "provider=scaleway does not support os %q; set scaleway.image or CRABBOX_SCALEWAY_IMAGE to an explicit Scaleway image", cfg.OSImage)
	}
	if strings.TrimSpace(imageForConfig(cfg)) == "" {
		return core.Exit(2, "scaleway image is required")
	}
	if strings.TrimSpace(serverTypeForConfig(cfg)) == "" {
		return core.Exit(2, "scaleway type is required")
	}
	return nil
}
