package cli

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	gitOriginRuntimeFallbackExitCode = 78
	gitOverlayFallbackExitCode       = 78
	gitOverlayFallbackMarker         = "CRABBOX_GIT_OVERLAY_FALLBACK:"
	gitOverlayMutationMarker         = "CRABBOX_GIT_OVERLAY_WORKSPACE_MUTATED"
	// POSIX ERE keeps Go and remote grep aligned; status digits in URLs are not authentication failures.
	gitOriginHTTPAuthPattern = `authentication (failed|required)|could not read Username|unable to get password|terminal prompts disabled|access denied|permission denied|HTTP(/[0-9.]+)?[[:space:]]+(401|403)([^[:alnum:]_]|$)|requested URL returned error:[[:space:]]*(401|403)([^[:alnum:]_]|$)`
)

var (
	gitOriginHTTPServerError = regexp.MustCompile(`(?i)(?:requested URL returned error:[[:space:]]*|HTTP(?:/[0-9.]+)?[[:space:]]+|HTTP code[[:space:]]*=[[:space:]]*)5[0-9][0-9]\b`)
	gitOriginHTTPAuthError   = regexp.MustCompile(`(?i)` + gitOriginHTTPAuthPattern)
	gitOriginTransportError  = regexp.MustCompile(`(?i)Could not resolve (?:host|proxy)|Could not resolve hostname|Failed to connect|Couldn.t connect to server|Connection (?:refused|timed out|reset by peer)|Operation timed out|Network is unreachable|No route to host|SSL certificate problem|server certificate verification failed|TLS connect error|SSL connect error|gnutls_handshake\(\) failed|Empty reply from server|Recv failure|Send failure|Failed sending data to the peer`)
	gitOriginHTTPNotFound    = regexp.MustCompile(`(?i)\brepository not found\b`)
	gitOriginFilesystemError = regexp.MustCompile(`(?i)does not appear to be a git repository|repository .* does not exist|No such file or directory|Permission denied|unable to access`)
)

var gitOverlayTransformAttributes = []string{
	"text", "crlf", "eol", "working-tree-encoding", "filter", "smudge", "clean", "ident",
}

type gitOverlayDecision struct {
	Requested bool
	Enabled   bool
	Reason    string
}

type gitOriginDisposition uint8

const (
	gitOriginAbsent gitOriginDisposition = iota
	gitOriginRemoteAttemptSafe
	gitOriginNonForwardable
)

const (
	gitOverlaySnapshotMaxAttempts = 3
	gitOverlayCleanupMaxAttempts  = 3
	gitOverlayCleanupRetryDelay   = 50 * time.Millisecond
)

type gitOverlaySnapshot struct {
	Root        string
	Manifest    SyncManifest
	Excludes    SyncExcludeRules
	Fingerprint string
	Checkout    gitOverlayCheckoutState
	cleanupRoot *gitOverlaySnapshotRoot
}

type gitOverlaySnapshotHook func(phase string, attempt int, root string)

type gitOverlayCheckoutState struct {
	Head      string
	IndexTree string
}

type gitOverlaySnapshotParentIdentity struct {
	path string
	info os.FileInfo
}

type gitOverlaySnapshotParent struct {
	source      gitOverlaySnapshotParentIdentity
	destination string
	relative    string
	handle      *os.File
	finalMode   os.FileMode
	tempMode    os.FileMode
	finalMTime  time.Time
	depth       int
}

type gitOverlaySnapshotParents struct {
	sourceRoot gitOverlaySnapshotParentIdentity
	byPath     map[string]*gitOverlaySnapshotParent
	ordered    []*gitOverlaySnapshotParent
}

type gitOverlaySnapshotThaw func(*gitOverlaySnapshotParents) error

type gitOverlaySnapshotRoot struct {
	parent      *os.Root
	root        *os.Root
	name        string
	identity    os.FileInfo
	directories []*gitOverlaySnapshotParent
}

var errGitOverlaySnapshotDrift = errors.New("local Git overlay changed during snapshot creation")

var gitOverlayGitExecutable = func() string {
	path, err := exec.LookPath("git")
	if err != nil {
		return "git"
	}
	return path
}()

func (snapshot *gitOverlaySnapshot) cleanup() error {
	return snapshot.cleanupWith(func(string) error {
		if snapshot.cleanupRoot == nil {
			return fmt.Errorf("missing git overlay snapshot cleanup capability")
		}
		return snapshot.cleanupRoot.remove()
	}, time.Sleep)
}

func (snapshot *gitOverlaySnapshot) cleanupWith(removeAll func(string) error, sleep func(time.Duration)) error {
	root := snapshot.Root
	if root == "" {
		return nil
	}
	var cleanupErr error
	for attempt := 1; attempt <= gitOverlayCleanupMaxAttempts; attempt++ {
		cleanupErr = removeAll(root)
		if cleanupErr == nil {
			snapshot.closeCleanupRoot()
			snapshot.Root = ""
			return nil
		}
		if attempt < gitOverlayCleanupMaxAttempts {
			sleep(gitOverlayCleanupRetryDelay)
		}
	}
	return fmt.Errorf("remove git overlay snapshot %q after %d attempts: %w", root, gitOverlayCleanupMaxAttempts, cleanupErr)
}

func (snapshot *gitOverlaySnapshot) closeCleanupRoot() {
	if snapshot.cleanupRoot == nil {
		return
	}
	snapshot.cleanupRoot.close()
	snapshot.cleanupRoot = nil
}

func newGitOverlaySnapshot() (gitOverlaySnapshot, error) {
	root, err := os.MkdirTemp("", "crabbox-git-overlay-")
	if err != nil {
		return gitOverlaySnapshot{}, err
	}
	parentPath := filepath.Dir(root)
	name := filepath.Base(root)
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		_ = os.Remove(root)
		return gitOverlaySnapshot{}, err
	}
	cleanupRoot := &gitOverlaySnapshotRoot{parent: parent, name: name}
	cleanupRoot.root, err = parent.OpenRoot(name)
	if err != nil {
		cleanupRoot.close()
		_ = os.Remove(root)
		return gitOverlaySnapshot{}, err
	}
	cleanupRoot.identity, err = cleanupRoot.root.Stat(".")
	if err != nil {
		cleanupRoot.close()
		_ = os.Remove(root)
		return gitOverlaySnapshot{}, err
	}
	if err := cleanupRoot.verifyIdentity(); err != nil {
		cleanupRoot.close()
		_ = os.Remove(root)
		return gitOverlaySnapshot{}, err
	}
	return gitOverlaySnapshot{Root: root, cleanupRoot: cleanupRoot}, nil
}

func (root *gitOverlaySnapshotRoot) verifyIdentity() error {
	if root == nil || root.parent == nil || root.root == nil || root.identity == nil {
		return fmt.Errorf("incomplete git overlay snapshot cleanup capability")
	}
	opened, err := root.root.Stat(".")
	if err != nil {
		return fmt.Errorf("stat git overlay snapshot root handle: %w", err)
	}
	current, err := root.parent.Lstat(root.name)
	if err != nil {
		return fmt.Errorf("stat git overlay snapshot root path: %w", err)
	}
	if !opened.IsDir() ||
		!current.IsDir() ||
		current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(root.identity, opened) ||
		!os.SameFile(opened, current) {
		return fmt.Errorf("git overlay snapshot root identity changed")
	}
	return nil
}

func (root *gitOverlaySnapshotRoot) remove() error {
	if err := root.verifyIdentity(); err != nil {
		return err
	}
	if err := root.thawDirectories(); err != nil {
		return err
	}
	if err := thawGitOverlaySnapshotFiles(root.root); err != nil {
		return err
	}
	directory, err := root.root.Open(".")
	if err != nil {
		return fmt.Errorf("open git overlay snapshot root: %w", err)
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil {
		return fmt.Errorf("read git overlay snapshot root: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close git overlay snapshot root: %w", closeErr)
	}
	for _, entry := range entries {
		if err := root.root.RemoveAll(entry.Name()); err != nil {
			return fmt.Errorf("remove git overlay snapshot entry %q: %w", entry.Name(), err)
		}
	}
	if err := root.verifyIdentity(); err != nil {
		return err
	}
	if err := root.parent.Remove(root.name); err != nil {
		return fmt.Errorf("remove git overlay snapshot root: %w", err)
	}
	return nil
}

func (root *gitOverlaySnapshotRoot) thawDirectories() error {
	for _, directory := range root.directories {
		if directory == nil || directory.handle == nil {
			continue
		}
		opened, err := directory.handle.Stat()
		if err != nil {
			return fmt.Errorf("stat git overlay snapshot cleanup handle %q: %w", directory.relative, err)
		}
		current, err := root.root.Lstat(filepath.ToSlash(directory.relative))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat git overlay snapshot cleanup path %q: %w", directory.relative, err)
		}
		if !opened.IsDir() ||
			!current.IsDir() ||
			current.Mode()&os.ModeSymlink != 0 ||
			!os.SameFile(opened, current) {
			return fmt.Errorf("git overlay snapshot cleanup path changed at %q", directory.relative)
		}
		if err := directory.handle.Chmod(directory.tempMode); err != nil {
			return fmt.Errorf("thaw git overlay snapshot directory %q: %w", directory.relative, err)
		}
	}
	return nil
}

func (root *gitOverlaySnapshotRoot) close() {
	if root == nil {
		return
	}
	for _, directory := range root.directories {
		if directory != nil && directory.handle != nil {
			_ = directory.handle.Close()
			directory.handle = nil
		}
	}
	if root.root != nil {
		_ = root.root.Close()
		root.root = nil
	}
	if root.parent != nil {
		_ = root.parent.Close()
		root.parent = nil
	}
}

func (root *gitOverlaySnapshotRoot) adopt(parents *gitOverlaySnapshotParents) {
	if root == nil || parents == nil || len(parents.ordered) == 0 {
		return
	}
	root.directories = append(root.directories, parents.ordered...)
	parents.ordered = nil
	parents.byPath = nil
}

func prepareGitOverlaySnapshot(
	repo Repo,
	cfg Config,
	excludes SyncExcludeRules,
	includes []string,
	plan gitCoherencePlan,
) (gitOverlaySnapshot, error) {
	return prepareGitOverlaySnapshotWithHook(repo, cfg, excludes, includes, plan, nil)
}

func prepareGitOverlaySnapshotWithHook(
	repo Repo,
	cfg Config,
	excludes SyncExcludeRules,
	includes []string,
	plan gitCoherencePlan,
	hook gitOverlaySnapshotHook,
) (gitOverlaySnapshot, error) {
	return prepareGitOverlaySnapshotWithCleanup(repo, cfg, excludes, includes, plan, hook, func(snapshot *gitOverlaySnapshot) error {
		return snapshot.cleanup()
	})
}

func prepareGitOverlaySnapshotWithCleanup(
	repo Repo,
	cfg Config,
	_ SyncExcludeRules,
	includes []string,
	plan gitCoherencePlan,
	hook gitOverlaySnapshotHook,
	cleanup func(*gitOverlaySnapshot) error,
) (gitOverlaySnapshot, error) {
	for attempt := 1; attempt <= gitOverlaySnapshotMaxAttempts; attempt++ {
		checkout, err := captureGitOverlayCheckoutState(repo.Root)
		if err != nil {
			if errors.Is(err, errGitOverlaySnapshotDrift) && attempt < gitOverlaySnapshotMaxAttempts {
				continue
			}
			return gitOverlaySnapshot{}, err
		}
		if checkout.Head != repo.Head || checkout.Head != plan.Target {
			return gitOverlaySnapshot{}, fmt.Errorf("head_changed")
		}
		if hook != nil {
			hook("initial_checkout_state_captured", attempt, "")
		}
		excludes, err := syncExcludes(repo.Root, cfg)
		if err != nil {
			return gitOverlaySnapshot{}, err
		}
		manifest, err := syncManifestFilteredRules(repo.Root, excludes, includes)
		if err != nil {
			return gitOverlaySnapshot{}, err
		}
		validationErr := validateGitOverlayManifestAtState(repo, manifest, checkout)
		if hook != nil {
			hook("before_initial_validation_checkout_state", attempt, "")
		}
		validatedCheckout, checkoutErr := captureGitOverlayCheckoutState(repo.Root)
		if checkoutErr != nil || validatedCheckout != checkout {
			if attempt < gitOverlaySnapshotMaxAttempts {
				continue
			}
			return gitOverlaySnapshot{}, errGitOverlaySnapshotDrift
		}
		if validationErr != nil {
			return gitOverlaySnapshot{}, validationErr
		}
		snapshot, err := newGitOverlaySnapshot()
		if err != nil {
			return gitOverlaySnapshot{}, err
		}
		snapshot.Manifest = manifest
		snapshot.Excludes = excludes
		snapshot.Checkout = checkout
		if hook != nil {
			hook("snapshot_created", attempt, snapshot.Root)
		}
		if err := copyGitOverlaySnapshotOwned(repo.Root, &snapshot, manifest.OverlayFiles, attempt, hook); err != nil {
			if retry, retained, result := retryGitOverlaySnapshotAfterDrift(&snapshot, err, cleanup); retry {
				continue
			} else {
				return retained, result
			}
		}
		if hook != nil {
			hook("snapshot_copied", attempt, snapshot.Root)
		}
		snapshotRepo := repo
		snapshotRepo.Root = snapshot.Root
		snapshot.Fingerprint, err = syncFingerprintForManifest(snapshotRepo, cfg, manifest, excludes, plan)
		if err != nil {
			return cleanupGitOverlaySnapshotAfterFailure(snapshot, err, cleanup)
		}
		if hook != nil {
			hook("snapshot_fingerprinted", attempt, snapshot.Root)
		}
		refreshedExcludes, err := syncExcludes(repo.Root, cfg)
		if err != nil {
			return cleanupGitOverlaySnapshotAfterFailure(snapshot, err, cleanup)
		}
		if !sameSyncExcludeRules(excludes, refreshedExcludes) {
			if retry, retained, result := retryGitOverlaySnapshotAfterDrift(&snapshot, errGitOverlaySnapshotDrift, cleanup); retry {
				continue
			} else {
				return retained, result
			}
		}
		refreshed, err := syncManifestFilteredRules(repo.Root, refreshedExcludes, includes)
		if err != nil {
			return cleanupGitOverlaySnapshotAfterFailure(snapshot, err, cleanup)
		}
		if !sameSyncManifest(manifest, refreshed) {
			if retry, retained, result := retryGitOverlaySnapshotAfterDrift(&snapshot, errGitOverlaySnapshotDrift, cleanup); retry {
				continue
			} else {
				return retained, result
			}
		}
		if hook != nil {
			hook("before_live_fingerprint", attempt, snapshot.Root)
		}
		liveFingerprint, err := syncFingerprintForManifest(repo, cfg, refreshed, refreshedExcludes, plan)
		if err != nil {
			return cleanupGitOverlaySnapshotAfterFailure(snapshot, err, cleanup)
		}
		if hook != nil {
			hook("live_fingerprinted", attempt, snapshot.Root)
		}
		finalExcludes, err := syncExcludes(repo.Root, cfg)
		if err != nil {
			return cleanupGitOverlaySnapshotAfterFailure(snapshot, err, cleanup)
		}
		finalManifest, err := syncManifestFilteredRules(repo.Root, finalExcludes, includes)
		if err != nil {
			return cleanupGitOverlaySnapshotAfterFailure(snapshot, err, cleanup)
		}
		if hook != nil {
			hook("before_final_checkout_state", attempt, snapshot.Root)
		}
		finalCheckout, err := captureGitOverlayCheckoutState(repo.Root)
		if err != nil || finalCheckout != checkout {
			if retry, retained, result := retryGitOverlaySnapshotAfterDrift(&snapshot, errGitOverlaySnapshotDrift, cleanup); retry {
				continue
			} else {
				return retained, result
			}
		}
		finalValidationErr := validateGitOverlayManifestAtState(repo, finalManifest, finalCheckout)
		acceptedExcludes, err := syncExcludes(repo.Root, cfg)
		if err != nil {
			return cleanupGitOverlaySnapshotAfterFailure(snapshot, err, cleanup)
		}
		acceptedManifest, err := syncManifestFilteredRules(repo.Root, acceptedExcludes, includes)
		if err != nil {
			return cleanupGitOverlaySnapshotAfterFailure(snapshot, err, cleanup)
		}
		acceptedFingerprint, err := syncFingerprintForManifest(repo, cfg, acceptedManifest, acceptedExcludes, plan)
		if err != nil {
			return cleanupGitOverlaySnapshotAfterFailure(snapshot, err, cleanup)
		}
		if hook != nil {
			hook("before_accepted_checkout_state", attempt, snapshot.Root)
		}
		acceptedCheckout, err := captureGitOverlayCheckoutState(repo.Root)
		if err != nil || acceptedCheckout != finalCheckout {
			if retry, retained, result := retryGitOverlaySnapshotAfterDrift(&snapshot, errGitOverlaySnapshotDrift, cleanup); retry {
				continue
			} else {
				return retained, result
			}
		}
		if !sameSyncExcludeRules(finalExcludes, acceptedExcludes) ||
			!sameSyncManifest(finalManifest, acceptedManifest) {
			if retry, retained, result := retryGitOverlaySnapshotAfterDrift(&snapshot, errGitOverlaySnapshotDrift, cleanup); retry {
				continue
			} else {
				return retained, result
			}
		}
		if finalValidationErr != nil {
			return cleanupGitOverlaySnapshotAfterFailure(snapshot, finalValidationErr, cleanup)
		}
		if sameSyncExcludeRules(refreshedExcludes, finalExcludes) &&
			sameSyncManifest(manifest, refreshed) &&
			sameSyncManifest(manifest, finalManifest) &&
			snapshot.Fingerprint == liveFingerprint &&
			snapshot.Fingerprint == acceptedFingerprint {
			snapshot.Manifest = acceptedManifest
			snapshot.Excludes = acceptedExcludes
			return snapshot, nil
		}
		if retry, retained, result := retryGitOverlaySnapshotAfterDrift(&snapshot, errGitOverlaySnapshotDrift, cleanup); retry {
			continue
		} else {
			return retained, result
		}
	}
	return gitOverlaySnapshot{}, errGitOverlaySnapshotDrift
}

func captureGitOverlayCheckoutState(root string) (gitOverlayCheckoutState, error) {
	return captureGitOverlayCheckoutStateWithHook(root, nil)
}

func captureGitOverlayCheckoutStateWithHook(root string, betweenSamples func()) (gitOverlayCheckoutState, error) {
	first, err := readGitOverlayCheckoutState(root)
	if err != nil {
		return gitOverlayCheckoutState{}, err
	}
	if betweenSamples != nil {
		betweenSamples()
	}
	topLevel, err := gitOverlayGitOutput(root, "rev-parse", "--show-toplevel")
	if err != nil {
		return gitOverlayCheckoutState{}, errGitOverlaySnapshotDrift
	}
	canonicalRoot, err := filepath.Abs(root)
	if err != nil {
		return gitOverlayCheckoutState{}, fmt.Errorf("invalid_checkout_root")
	}
	canonicalTopLevel, err := filepath.Abs(topLevel)
	if err != nil {
		return gitOverlayCheckoutState{}, fmt.Errorf("invalid_checkout_root")
	}
	canonicalRoot = canonicalRepositoryPath(canonicalRoot)
	canonicalTopLevel = canonicalRepositoryPath(canonicalTopLevel)
	if !sameCanonicalRepositoryPath(canonicalRoot, canonicalTopLevel) {
		return gitOverlayCheckoutState{}, fmt.Errorf("checkout_root_mismatch")
	}
	second, err := readGitOverlayCheckoutState(root)
	if err != nil {
		return gitOverlayCheckoutState{}, err
	}
	if first != second {
		return gitOverlayCheckoutState{}, errGitOverlaySnapshotDrift
	}
	return first, nil
}

func readGitOverlayCheckoutState(root string) (gitOverlayCheckoutState, error) {
	head, err := gitOverlayGitOutput(root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || !validGitObjectID(head) {
		if symbolic, symbolicErr := gitOverlayGitOutput(root, "symbolic-ref", "--quiet", "HEAD"); symbolicErr == nil && symbolic != "" {
			return gitOverlayCheckoutState{}, fmt.Errorf("unborn_head")
		}
		return gitOverlayCheckoutState{}, fmt.Errorf("invalid_head")
	}
	indexTree, err := gitOverlayGitOutput(root, "write-tree")
	if err != nil || !validGitObjectID(indexTree) {
		if unmerged, unmergedErr := gitOverlayGitBytes(root, "ls-files", "--unmerged", "-z"); unmergedErr == nil && len(unmerged) != 0 {
			return gitOverlayCheckoutState{}, fmt.Errorf("unmerged_index")
		}
		return gitOverlayCheckoutState{}, fmt.Errorf("invalid_index")
	}
	return gitOverlayCheckoutState{Head: head, IndexTree: indexTree}, nil
}

func validGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func gitOverlayGitOutput(root string, args ...string) (string, error) {
	output, err := gitOverlayGitBytes(root, args...)
	return strings.TrimSpace(string(output)), err
}

func gitOverlayGitBytes(root string, args ...string) ([]byte, error) {
	commandArgs := []string{
		"--no-optional-locks",
		"-c", "credential.helper=",
		"-c", "credential.interactive=never",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false",
		"-c", "core.attributesFile=" + os.DevNull,
		"-c", "core.excludesFile=" + os.DevNull,
	}
	commandArgs = append(commandArgs, args...)
	cmd := exec.Command(gitOverlayGitExecutable, commandArgs...)
	cmd.Dir = root
	cmd.Env = gitOverlayGitEnvironment()
	return cmd.Output()
}

func gitOverlayGitEnvironment() []string {
	environment := []string{
		"HOME=" + os.DevNull,
		"XDG_CONFIG_HOME=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=" + os.DevNull,
		"SSH_ASKPASS=" + os.DevNull,
		"GCM_INTERACTIVE=Never",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_LITERAL_PATHSPECS=1",
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "TMPDIR", "TMP", "TEMP":
			environment = append(environment, entry)
		}
	}
	return environment
}

func copyGitOverlaySnapshot(sourceRoot, snapshotRoot string, paths []string) error {
	return copyGitOverlaySnapshotWithHook(sourceRoot, snapshotRoot, paths, 0, nil)
}

func copyGitOverlaySnapshotWithHook(sourceRoot, snapshotRoot string, paths []string, attempt int, hook gitOverlaySnapshotHook) (result error) {
	return copyGitOverlaySnapshotContents(sourceRoot, snapshotRoot, paths, attempt, hook, nil)
}

func copyGitOverlaySnapshotOwned(sourceRoot string, snapshot *gitOverlaySnapshot, paths []string, attempt int, hook gitOverlaySnapshotHook) error {
	if snapshot == nil || snapshot.Root == "" || snapshot.cleanupRoot == nil {
		return fmt.Errorf("missing git overlay snapshot ownership")
	}
	return copyGitOverlaySnapshotContents(sourceRoot, snapshot.Root, paths, attempt, hook, snapshot.cleanupRoot)
}

func copyGitOverlaySnapshotContents(sourceRoot, snapshotRoot string, paths []string, attempt int, hook gitOverlaySnapshotHook, owner *gitOverlaySnapshotRoot) (result error) {
	return copyGitOverlaySnapshotContentsWithThaw(sourceRoot, snapshotRoot, paths, attempt, hook, owner, func(parents *gitOverlaySnapshotParents) error {
		return parents.thaw()
	})
}

func copyGitOverlaySnapshotContentsWithThaw(sourceRoot, snapshotRoot string, paths []string, attempt int, hook gitOverlaySnapshotHook, owner *gitOverlaySnapshotRoot, thaw gitOverlaySnapshotThaw) (result error) {
	parents, err := newGitOverlaySnapshotParents(sourceRoot)
	if err != nil {
		return err
	}
	defer func() {
		if result == nil {
			if owner != nil {
				owner.adopt(parents)
			}
			parents.close()
			return
		}
		thawErr := thaw(parents)
		if thawErr != nil && owner != nil {
			owner.adopt(parents)
		}
		parents.close()
		result = errors.Join(result, thawErr)
	}()
	for _, rel := range paths {
		if !safeRepoRel(rel) {
			return fmt.Errorf("unsafe overlay snapshot path %q", rel)
		}
		source := filepath.Join(sourceRoot, filepath.FromSlash(rel))
		destination := filepath.Join(snapshotRoot, filepath.FromSlash(rel))
		info, err := os.Lstat(source)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("%w: snapshot overlay path %q disappeared", errGitOverlaySnapshotDrift, rel)
			}
			return fmt.Errorf("snapshot overlay path %q: %w", rel, err)
		}
		if hook != nil {
			hook("after_lstat", attempt, snapshotRoot)
		}
		identities, err := parents.ensure(snapshotRoot, filepath.Dir(rel), source, info)
		if err != nil {
			return fmt.Errorf("create overlay snapshot parent for %q: %w", rel, err)
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(source)
			if err != nil {
				return fmt.Errorf("read overlay snapshot symlink %q: %w", rel, classifyGitOverlaySnapshotSourceError(source, info, err))
			}
			if err := os.Symlink(target, destination); err != nil {
				return fmt.Errorf("create overlay snapshot symlink %q: %w", rel, err)
			}
			finalMTime := normalizedGitOverlayFileTime(info.ModTime())
			if err := syncGitOverlaySymlinkTimes(destination, finalMTime); err != nil {
				return fmt.Errorf("restore overlay snapshot symlink mtime %q: %w", rel, err)
			}
			refreshed, err := os.Lstat(source)
			if err != nil {
				return fmt.Errorf("restat overlay snapshot symlink %q: %w", rel, classifyGitOverlaySnapshotSourceError(source, info, err))
			}
			if !sameGitOverlaySnapshotIdentity(info, refreshed) {
				return fmt.Errorf("%w: overlay snapshot symlink changed during copy at %q", errGitOverlaySnapshotDrift, rel)
			}
			refreshedTarget, err := os.Readlink(source)
			if err != nil {
				return fmt.Errorf("reread overlay snapshot symlink %q: %w", rel, classifyGitOverlaySnapshotSourceError(source, info, err))
			}
			if refreshedTarget != target {
				return fmt.Errorf("%w: overlay snapshot symlink target changed during copy at %q", errGitOverlaySnapshotDrift, rel)
			}
			copied, err := os.Lstat(destination)
			if err != nil {
				return fmt.Errorf("stat overlay snapshot symlink destination %q: %w", rel, err)
			}
			if copied.Mode()&os.ModeSymlink == 0 || !copied.ModTime().Equal(finalMTime) {
				return fmt.Errorf("overlay snapshot symlink metadata changed at %q", rel)
			}
			copiedTarget, err := os.Readlink(destination)
			if err != nil {
				return fmt.Errorf("read overlay snapshot symlink destination %q: %w", rel, err)
			}
			if copiedTarget != target {
				return fmt.Errorf("overlay snapshot symlink target changed at %q", rel)
			}
		case info.Mode().IsRegular():
			if err := copyGitOverlaySnapshotFile(source, destination, info); err != nil {
				return fmt.Errorf("copy overlay snapshot file %q: %w", rel, err)
			}
		default:
			return fmt.Errorf("unsupported overlay snapshot file type at %q", rel)
		}
		if err := verifyGitOverlaySnapshotParents(identities); err != nil {
			return fmt.Errorf("verify overlay snapshot parent for %q: %w", rel, err)
		}
		if err := parents.verifyDestinations(false); err != nil {
			return fmt.Errorf("verify overlay snapshot destination parent for %q: %w", rel, err)
		}
	}
	if err := parents.restore(attempt, snapshotRoot, hook); err != nil {
		return err
	}
	return nil
}

func newGitOverlaySnapshotParents(sourceRoot string) (*gitOverlaySnapshotParents, error) {
	rootInfo, err := os.Lstat(sourceRoot)
	if err != nil {
		return nil, err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("snapshot root is not a real directory")
	}
	return &gitOverlaySnapshotParents{
		sourceRoot: gitOverlaySnapshotParentIdentity{path: sourceRoot, info: rootInfo},
		byPath:     make(map[string]*gitOverlaySnapshotParent),
	}, nil
}

func (parents *gitOverlaySnapshotParents) ensure(snapshotRoot, parent, observedSource string, observedInfo os.FileInfo) ([]gitOverlaySnapshotParentIdentity, error) {
	identities := []gitOverlaySnapshotParentIdentity{parents.sourceRoot}
	if parent == "." || parent == "" {
		return identities, nil
	}
	source := parents.sourceRoot.path
	destination := snapshotRoot
	relative := ""
	for _, component := range strings.Split(filepath.ToSlash(parent), "/") {
		source = filepath.Join(source, filepath.FromSlash(component))
		destination = filepath.Join(destination, filepath.FromSlash(component))
		relative = filepath.Join(relative, filepath.FromSlash(component))
		info, err := os.Lstat(source)
		if err != nil {
			return nil, classifyGitOverlaySnapshotSourceError(observedSource, observedInfo, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("snapshot parent is not a real directory")
		}
		if retained, ok := parents.byPath[destination]; ok {
			if !sameGitOverlaySnapshotIdentity(retained.source.info, info) {
				return nil, fmt.Errorf("%w: snapshot parent changed during copy at %q", errGitOverlaySnapshotDrift, source)
			}
			if err := verifyGitOverlaySnapshotDestination(retained, retained.tempMode, true, false); err != nil {
				return nil, err
			}
			identities = append(identities, retained.source)
			continue
		}
		identity := gitOverlaySnapshotParentIdentity{path: source, info: info}
		finalMode := gitOverlaySupportedMode(info.Mode())
		tempMode := gitOverlayTemporaryDirectoryMode(finalMode)
		if err := os.Mkdir(destination, tempMode); err != nil {
			return nil, err
		}
		handle, err := openGitOverlaySnapshotParent(destination)
		if err != nil {
			return nil, err
		}
		retained := &gitOverlaySnapshotParent{
			source:      identity,
			destination: destination,
			relative:    relative,
			handle:      handle,
			finalMode:   finalMode,
			tempMode:    tempMode,
			finalMTime:  normalizedGitOverlayFileTime(info.ModTime()),
			depth:       len(identities),
		}
		parents.byPath[destination] = retained
		parents.ordered = append(parents.ordered, retained)
		if err := verifyGitOverlaySnapshotDestination(retained, 0, false, false); err != nil {
			return nil, err
		}
		if err := retained.handle.Chmod(tempMode); err != nil {
			return nil, err
		}
		if err := verifyGitOverlaySnapshotDestination(retained, tempMode, true, false); err != nil {
			return nil, err
		}
		identities = append(identities, identity)
	}
	if err := verifyGitOverlaySnapshotParents(identities); err != nil {
		return nil, err
	}
	return identities, nil
}

func (parents *gitOverlaySnapshotParents) restore(attempt int, snapshotRoot string, hook gitOverlaySnapshotHook) error {
	ordered := append([]*gitOverlaySnapshotParent(nil), parents.ordered...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].depth != ordered[j].depth {
			return ordered[i].depth > ordered[j].depth
		}
		return ordered[i].destination < ordered[j].destination
	})
	for _, parent := range ordered {
		if hook != nil {
			hook("before_parent_mode_restore", attempt, snapshotRoot)
		}
		if err := verifyGitOverlaySnapshotParents([]gitOverlaySnapshotParentIdentity{parents.sourceRoot, parent.source}); err != nil {
			return err
		}
		if err := verifyGitOverlaySnapshotDestination(parent, parent.tempMode, true, false); err != nil {
			return err
		}
		if err := syncGitOverlayFileTimes(parent.handle, parent.finalMTime); err != nil {
			return fmt.Errorf("restore overlay snapshot parent mtime at %q: %w", parent.destination, err)
		}
		if err := parent.handle.Chmod(parent.finalMode); err != nil {
			return fmt.Errorf("restore overlay snapshot parent mode at %q: %w", parent.destination, err)
		}
		if err := verifyGitOverlaySnapshotDestination(parent, parent.finalMode, true, true); err != nil {
			return err
		}
		if err := verifyGitOverlaySnapshotParents([]gitOverlaySnapshotParentIdentity{parents.sourceRoot, parent.source}); err != nil {
			return err
		}
	}
	if err := verifyGitOverlaySnapshotParents(parents.sourceIdentities()); err != nil {
		return err
	}
	return parents.verifyDestinations(true)
}

func (parents *gitOverlaySnapshotParents) sourceIdentities() []gitOverlaySnapshotParentIdentity {
	identities := make([]gitOverlaySnapshotParentIdentity, 0, len(parents.ordered)+1)
	identities = append(identities, parents.sourceRoot)
	for _, parent := range parents.ordered {
		identities = append(identities, parent.source)
	}
	return identities
}

func (parents *gitOverlaySnapshotParents) verifyDestinations(final bool) error {
	for _, parent := range parents.ordered {
		mode := parent.tempMode
		if final {
			mode = parent.finalMode
		}
		if err := verifyGitOverlaySnapshotDestination(parent, mode, true, final); err != nil {
			return err
		}
	}
	return nil
}

func verifyGitOverlaySnapshotDestination(parent *gitOverlaySnapshotParent, wantMode os.FileMode, checkMode, checkMTime bool) error {
	opened, err := parent.handle.Stat()
	if err != nil {
		return fmt.Errorf("stat overlay snapshot parent handle %q: %w", parent.destination, err)
	}
	current, err := os.Lstat(parent.destination)
	if err != nil {
		return fmt.Errorf("stat overlay snapshot parent path %q: %w", parent.destination, err)
	}
	if !opened.IsDir() ||
		!current.IsDir() ||
		current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(opened, current) {
		return fmt.Errorf("overlay snapshot parent path changed at %q", parent.destination)
	}
	if checkMode && (gitOverlaySupportedMode(opened.Mode()) != wantMode || gitOverlaySupportedMode(current.Mode()) != wantMode) {
		return fmt.Errorf("overlay snapshot parent mode at %q is %#o, want %#o", parent.destination, gitOverlaySupportedMode(current.Mode()), wantMode)
	}
	if checkMTime &&
		(!opened.ModTime().Equal(parent.finalMTime) || !current.ModTime().Equal(parent.finalMTime)) {
		return fmt.Errorf("overlay snapshot parent mtime at %q is %s, want %s", parent.destination, current.ModTime(), parent.finalMTime)
	}
	return nil
}

func gitOverlaySupportedMode(mode os.FileMode) os.FileMode {
	return mode & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}

func (parents *gitOverlaySnapshotParents) thaw() error {
	var errs []error
	for _, parent := range parents.ordered {
		if parent.handle == nil {
			continue
		}
		if err := parent.handle.Chmod(parent.tempMode); err != nil {
			errs = append(errs, fmt.Errorf("thaw overlay snapshot parent %q: %w", parent.destination, err))
		}
	}
	return errors.Join(errs...)
}

func (parents *gitOverlaySnapshotParents) close() {
	for _, parent := range parents.ordered {
		if parent.handle != nil {
			_ = parent.handle.Close()
			parent.handle = nil
		}
	}
}

func verifyGitOverlaySnapshotParents(identities []gitOverlaySnapshotParentIdentity) error {
	for _, identity := range identities {
		refreshed, err := os.Lstat(identity.path)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("%w: snapshot parent changed during copy at %q: %v", errGitOverlaySnapshotDrift, identity.path, err)
			}
			return fmt.Errorf("stat snapshot parent during copy at %q: %w", identity.path, err)
		}
		if !refreshed.IsDir() || refreshed.Mode()&os.ModeSymlink != 0 || !sameGitOverlaySnapshotIdentity(identity.info, refreshed) {
			return fmt.Errorf("%w: snapshot parent changed during copy at %q", errGitOverlaySnapshotDrift, identity.path)
		}
	}
	return nil
}

func copyGitOverlaySnapshotFile(source, destination string, info os.FileInfo) error {
	input, err := os.Open(source)
	if err != nil {
		return classifyGitOverlaySnapshotSourceError(source, info, err)
	}
	defer input.Close()
	openedInfo, err := input.Stat()
	if err != nil {
		return err
	}
	if !openedInfo.Mode().IsRegular() || !sameGitOverlaySnapshotIdentity(info, openedInfo) {
		return fmt.Errorf("%w: snapshot source changed before copy", errGitOverlaySnapshotDrift)
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, openedInfo.Mode().Perm())
	if err != nil {
		return err
	}
	written, err := io.Copy(output, input)
	if err != nil {
		_ = output.Close()
		return err
	}
	postCopyInfo, err := input.Stat()
	if err != nil {
		_ = output.Close()
		return err
	}
	finalPathInfo, finalPathErr := os.Lstat(source)
	if finalPathErr != nil {
		_ = output.Close()
		return classifyGitOverlaySnapshotSourceError(source, info, finalPathErr)
	}
	if !sameGitOverlaySnapshotIdentity(openedInfo, postCopyInfo) ||
		!sameGitOverlaySnapshotIdentity(postCopyInfo, finalPathInfo) ||
		written != openedInfo.Size() {
		_ = output.Close()
		return fmt.Errorf("%w: snapshot source changed during copy", errGitOverlaySnapshotDrift)
	}
	if err := verifyGitOverlaySnapshotFileDestination(output, destination, 0, time.Time{}, false); err != nil {
		_ = output.Close()
		return err
	}
	finalMode := gitOverlaySupportedMode(openedInfo.Mode())
	finalMTime := normalizedGitOverlayFileTime(openedInfo.ModTime())
	if err := syncGitOverlayFileTimes(output, finalMTime); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Chmod(finalMode); err != nil {
		_ = output.Close()
		return err
	}
	if err := verifyGitOverlaySnapshotFileDestination(output, destination, finalMode, finalMTime, true); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func verifyGitOverlaySnapshotFileDestination(file *os.File, path string, wantMode os.FileMode, wantMTime time.Time, checkMetadata bool) error {
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat overlay snapshot file handle %q: %w", path, err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat overlay snapshot file path %q: %w", path, err)
	}
	if !opened.Mode().IsRegular() ||
		!current.Mode().IsRegular() ||
		current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(opened, current) {
		return fmt.Errorf("overlay snapshot file path changed at %q", path)
	}
	if checkMetadata && (gitOverlaySupportedMode(opened.Mode()) != wantMode || gitOverlaySupportedMode(current.Mode()) != wantMode) {
		return fmt.Errorf("overlay snapshot file mode at %q is %#o, want %#o", path, gitOverlaySupportedMode(current.Mode()), wantMode)
	}
	if checkMetadata && (!opened.ModTime().Equal(wantMTime) || !current.ModTime().Equal(wantMTime)) {
		return fmt.Errorf("overlay snapshot file mtime at %q is %s, want %s", path, current.ModTime(), wantMTime)
	}
	return nil
}

func classifyGitOverlaySnapshotSourceError(source string, observed os.FileInfo, operationErr error) error {
	refreshed, err := os.Lstat(source)
	if os.IsNotExist(err) || (err == nil && !sameGitOverlaySnapshotIdentity(observed, refreshed)) {
		return fmt.Errorf("%w: snapshot source changed at %q", errGitOverlaySnapshotDrift, source)
	}
	return operationErr
}

func cleanupGitOverlaySnapshotAfterFailure(snapshot gitOverlaySnapshot, primary error, cleanup func(*gitOverlaySnapshot) error) (gitOverlaySnapshot, error) {
	cleanupErr := cleanup(&snapshot)
	if cleanupErr == nil {
		return gitOverlaySnapshot{}, primary
	}
	return snapshot, errors.Join(primary, cleanupErr)
}

func retryGitOverlaySnapshotAfterDrift(snapshot *gitOverlaySnapshot, err error, cleanup func(*gitOverlaySnapshot) error) (bool, gitOverlaySnapshot, error) {
	cleanupErr := cleanup(snapshot)
	if cleanupErr != nil {
		return false, *snapshot, errors.Join(err, cleanupErr)
	}
	if errors.Is(err, errGitOverlaySnapshotDrift) {
		return true, gitOverlaySnapshot{}, nil
	}
	return false, gitOverlaySnapshot{}, err
}

func sameGitOverlaySnapshotIdentity(left, right os.FileInfo) bool {
	return left != nil &&
		right != nil &&
		os.SameFile(left, right) &&
		left.Mode() == right.Mode() &&
		left.Size() == right.Size() &&
		left.ModTime().Equal(right.ModTime())
}

func sameSyncExcludeRules(left, right SyncExcludeRules) bool {
	return slices.Equal(left.rules, right.rules)
}

func decideGitOverlay(cfg Config, repo Repo, target SSHTarget, manifest SyncManifest, coherence gitCoherencePlan, credentialBlocked, fullResync, hydratedByActions bool) gitOverlayDecision {
	decision := gitOverlayDecision{Requested: cfg.Sync.GitOverlay}
	if !decision.Requested {
		return decision
	}
	originDisposition := classifyGitOrigin(repo.RemoteURL)
	switch {
	case target.TargetOS != targetLinux || isWindowsNativeTarget(target) || isWindowsWSL2Target(target):
		decision.Reason = "unsupported_target"
	case fullResync:
		decision.Reason = "full_resync"
	case hydratedByActions:
		decision.Reason = "actions_workspace"
	case !cfg.Sync.Delete:
		decision.Reason = "delete_disabled"
	case !cfg.Sync.GitSeed:
		decision.Reason = "git_seed_disabled"
	case len(syncIncludes(cfg)) != 0:
		decision.Reason = "include_whitelist"
	case credentialBlocked || gitRemoteURLHasCredentials(repo.RemoteURL):
		decision.Reason = "credential_origin"
	case originDisposition == gitOriginAbsent:
		decision.Reason = "missing_origin"
	case originDisposition != gitOriginRemoteAttemptSafe || !gitOverlayOriginTransportSupported(coherence.RemoteURL):
		decision.Reason = "unsupported_origin_transport"
	case !coherence.enabled():
		decision.Reason = "unseedable_head"
	default:
		if err := validateGitOverlayManifest(repo, manifest); err != nil {
			decision.Reason = gitOverlayLocalFallbackReason(err)
		} else {
			decision.Enabled = true
		}
	}
	return decision
}

func gitOverlayOriginTransportSupported(remoteURL string) bool {
	return classifyGitOrigin(remoteURL) == gitOriginRemoteAttemptSafe
}

func classifyGitOrigin(remoteURL string) gitOriginDisposition {
	raw := strings.TrimSpace(remoteURL)
	if raw == "" {
		return gitOriginAbsent
	}
	if gitRemoteURLHasCredentials(raw) || strings.ContainsAny(raw, "?#") {
		return gitOriginNonForwardable
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return gitOriginNonForwardable
	}
	if parsed.Scheme != "" {
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https":
			if parsed.Host != "" {
				return gitOriginRemoteAttemptSafe
			}
		case "file":
			if parsed.Path != "" && (parsed.Host == "" || strings.EqualFold(parsed.Host, "localhost")) {
				return gitOriginRemoteAttemptSafe
			}
		}
		return gitOriginNonForwardable
	}
	if parsed.Host != "" || strings.HasPrefix(raw, "//") || strings.Contains(raw, ":") || strings.HasPrefix(raw, "-") {
		return gitOriginNonForwardable
	}
	return gitOriginRemoteAttemptSafe
}

func validateGitOverlayManifest(repo Repo, manifest SyncManifest) error {
	if repo.Root == "" || repo.Head == "" {
		return fmt.Errorf("missing_repo_identity")
	}
	checkout, err := captureGitOverlayCheckoutState(repo.Root)
	if err != nil {
		return err
	}
	return validateGitOverlayManifestAtState(repo, manifest, checkout)
}

func validateGitOverlayManifestAtState(repo Repo, manifest SyncManifest, checkout gitOverlayCheckoutState) error {
	if repo.Root == "" || repo.Head == "" {
		return fmt.Errorf("missing_repo_identity")
	}
	if gitCheckoutSparseEnabled(repo.Root) {
		return fmt.Errorf("sparse_checkout")
	}
	if checkout.Head == "" || checkout.Head != repo.Head {
		return fmt.Errorf("head_changed")
	}
	if err := validateGitOverlayCheckoutConfig(repo.Root); err != nil {
		return err
	}
	tracked, err := loadGitTrackedPaths(repo.Root)
	if err != nil {
		return fmt.Errorf("tracked_paths")
	}
	for _, entry := range tracked {
		if entry.stage != 0 {
			return fmt.Errorf("unmerged_index")
		}
		if entry.skipWorktree {
			return fmt.Errorf("skip_worktree")
		}
		if entry.assumeUnchanged {
			return fmt.Errorf("assume_unchanged_index")
		}
		if entry.mode == "160000" {
			return fmt.Errorf("gitlink")
		}
	}
	targetTree := exec.Command("git", "ls-tree", "-r", "-z", "--name-only", "--full-tree", repo.Head)
	targetTree.Dir = repo.Root
	targetTree.Env = repositoryGitEnvironment()
	targetPaths, err := targetTree.Output()
	if err != nil {
		return fmt.Errorf("git_attribute_inspection")
	}
	for _, inspection := range []struct {
		source string
		paths  []string
	}{
		{source: repo.Head, paths: splitNul(targetPaths)},
		{paths: manifest.Files},
	} {
		attribute, err := gitOverlayTransformAttribute(repo.Root, inspection.source, inspection.paths)
		if err != nil {
			return fmt.Errorf("git_attribute_inspection")
		}
		if attribute != "" {
			return fmt.Errorf("git_attribute_%s", attribute)
		}
	}
	overlayFiles, overlayBytes := overlayPathSetBytes(repo.Root, manifest.Files, manifest.Changed)
	if !slices.Equal(overlayFiles, manifest.OverlayFiles) || overlayBytes != manifest.OverlayBytes {
		return fmt.Errorf("overlay_manifest_changed")
	}
	deleted := make(map[string]struct{}, len(manifest.Deleted))
	for _, rel := range manifest.Deleted {
		deleted[rel] = struct{}{}
	}
	overlay := make(map[string]struct{}, len(manifest.OverlayFiles))
	for _, rel := range manifest.OverlayFiles {
		overlay[rel] = struct{}{}
		if err := validateGitOverlayPath(repo.Root, rel); err != nil {
			return err
		}
	}
	for _, rel := range manifest.Changed {
		if _, uploaded := overlay[rel]; !uploaded {
			if _, removed := deleted[rel]; !removed {
				return fmt.Errorf("changed_path_not_represented")
			}
		}
	}
	return nil
}

func validateGitOverlayCheckoutConfig(root string) error {
	checks := []struct {
		key     string
		boolean bool
		allowed []string
	}{
		{key: "core.autocrlf", allowed: []string{"false", "no", "off", "0"}},
		{key: "core.eol", allowed: []string{"lf"}},
		{key: "core.symlinks", boolean: true, allowed: []string{"true"}},
		{key: "core.filemode", boolean: true, allowed: []string{"true"}},
	}
	for _, check := range checks {
		args := []string{"config"}
		if check.boolean {
			args = append(args, "--type=bool")
		}
		args = append(args, "--get", check.key)
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = repositoryGitEnvironment()
		out, err := cmd.Output()
		if err != nil {
			if exitCode(err) == 1 {
				continue
			}
			return fmt.Errorf("%s_inspection", strings.ReplaceAll(check.key, ".", "_"))
		}
		if !slices.Contains(check.allowed, strings.ToLower(strings.TrimSpace(string(out)))) {
			return fmt.Errorf("%s", strings.ReplaceAll(check.key, ".", "_"))
		}
	}
	return nil
}

func gitOverlayTransformAttribute(root, source string, paths []string) (string, error) {
	if len(paths) == 0 {
		return "", nil
	}
	var input bytes.Buffer
	for _, rel := range paths {
		if !safeRepoRel(rel) {
			return "", fmt.Errorf("unsafe path")
		}
		input.WriteString(rel)
		input.WriteByte(0)
	}
	args := []string{"check-attr"}
	if source != "" {
		args = append(args, "--source="+source)
	}
	args = append(args, "-z", "--stdin")
	args = append(args, gitOverlayTransformAttributes...)
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = repositoryGitEnvironment()
	cmd.Stdin = &input
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	fields := bytes.Split(out, []byte{0})
	for index := 2; index < len(fields); index += 3 {
		if value := string(fields[index]); value != "" && value != "unspecified" && value != "unset" {
			return string(fields[index-1]), nil
		}
	}
	return "", nil
}

func validateGitOverlayPath(root, rel string) error {
	if !safeRepoRel(rel) {
		return fmt.Errorf("unsafe_path")
	}
	parts := strings.Split(filepath.FromSlash(rel), string(filepath.Separator))
	current := root
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("missing_overlay_path")
		}
		if index < len(parts)-1 {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink_ancestor")
			}
		} else if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("unsupported_file_type")
		}
	}
	return nil
}

func sameSyncManifest(left, right SyncManifest) bool {
	return slices.Equal(left.Files, right.Files) &&
		slices.Equal(left.Deleted, right.Deleted) &&
		slices.Equal(left.Changed, right.Changed) &&
		slices.Equal(left.OverlayFiles, right.OverlayFiles) &&
		slices.Equal(left.ProtectedTrackedExcludes, right.ProtectedTrackedExcludes) &&
		left.Bytes == right.Bytes && left.ChangedBytes == right.ChangedBytes && left.OverlayBytes == right.OverlayBytes
}

func gitOverlayLocalFallbackReason(err error) string {
	reason := strings.ToLower(strings.TrimSpace(err.Error()))
	if reason == "" {
		return "local_invariant"
	}
	var b strings.Builder
	lastUnderscore := false
	for _, r := range reason {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		case b.Len() != 0 && !lastUnderscore:
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func gitOverlayFallbackResult(output string, err error) (string, bool) {
	reason, fallback, _ := gitOverlayFallbackOutcome(output, err)
	return reason, fallback
}

func gitOriginRuntimeFallbackResult(remoteURL, output string, err error) (string, bool) {
	if err == nil || exitCode(err) != gitOriginRuntimeFallbackExitCode {
		return "", false
	}
	var truncated *gitOriginDiagnosticsTruncatedError
	if errors.As(err, &truncated) {
		return "", false
	}
	parsed, parseErr := url.Parse(strings.TrimSpace(remoteURL))
	isHTTP := parseErr == nil && (strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https"))
	switch {
	case isHTTP && gitOriginHTTPAuthError.MatchString(output):
		return "origin_auth_required", true
	case isHTTP && gitOriginHTTPServerError.MatchString(output):
		return "", false
	case gitOriginTransportError.MatchString(output):
		return "origin_unavailable", true
	case isHTTP && gitOriginHTTPNotFound.MatchString(output):
		return "origin_auth_required", true
	case !isHTTP && gitOriginFilesystemError.MatchString(output):
		return "origin_unavailable", true
	}
	return "", false
}

func gitOverlayFallbackOutcome(output string, err error) (string, bool, bool) {
	if err == nil || exitCode(err) != gitOverlayFallbackExitCode {
		return "", false, false
	}
	mutated := false
	reason := ""
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == gitOverlayMutationMarker {
			mutated = true
			continue
		}
		if value, ok := strings.CutPrefix(line, gitOverlayFallbackMarker); ok {
			reason = gitOverlayLocalFallbackReason(fmt.Errorf("%s", value))
		}
	}
	if reason == "" {
		reason = "remote_preparation_invariant"
	}
	return reason, true, mutated
}

// Hidden index flags omit worktree changes from ordinary fingerprint inputs.
func gitOverlayLocalFingerprintUnsafe(root string) bool {
	tracked, err := loadGitTrackedPaths(root)
	if err != nil {
		return true
	}
	for _, entry := range tracked {
		if entry.skipWorktree || entry.assumeUnchanged {
			return true
		}
	}
	return false
}

func gitOverlayBoundaryViolation(reason string) bool {
	switch reason {
	case "unsafe_remote_root", "symlink_remote_root", "symlink_remote_parent", "symlink_git_directory", "symlink_git_config", "symlink_git_objects", "symlink_git_info", "symlink_git_attributes", "symlink_git_exclude", "symlink_git_objects_info", "symlink_git_alternates", "unsafe_git_metadata", "unsafe_overlay_metadata", "unsafe_runtime_state":
		return true
	default:
		return false
	}
}

func gitOverlayHermeticFunctions() string {
	return `git() {
	local overlay_git_environment=()
	if [ -n "${GIT_INDEX_FILE:-}" ]; then overlay_git_environment+=("GIT_INDEX_FILE=$GIT_INDEX_FILE"); fi
	if [ -n "${overlay_no_lazy_fetch:-}" ]; then overlay_git_environment+=("GIT_NO_LAZY_FETCH=1"); fi
  /usr/bin/env -i HOME=/dev/null XDG_CONFIG_HOME=/dev/null PATH=/usr/bin:/bin LANG=C LC_ALL=C \
    GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null \
    GIT_ATTR_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0 GIT_ASKPASS=/bin/false SSH_ASKPASS=/bin/false \
    GCM_INTERACTIVE=Never GIT_SSH_COMMAND=/bin/false GIT_LFS_SKIP_SMUDGE=1 \
    GIT_TRACE_PACKET="${overlay_packet_trace:-0}" "${overlay_git_environment[@]}" \
    /usr/bin/git -c credential.helper= -c credential.interactive=never \
      -c core.hooksPath=/dev/null -c core.attributesFile=/dev/null -c core.excludesFile=/dev/null \
      -c core.fsmonitor=false -c core.autocrlf=false -c core.eol=lf \
      -c core.symlinks=true -c core.filemode=true -c protocol.allow=never \
      -c protocol.file.allow=always -c protocol.http.allow=always -c protocol.https.allow=always \
      -c protocol.ext.allow=never -c protocol.git.allow=never -c protocol.ssh.allow=never \
      -c fetch.recurseSubmodules=false -c submodule.recurse=false "$@"
}
overlay_workspace_safe() {
  checkout_root="$(cd -P -- "$1" 2>/dev/null && pwd -P)" || return 1
  [ -d "$checkout_root/.git" ] && [ ! -L "$checkout_root/.git" ] || return 1
  [ -f "$checkout_root/.git/config" ] && [ ! -L "$checkout_root/.git/config" ] || return 1
  [ -d "$checkout_root/.git/objects" ] && [ ! -L "$checkout_root/.git/objects" ] || return 1
  for metadata_dir in "$checkout_root/.git/info" "$checkout_root/.git/objects/info"; do
    if [ -e "$metadata_dir" ] || [ -L "$metadata_dir" ]; then
      [ -d "$metadata_dir" ] && [ ! -L "$metadata_dir" ] || return 1
    fi
  done
  for metadata_file in "$checkout_root/.git/info/attributes" "$checkout_root/.git/info/exclude" "$checkout_root/.git/objects/info/alternates"; do
    if [ -e "$metadata_file" ] || [ -L "$metadata_file" ]; then
      [ -f "$metadata_file" ] && [ ! -L "$metadata_file" ] || return 1
    fi
  done
  [ ! -s "$checkout_root/.git/objects/info/alternates" ] || return 1
  [ ! -s "$checkout_root/.git/info/attributes" ] || return 1
  set +e
  unsafe_keys="$(git config --file "$checkout_root/.git/config" --no-includes --name-only --get-regexp \
    '^(include([.]|$)|includeif[.]|url[.].*[.](insteadof|pushinsteadof)|protocol([.]|$)|http([.]|$)|credential([.]|$)|filter[.]|extensions[.]worktreeconfig|core[.](hookspath|attributesfile|excludesfile|fsmonitor|gitproxy|sshcommand|alternaterefscommand|worktree)|remote[.].*[.](uploadpack|receivepack|proxy|vcs))' 2>/dev/null)"
  config_status=$?
  set -e
  { [ "$config_status" -eq 0 ] || [ "$config_status" -eq 1 ]; } && [ -z "$unsafe_keys" ] || return 1
  checkout_git_dir="$(git -C "$checkout_root" rev-parse --absolute-git-dir 2>/dev/null)" || return 1
  checkout_common_dir="$(git -C "$checkout_root" rev-parse --git-common-dir 2>/dev/null)" || return 1
  case "$checkout_common_dir" in /*) ;; *) checkout_common_dir="$checkout_root/$checkout_common_dir" ;; esac
  checkout_git_dir="$(cd -P -- "$checkout_git_dir" && pwd -P)" || return 1
  checkout_common_dir="$(cd -P -- "$checkout_common_dir" && pwd -P)" || return 1
  expected_git_dir="$(cd -P -- "$checkout_root/.git" && pwd -P)" || return 1
  [ "$checkout_git_dir" = "$expected_git_dir" ] && [ "$checkout_common_dir" = "$expected_git_dir" ] || return 1
  checkout_index="$(git -C "$checkout_root" rev-parse --git-path index 2>/dev/null)" || return 1
  case "$checkout_index" in /*) ;; *) checkout_index="$checkout_root/$checkout_index" ;; esac
  [ "$checkout_index" = "$expected_git_dir/index" ] && [ ! -L "$checkout_index" ]
}
overlay_metadata_safe() {
  checkout_root="$(cd -P -- "$1" 2>/dev/null && pwd -P)" || return 1
  metadata_root="$checkout_root/.git/crabbox"
  if [ -L "$metadata_root" ]; then return 1; fi
  if [ ! -e "$metadata_root" ]; then return 0; fi
  [ -d "$metadata_root" ] || return 1
  resolved_metadata="$(cd -P -- "$metadata_root" && pwd -P)" || return 1
  [ "$resolved_metadata" = "$checkout_root/.git/crabbox" ] || return 1
  for metadata_path in "$metadata_root"/*; do
    if [ ! -e "$metadata_path" ] && [ ! -L "$metadata_path" ]; then continue; fi
    case "${metadata_path##*/}" in
      sync-manifest*|sync-deleted*|sync-fingerprint*|sync-finalize*|sync-git-status*|git-hydrate-base*)
        if [ -L "$metadata_path" ] || [ ! -f "$metadata_path" ]; then return 1; fi
        ;;
    esac
  done
}
overlay_runtime_state_safe() {
  checkout_root="$(cd -P -- "$1" 2>/dev/null && pwd -P)" || return 1
  runtime_root="$checkout_root/.crabbox"
  if [ -L "$runtime_root" ]; then return 1; fi
  if [ ! -e "$runtime_root" ]; then return 0; fi
  [ -d "$runtime_root" ] || return 1
  [ "$(cd -P -- "$runtime_root" && pwd -P)" = "$runtime_root" ] || return 1
  for runtime_name in env scripts logs captures runs; do
    runtime_path="$runtime_root/$runtime_name"
    if [ -L "$runtime_path" ] || { [ -e "$runtime_path" ] && [ ! -d "$runtime_path" ]; }; then return 1; fi
  done
}
overlay_git_metadata_safe() (
  checkout_root="$(cd -P -- "$1" 2>/dev/null && pwd -P)" || return 1
  checkout_git_dir="$(git -C "$checkout_root" rev-parse --absolute-git-dir 2>/dev/null)" || return 1
  [ ! -L "$checkout_root/.git" ] && [ -d "$checkout_root/.git" ] || return 1
  checkout_git_dir="$(cd -P -- "$checkout_git_dir" 2>/dev/null && pwd -P)" || return 1
  [ "$checkout_git_dir" = "$checkout_root/.git" ] || return 1
  for metadata_dir in objects refs logs; do
    metadata_path="$checkout_git_dir/$metadata_dir"
    if [ -e "$metadata_path" ] || [ -L "$metadata_path" ]; then
      [ ! -L "$metadata_path" ] && [ -d "$metadata_path" ] || return 1
      [ "$(cd -P -- "$metadata_path" 2>/dev/null && pwd -P)" = "$metadata_path" ] || return 1
      if ! /usr/bin/find -P "$metadata_path" -type l -print -quit > "$git_runtime_root/git-metadata-symlinks"; then return 1; fi
      [ ! -s "$git_runtime_root/git-metadata-symlinks" ] || return 1
    fi
  done
  for metadata_file in HEAD config config.worktree index packed-refs shallow; do
    metadata_path="$checkout_git_dir/$metadata_file"
    if [ -e "$metadata_path" ] || [ -L "$metadata_path" ]; then
      [ ! -L "$metadata_path" ] && [ -f "$metadata_path" ] || return 1
    fi
  done
)
`
}

func remoteGitOverlayShellCommand(script string) string {
	return "/usr/bin/env -i PATH=/usr/bin:/bin LANG=C LC_ALL=C BASH_ENV=/dev/null ENV=/dev/null /bin/bash --noprofile --norc -c " + shellQuote(script)
}

func remoteDiscardGitOverlaySyncPendingMetadata(workdir, finalizeToken string) string {
	script := `set -e
cd ` + shellQuote(workdir) + `
` + gitOverlayHermeticFunctions() + remotePlainSyncMetaDirScript() + `
/bin/rm -f -- "$meta_dir/` + remoteSyncPendingManifestName(finalizeToken) + `" "$meta_dir/` + remoteSyncPendingDeletedName(finalizeToken) + `"
`
	return remoteGitOverlayShellCommand(script)
}

func remotePrepareGitOverlay(workdir string, plan gitCoherencePlan) string {
	return remotePrepareGitOverlayWithHint(workdir, plan, "", "", "")
}

func remotePrepareGitOverlayWithBase(workdir string, plan gitCoherencePlan, baseRef, baseSHA string) string {
	return remotePrepareGitOverlayWithHint(workdir, plan, baseRef, baseSHA, "")
}

func remotePrepareGitOverlayWithHint(workdir string, plan gitCoherencePlan, baseRef, baseSHA, fingerprint string) string {
	if !plan.enabled() || !gitOverlayOriginTransportSupported(plan.RemoteURL) {
		return "printf '" + gitOverlayFallbackMarker + "unsupported_origin_transport\\n' >&2; exit 78"
	}
	parent := filepath.ToSlash(filepath.Dir(workdir))
	script := `set -e -o pipefail
workdir=` + shellQuote(workdir) + `
parent=` + shellQuote(parent) + `
expected_origin=` + shellQuote(plan.RemoteURL) + `
expected_target=` + shellQuote(plan.Target) + `
expected_tree=` + shellQuote(plan.Tree) + `
advertised_branch=` + shellQuote(plan.Branch) + `
base_ref=` + shellQuote(baseRef) + `
expected_base=` + shellQuote(baseSHA) + `
expected_fingerprint=` + shellQuote(fingerprint) + `
overlay_mutated=
overlay_fallback() {
  if [ -n "$overlay_mutated" ]; then printf '%s\n' ` + shellQuote(gitOverlayMutationMarker) + ` >&2; fi
  printf '%s%s\n' ` + shellQuote(gitOverlayFallbackMarker) + ` "$1" >&2
  exit 78
}
case "$workdir" in "$parent"/*) ;; *) overlay_fallback unsafe_remote_root ;; esac
if [ -L "$workdir" ]; then overlay_fallback symlink_remote_root; fi
if [ -L "$parent" ]; then overlay_fallback symlink_remote_parent; fi
for prerequisite in /bin/bash /bin/cat /bin/mkdir /bin/rm /bin/mv /bin/cp /usr/bin/env /usr/bin/git /usr/bin/find /usr/bin/mktemp /usr/bin/awk /usr/bin/grep; do
  [ -x "$prerequisite" ] || overlay_fallback remote_prerequisite_missing
done
/bin/mkdir -p "$parent"
canonical_parent="$(cd -P -- "$parent" 2>/dev/null && pwd -P)" || overlay_fallback symlink_remote_parent
git_overlay_parent_safe() {
  [ ! -L "$parent" ] && [ -d "$parent" ] || return 1
  [ "$(cd -P -- "$parent" 2>/dev/null && pwd -P)" = "$canonical_parent" ]
}
git_overlay_parent_safe || overlay_fallback symlink_remote_parent
git_runtime_root="$(/usr/bin/mktemp -d "$parent/.overlay-git.XXXXXX")" || overlay_fallback remote_prerequisite_missing
seed_tmp=
baseline_tmp=
cleanup_git_overlay() {
  cleanup_code=$?
  if [ -n "$baseline_tmp" ]; then /bin/rm -f -- "$baseline_tmp"; fi
  if [ -n "$seed_tmp" ]; then /bin/rm -rf -- "$seed_tmp"; fi
  /bin/rm -rf -- "$git_runtime_root"
  trap - EXIT
  if [ "$cleanup_code" -ne 0 ]; then exit "$cleanup_code"; fi
}
trap cleanup_git_overlay EXIT
` + gitOverlayHermeticFunctions() + `
overlay_no_lazy_fetch=1
if [ -n "$base_ref" ] && [ -z "$expected_base" ]; then overlay_fallback base_ref_unavailable; fi
reuse_hint_valid=
if [ -e "$workdir/.git" ] || [ -L "$workdir/.git" ]; then
	if [ -L "$workdir/.git" ]; then overlay_fallback symlink_git_directory; fi
	if [ -L "$workdir/.git/config" ]; then overlay_fallback symlink_git_config; fi
	if [ -L "$workdir/.git/objects" ]; then overlay_fallback symlink_git_objects; fi
  if [ -L "$workdir/.git/info" ]; then overlay_fallback symlink_git_info; fi
  if [ -L "$workdir/.git/info/attributes" ]; then overlay_fallback symlink_git_attributes; fi
  if [ -L "$workdir/.git/info/exclude" ]; then overlay_fallback symlink_git_exclude; fi
  if [ -L "$workdir/.git/objects/info" ]; then overlay_fallback symlink_git_objects_info; fi
  if [ -L "$workdir/.git/objects/info/alternates" ]; then overlay_fallback symlink_git_alternates; fi
  overlay_workspace_safe "$workdir" || overlay_fallback unsafe_git_workspace
  overlay_git_metadata_safe "$workdir" || overlay_fallback unsafe_git_metadata
  overlay_metadata_safe "$workdir" || overlay_fallback unsafe_overlay_metadata
  overlay_runtime_state_safe "$workdir" || overlay_fallback unsafe_runtime_state
  [ "$(git -C "$workdir" remote get-url origin 2>/dev/null || true)" = "$expected_origin" ] || overlay_fallback origin_mismatch
  checkout_root="$workdir"
  meta_dir="$checkout_root/.git/crabbox"
  committed="$meta_dir/sync-finalize-token"
  complete="$meta_dir/sync-finalize-complete-token"
  published="$meta_dir/sync-fingerprint"
  committed_value=
  complete_value=
  if [ -n "$expected_fingerprint" ] &&
     [ -f "$committed" ] && [ ! -L "$committed" ] &&
     [ -f "$complete" ] && [ ! -L "$complete" ] &&
     [ -f "$published" ] && [ ! -L "$published" ]; then
    committed_value="$(/bin/cat "$committed" 2>/dev/null || true)"
    complete_value="$(/bin/cat "$complete" 2>/dev/null || true)"
    if [ -n "$committed_value" ] &&
       [ "$committed_value" = "$complete_value" ] &&
       [ "$(/bin/cat "$published" 2>/dev/null || true)" = "$expected_fingerprint" ] &&
       [ "$(git -C "$checkout_root" rev-parse --verify HEAD^{commit} 2>/dev/null || true)" = "$expected_target" ] &&
       [ "$(git -C "$checkout_root" write-tree 2>/dev/null || true)" = "$expected_tree" ] &&
       [ "$(git -C "$checkout_root" rev-parse --verify "$expected_target^{tree}" 2>/dev/null || true)" = "$expected_tree" ] &&
       git -C "$checkout_root" merge-base --is-ancestor "$expected_target" "refs/remotes/origin/$advertised_branch" >/dev/null 2>&1; then
      reuse_hint_valid=1
      if [ -n "$base_ref" ]; then
        if [ ! -f "$meta_dir/git-hydrate-base" ] ||
           [ -L "$meta_dir/git-hydrate-base" ] ||
           [ "$(/bin/cat "$meta_dir/git-hydrate-base" 2>/dev/null || true)" != "$base_ref $expected_base" ] ||
           [ "$(git -C "$checkout_root" rev-parse --verify "refs/remotes/origin/$base_ref^{commit}" 2>/dev/null || true)" != "$expected_base" ] ||
           ! git -C "$checkout_root" merge-base --is-ancestor "$expected_base" "$expected_target" >/dev/null 2>&1; then
          reuse_hint_valid=
        fi
      fi
    fi
  fi
elif [ -d "$workdir" ]; then
  overlay_fallback non_git_workspace
fi
if [ ! -d "$workdir/.git" ]; then
  git_overlay_parent_safe || overlay_fallback symlink_remote_parent
  seed_tmp="$(/usr/bin/mktemp -d "$parent/.overlay-seed.XXXXXX")"
  git init --quiet "$seed_tmp" || overlay_fallback checkout_init_failed
  git -C "$seed_tmp" remote add origin "$expected_origin" || overlay_fallback checkout_init_failed
  checkout_root="$seed_tmp"
fi
git -C "$checkout_root" ls-files -t -z >"$git_runtime_root/index-skip-flags" || overlay_fallback index_inspection_failed
while IFS= read -r -d '' index_entry; do
  case "$index_entry" in
    S\ *) overlay_fallback skip_worktree_index ;;
  esac
done <"$git_runtime_root/index-skip-flags"
git -C "$checkout_root" ls-files -v -z >"$git_runtime_root/index-assume-flags" || overlay_fallback index_inspection_failed
while IFS= read -r -d '' index_entry; do
  case "$index_entry" in
    [a-z]\ *) overlay_fallback assume_unchanged_index ;;
  esac
done <"$git_runtime_root/index-assume-flags"
if [ -z "$reuse_hint_valid" ]; then
  overlay_no_lazy_fetch=
  overlay_packet_trace="$git_runtime_root/packets"
  set +e
  git -C "$git_runtime_root" -c protocol.version=2 ls-remote --exit-code --heads "$expected_origin" \
    "refs/heads/$advertised_branch" "refs/heads/$base_ref" >"$git_runtime_root/advertised" 2>"$git_runtime_root/transport-error"
  transport_status=$?
  overlay_packet_trace=
  set -e
  if [ "$transport_status" -ne 0 ]; then
    if [ "$transport_status" -eq 2 ]; then overlay_fallback advertised_branch_missing; fi
    if /usr/bin/grep -Eiq ` + shellQuote(gitOriginHTTPAuthPattern+"|repository not found") + ` "$git_runtime_root/transport-error"; then
      overlay_fallback origin_auth_required
    fi
    overlay_fallback origin_unavailable
  fi
  if ! /usr/bin/grep -Eq 'packet:.*< fetch=.*filter' "$git_runtime_root/packets"; then
    overlay_fallback filtered_history_unsupported
  fi
  advertised_target="$(/usr/bin/awk -v branch="refs/heads/$advertised_branch" '$2 == branch { print $1; exit }' "$git_runtime_root/advertised")"
  [ -n "$advertised_target" ] || overlay_fallback advertised_branch_missing
  if [ -n "$base_ref" ]; then
    advertised_base="$(/usr/bin/awk -v branch="refs/heads/$base_ref" '$2 == branch { print $1; exit }' "$git_runtime_root/advertised")"
    [ -n "$advertised_base" ] || overlay_fallback base_ref_missing
    [ "$advertised_base" = "$expected_base" ] || overlay_fallback base_ref_mismatch
  fi
  git -C "$checkout_root" config --local remote.origin.promisor true || overlay_fallback filtered_history_unsupported
  git -C "$checkout_root" config --local remote.origin.partialclonefilter blob:none || overlay_fallback filtered_history_unsupported
  fetch_args=(--quiet --no-tags --no-write-fetch-head --filter=blob:none)
  if [ "$(git -C "$checkout_root" rev-parse --is-shallow-repository 2>/dev/null || true)" = true ]; then
    fetch_args+=(--unshallow)
  fi
  refspecs=("+refs/heads/$advertised_branch:refs/remotes/origin/$advertised_branch")
  if [ -n "$base_ref" ] && [ "$base_ref" != "$advertised_branch" ]; then
    refspecs+=("+refs/heads/$base_ref:refs/remotes/origin/$base_ref")
  fi
  set +e
  git -C "$checkout_root" fetch "${fetch_args[@]}" origin "${refspecs[@]}" 2>"$git_runtime_root/transport-error"
  transport_status=$?
  set -e
  if [ "$transport_status" -ne 0 ]; then
    if /usr/bin/grep -Eiq ` + shellQuote(gitOriginHTTPAuthPattern+"|repository not found") + ` "$git_runtime_root/transport-error"; then
      overlay_fallback origin_auth_required
    fi
    overlay_fallback origin_unavailable
  fi
  if /usr/bin/grep -Eiq 'filtering not recognized|filtering not supported|server does not support filter' "$git_runtime_root/transport-error"; then
    overlay_fallback filtered_history_unsupported
  fi
  fetched_branch="$(git -C "$checkout_root" rev-parse --verify "refs/remotes/origin/$advertised_branch^{commit}" 2>/dev/null || true)"
  [ "$fetched_branch" = "$advertised_target" ] || overlay_fallback advertised_branch_changed
  git -C "$checkout_root" cat-file -e "$expected_target^{commit}" 2>/dev/null || overlay_fallback target_missing
  git -C "$checkout_root" merge-base --is-ancestor "$expected_target" "$fetched_branch" >/dev/null 2>&1 || overlay_fallback target_not_advertised
  [ "$(git -C "$checkout_root" rev-parse --verify "$expected_target^{tree}" 2>/dev/null || true)" = "$expected_tree" ] || overlay_fallback tree_mismatch
  if [ -n "$base_ref" ] && [ "$(git -C "$checkout_root" rev-parse --verify "refs/remotes/origin/$base_ref^{commit}" 2>/dev/null || true)" != "$expected_base" ]; then
    overlay_fallback base_ref_mismatch
  fi
fi
overlay_metadata_safe "$checkout_root" || overlay_fallback unsafe_overlay_metadata
if [ -f "$checkout_root/.git/crabbox/sync-manifest" ]; then
  /bin/cp -- "$checkout_root/.git/crabbox/sync-manifest" "$git_runtime_root/previous-manifest"
fi
/bin/mkdir -p "$checkout_root/.git/crabbox"
overlay_metadata_safe "$checkout_root" || overlay_fallback unsafe_overlay_metadata
baseline_tmp="$(/usr/bin/mktemp "$checkout_root/.git/crabbox/sync-manifest.overlay.XXXXXX")" || overlay_fallback baseline_failed
if [ -f "$git_runtime_root/previous-manifest" ]; then
  /bin/cp -- "$git_runtime_root/previous-manifest" "$baseline_tmp" || overlay_fallback baseline_failed
else
  : > "$baseline_tmp" || overlay_fallback baseline_failed
fi
git -C "$checkout_root" ls-tree -r -z --name-only --full-tree "$expected_target" >> "$baseline_tmp" || overlay_fallback baseline_failed
/bin/mv -- "$baseline_tmp" "$checkout_root/.git/crabbox/sync-manifest" || overlay_fallback baseline_failed
baseline_tmp=
if [ -n "$seed_tmp" ]; then
  git_overlay_parent_safe || overlay_fallback symlink_remote_parent
  /bin/mv -- "$seed_tmp" "$workdir"
  seed_tmp=
fi
overlay_mutated=1
cd "$workdir"
workdir="$(pwd -P)"
overlay_workspace_safe "$workdir" || overlay_fallback unsafe_git_workspace
overlay_metadata_safe "$workdir" || overlay_fallback unsafe_overlay_metadata
overlay_runtime_state_safe "$workdir" || overlay_fallback unsafe_runtime_state
if [ "$(git config --bool core.sparseCheckout 2>/dev/null || true)" = true ] ||
   [ "$(git config --bool core.sparseCheckoutCone 2>/dev/null || true)" = true ]; then
  overlay_fallback sparse_checkout
fi
git checkout --quiet --force --detach "$expected_target" || overlay_fallback checkout_failed
git reset --hard --quiet "$expected_target" || overlay_fallback reset_failed
clean_args=(-ffdx --quiet -e /.crabbox/)
/usr/bin/find -P . \( -ipath './.git' -o -ipath './.crabbox' \) -prune -o \
  \( -type d -o -type l \) \( -iname node_modules -o -iname .pnpm-store -o -ipath '*/.yarn/cache' -o -ipath '*/.yarn/unplugged' \) \
  -print0 -prune >"$git_runtime_root/cache-paths" || overlay_fallback cache_discovery_failed
while IFS= read -r -d '' cache_path; do
  cache_lookup_path="$cache_path"
  cache_path="${cache_path#./}"
  if [ -L "$cache_path" ]; then overlay_fallback unsafe_cache_root; fi
  set +e
  printf '%s\0' "$cache_lookup_path" | git check-ignore --no-index -v -z --stdin >"$git_runtime_root/cache-ignore" 2>/dev/null
  ignore_status=$?
  set -e
  if [ "$ignore_status" -eq 1 ]; then continue; fi
  [ "$ignore_status" -eq 0 ] || overlay_fallback cache_ignore_failed
  {
    IFS= read -r -d '' ignore_source && IFS= read -r -d '' ignore_line &&
      IFS= read -r -d '' ignore_pattern && IFS= read -r -d '' ignore_path
  } <"$git_runtime_root/cache-ignore" || overlay_fallback cache_ignore_failed
  case "$ignore_source" in .gitignore|*/.gitignore) ;; *) continue ;; esac
  case "$ignore_source" in /*|../*|*/../*) continue ;; esac
  [ -f "$ignore_source" ] && [ ! -L "$ignore_source" ] || overlay_fallback cache_ignore_untrusted
  expected_ignore="$(git rev-parse --verify "$expected_target:$ignore_source" 2>/dev/null || true)"
  actual_ignore="$(git hash-object --no-filters -- "$ignore_source" 2>/dev/null || true)"
  [ -n "$expected_ignore" ] && [ "$actual_ignore" = "$expected_ignore" ] || continue
  if [ -L "$cache_path" ] || [ ! -d "$cache_path" ]; then overlay_fallback unsafe_cache_root; fi
  resolved_cache="$(cd -P -- "$cache_path" && pwd -P)" || overlay_fallback unsafe_cache_root
  case "$resolved_cache/" in "$workdir"/*) ;; *) overlay_fallback unsafe_cache_root ;; esac
  cache_pattern="${cache_path//\\/\\\\}"
  cache_pattern="${cache_pattern//\*/\\*}"
  cache_pattern="${cache_pattern//\?/\\?}"
  cache_pattern="${cache_pattern//\[/\\[}"
  cache_pattern="${cache_pattern//\]/\\]}"
  cache_pattern="${cache_pattern//!/\\!}"
  cache_pattern="${cache_pattern//#/\\#}"
  clean_args+=(-e "$cache_pattern/")
done <"$git_runtime_root/cache-paths"
git clean "${clean_args[@]}" || overlay_fallback clean_failed
[ "$(git rev-parse --verify HEAD^{commit})" = "$expected_target" ] || overlay_fallback head_mismatch
[ "$(git write-tree)" = "$expected_tree" ] || overlay_fallback index_tree_mismatch
overlay_metadata_safe "$workdir" || overlay_fallback unsafe_overlay_metadata
/bin/rm -f -- .git/crabbox/sync-fingerprint .git/crabbox/git-hydrate-base
`
	return remoteGitOverlayShellCommand(script)
}

func remotePruneGitOverlaySyncManifest(workdir, finalizeToken string, allowMassDeletions ...bool) string {
	return remotePruneSafeSyncManifest(workdir, finalizeToken, remotePlainSyncMetaDirScript(), allowMassDeletions...)
}

func remotePruneSafeSyncManifest(workdir, finalizeToken, metadataScript string, allowMassDeletions ...bool) string {
	manifestName := remoteSyncPendingManifestName(finalizeToken)
	deletedName := remoteSyncPendingDeletedName(finalizeToken)
	allowValue := "0"
	if len(allowMassDeletions) != 0 && allowMassDeletions[0] {
		allowValue = "1"
	}
	python := `import os, stat, sys
def read(path):
    if not os.path.isfile(path):
        return []
    with open(path, "rb") as stream:
        data = stream.read()
    if data and not data.endswith(b"\0"):
        raise SystemExit("malformed overlay deletion manifest")
    entries = data.split(b"\0")[:-1]
    for entry in entries:
        parts = entry.split(b"/")
        if not entry or entry.startswith(b"/") or any(part in (b"", b".", b"..", b".git", b".crabbox") for part in parts):
            raise SystemExit("unsafe overlay deletion path")
    return entries
def shape(path):
    current = b""
    parts = path.split(b"/")
    for index, part in enumerate(parts):
        current = part if not current else current + b"/" + part
        try:
            mode = os.lstat(current).st_mode
        except (FileNotFoundError, NotADirectoryError):
            return None
        if index + 1 < len(parts) and not stat.S_ISDIR(mode):
            return None
    return mode
def leaf(path):
    mode = shape(path)
    return mode is not None and not stat.S_ISDIR(mode)
old, new_entries, deleted = (read(path) for path in sys.argv[1:4])
new = set(new_entries)
pending = []
seen = set()
for path in deleted + old:
    if path in seen or path in new:
        continue
    if any(current.startswith(path + b"/") for current in new) and stat.S_ISDIR(shape(path) or 0):
        continue
    if any(path.startswith(current + b"/") and leaf(current) for current in new):
        continue
    seen.add(path)
    pending.append(path)
if sys.argv[4] != "1" and len(pending) >= 200:
    raise SystemExit("remote sync sanity failed: %d pending deletions" % len(pending))
sys.stdout.buffer.write(b"".join(path + b"\0" for path in pending))`
	perl := `use strict; use warnings;
sub read_manifest {
  return () unless -f $_[0]; open my $fh, "<", $_[0] or die "open: $!"; binmode $fh; local $/; my $data = <$fh>; $data //= "";
  die "malformed overlay deletion manifest\n" if length($data) && substr($data, -1) ne "\0";
  my @entries = split /\0/, $data, -1; pop @entries if @entries;
  for my $entry (@entries) {
    my @parts = split m{/}, $entry, -1;
    die "unsafe overlay deletion path\n" if !length($entry) || $entry =~ m{^/} || grep { $_ eq "" || $_ eq "." || $_ eq ".." || $_ eq ".git" || $_ eq ".crabbox" } @parts;
  }
  return @entries;
}
sub shape {
  my @parts = split m{/}, $_[0]; my $current = "";
  for my $index (0 .. $#parts) {
    $current = length($current) ? "$current/$parts[$index]" : $parts[$index];
    my @state = lstat($current); return undef unless @state;
    return undef if $index < $#parts && ($state[2] & 0170000) != 0040000;
    return $state[2] if $index == $#parts;
  }
  return undef;
}
sub leaf {
  my $mode = shape($_[0]); return defined($mode) && ($mode & 0170000) != 0040000;
}
my @old = read_manifest($ARGV[0]); my @entries = read_manifest($ARGV[1]); my @deleted = read_manifest($ARGV[2]);
my %new = map { $_ => 1 } @entries; my %seen; my @pending;
for my $path (@deleted, @old) {
  next if $seen{$path}++ || $new{$path};
  my $path_shape = shape($path);
  next if defined($path_shape) && ($path_shape & 0170000) == 0040000 && grep { index($_, $path . "/") == 0 } @entries;
  next if grep { index($path, $_ . "/") == 0 && leaf($_) } @entries;
  push @pending, $path;
}
die "remote sync sanity failed: " . scalar(@pending) . " pending deletions\n" if $ARGV[3] ne "1" && @pending >= 200;
binmode STDOUT; print STDOUT map { $_ . "\0" } @pending;`
	script := `set -e -o pipefail
cd ` + shellQuote(workdir) + `
` + gitOverlayHermeticFunctions() + metadataScript + `
old="$meta_dir/sync-manifest"
new="$meta_dir/` + manifestName + `"
deleted="$meta_dir/` + deletedName + `"
delete_paths() {
  while IFS= read -r -d '' rel; do
    case "$rel" in ''|/*|../*|*/../*|.git|.git/*|*/.git|*/.git/*|.crabbox|.crabbox/*|*/.crabbox|*/.crabbox/*) echo "unsafe overlay deletion path" >&2; exit 67 ;; esac
    remainder="$rel"
    ancestor=
    while [ "$remainder" != "${remainder#*/}" ]; do
      component="${remainder%%/*}"
      remainder="${remainder#*/}"
      if [ -z "$ancestor" ]; then ancestor="$component"; else ancestor="$ancestor/$component"; fi
      if [ -L "$ancestor" ]; then echo "overlay deletion refuses symlink ancestor: $ancestor" >&2; exit 67; fi
    done
    /bin/rm -f -- "$rel"
    dir=$(/usr/bin/dirname -- "$rel")
    while [ "$dir" != . ] && [ "$dir" != / ]; do
      if [ -L "$dir" ]; then echo "overlay deletion refuses symlink ancestor: $dir" >&2; exit 67; fi
      /bin/rmdir -- "$dir" 2>/dev/null || break
      dir=$(/usr/bin/dirname -- "$dir")
    done
  done
}
` + remoteSyncInterpreterCommand(python, perl, "\"$old\" \"$new\" \"$deleted\" "+shellQuote(allowValue)) + ` | delete_paths
`
	return remoteGitOverlayShellCommand(script)
}
