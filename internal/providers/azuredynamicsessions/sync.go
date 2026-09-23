package azuredynamicsessions

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func (b *azureDynamicSessionsBackend) workspace(client azureDynamicSessionsAPI, sessionID string, req core.RunRequest, workdir string) core.ArchiveWorkspace {
	workspace := core.NewArchiveWorkspace(b.cfg, b.rt, req, providerName, workdir)
	workspace.TempPattern = "crabbox-azds-sync-*.tgz"
	workspace.RemoteArchivePrefix = "crabbox-azds-sync-"
	workspace.CleanWorkdir = cleanAzureDynamicSessionsWorkspacePath
	workspace.Upload = func(ctx context.Context, remoteArchive string, body io.Reader) error {
		archive, ok := body.(*os.File)
		if !ok {
			return fmt.Errorf("%s sync archive must be a local file", providerName)
		}
		return providerError("upload archive", client.UploadFile(ctx, sessionID, archive.Name(), remoteArchive))
	}
	workspace.Exec = func(ctx context.Context, command string) error {
		return b.execShell(ctx, client, sessionID, command, io.Discard)
	}
	return workspace
}

func (b *azureDynamicSessionsBackend) execShell(ctx context.Context, client azureDynamicSessionsAPI, sessionID, command string, stdout io.Writer) error {
	timeoutMS, err := azureDynamicSessionsTimeoutMilliseconds(b.cfg)
	if err != nil {
		return err
	}
	code, err := client.ExecStream(ctx, sessionID, shared.CommandStreamRequest{
		Command:   command,
		Cwd:       "/",
		TimeoutMS: timeoutMS,
	}, stdout, b.rt.Stderr)
	if err != nil {
		return fmt.Errorf("%s exec %q: %w", providerName, command, err)
	}
	if code != 0 {
		return core.Exit(code, "%s exec %q exited %d", providerName, command, code)
	}
	return nil
}

func createAzureDynamicSessionsSyncArchive(ctx context.Context, repo core.Repo, manifest core.SyncManifest) (*os.File, error) {
	return core.CreateSyncArchive(ctx, repo, manifest, "crabbox-azds-sync-*.tgz")
}

func azureDynamicSessionsWorkspace(cfg core.Config) (string, error) {
	return cleanAzureDynamicSessionsWorkspacePath(core.Blank(strings.TrimSpace(cfg.AzureDynamicSessions.Workdir), core.AzureDynamicSessionsConfigDefaultWorkdir))
}

func cleanAzureDynamicSessionsWorkspacePath(workspace string) (string, error) {
	return shared.CleanPOSIXWorkspacePath("azure-dynamic-sessions workspace path", workspace, "/mnt", "/mnt/data", "/workspace")
}
