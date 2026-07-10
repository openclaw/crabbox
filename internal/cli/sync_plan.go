package cli

import (
	"context"
	"encoding/json"
	"fmt"
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
	Candidate           syncPlanJSONSize      `json:"candidate"`
	DirtyDelta          syncPlanJSONSize      `json:"dirtyDelta"`
	DeletedTrackedPaths int                   `json:"deletedTrackedPaths"`
	Guardrail           syncPlanJSONGuardrail `json:"guardrail"`
	TopFiles            []syncPlanJSONRow     `json:"topFiles"`
	TopDirs             []syncPlanJSONRow     `json:"topDirs"`
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

func (a App) syncPlan(ctx context.Context, args []string) error {
	_ = ctx
	fs := newFlagSet("sync-plan", a.Stderr)
	limit := fs.Int("limit", 20, "number of top files and directories to print")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *limit <= 0 {
		return exit(2, "sync-plan --limit must be positive")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	excludes, err := syncExcludes(repo.Root, cfg)
	if err != nil {
		return err
	}
	manifest, err := syncManifestFiltered(repo.Root, excludes, syncIncludes(cfg))
	if err != nil {
		return exit(6, "build sync file list: %v", err)
	}
	files, dirs := syncPlanRows(repo.Root, manifest, *limit)
	if *jsonOut {
		out := syncPlanJSON(manifest, files, dirs, cfg)
		if err := json.NewEncoder(a.Stdout).Encode(out); err != nil {
			return err
		}
		return nil
	}
	fmt.Fprintf(a.Stdout, "sync candidate: %d files, %s\n", len(manifest.Files), humanBytes(manifest.Bytes))
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

func syncPlanJSON(manifest SyncManifest, files, dirs []syncPlanRow, cfg Config) syncPlanJSONOutput {
	return syncPlanJSONOutput{
		Candidate:           syncPlanJSONSizeFor(len(manifest.Files), manifest.Bytes),
		DirtyDelta:          syncPlanJSONSizeFor(len(manifest.Changed), manifest.ChangedBytes),
		DeletedTrackedPaths: len(manifest.Deleted),
		Guardrail:           syncPlanJSONGuardrailFor(manifest, cfg),
		TopFiles:            syncPlanJSONRows(files),
		TopDirs:             syncPlanJSONRows(dirs),
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
