package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	dataRunDefaultManifest   = "reports/data/manifest.json"
	dataRunManifestSchema    = "crabbox.data-run.v1"
	dataRunSummarySchema     = "crabbox.data-run-summary.v1"
	dataRunMaxManifestBytes  = 64 * 1024
	dataRunMaxStringBytes    = 2048
	dataRunMaxCollectionSize = 128
	dataRunMaxSummaryKeys    = 32
)

type DataRunConfig struct {
	Provider          string
	Target            string
	WindowsMode       string
	Profile           string
	Class             string
	Architecture      string
	ServerType        string
	Market            string
	TTL               time.Duration
	IdleTimeout       time.Duration
	Network           string
	Shell             bool
	Command           string
	CommandArgs       []string
	NoSync            bool
	ForceSyncLarge    bool
	Manifest          string
	RequiredArtifacts []string
	ArtifactGlobs     []string
	JUnit             []string
	Downloads         []string
	Policy            DataRunPolicyConfig
}

type DataRunPolicyConfig struct {
	SourceIdentity string
	SinkIdentity   string
	Egress         string
	Promotion      string
	Enforcement    string
}

type dataRunManifest struct {
	SchemaVersion string                    `json:"schemaVersion"`
	Name          string                    `json:"name,omitempty"`
	Status        string                    `json:"status"`
	Inputs        []dataRunManifestDataset  `json:"inputs,omitempty"`
	Outputs       []dataRunManifestDataset  `json:"outputs,omitempty"`
	Summary       map[string]any            `json:"summary,omitempty"`
	Artifacts     []dataRunManifestArtifact `json:"artifacts,omitempty"`
	Policy        dataRunManifestPolicy     `json:"policy,omitempty"`
	Promotion     dataRunManifestPromotion  `json:"promotion,omitempty"`
}

type dataRunManifestDataset struct {
	Name     string `json:"name"`
	Identity string `json:"identity,omitempty"`
	Rows     int64  `json:"rows,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
}

type dataRunManifestArtifact struct {
	Path  string `json:"path"`
	Kind  string `json:"kind,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`
}

type dataRunManifestPolicy struct {
	SourceIdentity string `json:"sourceIdentity,omitempty"`
	SinkIdentity   string `json:"sinkIdentity,omitempty"`
	Egress         string `json:"egress,omitempty"`
	Enforcement    string `json:"enforcement,omitempty"`
}

type dataRunManifestPromotion struct {
	Mode        string `json:"mode,omitempty"`
	Target      string `json:"target,omitempty"`
	Enforcement string `json:"enforcement,omitempty"`
}

type DataRunSummary struct {
	SchemaVersion string                   `json:"schemaVersion"`
	Name          string                   `json:"name,omitempty"`
	Status        string                   `json:"status"`
	ManifestPath  string                   `json:"manifestPath"`
	Inputs        int                      `json:"inputs"`
	Outputs       int                      `json:"outputs"`
	OutputRows    int64                    `json:"outputRows,omitempty"`
	OutputBytes   int64                    `json:"outputBytes,omitempty"`
	Artifacts     int                      `json:"artifacts,omitempty"`
	Policy        dataRunManifestPolicy    `json:"policy,omitempty"`
	Promotion     dataRunManifestPromotion `json:"promotion,omitempty"`
	Summary       map[string]any           `json:"summary,omitempty"`
	GeneratedAt   string                   `json:"generatedAt"`
}

func (a App) data(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return a.dataList(nil)
	}
	switch args[0] {
	case "list", "ls":
		return a.dataList(args[1:])
	case "run":
		return a.dataRun(ctx, args[1:])
	case "validate-manifest":
		return a.dataValidateManifest(args[1:])
	default:
		return exit(2, "unknown data subcommand %q", args[0])
	}
}

func (a App) dataList(args []string) error {
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
		fmt.Fprintf(a.Stdout, "%s manifest=%s provider=%s target=%s policy=%s\n",
			name,
			blank(run.Manifest, dataRunDefaultManifest),
			blank(run.Provider, "-"),
			blank(run.Target, "-"),
			dataRunPolicyDisplay(run.Policy),
		)
	}
	return nil
}

func (a App) dataRun(ctx context.Context, args []string) error {
	fs := newFlagSet("data run", a.Stderr)
	id := fs.String("id", "", "existing lease id or slug")
	noHydrate := fs.Bool("no-hydrate", false, "skip configured Actions hydration")
	dryRun := fs.Bool("dry-run", false, "print the planned Crabbox commands without running them")
	manifestOverride := fs.String("manifest", "", "remote manifest path to require and validate")
	manifestOutput := fs.String("manifest-output", "", "local manifest download path")
	summaryOutput := fs.String("summary-output", "", "local bounded summary JSON path")
	timingJSON := fs.Bool("timing-json", false, "forward --timing-json to the underlying run")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox data run <name>")
	}
	name := fs.Arg(0)
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	run, ok := cfg.DataRuns[name]
	if !ok {
		return exit(2, "data run %q is not configured", name)
	}
	if err := validateDataRunConfig(name, run); err != nil {
		return err
	}
	manifestRemote := firstNonBlank(*manifestOverride, run.Manifest, dataRunDefaultManifest)
	if err := validateDataRunManifestPath(manifestRemote); err != nil {
		return err
	}
	manifestLocal := firstNonBlank(*manifestOutput, defaultDataRunManifestOutput(name))
	summaryLocal := firstNonBlank(*summaryOutput, defaultDataRunSummaryOutput(name))
	if err := validateDataRunLocalOutputPath("manifest output", manifestLocal); err != nil {
		return err
	}
	if err := validateDataRunLocalOutputPath("summary output", summaryLocal); err != nil {
		return err
	}
	runArgs, err := dataRunArgs(cfg, name, run, *id, *noHydrate, manifestRemote, manifestLocal, *timingJSON)
	if err != nil {
		return err
	}
	if *dryRun {
		for _, line := range dataRunPlanCommands(name, run, runArgs, manifestLocal, summaryLocal) {
			fmt.Fprintln(a.Stdout, line)
		}
		return nil
	}
	if display := dataRunPolicyDisplay(run.Policy); display != "-" {
		fmt.Fprintf(a.Stderr, "data policy %s\n", display)
	}
	var runStderr bytes.Buffer
	runApp := a
	runApp.Stderr = io.MultiWriter(a.Stderr, &runStderr)
	if err := runApp.runCommand(ctx, runArgs); err != nil {
		return err
	}
	summary, err := validateDataRunManifestFile(manifestLocal, name)
	if err != nil {
		return err
	}
	summary.Policy = mergeDataRunPolicySummary(summary.Policy, run.Policy)
	if summary.Policy.Enforcement == "" {
		result, err := dataRunProviderPolicy(ctx, dataRunPolicyConfig(cfg, run), run.Policy)
		if err != nil {
			return err
		}
		summary.Policy.Enforcement = result.Enforcement
	}
	if runID := dataRunRecordedRunID(runStderr.String()); runID != "" {
		if err := a.attachDataRunSummary(ctx, cfg, runID, summary); err != nil {
			return err
		}
	}
	if err := writeDataRunSummaryFile(summaryLocal, summary); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "data run %s manifest ok status=%s inputs=%d outputs=%d summary=%s\n", name, summary.Status, summary.Inputs, summary.Outputs, summaryLocal)
	return nil
}

func (a App) dataValidateManifest(args []string) error {
	fs := newFlagSet("data validate-manifest", a.Stderr)
	summaryOutput := fs.String("summary-output", "", "write bounded summary JSON to this path")
	jsonOut := fs.Bool("json", false, "print bounded summary JSON")
	name := fs.String("name", "", "data run name to attach to the summary")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox data validate-manifest <path>")
	}
	summary, err := validateDataRunManifestFile(fs.Arg(0), *name)
	if err != nil {
		return err
	}
	if *summaryOutput != "" {
		if err := writeDataRunSummaryFile(*summaryOutput, summary); err != nil {
			return err
		}
	}
	if *jsonOut {
		return writeDataRunSummary(a.Stdout, summary)
	}
	fmt.Fprintf(a.Stdout, "manifest ok status=%s inputs=%d outputs=%d artifacts=%d\n", summary.Status, summary.Inputs, summary.Outputs, summary.Artifacts)
	return nil
}

func (a App) attachDataRunSummary(ctx context.Context, cfg Config, runID string, summary DataRunSummary) error {
	if strings.TrimSpace(cfg.Coordinator) == "" || strings.TrimSpace(runID) == "" {
		return nil
	}
	client, _, err := newCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if _, err := client.UpdateRunDataSummary(ctx, runID, summary); err != nil {
		return err
	}
	fmt.Fprintf(a.Stderr, "recorded data summary run=%s\n", runID)
	return nil
}

func dataRunRecordedRunID(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") {
			var report TimingReport
			if err := json.Unmarshal([]byte(line), &report); err == nil && strings.TrimSpace(report.RunID) != "" {
				return strings.TrimSpace(report.RunID)
			}
		}
		if strings.HasPrefix(line, "run details ") {
			for _, field := range strings.Fields(line) {
				if strings.HasPrefix(field, "run=") {
					value := strings.TrimPrefix(field, "run=")
					if value != "" && value != "-" {
						return value
					}
				}
			}
		}
	}
	return ""
}

func dataRunProviderPolicy(ctx context.Context, cfg Config, policy DataRunPolicyConfig) (DataRunPolicyResult, error) {
	if dataRunPolicyDisplay(policy) == "-" {
		return DataRunPolicyResult{}, nil
	}
	backend, err := loadBackend(cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		return DataRunPolicyResult{}, err
	}
	if policyBackend, ok := backend.(DataRunPolicyBackend); ok {
		return policyBackend.DataRunPolicy(ctx, DataRunPolicyRequest{
			SourceIdentity: policy.SourceIdentity,
			SinkIdentity:   policy.SinkIdentity,
			Egress:         policy.Egress,
			Promotion:      policy.Promotion,
		})
	}
	return DataRunPolicyResult{Enforcement: "declared-only"}, nil
}

func dataRunPolicyConfig(cfg Config, run DataRunConfig) Config {
	if strings.TrimSpace(run.Provider) != "" {
		cfg.Provider = strings.TrimSpace(run.Provider)
	}
	if strings.TrimSpace(run.Target) != "" {
		cfg.TargetOS = strings.TrimSpace(run.Target)
	}
	if strings.TrimSpace(run.WindowsMode) != "" {
		cfg.WindowsMode = strings.TrimSpace(run.WindowsMode)
	}
	if strings.TrimSpace(run.Class) != "" {
		cfg.Class = strings.TrimSpace(run.Class)
	}
	if strings.TrimSpace(run.ServerType) != "" {
		cfg.ServerType = strings.TrimSpace(run.ServerType)
	}
	if strings.TrimSpace(run.Market) != "" {
		cfg.Capacity.Market = strings.TrimSpace(run.Market)
	}
	if run.TTL > 0 {
		cfg.TTL = run.TTL
	}
	if run.IdleTimeout > 0 {
		cfg.IdleTimeout = run.IdleTimeout
	}
	if strings.TrimSpace(run.Network) != "" {
		cfg.Network = NetworkMode(strings.TrimSpace(run.Network))
	}
	return cfg
}

func mergeDataRunPolicySummary(summary dataRunManifestPolicy, policy DataRunPolicyConfig) dataRunManifestPolicy {
	if summary.SourceIdentity == "" {
		summary.SourceIdentity = strings.TrimSpace(policy.SourceIdentity)
	}
	if summary.SinkIdentity == "" {
		summary.SinkIdentity = strings.TrimSpace(policy.SinkIdentity)
	}
	if summary.Egress == "" {
		summary.Egress = strings.TrimSpace(policy.Egress)
	}
	if summary.Enforcement == "" {
		summary.Enforcement = strings.TrimSpace(policy.Enforcement)
	}
	return summary
}

func validateDataRunConfig(name string, run DataRunConfig) error {
	if strings.TrimSpace(name) == "" {
		return exit(2, "data run name is required")
	}
	if strings.TrimSpace(run.Command) == "" && len(run.CommandArgs) == 0 {
		return exit(2, "data run %q requires command or commandArgs", name)
	}
	if err := validateDataRunManifestPath(blank(run.Manifest, dataRunDefaultManifest)); err != nil {
		return err
	}
	if err := validateRequiredRunArtifactGlobs(run.RequiredArtifacts); err != nil {
		return err
	}
	if err := validateRunArtifactGlobs(run.ArtifactGlobs); err != nil {
		return err
	}
	for _, download := range run.Downloads {
		spec, err := parseRunDownloadSpec(download)
		if err != nil {
			return err
		}
		if err := validateDataRunLocalOutputPath("download "+spec.Remote, spec.Local); err != nil {
			return err
		}
	}
	return validateDataRunPolicy(run.Policy)
}

func dataRunArgs(cfg Config, name string, run DataRunConfig, leaseID string, noHydrate bool, manifestRemote, manifestLocal string, timingJSON bool) ([]string, error) {
	job := dataRunJobConfig(run)
	args := jobLeaseCreateArgsFor(cfg, job, false)
	if leaseID != "" {
		args = append(args, "--id", leaseID)
	}
	if noHydrate {
		args = append(args, "--no-hydrate")
	}
	if run.NoSync {
		args = append(args, "--no-sync")
	}
	if run.ForceSyncLarge {
		args = append(args, "--force-sync-large")
	}
	args = append(args, "--label", "data:"+name)
	required := appendUniqueStrings([]string{manifestRemote}, run.RequiredArtifacts...)
	if err := validateRequiredRunArtifactGlobs(required); err != nil {
		return nil, err
	}
	for _, glob := range required {
		args = append(args, "--require-artifact", glob)
	}
	for _, glob := range run.ArtifactGlobs {
		args = append(args, "--artifact-glob", glob)
	}
	if len(run.JUnit) > 0 {
		args = append(args, "--junit", strings.Join(run.JUnit, ","))
	}
	args = append(args, "--download", manifestRemote+"="+manifestLocal)
	for _, download := range run.Downloads {
		args = append(args, "--download", download)
	}
	if timingJSON {
		args = append(args, "--timing-json")
	}
	if len(run.CommandArgs) > 0 {
		args = append(args, "--")
		args = append(args, run.CommandArgs...)
		return args, nil
	}
	if run.Shell {
		args = append(args, "--shell", "--", run.Command)
		return args, nil
	}
	args = append(args, "--")
	args = append(args, strings.Fields(run.Command)...)
	return args, nil
}

func dataRunJobConfig(run DataRunConfig) JobConfig {
	return JobConfig{
		Provider:       run.Provider,
		Target:         run.Target,
		WindowsMode:    run.WindowsMode,
		Profile:        run.Profile,
		Class:          run.Class,
		Architecture:   run.Architecture,
		ServerType:     run.ServerType,
		Market:         run.Market,
		TTL:            run.TTL,
		IdleTimeout:    run.IdleTimeout,
		Network:        run.Network,
		Shell:          run.Shell,
		Command:        run.Command,
		NoSync:         run.NoSync,
		ForceSyncLarge: run.ForceSyncLarge,
		JUnit:          run.JUnit,
		Downloads:      run.Downloads,
	}
}

func dataRunPlanCommands(name string, run DataRunConfig, runArgs []string, manifestLocal, summaryLocal string) []string {
	lines := []string{fmt.Sprintf("# data run %s", name)}
	if display := dataRunPolicyDisplay(run.Policy); display != "-" {
		lines = append(lines, "# data policy "+display)
	}
	lines = append(lines, "crabbox "+strings.Join(readableShellWords(append([]string{"run"}, runArgs...)), " "))
	lines = append(lines, "crabbox "+strings.Join(readableShellWords([]string{"data", "validate-manifest", "--name", name, "--summary-output", summaryLocal, manifestLocal}), " "))
	return lines
}

func validateDataRunManifestFile(path, runName string) (DataRunSummary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return DataRunSummary{}, exit(7, "read data manifest %s: %v", path, err)
	}
	if len(data) == 0 {
		return DataRunSummary{}, exit(7, "data manifest %s is empty", path)
	}
	if len(data) > dataRunMaxManifestBytes {
		return DataRunSummary{}, exit(7, "data manifest %s exceeds %d bytes", path, dataRunMaxManifestBytes)
	}
	var raw any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return DataRunSummary{}, exit(7, "parse data manifest %s: %v", path, err)
	}
	if err := rejectTrailingDataRunJSON(decoder, path); err != nil {
		return DataRunSummary{}, err
	}
	if err := scanDataRunManifestSafety("$", raw); err != nil {
		return DataRunSummary{}, err
	}
	var manifest dataRunManifest
	typedDecoder := json.NewDecoder(bytes.NewReader(data))
	typedDecoder.DisallowUnknownFields()
	if err := typedDecoder.Decode(&manifest); err != nil {
		return DataRunSummary{}, exit(7, "parse data manifest %s: %v", path, err)
	}
	if err := rejectTrailingDataRunJSON(typedDecoder, path); err != nil {
		return DataRunSummary{}, err
	}
	return summarizeDataRunManifest(path, runName, manifest)
}

func rejectTrailingDataRunJSON(decoder *json.Decoder, path string) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return exit(7, "parse data manifest %s: trailing JSON value", path)
		}
		return exit(7, "parse data manifest %s: trailing content: %v", path, err)
	}
	return nil
}

func summarizeDataRunManifest(path, runName string, manifest dataRunManifest) (DataRunSummary, error) {
	if manifest.SchemaVersion != dataRunManifestSchema {
		return DataRunSummary{}, exit(7, "data manifest schemaVersion must be %q", dataRunManifestSchema)
	}
	status := strings.ToLower(strings.TrimSpace(manifest.Status))
	switch status {
	case "success":
	case "":
		return DataRunSummary{}, exit(7, "data manifest status is required")
	default:
		return DataRunSummary{}, exit(7, "data manifest status must be success for a successful data run")
	}
	if len(manifest.Outputs) == 0 {
		return DataRunSummary{}, exit(7, "data manifest requires at least one output")
	}
	if len(manifest.Inputs) > dataRunMaxCollectionSize || len(manifest.Outputs) > dataRunMaxCollectionSize || len(manifest.Artifacts) > dataRunMaxCollectionSize {
		return DataRunSummary{}, exit(7, "data manifest collections must contain at most %d entries", dataRunMaxCollectionSize)
	}
	var rows, bytes int64
	for _, input := range manifest.Inputs {
		if err := validateDataRunDataset("input", input); err != nil {
			return DataRunSummary{}, err
		}
	}
	for _, output := range manifest.Outputs {
		if err := validateDataRunDataset("output", output); err != nil {
			return DataRunSummary{}, err
		}
		rows += output.Rows
		bytes += output.Bytes
	}
	for _, artifact := range manifest.Artifacts {
		if err := validateDataRunManifestArtifact(artifact); err != nil {
			return DataRunSummary{}, err
		}
	}
	if err := validateDataRunManifestPolicy(manifest.Policy, manifest.Promotion); err != nil {
		return DataRunSummary{}, err
	}
	if err := validateDataRunSummaryMap(manifest.Summary); err != nil {
		return DataRunSummary{}, err
	}
	name := firstNonBlank(strings.TrimSpace(runName), strings.TrimSpace(manifest.Name))
	return DataRunSummary{
		SchemaVersion: dataRunSummarySchema,
		Name:          name,
		Status:        status,
		ManifestPath:  path,
		Inputs:        len(manifest.Inputs),
		Outputs:       len(manifest.Outputs),
		OutputRows:    rows,
		OutputBytes:   bytes,
		Artifacts:     len(manifest.Artifacts),
		Policy:        manifest.Policy,
		Promotion:     manifest.Promotion,
		Summary:       manifest.Summary,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func validateDataRunDataset(kind string, dataset dataRunManifestDataset) error {
	if strings.TrimSpace(dataset.Name) == "" {
		return exit(7, "data manifest %s name is required", kind)
	}
	if dataset.Rows < 0 || dataset.Bytes < 0 {
		return exit(7, "data manifest %s counts cannot be negative", kind)
	}
	return nil
}

func validateDataRunManifestArtifact(artifact dataRunManifestArtifact) error {
	path := strings.TrimSpace(artifact.Path)
	if path == "" {
		return exit(7, "data manifest artifact path is required")
	}
	if !safeArtifactGlob(path) || strings.ContainsAny(path, "*?") {
		return exit(7, "data manifest artifact path must be a safe relative path: %s", path)
	}
	if artifact.Bytes < 0 {
		return exit(7, "data manifest artifact bytes cannot be negative")
	}
	return nil
}

func validateDataRunSummaryMap(summary map[string]any) error {
	if len(summary) > dataRunMaxSummaryKeys {
		return exit(7, "data manifest summary must contain at most %d keys", dataRunMaxSummaryKeys)
	}
	for key, value := range summary {
		if strings.TrimSpace(key) == "" {
			return exit(7, "data manifest summary keys cannot be empty")
		}
		switch v := value.(type) {
		case nil, bool, float64, string:
			if s, ok := v.(string); ok && len(s) > dataRunMaxStringBytes {
				return exit(7, "data manifest summary %s exceeds %d bytes", key, dataRunMaxStringBytes)
			}
		default:
			return exit(7, "data manifest summary %s must be a scalar", key)
		}
	}
	return nil
}

func scanDataRunManifestSafety(path string, value any) error {
	switch v := value.(type) {
	case map[string]any:
		if len(v) > dataRunMaxCollectionSize {
			return exit(7, "data manifest object %s has too many keys", path)
		}
		for key, child := range v {
			if unsafeDataRunManifestKey(key) {
				return exit(7, "data manifest contains unsafe proof field %s.%s", path, key)
			}
			if err := scanDataRunManifestSafety(path+"."+key, child); err != nil {
				return err
			}
		}
	case []any:
		if len(v) > dataRunMaxCollectionSize {
			return exit(7, "data manifest array %s has too many entries", path)
		}
		for i, child := range v {
			if err := scanDataRunManifestSafety(fmt.Sprintf("%s[%d]", path, i), child); err != nil {
				return err
			}
		}
	case string:
		if len(v) > dataRunMaxStringBytes {
			return exit(7, "data manifest string %s exceeds %d bytes", path, dataRunMaxStringBytes)
		}
	}
	return nil
}

func unsafeDataRunManifestKey(key string) bool {
	normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(strings.TrimSpace(key)))
	for _, token := range []string{
		"password", "passwd", "secret", "credential", "token", "apikey", "accesskey", "privatekey",
		"signedurl", "presignedurl", "rawrow", "rawrows", "rawdata", "rowsample", "samplerows", "sampledata",
	} {
		if strings.Contains(normalized, token) {
			return true
		}
	}
	return false
}

func validateDataRunManifestPath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return exit(2, "data manifest path is required")
	}
	if !safeArtifactGlob(path) || strings.ContainsAny(path, "*?") {
		return exit(2, "data manifest path must be a safe relative file path: %s", path)
	}
	return nil
}

func validateDataRunLocalOutputPath(label, localPath string) error {
	value := strings.TrimSpace(localPath)
	clean := filepath.ToSlash(filepath.Clean(value))
	if clean == "" || clean == "." || filepath.IsAbs(value) || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return exit(2, "%s must be a safe repo-relative path: %s", label, localPath)
	}
	if clean == ".git" || strings.HasPrefix(clean, ".git/") {
		return exit(2, "%s must not write inside .git: %s", label, localPath)
	}
	return nil
}

func validateDataRunPolicy(policy DataRunPolicyConfig) error {
	enforcement := strings.ToLower(strings.TrimSpace(policy.Enforcement))
	if enforcement == "" {
		return nil
	}
	switch enforcement {
	case "declared-only", "unsupported":
		return nil
	default:
		return exit(2, "data run policy enforcement must be declared-only or unsupported; enforced requires provider policy hooks")
	}
}

func validateDataRunManifestPolicy(policy dataRunManifestPolicy, promotion dataRunManifestPromotion) error {
	for _, value := range []string{policy.Enforcement, promotion.Enforcement} {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "", "declared-only", "unsupported":
		default:
			return exit(7, "data manifest enforcement must be declared-only or unsupported")
		}
	}
	return nil
}

func dataRunPolicyDisplay(policy DataRunPolicyConfig) string {
	parts := make([]string, 0, 5)
	if policy.SourceIdentity != "" {
		parts = append(parts, "source="+policy.SourceIdentity)
	}
	if policy.SinkIdentity != "" {
		parts = append(parts, "sink="+policy.SinkIdentity)
	}
	if policy.Egress != "" {
		parts = append(parts, "egress="+policy.Egress)
	}
	if policy.Promotion != "" {
		parts = append(parts, "promotion="+policy.Promotion)
	}
	if policy.Enforcement != "" {
		parts = append(parts, "enforcement="+policy.Enforcement)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

func defaultDataRunManifestOutput(name string) string {
	return filepath.Join(".crabbox", "data-runs", safeCaptureName(name), "manifest.json")
}

func defaultDataRunSummaryOutput(name string) string {
	return filepath.Join(".crabbox", "data-runs", safeCaptureName(name), "summary.json")
}

func writeDataRunSummaryFile(path string, summary DataRunSummary) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return exit(7, "create data summary directory: %v", err)
	}
	file, err := os.Create(path)
	if err != nil {
		return exit(7, "write data summary %s: %v", path, err)
	}
	defer file.Close()
	return writeDataRunSummary(file, summary)
}

func writeDataRunSummary(w io.Writer, summary DataRunSummary) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(summary)
}
