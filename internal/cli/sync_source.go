package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func effectiveSyncSource(cfg Config) string {
	if source := strings.TrimSpace(cfg.Sync.Source); source != "" {
		return source
	}
	return "git"
}

func validateSyncSource(cfg Config) error {
	switch effectiveSyncSource(cfg) {
	case "git", "directory":
		return nil
	default:
		return Exit(2, "sync.source must be git or directory")
	}
}

func findSyncRepo(cfg Config, syncEnabled bool) (Repo, error) {
	if err := validateSyncSource(cfg); err != nil {
		return Repo{}, err
	}
	if !syncEnabled || effectiveSyncSource(cfg) != "directory" {
		return findRepo()
	}
	root, err := os.Getwd()
	if err != nil {
		return Repo{}, err
	}
	boundary, err := nearestRepositoryBoundary(root, "")
	if err != nil {
		return Repo{}, err
	}
	if boundary.kind == repositoryBoundaryNativeJujutsu {
		return Repo{}, Exit(6, "directory sync does not support native Jujutsu workspaces; use colocated Git or --no-sync")
	}
	root = canonicalRepositoryPath(root)
	return Repo{Root: root, Name: filepath.Base(root)}, nil
}

func validateDirectorySyncConfig(cfg Config) error {
	if len(syncIncludes(cfg)) == 0 {
		return Exit(2, "sync.source=directory requires a nonempty sync.include allowlist")
	}
	if cfg.Sync.GitOverlay || strings.TrimSpace(cfg.Sync.BaseRef) != "" {
		return Exit(2, "sync.source=directory cannot use sync.gitOverlay or sync.baseRef")
	}
	if strings.TrimSpace(cfg.Actions.Workflow) != "" {
		return Exit(2, "sync.source=directory cannot use Actions hydration; remove actions.workflow")
	}
	if cfg.TargetOS == targetWindows && cfg.WindowsMode == windowsModeNormal {
		return Exit(2, "directory sync requires managed-manifest SSH sync on POSIX/WSL; native-Windows archive replacement is unsupported")
	}
	return nil
}

func validateDirectorySyncProvider(spec ProviderSpec) error {
	if spec.Kind != ProviderKindSSHLease || !spec.Features.Has(FeatureCrabboxSync) {
		return Exit(2, "provider=%s does not support directory sync: an ordinary SSH lease provider with crabbox-sync is required", spec.Name)
	}
	return nil
}

func syncManifestForSource(ctx context.Context, repo Repo, cfg Config, excludes SyncExcludeRules) (SyncManifest, error) {
	if err := validateSyncSource(cfg); err != nil {
		return SyncManifest{}, err
	}
	if effectiveSyncSource(cfg) == "git" {
		return syncManifestFilteredRules(repo.Root, excludes, syncIncludes(cfg))
	}
	if err := validateDirectorySyncConfig(cfg); err != nil {
		return SyncManifest{}, err
	}
	managed, excludes, err := prepareSyncManifestRoot(repo.Root, excludes)
	if err != nil {
		return SyncManifest{}, err
	}
	if cfg.Sync.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Sync.Timeout)
		defer cancel()
	}
	paths, err := directorySyncFileList(ctx, repo.Root)
	if err != nil {
		return SyncManifest{}, err
	}
	for _, candidate := range paths {
		if !strings.HasSuffix(candidate, "/") {
			continue
		}
		rel := strings.TrimSuffix(candidate, "/")
		isManaged, err := managed.contains(rel)
		if err != nil {
			return SyncManifest{}, err
		}
		if isManaged || !directorySyncNestedRepositoryInScope(rel, syncIncludes(cfg), excludes) {
			continue
		}
		return SyncManifest{}, fmt.Errorf("directory sync does not traverse nested repository %q; exclude that subtree or run from that directory", rel)
	}
	manifest, _, err := projectSyncManifest(repo.Root, excludes, syncIncludes(cfg), paths, syncManifestScope{}, managed)
	return manifest, err
}

func directorySyncNestedRepositoryInScope(rel string, includes []string, excludes SyncExcludeRules) bool {
	for _, include := range includes {
		if !includeMaySelectDescendant(rel, []string{include}) {
			continue
		}
		include = strings.Trim(filepath.ToSlash(strings.TrimSpace(include)), "/")
		if directorySyncIncludePatternExcluded(rel, include, excludes) {
			continue
		}
		scopeRoot := rel
		if !strings.ContainsAny(include, "*?[") && strings.HasPrefix(include, rel+"/") {
			scopeRoot = include
		}
		if directorySyncSubtreeIncluded(scopeRoot, excludes) {
			return true
		}
	}
	return false
}

// Identical include/exclude patterns prove exclusion without solving wildcard
// intersections. Later possible reinclusions keep the nested repository in scope.
func directorySyncIncludePatternExcluded(rel, include string, excludes SyncExcludeRules) bool {
	lastMatch, excluded := -1, false
	for i, rule := range excludes.rules {
		pattern, negated := excludeRule(rule.pattern)
		pattern = strings.Trim(filepath.ToSlash(pattern), "/")
		if pattern == include {
			lastMatch, excluded = i, !negated
		}
	}
	return excluded && !excludedDirMayContainReinclude(rel, excludePatternsAfter(excludes, lastMatch))
}

func excludePatternsAfter(excludes SyncExcludeRules, index int) []string {
	patterns := make([]string, 0, len(excludes.rules)-index-1)
	for _, rule := range excludes.rules[index+1:] {
		patterns = append(patterns, rule.pattern)
	}
	return patterns
}

func directorySyncSubtreeIncluded(rel string, excludes SyncExcludeRules) bool {
	if excludes.protectsManagedState(rel) {
		return false
	}
	lastMatch, excluded := -1, false
	for i, rule := range excludes.rules {
		pattern, negated := excludeRule(rule.pattern)
		if excludeMatches(rel, pattern) || protectedSyncExcludeMatches(rel, pattern) {
			lastMatch, excluded = i, !negated
		}
	}
	if !excluded {
		return true
	}
	return excludedDirMayContainReinclude(rel, excludePatternsAfter(excludes, lastMatch))
}

// Git owns .gitignore interpretation, but no source index or Git configuration
// participates. Temporary metadata must remain outside the selected source.
func directorySyncFileList(ctx context.Context, root string) (_ []string, err error) {
	root = canonicalRepositoryPath(root)
	tempBase, err := filepath.Abs(os.TempDir())
	if err != nil {
		return nil, err
	}
	if pathWithinRoot(canonicalRepositoryPath(tempBase), root) {
		return nil, fmt.Errorf("directory sync requires a temporary directory outside source %s; set TMPDIR to an external directory", root)
	}
	temp, err := os.MkdirTemp(tempBase, "crabbox-directory-sync-*")
	if err != nil {
		return nil, fmt.Errorf("create directory sync metadata: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(temp); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove directory sync metadata %s: %w", temp, cleanupErr))
		}
	}()
	env := make([]string, 0)
	for _, entry := range repositoryGitEnvironment() {
		name, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(name)
		if upper != "HOME" && upper != "XDG_CONFIG_HOME" && !strings.HasPrefix(upper, "GIT_") {
			env = append(env, entry)
		}
	}
	env = append(env, "HOME="+temp, "XDG_CONFIG_HOME="+temp,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	metadata := filepath.Join(temp, "metadata")
	invoke := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir, cmd.Env = root, env
		out, callErr := cmd.Output()
		if callErr != nil {
			return nil, fmt.Errorf("directory sync Git enumeration (installed Git required): %w", callErr)
		}
		return out, nil
	}
	if _, err := invoke("init", "--bare", "--template=", "--quiet", metadata); err != nil {
		return nil, err
	}
	out, err := invoke("--git-dir="+metadata, "--work-tree="+root,
		"-c", "core.excludesFile="+os.DevNull, "-c", "core.hooksPath="+os.DevNull,
		"ls-files", "--others", "--exclude-standard", "--full-name", "-z")
	if err != nil {
		return nil, err
	}
	return splitNul(out), nil
}
