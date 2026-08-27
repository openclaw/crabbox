package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/openclaw/crabbox/internal/runner"
	"github.com/openclaw/crabbox/internal/runner/runnerfs"
)

func copyOverResolvedSSHArchive(ctx context.Context, session *sshTransportSession, target SSHTarget, src, dst string, followLink bool, stdout, stderr anyWriter) (err error) {
	defer func() {
		var invalid runnerfs.InvalidArchiveError
		var remote runner.RemoteError
		if errors.As(err, &invalid) || (errors.As(err, &remote) && remote.Code == "invalid") {
			err = exit(2, "%s", err)
		}
	}()
	sourceRemote, source := sandboxCopyPath(src)
	_, destination := sandboxCopyPath(dst)
	if strings.TrimSpace(source) == "" || strings.TrimSpace(destination) == "" {
		return exit(2, "copy source and destination paths must not be empty")
	}
	client, err := newResolvedRunnerClient(ctx, session, target, stderr)
	if err != nil {
		return err
	}
	if sourceRemote {
		return client.Download(ctx, source, destination)
	}
	return client.Upload(ctx, source, destination, followLink)
}
