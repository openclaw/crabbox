package codesandbox

import (
	"context"
	"io"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *codeSandboxBackend) syncWorkspace(ctx context.Context, api codeSandboxAPI, sandboxID string, req core.RunRequest, workdir string, prepared ...*core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
	syncReq := core.DelegatedArchiveSyncRequest{
		Config:              b.cfg,
		Repo:                req.Repo,
		ForceSyncLarge:      req.ForceSyncLarge,
		Workdir:             workdir,
		TempPattern:         "crabbox-codesandbox-sync-*.tgz",
		RemoteArchiveDir:    defaultWorkdir,
		RemoteArchivePrefix: ".crabbox-codesandbox-sync-",
		PhaseName:           "codesandbox_sync",
		Provider:            providerName,
		Stderr:              b.rt.Stderr,
		Now:                 func() time.Time { return core.ClockNow(b.rt.Clock) },
		CleanupContext:      b.cleanupContext,
		Upload: func(uploadCtx context.Context, remoteArchive string, body io.Reader) error {
			return api.UploadFile(uploadCtx, sandboxID, remoteArchive, body)
		},
		Exec: func(execCtx context.Context, command string) error {
			return b.execShell(execCtx, api, sandboxID, command)
		},
	}
	if workdir == defaultWorkdir {
		syncReq.Replace = func(replaceCtx context.Context, stagingDir, workdir string) error {
			return b.execShell(replaceCtx, api, sandboxID, codeSandboxMountReplaceCommand(stagingDir, workdir))
		}
	}
	return core.RunDelegatedArchiveSync(ctx, syncReq, prepared...)
}

func (b *codeSandboxBackend) execShell(ctx context.Context, api codeSandboxAPI, sandboxID, command string) error {
	res, err := api.RunCommand(ctx, sandboxID, CommandRequest{
		Command: []string{"bash", "-lc", command},
		Timeout: b.execTimeoutSecs(),
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(res.Stdout)
		}
		return core.Exit(res.ExitCode, "codesandbox exec %q exited %d: %s", command, res.ExitCode, detail)
	}
	return nil
}

func codeSandboxMountReplaceCommand(stagingDir, workdir string) string {
	backupDir := path.Join(workdir, path.Base(stagingDir)+".previous")
	workdirGlob := core.ShellQuote(workdir) + "/*"
	backupGlob := core.ShellQuote(backupDir) + "/*"
	rollback := "rollback() { original_rc=$?; trap - EXIT HUP INT TERM; rollback_rc=0; " +
		"for entry in " + workdirGlob + "; do " +
		"if [ \"$entry\" != " + core.ShellQuote(backupDir) + " ]; then rm -rf -- \"$entry\" || rollback_rc=$?; fi; done; " +
		"if [ \"$rollback_rc\" -eq 0 ]; then for entry in " + backupGlob + "; do " +
		"mv -- \"$entry\" " + core.ShellQuote(workdir+"/") + " || { rollback_rc=$?; break; }; done; fi; " +
		"if [ \"$rollback_rc\" -eq 0 ]; then rmdir " + core.ShellQuote(backupDir) + " || rollback_rc=$?; fi; " +
		"if [ \"$rollback_rc\" -ne 0 ]; then exit \"$rollback_rc\"; fi; exit \"$original_rc\"; }"
	return "shopt -s dotglob nullglob; " + rollback +
		"; mkdir -p " + core.ShellQuote(workdir) +
		" && rm -rf " + core.ShellQuote(backupDir) +
		" && mkdir -p " + core.ShellQuote(backupDir) +
		" && trap rollback EXIT HUP INT TERM" +
		" && for entry in " + workdirGlob + "; do " +
		"if [ \"$entry\" != " + core.ShellQuote(backupDir) + " ]; then mv -- \"$entry\" " + core.ShellQuote(backupDir+"/") + " || exit 1; fi; done" +
		" && cp -a " + core.ShellQuote(stagingDir+"/.") + " " + core.ShellQuote(workdir+"/") +
		" && trap - EXIT HUP INT TERM" +
		" && rm -rf " + core.ShellQuote(backupDir) + " " + core.ShellQuote(stagingDir)
}

func (b *codeSandboxBackend) ensureWorkspace(ctx context.Context, api codeSandboxAPI, sandboxID, workdir string) error {
	return b.execShell(ctx, api, sandboxID, "mkdir -p "+core.ShellQuote(workdir))
}
