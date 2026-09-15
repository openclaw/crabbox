package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const jjSyncHardBytes = uint64(512 << 20)
const jjSyncMetadataBytes = 64 << 20

type preparedJJSync struct {
	Root          string
	Manifest      SyncManifest
	Excludes      SyncExcludeRules
	Identity      jjSourceIdentity
	Mode          string
	ContentDigest string
	live          jjSourceSnapshot
	recorded      jjRecordedSourceSnapshot
}

func (prepared *preparedJJSync) cleanup() error {
	err := errors.Join(prepared.live.cleanup(), prepared.recorded.cleanup())
	if err == nil {
		prepared.Root = ""
	}
	return err
}

func validateSyncRevision(cfg Config) error {
	if effectiveSyncSource(cfg) != "jj" && strings.TrimSpace(cfg.Sync.Revision) != "" {
		return Exit(2, "sync.revision requires sync.source=jj")
	}
	return nil
}

func validateJJSyncConfig(cfg Config) error {
	if cfg.Sync.GitOverlay || strings.TrimSpace(cfg.Sync.BaseRef) != "" {
		return Exit(2, "sync.source=jj cannot use sync.gitOverlay or sync.baseRef")
	}
	if cfg.TargetOS == targetWindows && cfg.WindowsMode == windowsModeNormal {
		return Exit(2, "native JJ sync requires managed-manifest SSH sync on POSIX/WSL; native-Windows archive replacement is not yet supported")
	}
	return nil
}

func jjSyncProcessLimits(cfg Config) jjSourceProcessLimits {
	timeout := cfg.Sync.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return jjSourceProcessLimits{ObjectBytes: jjSyncHardBytes, InputBytes: jjSyncHardBytes, ScratchBytes: jjSyncHardBytes, MetadataBytes: jjSyncMetadataBytes, Timeout: timeout}
}

func prepareJJSync(ctx context.Context, repo Repo, cfg Config, force bool, stderr io.Writer) (prepared preparedJJSync, err error) {
	limits := jjSyncProcessLimits(cfg)
	ctx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()
	// Retain ownership even if preparation returns a cleanup failure.
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.cleanup())
		}
	}()
	directory, err := os.Getwd()
	if err != nil {
		return prepared, err
	}
	process, err := newInstalledJJSourceProcess(ctx, directory, limits, nil)
	if err != nil {
		return prepared, err
	}
	if strings.TrimSpace(cfg.Sync.Revision) != "" {
		prepared.recorded, err = prepareJJRecordedSnapshot(ctx, process, cfg.Sync.Revision, cfg, jjRecordedExportLimits{EntryBytes: jjSyncHardBytes, OutputBytes: jjSyncHardBytes}, force, stderr)
		if err != nil {
			return prepared, err
		}
		prepared.Root, prepared.Manifest, prepared.Excludes = prepared.recorded.Root, prepared.recorded.Manifest, prepared.recorded.Excludes
		prepared.Identity, prepared.Mode, prepared.ContentDigest = prepared.recorded.Inventory.Identity, "recorded", prepared.recorded.ContentDigest
	} else {
		prepared.live, err = prepareJJSourceSnapshot(ctx, repo.Root, cfg, process.readLive, force, stderr)
		if err != nil {
			return prepared, err
		}
		prepared.Root, prepared.Manifest, prepared.Excludes = prepared.live.Root, prepared.live.Selection.Manifest, prepared.live.Selection.Excludes
		prepared.Identity, prepared.Mode, prepared.ContentDigest = prepared.live.Read.Inventory.Identity, "live", prepared.live.ContentDigest
	}
	if !sameRepositoryPath(repo.Root, prepared.Identity.WorkspaceRoot) {
		return prepared, fmt.Errorf("native source workspace changed during preparation")
	}
	return prepared, nil
}
