package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

type DelegatedArchivePreparationRequest struct {
	Config         Config
	Repo           Repo
	ForceSyncLarge bool
	TempPattern    string
	Stderr         io.Writer
	Now            func() time.Time
}

type PreparedArchive struct {
	File              *os.File
	Size              int64
	Manifest          SyncManifest
	ManifestDuration  time.Duration
	PreflightDuration time.Duration
	ArchiveDuration   time.Duration

	closeOnce sync.Once
	closeErr  error
}

// Close closes the prepared archive and removes its temporary file exactly once.
func (archive *PreparedArchive) Close() error {
	archive.closeOnce.Do(func() {
		name := archive.File.Name()
		archive.closeErr = errors.Join(archive.File.Close(), os.Remove(name))
	})
	return archive.closeErr
}

// PrepareDelegatedArchive builds and bounds a workspace archive before any
// provider resource exists. The returned archive owns its temporary file;
// Close removes it. Workspace changes after preparation are intentionally not
// reflected in the uploaded pre-acquisition snapshot.
func PrepareDelegatedArchive(ctx context.Context, req DelegatedArchivePreparationRequest) (*PreparedArchive, error) {
	now := req.Now
	if now == nil {
		now = time.Now
	}
	stderr := req.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	excludes, err := syncExcludes(req.Repo.Root, req.Config)
	if err != nil {
		return nil, err
	}
	manifestStart := now()
	manifest, err := syncManifestFilteredRules(req.Repo.Root, excludes, req.Config.Sync.Includes)
	if err != nil {
		return nil, exit(6, "build sync file list: %v", err)
	}
	manifestDuration := now().Sub(manifestStart)

	preflightStart := now()
	archiveManifest := manifest
	archiveManifest.Changed = nil
	archiveManifest.ChangedBytes = 0
	if err := checkSyncPreflight(archiveManifest, req.Config, req.ForceSyncLarge, stderr); err != nil {
		return nil, err
	}
	preflightDuration := now().Sub(preflightStart)

	archiveCtx := ctx
	cancel := func() {}
	if req.Config.Sync.Timeout > 0 {
		archiveCtx, cancel = context.WithTimeout(ctx, req.Config.Sync.Timeout)
	}
	defer cancel()

	archiveStart := now()
	file, err := CreateSyncArchive(archiveCtx, req.Repo, manifest, blank(req.TempPattern, "crabbox-delegated-sync-*.tgz"))
	if err != nil {
		return nil, err
	}
	archiveDuration := now().Sub(archiveStart)
	info, err := file.Stat()
	if err != nil {
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
		return nil, fmt.Errorf("stat sync archive: %w", err)
	}
	return &PreparedArchive{
		File:              file,
		Size:              info.Size(),
		Manifest:          manifest,
		ManifestDuration:  manifestDuration,
		PreflightDuration: preflightDuration,
		ArchiveDuration:   archiveDuration,
	}, nil
}

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

func RunDelegatedArchiveSync(ctx context.Context, req DelegatedArchiveSyncRequest, prepared ...*PreparedArchive) ([]TimingPhase, time.Duration, error) {
	var preparedArchive *PreparedArchive
	if len(prepared) > 0 {
		preparedArchive = prepared[0]
		if preparedArchive != nil {
			defer preparedArchive.Close()
		}
	}
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
	var manifestDuration, preflightDuration, archiveDuration time.Duration
	var archive *os.File
	syncCtx := ctx
	if preparedArchive == nil {
		excludes, err := syncExcludes(req.Repo.Root, req.Config)
		if err != nil {
			return nil, 0, err
		}
		manifestStart := now()
		manifest, err := syncManifestFilteredRules(req.Repo.Root, excludes, req.Config.Sync.Includes)
		if err != nil {
			return nil, 0, exit(6, "build sync file list: %v", err)
		}
		manifestDuration = now().Sub(manifestStart)

		preflightStart := now()
		archiveManifest := manifest
		archiveManifest.Changed = nil
		archiveManifest.ChangedBytes = 0
		if err := checkSyncPreflight(archiveManifest, req.Config, req.ForceSyncLarge, stderr); err != nil {
			return nil, 0, err
		}
		preflightDuration = now().Sub(preflightStart)

		// Match SSH sync semantics: local manifest planning is outside the
		// transfer timeout. The timeout bounds archiving and synchronization.
		if req.Config.Sync.Timeout > 0 {
			archiveCtx, cancel := context.WithTimeout(ctx, req.Config.Sync.Timeout)
			defer cancel()
			syncCtx = archiveCtx
		}

		archiveStart := now()
		archive, err = CreateSyncArchive(syncCtx, req.Repo, manifest, tempPattern)
		if err != nil {
			return nil, 0, err
		}
		defer func() {
			_ = archive.Close()
			_ = os.Remove(archive.Name())
		}()
		archiveDuration = now().Sub(archiveStart)
	} else {
		archive = preparedArchive.File
		manifestDuration = preparedArchive.ManifestDuration
		preflightDuration = preparedArchive.PreflightDuration
		archiveDuration = preparedArchive.ArchiveDuration
	}

	// A prepared archive has already consumed part of the original sync budget.
	if preparedArchive != nil && req.Config.Sync.Timeout > 0 {
		remaining := req.Config.Sync.Timeout - archiveDuration
		if remaining < 0 {
			remaining = 0
		}
		transferCtx, cancel := context.WithTimeout(ctx, remaining)
		defer cancel()
		syncCtx = transferCtx
	}

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
		command := "rm -f " + ShellQuote(remoteArchive) + " && crabbox_cleanup_status=0 || crabbox_cleanup_status=$?"
		if stagingDir != "" {
			command += "; rm -rf " + ShellQuote(stagingDir) + " || crabbox_cleanup_status=$?"
		}
		command += "; exit \"$crabbox_cleanup_status\""
		if err := req.Exec(cleanupCtx, command); err != nil {
			fmt.Fprintf(stderr, "warning: %s sync cleanup failed: %v\n", provider, err)
		}
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
	var err error
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
	if preparedArchive != nil {
		total += manifestDuration + preflightDuration + archiveDuration
	}
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
