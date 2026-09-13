package blacksmith

import (
	"fmt"
	"path/filepath"
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
	blacksmithStatusPollDelay  = 5 * time.Second
)

type blacksmithListItem struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Repo     string `json:"repo"`
	Workflow string `json:"workflow"`
	Job      string `json:"job"`
	Ref      string `json:"ref"`
	Created  string `json:"created"`
}

func blacksmithWarmupArgs(cfg core.Config, publicKey string) ([]string, error) {
	workflow := blacksmithWorkflow(cfg)
	if workflow == "" {
		return nil, core.Exit(2, "blacksmith-testbox requires blacksmith.workflow or actions.workflow")
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
	for _, spec := range core.CacheVolumeStickyDiskSpecs(cfg.Cache.Volumes) {
		args = append(args, "--sticky-disk", spec)
	}
	args = append(args, "--idle-timeout", fmt.Sprint(core.DurationMinutesCeil(blacksmithIdleTimeout(cfg))))
	return args, nil
}

func blacksmithRunArgs(cfg core.Config, leaseID, keyPath string, command []string, debug, shellMode bool) []string {
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

func blacksmithStopArgs(cfg core.Config, leaseID string) []string {
	args := blacksmithBaseArgs(cfg)
	return append(args, "testbox", "stop", "--id", leaseID)
}

func blacksmithListArgs(cfg core.Config) []string {
	args := blacksmithBaseArgs(cfg)
	return append(args, "testbox", "list")
}

func blacksmithListAllArgs(cfg core.Config) []string {
	return append(blacksmithListArgs(cfg), "--all")
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

func blacksmithBaseArgs(cfg core.Config) []string {
	args := []string{}
	if cfg.Blacksmith.Org != "" {
		args = append(args, "--org", cfg.Blacksmith.Org)
	}
	return args
}

func blacksmithWorkflow(cfg core.Config) string {
	if cfg.Blacksmith.Workflow != "" {
		return cfg.Blacksmith.Workflow
	}
	if blacksmithCanFallbackToActionsWorkflow(cfg) {
		return cfg.Actions.Workflow
	}
	return ""
}

func blacksmithJob(cfg core.Config) string {
	if cfg.Blacksmith.Job != "" {
		return cfg.Blacksmith.Job
	}
	if blacksmithCanFallbackToActionsField(cfg) {
		return cfg.Actions.Job
	}
	return ""
}

func blacksmithRef(cfg core.Config) string {
	if cfg.Blacksmith.Ref != "" {
		return cfg.Blacksmith.Ref
	}
	if blacksmithCanFallbackToActionsField(cfg) {
		return cfg.Actions.Ref
	}
	return ""
}

func blacksmithCanFallbackToActionsField(cfg core.Config) bool {
	return strings.TrimSpace(cfg.Blacksmith.Workflow) != "" || blacksmithCanFallbackToActionsWorkflow(cfg)
}

func blacksmithCanFallbackToActionsWorkflow(cfg core.Config) bool {
	workflow := strings.TrimSpace(cfg.Actions.Workflow)
	if workflow == "" {
		return false
	}
	return !blacksmithLooksLikeGenericHydrateWorkflow(workflow)
}

func blacksmithLooksLikeGenericHydrateWorkflow(workflow string) bool {
	name := strings.TrimSpace(filepath.Base(workflow))
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	normalized := strings.NewReplacer(" ", "", "-", "", "_", "").Replace(strings.ToLower(stem))
	switch normalized {
	case "hydrate", "crabbox", "crabboxhydrate":
		return true
	}
	return false
}

func blacksmithIdleTimeout(cfg core.Config) time.Duration {
	if cfg.Blacksmith.IdleTimeout > 0 {
		return cfg.Blacksmith.IdleTimeout
	}
	return cfg.IdleTimeout
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

func resolveBlacksmithDiscoveryID(identifier string) (string, error) {
	if identifier == "" {
		return "", core.Exit(2, "blacksmith-testbox requires --id <tbx-id-or-slug>")
	}
	if parseBlacksmithID(identifier) == identifier {
		return identifier, nil
	}
	claim, ok, err := core.ResolveLeaseClaim(identifier)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", core.Exit(4, "unknown blacksmith testbox %q", identifier)
	}
	if claim.Provider != "" && claim.Provider != blacksmithTestboxProvider {
		return "", core.Exit(4, "%q is claimed by provider %s", identifier, claim.Provider)
	}
	return claim.LeaseID, nil
}

func blacksmithCommandString(command []string, shellMode bool) string {
	if len(command) == 0 {
		return ""
	}
	if shellMode || len(command) == 1 {
		return trimBlacksmithShellCommand(strings.Join(command, " "))
	}
	if core.ShouldUseShell(command) {
		return core.ShellScriptFromArgv(command)
	}
	parts := make([]string, 0, len(command))
	seenCommand := false
	for _, word := range command {
		if !seenCommand && core.IsShellEnvAssignment(word) {
			key, value, _ := strings.Cut(word, "=")
			parts = append(parts, key+"="+core.ShellQuote(value))
			continue
		}
		seenCommand = true
		parts = append(parts, core.ShellQuote(word))
	}
	return strings.Join(parts, " ")
}

func trimBlacksmithShellCommand(command string) string {
	return strings.TrimRight(command, " \t\r\n")
}
