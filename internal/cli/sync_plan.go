package cli

import (
	"context"
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

func (a App) syncPlan(ctx context.Context, args []string) error {
	_ = ctx
	fs := newFlagSet("sync-plan", a.Stderr)
	limit := fs.Int("limit", 20, "number of top files and directories to print")
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
