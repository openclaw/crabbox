package azuredynamicsessions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/openclaw/crabbox/internal/providers/shared"
)

func (b *azureDynamicSessionsBackend) syncWorkspace(ctx context.Context, client azureDynamicSessionsAPI, sessionID string, req RunRequest, workspace string) ([]timingPhase, time.Duration, error) {
	workspace, err := cleanAzureDynamicSessionsWorkspacePath(workspace)
	if err != nil {
		return nil, 0, err
	}
	return shared.RunSandboxArchiveSync(ctx, shared.SandboxArchiveSyncRequest{
		Config:              b.cfg,
		Repo:                req.Repo,
		ForceSyncLarge:      req.ForceSyncLarge,
		Workdir:             workspace,
		TempPattern:         "crabbox-azds-sync-*.tgz",
		RemoteArchivePrefix: "crabbox-azds-sync-",
		PhaseName:           "azure_dynamic_sessions_sync",
		Provider:            providerName,
		Stderr:              b.rt.Stderr,
		Now:                 b.now,
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
	})
}

func (b *azureDynamicSessionsBackend) prepareWorkspace(ctx context.Context, client azureDynamicSessionsAPI, sessionID, workspace string) error {
	workspace, err := cleanAzureDynamicSessionsWorkspacePath(workspace)
	if err != nil {
		return err
	}
	return b.execShell(ctx, client, sessionID, "mkdir -p "+shellQuote(workspace), io.Discard)
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
		return exit(code, "%s exec %q exited %d", providerName, command, code)
	}
	return nil
}

func createAzureDynamicSessionsSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest) (*os.File, error) {
	return createPortableSyncArchive(ctx, repo, manifest, "crabbox-azds-sync-*.tgz")
}

func azureDynamicSessionsWorkspace(cfg Config) (string, error) {
	return cleanAzureDynamicSessionsWorkspacePath(blank(strings.TrimSpace(cfg.AzureDynamicSessions.Workdir), "/workspace/crabbox"))
}

func cleanAzureDynamicSessionsWorkspacePath(workspace string) (string, error) {
	trimmed := strings.TrimSpace(workspace)
	if trimmed == "" {
		return "", exit(2, "%s workspace path is empty", providerName)
	}
	clean := path.Clean(trimmed)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "%s workspace path %q must resolve to an absolute path", providerName, workspace)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/mnt", "/mnt/data", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace":
		return "", exit(2, "%s workspace path %q is too broad; choose a dedicated subdirectory", providerName, clean)
	}
	return clean, nil
}

func buildAzureDynamicSessionsCommand(command []string, shellMode bool) (string, error) {
	if len(command) == 0 {
		return "", errors.New("missing command")
	}
	if shellMode {
		return strings.Join(command, " "), nil
	}
	if len(command) == 1 && shouldUseShell(command) {
		return command[0], nil
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		return shellScriptFromArgv(command), nil
	}
	return strings.Join(shellWords(command), " "), nil
}
