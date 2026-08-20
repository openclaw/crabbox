package shared

import (
	"context"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type SandboxArchiveSyncRequest struct {
	Config              core.Config
	Repo                core.Repo
	ForceSyncLarge      bool
	Workdir             string
	TempPattern         string
	RemoteArchivePrefix string
	PhaseName           string
	Provider            string
	Stderr              io.Writer
	Now                 func() time.Time
	CleanupContext      func(context.Context) (context.Context, context.CancelFunc)
	Upload              func(context.Context, string, io.Reader) error
	Exec                func(context.Context, string) error
}

func RunSandboxArchiveSync(ctx context.Context, req SandboxArchiveSyncRequest, prepared ...*core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
	return core.RunDelegatedArchiveSync(ctx, core.DelegatedArchiveSyncRequest{
		Config:              req.Config,
		Repo:                req.Repo,
		ForceSyncLarge:      req.ForceSyncLarge,
		Workdir:             req.Workdir,
		TempPattern:         req.TempPattern,
		RemoteArchiveDir:    "/tmp",
		RemoteArchivePrefix: req.RemoteArchivePrefix,
		PhaseName:           req.PhaseName,
		Provider:            req.Provider,
		Stderr:              req.Stderr,
		Now:                 req.Now,
		CleanupContext:      req.CleanupContext,
		Upload:              req.Upload,
		Exec:                req.Exec,
	}, prepared...)
}
