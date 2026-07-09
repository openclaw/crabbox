package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

type watchRunExecutor func(ctx context.Context, runArgs []string) error

var watchForbiddenRunFlags = map[string]bool{
	"pool":              true,
	"pool-return":       true,
	"sync-only":         true,
	"no-sync":           true,
	"apply-local-patch": true,
	"fresh-pr":          true,
	"script":            true,
	"script-stdin":      true,
	"stop-after":        true,
	"lease-output":      true,
	"keep-on-failure":   true,
	"capture-stdout":    true,
	"capture-stderr":    true,
	"download":          true,
	"emit-proof":        true,
	"proof-template":    true,
	"attest":            true,
	"attest-key":        true,
	"label":             true,
}

var watchOwnedOnlyFlags = map[string]bool{
	"id":        true,
	"keep":      true,
	"debounce":  true,
	"idle-exit": true,
	"slug":      true,
}

var watchFilterSourceFiles = map[string]bool{
	".crabboxignore": true,
	"crabbox.yaml":   true,
	".crabbox.yaml":  true,
}

type watchOptions struct {
	LeaseID        string
	Keep           bool
	Reclaim        bool
	Debounce       time.Duration
	IdleExit       time.Duration
	IdleExitSet    bool
	IdleTimeoutSet bool
	RequestedSlug  string
	RunArgs        []string
	Command        []string
}

func (a App) watch(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("watch", a.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  crabbox watch [flags] -- <command...>")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Runs the command once on a warm lease, then watches the repository and")
		fmt.Fprintln(fs.Output(), "re-runs it when synced files change. Reruns queue while a run is active;")
		fmt.Fprintln(fs.Output(), "at most one rerun is pending. Requires an SSH lease provider with the")
		fmt.Fprintln(fs.Output(), "crabbox-sync feature. Other run flags pass through to each iteration.")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
	}
	leaseFlags := registerLeaseCreateFlags(fs, defaults)
	leaseIDFlag := fs.String("id", "", "existing lease or server id")
	keep := fs.Bool("keep", false, "keep an acquired lease when the watch loop exits")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	preset := fs.String("preset", "", "configured profile preset that provides the watched command")
	debounce := fs.Duration("debounce", 250*time.Millisecond, "quiet period before a change batch triggers a rerun")
	idleExit := fs.Duration("idle-exit", 0, "exit after this period without qualifying local changes; defaults to the lease idle timeout")
	flagArgs, command := splitWatchArgs(args)
	watchArgs, runArgs, err := partitionWatchRunArgs(fs, flagArgs)
	if err != nil {
		return err
	}
	if err := parseFlags(fs, watchArgs); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return exit(2, "unexpected argument %q; place the command after --", fs.Arg(0))
	}
	if len(command) == 0 && strings.TrimSpace(*preset) == "" {
		return exit(2, "usage: crabbox watch [flags] -- <command...>")
	}
	if *debounce <= 0 {
		return exit(2, "--debounce must be positive")
	}
	requestedSlug, err := requestedLeaseSlug(*leaseFlags.Slug)
	if err != nil {
		return err
	}
	if requestedSlug != "" && strings.TrimSpace(*leaseIDFlag) != "" {
		return exit(2, "--slug only applies when creating a new lease; omit --id or use the existing slug")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Profile = *leaseFlags.Profile
	if err := applySelectedProfileConfig(&cfg); err != nil {
		return err
	}
	if err := applyLeaseCreateFlagsForLease(&cfg, fs, leaseFlags, *leaseIDFlag); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	sshBackend, err := watchBackendGate(backend)
	if err != nil {
		return err
	}
	opts := watchOptions{
		LeaseID:        strings.TrimSpace(*leaseIDFlag),
		Keep:           *keep,
		Reclaim:        *reclaim,
		Debounce:       *debounce,
		IdleExit:       *idleExit,
		IdleExitSet:    flagWasSet(fs, "idle-exit"),
		IdleTimeoutSet: flagWasSet(fs, "idle-timeout"),
		RequestedSlug:  requestedSlug,
		RunArgs:        runArgs,
		Command:        command,
	}
	return a.watchWithBackend(ctx, opts, repo, cfg, sshBackend, func(runCtx context.Context, iterationArgs []string) error {
		return a.runCommand(runCtx, iterationArgs)
	})
}

func watchBackendGate(backend Backend) (SSHLeaseBackend, error) {
	sshBackend, ok := backend.(SSHLeaseBackend)
	if !ok || !backend.Spec().Features.Has(FeatureCrabboxSync) {
		return nil, exit(2, "provider=%s does not support watch: it requires an SSH lease provider with the crabbox-sync feature; use crabbox run instead", backend.Spec().Name)
	}
	return sshBackend, nil
}

func (a App) watchWithBackend(ctx context.Context, opts watchOptions, repo Repo, cfg Config, backend SSHLeaseBackend, execute watchRunExecutor) error {
	options := leaseOptionsFromConfig(cfg)
	if opts.LeaseID != "" {
		lease, err := resolveSSHLeaseTarget(ctx, backend, ResolveRequest{Repo: repo, Options: options, ID: opts.LeaseID, Reclaim: opts.Reclaim})
		if err != nil {
			return err
		}
		if !opts.IdleTimeoutSet {
			if duration, ok := parseDurationSecondsLabel(lease.Server.Labels["idle_timeout_secs"]); ok {
				cfg.IdleTimeout = duration
			} else if duration, ok := parseDurationSecondsLabel(lease.Server.Labels["idle_timeout"]); ok {
				cfg.IdleTimeout = duration
			}
		}
		idleExit, err := watchEffectiveIdleExit(opts, cfg.IdleTimeout)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "watch lease=%s slug=%s provider=%s idle_timeout=%s idle_exit=%s\n", lease.LeaseID, blank(serverSlug(lease.Server), "-"), cfg.Provider, cfg.IdleTimeout, idleExit)
		return a.watchLoop(ctx, opts, repo, cfg, lease.LeaseID, idleExit, execute)
	}
	idleExit, err := watchEffectiveIdleExit(opts, cfg.IdleTimeout)
	if err != nil {
		return err
	}
	lease, err := backend.Acquire(ctx, AcquireRequest{Repo: repo, Options: options, Keep: opts.Keep, Reclaim: opts.Reclaim, RequestedSlug: opts.RequestedSlug})
	if err != nil {
		return err
	}
	if !opts.Keep {
		defer func() {
			if releaseErr := a.releaseBackendLeaseBestEffort(context.Background(), backend, cfg, lease); releaseErr != nil {
				fmt.Fprintf(a.Stderr, "warning: watch lease release failed for %s: %v\n", lease.LeaseID, releaseErr)
			}
		}()
	}
	applyResolvedServerConfig(&cfg, lease.Server)
	if err := a.claimLeaseTargetForRepoAndRegister(ctx, lease.LeaseID, serverSlug(lease.Server), cfg, lease.Server, lease.SSH, repo.Root, opts.Reclaim); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "leased %s slug=%s provider=%s idle_timeout=%s idle_exit=%s\n", lease.LeaseID, blank(serverSlug(lease.Server), "-"), cfg.Provider, cfg.IdleTimeout, idleExit)
	return a.watchLoop(ctx, opts, repo, cfg, lease.LeaseID, idleExit, execute)
}

func watchEffectiveIdleExit(opts watchOptions, idleTimeout time.Duration) (time.Duration, error) {
	if idleTimeout <= 0 {
		return 0, exit(2, "lease idle timeout must be positive")
	}
	if !opts.IdleExitSet {
		return idleTimeout, nil
	}
	if opts.IdleExit <= 0 || opts.IdleExit > idleTimeout {
		return 0, exit(2, "--idle-exit must be positive and at most the lease idle timeout (%s); raise --idle-timeout for longer sessions", idleTimeout)
	}
	return opts.IdleExit, nil
}

func (a App) watchLoop(ctx context.Context, opts watchOptions, repo Repo, cfg Config, leaseID string, idleExit time.Duration, execute watchRunExecutor) error {
	session := &watchSession{
		root:     repo.Root,
		leaseID:  leaseID,
		runArgs:  opts.RunArgs,
		command:  opts.Command,
		debounce: opts.Debounce,
		idleExit: idleExit,
		cfg:      cfg,
		execute:  execute,
		stderr:   a.Stderr,
	}
	return session.run(ctx)
}

type watchSession struct {
	root     string
	leaseID  string
	runArgs  []string
	command  []string
	debounce time.Duration
	idleExit time.Duration
	cfg      Config
	execute  watchRunExecutor
	stderr   io.Writer
	excludes []string
	watcher  *fsnotify.Watcher
	paths    map[string]watchPathState
}

type watchPathState struct {
	mode    fs.FileMode
	size    int64
	modTime int64
}

func (s *watchSession) run(ctx context.Context) error {
	excludes, err := syncExcludes(s.root, s.cfg)
	if err != nil {
		return err
	}
	s.excludes = excludes
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return exit(1, "watch: start watcher: %v", err)
	}
	defer watcher.Close()
	s.watcher = watcher
	files, err := addWatchTree(watcher, s.root, s.root, s.excludes)
	if err != nil {
		return exit(1, "watch: watch %s: %v", s.root, err)
	}
	s.paths = make(map[string]watchPathState, len(files))
	for _, rel := range files {
		s.rememberPath(rel, filepath.Join(s.root, filepath.FromSlash(rel)))
	}
	fmt.Fprintf(s.stderr, "watch root=%s debounce=%s idle_exit=%s\n", s.root, s.debounce, s.idleExit)

	runDone := make(chan error, 1)
	running := false
	pending := false
	idleExiting := false
	iteration := 0
	pendingChanges := 0
	batch := map[string]struct{}{}
	iterCancel := context.CancelFunc(func() {})
	defer func() { iterCancel() }()
	startRun := func(changes int) {
		iteration++
		iterCancel()
		iterCtx, cancel := context.WithCancel(ctx)
		iterCancel = cancel
		iterationArgs := watchIterationArgs(s.leaseID, iteration, s.runArgs, s.command)
		if iteration > 1 {
			fmt.Fprintf(s.stderr, "watch run=%d changes=%d\n", iteration, changes)
		}
		running = true
		go func() {
			runDone <- s.execute(iterCtx, iterationArgs)
		}()
	}
	awaitRun := func() {
		if !running {
			return
		}
		iterCancel()
		<-runDone
		running = false
	}

	debounceTimer := time.NewTimer(s.debounce)
	debounceTimer.Stop()
	defer debounceTimer.Stop()
	idleTimer := time.NewTimer(s.idleExit)
	defer idleTimer.Stop()
	startRun(0)

	for {
		select {
		case <-ctx.Done():
			awaitRun()
			fmt.Fprintf(s.stderr, "watch interrupted runs=%d\n", iteration)
			return nil
		case watchErr, ok := <-watcher.Errors:
			awaitRun()
			if !ok {
				return exit(1, "watch: watcher closed unexpectedly")
			}
			return exit(1, "watch: watcher failed: %v; add noisy paths to .crabboxignore if event volume is the cause", watchErr)
		case event, ok := <-watcher.Events:
			if !ok {
				awaitRun()
				return exit(1, "watch: watcher closed unexpectedly")
			}
			if s.observeEvent(watcher, event, batch) {
				debounceTimer.Reset(s.debounce)
			}
		case <-debounceTimer.C:
			paths := make([]string, 0, len(batch))
			for path := range batch {
				paths = append(paths, path)
			}
			clear(batch)
			qualified, err := s.qualifyBatch(paths)
			if err != nil {
				awaitRun()
				return err
			}
			if len(qualified) == 0 {
				continue
			}
			idleTimer.Reset(s.idleExit)
			idleExiting = false
			if running {
				pending = true
				pendingChanges += len(qualified)
			} else {
				startRun(len(qualified))
			}
		case runErr := <-runDone:
			running = false
			iterCancel()
			if runErr != nil {
				if ctx.Err() != nil {
					fmt.Fprintf(s.stderr, "watch interrupted runs=%d\n", iteration)
					return nil
				}
				if !isWatchIterationResult(runErr) {
					return runErr
				}
				fmt.Fprintf(s.stderr, "watch run=%d result=nonzero watching for changes\n", iteration)
			}
			if pending {
				pending = false
				changes := pendingChanges
				pendingChanges = 0
				startRun(changes)
			} else if idleExiting {
				fmt.Fprintf(s.stderr, "watch idle_exit=%s runs=%d\n", s.idleExit, iteration)
				return nil
			}
		case <-idleTimer.C:
			if running {
				idleExiting = true
				continue
			}
			fmt.Fprintf(s.stderr, "watch idle_exit=%s runs=%d\n", s.idleExit, iteration)
			return nil
		}
	}
}

func (s *watchSession) observeEvent(watcher *fsnotify.Watcher, event fsnotify.Event, batch map[string]struct{}) bool {
	rel, err := filepath.Rel(s.root, event.Name)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || !safeRepoRel(rel) {
		return false
	}
	excluded := pathExcluded(rel, s.excludes)
	if excluded {
		if !excludedDirMayContainReinclude(rel, s.excludes) {
			return false
		}
		if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
			changed := false
			for path := range s.paths {
				if strings.HasPrefix(path, rel+"/") && !pathExcluded(path, s.excludes) {
					delete(s.paths, path)
					batch[path] = struct{}{}
					changed = true
				}
			}
			return changed
		}
		if event.Op&fsnotify.Create == 0 {
			return false
		}
		info, err := os.Lstat(event.Name)
		if err != nil || !info.IsDir() {
			return false
		}
		files, err := addWatchTree(watcher, s.root, event.Name, s.excludes)
		if err != nil {
			return false
		}
		for _, file := range files {
			s.rememberPath(file, filepath.Join(s.root, filepath.FromSlash(file)))
			batch[file] = struct{}{}
		}
		return len(files) > 0
	}
	if event.Op == fsnotify.Chmod && !s.rememberPath(rel, event.Name) {
		return false
	}
	if event.Op != fsnotify.Chmod {
		s.rememberPath(rel, event.Name)
	}
	if event.Op&fsnotify.Create != 0 {
		if info, err := os.Lstat(event.Name); err == nil && info.IsDir() {
			if files, err := addWatchTree(watcher, s.root, event.Name, s.excludes); err == nil {
				for _, file := range files {
					s.rememberPath(file, filepath.Join(s.root, filepath.FromSlash(file)))
					batch[file] = struct{}{}
				}
			}
		}
	}
	batch[rel] = struct{}{}
	return true
}

func (s *watchSession) rememberPath(rel, name string) bool {
	if s.paths == nil {
		s.paths = map[string]watchPathState{}
	}
	info, err := os.Lstat(name)
	if err != nil {
		_, existed := s.paths[rel]
		delete(s.paths, rel)
		return existed
	}
	current := watchPathState{mode: info.Mode(), size: info.Size(), modTime: info.ModTime().UnixNano()}
	previous, existed := s.paths[rel]
	s.paths[rel] = current
	return !existed || current != previous
}

func (s *watchSession) qualifyBatch(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	for _, path := range paths {
		if watchFilterSourceFiles[path] {
			cfg, err := loadConfig()
			if err != nil {
				return nil, err
			}
			s.cfg.Sync = cfg.Sync
			break
		}
	}
	excludes, err := syncExcludes(s.root, s.cfg)
	if err != nil {
		return nil, err
	}
	excludesChanged := !slices.Equal(excludes, s.excludes)
	s.excludes = excludes
	if excludesChanged && s.watcher != nil {
		files, err := addWatchTree(s.watcher, s.root, s.root, s.excludes)
		if err != nil {
			return nil, exit(1, "watch: rewatch %s after filter change: %v", s.root, err)
		}
		for _, file := range files {
			s.rememberPath(file, filepath.Join(s.root, filepath.FromSlash(file)))
		}
	}
	includes := syncIncludes(s.cfg)
	existing := make([]string, 0, len(paths))
	missing := make([]string, 0, len(paths))
	for _, path := range paths {
		if !safeRepoRel(path) || pathExcluded(path, s.excludes) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(s.root, filepath.FromSlash(path))); err == nil {
			existing = append(existing, path)
		} else {
			missing = append(missing, path)
		}
	}
	qualified := map[string]struct{}{}
	if len(existing) > 0 {
		universe, err := watchGitPaths(s.root, existing, "ls-files", "--cached", "--others", "--exclude-standard", "-z", "--")
		if err != nil {
			return nil, err
		}
		for _, rel := range universe {
			if safeRepoRel(rel) && !pathExcluded(rel, s.excludes) && pathIncluded(rel, includes) {
				qualified[rel] = struct{}{}
			}
		}
	}
	if len(missing) > 0 {
		tracked, err := watchGitPaths(s.root, missing, "ls-files", "--cached", "-z", "--")
		if err != nil {
			return nil, err
		}
		trackedSet := map[string]struct{}{}
		for _, rel := range tracked {
			trackedSet[rel] = struct{}{}
			if pathIncluded(rel, includes) {
				qualified[rel] = struct{}{}
			}
		}
		untracked := make([]string, 0, len(missing))
		for _, rel := range missing {
			if _, ok := trackedSet[rel]; !ok {
				untracked = append(untracked, rel)
			}
		}
		if len(untracked) > 0 {
			ignored, err := watchGitIgnored(s.root, untracked)
			if err != nil {
				return nil, err
			}
			for _, rel := range untracked {
				if _, ok := ignored[rel]; !ok && pathIncluded(rel, includes) {
					qualified[rel] = struct{}{}
				}
			}
		}
	}
	result := make([]string, 0, len(qualified))
	for rel := range qualified {
		result = append(result, rel)
	}
	sort.Strings(result)
	return result, nil
}

func watchGitPaths(root string, paths []string, gitArgs ...string) ([]string, error) {
	args := make([]string, 0, len(gitArgs)+len(paths)+1)
	args = append(args, "--literal-pathspecs")
	args = append(args, gitArgs...)
	args = append(args, paths...)
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = repositoryGitEnvironment()
	out, err := cmd.Output()
	if err != nil {
		return nil, exit(1, "watch: git %s failed: %s", gitArgs[0], watchGitError(err))
	}
	return splitNul(out), nil
}

func watchGitIgnored(root string, paths []string) (map[string]struct{}, error) {
	cmd := exec.Command("git", "check-ignore", "--stdin", "-z")
	cmd.Dir = root
	cmd.Env = repositoryGitEnvironment()
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return map[string]struct{}{}, nil
		}
		return nil, exit(1, "watch: git check-ignore failed: %s", watchGitError(err))
	}
	ignored := map[string]struct{}{}
	for _, rel := range splitNul(out) {
		ignored[rel] = struct{}{}
	}
	return ignored, nil
}

func watchGitError(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return strings.TrimSpace(string(exitErr.Stderr))
	}
	return err.Error()
}

func watchIterationArgs(leaseID string, iteration int, runArgs, command []string) []string {
	args := make([]string, 0, len(runArgs)+len(command)+5)
	args = append(args, "--id", leaseID, "--label", fmt.Sprintf("watch #%d", iteration))
	args = append(args, runArgs...)
	args = append(args, "--")
	args = append(args, command...)
	return args
}

func isWatchIterationResult(err error) bool {
	var exitErr ExitError
	if !AsExitError(err, &exitErr) {
		return false
	}
	return strings.HasPrefix(exitErr.Message, "remote command exited") || strings.HasPrefix(exitErr.Message, "JUnit results contain")
}

func addWatchTree(watcher *fsnotify.Watcher, root, dir string, excludes []string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel != "." &&
				(!safeRepoRel(rel) ||
					(pathExcluded(rel, excludes) && !excludedDirMayContainReinclude(rel, excludes))) {
				return fs.SkipDir
			}
			if err := watcher.Add(path); err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !safeRepoRel(rel) || pathExcluded(rel, excludes) {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	return files, err
}

func splitWatchArgs(args []string) ([]string, []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

type boolValueFlag interface {
	IsBoolFlag() bool
}

func watchFlagIsBool(fs *flag.FlagSet, name string) bool {
	registered := fs.Lookup(name)
	if registered == nil {
		return false
	}
	value, ok := registered.Value.(boolValueFlag)
	return ok && value.IsBoolFlag()
}

func partitionWatchRunArgs(fs *flag.FlagSet, flagArgs []string) ([]string, []string, error) {
	return partitionForwardedRunArgs(fs, flagArgs, watchOwnedOnlyFlags, watchForbiddenRunFlags, "watch: it conflicts with a persistent watch session")
}

func partitionForwardedRunArgs(fs *flag.FlagSet, flagArgs []string, ownedOnly, forbidden map[string]bool, forbiddenReason string) ([]string, []string, error) {
	ownArgs := []string{}
	runArgs := []string{}
	i := 0
	for i < len(flagArgs) {
		token := flagArgs[i]
		if token == "-" || !strings.HasPrefix(token, "-") {
			return nil, nil, exit(2, "unexpected argument %q; place the command after --", token)
		}
		name := strings.TrimLeft(token, "-")
		hasValue := false
		if eq := strings.Index(name, "="); eq >= 0 {
			name = name[:eq]
			hasValue = true
		}
		if name == "" {
			return nil, nil, exit(2, "invalid flag %q", token)
		}
		if name == "h" || name == "help" {
			ownArgs = append(ownArgs, token)
			i++
			continue
		}
		if forbidden[name] {
			return nil, nil, exit(2, "--%s cannot be used with %s", name, forbiddenReason)
		}
		known := fs.Lookup(name) != nil
		consume := 0
		if !hasValue {
			if known && !watchFlagIsBool(fs, name) {
				if i+1 >= len(flagArgs) {
					return nil, nil, exit(2, "flag needs an argument: --%s", name)
				}
				consume = 1
			} else if !known && i+1 < len(flagArgs) && !strings.HasPrefix(flagArgs[i+1], "-") {
				consume = 1
			}
		}
		tokens := flagArgs[i : i+1+consume]
		switch {
		case ownedOnly[name]:
			ownArgs = append(ownArgs, tokens...)
		case known:
			ownArgs = append(ownArgs, tokens...)
			runArgs = append(runArgs, tokens...)
		default:
			runArgs = append(runArgs, tokens...)
		}
		i += 1 + consume
	}
	return ownArgs, runArgs, nil
}
