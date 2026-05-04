package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	isloProvider     = "islo"
	isloSandboxAlias = "islo-sandbox"
	isloIDPrefix     = "isb_"
	isloNamePrefix   = "crabbox-"
)

var (
	isloCommandContext = exec.CommandContext
	isloIDPattern      = regexp.MustCompile(`\bisb_[A-Za-z0-9_-]+\b`)
)

type isloRunOptions struct {
	ID          string
	Keep        bool
	Reclaim     bool
	SyncOnly    bool
	Debug       bool
	ShellMode   bool
	Command     []string
	IdleTimeout time.Duration
	TimingJSON  bool
}

type isloFlagValues struct {
	Org            *string
	Image          *string
	Source         *string
	Workdir        *string
	GatewayProfile *string
	Session        *string
}

type isloListItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Image     string `json:"image,omitempty"`
	CreatedBy string `json:"createdBy,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
	DeletedAt string `json:"deletedAt,omitempty"`
}

func isIsloProvider(provider string) bool {
	return provider == isloProvider || provider == isloSandboxAlias
}

func registerIsloFlags(fs *flag.FlagSet, defaults Config) isloFlagValues {
	return isloFlagValues{
		Org:            fs.String("islo-org", defaults.Islo.Org, "islo organization"),
		Image:          fs.String("islo-image", defaults.Islo.Image, "islo container image"),
		Source:         fs.String("islo-source", defaults.Islo.Source, "islo repo source (github://owner/repo[:branch] or https://...)"),
		Workdir:        fs.String("islo-workdir", defaults.Islo.Workdir, "islo working directory"),
		GatewayProfile: fs.String("islo-gateway-profile", defaults.Islo.GatewayProfile, "islo gateway profile"),
		Session:        fs.String("islo-session", defaults.Islo.Session, "islo session name (default: main)"),
	}
}

func applyIsloFlagOverrides(cfg *Config, fs *flag.FlagSet, values isloFlagValues) {
	if flagWasSet(fs, "islo-org") {
		cfg.Islo.Org = *values.Org
	}
	if flagWasSet(fs, "islo-image") {
		cfg.Islo.Image = *values.Image
	}
	if flagWasSet(fs, "islo-source") {
		cfg.Islo.Source = *values.Source
	}
	if flagWasSet(fs, "islo-workdir") {
		cfg.Islo.Workdir = *values.Workdir
	}
	if flagWasSet(fs, "islo-gateway-profile") {
		cfg.Islo.GatewayProfile = *values.GatewayProfile
	}
	if flagWasSet(fs, "islo-session") {
		cfg.Islo.Session = *values.Session
	}
}

func (a App) isloWarmup(ctx context.Context, cfg Config, repo Repo, keep, reclaim, timingJSON bool) error {
	started := time.Now()
	sandboxName, leaseID, slug, err := a.isloWarmupLease(ctx, cfg, repo, "", reclaim)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "leased %s sandbox=%s slug=%s provider=%s idle_timeout=%s\n", leaseID, sandboxName, slug, isloProvider, isloIdleTimeout(cfg))
	if !keep {
		fmt.Fprintf(a.Stderr, "warning: islo warmup keeps the sandbox until idle timeout or explicit stop\n")
	}
	fmt.Fprintf(a.Stdout, "warmup complete total=%s\n", time.Since(started).Round(time.Millisecond))
	if timingJSON {
		total := time.Since(started)
		if err := writeTimingJSON(a.Stderr, timingReport{
			Provider: isloProvider,
			LeaseID:  leaseID,
			Slug:     slug,
			TotalMs:  total.Milliseconds(),
			ExitCode: 0,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (a App) isloRun(ctx context.Context, cfg Config, repo Repo, opts isloRunOptions) error {
	if opts.SyncOnly {
		return exit(2, "islo delegates sync to the sandbox; --sync-only is not supported")
	}
	started := time.Now()
	identifier := opts.ID
	var sandboxName, leaseID, slug string
	var err error
	acquired := false
	if identifier == "" {
		sandboxName, leaseID, slug, err = a.isloWarmupLease(ctx, cfg, repo, "", opts.Reclaim)
		if err != nil {
			return err
		}
		acquired = true
	} else {
		sandboxName, leaseID, err = resolveIsloLeaseID(identifier, repo.Root, opts.Reclaim)
		if err != nil {
			return err
		}
		slug, err = isloClaimSlug(identifier, leaseID)
		if err != nil {
			return err
		}
		if err := claimLeaseForRepoProvider(leaseID, slug, isloProvider, repo.Root, opts.IdleTimeout, opts.Reclaim); err != nil {
			return err
		}
	}
	if acquired && !opts.Keep {
		defer func() {
			if err := a.isloRemoveSandbox(context.Background(), cfg, sandboxName); err != nil {
				fmt.Fprintf(a.Stderr, "warning: islo rm failed for %s: %v\n", sandboxName, err)
				return
			}
			removeLeaseClaim(leaseID)
		}()
	}
	fmt.Fprintf(a.Stderr, "provider=islo id=%s sandbox=%s sync=delegated auth=islo\n", leaseID, sandboxName)
	commandStart := time.Now()
	code := a.runIsloUse(ctx, cfg, sandboxName, opts.Command, opts.Debug || cfg.Islo.Debug, opts.ShellMode)
	commandDuration := time.Since(commandStart)
	total := time.Since(started)
	fmt.Fprintf(a.Stderr, "islo run summary sync=delegated command=%s total=%s exit=%d\n", commandDuration.Round(time.Millisecond), total.Round(time.Millisecond), code)
	if opts.TimingJSON {
		if err := writeTimingJSON(a.Stderr, timingReport{
			Provider:      isloProvider,
			LeaseID:       leaseID,
			Slug:          slug,
			SyncPhases:    []timingPhase{{Name: "delegated", Skipped: true, Reason: "islo owns sync"}},
			SyncDelegated: true,
			CommandMs:     commandDuration.Milliseconds(),
			TotalMs:       total.Milliseconds(),
			ExitCode:      code,
		}); err != nil {
			return err
		}
	}
	if code != 0 {
		return ExitError{Code: code, Message: fmt.Sprintf("islo run exited %d", code)}
	}
	return nil
}

func (a App) isloList(ctx context.Context, cfg Config, jsonOut bool) error {
	args := isloListArgs(cfg, jsonOut)
	if !jsonOut {
		return a.streamIslo(ctx, args)
	}
	cmd := isloCommandContext(ctx, "islo", args...)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(exitErr.Stderr))
		}
		return ExitError{Code: exitCode(err), Message: fmt.Sprintf("islo failed: %v: %s", err, stderr)}
	}
	items := parseIsloListJSON(out)
	return json.NewEncoder(a.Stdout).Encode(items)
}

func (a App) isloStatus(ctx context.Context, cfg Config, id string, wait bool, waitTimeout time.Duration, jsonOut bool) error {
	sandboxName, _, err := resolveIsloLeaseID(id, "", false)
	if err != nil {
		return err
	}
	if !wait {
		return a.streamIslo(ctx, isloStatusArgs(cfg, sandboxName, jsonOut))
	}
	deadline := time.Now().Add(waitTimeout)
	for {
		cmd := isloCommandContext(ctx, "islo", isloStatusArgs(cfg, sandboxName, true)...)
		out, runErr := cmd.Output()
		if runErr == nil {
			var view struct {
				Status string `json:"status"`
			}
			if jsonErr := json.Unmarshal(out, &view); jsonErr == nil && view.Status == "running" {
				if jsonOut {
					_, _ = a.Stdout.Write(out)
					return nil
				}
				return a.streamIslo(ctx, isloStatusArgs(cfg, sandboxName, false))
			}
		}
		if time.Now().After(deadline) {
			return exit(5, "timed out waiting for %s to become ready", sandboxName)
		}
		time.Sleep(5 * time.Second)
	}
}

func (a App) isloStop(ctx context.Context, cfg Config, id string) error {
	sandboxName, leaseID, err := resolveIsloLeaseID(id, "", false)
	if err != nil {
		return err
	}
	if err := a.isloRemoveSandbox(ctx, cfg, sandboxName); err != nil {
		return err
	}
	removeLeaseClaim(leaseID)
	return nil
}

func (a App) isloWarmupLease(ctx context.Context, cfg Config, repo Repo, requestedName string, reclaim bool) (string, string, string, error) {
	sandboxName := requestedName
	if sandboxName == "" {
		sandboxName = newIsloSandboxName()
	}
	args := isloWarmupArgs(cfg, sandboxName)
	var stderr bytes.Buffer
	cmd := isloCommandContext(ctx, "islo", args...)
	cmd.Stdout = a.Stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", "", "", exit(exitCode(err), "islo use failed: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	leaseID := isloIDPrefix + sandboxName
	slug := newLeaseSlug(leaseID)
	if err := claimLeaseForRepoProvider(leaseID, slug, isloProvider, repo.Root, isloIdleTimeout(cfg), reclaim); err != nil {
		_ = a.isloRemoveSandbox(ctx, cfg, sandboxName)
		return "", "", "", err
	}
	return sandboxName, leaseID, slug, nil
}

func (a App) runIsloUse(ctx context.Context, cfg Config, sandboxName string, command []string, debug, shellMode bool) int {
	args := isloRunArgs(cfg, sandboxName, command, shellMode)
	if debug {
		fmt.Fprintf(a.Stderr, "islo %s\n", strings.Join(args, " "))
	}
	cmd := isloCommandContext(ctx, "islo", args...)
	cmd.Stdout = a.Stdout
	cmd.Stderr = a.Stderr
	if err := cmd.Run(); err != nil {
		return exitCode(err)
	}
	return 0
}

func (a App) isloRemoveSandbox(ctx context.Context, cfg Config, sandboxName string) error {
	return a.streamIslo(ctx, isloStopArgs(cfg, sandboxName))
}

func (a App) streamIslo(ctx context.Context, args []string) error {
	cmd := isloCommandContext(ctx, "islo", args...)
	cmd.Stdout = a.Stdout
	cmd.Stderr = a.Stderr
	if err := cmd.Run(); err != nil {
		return ExitError{Code: exitCode(err), Message: fmt.Sprintf("islo failed: %v", err)}
	}
	return nil
}

func isloWarmupArgs(cfg Config, sandboxName string) []string {
	args := isloBaseArgs(cfg)
	args = append(args, "use", sandboxName)
	args = append(args, isloUseFlags(cfg)...)
	args = append(args, "--", "true")
	return args
}

func isloRunArgs(cfg Config, sandboxName string, command []string, shellMode bool) []string {
	args := isloBaseArgs(cfg)
	args = append(args, "use", sandboxName)
	args = append(args, isloUseFlags(cfg)...)
	if len(command) == 0 {
		return args
	}
	if shellMode || shouldUseShell(command) {
		return append(args, "--", "bash", "-lc", strings.Join(command, " "))
	}
	args = append(args, "--")
	return append(args, command...)
}

func isloStatusArgs(cfg Config, sandboxName string, jsonOut bool) []string {
	args := isloBaseArgs(cfg)
	if jsonOut {
		args = append(args, "-o", "json")
	}
	return append(args, "status", sandboxName)
}

func isloStopArgs(cfg Config, sandboxName string) []string {
	args := isloBaseArgs(cfg)
	return append(args, "rm", sandboxName, "--force")
}

func isloListArgs(cfg Config, jsonOut bool) []string {
	args := isloBaseArgs(cfg)
	if jsonOut {
		args = append(args, "-o", "json")
	}
	return append(args, "ls")
}

func isloBaseArgs(cfg Config) []string {
	return []string{}
}

func isloUseFlags(cfg Config) []string {
	args := []string{}
	if cfg.Islo.Image != "" {
		args = append(args, "--image", cfg.Islo.Image)
	}
	if cfg.Islo.Source != "" {
		args = append(args, "--source", cfg.Islo.Source)
	}
	if cfg.Islo.Workdir != "" {
		args = append(args, "--workdir", cfg.Islo.Workdir)
	}
	if cfg.Islo.GatewayProfile != "" {
		args = append(args, "--gateway-profile", cfg.Islo.GatewayProfile)
	}
	if cfg.Islo.Session != "" {
		args = append(args, "--session", cfg.Islo.Session)
	}
	return args
}

func parseIsloListJSON(data []byte) []isloListItem {
	items := []isloListItem{}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return items
	}
	if err := json.Unmarshal(trimmed, &items); err == nil {
		return items
	}
	var wrapped struct {
		Sandboxes []isloListItem `json:"sandboxes"`
	}
	if err := json.Unmarshal(trimmed, &wrapped); err == nil && wrapped.Sandboxes != nil {
		return wrapped.Sandboxes
	}
	return items
}

func isloIdleTimeout(cfg Config) time.Duration {
	if cfg.Islo.IdleTimeout > 0 {
		return cfg.Islo.IdleTimeout
	}
	return cfg.IdleTimeout
}

func newIsloSandboxName() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return isloNamePrefix + strings.ReplaceAll(time.Now().UTC().Format("150405.000"), ".", "")
	}
	return isloNamePrefix + hex.EncodeToString(b[:])
}

func resolveIsloLeaseID(identifier, repoRoot string, reclaim bool) (string, string, error) {
	if identifier == "" {
		return "", "", exit(2, "islo requires --id <sandbox-name-or-slug>")
	}
	if isloIDPattern.MatchString(identifier) {
		match := isloIDPattern.FindString(identifier)
		if match == identifier {
			return strings.TrimPrefix(match, isloIDPrefix), match, nil
		}
	}
	claim, ok, err := resolveLeaseClaim(identifier)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return identifier, isloIDPrefix + identifier, nil
	}
	if claim.Provider != "" && claim.Provider != isloProvider {
		return "", "", exit(4, "%q is claimed by provider %s", identifier, claim.Provider)
	}
	if repoRoot != "" && claim.RepoRoot != "" && claim.RepoRoot != repoRoot && !reclaim {
		return "", "", exit(2, "lease %s is claimed by repo %s; use --reclaim to claim it for %s", claim.LeaseID, claim.RepoRoot, repoRoot)
	}
	return strings.TrimPrefix(claim.LeaseID, isloIDPrefix), claim.LeaseID, nil
}

func isloClaimSlug(identifier, leaseID string) (string, error) {
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
