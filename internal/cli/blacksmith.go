package cli

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const blacksmithTestboxProvider = "blacksmith-testbox"

var (
	blacksmithCommandContext = exec.CommandContext
	blacksmithIDPattern      = regexp.MustCompile(`\btbx_[A-Za-z0-9_-]+\b`)
)

type blacksmithRunOptions struct {
	ID          string
	Keep        bool
	Reclaim     bool
	SyncOnly    bool
	Debug       bool
	ShellMode   bool
	Command     []string
	IdleTimeout time.Duration
}

type blacksmithFlagValues struct {
	Org      *string
	Workflow *string
	Job      *string
	Ref      *string
}

func isBlacksmithProvider(provider string) bool {
	return provider == blacksmithTestboxProvider || provider == "blacksmith"
}

func registerBlacksmithFlags(fs *flag.FlagSet, defaults Config) blacksmithFlagValues {
	return blacksmithFlagValues{
		Org:      fs.String("blacksmith-org", defaults.Blacksmith.Org, "Blacksmith organization"),
		Workflow: fs.String("blacksmith-workflow", defaults.Blacksmith.Workflow, "Blacksmith Testbox workflow file, name, or id"),
		Job:      fs.String("blacksmith-job", defaults.Blacksmith.Job, "Blacksmith Testbox workflow job"),
		Ref:      fs.String("blacksmith-ref", defaults.Blacksmith.Ref, "Blacksmith Testbox git ref"),
	}
}

func applyBlacksmithFlagOverrides(cfg *Config, fs *flag.FlagSet, values blacksmithFlagValues) {
	if flagWasSet(fs, "blacksmith-org") {
		cfg.Blacksmith.Org = *values.Org
	}
	if flagWasSet(fs, "blacksmith-workflow") {
		cfg.Blacksmith.Workflow = *values.Workflow
	}
	if flagWasSet(fs, "blacksmith-job") {
		cfg.Blacksmith.Job = *values.Job
	}
	if flagWasSet(fs, "blacksmith-ref") {
		cfg.Blacksmith.Ref = *values.Ref
	}
}

func (a App) blacksmithWarmup(ctx context.Context, cfg Config, repo Repo, keep, reclaim bool) error {
	started := time.Now()
	leaseID, slug, err := a.blacksmithWarmupLease(ctx, cfg, repo, reclaim)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "leased %s slug=%s provider=%s idle_timeout=%s\n", leaseID, slug, blacksmithTestboxProvider, blacksmithIdleTimeout(cfg))
	if !keep {
		fmt.Fprintf(a.Stderr, "warning: blacksmith warmup keeps the testbox until idle timeout or explicit stop\n")
	}
	fmt.Fprintf(a.Stdout, "warmup complete total=%s\n", time.Since(started).Round(time.Millisecond))
	return nil
}

func (a App) blacksmithRun(ctx context.Context, cfg Config, repo Repo, opts blacksmithRunOptions) error {
	if opts.SyncOnly {
		return exit(2, "blacksmith-testbox delegates sync to Blacksmith; --sync-only is not supported")
	}
	started := time.Now()
	leaseID := opts.ID
	acquired := false
	var err error
	if leaseID == "" {
		leaseID, _, err = a.blacksmithWarmupLease(ctx, cfg, repo, opts.Reclaim)
		if err != nil {
			return err
		}
		acquired = true
	} else {
		leaseID, err = resolveBlacksmithLeaseID(leaseID, repo.Root, opts.Reclaim)
		if err != nil {
			return err
		}
		slug, err := blacksmithClaimSlug(opts.ID, leaseID)
		if err != nil {
			return err
		}
		if err := claimLeaseForRepoProvider(leaseID, slug, blacksmithTestboxProvider, repo.Root, opts.IdleTimeout, opts.Reclaim); err != nil {
			return err
		}
	}
	if acquired && !opts.Keep {
		defer func() {
			if err := a.blacksmithStopLease(context.Background(), cfg, leaseID); err != nil {
				fmt.Fprintf(a.Stderr, "warning: blacksmith stop failed for %s: %v\n", leaseID, err)
				return
			}
			removeLeaseClaim(leaseID)
			removeStoredTestboxKey(leaseID)
		}()
	}
	fmt.Fprintf(a.Stderr, "provider=blacksmith-testbox id=%s sync=delegated auth=blacksmith\n", leaseID)
	commandStart := time.Now()
	code := a.runBlacksmithTestbox(ctx, cfg, leaseID, opts.Command, opts.Debug, opts.ShellMode)
	commandDuration := time.Since(commandStart)
	total := time.Since(started)
	fmt.Fprintf(a.Stderr, "blacksmith run summary sync=delegated command=%s total=%s exit=%d\n", commandDuration.Round(time.Millisecond), total.Round(time.Millisecond), code)
	if code != 0 {
		return ExitError{Code: code, Message: fmt.Sprintf("blacksmith testbox run exited %d", code)}
	}
	return nil
}

func (a App) blacksmithList(ctx context.Context, cfg Config, jsonOut bool) error {
	if jsonOut {
		return exit(2, "blacksmith-testbox list does not support --json")
	}
	return a.streamBlacksmith(ctx, blacksmithListArgs(cfg))
}

func (a App) blacksmithStatus(ctx context.Context, cfg Config, id string, wait bool, waitTimeout time.Duration, jsonOut bool) error {
	if jsonOut {
		return exit(2, "blacksmith-testbox status does not support --json")
	}
	leaseID, err := resolveBlacksmithLeaseID(id, "", false)
	if err != nil {
		return err
	}
	return a.streamBlacksmith(ctx, blacksmithStatusArgs(cfg, leaseID, wait, waitTimeout))
}

func (a App) blacksmithStop(ctx context.Context, cfg Config, id string) error {
	leaseID, err := resolveBlacksmithLeaseID(id, "", false)
	if err != nil {
		return err
	}
	if err := a.blacksmithStopLease(ctx, cfg, leaseID); err != nil {
		return err
	}
	removeLeaseClaim(leaseID)
	removeStoredTestboxKey(leaseID)
	return nil
}

func (a App) blacksmithWarmupLease(ctx context.Context, cfg Config, repo Repo, reclaim bool) (string, string, error) {
	pendingID := "tbx_pending_" + strings.TrimPrefix(newLeaseID(), "cbx_")
	cleanupKeyID := pendingID
	defer func() {
		if cleanupKeyID != "" {
			removeStoredTestboxKey(cleanupKeyID)
		}
	}()
	_, publicKey, err := ensureTestboxKey(pendingID)
	if err != nil {
		return "", "", err
	}
	args, err := blacksmithWarmupArgs(cfg, publicKey)
	if err != nil {
		return "", "", err
	}
	var output bytes.Buffer
	cmd := blacksmithCommandContext(ctx, "blacksmith", args...)
	cmd.Stdout = io.MultiWriter(a.Stdout, &output)
	cmd.Stderr = io.MultiWriter(a.Stderr, &output)
	if err := cmd.Run(); err != nil {
		return "", "", exit(exitCode(err), "blacksmith testbox warmup failed: %v", err)
	}
	leaseID := parseBlacksmithID(output.String())
	if leaseID == "" {
		return "", "", exit(5, "blacksmith testbox warmup did not print a tbx_ id")
	}
	if err := moveStoredTestboxKey(pendingID, leaseID); err != nil {
		_ = a.blacksmithStopLease(ctx, cfg, leaseID)
		return "", "", exit(2, "store blacksmith key for %s: %v", leaseID, err)
	}
	cleanupKeyID = leaseID
	slug := newLeaseSlug(leaseID)
	if err := claimLeaseForRepoProvider(leaseID, slug, blacksmithTestboxProvider, repo.Root, blacksmithIdleTimeout(cfg), reclaim); err != nil {
		_ = a.blacksmithStopLease(ctx, cfg, leaseID)
		return "", "", err
	}
	cleanupKeyID = ""
	return leaseID, slug, nil
}

func (a App) runBlacksmithTestbox(ctx context.Context, cfg Config, leaseID string, command []string, debug, shellMode bool) int {
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		fmt.Fprintf(a.Stderr, "blacksmith key path failed: %v\n", err)
		return 2
	}
	args := blacksmithRunArgs(cfg, leaseID, keyPath, command, debug || cfg.Blacksmith.Debug, shellMode)
	cmd := blacksmithCommandContext(ctx, "blacksmith", args...)
	cmd.Stdout = a.Stdout
	cmd.Stderr = a.Stderr
	if err := cmd.Run(); err != nil {
		return exitCode(err)
	}
	return 0
}

func (a App) blacksmithStopLease(ctx context.Context, cfg Config, leaseID string) error {
	return a.streamBlacksmith(ctx, blacksmithStopArgs(cfg, leaseID))
}

func (a App) streamBlacksmith(ctx context.Context, args []string) error {
	cmd := blacksmithCommandContext(ctx, "blacksmith", args...)
	cmd.Stdout = a.Stdout
	cmd.Stderr = a.Stderr
	if err := cmd.Run(); err != nil {
		return ExitError{Code: exitCode(err), Message: fmt.Sprintf("blacksmith failed: %v", err)}
	}
	return nil
}

func blacksmithWarmupArgs(cfg Config, publicKey string) ([]string, error) {
	workflow := blacksmithWorkflow(cfg)
	if workflow == "" {
		return nil, exit(2, "blacksmith-testbox requires blacksmith.workflow or actions.workflow")
	}
	args := blacksmithBaseArgs(cfg)
	args = append(args, "testbox", "warmup", workflow)
	if job := blacksmithJob(cfg); job != "" {
		args = append(args, "--job", job)
	}
	if ref := blacksmithRef(cfg); ref != "" {
		args = append(args, "--ref", ref)
	}
	if publicKey != "" {
		args = append(args, "--ssh-public-key", publicKey)
	}
	args = append(args, "--idle-timeout", fmt.Sprint(durationMinutesCeil(blacksmithIdleTimeout(cfg))))
	return args, nil
}

func blacksmithRunArgs(cfg Config, leaseID, keyPath string, command []string, debug, shellMode bool) []string {
	args := blacksmithBaseArgs(cfg)
	args = append(args, "testbox", "run", "--id", leaseID)
	if keyPath != "" {
		args = append(args, "--ssh-private-key", keyPath)
	}
	if debug {
		args = append(args, "--debug")
	}
	args = append(args, blacksmithCommandString(command, shellMode))
	return args
}

func blacksmithStatusArgs(cfg Config, leaseID string, wait bool, waitTimeout time.Duration) []string {
	args := blacksmithBaseArgs(cfg)
	args = append(args, "testbox", "status", "--id", leaseID)
	if wait {
		args = append(args, "--wait", "--wait-timeout", waitTimeout.String())
	}
	return args
}

func blacksmithStopArgs(cfg Config, leaseID string) []string {
	args := blacksmithBaseArgs(cfg)
	return append(args, "testbox", "stop", "--id", leaseID)
}

func blacksmithListArgs(cfg Config) []string {
	args := blacksmithBaseArgs(cfg)
	return append(args, "testbox", "list")
}

func blacksmithBaseArgs(cfg Config) []string {
	args := []string{}
	if cfg.Blacksmith.Org != "" {
		args = append(args, "--org", cfg.Blacksmith.Org)
	}
	return args
}

func blacksmithWorkflow(cfg Config) string {
	return blank(cfg.Blacksmith.Workflow, cfg.Actions.Workflow)
}

func blacksmithJob(cfg Config) string {
	return blank(cfg.Blacksmith.Job, cfg.Actions.Job)
}

func blacksmithRef(cfg Config) string {
	return blank(cfg.Blacksmith.Ref, cfg.Actions.Ref)
}

func blacksmithIdleTimeout(cfg Config) time.Duration {
	if cfg.Blacksmith.IdleTimeout > 0 {
		return cfg.Blacksmith.IdleTimeout
	}
	return cfg.IdleTimeout
}

func durationMinutesCeil(duration time.Duration) int {
	if duration <= 0 {
		return 1
	}
	minutes := int(duration / time.Minute)
	if duration%time.Minute != 0 {
		minutes++
	}
	if minutes < 1 {
		return 1
	}
	return minutes
}

func parseBlacksmithID(output string) string {
	return blacksmithIDPattern.FindString(output)
}

func resolveBlacksmithLeaseID(identifier, repoRoot string, reclaim bool) (string, error) {
	if identifier == "" {
		return "", exit(2, "blacksmith-testbox requires --id <tbx-id-or-slug>")
	}
	if parseBlacksmithID(identifier) == identifier {
		return identifier, nil
	}
	claim, ok, err := resolveLeaseClaim(identifier)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", exit(4, "unknown blacksmith testbox %q", identifier)
	}
	if claim.Provider != "" && claim.Provider != blacksmithTestboxProvider {
		return "", exit(4, "%q is claimed by provider %s", identifier, claim.Provider)
	}
	if repoRoot != "" && claim.RepoRoot != "" && claim.RepoRoot != repoRoot && !reclaim {
		return "", exit(2, "lease %s is claimed by repo %s; use --reclaim to claim it for %s", claim.LeaseID, claim.RepoRoot, repoRoot)
	}
	return claim.LeaseID, nil
}

func blacksmithClaimSlug(identifier, leaseID string) (string, error) {
	for _, candidate := range []string{identifier, leaseID} {
		claim, ok, err := resolveLeaseClaim(candidate)
		if err != nil {
			return "", err
		}
		if ok && claim.LeaseID == leaseID {
			return claim.Slug, nil
		}
	}
	return "", nil
}

func blacksmithCommandString(command []string, shellMode bool) string {
	if len(command) == 0 {
		return ""
	}
	if shellMode || shouldUseShell(command) || len(command) == 1 {
		return strings.Join(command, " ")
	}
	parts := make([]string, 0, len(command))
	seenCommand := false
	for _, word := range command {
		if !seenCommand && isShellEnvAssignment(word) {
			key, value, _ := strings.Cut(word, "=")
			parts = append(parts, key+"="+shellQuote(value))
			continue
		}
		seenCommand = true
		parts = append(parts, shellQuote(word))
	}
	return strings.Join(parts, " ")
}

func isShellEnvAssignment(word string) bool {
	if word == "" {
		return false
	}
	idx := strings.IndexByte(word, '=')
	if idx <= 0 {
		return false
	}
	for i, r := range word[:idx] {
		if i == 0 {
			if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_') {
				return false
			}
			continue
		}
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_') {
			return false
		}
	}
	return true
}
