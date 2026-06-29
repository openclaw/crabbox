package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	dataRunModeExecute = "execute"
	dataRunModeDryRun  = "dry-run"
)

func (a App) dataList(_ context.Context, args []string) error {
	fs := newFlagSet("data list", a.Stderr)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(cfg.DataRuns))
	for name := range cfg.DataRuns {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Fprintln(a.Stdout, "no data runs configured")
		return nil
	}
	for _, name := range names {
		run := cfg.DataRuns[name]
		manifestPath, _ := dataRunManifestPath(name, dataRunModeExecute, run)
		fmt.Fprintf(a.Stdout, "%s provider=%s target=%s source=%s:%s sink=%s:%s manifest=%s\n",
			name,
			blank(run.Job.Provider, "-"),
			blank(run.Job.Target, "-"),
			blank(run.Source.Kind, "-"),
			blank(run.Source.URI, "-"),
			blank(run.Sink.Kind, "-"),
			blank(run.Sink.URI, "-"),
			blank(manifestPath, "-"),
		)
	}
	return nil
}

func (a App) dataPlan(_ context.Context, args []string) error {
	fs := newFlagSet("data plan", a.Stderr)
	modeFlag := fs.String("mode", dataRunModeExecute, "data run mode: execute or dry-run")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox data plan <name>")
	}
	mode, err := normalizeDataRunMode(*modeFlag)
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	name := fs.Arg(0)
	run, err := configuredDataRun(cfg, name)
	if err != nil {
		return err
	}
	if err := validateDataRunConfig(name, run); err != nil {
		return err
	}
	if err := validateDataRunExecutionSupport(cfg, name, run, mode); err != nil {
		return err
	}
	manifestPath, err := dataRunManifestPath(name, mode, run)
	if err != nil {
		return err
	}
	dataRunPrintPlan(a.Stdout, cfg, name, run, mode, manifestPath)
	return nil
}

func (a App) dataRun(ctx context.Context, args []string) (err error) {
	fs := newFlagSet("data run", a.Stderr)
	id := fs.String("id", "", "existing lease id or slug")
	noHydrate := fs.Bool("no-hydrate", false, "skip configured Actions hydration")
	githubRunner := fs.Bool("github-runner", false, "hydrate by registering a GitHub self-hosted runner instead of local SSH execution")
	stopOverride := fs.String("stop", "", "stop policy: auto, always, success, failure, never")
	dryRun := fs.Bool("dry-run", false, "run in data dry-run mode instead of execute mode")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox data run <name>")
	}
	mode := dataRunModeExecute
	if *dryRun {
		mode = dataRunModeDryRun
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	name := fs.Arg(0)
	run, err := configuredDataRun(cfg, name)
	if err != nil {
		return err
	}
	if err := validateDataRunConfig(name, run); err != nil {
		return err
	}
	if mode == dataRunModeExecute && run.Policy.RequireDryRun {
		return exit(2, "data run %q requires dry-run first; run `crabbox data run --dry-run %s`, then set policy.requireDryRun=false for execute until broker dry-run history exists", name, name)
	}
	if err := validateDataRunExecutionSupport(cfg, name, run, mode); err != nil {
		return err
	}
	stopPolicy := normalizeJobStopPolicy(run.Job.Stop, *stopOverride)
	if err := validateJobStopPolicy(stopPolicy); err != nil {
		return err
	}
	manifestPath, err := dataRunManifestPath(name, mode, run)
	if err != nil {
		return err
	}
	leaseID := strings.TrimSpace(*id)
	createdLease := leaseID == ""
	runNoHydrate := *noHydrate || !run.Job.Hydrate.Actions
	if createdLease {
		var out strings.Builder
		requestedSlug := dataRunLeaseSlug(name)
		warmupApp := App{Stdout: io.MultiWriter(a.Stdout, &out), Stderr: a.Stderr}
		if err := warmupApp.warmup(ctx, append(jobLeaseCreateArgs(cfg, run.Job), "--keep=true", "--slug", requestedSlug)); err != nil {
			return err
		}
		leaseID = parseWarmupLeaseID(out.String())
		if leaseID == "" {
			stopArgs := append(jobStopRoutingArgs(run.Job), requestedSlug)
			if stopErr := a.stop(context.Background(), stopArgs); stopErr != nil {
				return exit(2, "data run %q could not parse warmup lease id; cleanup by slug %s failed: %v", name, requestedSlug, stopErr)
			}
			return exit(2, "data run %q could not parse warmup lease id", name)
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
		stopArgs := append(jobStopRoutingArgs(run.Job), leaseID)
		if stopErr := a.stop(context.Background(), stopArgs); stopErr != nil && err == nil {
			err = stopErr
		}
	}()
	if run.Job.Hydrate.Actions && !*noHydrate {
		if hydrateErr := a.actionsHydrate(ctx, jobActionsHydrateArgs(run.Job, leaseID, *githubRunner)); hydrateErr != nil {
			err = hydrateErr
			return err
		}
	}
	manifestDownload := ""
	if dataRunManifestRequired(mode, run) {
		tmp, err := os.MkdirTemp("", "crabbox-data-manifest-*")
		if err != nil {
			return exit(2, "create data manifest temp dir: %v", err)
		}
		defer os.RemoveAll(tmp)
		manifestDownload = filepath.Join(tmp, safeCaptureName(name)+"-manifest.json")
	}
	env := dataRunEnv(name, mode, run, manifestPath)
	restoreEnv := setTemporaryEnv(env)
	defer restoreEnv()
	err = a.runCommand(ctx, dataRunRunArgs(cfg, name, run, leaseID, runNoHydrate, mode, manifestPath, manifestDownload))
	if err != nil {
		return err
	}
	if manifestDownload != "" {
		if err := validateDataRunManifestFile(manifestDownload, name, mode); err != nil {
			return err
		}
		info, statErr := os.Stat(manifestDownload)
		if statErr == nil {
			fmt.Fprintf(a.Stderr, "data manifest validated path=%s bytes=%d\n", manifestPath, info.Size())
		}
	}
	return nil
}

func (a App) dataPromote(_ context.Context, args []string) error {
	fs := newFlagSet("data promote", a.Stderr)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	return exit(2, "crabbox data promote is not implemented in the POC; promote staging output outside Crabbox and keep the manifest artifact")
}

func (a App) dataManifest(_ context.Context, args []string) error {
	fs := newFlagSet("data manifest", a.Stderr)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	return exit(2, "crabbox data manifest is not implemented in the POC; use the validated manifest artifact from data run output")
}

func configuredDataRun(cfg Config, name string) (DataRunConfig, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return DataRunConfig{}, exit(2, "data run name is required")
	}
	run, ok := cfg.DataRuns[name]
	if !ok {
		return DataRunConfig{}, exit(2, "data run %q is not configured", name)
	}
	return run, nil
}

func validateDataRunConfig(name string, run DataRunConfig) error {
	if strings.TrimSpace(run.Job.Command) == "" {
		return exit(2, "data run %q requires command", name)
	}
	if run.Job.SyncOnly {
		return exit(2, "data run %q cannot use syncOnly", name)
	}
	sourceMode := dataRunSourceMode(run)
	if sourceMode != "read" {
		return exit(2, "data run %q source.mode must be read", name)
	}
	sinkMode := dataRunSinkMode(run)
	switch sinkMode {
	case "write", "write-staging":
	default:
		return exit(2, "data run %q sink.mode must be write or write-staging", name)
	}
	if strings.TrimSpace(run.Source.Kind) == "" || strings.TrimSpace(run.Source.URI) == "" {
		return exit(2, "data run %q requires source.kind and source.uri", name)
	}
	if strings.TrimSpace(run.Sink.Kind) == "" || strings.TrimSpace(run.Sink.URI) == "" {
		return exit(2, "data run %q requires sink.kind and sink.uri", name)
	}
	if run.Policy.PIILogging != "" {
		switch strings.ToLower(strings.TrimSpace(run.Policy.PIILogging)) {
		case "forbid", "redact", "allow":
		default:
			return exit(2, "data run %q policy.piiLogging must be forbid, redact, or allow", name)
		}
	}
	if _, err := dataRunManifestPath(name, dataRunModeExecute, run); err != nil {
		return err
	}
	return nil
}

func validateDataRunExecutionSupport(cfg Config, name string, run DataRunConfig, mode string) error {
	if !dataRunManifestRequired(mode, run) {
		return nil
	}
	manifestPath, err := dataRunManifestPath(name, mode, run)
	if err != nil {
		return err
	}
	if dataRunManifestPathIgnoredByArtifacts(manifestPath) {
		return exit(2, "data run %q manifest.path must not be under .crabbox when manifest is required", name)
	}
	providerName := strings.TrimSpace(firstNonBlank(run.Job.Provider, cfg.Provider))
	if providerName == "" {
		return nil
	}
	provider, err := ProviderFor(providerName)
	if err != nil {
		return err
	}
	spec := provider.Spec()
	if spec.Kind != ProviderKindDelegatedRun {
		return nil
	}
	if featureSetHas(spec.Features, FeatureRunDownloads) {
		return nil
	}
	return exit(2, "data run %q execute mode requires manifest download support; provider %s delegates run execution and cannot download required artifacts", name, spec.Name)
}

func normalizeDataRunMode(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", dataRunModeExecute:
		return dataRunModeExecute, nil
	case dataRunModeDryRun, "dryrun", "dry_run":
		return dataRunModeDryRun, nil
	default:
		return "", exit(2, "data run mode must be execute or dry-run")
	}
}

func dataRunSourceMode(run DataRunConfig) string {
	return firstNonBlank(strings.ToLower(strings.TrimSpace(run.Source.Mode)), "read")
}

func dataRunSinkMode(run DataRunConfig) string {
	return firstNonBlank(strings.ToLower(strings.TrimSpace(run.Sink.Mode)), "write-staging")
}

func dataRunManifestRequired(mode string, run DataRunConfig) bool {
	return mode == dataRunModeExecute && (!run.Manifest.RequiredSet || run.Manifest.Required)
}

func dataRunManifestPath(name, mode string, run DataRunConfig) (string, error) {
	value := strings.TrimSpace(run.Manifest.Path)
	if value == "" {
		value = "crabbox-data/" + safeCaptureName(name) + "/" + mode + "-manifest.json"
	}
	clean, err := cleanDataRunManifestPath(value)
	if err != nil {
		return "", err
	}
	return clean, nil
}

func dataRunManifestPathIgnoredByArtifacts(value string) bool {
	clean := path.Clean(strings.TrimSpace(strings.ReplaceAll(value, `\`, "/")))
	return clean == ".crabbox" || strings.HasPrefix(clean, ".crabbox/")
}

func dataRunLeaseSlug(name string) string {
	base := normalizeLeaseSlug("data-" + name)
	if base == "" {
		base = "data-run"
	}
	suffix := fmt.Sprintf("%06x", uint64(time.Now().UnixNano())&0xffffff)
	maxBase := maxRequestedLeaseSlugLength - len(suffix) - 1
	if len(base) > maxBase {
		base = strings.Trim(base[:maxBase], "-")
	}
	if base == "" {
		base = "data"
	}
	return base + "-" + suffix
}

func cleanDataRunManifestPath(value string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, `\`, "/"))
	if value == "" {
		return "", exit(2, "data manifest path must not be empty")
	}
	if strings.HasPrefix(value, "/") {
		return "", exit(2, "data manifest path must be relative to the run workdir")
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", exit(2, "data manifest path must stay inside the run workdir")
	}
	return clean, nil
}

func dataRunEnv(name, mode string, run DataRunConfig, manifestPath string) map[string]string {
	env := map[string]string{
		"CRABBOX_DATA_RUN":         "1",
		"CRABBOX_DATA_RUN_NAME":    strings.TrimSpace(name),
		"CRABBOX_DATA_MODE":        mode,
		"CRABBOX_DATA_SOURCE_KIND": strings.TrimSpace(run.Source.Kind),
		"CRABBOX_DATA_SOURCE_MODE": dataRunSourceMode(run),
		"CRABBOX_DATA_SOURCE_URI":  strings.TrimSpace(run.Source.URI),
		"CRABBOX_DATA_SINK_KIND":   strings.TrimSpace(run.Sink.Kind),
		"CRABBOX_DATA_SINK_MODE":   dataRunSinkMode(run),
		"CRABBOX_DATA_SINK_URI":    strings.TrimSpace(run.Sink.URI),
		"CRABBOX_DATA_MANIFEST":    manifestPath,
	}
	if strings.TrimSpace(run.Source.Watermark) != "" {
		env["CRABBOX_DATA_SOURCE_WATERMARK"] = strings.TrimSpace(run.Source.Watermark)
	}
	return env
}

func dataRunEnvNames(env map[string]string) []string {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func setTemporaryEnv(values map[string]string) func() {
	type previous struct {
		value string
		ok    bool
	}
	old := map[string]previous{}
	for key, value := range values {
		current, ok := os.LookupEnv(key)
		old[key] = previous{value: current, ok: ok}
		_ = os.Setenv(key, value)
	}
	return func() {
		for key, value := range old {
			if value.ok {
				_ = os.Setenv(key, value.value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
	}
}

func dataRunRunArgs(cfg Config, name string, run DataRunConfig, leaseID string, noHydrate bool, mode string, manifestPath string, manifestDownload string) []string {
	job := run.Job
	if strings.TrimSpace(job.Label) == "" {
		job.Label = "data:" + name + ":" + mode
	}
	args := jobRunArgs(cfg, job, leaseID, noHydrate)
	env := dataRunEnv(name, mode, run, manifestPath)
	extra := []string{"--allow-env", strings.Join(dataRunEnvNames(env), ",")}
	if dataRunManifestRequired(mode, run) {
		extra = append(extra, "--require-artifact", manifestPath)
		if strings.TrimSpace(manifestDownload) != "" {
			extra = append(extra, "--download", manifestPath+"="+manifestDownload)
		}
	}
	return insertRunArgsBeforeCommand(args, extra...)
}

func insertRunArgsBeforeCommand(args []string, extra ...string) []string {
	if len(extra) == 0 {
		return args
	}
	for i, arg := range args {
		if arg == "--" {
			out := make([]string, 0, len(args)+len(extra))
			out = append(out, args[:i]...)
			out = append(out, extra...)
			out = append(out, args[i:]...)
			return out
		}
	}
	return append(args, extra...)
}

func dataRunPrintPlan(w io.Writer, cfg Config, name string, run DataRunConfig, mode string, manifestPath string) {
	fmt.Fprintf(w, "data run %s mode=%s status=poc\n", name, mode)
	fmt.Fprintf(w, "provider=%s target=%s\n", blank(firstNonBlank(run.Job.Provider, cfg.Provider), "-"), blank(firstNonBlank(run.Job.Target, cfg.TargetOS), "-"))
	fmt.Fprintf(w, "source kind=%s mode=%s uri=%s\n", run.Source.Kind, dataRunSourceMode(run), run.Source.URI)
	fmt.Fprintf(w, "sink kind=%s mode=%s uri=%s\n", run.Sink.Kind, dataRunSinkMode(run), run.Sink.URI)
	fmt.Fprintf(w, "manifest path=%s required=%t\n", manifestPath, dataRunManifestRequired(mode, run))
	fmt.Fprintf(w, "policy source.mode=%s declared=only\n", dataRunSourceMode(run))
	fmt.Fprintf(w, "policy sink.mode=%s declared=only\n", dataRunSinkMode(run))
	if run.Job.TTL > 0 {
		fmt.Fprintf(w, "policy ttl=%s enforced=lease\n", run.Job.TTL)
	}
	if run.Policy.RequireDryRun {
		fmt.Fprintln(w, "policy requireDryRun=true enforced=execute-gate")
	}
	if run.Policy.MaxBytes != "" {
		fmt.Fprintf(w, "policy maxBytes=%s declared=only\n", run.Policy.MaxBytes)
	}
	if run.Policy.MaxRows > 0 {
		fmt.Fprintf(w, "policy maxRows=%d declared=only\n", run.Policy.MaxRows)
	}
	if len(run.Policy.EgressAllow) > 0 {
		fmt.Fprintln(w, "policy egress.allow declared=only")
	}
	env := dataRunEnv(name, mode, run, manifestPath)
	envWords := []string{"env"}
	for _, key := range dataRunEnvNames(env) {
		envWords = append(envWords, key+"="+env[key])
	}
	runArgs := dataRunRunArgs(cfg, name, run, "<lease>", true, mode, manifestPath, "")
	fmt.Fprintln(w, strings.Join(readableShellWords(append(envWords, append([]string{"crabbox", "run"}, runArgs...)...)), " "))
}

func validateDataRunManifestFile(localPath, name, mode string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return exit(7, "read data manifest: %v", err)
	}
	var manifest struct {
		SchemaVersion int             `json:"schemaVersion"`
		DataRun       string          `json:"dataRun"`
		Mode          string          `json:"mode"`
		Source        json.RawMessage `json:"source"`
		Sink          json.RawMessage `json:"sink"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return exit(7, "data manifest is not valid JSON: %v", err)
	}
	if manifest.SchemaVersion <= 0 {
		return exit(7, "data manifest missing positive schemaVersion")
	}
	if strings.TrimSpace(manifest.DataRun) != name {
		return exit(7, "data manifest dataRun=%q, want %q", manifest.DataRun, name)
	}
	if strings.TrimSpace(manifest.Mode) != mode {
		return exit(7, "data manifest mode=%q, want %q", manifest.Mode, mode)
	}
	if len(manifest.Source) == 0 || string(manifest.Source) == "null" {
		return exit(7, "data manifest missing source")
	}
	if len(manifest.Sink) == 0 || string(manifest.Sink) == "null" {
		return exit(7, "data manifest missing sink")
	}
	return nil
}
