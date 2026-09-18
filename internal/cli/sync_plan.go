package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type syncPlanRow struct {
	Path  string
	Bytes int64
}

type syncPlanJSONOutput struct {
	Source              string                    `json:"source,omitempty"`
	Root                string                    `json:"root,omitempty"`
	Candidate           syncPlanJSONSize          `json:"candidate"`
	DirtyDelta          syncPlanJSONSize          `json:"dirtyDelta"`
	DeletedTrackedPaths int                       `json:"deletedTrackedPaths"`
	ProtectedTracked    syncPlanJSONProtected     `json:"protectedTrackedFiles"`
	Guardrail           syncPlanJSONGuardrail     `json:"guardrail"`
	TopFiles            []syncPlanJSONRow         `json:"topFiles"`
	TopDirs             []syncPlanJSONRow         `json:"topDirs"`
	LocalGitSeed        *syncPlanJSONLocalGitSeed `json:"localGitSeed,omitempty"`
}

type syncPlanJSONLocalGitSeed struct {
	Source                       string `json:"source"`
	Head                         string `json:"head"`
	Base                         string `json:"base,omitempty"`
	BaseRef                      string `json:"baseRef,omitempty"`
	ObjectFormat                 string `json:"objectFormat"`
	Objects                      int    `json:"objects"`
	ObjectBytes                  int64  `json:"objectBytes"`
	SeedBytes                    int64  `json:"seedBytes"`
	Digest                       string `json:"sha256"`
	HistoryIncludesExcludedPaths bool   `json:"historyIncludesExcludedPaths"`
}

type syncPlanJSONProtected struct {
	Count    int                           `json:"count"`
	Examples []SyncProtectedTrackedExclude `json:"examples,omitempty"`
}

type syncPlanJSONSize struct {
	Files      int    `json:"files"`
	Bytes      int64  `json:"bytes"`
	HumanBytes string `json:"humanBytes"`
}

type syncPlanJSONRow struct {
	Path       string `json:"path"`
	Bytes      int64  `json:"bytes"`
	HumanBytes string `json:"humanBytes"`
}

type syncPlanJSONGuardrail struct {
	Scope      string                        `json:"scope"`
	Files      int                           `json:"files"`
	Bytes      int64                         `json:"bytes"`
	HumanBytes string                        `json:"humanBytes"`
	Limits     syncPlanJSONGuardrailLimits   `json:"limits"`
	AllowLarge bool                          `json:"allowLarge"`
	Status     string                        `json:"status"`
	Reasons    []syncPlanJSONGuardrailReason `json:"reasons,omitempty"`
}

type syncPlanJSONGuardrailLimits struct {
	WarnFiles int   `json:"warnFiles"`
	WarnBytes int64 `json:"warnBytes"`
	FailFiles int   `json:"failFiles"`
	FailBytes int64 `json:"failBytes"`
}

type syncPlanJSONGuardrailReason struct {
	Status string `json:"status"`
	Metric string `json:"metric"`
	Actual int64  `json:"actual"`
	Limit  int64  `json:"limit"`
}

func (a App) syncPlan(ctx context.Context, args []string) (err error) {
	fs := newFlagSet("sync-plan", a.Stderr)
	limit := fs.Int("limit", 20, "number of top files and directories to print")
	jsonOut := fs.Bool("json", false, "print JSON")
	gitSeedSource := fs.String("git-seed-source", "", "Git metadata source: origin or explicit offline local objects")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *limit <= 0 {
		return Exit(2, "sync-plan --limit must be positive")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := validateSyncSource(cfg); err != nil {
		return err
	}
	if flagWasSet(fs, "git-seed-source") {
		cfg.Sync.GitSeedSource = *gitSeedSource
	}
	if err := validateGitSeedSource(cfg); err != nil {
		return err
	}
	var repo Repo
	directory := effectiveSyncSource(cfg) == "directory"
	if directory {
		repo, err = findSyncRepo(cfg, true)
		if err == nil {
			err = validateDirectorySyncConfig(cfg)
		}
		if err == nil {
			provider, providerErr := ProviderFor(cfg.Provider)
			if providerErr != nil {
				return providerErr
			}
			err = validateDirectorySyncProvider(provider.Spec())
		}
	} else {
		var boundary repositoryBoundary
		boundary, err = findRepositoryBoundary()
		repo.Root = boundary.root
	}
	if err != nil {
		return err
	}
	var localSeed preparedLocalGitSeed
	local := effectiveGitSeedSource(cfg) == "local"
	if local {
		repo.BaseRef = defaultBaseRef(repo.Root)
		localSeed, err = prepareLocalGitSeed(ctx, repo, cfg, true, io.Discard)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, localSeed.cleanup()) }()
	}
	var manifest SyncManifest
	rowsRoot := repo.Root
	if local {
		manifest, rowsRoot = localSeed.Snapshot.Manifest, localSeed.Snapshot.Root
	} else {
		excludes, err := syncExcludes(repo.Root, cfg)
		if err != nil {
			return err
		}
		manifest, err = syncManifestForSource(ctx, repo, cfg, excludes)
		if err != nil {
			return Exit(6, "build sync file list: %v", err)
		}
	}
	files, dirs := syncPlanRows(rowsRoot, manifest, *limit)
	if *jsonOut {
		provider, err := ProviderFor(cfg.Provider)
		if err != nil {
			return err
		}
		out := syncPlanJSON(manifest, files, dirs, cfg, provider.Spec().SyncGuardrailFullCandidate)
		if local {
			guard := FullSyncGuardrailManifest(manifest)
			guard.Bytes += localSeed.Artifact.ObjectBytes
			out.Guardrail = syncPlanJSONGuardrailFor(guard, cfg)
			out.Guardrail.Scope = "candidate_and_git_objects"
			out.LocalGitSeed = &syncPlanJSONLocalGitSeed{
				Source: "local", Head: localSeed.Selection.Head, Base: localSeed.Selection.Base, BaseRef: localSeed.Selection.BaseRef,
				ObjectFormat: localSeed.Artifact.ObjectFormat, Objects: localSeed.Artifact.ObjectCount,
				ObjectBytes: localSeed.Artifact.ObjectBytes, SeedBytes: localSeed.Artifact.PackedBytes, Digest: localSeed.Artifact.Digest,
				HistoryIncludesExcludedPaths: true,
			}
		}
		if directory {
			out.Source, out.Root = "directory", repo.Root
		}
		if err := json.NewEncoder(a.Stdout).Encode(out); err != nil {
			return err
		}
		return nil
	}
	if directory {
		fmt.Fprintf(a.Stdout, "sync source=directory root=%s\n", repo.Root)
	}
	if local {
		fmt.Fprintf(a.Stdout, "Git seed source=local head=%s base=%s objects=%d object_bytes=%d seed_bytes=%d\n", localSeed.Selection.Head, blank(localSeed.Selection.Base, "absent"), localSeed.Artifact.ObjectCount, localSeed.Artifact.ObjectBytes, localSeed.Artifact.PackedBytes)
		fmt.Fprintln(a.Stdout, "Exclusions govern materialized files; complete selected history can contain excluded paths.")
	}
	fmt.Fprintf(a.Stdout, "sync candidate: %d files, %s\n", len(manifest.Files), humanBytes(manifest.Bytes))
	printProtectedTrackedExcludes(a.Stdout, manifest)
	if len(manifest.Deleted) > 0 {
		fmt.Fprintf(a.Stdout, "deleted tracked paths: %d\n", len(manifest.Deleted))
	}
	fmt.Fprintln(a.Stdout, "top files:")
	for _, row := range files {
		fmt.Fprintf(a.Stdout, "  %-10s %s\n", humanBytes(row.Bytes), row.Path)
	}
	fmt.Fprintln(a.Stdout, "top dirs:")
	for _, row := range dirs {
		fmt.Fprintf(a.Stdout, "  %-10s %s\n", humanBytes(row.Bytes), row.Path)
	}
	return nil
}

func syncPlanJSON(manifest SyncManifest, files, dirs []syncPlanRow, cfg Config, fullCandidate bool) syncPlanJSONOutput {
	guardrailManifest := manifest
	if fullCandidate {
		guardrailManifest = FullSyncGuardrailManifest(manifest)
	}
	return syncPlanJSONOutput{
		Candidate:           syncPlanJSONSizeFor(len(manifest.Files), manifest.Bytes),
		DirtyDelta:          syncPlanJSONSizeFor(len(manifest.Changed), manifest.ChangedBytes),
		DeletedTrackedPaths: len(manifest.Deleted),
		ProtectedTracked:    syncPlanJSONProtectedFor(manifest),
		Guardrail:           syncPlanJSONGuardrailFor(guardrailManifest, cfg),
		TopFiles:            syncPlanJSONRows(files),
		TopDirs:             syncPlanJSONRows(dirs),
	}
}

const protectedTrackedExcludeExampleLimit = 5

func syncPlanJSONProtectedFor(manifest SyncManifest) syncPlanJSONProtected {
	examples := manifest.ProtectedTrackedExcludes
	if len(examples) > protectedTrackedExcludeExampleLimit {
		examples = examples[:protectedTrackedExcludeExampleLimit]
	}
	return syncPlanJSONProtected{
		Count:    len(manifest.ProtectedTrackedExcludes),
		Examples: append([]SyncProtectedTrackedExclude(nil), examples...),
	}
}

func syncPlanJSONSizeFor(files int, bytes int64) syncPlanJSONSize {
	return syncPlanJSONSize{Files: files, Bytes: bytes, HumanBytes: humanBytes(bytes)}
}

func syncPlanJSONRows(rows []syncPlanRow) []syncPlanJSONRow {
	out := make([]syncPlanJSONRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, syncPlanJSONRow{Path: row.Path, Bytes: row.Bytes, HumanBytes: humanBytes(row.Bytes)})
	}
	return out
}

func syncPlanJSONGuardrailFor(manifest SyncManifest, cfg Config) syncPlanJSONGuardrail {
	evaluation := evaluateSyncGuardrail(manifest, cfg, false)
	out := syncPlanJSONGuardrail{
		Scope:      evaluation.Scope,
		Files:      evaluation.Count,
		Bytes:      evaluation.Bytes,
		HumanBytes: humanBytes(evaluation.Bytes),
		Limits: syncPlanJSONGuardrailLimits{
			WarnFiles: cfg.Sync.WarnFiles,
			WarnBytes: cfg.Sync.WarnBytes,
			FailFiles: cfg.Sync.FailFiles,
			FailBytes: cfg.Sync.FailBytes,
		},
		AllowLarge: evaluation.AllowLarge,
		Status:     evaluation.Status,
	}
	for _, reason := range evaluation.Reasons {
		out.Reasons = append(out.Reasons, syncPlanJSONGuardrailReason{
			Status: reason.Status,
			Metric: reason.Metric,
			Actual: reason.Actual,
			Limit:  reason.Limit,
		})
	}
	return out
}

func syncPlanRows(root string, manifest SyncManifest, limit int) ([]syncPlanRow, []syncPlanRow) {
	files := make([]syncPlanRow, 0, len(manifest.Files))
	dirBytes := map[string]int64{}
	for _, rel := range manifest.Files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil || info.IsDir() {
			continue
		}
		size := info.Size()
		files = append(files, syncPlanRow{Path: rel, Bytes: size})
		dirBytes[syncPlanDir(rel)] += size
	}
	sortSyncPlanRows(files)
	dirs := make([]syncPlanRow, 0, len(dirBytes))
	for dir, size := range dirBytes {
		dirs = append(dirs, syncPlanRow{Path: dir, Bytes: size})
	}
	sortSyncPlanRows(dirs)
	if len(files) > limit {
		files = files[:limit]
	}
	if len(dirs) > limit {
		dirs = dirs[:limit]
	}
	return files, dirs
}

func syncPlanDir(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 1 {
		return "."
	}
	if len(parts) == 2 {
		return parts[0]
	}
	return parts[0] + "/" + parts[1]
}

func sortSyncPlanRows(rows []syncPlanRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Bytes == rows[j].Bytes {
			return rows[i].Path < rows[j].Path
		}
		return rows[i].Bytes > rows[j].Bytes
	})
}
