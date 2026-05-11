package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	return Repo{
		Root:      root,
		Name:      filepath.Base(root),
		RemoteURL: gitOutput(root, "remote", "get-url", "origin"),
		Head:      gitOutput(root, "rev-parse", "HEAD"),
		BaseRef:   defaultBaseRef(root),
	}, nil
}

func defaultExcludes() []string {
	return []string{
		".git",
		"._*",
		"node_modules",
		".turbo",
		".next",
		"dist",
		"dist-runtime",
		"coverage",
		".cache",
		".local",
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
		if envAllowed(k, allow) {
			out[k] = v
		}
	}
	return out
}

func AllowedEnv(allow []string) map[string]string {
	return allowedEnv(allow)
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

func syncFingerprint(repo Repo, cfg Config) (string, error) {
	excludes, err := syncExcludes(repo.Root, cfg)
	if err != nil {
		return "", err
	}
	manifest, err := syncManifest(repo.Root, excludes)
	if err != nil {
		return "", err
	}
	return syncFingerprintForManifest(repo, cfg, manifest, excludes)
}

func syncFingerprintForManifest(repo Repo, cfg Config, manifest SyncManifest, excludes []string) (string, error) {
	if repo.Head == "" {
		return "", nil
	}
	paths, err := changedSyncPaths(repo.Root, excludes)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "v3\nhead=%s\n", repo.Head)
	fmt.Fprintf(h, "delete=%t\nchecksum=%t\n", cfg.Sync.Delete, cfg.Sync.Checksum)
	fmt.Fprintf(h, "manifest=%x\n", sha256.Sum256(manifest.NUL()))
	fmt.Fprintf(h, "deleted=%x\n", sha256.Sum256(manifest.DeletedNUL()))
	for _, exclude := range excludes {
		fmt.Fprintf(h, "exclude=%s\n", exclude)
	}
	for _, rel := range paths {
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
	Files   []string
	Deleted []string
	Bytes   int64
}

func syncManifest(root string, excludes []string) (SyncManifest, error) {
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
		if !safeRepoRel(rel) || pathExcluded(rel, excludes) || seen[rel] {
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
	deleted, err := syncDeletedPaths(root, excludes)
	if err != nil {
		return SyncManifest{}, err
	}
	manifest.Deleted = filterDeletedPaths(deleted, seen)
	return manifest, nil
}

func BuildSyncManifest(root string, excludes []string) (SyncManifest, error) {
	return syncManifest(root, excludes)
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

func syncDeletedPaths(root string, excludes []string) ([]string, error) {
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
			if !safeRepoRel(rel) || pathExcluded(rel, excludes) {
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
	fmt.Fprintf(stderr, "sync candidate: %d files, %s\n", fileCount, humanBytes(manifest.Bytes))
	allowLarge := force || cfg.Sync.AllowLarge || os.Getenv("CRABBOX_SYNC_ALLOW_LARGE") == "1"
	if !allowLarge {
		if cfg.Sync.FailFiles > 0 && fileCount >= cfg.Sync.FailFiles {
			return exit(6, "sync candidate too large: %d files >= limit %d; use --force-sync-large or CRABBOX_SYNC_ALLOW_LARGE=1", fileCount, cfg.Sync.FailFiles)
		}
		if cfg.Sync.FailBytes > 0 && manifest.Bytes >= cfg.Sync.FailBytes {
			return exit(6, "sync candidate too large: %s >= limit %s; use --force-sync-large or CRABBOX_SYNC_ALLOW_LARGE=1", humanBytes(manifest.Bytes), humanBytes(cfg.Sync.FailBytes))
		}
	}
	if cfg.Sync.WarnFiles > 0 && fileCount >= cfg.Sync.WarnFiles {
		fmt.Fprintf(stderr, "warning: large sync candidate: %d files >= warning threshold %d\n", fileCount, cfg.Sync.WarnFiles)
	}
	if cfg.Sync.WarnBytes > 0 && manifest.Bytes >= cfg.Sync.WarnBytes {
		fmt.Fprintf(stderr, "warning: large sync candidate: %s >= warning threshold %s\n", humanBytes(manifest.Bytes), humanBytes(cfg.Sync.WarnBytes))
	}
	return nil
}

func CheckSyncPreflight(manifest SyncManifest, cfg Config, force bool, stderr io.Writer) error {
	return checkSyncPreflight(manifest, cfg, force, stderr)
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

func changedSyncPaths(root string, excludes []string) ([]string, error) {
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
			if rel == "" || pathExcluded(rel, excludes) {
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
