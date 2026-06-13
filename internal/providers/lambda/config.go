package lambda

import (
	"os"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName       = "lambda"
	defaultAPIBaseURL  = "https://cloud.lambda.ai/api/v1"
	defaultRegion      = "us-west-1"
	defaultType        = "gpu_1x_a10"
	defaultImageFamily = "lambda-stack-24-04"
	defaultUser        = "ubuntu"
	defaultPort        = "22"
	tokenEnv           = "LAMBDA_API_KEY"
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

func regionForConfig(cfg core.Config) string {
	return firstNonBlank(cfg.Lambda.Region, defaultRegion)
}

func typeForConfig(cfg core.Config) string {
	if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
		return strings.TrimSpace(cfg.ServerType)
	}
	return firstNonBlank(cfg.Lambda.Type, defaultType)
}

func imageFamilyForConfig(cfg core.Config) string {
	if strings.TrimSpace(cfg.Lambda.Image) != "" {
		return ""
	}
	return firstNonBlank(cfg.Lambda.ImageFamily, defaultImageFamily)
}

func imageForConfig(cfg core.Config) string {
	return strings.TrimSpace(cfg.Lambda.Image)
}

func serverTypeForClass(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "standard", "fast", "large", "beast":
		return defaultType
	default:
		return defaultType
	}
}

func validateConfig(cfg core.Config) error {
	if strings.TrimSpace(regionForConfig(cfg)) == "" {
		return core.Exit(2, "lambda region is required")
	}
	if strings.TrimSpace(typeForConfig(cfg)) == "" {
		return core.Exit(2, "lambda type is required")
	}
	if core.OSImageWasExplicit(cfg) && strings.TrimSpace(cfg.Lambda.Image) == "" && strings.TrimSpace(cfg.Lambda.ImageFamily) == "" {
		return core.Exit(2, "provider=lambda does not support os %q; set lambda.image, lambda.imageFamily, CRABBOX_LAMBDA_IMAGE, or CRABBOX_LAMBDA_IMAGE_FAMILY", cfg.OSImage)
	}
	if strings.TrimSpace(cfg.Lambda.Image) != "" && strings.TrimSpace(cfg.Lambda.ImageFamily) != "" {
		return core.Exit(2, "lambda image and imageFamily are mutually exclusive")
	}
	return nil
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
