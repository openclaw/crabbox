package phala

import (
	"context"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) create(ctx context.Context, cfg core.Config, publicKeyPath string, labels map[string]string) (string, error) {
	compose, err := composeFileForDeploy(cfg, publicKeyPath)
	if err != nil {
		return "", err
	}
	return b.createWithCompose(ctx, cfg, publicKeyPath, labels, compose)
}
