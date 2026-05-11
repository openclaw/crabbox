package blacksmith

import (
	"flag"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const blacksmithTestboxProvider = "blacksmith-testbox"

var (
	blacksmithIDPattern        = regexp.MustCompile(`\btbx_[A-Za-z0-9_-]+\b`)
	blacksmithSyncStartPattern = regexp.MustCompile(`(?i)^\s*Syncing(?:\.\.\.| from repo root:)`)
	blacksmithSyncDonePattern  = regexp.MustCompile(`(?i)^\s*(Changes synced in|No changes to sync|Sync complete)\b`)
	blacksmithCleanupAttempts  = 36
	blacksmithCleanupDelay     = 5 * time.Second
	blacksmithCleanupQuiet     = 12
)

type blacksmithFlagValues struct {
	Org      *string
	Workflow *string
	Job      *string
	Ref      *string
}

type blacksmithListItem struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Repo     string `json:"repo"`
	Workflow string `json:"workflow"`
	Job      string `json:"job"`
	Ref      string `json:"ref"`
	Created  string `json:"created"`
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

func blacksmithListAllArgs(cfg Config) []string {
	return append(blacksmithListArgs(cfg), "--all")
}

func blacksmithMatchesConfig(item blacksmithListItem, cfg Config) bool {
	if workflow := blacksmithWorkflow(cfg); workflow != "" && item.Workflow != workflow {
		return false
	}
	if job := blacksmithJob(cfg); job != "" && item.Job != job {
		return false
	}
	if ref := blacksmithRef(cfg); ref != "" && item.Ref != ref {
		return false
	}
	return true
}

func parseBlacksmithList(output string) []blacksmithListItem {
	items := []blacksmithListItem{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 || fields[0] == "ID" {
			continue
		}
		if !blacksmithIDPattern.MatchString(fields[0]) {
			continue
		}
		items = append(items, blacksmithListItem{
			ID:       fields[0],
			Status:   fields[1],
			Repo:     fields[2],
			Workflow: fields[3],
			Job:      fields[4],
			Ref:      fields[5],
			Created:  fields[6],
		})
	}
	return items
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
	return core.DurationMinutesCeil(duration)
}

func parseBlacksmithID(output string) string {
	return blacksmithIDPattern.FindString(output)
}

func blacksmithSyncTimeout(env func(string) string) time.Duration {
	for _, key := range []string{"CRABBOX_BLACKSMITH_SYNC_TIMEOUT_MS", "OPENCLAW_TESTBOX_SYNC_TIMEOUT_MS"} {
		raw := strings.TrimSpace(env(key))
		if raw == "" {
			continue
		}
		parsed, err := strconv.Atoi(raw)
		if err == nil && parsed >= 0 {
			return time.Duration(parsed) * time.Millisecond
		}
	}
	return 5 * time.Minute
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
	if shellMode || len(command) == 1 {
		return trimBlacksmithShellCommand(strings.Join(command, " "))
	}
	if shouldUseShell(command) {
		return shellScriptFromArgv(command)
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

func trimBlacksmithShellCommand(command string) string {
	return strings.TrimRight(command, " \t\r\n")
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

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func blank(value, fallback string) string {
	return core.Blank(value, fallback)
}

func resolveLeaseClaim(identifier string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaim(identifier)
}

func shouldUseShell(command []string) bool {
	return core.ShouldUseShell(command)
}

func shellScriptFromArgv(command []string) string {
	return core.ShellScriptFromArgv(command)
}

func shellQuote(s string) string {
	return core.ShellQuote(s)
}
