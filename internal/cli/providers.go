package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

type providerMatrixEntry struct {
	Provider    string       `json:"provider"`
	Family      string       `json:"family"`
	Aliases     []string     `json:"aliases,omitempty"`
	Kind        ProviderKind `json:"kind"`
	Category    string       `json:"category,omitempty"`
	Targets     []string     `json:"targets"`
	Features    []Feature    `json:"features"`
	Workspace   []string     `json:"workspace,omitempty"`
	Evidence    []string     `json:"evidence,omitempty"`
	Coordinator string       `json:"coordinator"`
}

type providerRecommendationEntry struct {
	Provider  string       `json:"provider"`
	Kind      ProviderKind `json:"kind"`
	Category  string       `json:"category,omitempty"`
	Targets   []string     `json:"targets"`
	Features  []Feature    `json:"features"`
	Workspace []string     `json:"workspace,omitempty"`
	Evidence  []string     `json:"evidence,omitempty"`
	Score     int          `json:"score"`
	Reasons   []string     `json:"reasons"`
}

type providerFilterValuesEntry struct {
	Kind      []string `json:"kind"`
	Category  []string `json:"category"`
	Target    []string `json:"target"`
	Feature   []string `json:"feature"`
	Workspace []string `json:"workspace"`
	Evidence  []string `json:"evidence"`
}

type providerMatrixFilters struct {
	Kinds      []string
	Categories []string
	Targets    []string
	Features   []string
	Workspaces []string
	Evidence   []string
}

type providerMatrixFilterFlagValues struct {
	Kinds      stringListFlag
	Categories stringListFlag
	Targets    stringListFlag
	Features   stringListFlag
	Workspaces stringListFlag
	Evidence   stringListFlag
}

func (a App) providers(_ context.Context, args []string) error {
	if len(args) > 0 && args[0] == "filters" {
		return a.providerFilters(args[1:])
	}
	if len(args) > 0 && args[0] == "recommend" {
		return a.providerRecommendations(args[1:])
	}
	fs := newFlagSet("providers", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	filterFlags := registerProviderMatrixFilterFlags(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return exit(2, "usage: crabbox providers [--json] [--kind KIND] [--category CATEGORY] [--target TARGET] [--feature FEATURE] [--workspace CAPABILITY] [--evidence CAPABILITY] OR crabbox providers filters [--json] OR crabbox providers recommend <use-case> [--limit N] [--json]")
	}
	entries := providerMatrix()
	filters := filterFlags.filters()
	if err := validateProviderMatrixFilters(filters, entries); err != nil {
		return err
	}
	entries = filterProviderMatrix(entries, filters)
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(entries)
	}
	printProviderMatrix(a.Stdout, entries)
	return nil
}

func (a App) providerFilters(args []string) error {
	fs := newFlagSet("providers filters", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return exit(2, "usage: crabbox providers filters [--json]")
	}
	values := providerMatrixFilterValues(providerMatrix())
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(values)
	}
	printProviderFilterValues(a.Stdout, values)
	return nil
}

func (a App) providerRecommendations(args []string) error {
	positionalUseCase := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") && !isHelpArg(args[0]) {
		positionalUseCase = args[0]
		args = args[1:]
	}
	fs := newFlagSet("providers recommend", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	limit := fs.Int("limit", 5, "maximum recommendations to print")
	useCaseFlag := fs.String("use-case", "", "use case to optimize for")
	filterFlags := registerProviderMatrixFilterFlags(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *limit <= 0 {
		return exit(2, "--limit must be greater than 0")
	}
	useCase := strings.TrimSpace(*useCaseFlag)
	if positionalUseCase != "" {
		if useCase != "" {
			return exit(2, "pass the use case either positionally or with --use-case, not both")
		}
		useCase = strings.TrimSpace(positionalUseCase)
	}
	switch fs.NArg() {
	case 0:
	case 1:
		if useCase != "" {
			return exit(2, "pass the use case either positionally or with --use-case, not both")
		}
		useCase = strings.TrimSpace(fs.Arg(0))
	default:
		return exit(2, "usage: crabbox providers recommend <use-case> [--limit N] [--json]")
	}
	if useCase == "" {
		if !providerMatrixFiltersEmpty(filterFlags.filters()) {
			return exit(2, "provider recommendation filters require a use case")
		}
		if *jsonOut {
			return json.NewEncoder(a.Stdout).Encode(providerRecommendationUseCases())
		}
		printProviderRecommendationUseCases(a.Stdout)
		return nil
	}
	canonical, ok := normalizeProviderRecommendationUseCase(useCase)
	if !ok {
		return exit(2, "unknown provider recommendation use case %q; try one of: %s", useCase, strings.Join(providerRecommendationUseCases(), ", "))
	}
	entries := providerMatrix()
	filters := filterFlags.filters()
	if err := validateProviderMatrixFilters(filters, entries); err != nil {
		return err
	}
	entries = filterProviderMatrix(entries, filters)
	recommendations := recommendProvidersForUseCase(entries, canonical, *limit)
	if len(recommendations) == 0 {
		if providerMatrixFiltersEmpty(filters) {
			return exit(1, "no providers matched use case %q", canonical)
		}
		return exit(1, "no providers matched use case %q with the requested filters", canonical)
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(recommendations)
	}
	printProviderRecommendations(a.Stdout, canonical, recommendations)
	return nil
}

func providerMatrix() []providerMatrixEntry {
	providers := registeredProviders()
	entries := make([]providerMatrixEntry, 0, len(providers))
	for _, provider := range providers {
		spec := provider.Spec()
		entries = append(entries, providerMatrixEntry{
			Provider:    firstNonBlank(spec.Name, provider.Name()),
			Family:      firstNonBlank(spec.Family, provider.Name()),
			Aliases:     append([]string(nil), provider.Aliases()...),
			Kind:        spec.Kind,
			Category:    benchmarkProviderCategories[firstNonBlank(spec.Name, provider.Name())],
			Targets:     formatProviderTargets(spec.Targets),
			Features:    append(FeatureSet{}, spec.Features...),
			Workspace:   workspaceCapabilitiesForFeatures(spec.Features),
			Evidence:    evidenceCapabilitiesForFeatures(spec.Features),
			Coordinator: string(spec.Coordinator),
		})
	}
	return entries
}

func registerProviderMatrixFilterFlags(fs *flag.FlagSet) *providerMatrixFilterFlagValues {
	values := &providerMatrixFilterFlagValues{}
	fs.Var(&values.Kinds, "kind", "filter by provider kind; repeatable")
	fs.Var(&values.Categories, "category", "filter by provider category; repeatable")
	fs.Var(&values.Targets, "target", "filter by target such as linux or worker-runtime; repeatable")
	fs.Var(&values.Features, "feature", "filter by raw feature flag such as ssh or run-proof; repeatable")
	fs.Var(&values.Workspaces, "workspace", "filter by normalized workspace capability; repeatable")
	fs.Var(&values.Evidence, "evidence", "filter by normalized evidence capability; repeatable")
	return values
}

func (values *providerMatrixFilterFlagValues) filters() providerMatrixFilters {
	if values == nil {
		return providerMatrixFilters{}
	}
	return providerMatrixFilters{
		Kinds:      providerFilterValues(values.Kinds),
		Categories: providerFilterValues(values.Categories),
		Targets:    providerFilterValues(values.Targets),
		Features:   providerFilterValues(values.Features),
		Workspaces: providerFilterValues(values.Workspaces),
		Evidence:   providerFilterValues(values.Evidence),
	}
}

func providerFilterValues(values stringListFlag) []string {
	var out []string
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.ToLower(strings.TrimSpace(part))
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func providerMatrixFilterValues(entries []providerMatrixEntry) providerFilterValuesEntry {
	allowed := map[string]map[string]bool{
		"kind":      {},
		"category":  {},
		"target":    {},
		"feature":   {},
		"workspace": {},
		"evidence":  {},
	}
	for _, entry := range entries {
		addProviderFilterAllowed(allowed["kind"], []string{string(entry.Kind)})
		addProviderFilterAllowed(allowed["category"], []string{entry.Category})
		addProviderFilterAllowed(allowed["target"], entry.Targets)
		addProviderFilterAllowed(allowed["feature"], featuresToStrings(entry.Features))
		addProviderFilterAllowed(allowed["workspace"], entry.Workspace)
		addProviderFilterAllowed(allowed["evidence"], entry.Evidence)
	}
	return providerFilterValuesEntry{
		Kind:      sortedProviderFilterValues(allowed["kind"]),
		Category:  sortedProviderFilterValues(allowed["category"]),
		Target:    sortedProviderFilterValues(allowed["target"]),
		Feature:   sortedProviderFilterValues(allowed["feature"]),
		Workspace: sortedProviderFilterValues(allowed["workspace"]),
		Evidence:  sortedProviderFilterValues(allowed["evidence"]),
	}
}

func validateProviderMatrixFilters(filters providerMatrixFilters, entries []providerMatrixEntry) error {
	allowed := providerMatrixFilterAllowedValues(entries)
	check := func(name string, values []string) error {
		for _, value := range values {
			if !allowed[name][value] {
				return exit(2, "unknown provider %s filter %q; try one of: %s", name, value, strings.Join(sortedProviderFilterValues(allowed[name]), ", "))
			}
		}
		return nil
	}
	if err := check("kind", filters.Kinds); err != nil {
		return err
	}
	if err := check("category", filters.Categories); err != nil {
		return err
	}
	if err := check("target", filters.Targets); err != nil {
		return err
	}
	if err := check("feature", filters.Features); err != nil {
		return err
	}
	if err := check("workspace", filters.Workspaces); err != nil {
		return err
	}
	return check("evidence", filters.Evidence)
}

func providerMatrixFilterAllowedValues(entries []providerMatrixEntry) map[string]map[string]bool {
	values := providerMatrixFilterValues(entries)
	return map[string]map[string]bool{
		"kind":      providerFilterAllowedSet(values.Kind),
		"category":  providerFilterAllowedSet(values.Category),
		"target":    providerFilterAllowedSet(values.Target),
		"feature":   providerFilterAllowedSet(values.Feature),
		"workspace": providerFilterAllowedSet(values.Workspace),
		"evidence":  providerFilterAllowedSet(values.Evidence),
	}
}

func providerFilterAllowedSet(values []string) map[string]bool {
	allowed := make(map[string]bool, len(values))
	for _, value := range values {
		allowed[value] = true
	}
	return allowed
}

func addProviderFilterAllowed(allowed map[string]bool, values []string) {
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			allowed[value] = true
		}
	}
}

func sortedProviderFilterValues(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func printProviderFilterValues(out io.Writer, values providerFilterValuesEntry) {
	fmt.Fprintln(out, "provider filter values:")
	fmt.Fprintf(out, "  kind: %s\n", providerFilterValueLine(values.Kind))
	fmt.Fprintf(out, "  category: %s\n", providerFilterValueLine(values.Category))
	fmt.Fprintf(out, "  target: %s\n", providerFilterValueLine(values.Target))
	fmt.Fprintf(out, "  feature: %s\n", providerFilterValueLine(values.Feature))
	fmt.Fprintf(out, "  workspace: %s\n", providerFilterValueLine(values.Workspace))
	fmt.Fprintf(out, "  evidence: %s\n", providerFilterValueLine(values.Evidence))
}

func providerFilterValueLine(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ",")
}

func filterProviderMatrix(entries []providerMatrixEntry, filters providerMatrixFilters) []providerMatrixEntry {
	if providerMatrixFiltersEmpty(filters) {
		return entries
	}
	out := make([]providerMatrixEntry, 0, len(entries))
	for _, entry := range entries {
		if !providerEntryMatchesFilters(entry, filters) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func providerMatrixFiltersEmpty(filters providerMatrixFilters) bool {
	return len(filters.Kinds) == 0 && len(filters.Categories) == 0 && len(filters.Targets) == 0 && len(filters.Features) == 0 && len(filters.Workspaces) == 0 && len(filters.Evidence) == 0
}

func providerEntryMatchesFilters(entry providerMatrixEntry, filters providerMatrixFilters) bool {
	return providerFieldContainsAll([]string{string(entry.Kind)}, filters.Kinds) &&
		providerFieldContainsAll([]string{entry.Category}, filters.Categories) &&
		providerFieldContainsAll(entry.Targets, filters.Targets) &&
		providerFieldContainsAll(featuresToStrings(entry.Features), filters.Features) &&
		providerFieldContainsAll(entry.Workspace, filters.Workspaces) &&
		providerFieldContainsAll(entry.Evidence, filters.Evidence)
}

func providerFieldContainsAll(values, wants []string) bool {
	if len(wants) == 0 {
		return true
	}
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[strings.ToLower(strings.TrimSpace(value))] = true
	}
	for _, want := range wants {
		if !set[want] {
			return false
		}
	}
	return true
}

func providerRecommendationUseCases() []string {
	return []string{
		"agent-sandbox",
		"byo-ssh",
		"ci-proof",
		"desktop",
		"gpu",
		"linux-vm",
		"local",
		"macos",
		"run-evidence",
		"self-hosted",
		"versioned-workspace",
		"windows",
		"worker-runtime",
	}
}

func normalizeProviderRecommendationUseCase(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "agent", "agents", "agent-sandbox", "sandbox", "sandboxes", "devbox", "devboxes":
		return "agent-sandbox", true
	case "byo", "byo-ssh", "ssh", "static", "static-ssh":
		return "byo-ssh", true
	case "ci", "ci-proof", "proof", "testbox", "repro":
		return "ci-proof", true
	case "desktop", "browser", "code", "vnc":
		return "desktop", true
	case "gpu", "cuda", "ml":
		return "gpu", true
	case "linux", "linux-vm", "vm":
		return "linux-vm", true
	case "local", "local-vm", "local-runtime", "local-sandbox":
		return "local", true
	case "mac", "macos", "darwin":
		return "macos", true
	case "run-evidence", "evidence", "artifacts", "artifact", "downloads", "download", "preview", "preview-url", "url-bridge":
		return "run-evidence", true
	case "self-hosted", "selfhosted", "homelab", "virtualization":
		return "self-hosted", true
	case "versioned-workspace", "workspace", "workspaces", "checkpoint", "checkpoints", "snapshot", "snapshots", "fork", "forks":
		return "versioned-workspace", true
	case "windows", "win":
		return "windows", true
	case "worker", "worker-runtime", "module", "module-run":
		return "worker-runtime", true
	default:
		return "", false
	}
}

func recommendProvidersForUseCase(entries []providerMatrixEntry, useCase string, limit int) []providerRecommendationEntry {
	recommendations := make([]providerRecommendationEntry, 0, len(entries))
	for _, entry := range entries {
		score, reasons := scoreProviderRecommendation(entry, useCase)
		if score <= 0 {
			continue
		}
		recommendations = append(recommendations, providerRecommendationEntry{
			Provider:  entry.Provider,
			Kind:      entry.Kind,
			Category:  benchmarkProviderCategories[entry.Provider],
			Targets:   append([]string(nil), entry.Targets...),
			Features:  append([]Feature(nil), entry.Features...),
			Workspace: append([]string(nil), entry.Workspace...),
			Evidence:  append([]string(nil), entry.Evidence...),
			Score:     score,
			Reasons:   reasons,
		})
	}
	sort.SliceStable(recommendations, func(i, j int) bool {
		if recommendations[i].Score != recommendations[j].Score {
			return recommendations[i].Score > recommendations[j].Score
		}
		return recommendations[i].Provider < recommendations[j].Provider
	})
	if len(recommendations) > limit {
		recommendations = recommendations[:limit]
	}
	return recommendations
}

func scoreProviderRecommendation(entry providerMatrixEntry, useCase string) (int, []string) {
	score := 0
	var reasons []string
	add := func(points int, reason string) {
		score += points
		reasons = append(reasons, reason)
	}
	category := benchmarkProviderCategories[entry.Provider]
	hasTarget := func(target string) bool { return providerRecommendationHasString(entry.Targets, target) }
	hasFeature := func(feature Feature) bool { return providerRecommendationHasFeature(entry.Features, feature) }
	switch useCase {
	case "agent-sandbox":
		if category == "delegated-sandbox" {
			add(70, "delegated sandbox provider")
		}
		if hasFeature(FeatureArchiveSync) {
			add(25, "accepts archive sync from the current checkout")
		}
		if hasFeature(FeatureRunSession) {
			add(20, "returns reusable run sessions")
		}
		if hasFeature(FeatureURLBridge) {
			add(12, "can expose provider-native URLs")
		}
		if hasFeature(FeaturePauseResume) {
			add(12, "supports pause and resume")
		}
		if hasFeature(FeatureModuleRun) {
			add(10, "runs source modules in a worker runtime")
		}
		if entry.Kind == ProviderKindSSHLease && hasTarget(targetLinux) && category == "direct-cloud" {
			add(18, "managed Linux devbox with normal Crabbox SSH workflow")
		}
	case "byo-ssh":
		if category == "byo-ssh" {
			add(90, "bring-your-own SSH host")
		}
		if hasFeature(FeatureSSH) {
			add(20, "supports Crabbox-managed SSH access")
		}
		if hasFeature(FeatureCrabboxSync) {
			add(15, "uses Crabbox sync and run")
		}
		if hasFeature(FeatureDesktop) || hasFeature(FeatureBrowser) || hasFeature(FeatureCode) {
			add(10, "can expose interactive lease capabilities when the host supports them")
		}
	case "ci-proof":
		if category == "ci-proof-runner" {
			add(90, "CI proof runner")
		}
		if hasFeature(FeatureRunProof) {
			add(30, "returns provider run proof")
		}
		if hasFeature(FeatureRunSession) && (category == "ci-proof-runner" || hasFeature(FeatureRunProof) || hasFeature(FeatureRunArtifacts) || hasFeature(FeatureRunDownloads)) {
			add(20, "can reuse provider run sessions")
		}
		if hasFeature(FeatureRunArtifacts) || hasFeature(FeatureRunDownloads) {
			add(18, "can collect provider run artifacts or downloads")
		}
		if category == "ci-proof-runner" && entry.Kind == ProviderKindSSHLease && hasFeature(FeatureCrabboxSync) {
			add(12, "can debug through normal Crabbox sync and SSH")
		}
	case "desktop":
		if hasFeature(FeatureDesktop) {
			add(70, "supports visible desktop leases")
		}
		if hasFeature(FeatureBrowser) {
			add(25, "can provision a browser")
		}
		if hasFeature(FeatureCode) {
			add(20, "can provision code-server")
		}
		if hasTarget(targetLinux) {
			add(8, "supports Linux desktop/browser flows")
		}
		if hasTarget(targetMacOS) || hasTarget(targetWindows+"/"+WindowsModeNormal) {
			add(8, "supports native desktop OS targets")
		}
	case "gpu":
		if category == "gpu-cloud" {
			add(90, "GPU-oriented provider")
		}
		if hasTarget(targetLinux) {
			add(10, "supports Linux GPU workloads")
		}
		if hasFeature(FeatureSSH) {
			add(8, "supports SSH debugging")
		}
		if hasFeature(FeatureRunSession) {
			add(8, "supports reusable run sessions")
		}
	case "linux-vm":
		if hasTarget(targetLinux) {
			add(30, "supports Linux")
		}
		if entry.Kind == ProviderKindSSHLease {
			add(35, "normal SSH lease lifecycle")
		}
		if hasFeature(FeatureCrabboxSync) {
			add(25, "uses Crabbox sync")
		}
		if hasFeature(FeatureCleanup) {
			add(15, "can clean up owned resources")
		}
		if category == "brokerable-cloud" {
			add(20, "can use coordinator spend and cleanup controls")
		}
		if category == "direct-cloud" {
			add(14, "direct cloud lease path")
		}
	case "local":
		if strings.HasPrefix(category, "local-") {
			add(80, "local execution provider")
		}
		if hasFeature(FeatureCrabboxSync) || hasFeature(FeatureArchiveSync) {
			add(18, "can sync the current checkout")
		}
		if hasFeature(FeatureCleanup) {
			add(12, "cleans up local runtime state")
		}
	case "macos":
		if hasTarget(targetMacOS) {
			add(80, "supports macOS")
		}
		if hasFeature(FeatureSSH) {
			add(15, "supports SSH access")
		}
		if hasFeature(FeatureDesktop) {
			add(12, "supports desktop access")
		}
		if hasFeature(FeatureSnapshot) || hasFeature(FeatureCheckpoint) || hasFeature(FeatureFork) {
			add(10, "supports provider state reuse")
		}
	case "run-evidence":
		hasEvidenceCapability := hasFeature(FeatureRunProof) || hasFeature(FeatureRunArtifacts) || hasFeature(FeatureRunDownloads) || hasFeature(FeatureURLBridge)
		if !hasEvidenceCapability {
			break
		}
		if hasFeature(FeatureRunProof) {
			add(45, "returns provider run proof")
		}
		if hasFeature(FeatureRunArtifacts) {
			add(35, "can collect provider run artifacts")
		}
		if hasFeature(FeatureRunDownloads) {
			add(30, "can materialize provider run downloads")
		}
		if hasFeature(FeatureURLBridge) {
			add(25, "can expose provider-native preview URLs")
		}
		if hasFeature(FeatureRunSession) {
			add(15, "returns reusable run sessions for later inspection")
		}
	case "self-hosted":
		if category == "self-hosted-virtualization" {
			add(80, "self-hosted virtualization provider")
		}
		if category == "external-provider" {
			add(60, "external provider contract for private infrastructure")
		}
		if category == "byo-ssh" {
			add(50, "bring-your-own host")
		}
		if hasFeature(FeatureSSH) {
			add(15, "supports Crabbox-managed SSH")
		}
		if hasFeature(FeatureCleanup) {
			add(10, "can clean up owned resources")
		}
	case "versioned-workspace":
		hasWorkspaceCapability := hasFeature(FeatureCheckpoint) || hasFeature(FeatureFork) || hasFeature(FeatureRestore) || hasFeature(FeatureSnapshot)
		if !hasWorkspaceCapability {
			break
		}
		if hasFeature(FeatureCheckpoint) {
			add(45, "can create provider-aware checkpoints")
		}
		if hasFeature(FeatureFork) {
			add(45, "can fork a new workspace from checkpoint state")
		}
		if hasFeature(FeatureRestore) {
			add(30, "can restore an existing workspace to checkpoint state")
		}
		if hasFeature(FeatureSnapshot) {
			add(20, "exposes provider-native snapshot identifiers")
		}
		if hasFeature(FeatureCrabboxSync) || hasFeature(FeatureArchiveSync) {
			add(10, "can seed checkpointable state from the current checkout")
		}
		if hasFeature(FeatureCleanup) {
			add(8, "can clean up provider-owned workspace resources")
		}
	case "windows":
		if hasTarget(targetWindows + "/" + WindowsModeNormal) {
			add(80, "supports native Windows")
		}
		if hasTarget(targetWindows + "/" + WindowsModeWSL2) {
			add(45, "supports Windows WSL2")
		}
		if hasFeature(FeatureSSH) {
			add(15, "supports SSH access")
		}
		if hasFeature(FeatureArchiveSync) || hasFeature(FeatureCrabboxSync) {
			add(12, "can sync the current checkout")
		}
	case "worker-runtime":
		if hasTarget(targetWorkerRuntime) {
			add(90, "runs in a worker runtime")
		}
		if hasFeature(FeatureModuleRun) {
			add(35, "supports module source execution")
		}
		if hasFeature(FeatureRunSession) && (hasTarget(targetWorkerRuntime) || hasFeature(FeatureModuleRun)) {
			add(10, "returns reusable run sessions")
		}
	}
	if entry.Kind == ProviderKindServiceControl && useCase != "desktop" {
		score -= 40
		reasons = append(reasons, "service-control provider cannot run arbitrary commands")
	}
	if score <= 0 {
		return 0, nil
	}
	return score, reasons
}

func providerRecommendationHasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func providerRecommendationHasFeature(values []Feature, want Feature) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func printProviderRecommendationUseCases(out io.Writer) {
	fmt.Fprintln(out, "provider recommendation use cases:")
	for _, useCase := range providerRecommendationUseCases() {
		fmt.Fprintf(out, "  %s\n", useCase)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "examples:")
	fmt.Fprintln(out, "  crabbox providers recommend ci-proof")
	fmt.Fprintln(out, "  crabbox providers recommend agent-sandbox --json")
	fmt.Fprintln(out, "  crabbox providers recommend linux-vm --limit 8")
	fmt.Fprintln(out, "  crabbox providers recommend run-evidence")
	fmt.Fprintln(out, "  crabbox providers recommend versioned-workspace")
}

func printProviderRecommendations(out io.Writer, useCase string, entries []providerRecommendationEntry) {
	fmt.Fprintf(out, "recommended providers for %s:\n", useCase)
	for _, entry := range entries {
		fmt.Fprintf(out, "%s\n", entry.Provider)
		fmt.Fprintf(out, "  score: %d\n", entry.Score)
		fmt.Fprintf(out, "  kind: %s\n", entry.Kind)
		fmt.Fprintf(out, "  category: %s\n", blank(entry.Category, "-"))
		fmt.Fprintf(out, "  targets: %s\n", commaOrDash(entry.Targets))
		fmt.Fprintf(out, "  features: %s\n", commaOrDash(featuresToStrings(entry.Features)))
		if len(entry.Workspace) > 0 {
			fmt.Fprintf(out, "  workspace: %s\n", commaOrDash(entry.Workspace))
		}
		if len(entry.Evidence) > 0 {
			fmt.Fprintf(out, "  evidence: %s\n", commaOrDash(entry.Evidence))
		}
		fmt.Fprintf(out, "  reasons: %s\n", strings.Join(entry.Reasons, "; "))
	}
}

func printProviderMatrix(out io.Writer, entries []providerMatrixEntry) {
	for _, entry := range entries {
		fmt.Fprintf(out, "%s\n", entry.Provider)
		fmt.Fprintf(out, "  family: %s\n", entry.Family)
		fmt.Fprintf(out, "  kind: %s\n", entry.Kind)
		fmt.Fprintf(out, "  category: %s\n", blank(entry.Category, "-"))
		fmt.Fprintf(out, "  targets: %s\n", commaOrDash(entry.Targets))
		fmt.Fprintf(out, "  features: %s\n", commaOrDash(featuresToStrings(entry.Features)))
		if len(entry.Workspace) > 0 {
			fmt.Fprintf(out, "  workspace: %s\n", commaOrDash(entry.Workspace))
		}
		if len(entry.Evidence) > 0 {
			fmt.Fprintf(out, "  evidence: %s\n", commaOrDash(entry.Evidence))
		}
		fmt.Fprintf(out, "  coordinator: %s\n", blank(entry.Coordinator, "never"))
		if len(entry.Aliases) > 0 {
			fmt.Fprintf(out, "  aliases: %s\n", strings.Join(entry.Aliases, ","))
		}
	}
}

func formatProviderTargets(targets []TargetSpec) []string {
	out := make([]string, 0, len(targets))
	for _, target := range targets {
		value := strings.TrimSpace(target.OS)
		if value == "" {
			continue
		}
		if strings.TrimSpace(target.WindowsMode) != "" {
			value += "/" + strings.TrimSpace(target.WindowsMode)
		}
		out = append(out, value)
	}
	return out
}

func workspaceCapabilitiesForFeatures(features []Feature) []string {
	var out []string
	add := func(feature Feature, capability string) {
		if FeatureSet(features).Has(feature) {
			out = append(out, capability)
		}
	}
	add(FeatureCheckpoint, "checkpoint")
	add(FeatureFork, "fork")
	add(FeatureRestore, "restore")
	add(FeatureSnapshot, "snapshot-ref")
	return out
}

func evidenceCapabilitiesForFeatures(features []Feature) []string {
	var out []string
	add := func(feature Feature, capability string) {
		if FeatureSet(features).Has(feature) {
			out = append(out, capability)
		}
	}
	add(FeatureRunProof, "proof")
	add(FeatureRunArtifacts, "artifacts")
	add(FeatureRunDownloads, "downloads")
	add(FeatureURLBridge, "preview-url")
	add(FeatureRunSession, "session")
	return out
}

func featuresToStrings(features []Feature) []string {
	out := make([]string, 0, len(features))
	for _, feature := range features {
		out = append(out, string(feature))
	}
	return out
}

func commaOrDash(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ",")
}
