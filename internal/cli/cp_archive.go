package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/openclaw/crabbox/internal/runner"
)

func copyOverResolvedSSHArchive(ctx context.Context, target SSHTarget, src, dst string, followLink bool, stderr anyWriter) (err error) {
	if err := validateCopyArgs(src, dst); err != nil {
		return err
	}
	srcRemote, srcPath := sandboxCopyPath(src)
	_, dstPath := sandboxCopyPath(dst)
	if strings.TrimSpace(srcPath) == "" || strings.TrimSpace(dstPath) == "" {
		return Exit(2, "copy source and destination paths must not be empty")
	}
	scope, err := newFilesystemRuntimeScope(runner.DevelopmentSource())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, scope.finish(context.WithoutCancel(ctx))) }()
	client, err := scope.filesystemClient(ctx, target, stderr)
	if err != nil {
		return err
	}
	if srcRemote {
		return client.Download(ctx, srcPath, dstPath)
	}
	return client.Upload(ctx, srcPath, dstPath, followLink)
}
