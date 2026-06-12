package linode

import (
	"os"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	defaultRegion = "us-ord"
	defaultImage  = "linode/ubuntu24.04"
	defaultType   = "g6-standard-1"
	tokenEnv      = "LINODE_TOKEN"
)

func tokenFromEnv() string {
	return strings.TrimSpace(os.Getenv(tokenEnv))
}

func requireToken() (string, error) {
	token := tokenFromEnv()
	if token == "" {
		return "", core.Exit(3, "%s is required", tokenEnv)
	}
	return token, nil
}

func linodeRegionForConfig(cfg core.Config) string {
	if strings.TrimSpace(cfg.Linode.Region) != "" {
		return strings.TrimSpace(cfg.Linode.Region)
	}
	return defaultRegion
}

func linodeImageForConfig(cfg core.Config) string {
	if strings.TrimSpace(cfg.Linode.Image) != "" {
		return strings.TrimSpace(cfg.Linode.Image)
	}
	return defaultImage
}

func linodeServerTypeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
		return strings.TrimSpace(cfg.ServerType)
	}
	if strings.TrimSpace(cfg.Linode.Type) != "" {
		return strings.TrimSpace(cfg.Linode.Type)
	}
	return linodeServerTypeForClass(cfg.Class)
}

func linodeServerTypeForClass(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "standard", "fast", "large", "beast":
		return defaultType
	default:
		return defaultType
	}
}

func validateFoundationConfig(cfg core.Config) error {
	if strings.TrimSpace(linodeRegionForConfig(cfg)) == "" {
		return core.Exit(2, "linode region is required")
	}
	if core.OSImageWasExplicit(cfg) && strings.TrimSpace(cfg.Linode.Image) == "" {
		return core.Exit(2, "provider=linode does not support os %q; set linode.image or CRABBOX_LINODE_IMAGE to an explicit Linode image", cfg.OSImage)
	}
	if strings.TrimSpace(linodeImageForConfig(cfg)) == "" {
		return core.Exit(2, "linode image is required")
	}
	if strings.TrimSpace(linodeServerTypeForConfig(cfg)) == "" {
		return core.Exit(2, "linode type is required")
	}
	if _, err := parseLinodeFirewallID(cfg.Linode.FirewallID); err != nil {
		return err
	}
	return nil
}
