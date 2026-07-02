package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

type Repo struct {
	Root      string
	Name      string
	RemoteURL string
	Head      string
	BaseRef   string
}

func findRepo() (Repo, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		wd, _ := os.Getwd()
		return Repo{Root: wd, Name: filepath.Base(wd)}, nil
	}
	root := strings.TrimSpace(string(out))
	remoteURL := gitOutput(root, "remote", "get-url", "origin")
	return Repo{
		Root:      root,
		Name:      repoNameFromRootAndRemote(root, remoteURL),
		RemoteURL: remoteURL,
		Head:      gitOutput(root, "rev-parse", "HEAD"),
		BaseRef:   defaultBaseRef(root),
	}, nil
}

func repoNameFromRootAndRemote(root, remoteURL string) string {
	fallback := filepath.Base(root)
	if repo, err := parseGitHubRepo(remoteURL); err == nil && repo.Name != "" {
		return repo.Name
	}
	if name := repoNameFromRemoteURL(remoteURL); name != "" {
		return name
	}
	return fallback
}

func repoNameFromRemoteURL(remoteURL string) string {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return ""
	}
	if strings.Contains(remoteURL, "://") {
		if u, err := url.Parse(remoteURL); err == nil {
			return cleanRemoteRepoName(path.Base(strings.Trim(u.Path, "/")))
		}
	}
	remotePath := strings.TrimRight(remoteURL, "/")
	if before, after, ok := strings.Cut(remotePath, ":"); ok && !strings.Contains(before, "/") {
		remotePath = after
	}
	return cleanRemoteRepoName(path.Base(strings.Trim(remotePath, "/")))
}

func cleanRemoteRepoName(name string) string {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".git")
	if name == "" || name == "." || name == "/" {
		return ""
	}
	return name
}

func defaultExcludes() []string {
	return []string{
		".git",
		"._*",
		"node_modules",
		".ignored",
		".turbo",
		".next",
		".vite",
		".parcel-cache",
		".rollup.cache",
		"dist",
		"dist-runtime",
		"coverage",
		"playwright-report",
		"test-results",
		".cache",
		".tmp",
		".local",
		".crabbox/logs",
		".crabbox/captures",
		".crabbox/runs",
		".swiftpm",
		".build",
		"apps/*/.build",
		".pnpm-store",
		".npm",
		".yarn/cache",
		".venv",
		"__pycache__",
		".pytest_cache",
		".mypy_cache",
		".ruff_cache",
		".gradle",
		"target",
	}
}

func configuredExcludes(cfg Config) []string {
	return appendUniqueStrings(defaultExcludes(), cfg.Sync.Excludes...)
}

func syncExcludes(root string, cfg Config) ([]string, error) {
	excludes := configuredExcludes(cfg)
	ignore, err := readCrabboxIgnore(root)
	if err != nil {
		return nil, err
	}
	return appendUniqueStrings(excludes, ignore...), nil
}

// syncIncludes returns the configured sync include (whitelist) patterns. When
// empty the whole working tree is synced (minus excludes); when non-empty only
// matching paths are synced.
func syncIncludes(cfg Config) []string {
	out := make([]string, 0, len(cfg.Sync.Includes))
	for _, p := range cfg.Sync.Includes {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func syncGitSeedEnabled(cfg Config, repo Repo) bool {
	return cfg.Sync.GitSeed && len(syncIncludes(cfg)) == 0 && remoteGitSeedCandidate(repo)
}

func SyncExcludes(root string, cfg Config) ([]string, error) {
	return syncExcludes(root, cfg)
}

func readCrabboxIgnore(root string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(root, ".crabboxignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, exit(2, "read .crabboxignore: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	patterns := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns, nil
}

func allowedEnv(allow []string) map[string]string {
	out := map[string]string{}
	for _, env := range os.Environ() {
		k, v, ok := strings.Cut(env, "=")
		if !ok {
			continue
		}
		if !validEnvName(k) {
			continue
		}
		if envAllowed(k, allow) {
			out[k] = v
		}
	}
	return out
}

func envAllowed(name string, allow []string) bool {
	for _, pattern := range allow {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if strings.HasSuffix(pattern, "*") {
			prefix := strings.TrimSuffix(pattern, "*")
			if prefix == "" {
				continue
			}
			if strings.HasPrefix(name, prefix) {
				return true
			}
			continue
		}
		if name == pattern {
			return true
		}
	}
	return false
}

func gitOutput(root string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func remoteGitSeedCandidate(repo Repo) bool {
	if repo.Root == "" || repo.RemoteURL == "" || repo.Head == "" {
		return false
	}
	return gitOutput(repo.Root, "for-each-ref", "--contains", repo.Head, "--format=%(refname)", "refs/remotes") != ""
}

func defaultBaseRef(root string) string {
	originHead := gitOutput(root, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if originHead != "" {
		return strings.TrimPrefix(originHead, "origin/")
	}
	branch := gitOutput(root, "branch", "--show-current")
	if branch != "" {
		return branch
	}
	return ""
}

func syncFingerprintForManifest(repo Repo, cfg Config, manifest SyncManifest, excludes []string) (string, error) {
	if repo.Head == "" {
		return "", nil
	}
	h := sha256.New()
	fmt.Fprintf(h, "v3\nhead=%s\n", repo.Head)
	fmt.Fprintf(h, "delete=%t\nchecksum=%t\n", cfg.Sync.Delete, cfg.Sync.Checksum)
	fmt.Fprintf(h, "manifest=%x\n", sha256.Sum256(manifest.NUL()))
	fmt.Fprintf(h, "deleted=%x\n", sha256.Sum256(manifest.DeletedNUL()))
	for _, exclude := range excludes {
		fmt.Fprintf(h, "exclude=%s\n", exclude)
	}
	for _, rel := range manifest.Changed {
		fmt.Fprintf(h, "path=%s\n", rel)
		full := filepath.Join(repo.Root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			fmt.Fprintf(h, "missing\n")
			continue
		}
		fmt.Fprintf(h, "mode=%s size=%d\n", info.Mode().String(), info.Size())
		if info.IsDir() {
			continue
		}
		file, err := os.Open(full)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(h, file); err != nil {
			_ = file.Close()
			return "", err
		}
		_ = file.Close()
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type SyncManifest struct {
	Files        []string
	Deleted      []string
	Changed      []string
	Bytes        int64
	ChangedBytes int64
}

func syncManifest(root string, excludes []string) (SyncManifest, error) {
	return syncManifestFiltered(root, excludes, nil)
}

// syncManifestFiltered builds the sync manifest applying excludes and, when
// includes is non-empty, a whitelist: only paths matching an include pattern are
// synced. This lets a job sync a few selected paths instead of the whole working
// tree (e.g. sync just `src/` and `scripts/` out of a large repo).
func syncManifestFiltered(root string, excludes, includes []string) (SyncManifest, error) {
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return SyncManifest{}, err
	}
	seen := map[string]bool{}
	manifest := SyncManifest{}
	for _, rel := range splitNul(out) {
		rel = filepath.ToSlash(rel)
		if !safeRepoRel(rel) || pathExcluded(rel, excludes) || !pathIncluded(rel, includes) || seen[rel] {
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil || info.IsDir() {
			continue
		}
		seen[rel] = true
		manifest.Files = append(manifest.Files, rel)
		manifest.Bytes += info.Size()
	}
	sort.Strings(manifest.Files)
	deleted, err := syncDeletedPaths(root, excludes, includes)
	if err != nil {
		return SyncManifest{}, err
	}
	manifest.Deleted = filterDeletedPaths(deleted, seen)
	changed, err := changedSyncPaths(root, excludes, includes)
	if err != nil {
		return SyncManifest{}, err
	}
	manifest.Changed, manifest.ChangedBytes = changedPathSetBytes(root, changed)
	return manifest, nil
}

func BuildSyncManifestFiltered(root string, excludes, includes []string) (SyncManifest, error) {
	return syncManifestFiltered(root, excludes, includes)
}

func (m SyncManifest) NUL() []byte {
	var b bytes.Buffer
	for _, rel := range m.Files {
		b.WriteString(rel)
		b.WriteByte(0)
	}
	return b.Bytes()
}

func (m SyncManifest) DeletedNUL() []byte {
	var b bytes.Buffer
	for _, rel := range m.Deleted {
		b.WriteString(rel)
		b.WriteByte(0)
	}
	return b.Bytes()
}

func syncDeletedPaths(root string, excludes, includes []string) ([]string, error) {
	sets := [][]string{}
	for _, args := range [][]string{
		{"ls-files", "--deleted", "-z"},
		{"diff", "--cached", "--name-only", "--diff-filter=D", "-z"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		sets = append(sets, splitNul(out))
	}
	seen := map[string]bool{}
	for _, set := range sets {
		for _, rel := range set {
			rel = filepath.ToSlash(rel)
			if !safeRepoRel(rel) || pathExcluded(rel, excludes) || !pathIncluded(rel, includes) {
				continue
			}
			seen[rel] = true
		}
	}
	out := make([]string, 0, len(seen))
	for rel := range seen {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}

func filterDeletedPaths(deleted []string, files map[string]bool) []string {
	out := deleted[:0]
	for _, rel := range deleted {
		if !files[rel] {
			out = append(out, rel)
		}
	}
	return out
}

func changedPathSetBytes(root string, paths []string) ([]string, int64) {
	out := make([]string, 0, len(paths))
	var bytes int64
	for _, rel := range paths {
		if !safeRepoRel(rel) {
			continue
		}
		out = append(out, rel)
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil || info.IsDir() {
			continue
		}
		bytes += info.Size()
	}
	sort.Strings(out)
	return out, bytes
}

func safeRepoRel(rel string) bool {
	if rel == "" || strings.HasPrefix(rel, "/") {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func checkSyncPreflight(manifest SyncManifest, cfg Config, force bool, stderr io.Writer) error {
	fileCount := len(manifest.Files)
	guardCount, guardBytes, guardScope, guardPaths := syncGuardrailScope(manifest)
	if len(manifest.Changed) > 0 {
		fmt.Fprintf(stderr, "sync candidate: %d files, %s dirty_delta=%d files, %s\n", fileCount, humanBytes(manifest.Bytes), len(manifest.Changed), humanBytes(manifest.ChangedBytes))
	} else {
		fmt.Fprintf(stderr, "sync candidate: %d files, %s\n", fileCount, humanBytes(manifest.Bytes))
	}
	allowLarge := force || cfg.Sync.AllowLarge || os.Getenv("CRABBOX_SYNC_ALLOW_LARGE") == "1"
	if !allowLarge {
		if cfg.Sync.FailFiles > 0 && guardCount >= cfg.Sync.FailFiles {
			printSyncTopDirs(stderr, guardPaths)
			return exit(6, "sync %s too large: %d files >= limit %d; use --force-sync-large or CRABBOX_SYNC_ALLOW_LARGE=1", guardScope, guardCount, cfg.Sync.FailFiles)
		}
		if cfg.Sync.FailBytes > 0 && guardBytes >= cfg.Sync.FailBytes {
			printSyncTopDirs(stderr, guardPaths)
			return exit(6, "sync %s too large: %s >= limit %s; use --force-sync-large or CRABBOX_SYNC_ALLOW_LARGE=1", guardScope, humanBytes(guardBytes), humanBytes(cfg.Sync.FailBytes))
		}
	}
	warned := false
	if cfg.Sync.WarnFiles > 0 && guardCount >= cfg.Sync.WarnFiles {
		fmt.Fprintf(stderr, "warning: large sync %s: %d files >= warning threshold %d\n", guardScope, guardCount, cfg.Sync.WarnFiles)
		warned = true
	}
	if cfg.Sync.WarnBytes > 0 && guardBytes >= cfg.Sync.WarnBytes {
		fmt.Fprintf(stderr, "warning: large sync %s: %s >= warning threshold %s\n", guardScope, humanBytes(guardBytes), humanBytes(cfg.Sync.WarnBytes))
		warned = true
	}
	if warned {
		printSyncTopDirs(stderr, guardPaths)
	}
	return nil
}

func syncGuardrailScope(manifest SyncManifest) (count int, bytes int64, scope string, paths []string) {
	if len(manifest.Changed) > 0 {
		return len(manifest.Changed), manifest.ChangedBytes, "dirty_delta", manifest.Changed
	}
	return len(manifest.Files), manifest.Bytes, "candidate", manifest.Files
}

func CheckSyncPreflight(manifest SyncManifest, cfg Config, force bool, stderr io.Writer) error {
	return checkSyncPreflight(manifest, cfg, force, stderr)
}

func printSyncTopDirs(stderr io.Writer, paths []string) {
	if stderr == nil {
		return
	}
	type dirCount struct {
		Dir   string
		Count int
	}
	counts := map[string]int{}
	for _, file := range paths {
		dir := strings.Split(file, "/")[0]
		if dir == "" {
			dir = "."
		}
		counts[dir]++
	}
	dirs := make([]dirCount, 0, len(counts))
	for dir, count := range counts {
		dirs = append(dirs, dirCount{Dir: dir, Count: count})
	}
	sort.Slice(dirs, func(i, j int) bool {
		if dirs[i].Count == dirs[j].Count {
			return dirs[i].Dir < dirs[j].Dir
		}
		return dirs[i].Count > dirs[j].Count
	})
	if len(dirs) > 5 {
		dirs = dirs[:5]
	}
	parts := make([]string, 0, len(dirs))
	for _, item := range dirs {
		parts = append(parts, fmt.Sprintf("%s:%d", item.Dir, item.Count))
	}
	if len(parts) > 0 {
		fmt.Fprintf(stderr, "sync top dirs: %s\n", strings.Join(parts, ","))
		fmt.Fprintln(stderr, "sync hint: add generated paths to .crabboxignore or sync.exclude, or use --force-sync-large when intentional")
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}

func changedSyncPaths(root string, excludes, includes []string) ([]string, error) {
	sets := [][]string{}
	for _, args := range [][]string{
		{"diff", "--name-only", "-z"},
		{"diff", "--cached", "--name-only", "-z"},
		{"ls-files", "--others", "--exclude-standard", "-z"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		sets = append(sets, splitNul(out))
	}
	seen := map[string]bool{}
	for _, set := range sets {
		for _, rel := range set {
			rel = filepath.ToSlash(rel)
			if rel == "" || pathExcluded(rel, excludes) || !pathIncluded(rel, includes) {
				continue
			}
			seen[rel] = true
		}
	}
	out := make([]string, 0, len(seen))
	for rel := range seen {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}

func splitNul(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	parts := bytes.Split(data, []byte{0})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) > 0 {
			out = append(out, string(part))
		}
	}
	return out
}

func pathExcluded(rel string, excludes []string) bool {
	rel = filepath.ToSlash(rel)
	parts := strings.Split(rel, "/")
	for _, exclude := range excludes {
		exclude = strings.Trim(filepath.ToSlash(strings.TrimSpace(exclude)), "/")
		if exclude == "" {
			continue
		}
		if rel == exclude || strings.HasPrefix(rel, exclude+"/") {
			return true
		}
		if !strings.Contains(exclude, "/") {
			for _, part := range parts {
				if part == exclude {
					return true
				}
				if ok, _ := filepath.Match(exclude, part); ok {
					return true
				}
			}
		}
		if ok, _ := filepath.Match(exclude, filepath.Base(rel)); ok {
			return true
		}
		if ok, _ := filepath.Match(exclude, rel); ok {
			return true
		}
		for i := 1; i < len(parts); i++ {
			prefix := strings.Join(parts[:i], "/")
			if ok, _ := filepath.Match(exclude, prefix); ok {
				return true
			}
		}
	}
	return false
}

// pathIncluded reports whether rel should be kept under a sync include
// whitelist. Includes are root-relative: "src" keeps only the top-level src
// tree, "package.json" keeps only that root file, and globs match the
// root-relative path.
func pathIncluded(rel string, includes []string) bool {
	if len(includes) == 0 {
		return true
	}
	rel = strings.Trim(filepath.ToSlash(rel), "/")
	for _, include := range includes {
		include = strings.Trim(filepath.ToSlash(strings.TrimSpace(include)), "/")
		if include == "" {
			continue
		}
		if rel == include || strings.HasPrefix(rel, include+"/") {
			return true
		}
		if ok, _ := filepath.Match(include, rel); ok {
			return true
		}
	}
	return false
}
