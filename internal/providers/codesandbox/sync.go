package codesandbox

import (
	"context"
	"io"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *codeSandboxBackend) syncWorkspace(ctx context.Context, api codeSandboxAPI, sandboxID string, req RunRequest, workdir string) ([]timingPhase, time.Duration, error) {
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
		Now:                 b.now,
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
	return core.RunDelegatedArchiveSync(ctx, syncReq)
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
		return exit(res.ExitCode, "codesandbox exec %q exited %d: %s", command, res.ExitCode, detail)
	}
	return nil
}

func codeSandboxMountReplaceCommand(stagingDir, workdir string) string {
	backupDir := path.Join(workdir, path.Base(stagingDir)+".previous")
	workdirGlob := shellQuote(workdir) + "/*"
	backupGlob := shellQuote(backupDir) + "/*"
	rollback := "rollback() { original_rc=$?; trap - EXIT HUP INT TERM; rollback_rc=0; " +
		"for entry in " + workdirGlob + "; do " +
		"if [ \"$entry\" != " + shellQuote(backupDir) + " ]; then rm -rf -- \"$entry\" || rollback_rc=$?; fi; done; " +
		"if [ \"$rollback_rc\" -eq 0 ]; then for entry in " + backupGlob + "; do " +
		"mv -- \"$entry\" " + shellQuote(workdir+"/") + " || { rollback_rc=$?; break; }; done; fi; " +
		"if [ \"$rollback_rc\" -eq 0 ]; then rmdir " + shellQuote(backupDir) + " || rollback_rc=$?; fi; " +
		"if [ \"$rollback_rc\" -ne 0 ]; then exit \"$rollback_rc\"; fi; exit \"$original_rc\"; }"
	return "shopt -s dotglob nullglob; " + rollback +
		"; mkdir -p " + shellQuote(workdir) +
		" && rm -rf " + shellQuote(backupDir) +
		" && mkdir -p " + shellQuote(backupDir) +
		" && trap rollback EXIT HUP INT TERM" +
		" && for entry in " + workdirGlob + "; do " +
		"if [ \"$entry\" != " + shellQuote(backupDir) + " ]; then mv -- \"$entry\" " + shellQuote(backupDir+"/") + " || exit 1; fi; done" +
		" && cp -a " + shellQuote(stagingDir+"/.") + " " + shellQuote(workdir+"/") +
		" && trap - EXIT HUP INT TERM" +
		" && rm -rf " + shellQuote(backupDir) + " " + shellQuote(stagingDir)
}

func (b *codeSandboxBackend) ensureWorkspace(ctx context.Context, api codeSandboxAPI, sandboxID, workdir string) error {
	return b.execShell(ctx, api, sandboxID, "mkdir -p "+shellQuote(workdir))
}
