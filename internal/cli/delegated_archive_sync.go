package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
)

type DelegatedArchiveSyncRequest struct {
	Config              Config
	Repo                Repo
	ForceSyncLarge      bool
	Workdir             string
	TempPattern         string
	RemoteArchiveDir    string
	RemoteArchivePrefix string
	PhaseName           string
	Provider            string
	Stderr              io.Writer
	Now                 func() time.Time
	Suffix              func() string
	CleanupContext      func(context.Context) (context.Context, context.CancelFunc)
	Upload              func(context.Context, string, io.Reader) error
	Exec                func(context.Context, string) error
	Replace             func(context.Context, string, string) error
}

func RunDelegatedArchiveSync(ctx context.Context, req DelegatedArchiveSyncRequest) ([]TimingPhase, time.Duration, error) {
	if req.Upload == nil || req.Exec == nil {
		return nil, 0, fmt.Errorf("delegated archive sync requires upload and exec callbacks")
	}
	if strings.TrimSpace(req.Workdir) == "" {
		return nil, 0, fmt.Errorf("delegated archive sync requires workdir")
	}
	now := req.Now
	if now == nil {
		now = time.Now
	}
	suffix := req.Suffix
	if suffix == nil {
		suffix = delegatedArchiveSyncSuffix
	}
	cleanupContext := req.CleanupContext
	if cleanupContext == nil {
		cleanupContext = func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
		}
	}
	tempPattern := blank(req.TempPattern, "crabbox-delegated-sync-*.tgz")
	remoteDir := blank(req.RemoteArchiveDir, "/tmp")
	remotePrefix := blank(req.RemoteArchivePrefix, "crabbox-sync-")
	phaseName := blank(req.PhaseName, "delegated_archive_sync")
	provider := blank(req.Provider, "delegated")
	stderr := req.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	start := now()
	syncCtx := ctx
	cancel := func() {}
	if req.Config.Sync.Timeout > 0 {
		syncCtx, cancel = context.WithTimeout(ctx, req.Config.Sync.Timeout)
	}
	defer cancel()

	excludes, err := syncExcludes(req.Repo.Root, req.Config)
	if err != nil {
		return nil, 0, err
	}
	manifestStart := now()
	manifest, err := syncManifestFiltered(req.Repo.Root, excludes, req.Config.Sync.Includes)
	if err != nil {
		return nil, 0, exit(6, "build sync file list: %v", err)
	}
	manifestDuration := now().Sub(manifestStart)

	preflightStart := now()
	archiveManifest := manifest
	archiveManifest.Changed = nil
	archiveManifest.ChangedBytes = 0
	if err := checkSyncPreflight(archiveManifest, req.Config, req.ForceSyncLarge, stderr); err != nil {
		return nil, 0, err
	}
	preflightDuration := now().Sub(preflightStart)

	archiveStart := now()
	archive, err := CreateSyncArchive(syncCtx, req.Repo, manifest, tempPattern)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()
	archiveDuration := now().Sub(archiveStart)

	remoteArchive := path.Join(remoteDir, remotePrefix+suffix()+".tgz")
	extractDir := req.Workdir
	stagingDir := ""
	if req.Config.Sync.Delete {
		stagingDir = path.Join(path.Dir(req.Workdir), "."+path.Base(req.Workdir)+".crabbox-sync-"+suffix())
		extractDir = stagingDir
	}
	cleanupRemote := func() {
		cleanupCtx, cleanupCancel := cleanupContext(ctx)
		defer cleanupCancel()
		command := "rm -f " + ShellQuote(remoteArchive) + " 2>/dev/null || true"
		if stagingDir != "" {
			command += "; rm -rf " + ShellQuote(stagingDir) + " 2>/dev/null || true"
		}
		_ = req.Exec(cleanupCtx, command)
	}
	cleanupPending := true
	defer func() {
		if cleanupPending {
			cleanupRemote()
		}
	}()

	uploadStart := now()
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return nil, 0, exit(6, "rewind sync archive: %v", err)
	}
	if err := req.Upload(syncCtx, remoteArchive, archive); err != nil {
		return nil, 0, err
	}
	uploadDuration := now().Sub(uploadStart)

	prepareStart := now()
	if stagingDir == "" {
		err = req.Exec(syncCtx, "mkdir -p "+ShellQuote(req.Workdir))
	} else {
		err = req.Exec(syncCtx, "rm -rf "+ShellQuote(stagingDir)+" && mkdir -p "+ShellQuote(stagingDir))
	}
	if err != nil {
		return nil, 0, err
	}
	prepareDuration := now().Sub(prepareStart)

	extractStart := now()
	if err := req.Exec(syncCtx, "tar -xzf "+ShellQuote(remoteArchive)+" -C "+ShellQuote(extractDir)); err != nil {
		return nil, 0, err
	}
	extractDuration := now().Sub(extractStart)

	replaceDuration := time.Duration(0)
	if stagingDir != "" {
		replaceStart := now()
		replace := req.Replace
		if replace == nil {
			replace = func(ctx context.Context, stagingDir, workdir string) error {
				return replaceDelegatedArchiveWorkspace(ctx, req.Exec, stagingDir, workdir, provider, stderr)
			}
		}
		if err := replace(syncCtx, stagingDir, req.Workdir); err != nil {
			return nil, 0, err
		}
		replaceDuration = now().Sub(replaceStart)
	}

	cleanupStart := now()
	cleanupRemote()
	cleanupPending = false
	cleanupDuration := now().Sub(cleanupStart)

	total := now().Sub(start)
	phases := []TimingPhase{
		{Name: "manifest", Ms: manifestDuration.Milliseconds()},
		{Name: "preflight", Ms: preflightDuration.Milliseconds()},
		{Name: "archive", Ms: archiveDuration.Milliseconds()},
		{Name: "upload", Ms: uploadDuration.Milliseconds()},
		{Name: "prepare", Ms: prepareDuration.Milliseconds()},
		{Name: "extract", Ms: extractDuration.Milliseconds()},
	}
	if stagingDir != "" {
		phases = append(phases, TimingPhase{Name: "replace", Ms: replaceDuration.Milliseconds()})
	}
	phases = append(phases, TimingPhase{Name: "cleanup", Ms: cleanupDuration.Milliseconds()})
	phases = append(phases, TimingPhase{Name: phaseName, Ms: total.Milliseconds()})
	return phases, total, nil
}

func replaceDelegatedArchiveWorkspace(ctx context.Context, exec func(context.Context, string) error, stagingDir, workdir, provider string, stderr io.Writer) error {
	backupDir := stagingDir + ".previous"
	command := "rm -rf " + ShellQuote(backupDir) +
		" && if [ -e " + ShellQuote(workdir) + " ]; then mv " + ShellQuote(workdir) + " " + ShellQuote(backupDir) + "; fi" +
		" && if mv " + ShellQuote(stagingDir) + " " + ShellQuote(workdir) +
		"; then exit 0" +
		"; else rc=$?; if [ -e " + ShellQuote(backupDir) + " ]; then mv " + ShellQuote(backupDir) + " " + ShellQuote(workdir) +
		"; fi; exit \"$rc\"; fi"
	if err := exec(ctx, command); err != nil {
		return err
	}
	if err := exec(ctx, "rm -rf "+ShellQuote(backupDir)); err != nil && stderr != nil {
		fmt.Fprintf(stderr, "warning: %s previous workspace cleanup failed path=%s: %v\n", provider, backupDir, err)
	}
	return nil
}

func delegatedArchiveSyncSuffix() string {
	var data [3]byte
	if _, err := rand.Read(data[:]); err == nil {
		return hex.EncodeToString(data[:])
	}
	return fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
}
