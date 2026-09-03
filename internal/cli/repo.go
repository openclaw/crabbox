package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
)

type Repo struct {
	Root      string
	Name      string
	RemoteURL string
	Head      string
	BaseRef   string
}

type repositoryBoundaryKind uint8

const (
	repositoryBoundaryGit repositoryBoundaryKind = iota + 1
	repositoryBoundaryNativeJujutsu
)

type repositoryBoundary struct {
	root string
	kind repositoryBoundaryKind
}

// nearestRepositoryBoundary keeps repository ownership tied to the closest
// workspace marker before stop. In particular, Git must not discover an outer
// checkout through a native Jujutsu workspace nested inside it.
func nearestRepositoryBoundary(start, stop string) (repositoryBoundary, error) {
	current, err := filepath.Abs(start)
	if err != nil {
		return repositoryBoundary{}, err
	}
	current = canonicalRepositoryPath(current)
	start = current
	if stop != "" {
		stop = canonicalRepositoryPath(stop)
	}
	ceilings := gitDiscoveryCeilings()
	for {
		if stop != "" && sameCanonicalRepositoryPath(current, stop) {
			return repositoryBoundary{}, nil
		}
		if len(ceilings) > 0 && !sameCanonicalRepositoryPath(current, start) && canonicalRepositoryPathSetContains(ceilings, current) {
			return repositoryBoundary{}, nil
		}
		hasGit, err := repositoryMarkerExists(filepath.Join(current, ".git"))
		if err != nil {
			return repositoryBoundary{}, err
		}
		hasJujutsu, err := repositoryMarkerExists(filepath.Join(current, ".jj"))
		if err != nil {
			return repositoryBoundary{}, err
		}
		if hasGit {
			if hasJujutsu && !gitBoundaryIsValid(current) {
				return repositoryBoundary{root: current, kind: repositoryBoundaryNativeJujutsu}, nil
			}
			return repositoryBoundary{root: current, kind: repositoryBoundaryGit}, nil
		}
		if hasJujutsu {
			return repositoryBoundary{root: current, kind: repositoryBoundaryNativeJujutsu}, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return repositoryBoundary{}, nil
		}
		current = parent
	}
}

func gitBoundaryIsValid(root string) bool {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = root
	cmd.Env = repositoryGitEnvironmentWithCeiling(filepath.Dir(root))
	out, err := cmd.Output()
	return err == nil && sameRepositoryPath(strings.TrimSpace(string(out)), root)
}

func repositoryGitEnvironmentWithCeiling(ceiling string) []string {
	env := childEnvironmentWithout(repositoryGitEnvironment(), "GIT_CEILING_DIRECTORIES")
	return append(env, "GIT_CEILING_DIRECTORIES="+ceiling)
}

func explicitGitRepositoryRouting() bool {
	return strings.TrimSpace(os.Getenv("GIT_DIR")) != "" || strings.TrimSpace(os.Getenv("GIT_WORK_TREE")) != ""
}

func gitDiscoveryCeilings() []string {
	entries := filepath.SplitList(os.Getenv("GIT_CEILING_DIRECTORIES"))
	ceilings := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry == "" || !filepath.IsAbs(entry) {
			continue
		}
		ceilings = append(ceilings, canonicalRepositoryPath(entry))
	}
	return ceilings
}

func canonicalRepositoryPathSetContains(paths []string, candidate string) bool {
	for _, path := range paths {
		if sameCanonicalRepositoryPath(path, candidate) {
			return true
		}
	}
	return false
}

func sameRepositoryPath(left, right string) bool {
	left = canonicalRepositoryPath(left)
	right = canonicalRepositoryPath(right)
	return sameCanonicalRepositoryPath(left, right)
}

func sameCanonicalRepositoryPath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func canonicalRepositoryPath(value string) string {
	value = filepath.Clean(value)
	if resolved, err := filepath.EvalSymlinks(value); err == nil {
		return filepath.Clean(resolved)
	}
	return value
}

func repositoryMarkerExists(marker string) (bool, error) {
	_, err := os.Lstat(marker)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("inspect repository marker %s: %w", marker, err)
}

// Repository inspection never needs provider or desktop credentials. Keep its
// environment intentionally small because discovery runs before the complete
// project configuration (and therefore its secret denylist) is known.
func repositoryGitEnvironment() []string {
	allowed := map[string]struct{}{
		"PATH": {}, "HOME": {}, "USER": {}, "LOGNAME": {}, "SHELL": {},
		"TMPDIR": {}, "TMP": {}, "TEMP": {}, "LANG": {}, "TZ": {},
		"XDG_CONFIG_HOME": {}, "SYSTEMROOT": {}, "WINDIR": {}, "COMSPEC": {},
		"PATHEXT": {}, "USERPROFILE": {}, "HOMEDRIVE": {}, "HOMEPATH": {},
		"APPDATA": {}, "LOCALAPPDATA": {},
		"GIT_DIR": {}, "GIT_WORK_TREE": {}, "GIT_CEILING_DIRECTORIES": {},
		"GIT_DISCOVERY_ACROSS_FILESYSTEM": {},
	}
	result := make([]string, 0, len(allowed)+4)
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(name)
		if _, ok := allowed[upper]; ok || strings.HasPrefix(upper, "LC_") {
			if (upper == "GIT_DIR" || upper == "GIT_WORK_TREE") && value != "" && !filepath.IsAbs(value) {
				if absolute, err := filepath.Abs(value); err == nil {
					entry = name + "=" + absolute
				}
			}
			result = append(result, entry)
		}
	}
	return result
}

type gitTrackedPath struct {
	name            string
	mode            string
	stage           int
	skipWorktree    bool
	assumeUnchanged bool
}

// GitCheckoutHasHiddenOmissions reports whether sparse rules or skip-worktree
// index state leave tracked paths absent. Delegated sync tools must not treat
// them as deletions from a complete checkout.
func GitCheckoutHasHiddenOmissions(root string) (bool, error) {
	return gitCheckoutHasHiddenOmissions(root, sparseCheckoutIncludedPaths)
}

func gitCheckoutHasHiddenOmissions(root string, resolveSparseRules func(string, []gitTrackedPath) (map[string]struct{}, error)) (bool, error) {
	path, err := gitCheckoutHiddenOmission(root, nil, resolveSparseRules)
	return path != "", err
}

func gitCheckoutHiddenOmission(
	root string,
	inScope func(gitTrackedPath) bool,
	resolveSparseRules func(string, []gitTrackedPath) (map[string]struct{}, error),
) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", nil
	}
	sparseEnabled := gitCheckoutSparseEnabled(root)
	if !sparseEnabled && gitOutput(root, "rev-parse", "--is-inside-work-tree") != "true" {
		return "", nil
	}
	tracked, err := loadGitTrackedPaths(root)
	if err != nil {
		return "", err
	}
	return gitCheckoutHiddenOmissionForTracked(root, tracked, sparseEnabled, inScope, resolveSparseRules)
}

func gitCheckoutSparseEnabled(root string) bool {
	return strings.EqualFold(
		strings.TrimSpace(gitOutput(root, "config", "--bool", "core.sparseCheckout")),
		"true",
	)
}

func loadGitTrackedPaths(root string) ([]gitTrackedPath, error) {
	trackedCmd := exec.Command("git", "ls-files", "-v", "--stage", "-z")
	trackedCmd.Dir = root
	trackedCmd.Env = repositoryGitEnvironment()
	tagged, err := trackedCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list tracked paths: %w", err)
	}
	tracked, err := parseGitTrackedPaths(tagged)
	if err != nil {
		return nil, err
	}
	return tracked, nil
}

func gitCheckoutHiddenOmissionForTracked(
	root string,
	tracked []gitTrackedPath,
	sparseEnabled bool,
	inScope func(gitTrackedPath) bool,
	resolveSparseRules func(string, []gitTrackedPath) (map[string]struct{}, error),
) (string, error) {
	scoped := make([]gitTrackedPath, 0, len(tracked))
	for _, entry := range tracked {
		if inScope != nil && !inScope(entry) {
			continue
		}
		scoped = append(scoped, entry)
	}
	if len(scoped) == 0 {
		return "", nil
	}
	includedPaths := make(map[string]struct{})
	var sparseRulesErr error
	if sparseEnabled {
		includedPaths, sparseRulesErr = resolveSparseRules(root, scoped)
	}
	for _, entry := range scoped {
		if sparseEnabled && sparseRulesErr != nil && !entry.skipWorktree {
			exists, statErr := trackedPathExistsWithoutSymlinkAncestor(root, entry.name)
			if statErr != nil {
				return "", fmt.Errorf("inspect tracked path %q: %w", entry.name, statErr)
			}
			if !exists {
				return "", fmt.Errorf("classify missing tracked path %q without git sparse-checkout check-rules (requires Git 2.41 or newer): %w", entry.name, sparseRulesErr)
			}
			continue
		}
		hiddenCandidate := entry.skipWorktree
		if sparseEnabled && sparseRulesErr == nil {
			_, ruleIncludesPath := includedPaths[entry.name]
			hiddenCandidate = hiddenCandidate || !ruleIncludesPath
		}
		if !hiddenCandidate {
			continue
		}
		exists, statErr := trackedPathExistsWithoutSymlinkAncestor(root, entry.name)
		if statErr != nil {
			return "", fmt.Errorf("inspect tracked path %q: %w", entry.name, statErr)
		}
		if exists {
			continue
		}
		return entry.name, nil
	}
	return "", nil
}

func parseGitTrackedPaths(tagged []byte) ([]gitTrackedPath, error) {
	tracked := make([]gitTrackedPath, 0, bytes.Count(tagged, []byte{0}))
	for _, record := range bytes.Split(tagged, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		if len(record) < 3 || record[1] != ' ' {
			return nil, fmt.Errorf("parse tracked path metadata")
		}
		metadata, name, ok := bytes.Cut(record[2:], []byte{'\t'})
		fields := bytes.Fields(metadata)
		if !ok || len(name) == 0 || len(fields) != 3 {
			return nil, fmt.Errorf("parse tracked path metadata")
		}
		mode := string(fields[0])
		if _, err := strconv.ParseUint(mode, 8, 32); err != nil {
			return nil, fmt.Errorf("parse tracked path mode %q", mode)
		}
		stage, err := strconv.Atoi(string(fields[2]))
		if err != nil || stage < 0 || stage > 3 {
			return nil, fmt.Errorf("parse tracked path stage %q", fields[2])
		}
		tracked = append(tracked, gitTrackedPath{
			name:            string(name),
			mode:            mode,
			stage:           stage,
			skipWorktree:    record[0] == 'S' || record[0] == 's',
			assumeUnchanged: record[0] >= 'a' && record[0] <= 'z',
		})
	}
	return tracked, nil
}

func trackedPathExistsWithoutSymlinkAncestor(root, gitPath string) (bool, error) {
	parts := strings.Split(filepath.FromSlash(gitPath), string(filepath.Separator))
	current := root
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if i < len(parts)-1 && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return false, nil
		}
	}
	return true, nil
}

func sparseCheckoutIncludedPaths(root string, tracked []gitTrackedPath) (map[string]struct{}, error) {
	var paths bytes.Buffer
	for _, trackedPath := range tracked {
		paths.WriteString(trackedPath.name)
		paths.WriteByte(0)
	}
	cmd := exec.Command("git", "sparse-checkout", "check-rules", "-z")
	cmd.Dir = root
	cmd.Env = repositoryGitEnvironment()
	cmd.Stdin = bytes.NewReader(paths.Bytes())
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git sparse-checkout check-rules unavailable: %w", err)
	}
	return nulPathSet(out), nil
}

func nulPathSet(out []byte) map[string]struct{} {
	paths := make(map[string]struct{})
	for _, item := range bytes.Split(out, []byte{0}) {
		if len(item) > 0 {
			paths[string(item)] = struct{}{}
		}
	}
	return paths
}

func findRepo() (Repo, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Env = repositoryGitEnvironment()
	out, err := cmd.Output()
	if err != nil {
		wd, _ := os.Getwd()
		if !explicitGitRepositoryRouting() {
			boundary, boundaryErr := nearestRepositoryBoundary(wd, "")
			if boundaryErr != nil {
				return Repo{}, boundaryErr
			}
			if boundary.kind == repositoryBoundaryNativeJujutsu {
				return Repo{Root: boundary.root, Name: filepath.Base(boundary.root)}, nil
			}
		}
		return Repo{Root: wd, Name: filepath.Base(wd)}, nil
	}
	root := strings.TrimSpace(string(out))
	if !explicitGitRepositoryRouting() {
		wd, getwdErr := os.Getwd()
		if getwdErr != nil {
			return Repo{}, getwdErr
		}
		if boundary, boundaryErr := nearestRepositoryBoundary(wd, root); boundaryErr != nil {
			return Repo{}, boundaryErr
		} else if boundary.kind == repositoryBoundaryNativeJujutsu {
			return Repo{Root: boundary.root, Name: filepath.Base(boundary.root)}, nil
		}
	}
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
	excludes := []string{
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
	return append(excludes, protectedSyncExcludes()...)
}

type syncExcludeOrigin uint8

const (
	syncExcludeBuiltIn syncExcludeOrigin = iota
	syncExcludeConfigured
	syncExcludeProtected
)

type syncExcludeRule struct {
	pattern string
	origin  syncExcludeOrigin
}

// SyncExcludeRules keeps ordered matcher provenance internal while allowing
// provider adapters to pass the rules back to the core manifest owner.
type SyncExcludeRules struct {
	rules []syncExcludeRule
}

func configuredExcludes(cfg Config) SyncExcludeRules {
	rules := newSyncExcludeRules(defaultExcludes(), syncExcludeBuiltIn)
	for i := range rules.rules {
		if isProtectedSyncExclude(rules.rules[i].pattern) {
			rules.rules[i].origin = syncExcludeProtected
		}
	}
	return rules.append(cfg.Sync.Excludes, syncExcludeConfigured)
}

func syncExcludes(root string, cfg Config) (SyncExcludeRules, error) {
	excludes := configuredExcludes(cfg)
	ignore, err := readCrabboxIgnore(root)
	if err != nil {
		return SyncExcludeRules{}, err
	}
	excludes = excludes.append(ignore, syncExcludeConfigured)
	return excludes.append(protectedSyncExcludes(), syncExcludeProtected), nil
}

func newSyncExcludeRules(patterns []string, origin syncExcludeOrigin) SyncExcludeRules {
	return (SyncExcludeRules{}).append(patterns, origin)
}

func (r SyncExcludeRules) append(patterns []string, origin syncExcludeOrigin) SyncExcludeRules {
	out := SyncExcludeRules{rules: append([]syncExcludeRule(nil), r.rules...)}
	for _, pattern := range patterns {
		if pattern = strings.TrimSpace(pattern); pattern != "" {
			out.rules = append(out.rules, syncExcludeRule{pattern: pattern, origin: origin})
		}
	}
	return out
}

func (r SyncExcludeRules) patterns() []string {
	out := make([]string, 0, len(r.rules))
	for _, rule := range r.rules {
		out = append(out, rule.pattern)
	}
	return out
}

func protectedSyncExcludes() []string {
	return []string{
		".crabbox/env",
		".crabbox/scripts",
		".crabbox/logs",
		".crabbox/captures",
		".crabbox/runs",
	}
}

func isProtectedSyncExclude(pattern string) bool {
	pattern = strings.ToLower(strings.Trim(filepath.ToSlash(pattern), "/"))
	for _, protected := range protectedSyncExcludes() {
		if pattern == protected {
			return true
		}
	}
	return false
}

func isAmbiguousBuiltInExclude(pattern string) bool {
	pattern, negated := excludeRule(pattern)
	if negated {
		return false
	}
	pattern = strings.Trim(filepath.ToSlash(pattern), "/")
	switch pattern {
	case "dist", "dist-runtime", "coverage", "playwright-report", "test-results", ".build", "apps/*/.build", "target":
		return true
	default:
		return false
	}
}

func protectedSyncExcludeMatches(rel, exclude string) bool {
	rel = strings.ToLower(strings.Trim(filepath.ToSlash(rel), "/"))
	exclude = strings.ToLower(strings.Trim(filepath.ToSlash(exclude), "/"))
	for _, protected := range protectedSyncExcludes() {
		if exclude == protected {
			return rel == protected || strings.HasPrefix(rel, protected+"/")
		}
	}
	return false
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

type gitCoherencePlan struct{ RemoteURL, Target, Tree, Branch string }

func (p gitCoherencePlan) seedEnabled() bool {
	return p.RemoteURL != "" && normalizeGitRemoteURL(p.RemoteURL) == p.RemoteURL && p.Target != "" && p.Branch != ""
}

func (p gitCoherencePlan) enabled() bool { return p.seedEnabled() && p.Tree != "" }

func syncGitCoherencePlan(cfg Config, repo Repo) (gitCoherencePlan, bool) {
	if !cfg.Sync.GitSeed || len(syncIncludes(cfg)) != 0 || repo.Root == "" || repo.RemoteURL == "" || repo.Head == "" {
		return gitCoherencePlan{}, false
	}
	if gitRemoteURLHasCredentials(repo.RemoteURL) {
		return gitCoherencePlan{}, true
	}
	target := gitOutput(repo.Root, "rev-parse", "--verify", repo.Head+"^{commit}")
	tree := gitOutput(repo.Root, "rev-parse", "--verify", repo.Head+"^{tree}")
	branch := originBranchForTarget(repo.Root, repo.BaseRef, target)
	plan := gitCoherencePlan{RemoteURL: normalizeGitRemoteURL(repo.RemoteURL), Target: target, Branch: branch}
	if target == "" || target != repo.Head || branch == "" {
		return gitCoherencePlan{}, false
	}
	overlayOnly, err := gitTargetRequiresOverlayOnly(repo.Root, target)
	if tree == "" || err != nil || overlayOnly {
		return plan, false
	}
	plan.Tree = tree
	return plan, false
}

func originBranchForTarget(root, baseRef, target string) string {
	if target == "" {
		return ""
	}
	out := gitOutput(root, "for-each-ref", "--contains="+target, "--sort=refname", "--format=%(refname)", "refs/remotes/origin")
	eligibleRefs := strings.Split(out, "\n")
	if branch := normalizedOriginBranch(baseRef); branch != "" &&
		slices.Contains(eligibleRefs, "refs/remotes/origin/"+branch) {
		return branch
	}
	originHead := gitOutput(root, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
	if branch := normalizedOriginBranch(originHead); branch != "" &&
		slices.Contains(eligibleRefs, "refs/remotes/origin/"+branch) {
		return branch
	}
	for _, line := range eligibleRefs {
		if branch := normalizedOriginBranch(line); branch != "" {
			return branch
		}
	}
	return ""
}

func normalizedOriginBranch(ref string) string {
	ref = strings.TrimSpace(ref)
	switch {
	case strings.HasPrefix(ref, "refs/remotes/origin/"):
		ref = strings.TrimPrefix(ref, "refs/remotes/origin/")
	case strings.HasPrefix(ref, "refs/heads/"):
		ref = strings.TrimPrefix(ref, "refs/heads/")
	case strings.HasPrefix(ref, "origin/"):
		ref = strings.TrimPrefix(ref, "origin/")
	}
	if ref == "" || ref == "HEAD" {
		return ""
	}
	cmd := exec.Command("git", "check-ref-format", "--branch", ref)
	cmd.Env = repositoryGitEnvironment()
	if err := cmd.Run(); err != nil {
		return ""
	}
	return ref
}

func gitTargetRequiresOverlayOnly(root, target string) (bool, error) {
	cmd := exec.Command("git", "ls-tree", "-r", "-z", "--full-tree", target)
	cmd.Dir = root
	cmd.Env = repositoryGitEnvironment()
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	var paths bytes.Buffer
	for _, entry := range bytes.Split(out, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		metadata, name, ok := bytes.Cut(entry, []byte{'\t'})
		if !ok {
			return false, fmt.Errorf("parse target tree")
		}
		if bytes.HasPrefix(metadata, []byte("160000 ")) {
			return true, nil
		}
		paths.Write(name)
		paths.WriteByte(0)
	}
	if paths.Len() == 0 {
		return false, nil
	}
	cmd = exec.Command("git", "check-attr", "--source="+target, "-z", "--stdin", "filter")
	cmd.Dir = root
	cmd.Env = repositoryGitEnvironment()
	cmd.Stdin = bytes.NewReader(paths.Bytes())
	out, err = cmd.Output()
	if err != nil {
		return false, err
	}
	fields := bytes.Split(out, []byte{0})
	for i := 2; i < len(fields); i += 3 {
		value := string(fields[i])
		if value != "" && value != "unspecified" && value != "unset" {
			return true, nil
		}
	}
	return false, nil
}

func warnCredentialBearingGitSeed(w io.Writer) {
	fmt.Fprintln(w, "warning: git seed disabled because origin URL contains embedded credentials; continuing with file sync without forwarding the remote URL")
}

func SyncExcludes(root string, cfg Config) (SyncExcludeRules, error) {
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
	cmd.Env = repositoryGitEnvironment()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitRemoteURLHasCredentials(remoteURL string) bool {
	raw := strings.TrimSpace(remoteURL)
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd <= 0 {
		return false
	}
	scheme := strings.ToLower(raw[:schemeEnd])
	if parsed, err := url.Parse(raw); err == nil && parsed.User != nil {
		if scheme == "http" || scheme == "https" {
			return true
		}
		_, hasPassword := parsed.User.Password()
		return hasPassword
	}
	authority := raw[schemeEnd+len("://"):]
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	userinfoEnd := strings.LastIndex(authority, "@")
	if userinfoEnd < 0 {
		return false
	}
	if scheme == "http" || scheme == "https" {
		return true
	}
	return strings.Contains(authority[:userinfoEnd], ":")
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

func syncFingerprintForManifest(repo Repo, cfg Config, manifest SyncManifest, excludes SyncExcludeRules, plan gitCoherencePlan) (string, error) {
	if !plan.enabled() {
		return "", nil
	}
	h := sha256.New()
	if cfg.Sync.GitOverlay {
		fmt.Fprintf(h, "v1-overlay\nremote=%s\nbranch=%s\nhead=%s\ntree=%s\n", plan.RemoteURL, plan.Branch, plan.Target, plan.Tree)
		fmt.Fprintf(h, "delete=%t\nchecksum=%t\ngitOverlay=true\n", cfg.Sync.Delete, cfg.Sync.Checksum)
	} else {
		fmt.Fprintf(h, "v6\nremote=%s\nbranch=%s\nhead=%s\ntree=%s\n", plan.RemoteURL, plan.Branch, plan.Target, plan.Tree)
		fmt.Fprintf(h, "delete=%t\nchecksum=%t\n", cfg.Sync.Delete, cfg.Sync.Checksum)
	}
	fmt.Fprintf(h, "manifest=%x\n", sha256.Sum256(manifest.NUL()))
	fmt.Fprintf(h, "deleted=%x\n", sha256.Sum256(manifest.DeletedNUL()))
	for _, exclude := range excludes.rules {
		fmt.Fprintf(h, "exclude=%d:%s\n", exclude.origin, exclude.pattern)
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
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(full)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(h, "symlink=%s\n", target)
			h.Write([]byte{0})
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
	Files                    []string
	Deleted                  []string
	Changed                  []string
	OverlayFiles             []string
	ProtectedTrackedExcludes []SyncProtectedTrackedExclude
	Bytes                    int64
	ChangedBytes             int64
	OverlayBytes             int64
}

type SyncProtectedTrackedExclude struct {
	Path    string `json:"path"`
	Pattern string `json:"pattern"`
}

// gitManifestError turns a raw git ls-files failure into an actionable,
// workdir-aware diagnostic.
func gitManifestError(root string, err error) error {
	stderr := ""
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		stderr = strings.TrimSpace(string(exitErr.Stderr))
	}
	if isNotAGitRepoError(stderr) {
		return fmt.Errorf("%s is not a Git repository: crabbox builds its sync manifest from Git; run `git init` here to sync files, or pass --no-sync to run without syncing local files", root)
	}
	if stderr != "" {
		return fmt.Errorf("git ls-files in %s failed: %s", root, firstLine(stderr))
	}
	return fmt.Errorf("git ls-files in %s failed: %w", root, err)
}

func isNotAGitRepoError(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "not a git repository")
}

func gitSyncFileList(root string) ([]byte, error) {
	if !explicitGitRepositoryRouting() {
		boundary, err := nearestRepositoryBoundary(root, "")
		if err != nil {
			return nil, err
		}
		if boundary.kind == repositoryBoundaryNativeJujutsu {
			return nil, fmt.Errorf("%s is a native Jujutsu workspace without colocated Git metadata: Crabbox sync is Git-manifest-based, and native Jujutsu sync is not supported yet because it risks syncing the wrong revision; use a colocated Git workspace instead (for example, from an existing Git checkout, initialize Jujutsu with `jj git init --git-repo=.`; this does not convert the current native workspace in place), or pass --no-sync to run without syncing local files", boundary.root)
		}
	}
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	cmd.Dir = root
	cmd.Env = repositoryGitEnvironment()
	out, err := cmd.Output()
	if err != nil {
		return nil, gitManifestError(root, err)
	}
	return out, nil
}

func validateLocalWorkspaceSyncSource(repo Repo) error {
	if _, err := gitSyncFileList(repo.Root); err != nil {
		return exit(6, "build sync file list: %v", err)
	}
	return nil
}

func syncManifest(root string, excludes SyncExcludeRules) (SyncManifest, error) {
	return syncManifestFilteredRules(root, excludes, nil)
}

// syncManifestFiltered builds the sync manifest applying excludes and, when
// includes is non-empty, a whitelist: only paths matching an include pattern are
// synced. This lets a job sync a few selected paths instead of the whole working
// tree (e.g. sync just `src/` and `scripts/` out of a large repo).
func syncManifestFiltered(root string, excludes, includes []string) (SyncManifest, error) {
	return syncManifestFilteredRules(root, newSyncExcludeRules(excludes, syncExcludeConfigured), includes)
}

func syncManifestFilteredRules(root string, excludes SyncExcludeRules, includes []string) (SyncManifest, error) {
	out, err := gitSyncFileList(root)
	if err != nil {
		return SyncManifest{}, err
	}
	tracked, err := loadGitTrackedPaths(root)
	if err != nil {
		return SyncManifest{}, fmt.Errorf("verify sync manifest scope: %w", err)
	}
	trackedRegular := trackedRegularPathSet(tracked)
	inManifestScope := func(entry gitTrackedPath) bool {
		rel := filepath.ToSlash(entry.name)
		return safeRepoRel(rel) &&
			!pathExcludedByRules(rel, excludes, gitModeIsRegular(entry.mode)) &&
			pathIncluded(rel, includes)
	}
	gitlinkPaths, err := trackedGitlinkPaths(tracked, inManifestScope)
	if err != nil {
		return SyncManifest{}, fmt.Errorf("verify sync manifest scope: %w", err)
	}
	hidden, err := gitCheckoutHiddenOmissionForTracked(root, tracked, gitCheckoutSparseEnabled(root), func(entry gitTrackedPath) bool {
		return entry.mode != "160000" && inManifestScope(entry)
	}, sparseCheckoutIncludedPaths)
	if err != nil {
		return SyncManifest{}, fmt.Errorf("verify sync manifest scope: %w", err)
	}
	if hidden != "" {
		return SyncManifest{}, fmt.Errorf("tracked path %q is hidden by sparse checkout or skip-worktree state but remains in sync manifest scope", hidden)
	}
	seen := map[string]bool{}
	manifest := SyncManifest{}
	for _, rel := range splitNul(out) {
		rel = filepath.ToSlash(rel)
		_, isTrackedRegular := trackedRegular[rel]
		excluded, protectedPattern := pathExcludeDecision(rel, excludes, isTrackedRegular)
		if _, isGitlink := gitlinkPaths[rel]; isGitlink || !safeRepoRel(rel) || excluded || !pathIncluded(rel, includes) || seen[rel] {
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
		if protectedPattern != "" {
			manifest.ProtectedTrackedExcludes = append(manifest.ProtectedTrackedExcludes, SyncProtectedTrackedExclude{
				Path:    rel,
				Pattern: protectedPattern,
			})
		}
	}
	sort.Strings(manifest.Files)
	sort.Slice(manifest.ProtectedTrackedExcludes, func(i, j int) bool {
		return manifest.ProtectedTrackedExcludes[i].Path < manifest.ProtectedTrackedExcludes[j].Path
	})
	deleted, deletedGitlinks, deletedRegular, err := syncDeletedPaths(root, excludes, includes, trackedRegular)
	if err != nil {
		return SyncManifest{}, err
	}
	for rel := range deletedGitlinks {
		gitlinkPaths[rel] = struct{}{}
	}
	manifest.Deleted = filterDeletedPaths(deleted, seen, gitlinkPaths)
	for rel := range deletedRegular {
		trackedRegular[rel] = struct{}{}
	}
	changed, err := changedSyncPaths(root, excludes, includes, trackedRegular)
	if err != nil {
		return SyncManifest{}, err
	}
	manifest.Changed, manifest.ChangedBytes = changedPathSetBytes(root, changed)
	manifest.OverlayFiles, manifest.OverlayBytes = overlayPathSetBytes(root, manifest.Files, manifest.Changed)
	return manifest, nil
}

func trackedRegularPathSet(tracked []gitTrackedPath) map[string]struct{} {
	paths := make(map[string]struct{})
	for _, entry := range tracked {
		if gitModeIsRegular(entry.mode) {
			paths[filepath.ToSlash(entry.name)] = struct{}{}
		}
	}
	return paths
}

func gitModeIsRegular(mode string) bool {
	return mode == "100644" || mode == "100755"
}

func trackedGitlinkPaths(tracked []gitTrackedPath, inScope func(gitTrackedPath) bool) (map[string]struct{}, error) {
	paths := make(map[string]struct{})
	firstByPath := make(map[string]gitTrackedPath)
	for _, entry := range tracked {
		if inScope != nil && !inScope(entry) {
			continue
		}
		rel := filepath.ToSlash(entry.name)
		first, seen := firstByPath[rel]
		if !seen {
			firstByPath[rel] = entry
			if entry.mode == "160000" {
				paths[rel] = struct{}{}
			}
			continue
		}
		if (first.mode == "160000") == (entry.mode == "160000") {
			continue
		}
		file, gitlink := first, entry
		if first.mode == "160000" {
			file, gitlink = entry, first
		}
		return nil, fmt.Errorf(
			"tracked path %q has mixed file mode %s at stage %d and gitlink mode 160000 at stage %d in sync manifest scope",
			rel,
			file.mode,
			file.stage,
			gitlink.stage,
		)
	}
	return paths, nil
}

func BuildSyncManifestFiltered(root string, excludes SyncExcludeRules, includes []string) (SyncManifest, error) {
	return syncManifestFilteredRules(root, excludes, includes)
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

func (m SyncManifest) OverlayNUL() []byte {
	var b bytes.Buffer
	for _, rel := range m.OverlayFiles {
		b.WriteString(rel)
		b.WriteByte(0)
	}
	return b.Bytes()
}

type gitCachedDeletion struct {
	name         string
	preimageMode string
}

func syncDeletedPaths(root string, excludes SyncExcludeRules, includes []string, trackedRegular map[string]struct{}) ([]string, map[string]struct{}, map[string]struct{}, error) {
	cmd := exec.Command("git", "ls-files", "--deleted", "-z")
	cmd.Dir = root
	cmd.Env = repositoryGitEnvironment()
	worktreeOut, err := cmd.Output()
	if err != nil {
		return nil, nil, nil, err
	}
	cached, err := loadGitCachedDeletions(root)
	if err != nil {
		return nil, nil, nil, err
	}
	seen := map[string]bool{}
	gitlinks := map[string]struct{}{}
	regular := map[string]struct{}{}
	inScope := func(rel string, isRegular bool) bool {
		return safeRepoRel(rel) && !pathExcludedByRules(rel, excludes, isRegular) && pathIncluded(rel, includes)
	}
	for _, rel := range splitNul(worktreeOut) {
		rel = filepath.ToSlash(rel)
		_, isRegular := trackedRegular[rel]
		if inScope(rel, isRegular) {
			seen[rel] = true
			if isRegular {
				regular[rel] = struct{}{}
			}
		}
	}
	for _, deletion := range cached {
		rel := filepath.ToSlash(deletion.name)
		isRegular := gitModeIsRegular(deletion.preimageMode)
		if !inScope(rel, isRegular) {
			continue
		}
		seen[rel] = true
		if deletion.preimageMode == "160000" {
			gitlinks[rel] = struct{}{}
		}
		if isRegular {
			regular[rel] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for rel := range seen {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, gitlinks, regular, nil
}

func loadGitCachedDeletions(root string) ([]gitCachedDeletion, error) {
	cmd := exec.Command("git", "diff", "--cached", "--raw", "--format=", "-z", "--diff-filter=D", "--no-renames")
	cmd.Dir = root
	cmd.Env = repositoryGitEnvironment()
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseGitCachedDeletions(out)
}

func trackedRegularPathSetWithCachedDeletions(root string, tracked []gitTrackedPath) (map[string]struct{}, error) {
	paths := trackedRegularPathSet(tracked)
	deletions, err := loadGitCachedDeletions(root)
	if err != nil {
		return nil, err
	}
	for _, deletion := range deletions {
		if gitModeIsRegular(deletion.preimageMode) {
			paths[filepath.ToSlash(deletion.name)] = struct{}{}
		}
	}
	return paths, nil
}

func parseGitCachedDeletions(raw []byte) ([]gitCachedDeletion, error) {
	if len(raw) > 0 && raw[len(raw)-1] != 0 {
		return nil, fmt.Errorf("parse staged deletion metadata")
	}
	records := bytes.Split(raw, []byte{0})
	if len(records) > 0 && len(records[len(records)-1]) == 0 {
		records = records[:len(records)-1]
	}
	if len(records)%2 != 0 {
		return nil, fmt.Errorf("parse staged deletion metadata")
	}
	deletions := make([]gitCachedDeletion, 0, len(records)/2)
	for i := 0; i < len(records); i += 2 {
		fields := bytes.Fields(records[i])
		if len(fields) != 5 || len(fields[0]) < 2 || fields[0][0] != ':' || string(fields[4]) != "D" || len(records[i+1]) == 0 {
			return nil, fmt.Errorf("parse staged deletion metadata")
		}
		mode := string(fields[0][1:])
		if _, err := strconv.ParseUint(mode, 8, 32); err != nil {
			return nil, fmt.Errorf("parse staged deletion mode %q", mode)
		}
		deletions = append(deletions, gitCachedDeletion{
			name:         string(records[i+1]),
			preimageMode: mode,
		})
	}
	return deletions, nil
}

func filterDeletedPaths(deleted []string, files map[string]bool, gitlinks map[string]struct{}) []string {
	out := deleted[:0]
	for _, rel := range deleted {
		if _, isGitlink := gitlinks[rel]; !isGitlink && !files[rel] {
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

func overlayPathSetBytes(root string, files, changed []string) ([]string, int64) {
	fileSet := make(map[string]struct{}, len(files))
	for _, rel := range files {
		fileSet[rel] = struct{}{}
	}
	overlay := make([]string, 0, len(changed))
	for _, rel := range changed {
		if _, ok := fileSet[rel]; ok {
			overlay = append(overlay, rel)
		}
	}
	return changedPathSetBytes(root, overlay)
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
	guard := evaluateSyncGuardrail(manifest, cfg, force)
	printProtectedTrackedExcludes(stderr, manifest)
	if len(manifest.Changed) > 0 {
		fmt.Fprintf(stderr, "sync candidate: %d files, %s dirty_delta=%d files, %s\n", fileCount, humanBytes(manifest.Bytes), len(manifest.Changed), humanBytes(manifest.ChangedBytes))
	} else {
		fmt.Fprintf(stderr, "sync candidate: %d files, %s\n", fileCount, humanBytes(manifest.Bytes))
	}
	for _, reason := range guard.Reasons {
		if reason.Status != "failed" {
			continue
		}
		printSyncTopDirs(stderr, guard.Paths)
		if reason.Metric == "files" {
			return exit(6, "sync %s too large: %d files >= limit %d; use --force-sync-large or CRABBOX_SYNC_ALLOW_LARGE=1", guard.Scope, reason.Actual, reason.Limit)
		}
		return exit(6, "sync %s too large: %s >= limit %s; use --force-sync-large or CRABBOX_SYNC_ALLOW_LARGE=1", guard.Scope, humanBytes(reason.Actual), humanBytes(reason.Limit))
	}
	warned := false
	for _, reason := range guard.Reasons {
		if reason.Status != "warning" {
			continue
		}
		if reason.Metric == "files" {
			fmt.Fprintf(stderr, "warning: large sync %s: %d files >= warning threshold %d\n", guard.Scope, reason.Actual, reason.Limit)
		} else {
			fmt.Fprintf(stderr, "warning: large sync %s: %s >= warning threshold %s\n", guard.Scope, humanBytes(reason.Actual), humanBytes(reason.Limit))
		}
		warned = true
	}
	if warned {
		printSyncTopDirs(stderr, guard.Paths)
	}
	return nil
}

func printProtectedTrackedExcludes(w io.Writer, manifest SyncManifest) {
	if w == nil || len(manifest.ProtectedTrackedExcludes) == 0 {
		return
	}
	limit := len(manifest.ProtectedTrackedExcludes)
	if limit > protectedTrackedExcludeExampleLimit {
		limit = protectedTrackedExcludeExampleLimit
	}
	items := make([]string, 0, limit)
	for _, protected := range manifest.ProtectedTrackedExcludes[:limit] {
		items = append(items, fmt.Sprintf("%s (%s)", protected.Path, protected.Pattern))
	}
	more := ""
	if remaining := len(manifest.ProtectedTrackedExcludes) - limit; remaining > 0 {
		more = fmt.Sprintf(" (+%d more)", remaining)
	}
	fmt.Fprintf(w, "warning: protected %d tracked files from built-in artifact excludes: %s%s\n", len(manifest.ProtectedTrackedExcludes), strings.Join(items, ", "), more)
}

type syncGuardrailEvaluation struct {
	Count      int
	Bytes      int64
	Scope      string
	Paths      []string
	AllowLarge bool
	Status     string
	Reasons    []syncGuardrailReason
}

type syncGuardrailReason struct {
	Status string
	Metric string
	Actual int64
	Limit  int64
}

func evaluateSyncGuardrail(manifest SyncManifest, cfg Config, force bool) syncGuardrailEvaluation {
	count, bytes, scope, paths := syncGuardrailScope(manifest)
	out := syncGuardrailEvaluation{
		Count:      count,
		Bytes:      bytes,
		Scope:      scope,
		Paths:      paths,
		AllowLarge: force || cfg.Sync.AllowLarge || os.Getenv("CRABBOX_SYNC_ALLOW_LARGE") == "1",
		Status:     "ok",
	}
	if !out.AllowLarge {
		if cfg.Sync.FailFiles > 0 && count >= cfg.Sync.FailFiles {
			out.addReason("failed", "files", int64(count), int64(cfg.Sync.FailFiles))
		}
		if cfg.Sync.FailBytes > 0 && bytes >= cfg.Sync.FailBytes {
			out.addReason("failed", "bytes", bytes, cfg.Sync.FailBytes)
		}
	}
	if cfg.Sync.WarnFiles > 0 && count >= cfg.Sync.WarnFiles {
		out.addReason("warning", "files", int64(count), int64(cfg.Sync.WarnFiles))
	}
	if cfg.Sync.WarnBytes > 0 && bytes >= cfg.Sync.WarnBytes {
		out.addReason("warning", "bytes", bytes, cfg.Sync.WarnBytes)
	}
	return out
}

func (g *syncGuardrailEvaluation) addReason(status, metric string, actual, limit int64) {
	if status == "failed" || g.Status == "ok" {
		g.Status = status
	}
	g.Reasons = append(g.Reasons, syncGuardrailReason{
		Status: status,
		Metric: metric,
		Actual: actual,
		Limit:  limit,
	})
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

func changedSyncPaths(root string, excludes SyncExcludeRules, includes []string, trackedRegular map[string]struct{}) ([]string, error) {
	sets := [][]string{}
	for _, args := range [][]string{
		{"diff", "--name-only", "-z"},
		{"diff", "--cached", "--name-only", "-z"},
		{"ls-files", "--others", "--exclude-standard", "-z"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = repositoryGitEnvironment()
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
			_, isTrackedRegular := trackedRegular[rel]
			if rel == "" || pathExcludedByRules(rel, excludes, isTrackedRegular) || !pathIncluded(rel, includes) {
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
	rules := newSyncExcludeRules(excludes, syncExcludeConfigured)
	excluded, _ := pathExcludeDecision(rel, rules, false)
	return excluded
}

func pathExcludedByRules(rel string, rules SyncExcludeRules, trackedRegular bool) bool {
	excluded, _ := pathExcludeDecision(rel, rules, trackedRegular)
	return excluded
}

func pathExcludeDecision(rel string, rules SyncExcludeRules, trackedRegular bool) (bool, string) {
	rel = filepath.ToSlash(rel)
	excluded := false
	protectedPattern := ""
	for _, rule := range rules.rules {
		exclude, negated := excludeRule(rule.pattern)
		if excludeMatches(rel, exclude) || protectedSyncExcludeMatches(rel, exclude) {
			if trackedRegular && rule.origin == syncExcludeBuiltIn && !negated && isAmbiguousBuiltInExclude(rule.pattern) {
				protectedPattern = exclude
				continue
			}
			excluded = !negated
			protectedPattern = ""
		}
	}
	if excluded {
		protectedPattern = ""
	}
	return excluded, protectedPattern
}

func excludeRule(rule string) (pattern string, negated bool) {
	rule = strings.TrimSpace(rule)
	if strings.HasPrefix(rule, `\!`) {
		return strings.TrimPrefix(rule, `\`), false
	}
	if strings.HasPrefix(rule, "!") {
		return strings.TrimSpace(strings.TrimPrefix(rule, "!")), true
	}
	return rule, false
}

// excludedDirMayContainReinclude keeps watch traversal open when a later file
// can be re-included below an otherwise excluded directory.
func excludedDirMayContainReinclude(rel string, excludes []string) bool {
	rel = strings.Trim(filepath.ToSlash(rel), "/")
	for _, rule := range excludes {
		pattern, negated := excludeRule(rule)
		if !negated {
			continue
		}
		pattern = strings.Trim(filepath.ToSlash(pattern), "/")
		if pattern == "" {
			continue
		}
		if !strings.Contains(pattern, "/") {
			return true
		}
		meta := strings.IndexAny(pattern, "*?[")
		if meta < 0 {
			if pattern == rel || strings.HasPrefix(pattern, rel+"/") || strings.HasPrefix(rel, pattern+"/") {
				return true
			}
			continue
		}
		prefix := strings.TrimSuffix(pattern[:meta], "/")
		if prefix == "" || prefix == rel || strings.HasPrefix(prefix, rel+"/") || strings.HasPrefix(rel, prefix) {
			return true
		}
	}
	return false
}

func excludeMatches(rel string, exclude string) bool {
	parts := strings.Split(rel, "/")
	exclude = strings.Trim(filepath.ToSlash(strings.TrimSpace(exclude)), "/")
	if exclude == "" {
		return false
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
