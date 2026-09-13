package azuredynamicsessions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func (b *azureDynamicSessionsBackend) syncWorkspace(ctx context.Context, client azureDynamicSessionsAPI, sessionID string, req core.RunRequest, workspace string, prepared ...*core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
	workspace, err := cleanAzureDynamicSessionsWorkspacePath(workspace)
	if err != nil {
		return nil, 0, err
	}
	return core.RunDelegatedArchiveSync(ctx, core.DelegatedArchiveSyncRequest{
		Config:              b.cfg,
		Repo:                req.Repo,
		ForceSyncLarge:      req.ForceSyncLarge,
		Workdir:             workspace,
		TempPattern:         "crabbox-azds-sync-*.tgz",
		RemoteArchivePrefix: "crabbox-azds-sync-",
		PhaseName:           "azure_dynamic_sessions_sync",
		Provider:            providerName,
		Stderr:              b.rt.Stderr,
		Now:                 func() time.Time { return core.ClockNow(b.rt.Clock) },
		Upload: func(uploadCtx context.Context, remoteArchive string, body io.Reader) error {
			archive, ok := body.(*os.File)
			if !ok {
				return fmt.Errorf("%s sync archive must be a local file", providerName)
			}
			return providerError("upload archive", client.UploadFile(uploadCtx, sessionID, archive.Name(), remoteArchive))
		},
		Exec: func(execCtx context.Context, command string) error {
			return b.execShell(execCtx, client, sessionID, command, io.Discard)
		},
	}, prepared...)
}

func (b *azureDynamicSessionsBackend) prepareWorkspace(ctx context.Context, client azureDynamicSessionsAPI, sessionID, workspace string) error {
	workspace, err := cleanAzureDynamicSessionsWorkspacePath(workspace)
	if err != nil {
		return err
	}
	return b.execShell(ctx, client, sessionID, "mkdir -p "+core.ShellQuote(workspace), io.Discard)
}

func (b *azureDynamicSessionsBackend) execShell(ctx context.Context, client azureDynamicSessionsAPI, sessionID, command string, stdout io.Writer) error {
	code, err := client.ExecStream(ctx, sessionID, azureDynamicSessionsExecRequest{
		Command:   command,
		Cwd:       "/",
		TimeoutMS: durationMillisecondsCeil(azureDynamicSessionsTimeout(b.cfg)),
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

func buildAzureDynamicSessionsCommand(command []string, shellMode bool) (string, error) {
	if len(command) == 0 {
		return "", errors.New("missing command")
	}
	if shellMode {
		return strings.Join(command, " "), nil
	}
	if len(command) == 1 && core.ShouldUseShell(command) {
		return command[0], nil
	}
	if core.ShouldUseShell(command) || core.LeadingEnvAssignment(command) {
		return core.ShellScriptFromArgv(command), nil
	}
	return strings.Join(core.ShellWords(command), " "), nil
}
