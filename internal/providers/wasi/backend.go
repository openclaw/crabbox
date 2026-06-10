package wasi

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// WASI gives Wasm modules a standards-track system interface while the host
// decides which capabilities are available. This provider uses wazero by default
// and wasmtime as an opt-in CLI runtime, then mounts only the synced guest
// workdir as /work plus stdio, args, and explicit environment variables.

const (
	defaultWasiWorkdir = "crabbox"
	defaultWasiRuntime = "wazero"
)

type wasiFlagValues struct {
	Workdir *string
	Runtime *string
}

func RegisterWasiProviderFlags(fs *flag.FlagSet, defaults Config) any {
	wd := defaultWasiWorkdir
	if defaults.Wasi.Workdir != "" {
		wd = defaults.Wasi.Workdir
	}
	rt := defaultWasiRuntime
	if defaults.Wasi.Runtime != "" {
		rt = defaults.Wasi.Runtime
	}
	return wasiFlagValues{
		Workdir: fs.String("wasi-workdir", wd, "WASI guest working directory (preopened as /work)"),
		Runtime: fs.String("wasi-runtime", rt, "WASI runtime: wazero (embedded, default) or wasmtime (if in PATH)"),
	}
}

func ApplyWasiProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	// Reject --class/--type for the wasi provider and all its aliases (wasm, wazero).
	// Apply may be called while cfg.Provider is still an alias (before resolution in loadBackend).
	if cfg.Provider == providerName || cfg.Provider == "wasm" || cfg.Provider == "wazero" {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s", providerName)
		}
	}
	v, ok := values.(wasiFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "wasi-workdir") {
		workdir, err := cleanWasiWorkdir(*v.Workdir)
		if err != nil {
			return err
		}
		cfg.Wasi.Workdir = workdir
	}
	if flagWasSet(fs, "wasi-runtime") {
		runtime, err := normalizeWasiRuntime(*v.Runtime)
		if err != nil {
			return err
		}
		cfg.Wasi.Runtime = runtime
	}
	return nil
}

func NewBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	if cfg.Wasi.Workdir == "" {
		cfg.Wasi.Workdir = defaultWasiWorkdir
	}
	if cfg.Wasi.Runtime == "" {
		cfg.Wasi.Runtime = defaultWasiRuntime
	} else if runtime, err := normalizeWasiRuntime(cfg.Wasi.Runtime); err == nil {
		cfg.Wasi.Runtime = runtime
	}
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) now() time.Time { return nowFrom(b.rt) }

func (b *backend) Warmup(ctx context.Context, req WarmupRequest) error {
	if req.ActionsRunner {
		return exit(2, "--actions-runner is not supported for provider=%s", providerName)
	}
	started := b.now()
	leaseID, slug, err := b.ensureLease(req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
	if err != nil {
		return err
	}
	guestRoot, syncDur, err := b.prepareGuest(ctx, req.Repo.Root, leaseID, false, false)
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stdout, "leased %s slug=%s provider=%s guest=%s\n", leaseID, slug, providerName, guestRoot)
	if !req.Keep {
		fmt.Fprintf(b.rt.Stderr, "warning: wasi warmup keeps the guest dir until explicit stop\n")
	}
	total := b.now().Sub(started)
	fmt.Fprintf(b.rt.Stdout, "warmup complete total=%s\n", total.Round(time.Millisecond))
	if req.TimingJSON {
		return writeTimingJSON(b.rt.Stderr, timingReport{
			Provider: providerName,
			LeaseID:  leaseID,
			Slug:     slug,
			TotalMs:  total.Milliseconds(),
			SyncMs:   syncDur.Milliseconds(),
			ExitCode: 0,
		})
	}
	return nil
}

func (b *backend) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	started := b.now()
	command := req.Command
	if !req.SyncOnly && len(command) == 0 {
		return RunResult{}, exit(2, "missing command (provide a .wasm module)")
	}
	pureNoFS := !req.SyncOnly && isPureNoFSBuiltin(command)
	noSync := req.NoSync || pureNoFS

	leaseID, slug, err := b.ensureLeaseForRun(req)
	if err != nil {
		return RunResult{}, err
	}
	acquired := req.ID == ""
	shouldStop := acquired && !req.Keep
	if shouldStop {
		defer func() {
			if !shouldStop {
				return
			}
			b.cleanupGuest(leaseID)
			removeLeaseClaim(leaseID)
		}()
	}

	guestRoot, syncDur, err := b.prepareGuest(ctx, req.Repo.Root, leaseID, noSync, req.ForceSyncLarge)
	if err != nil {
		return RunResult{Total: b.now().Sub(started), SyncDelegated: true}, err
	}

	if req.SyncOnly {
		result := RunResult{
			Total:         b.now().Sub(started),
			SyncDelegated: true,
			Provider:      providerName,
			LeaseID:       leaseID,
			Slug:          slug,
			Session: &RunSessionHandle{
				Provider:       providerName,
				LeaseID:        leaseID,
				Slug:           slug,
				Reused:         !acquired,
				Kept:           !shouldStop,
				CleanupCommand: wasiCleanupCommand(leaseID),
			},
		}
		fmt.Fprintf(b.rt.Stdout, "synced %s\n", guestRoot)
		if req.TimingJSON {
			_ = writeTimingJSON(b.rt.Stderr, timingReport{
				Provider:      providerName,
				LeaseID:       leaseID,
				Slug:          slug,
				SyncDelegated: true,
				SyncMs:        syncDur.Milliseconds(),
				SyncSkipped:   noSync,
				TotalMs:       result.Total.Milliseconds(),
				ExitCode:      0,
			})
		}
		return result, nil
	}

	fmt.Fprintf(b.rt.Stderr, "running on wasi %s\n", strings.Join(command, " "))

	exitCode, cmdDur, runErr := b.executeCommand(ctx, guestRoot, req, command)

	total := b.now().Sub(started)
	result := RunResult{
		ExitCode:      exitCode,
		Command:       cmdDur,
		Total:         total,
		SyncDelegated: true,
		Provider:      providerName,
		LeaseID:       leaseID,
		Slug:          slug,
		CommandText:   strings.Join(command, " "),
		Session: &RunSessionHandle{
			Provider:       providerName,
			LeaseID:        leaseID,
			Slug:           slug,
			Reused:         !acquired,
			Kept:           !shouldStop,
			CleanupCommand: wasiCleanupCommand(leaseID),
		},
	}

	if noSync {
		fmt.Fprintf(b.rt.Stderr, "wasi run summary sync_skipped=true command=%s total=%s exit=%d\n", result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	} else {
		fmt.Fprintf(b.rt.Stderr, "wasi run summary sync=%s command=%s total=%s exit=%d\n", syncDur.Round(time.Millisecond), result.Command.Round(time.Millisecond), result.Total.Round(time.Millisecond), result.ExitCode)
	}

	if req.TimingJSON {
		_ = writeTimingJSON(b.rt.Stderr, timingReport{
			Provider:      providerName,
			LeaseID:       leaseID,
			Slug:          slug,
			SyncDelegated: true,
			SyncMs:        syncDur.Milliseconds(),
			SyncSkipped:   noSync,
			CommandMs:     cmdDur.Milliseconds(),
			TotalMs:       result.Total.Milliseconds(),
			ExitCode:      result.ExitCode,
		})
	}

	if runErr != nil {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, runErr
	}
	if result.ExitCode != 0 {
		handleDelegatedRunFailure(b.rt.Stderr, req, providerName, leaseID, slug, b.cfg.IdleTimeout, b.cfg.TTL, acquired, &shouldStop)
		return result, ExitError{Code: result.ExitCode, Message: fmt.Sprintf("wasi run exited %d", result.ExitCode)}
	}
	return result, nil
}

func (b *backend) List(ctx context.Context, req ListRequest) ([]LeaseView, error) {
	_ = ctx
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, err
	}
	out := make([]LeaseView, 0, len(claims))
	for _, claim := range claims {
		if claim.Provider != providerName {
			continue
		}
		state := b.leaseState(claim.LeaseID)
		if state == "not-found" && !req.All {
			continue
		}
		out = append(out, wasiClaimToServer(claim, state))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (b *backend) Status(ctx context.Context, req StatusRequest) (StatusView, error) {
	_ = ctx
	leaseID := req.ID
	if leaseID == "" {
		return StatusView{}, exit(2, "missing --id for wasi status")
	}
	canonical, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil {
		return StatusView{}, err
	}
	if !ok {
		return StatusView{ID: leaseID, Provider: providerName, State: "not-found"}, nil
	}
	leaseID = canonical.LeaseID
	guest, err := b.guestRootPath(leaseID)
	if err != nil {
		return StatusView{}, err
	}
	if _, err := os.Stat(guest); err != nil {
		if os.IsNotExist(err) {
			return wasiStatusView(canonical, "not-found"), nil
		}
		return wasiStatusView(canonical, "error"), nil
	}
	return wasiStatusView(canonical, "ready"), nil
}

func (b *backend) Stop(ctx context.Context, req StopRequest) error {
	_ = ctx
	leaseID := req.ID
	if leaseID == "" {
		return exit(2, "missing id for wasi stop")
	}
	canonical, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil {
		return err
	}
	if !ok {
		return exit(4, "wasi lease %q was not found", req.ID)
	}
	leaseID = canonical.LeaseID
	b.cleanupGuest(leaseID)
	removeLeaseClaim(leaseID)
	fmt.Fprintf(b.rt.Stderr, "stopped wasi lease=%s\n", leaseID)
	return nil
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	leases, err := b.List(ctx, ListRequest{})
	if err != nil {
		return DoctorResult{}, err
	}
	return inventoryDoctorResult(providerName, len(leases)), nil
}

func (b *backend) ensureLease(repo Repo, keep, reclaim bool, requestedSlug string) (string, string, error) {
	leaseID := newLeaseID()
	slug, err := allocateClaimLeaseSlug(leaseID, requestedSlug)
	if err != nil {
		return "", "", err
	}
	if err := claimLeaseForRepoProvider(leaseID, slug, providerName, repo.Root, b.cfg.IdleTimeout, reclaim); err != nil {
		return "", "", err
	}
	return leaseID, slug, nil
}

func (b *backend) ensureLeaseForRun(req RunRequest) (string, string, error) {
	if req.ID == "" {
		return b.ensureLease(req.Repo, req.Keep, req.Reclaim, req.RequestedSlug)
	}
	claim, ok, err := resolveLeaseClaimForProvider(req.ID, providerName)
	if err != nil || !ok {
		return "", "", exit(2, "no wasi lease for %s", req.ID)
	}
	return claim.LeaseID, claim.Slug, nil
}

func (b *backend) prepareGuest(ctx context.Context, repoRoot, leaseID string, noSync, forceSyncLarge bool) (string, time.Duration, error) {
	if repoRoot == "" {
		repoRoot = "."
	}
	guestRoot, err := b.guestRootPath(leaseID)
	if err != nil {
		return "", 0, err
	}
	if err := assertGuestRootOutsideRepo(guestRoot, repoRoot); err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(guestRoot, 0o755); err != nil {
		return "", 0, exit(6, "create wasi guest root: %v", err)
	}
	// The guest root is reused across runs (run-session handles). The
	// per-component no-follow checks below only validate children, so the root
	// itself must be proven to be a real directory here: a root replaced with a
	// symlink (e.g. pointing at a host directory) would otherwise be followed by
	// the workdir mkdir/copy and let sync escape the guest tree.
	if err := assertRealDir(guestRoot); err != nil {
		return guestRoot, 0, exit(2, "wasi guest root: %v", err)
	}

	syncDur := time.Duration(0)
	target, err := b.guestWorkdirPath(guestRoot)
	if err != nil {
		return guestRoot, 0, err
	}
	if !noSync {
		excludes, err := syncExcludes(repoRoot, b.cfg)
		if err != nil {
			return guestRoot, 0, err
		}
		manifest, err := syncManifest(repoRoot, excludes)
		if err != nil {
			return guestRoot, 0, exit(6, "build sync manifest: %v", err)
		}
		if err := checkSyncPreflight(manifest, b.cfg, forceSyncLarge, b.rt.Stderr); err != nil {
			return guestRoot, 0, err
		}
		if b.cfg.Sync.Delete {
			if err := removeAllNoFollow(guestRoot, target); err != nil {
				return guestRoot, 0, exit(6, "wasi delete-sync reset: %v", err)
			}
		}
		start := b.now()
		if err := b.copyManifestToGuest(repoRoot, guestRoot, manifest); err != nil {
			return guestRoot, 0, exit(6, "copy to wasi guest: %v", err)
		}
		syncDur = b.now().Sub(start)
		fmt.Fprintf(b.rt.Stderr, "wasi sync complete in %s (guest=%s)\n", syncDur.Round(time.Millisecond), guestRoot)
	} else {
		if err := mkdirAllNoFollow(guestRoot, target, 0o755); err != nil {
			return guestRoot, 0, exit(6, "create wasi guest workdir: %v", err)
		}
		fmt.Fprintf(b.rt.Stderr, "wasi sync skipped (guest=%s)\n", guestRoot)
	}
	return guestRoot, syncDur, nil
}

func (b *backend) guestRootPath(leaseID string) (string, error) {
	base := os.TempDir()
	if b.cfg.Wasi.GuestBaseDir != "" {
		base = b.cfg.Wasi.GuestBaseDir
		if !filepath.IsAbs(base) {
			return "", exit(2, "wasi guest base dir must be an absolute path: %q", base)
		}
	}
	return filepath.Join(base, "crabbox-wasi-"+leaseID), nil
}

// assertGuestRootOutsideRepo rejects guest roots that overlap the synced
// repository tree. A guest root under repoRoot is created before the sync
// manifest is built and is not covered by the default excludes, so a kept
// session's copied files would feed back into later manifests and recursively
// pollute /work; a repo under the guest root would copy into itself.
func assertGuestRootOutsideRepo(guestRoot, repoRoot string) error {
	repo := resolveExistingPrefix(repoRoot)
	guest := resolveExistingPrefix(guestRoot)
	if pathWithin(repo, guest) || pathWithin(guest, repo) {
		return exit(2, "wasi guest root %s overlaps repo root %s; set wasi.guestBaseDir outside the repository", guestRoot, repo)
	}
	return nil
}

// resolveExistingPrefix resolves symlinks in the longest existing ancestor of
// p (the path itself may not exist yet) so overlap checks compare canonical
// paths, e.g. /var vs /private/var on macOS.
func resolveExistingPrefix(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	rest := ""
	for cur := abs; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func (b *backend) leaseState(leaseID string) string {
	guest, err := b.guestRootPath(leaseID)
	if err != nil {
		return "error"
	}
	if _, err := os.Stat(guest); err != nil {
		if os.IsNotExist(err) {
			return "not-found"
		}
		return "error"
	}
	return "ready"
}

func (b *backend) copyManifestToGuest(repoRoot, guestRoot string, manifest SyncManifest) error {
	if err := assertRealDir(guestRoot); err != nil {
		return fmt.Errorf("guest root: %w", err)
	}
	target, err := b.guestWorkdirPath(guestRoot)
	if err != nil {
		return err
	}
	if err := mkdirAllNoFollow(guestRoot, target, 0o755); err != nil {
		return err
	}
	for _, f := range manifest.Files {
		src := filepath.Join(repoRoot, filepath.FromSlash(f))
		dst, err := joinUnder(target, f)
		if err != nil {
			return fmt.Errorf("copy %s: %w", f, err)
		}
		info, err := os.Lstat(src)
		if err != nil {
			return fmt.Errorf("copy %s: %w", f, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Validate that the symlink's ultimate target resolves inside the repo
			// root (security). Then *rewrite* the stored link target as a clean
			// relative path (computed via Rel from the symlink location to the
			// resolved target). This ensures the symlink entry created in the
			// guest tree is always self-contained/relative (never absolute or
			// host-absolute), matching the /work preopen and preventing any
			// bypass of the guest boundary even for included targets. Targets
			// excluded from the manifest remain dangling (no content leak).
			resolved, _, err := resolveRepoSymlink(repoRoot, src)
			if err != nil {
				return fmt.Errorf("copy %s: %w", f, err)
			}
			linkTarget, err := filepath.Rel(filepath.Dir(src), resolved)
			if err != nil {
				return fmt.Errorf("copy %s: %w", f, err)
			}
			// Use forward slashes for the stored target (WASI/guest expectation;
			// matches how manifest rels are handled and tar-style providers).
			linkTarget = filepath.ToSlash(linkTarget)
			if err := prepareGuestDestination(target, dst); err != nil {
				return fmt.Errorf("copy %s: %w", f, err)
			}
			if err := os.Symlink(linkTarget, dst); err != nil {
				return fmt.Errorf("copy %s: %w", f, err)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("copy %s: unsupported file mode %s", f, info.Mode())
		}
		if err := prepareGuestDestination(target, dst); err != nil {
			return fmt.Errorf("copy %s: %w", f, err)
		}
		if err := copyFileNoFollow(src, dst, info.Mode().Perm()); err != nil {
			return fmt.Errorf("copy %s: %w", f, err)
		}
	}
	return nil
}

func prepareGuestDestination(root, dst string) error {
	parent := filepath.Dir(dst)
	return mkdirAllNoFollow(root, parent, 0o755)
}

// assertRealDir verifies that p exists and is a real directory, not a symlink.
// It is used to validate a (possibly reused) guest root before anything is
// written underneath it, closing the gap left by the per-component no-follow
// checks which trust their starting root.
func assertRealDir(p string) error {
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", p)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", p)
	}
	return nil
}

// removeAllNoFollow removes target only if every path component from root down
// to target (including target) is a real, non-symlink entry. A reused/tampered
// guest can replace an intermediate workdir component with a symlink to a host
// directory; a raw os.RemoveAll would then traverse that symlink and delete
// host-side paths outside the guest root. Components that do not exist are a
// no-op (nothing to remove). target must lie within root.
func removeAllNoFollow(root, target string) error {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	if !pathWithin(root, target) {
		return fmt.Errorf("delete target %s is outside guest root", target)
	}
	if err := assertRealDir(root); err != nil {
		return err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if rel == "." {
		// target == root: never wipe the whole guest root through this path.
		return fmt.Errorf("refusing to delete guest root %s", target)
	}
	cur := root
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil // nothing to remove
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("delete path component %s is a symlink", cur)
		}
	}
	return os.RemoveAll(target)
}

func mkdirAllNoFollow(root, p string, perm os.FileMode) error {
	root = filepath.Clean(root)
	p = filepath.Clean(p)
	if !pathWithin(root, p) {
		return fmt.Errorf("destination %s is outside guest root", p)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	cur := root
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			if err := os.Mkdir(cur, perm); err != nil && !os.IsExist(err) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("destination component %s is a symlink", cur)
		}
		if !info.IsDir() {
			return fmt.Errorf("destination component %s is not a directory", cur)
		}
	}
	return nil
}

func copyFileNoFollow(src, dst string, perm os.FileMode) error {
	if info, err := os.Lstat(dst); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("destination %s is a symlink", dst)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return copyFile(src, dst, perm)
}

func copyFile(src, dst string, perm os.FileMode) error {
	if perm == 0 {
		perm = 0o644
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, perm)
}

func (b *backend) cleanupGuest(leaseID string) {
	guest, err := b.guestRootPath(leaseID)
	if err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: wasi cleanup lease=%s: %v\n", leaseID, err)
		return
	}
	if err := os.RemoveAll(guest); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(b.rt.Stderr, "warning: wasi cleanup guest %s: %v\n", guest, err)
	}
}

func (b *backend) executeCommand(ctx context.Context, guestRoot string, req RunRequest, command []string) (int, time.Duration, error) {
	start := b.now()
	cmd0 := command[0]

	if isWasiModuleCommand(cmd0) {
		runtime, err := normalizeWasiRuntime(b.cfg.Wasi.Runtime)
		if err != nil {
			return 1, b.now().Sub(start), err
		}
		wasmPath, err := b.resolveWasmHostPath(guestRoot, cmd0)
		if err != nil {
			return 1, b.now().Sub(start), err
		}
		if strings.HasSuffix(strings.ToLower(cmd0), ".cwasm") && runtime != "wasmtime" {
			return 1, b.now().Sub(start), exit(2, "wasi: .cwasm modules require --wasi-runtime wasmtime")
		}
		if runtime == "wasmtime" {
			if bin, err := exec.LookPath("wasmtime"); err == nil {
				wasmtimeCommand := append([]string{wasmPath}, command[1:]...)
				return b.runWithWasmtimeCLI(ctx, bin, guestRoot, wasmtimeCommand, req.Env)
			}
			if strings.HasSuffix(strings.ToLower(cmd0), ".cwasm") {
				return 1, b.now().Sub(start), exit(2, "wasi: wasmtime runtime requested but wasmtime was not found in PATH")
			}
			fmt.Fprintln(b.rt.Stderr, "warning: wasmtime not found in PATH; falling back to wazero")
		}
		data, err := os.ReadFile(wasmPath)
		if err != nil {
			return 1, b.now().Sub(start), exit(2, "read wasm %s: %v", wasmPath, err)
		}
		exitCode, err := b.runWithWazero(ctx, data, command[1:], req.Env, guestRoot)
		return exitCode, b.now().Sub(start), err
	}

	// Builtins (host side, operating on guest tree for ls/cat)
	switch cmd0 {
	case "echo":
		fmt.Fprintln(b.rt.Stdout, strings.Join(command[1:], " "))
		return 0, b.now().Sub(start), nil
	case "true":
		return 0, b.now().Sub(start), nil
	case "false":
		return 1, b.now().Sub(start), nil
	case "ls":
		dir, err := b.resolveExistingGuestPath(guestRoot, ".")
		if err != nil {
			return 1, b.now().Sub(start), err
		}
		if len(command) > 1 {
			dir, err = b.resolveExistingGuestPath(guestRoot, command[1])
			if err != nil {
				return 1, b.now().Sub(start), err
			}
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			return 1, b.now().Sub(start), err
		}
		for _, e := range ents {
			fmt.Fprintln(b.rt.Stdout, e.Name())
		}
		return 0, b.now().Sub(start), nil
	case "cat":
		if len(command) < 2 {
			return 1, b.now().Sub(start), exit(2, "cat: missing file")
		}
		for _, arg := range command[1:] {
			p, err := b.resolveExistingGuestPath(guestRoot, arg)
			if err != nil {
				return 1, b.now().Sub(start), err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return 1, b.now().Sub(start), err
			}
			if _, err := b.rt.Stdout.Write(data); err != nil {
				return 1, b.now().Sub(start), err
			}
		}
		return 0, b.now().Sub(start), nil
	default:
		return 1, b.now().Sub(start), exit(2, "wasi: unsupported command %q (expected .wasm module or echo/true/false/ls/cat). Compile your code to wasm32-wasi.", cmd0)
	}
}

func (b *backend) runWithWazero(ctx context.Context, wasmBytes []byte, args []string, env map[string]string, guestRoot string) (int, error) {
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)

	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	guestWork, err := b.guestWorkdirPath(guestRoot)
	if err != nil {
		return 1, err
	}
	fsConfig := wazero.NewFSConfig().WithDirMount(guestWork, "/work")

	mc := wazero.NewModuleConfig().
		WithFSConfig(fsConfig).
		WithArgs(append([]string{"wasi"}, args...)...).
		WithStdout(b.rt.Stdout).
		WithStderr(b.rt.Stderr)

	for k, v := range env {
		mc = mc.WithEnv(k, v)
	}

	mod, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return 1, exit(2, "compile wasm: %v", err)
	}

	_, err = rt.InstantiateModule(ctx, mod, mc)
	if err != nil {
		if exitErr, ok := err.(*sys.ExitError); ok {
			return int(exitErr.ExitCode()), nil
		}
		return 1, exit(1, "wasi run: %v", err)
	}
	return 0, nil
}

func (b *backend) runWithWasmtimeCLI(ctx context.Context, wasmtimeBin, guestRoot string, command []string, env map[string]string) (int, time.Duration, error) {
	start := b.now()
	guestWork, err := b.guestWorkdirPath(guestRoot)
	if err != nil {
		return 1, b.now().Sub(start), err
	}

	args, childEnv := buildWasmtimeRunArgsAndEnv(guestWork, command, env)

	if b.rt.Exec == nil {
		// default should be set by provider backend, but be explicit
		return 1, b.now().Sub(start), exit(2, "wasi: no Exec runner available for wasmtime")
	}

	result, runErr := b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:   wasmtimeBin,
		Args:   args,
		Env:    childEnv,
		Dir:    guestWork,
		Stdout: b.rt.Stdout,
		Stderr: b.rt.Stderr,
	})
	dur := b.now().Sub(start)
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			return exitErr.ExitCode(), dur, nil
		}
		return result.ExitCode, dur, runErr
	}
	return result.ExitCode, dur, nil
}

// buildWasmtimeRunArgsAndEnv constructs the argv and child environment for
// invoking the wasmtime CLI such that:
//   - No secret *values* ever appear in the wasmtime process argv (only bare
//     `--env KEY` names).
//   - The child env for the wasmtime subprocess is restricted to a minimal set
//     of runtime variables (PATH, HOME, etc.) plus exactly the explicitly allowed
//     values from the request. Unrelated ambient host env (tokens, config, etc.)
//     are not present. This keeps the trust boundary narrow for the wasmtime
//     execution path.
//   - wasmtime's bare `--env KEY` (listed only for allowed keys) will inherit
//     the value from its (restricted) environment and expose it to the guest.
//   - Only the keys we list with `--env` reach the guest.
//
// This keeps ambient host state out of the wasmtime subprocess: a full
// os.Environ would otherwise expose unrelated host variables to the guest.
func buildWasmtimeRunArgsAndEnv(guestWork string, command []string, env map[string]string) (args []string, childEnv []string) {
	args = []string{"run", "--dir", "/work::" + guestWork}
	if len(env) > 0 {
		// Bare names only in argv. Values come from childEnv below.
		envKeys := make([]string, 0, len(env))
		for k := range env {
			envKeys = append(envKeys, k)
		}
		sort.Strings(envKeys)
		for _, k := range envKeys {
			args = append(args, "--env", k)
		}
	}
	args = append(args, command...)

	// Build restricted child env: only a small set of runtime essentials that
	// wasmtime (and basic subprocess execution) may need, plus the explicitly
	// allowed values. No full os.Environ() ambient leak.
	// Deterministic order via sorted keys.
	runtimeBase := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true,
		"SHELL": true, "TERM": true, "LANG": true, "LC_ALL": true,
		"TMPDIR": true, "TEMP": true, "TMP": true,
	}
	childEnvMap := map[string]string{}
	for _, e := range os.Environ() {
		if k, v, ok := strings.Cut(e, "="); ok {
			if runtimeBase[k] {
				childEnvMap[k] = v
			}
		}
	}
	for k, v := range env {
		childEnvMap[k] = v
	}
	keys := make([]string, 0, len(childEnvMap))
	for k := range childEnvMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	childEnv = make([]string, 0, len(childEnvMap))
	for _, k := range keys {
		childEnv = append(childEnv, k+"="+childEnvMap[k])
	}
	return args, childEnv
}

func isPureNoFSBuiltin(command []string) bool {
	if len(command) == 0 {
		return false
	}
	switch command[0] {
	case "echo", "true", "false":
		return true
	default:
		return false
	}
}

func isWasiModuleCommand(command string) bool {
	lower := strings.ToLower(command)
	return strings.HasSuffix(lower, ".wasm") || strings.HasSuffix(lower, ".cwasm")
}

func cleanWasiWorkdir(value string) (string, error) {
	workdir := strings.TrimSpace(value)
	if workdir == "" {
		return defaultWasiWorkdir, nil
	}
	clean := filepath.ToSlash(filepath.Clean(workdir))
	if clean == "." || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", exit(2, "wasi workdir %q must be a relative path below the guest root", value)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." {
			return "", exit(2, "wasi workdir %q must be a relative path below the guest root", value)
		}
	}
	return filepath.FromSlash(clean), nil
}

func normalizeWasiRuntime(value string) (string, error) {
	runtime := strings.ToLower(strings.TrimSpace(value))
	if runtime == "" {
		return defaultWasiRuntime, nil
	}
	switch runtime {
	case "wazero", "wasmtime":
		return runtime, nil
	default:
		return "", exit(2, "unsupported wasi runtime %q (expected wazero or wasmtime)", value)
	}
}

func (b *backend) guestWorkdirPath(guestRoot string) (string, error) {
	workdir, err := cleanWasiWorkdir(b.cfg.Wasi.Workdir)
	if err != nil {
		return "", err
	}
	target := filepath.Join(guestRoot, workdir)
	if !pathWithin(guestRoot, target) {
		return "", exit(2, "wasi workdir %q escapes guest root", b.cfg.Wasi.Workdir)
	}
	return target, nil
}

func (b *backend) resolveWasmHostPath(guestRoot, command string) (string, error) {
	slashCommand := filepath.ToSlash(command)
	if slashCommand == "/work" || strings.HasPrefix(slashCommand, "/work/") {
		return b.resolveExistingGuestPath(guestRoot, slashCommand)
	}
	if filepath.IsAbs(command) {
		return command, nil
	}
	return b.resolveExistingGuestPath(guestRoot, command)
}

func (b *backend) resolveExistingGuestPath(guestRoot, guestPath string) (string, error) {
	target, err := b.resolveGuestPath(guestRoot, guestPath)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	workdir, err := b.guestWorkdirPath(guestRoot)
	if err != nil {
		return "", err
	}
	resolvedWorkdir, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		return "", err
	}
	if !pathWithin(resolvedWorkdir, resolved) {
		return "", exit(2, "wasi path %q resolves outside /work", guestPath)
	}
	return resolved, nil
}

func (b *backend) resolveGuestPath(guestRoot, guestPath string) (string, error) {
	workdir, err := b.guestWorkdirPath(guestRoot)
	if err != nil {
		return "", err
	}
	arg := filepath.ToSlash(strings.TrimSpace(guestPath))
	if arg == "" {
		arg = "."
	}
	var rel string
	switch {
	case arg == "/work":
		rel = "."
	case strings.HasPrefix(arg, "/work/"):
		rel = strings.TrimPrefix(arg, "/work/")
	case strings.HasPrefix(arg, "/"):
		return "", exit(2, "wasi path %q is outside /work", guestPath)
	default:
		rel = arg
	}
	clean := path.Clean(rel)
	if clean == "." {
		return workdir, nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", exit(2, "wasi path %q is outside /work", guestPath)
	}
	target := filepath.Join(workdir, filepath.FromSlash(clean))
	if !pathWithin(workdir, target) {
		return "", exit(2, "wasi path %q is outside /work", guestPath)
	}
	return target, nil
}

func joinUnder(root, rel string) (string, error) {
	clean := path.Clean(filepath.ToSlash(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("unsafe relative path %q", rel)
	}
	target := filepath.Join(root, filepath.FromSlash(clean))
	if !pathWithin(root, target) {
		return "", fmt.Errorf("path %q escapes %s", rel, root)
	}
	return target, nil
}

func resolveRepoSymlink(repoRoot, src string) (string, os.FileInfo, error) {
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		return "", nil, err
	}
	resolvedRepoRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", nil, err
	}
	if !pathWithin(resolvedRepoRoot, resolved) {
		return "", nil, fmt.Errorf("symlink target %s is outside repo root", resolved)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", nil, err
	}
	return resolved, info, nil
}

func pathWithin(root, candidate string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, candidateAbs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

func wasiCleanupCommand(leaseID string) string {
	return fmt.Sprintf("crabbox stop --provider %s %s", providerName, shellQuote(leaseID))
}

func wasiClaimToServer(claim LeaseClaim, state string) Server {
	labels := map[string]string{
		"provider": providerName,
		"lease":    claim.LeaseID,
		"slug":     blank(claim.Slug, claim.LeaseID),
		"target":   targetLinux,
		"state":    state,
	}
	server := Server{
		Provider: providerName,
		CloudID:  claim.LeaseID,
		Name:     blank(claim.Slug, claim.LeaseID),
		Status:   state,
		Labels:   labels,
	}
	server.ServerType.Name = providerName
	return server
}

func wasiStatusView(claim LeaseClaim, state string) StatusView {
	server := wasiClaimToServer(claim, state)
	return StatusView{
		ID:         claim.LeaseID,
		Slug:       claim.Slug,
		Provider:   providerName,
		TargetOS:   targetLinux,
		State:      state,
		ServerID:   server.CloudID,
		ServerType: server.ServerType.Name,
		Labels:     server.Labels,
		Ready:      state == "ready",
	}
}
