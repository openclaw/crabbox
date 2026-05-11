package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

func (a App) job(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return a.jobList(ctx, nil)
	}
	switch args[0] {
	case "list", "ls":
		return a.jobList(ctx, args[1:])
	case "run":
		return a.jobRun(ctx, args[1:])
	default:
		return exit(2, "unknown job subcommand %q", args[0])
	}
}

func (a App) jobList(_ context.Context, args []string) error {
	fs := newFlagSet("job list", a.Stderr)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(cfg.Jobs))
	for name := range cfg.Jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Fprintln(a.Stdout, "no jobs configured")
		return nil
	}
	for _, name := range names {
		job := cfg.Jobs[name]
		fmt.Fprintf(a.Stdout, "%s provider=%s target=%s hydrate_actions=%t stop=%s\n", name, blank(job.Provider, "-"), blank(job.Target, "-"), job.Hydrate.Actions, blank(job.Stop, "auto"))
	}
	return nil
}

func (a App) jobRun(ctx context.Context, args []string) (err error) {
	fs := newFlagSet("job run", a.Stderr)
	id := fs.String("id", "", "existing lease id or slug")
	noHydrate := fs.Bool("no-hydrate", false, "skip configured Actions hydration")
	stopOverride := fs.String("stop", "", "stop policy: auto, always, success, failure, never")
	dryRun := fs.Bool("dry-run", false, "print the planned Crabbox commands without running them")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox job run <name>")
	}
	name := fs.Arg(0)
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	job, ok := cfg.Jobs[name]
	if !ok {
		return exit(2, "job %q is not configured", name)
	}
	if err := validateJobConfig(name, job); err != nil {
		return err
	}
	stopPolicy := normalizeJobStopPolicy(job.Stop, *stopOverride)
	if err := validateJobStopPolicy(stopPolicy); err != nil {
		return err
	}
	leaseID := strings.TrimSpace(*id)
	createdLease := leaseID == ""
	plannedLease := blank(leaseID, "<lease>")
	if *dryRun {
		for _, line := range jobPlanCommands(name, job, plannedLease, createdLease, !*noHydrate, stopPolicy) {
			fmt.Fprintln(a.Stdout, line)
		}
		return nil
	}
	if createdLease {
		var out bytes.Buffer
		warmupApp := App{Stdout: io.MultiWriter(a.Stdout, &out), Stderr: a.Stderr}
		if err := warmupApp.warmup(ctx, append(jobLeaseCreateArgs(job), "--keep=true")); err != nil {
			return err
		}
		leaseID = parseWarmupLeaseID(out.String())
		if leaseID == "" {
			return exit(2, "job %q could not parse warmup lease id", name)
		}
	}
	shouldStop := false
	if createdLease {
		shouldStop = stopPolicy == "" || stopPolicy == "auto" || stopPolicy == "always" || stopPolicy == "success" || stopPolicy == "failure"
	} else {
		shouldStop = stopPolicy == "always" || stopPolicy == "success" || stopPolicy == "failure"
	}
	defer func() {
		if !shouldStop {
			return
		}
		if stopPolicy == "success" && err != nil {
			return
		}
		if stopPolicy == "failure" && err == nil {
			return
		}
		stopArgs := append(jobStopRoutingArgs(job), leaseID)
		if stopErr := a.stop(context.Background(), stopArgs); stopErr != nil && err == nil {
			err = stopErr
		}
	}()
	if job.Hydrate.Actions && !*noHydrate {
		if hydrateErr := a.actionsHydrate(ctx, jobActionsHydrateArgs(job, leaseID)); hydrateErr != nil {
			err = hydrateErr
			return err
		}
	}
	err = a.runCommand(ctx, jobRunArgs(job, leaseID))
	return err
}

func validateJobConfig(name string, job JobConfig) error {
	if strings.TrimSpace(job.Command) == "" && !job.SyncOnly {
		return exit(2, "job %q requires command or syncOnly", name)
	}
	return nil
}

func normalizeJobStopPolicy(configured, override string) string {
	if override != "" {
		return strings.ToLower(strings.TrimSpace(override))
	}
	return strings.ToLower(strings.TrimSpace(configured))
}

func validateJobStopPolicy(policy string) error {
	switch policy {
	case "", "auto", "always", "success", "failure", "never":
		return nil
	default:
		return exit(2, "--stop must be auto, always, success, failure, or never")
	}
}

func parseWarmupLeaseID(out string) string {
	re := regexp.MustCompile(`(?m)^leased\s+([^\s]+)\s`)
	match := re.FindStringSubmatch(out)
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

func jobPlanCommands(name string, job JobConfig, leaseID string, createLease, hydrate bool, stopPolicy string) []string {
	lines := []string{fmt.Sprintf("# job %s", name)}
	if createLease {
		lines = append(lines, "crabbox "+strings.Join(readableShellWords(append([]string{"warmup"}, append(jobLeaseCreateArgs(job), "--keep=true")...)), " "))
	}
	if hydrate && job.Hydrate.Actions {
		lines = append(lines, "crabbox "+strings.Join(readableShellWords(append([]string{"actions", "hydrate"}, jobActionsHydrateArgs(job, leaseID)...)), " "))
	}
	lines = append(lines, "crabbox "+strings.Join(readableShellWords(append([]string{"run"}, jobRunArgs(job, leaseID)...)), " "))
	if shouldPlanJobStop(createLease, stopPolicy) {
		lines = append(lines, "crabbox "+strings.Join(readableShellWords(append([]string{"stop"}, jobStopRoutingArgs(job)...)), " ")+" "+leaseID)
	}
	return lines
}

func shouldPlanJobStop(createdLease bool, stopPolicy string) bool {
	if createdLease && (stopPolicy == "" || stopPolicy == "auto") {
		return true
	}
	switch stopPolicy {
	case "always", "success", "failure":
		return true
	default:
		return false
	}
}

func jobLeaseCreateArgs(job JobConfig) []string {
	args := jobRoutingArgs(job, true)
	if job.Profile != "" {
		args = append(args, "--profile", job.Profile)
	}
	if job.Class != "" {
		args = append(args, "--class", job.Class)
	}
	if job.ServerType != "" {
		args = append(args, "--type", job.ServerType)
	}
	if job.Market != "" {
		args = append(args, "--market", job.Market)
	}
	appendDuration := func(flag string, d time.Duration) {
		if d > 0 {
			args = append(args, flag, d.String())
		}
	}
	appendDuration("--ttl", job.TTL)
	appendDuration("--idle-timeout", job.IdleTimeout)
	if job.Desktop != nil {
		args = append(args, "--desktop="+fmt.Sprint(*job.Desktop))
	}
	if job.Browser != nil {
		args = append(args, "--browser="+fmt.Sprint(*job.Browser))
	}
	if job.Code != nil {
		args = append(args, "--code="+fmt.Sprint(*job.Code))
	}
	return args
}

func jobRoutingArgs(job JobConfig, includeNetwork bool) []string {
	var args []string
	if job.Provider != "" {
		args = append(args, "--provider", job.Provider)
	}
	if job.Target != "" {
		args = append(args, "--target", job.Target)
	}
	if job.WindowsMode != "" {
		args = append(args, "--windows-mode", job.WindowsMode)
	}
	if includeNetwork && job.Network != "" {
		args = append(args, "--network", job.Network)
	}
	return args
}

func jobStopRoutingArgs(job JobConfig) []string {
	return jobRoutingArgs(job, false)
}

func jobActionsHydrateArgs(job JobConfig, leaseID string) []string {
	args := append(jobRoutingArgs(job, true), "--id", leaseID)
	if job.Actions.Repo != "" {
		args = append(args, "--repo", job.Actions.Repo)
	}
	if job.Actions.Workflow != "" {
		args = append(args, "--workflow", job.Actions.Workflow)
	}
	if job.Actions.Ref != "" {
		args = append(args, "--ref", job.Actions.Ref)
	}
	if job.Actions.Job != "" {
		args = append(args, "--job", job.Actions.Job)
	}
	if job.Hydrate.WaitTimeout > 0 {
		args = append(args, "--wait-timeout", job.Hydrate.WaitTimeout.String())
	}
	if job.Hydrate.KeepAliveMinutes > 0 {
		args = append(args, "--keep-alive-minutes", fmt.Sprint(job.Hydrate.KeepAliveMinutes))
	}
	for _, field := range job.Actions.Fields {
		args = append(args, "--field", field)
	}
	return args
}

func jobRunArgs(job JobConfig, leaseID string) []string {
	args := append(jobLeaseCreateArgs(job), "--id", leaseID)
	if job.NoSync {
		args = append(args, "--no-sync")
	}
	if job.SyncOnly {
		args = append(args, "--sync-only")
	}
	if job.Checksum != nil {
		args = append(args, "--checksum="+fmt.Sprint(*job.Checksum))
	}
	if job.ForceSyncLarge {
		args = append(args, "--force-sync-large")
	}
	if len(job.JUnit) > 0 {
		args = append(args, "--junit", strings.Join(job.JUnit, ","))
	}
	for _, download := range job.Downloads {
		args = append(args, "--download", download)
	}
	if strings.TrimSpace(job.Command) == "" {
		return args
	}
	if job.Shell {
		args = append(args, "--shell", "--", job.Command)
		return args
	}
	args = append(args, "--")
	args = append(args, strings.Fields(job.Command)...)
	return args
}
