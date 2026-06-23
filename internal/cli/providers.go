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
	Provider     string       `json:"provider"`
	Family       string       `json:"family"`
	Aliases      []string     `json:"aliases,omitempty"`
	Kind         ProviderKind `json:"kind"`
	Category     string       `json:"category,omitempty"`
	Targets      []string     `json:"targets"`
	Features     []Feature    `json:"features"`
	Runtime      []string     `json:"runtime,omitempty"`
	Reachability []string     `json:"reachability,omitempty"`
	Workspace    []string     `json:"workspace,omitempty"`
	Evidence     []string     `json:"evidence,omitempty"`
	Lifecycle    []string     `json:"lifecycle,omitempty"`
	Coordinator  string       `json:"coordinator"`
}

type providerRecommendationEntry struct {
	Provider     string       `json:"provider"`
	Kind         ProviderKind `json:"kind"`
	Category     string       `json:"category,omitempty"`
	Targets      []string     `json:"targets"`
	Features     []Feature    `json:"features"`
	Runtime      []string     `json:"runtime,omitempty"`
	Reachability []string     `json:"reachability,omitempty"`
	Workspace    []string     `json:"workspace,omitempty"`
	Evidence     []string     `json:"evidence,omitempty"`
	Lifecycle    []string     `json:"lifecycle,omitempty"`
	Score        int          `json:"score"`
	Reasons      []string     `json:"reasons"`
}

type providerFilterValuesEntry struct {
	Kind         []string `json:"kind"`
	Category     []string `json:"category"`
	Target       []string `json:"target"`
	Feature      []string `json:"feature"`
	Runtime      []string `json:"runtime"`
	Reachability []string `json:"reachability"`
	Workspace    []string `json:"workspace"`
	Evidence     []string `json:"evidence"`
	Lifecycle    []string `json:"lifecycle"`
}

type providerMatrixFilters struct {
	Kinds        []string
	Categories   []string
	Targets      []string
	Features     []string
	Runtimes     []string
	Reachability []string
	Workspaces   []string
	Evidence     []string
	Lifecycle    []string
}

type providerMatrixFilterFlagValues struct {
	Kinds        stringListFlag
	Categories   stringListFlag
	Targets      stringListFlag
	Features     stringListFlag
	Runtimes     stringListFlag
	Reachability stringListFlag
	Workspaces   stringListFlag
	Evidence     stringListFlag
	Lifecycle    stringListFlag
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
		return exit(2, "usage: crabbox providers [--json] [--kind KIND] [--category CATEGORY] [--target TARGET] [--feature FEATURE] [--runtime CAPABILITY] [--reachability CAPABILITY] [--workspace CAPABILITY] [--evidence CAPABILITY] [--lifecycle CAPABILITY] OR crabbox providers filters [--json] OR crabbox providers recommend <use-case> [--limit N] [--json]")
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
		category := benchmarkProviderCategories[firstNonBlank(spec.Name, provider.Name())]
		targets := formatProviderTargets(spec.Targets)
		entries = append(entries, providerMatrixEntry{
			Provider:     firstNonBlank(spec.Name, provider.Name()),
			Family:       firstNonBlank(spec.Family, provider.Name()),
			Aliases:      append([]string(nil), provider.Aliases()...),
			Kind:         spec.Kind,
			Category:     category,
			Targets:      targets,
			Features:     append(FeatureSet{}, spec.Features...),
			Runtime:      runtimeCapabilitiesForProvider(firstNonBlank(spec.Name, provider.Name()), spec.Kind, category, targets, spec.Features),
			Reachability: reachabilityCapabilitiesForProvider(firstNonBlank(spec.Name, provider.Name())),
			Workspace:    workspaceCapabilitiesForFeatures(spec.Features),
			Evidence:     evidenceCapabilitiesForFeatures(spec.Features),
			Lifecycle:    lifecycleCapabilitiesForProvider(spec.Coordinator, spec.Features),
			Coordinator:  string(spec.Coordinator),
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
	fs.Var(&values.Runtimes, "runtime", "filter by normalized runtime capability; repeatable")
	fs.Var(&values.Reachability, "reachability", "filter by normalized reachability capability; repeatable")
	fs.Var(&values.Workspaces, "workspace", "filter by normalized workspace capability; repeatable")
	fs.Var(&values.Evidence, "evidence", "filter by normalized evidence capability; repeatable")
	fs.Var(&values.Lifecycle, "lifecycle", "filter by normalized lifecycle capability; repeatable")
	return values
}

func (values *providerMatrixFilterFlagValues) filters() providerMatrixFilters {
	if values == nil {
		return providerMatrixFilters{}
	}
	return providerMatrixFilters{
		Kinds:        providerFilterValues(values.Kinds),
		Categories:   providerFilterValues(values.Categories),
		Targets:      providerFilterValues(values.Targets),
		Features:     providerFilterValues(values.Features),
		Runtimes:     providerFilterValues(values.Runtimes),
		Reachability: providerFilterValues(values.Reachability),
		Workspaces:   providerFilterValues(values.Workspaces),
		Evidence:     providerFilterValues(values.Evidence),
		Lifecycle:    providerFilterValues(values.Lifecycle),
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
		"kind":         {},
		"category":     {},
		"target":       {},
		"feature":      {},
		"runtime":      {},
		"reachability": {},
		"workspace":    {},
		"evidence":     {},
		"lifecycle":    {},
	}
	for _, entry := range entries {
		addProviderFilterAllowed(allowed["kind"], []string{string(entry.Kind)})
		addProviderFilterAllowed(allowed["category"], []string{entry.Category})
		addProviderFilterAllowed(allowed["target"], entry.Targets)
		addProviderFilterAllowed(allowed["feature"], featuresToStrings(entry.Features))
		addProviderFilterAllowed(allowed["runtime"], entry.Runtime)
		addProviderFilterAllowed(allowed["reachability"], entry.Reachability)
		addProviderFilterAllowed(allowed["workspace"], entry.Workspace)
		addProviderFilterAllowed(allowed["evidence"], entry.Evidence)
		addProviderFilterAllowed(allowed["lifecycle"], entry.Lifecycle)
	}
	return providerFilterValuesEntry{
		Kind:         sortedProviderFilterValues(allowed["kind"]),
		Category:     sortedProviderFilterValues(allowed["category"]),
		Target:       sortedProviderFilterValues(allowed["target"]),
		Feature:      sortedProviderFilterValues(allowed["feature"]),
		Runtime:      sortedProviderFilterValues(allowed["runtime"]),
		Reachability: sortedProviderFilterValues(allowed["reachability"]),
		Workspace:    sortedProviderFilterValues(allowed["workspace"]),
		Evidence:     sortedProviderFilterValues(allowed["evidence"]),
		Lifecycle:    sortedProviderFilterValues(allowed["lifecycle"]),
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
	if err := check("runtime", filters.Runtimes); err != nil {
		return err
	}
	if err := check("reachability", filters.Reachability); err != nil {
		return err
	}
	if err := check("workspace", filters.Workspaces); err != nil {
		return err
	}
	if err := check("evidence", filters.Evidence); err != nil {
		return err
	}
	return check("lifecycle", filters.Lifecycle)
}

func providerMatrixFilterAllowedValues(entries []providerMatrixEntry) map[string]map[string]bool {
	values := providerMatrixFilterValues(entries)
	return map[string]map[string]bool{
		"kind":         providerFilterAllowedSet(values.Kind),
		"category":     providerFilterAllowedSet(values.Category),
		"target":       providerFilterAllowedSet(values.Target),
		"feature":      providerFilterAllowedSet(values.Feature),
		"runtime":      providerFilterAllowedSet(values.Runtime),
		"reachability": providerFilterAllowedSet(values.Reachability),
		"workspace":    providerFilterAllowedSet(values.Workspace),
		"evidence":     providerFilterAllowedSet(values.Evidence),
		"lifecycle":    providerFilterAllowedSet(values.Lifecycle),
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
	fmt.Fprintf(out, "  runtime: %s\n", providerFilterValueLine(values.Runtime))
	fmt.Fprintf(out, "  reachability: %s\n", providerFilterValueLine(values.Reachability))
	fmt.Fprintf(out, "  workspace: %s\n", providerFilterValueLine(values.Workspace))
	fmt.Fprintf(out, "  evidence: %s\n", providerFilterValueLine(values.Evidence))
	fmt.Fprintf(out, "  lifecycle: %s\n", providerFilterValueLine(values.Lifecycle))
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
	return len(filters.Kinds) == 0 && len(filters.Categories) == 0 && len(filters.Targets) == 0 && len(filters.Features) == 0 && len(filters.Runtimes) == 0 && len(filters.Reachability) == 0 && len(filters.Workspaces) == 0 && len(filters.Evidence) == 0 && len(filters.Lifecycle) == 0
}

func providerEntryMatchesFilters(entry providerMatrixEntry, filters providerMatrixFilters) bool {
	return providerFieldContainsAll([]string{string(entry.Kind)}, filters.Kinds) &&
		providerFieldContainsAll([]string{entry.Category}, filters.Categories) &&
		providerFieldContainsAll(entry.Targets, filters.Targets) &&
		providerFieldContainsAll(featuresToStrings(entry.Features), filters.Features) &&
		providerFieldContainsAll(entry.Runtime, filters.Runtimes) &&
		providerFieldContainsAll(entry.Reachability, filters.Reachability) &&
		providerFieldContainsAll(entry.Workspace, filters.Workspaces) &&
		providerFieldContainsAll(entry.Evidence, filters.Evidence) &&
		providerFieldContainsAll(entry.Lifecycle, filters.Lifecycle)
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
		"artifact-download",
		"byo-ssh",
		"ci-proof",
		"code-interpreter",
		"cost-control",
		"desktop",
		"disposable-execution",
		"fast-feedback",
		"failure-diagnostics",
		"fanout-testing",
		"gpu",
		"interactive-debug",
		"isolated-execution",
		"linux-vm",
		"live-smoke",
		"local",
		"macos",
		"mcp-sandbox",
		"network-isolation",
		"offline-validation",
		"pause-resume",
		"preview-url",
		"reachability",
		"remote-dev",
		"resource-observability",
		"run-evidence",
		"run-session",
		"self-hosted",
		"team-cloud",
		"versioned-workspace",
		"warm-start",
		"web-app-smoke",
		"windows",
		"worker-runtime",
	}
}

func normalizeProviderRecommendationUseCase(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "agent", "agents", "agent-sandbox", "sandbox", "sandboxes", "devbox", "devboxes":
		return "agent-sandbox", true
	case "artifact-download", "artifact-downloads", "downloadable-artifacts",
		"run-artifacts", "run-downloads", "evidence-downloads":
		return "artifact-download", true
	case "byo", "byo-ssh", "ssh", "static", "static-ssh":
		return "byo-ssh", true
	case "ci", "ci-proof", "proof", "testbox", "repro":
		return "ci-proof", true
	case "code-interpreter", "code-interpreters", "code-execution",
		"ai-code", "ai-code-runner", "generated-code", "generated-code-runner",
		"python-sandbox", "script-runner", "notebook-sandbox":
		return "code-interpreter", true
	case "cost", "cost-control", "cost-aware", "budget", "budget-control",
		"quota", "quota-safe", "spend", "spend-control":
		return "cost-control", true
	case "desktop", "browser", "code", "vnc":
		return "desktop", true
	case "disposable-execution", "disposable", "ephemeral-execution",
		"ephemeral-sandbox", "ephemeral-sandboxes", "throwaway",
		"throwaway-sandbox", "throwaway-sandboxes", "auto-cleanup",
		"clean-sandbox", "cleanup-sandbox", "temporary-sandbox":
		return "disposable-execution", true
	case "fast-feedback", "feedback", "fast-test", "fast-tests", "cache", "cached", "cache-heavy":
		return "fast-feedback", true
	case "failure-diagnostics", "failure-diagnostic", "failed-run", "failed-runs",
		"failure-triage", "run-debugging", "debuggable-run", "debuggable-runs",
		"debuggability", "postmortem", "post-mortem":
		return "failure-diagnostics", true
	case "fanout", "fanout-testing", "best-of-n", "best-of-n-testing",
		"parallel-testing", "parallel-exploration", "branch-race",
		"snapshot-fanout", "fork-fanout":
		return "fanout-testing", true
	case "gpu", "cuda", "ml":
		return "gpu", true
	case "interactive-debug", "interactive-debugging", "live-debug",
		"live-debugging", "debug-session", "debug-sessions",
		"ssh-debug", "browser-debug", "code-debug", "inspectable-debug":
		return "interactive-debug", true
	case "isolated", "isolated-execution", "isolation", "secure", "secure-sandbox", "untrusted", "untrusted-code":
		return "isolated-execution", true
	case "linux", "linux-vm", "vm":
		return "linux-vm", true
	case "live-smoke", "live-smokes", "provider-smoke", "provider-smokes",
		"smoke", "smokes", "smoke-test", "smoke-tests",
		"live-validation", "live-validate", "live-proof":
		return "live-smoke", true
	case "local", "local-vm", "local-runtime", "local-sandbox":
		return "local", true
	case "mac", "macos", "darwin":
		return "macos", true
	case "mcp", "mcp-sandbox", "mcp-attachments", "tool-sandbox", "tool-sandboxes":
		return "mcp-sandbox", true
	case "network-isolation", "network-isolated", "network-containment",
		"egress-control", "egress-controlled", "contained-execution",
		"contained-sandbox", "contained-sandboxes":
		return "network-isolation", true
	case "offline", "offline-validation", "offline-smoke", "no-credentials",
		"no-provider-credentials", "credentialless", "without-credentials",
		"local-first", "local-validation":
		return "offline-validation", true
	case "pause-resume", "pause", "resume", "suspend", "suspended",
		"pausable", "pausable-workspace", "pausable-workspaces",
		"resumable", "resumable-workspace", "resumable-workspaces":
		return "pause-resume", true
	case "preview", "preview-url", "preview-urls", "url-bridge", "app-preview", "app-previews", "web-preview", "web-previews":
		return "preview-url", true
	case "reachability", "reachable", "network", "networking", "ports", "port", "pond":
		return "reachability", true
	case "remote-dev", "remote-development",
		"dev-environment", "dev-environments",
		"cloud-dev", "cloud-development", "cde",
		"codespace", "codespaces", "remote-workspace":
		return "remote-dev", true
	case "resource-observability", "resource-observe", "observability",
		"resource-telemetry", "telemetry", "usage-observability",
		"usage-visibility", "usage-metadata", "metering",
		"cost-observability", "cost-visibility", "billing-visibility":
		return "resource-observability", true
	case "run-evidence", "evidence", "artifacts", "artifact", "downloads", "download":
		return "run-evidence", true
	case "run-session", "run-sessions", "session", "sessions",
		"inspectable-run", "inspectable-runs", "reusable-run", "reusable-runs",
		"session-inspection":
		return "run-session", true
	case "self-hosted", "selfhosted", "homelab", "virtualization":
		return "self-hosted", true
	case "team", "team-cloud", "shared-cloud", "brokered-cloud", "coordinator", "coordinated-cloud", "managed-cloud":
		return "team-cloud", true
	case "versioned-workspace", "workspace", "workspaces",
		"workspace-reuse", "reusable-workspace", "reusable-workspaces",
		"checkpoint", "checkpoints", "snapshot", "snapshots",
		"fork", "forks", "forkable", "forkable-workspace",
		"forkable-workspaces", "durable-workspace", "stateful-workspace",
		"snapshot-fork":
		return "versioned-workspace", true
	case "warm-start", "warm", "warmup", "warm-up", "warm-pool", "warm-pools",
		"prewarm", "pre-warm", "prewarmed", "pre-warmed", "low-latency-start",
		"low-latency", "reuse-state", "reuse-runtime":
		return "warm-start", true
	case "web-app-smoke", "web-smoke", "app-smoke", "service-smoke",
		"preview-smoke", "browser-smoke", "url-smoke", "http-smoke",
		"web-preview-smoke":
		return "web-app-smoke", true
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
			Provider:     entry.Provider,
			Kind:         entry.Kind,
			Category:     benchmarkProviderCategories[entry.Provider],
			Targets:      append([]string(nil), entry.Targets...),
			Features:     append([]Feature(nil), entry.Features...),
			Runtime:      append([]string(nil), entry.Runtime...),
			Reachability: append([]string(nil), entry.Reachability...),
			Workspace:    append([]string(nil), entry.Workspace...),
			Evidence:     append([]string(nil), entry.Evidence...),
			Lifecycle:    append([]string(nil), entry.Lifecycle...),
			Score:        score,
			Reasons:      reasons,
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
	capabilities := providerCapabilities(entry.Provider)
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
		if hasFeature(FeatureMCP) {
			add(8, "can attach MCP servers at sandbox creation")
		}
		if entry.Kind == ProviderKindSSHLease && hasTarget(targetLinux) && category == "direct-cloud" {
			add(18, "managed Linux devbox with normal Crabbox SSH workflow")
		}
	case "artifact-download":
		if !hasFeature(FeatureRunArtifacts) && !hasFeature(FeatureRunDownloads) {
			break
		}
		if hasFeature(FeatureRunArtifacts) {
			add(80, "can collect provider run artifacts")
		}
		if hasFeature(FeatureRunDownloads) {
			add(75, "can materialize provider run downloads")
		}
		if hasFeature(FeatureRunProof) {
			add(22, "can pair downloads with provider run proof")
		}
		if hasFeature(FeatureRunSession) {
			add(18, "returns reusable sessions for later artifact inspection")
		}
		if hasFeature(FeatureURLBridge) {
			add(14, "can pair artifacts with provider-native preview URLs")
		}
		if entry.Kind == ProviderKindDelegatedRun {
			add(12, "provider owns artifact-producing command execution")
		}
		if category == "ci-proof-runner" {
			add(12, "CI proof runner is designed to retain validation outputs")
		}
		if hasTarget(targetLinux) {
			add(8, "supports common Linux artifact workloads")
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
	case "code-interpreter":
		isSandboxRuntime := category == "delegated-sandbox" || category == "local-sandbox"
		hasExecutionSignal := hasFeature(FeatureArchiveSync) || hasFeature(FeatureRunSession) ||
			hasFeature(FeatureRunDownloads) || hasFeature(FeatureRunArtifacts) ||
			hasFeature(FeatureURLBridge) || hasFeature(FeatureMCP) ||
			hasFeature(FeatureModuleRun)
		if !isSandboxRuntime || !hasExecutionSignal {
			break
		}
		if category == "delegated-sandbox" {
			add(50, "delegated sandbox for provider-owned code execution")
		}
		if category == "local-sandbox" {
			add(40, "local sandbox for credentialless code execution")
		}
		if hasFeature(FeatureRunSession) {
			add(30, "returns reusable interpreter sessions")
		}
		if hasFeature(FeatureArchiveSync) {
			add(24, "can seed generated-code workloads from an archive")
		}
		if hasFeature(FeatureRunDownloads) || hasFeature(FeatureRunArtifacts) {
			add(22, "can retain generated outputs")
		}
		if hasFeature(FeatureURLBridge) {
			add(16, "can expose generated app or notebook previews")
		}
		if hasFeature(FeatureMCP) {
			add(14, "can attach MCP tools to the sandbox")
		}
		if hasFeature(FeatureModuleRun) {
			add(12, "can execute module source directly")
		}
		if hasFeature(FeatureCleanup) {
			add(10, "can clean up disposable interpreter resources")
		}
		if hasTarget(targetLinux) || hasTarget(targetWorkerRuntime) {
			add(8, "supports common interpreter execution targets")
		}
	case "disposable-execution":
		isSandboxRuntime := category == "delegated-sandbox" || category == "local-sandbox"
		if !isSandboxRuntime || !hasFeature(FeatureCleanup) {
			break
		}
		if category == "delegated-sandbox" {
			add(55, "delegated sandbox for provider-owned disposable execution")
		}
		if category == "local-sandbox" {
			add(45, "local sandbox for credentialless disposable execution")
		}
		add(45, "can clean up disposable sandbox resources")
		if hasFeature(FeatureArchiveSync) {
			add(24, "can seed a temporary workload from the current checkout")
		}
		if hasFeature(FeatureRunSession) {
			add(18, "returns a run session before cleanup")
		}
		if hasFeature(FeatureRunArtifacts) || hasFeature(FeatureRunDownloads) {
			add(16, "can retain outputs before deleting runtime resources")
		}
		if hasFeature(FeatureURLBridge) {
			add(12, "can expose temporary app previews")
		}
		if hasFeature(FeaturePauseResume) {
			add(8, "can pause disposable state before release when needed")
		}
		if hasTarget(targetLinux) || hasTarget(targetWorkerRuntime) {
			add(8, "supports common disposable execution targets")
		}
	case "cost-control":
		if strings.HasPrefix(category, "local-") {
			add(75, "local runtime avoids provider spend and cloud quota")
		}
		if entry.Coordinator == string(CoordinatorSupported) {
			add(45, "can use coordinator spend and cleanup controls")
		}
		if category == "ci-proof-runner" {
			add(35, "CI proof runner is designed for bounded validation work")
		}
		if hasFeature(FeatureCleanup) {
			add(25, "can clean up owned runtime resources")
		}
		if hasFeature(FeatureCacheVolume) {
			add(20, "can reuse dependency and build caches")
		}
		if hasFeature(FeatureCrabboxSync) || hasFeature(FeatureArchiveSync) {
			add(15, "can sync only the current workload")
		}
		if hasFeature(FeaturePauseResume) {
			add(12, "can pause runtime state instead of keeping it hot")
		}
		if hasFeature(FeatureCheckpoint) || hasFeature(FeatureFork) || hasFeature(FeatureRestore) || hasFeature(FeatureSnapshot) {
			add(10, "can reuse workspace state across runs")
		}
		if hasFeature(FeatureRunProof) || hasFeature(FeatureRunArtifacts) || hasFeature(FeatureRunDownloads) {
			add(10, "can retain proof without keeping capacity alive")
		}
		if hasTarget(targetLinux) {
			add(5, "supports common Linux validation workloads")
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
	case "fast-feedback":
		if hasFeature(FeatureCacheVolume) {
			add(45, "can reuse dependency/cache volumes across runs")
		}
		if hasFeature(FeatureCrabboxSync) || hasFeature(FeatureArchiveSync) {
			add(25, "can sync the current checkout")
		}
		if strings.HasPrefix(category, "local-") {
			add(20, "local runtime avoids cloud credentials and queueing")
		}
		if category == "ci-proof-runner" {
			add(18, "CI-shaped runner for repeated validation loops")
		}
		if hasFeature(FeatureRunProof) || hasFeature(FeatureRunArtifacts) || hasFeature(FeatureRunDownloads) {
			add(15, "can preserve validation evidence")
		}
		if hasFeature(FeatureRunSession) {
			add(10, "returns reusable run sessions")
		}
		if hasFeature(FeatureCleanup) {
			add(10, "can clean up owned runtime state")
		}
		if hasTarget(targetLinux) {
			add(8, "supports common Linux test workloads")
		}
	case "failure-diagnostics":
		hasDiagnostics := hasFeature(FeatureRunProof) || hasFeature(FeatureRunSession) ||
			hasFeature(FeatureRunArtifacts) || hasFeature(FeatureRunDownloads) ||
			hasFeature(FeatureURLBridge) ||
			(hasFeature(FeatureSSH) && hasFeature(FeatureCrabboxSync))
		if !hasDiagnostics {
			break
		}
		if hasFeature(FeatureRunProof) {
			add(45, "returns provider run proof")
		}
		if hasFeature(FeatureRunSession) {
			add(35, "keeps inspectable run or session handles")
		}
		if hasFeature(FeatureRunArtifacts) {
			add(30, "can retain failure artifacts")
		}
		if hasFeature(FeatureRunDownloads) {
			add(25, "can materialize diagnostic downloads")
		}
		if hasFeature(FeatureURLBridge) {
			add(22, "can expose provider-native preview URLs")
		}
		if category == "ci-proof-runner" {
			add(20, "CI proof runner is optimized for reproducible failure evidence")
		}
		if hasFeature(FeatureSSH) && hasFeature(FeatureCrabboxSync) {
			add(16, "supports SSH debugging against a synced checkout")
		}
		if entry.Kind == ProviderKindDelegatedRun {
			add(10, "provider owns command execution and inspection")
		}
		if hasFeature(FeaturePauseResume) {
			add(8, "can preserve runtime state during failure triage")
		}
		if hasFeature(FeatureCleanup) {
			add(8, "can clean up diagnostic resources")
		}
		if hasTarget(targetLinux) || hasTarget(targetWorkerRuntime) {
			add(6, "supports common diagnostic run targets")
		}
	case "interactive-debug":
		hasDebugSurface := (hasFeature(FeatureSSH) && hasFeature(FeatureCrabboxSync)) ||
			hasFeature(FeatureDesktop) || hasFeature(FeatureBrowser) ||
			hasFeature(FeatureCode) || hasFeature(FeatureRunSession) ||
			hasFeature(FeatureURLBridge)
		if !hasDebugSurface {
			break
		}
		if hasFeature(FeatureSSH) && hasFeature(FeatureCrabboxSync) {
			add(45, "can debug a synced checkout through SSH")
		}
		if hasFeature(FeatureBrowser) {
			add(35, "can inspect browser-visible behavior")
		}
		if hasFeature(FeatureCode) {
			add(30, "can inspect and edit through code-server access")
		}
		if hasFeature(FeatureDesktop) {
			add(24, "can inspect interactive desktop state")
		}
		if hasFeature(FeatureRunSession) {
			add(26, "returns reusable sessions for interactive inspection")
		}
		if hasFeature(FeatureURLBridge) {
			add(20, "can expose provider-native URLs during debugging")
		}
		if capabilities.Tailscale || capabilities.SSHMesh {
			add(14, "has a routable operator access plane")
		}
		if hasFeature(FeatureRunArtifacts) || hasFeature(FeatureRunDownloads) || hasFeature(FeatureRunProof) {
			add(12, "can retain evidence after the debug session")
		}
		if hasFeature(FeaturePauseResume) {
			add(10, "can preserve runtime state while debugging")
		}
		if hasFeature(FeatureCleanup) {
			add(8, "can clean up debug resources")
		}
		if hasTarget(targetLinux) || hasTarget(targetWindows) || hasTarget(targetMacOS) || hasTarget(targetWorkerRuntime) {
			add(6, "supports common interactive debug targets")
		}
	case "fanout-testing":
		if !hasFeature(FeatureFork) {
			break
		}
		add(90, "can fork workspaces for parallel exploration")
		if hasFeature(FeatureCheckpoint) {
			add(35, "can checkpoint a prepared baseline")
		}
		if hasFeature(FeatureRestore) {
			add(25, "can restore workspace state after experiments")
		}
		if hasFeature(FeatureSnapshot) {
			add(18, "exposes provider-native snapshot identifiers")
		}
		if providerRecommendationHasString(entry.Runtime, "local-runtime") {
			add(20, "local runtime avoids cloud quota for fanout")
		}
		if hasFeature(FeatureCleanup) {
			add(18, "can clean up forked workspace resources")
		}
		if hasFeature(FeatureCrabboxSync) || hasFeature(FeatureArchiveSync) {
			add(16, "can seed fanout from the current checkout")
		}
		if hasFeature(FeatureCacheVolume) {
			add(12, "can reuse dependency/cache state across branches")
		}
		if hasFeature(FeatureRunProof) || hasFeature(FeatureRunArtifacts) || hasFeature(FeatureRunDownloads) {
			add(10, "can retain evidence from competing runs")
		}
		if hasTarget(targetLinux) || hasTarget(targetMacOS) {
			add(8, "supports common fanout test targets")
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
	case "isolated-execution":
		if category == "delegated-sandbox" {
			add(55, "delegated sandbox provider")
		}
		if category == "local-sandbox" {
			add(45, "local policy-constrained sandbox")
		}
		if entry.Kind == ProviderKindDelegatedRun {
			add(30, "provider owns command execution boundary")
		}
		if hasFeature(FeatureArchiveSync) {
			add(15, "accepts bounded archive sync instead of a long-lived SSH lease")
		}
		if hasFeature(FeatureCleanup) {
			add(12, "can clean up provider-owned sandbox state")
		}
		if hasTarget(targetLinux) {
			add(8, "supports common Linux sandbox workloads")
		}
		if hasFeature(FeatureRunSession) {
			add(6, "returns sandbox run sessions for inspection")
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
	case "live-smoke":
		if hasFeature(FeatureCleanup) {
			add(30, "can clean up provider-owned smoke resources")
		}
		if hasFeature(FeatureCrabboxSync) || hasFeature(FeatureArchiveSync) {
			add(24, "can sync a smoke workload")
		}
		if entry.Kind == ProviderKindSSHLease && hasFeature(FeatureSSH) {
			add(22, "normal SSH lifecycle can run generic smoke commands")
		}
		if entry.Kind == ProviderKindDelegatedRun && (hasFeature(FeatureRunSession) || hasFeature(FeatureArchiveSync) || hasFeature(FeatureRunProof)) {
			add(18, "delegated run lifecycle can execute smoke commands")
		}
		if hasFeature(FeatureRunProof) {
			add(35, "returns provider smoke proof")
		}
		if hasFeature(FeatureRunArtifacts) || hasFeature(FeatureRunDownloads) {
			add(24, "can preserve smoke artifacts or downloads")
		}
		if hasFeature(FeatureURLBridge) {
			add(20, "can expose smoke preview URLs")
		}
		if hasFeature(FeatureRunSession) {
			add(14, "returns reusable smoke sessions")
		}
		if category == "local-runtime" {
			add(32, "local runtime smoke does not need cloud credentials")
		}
		if strings.HasPrefix(category, "local-") {
			add(14, "can smoke without cloud credentials")
		}
		if category == "ci-proof-runner" {
			add(25, "CI proof runner is designed for validation smoke")
		}
		if category == "brokerable-cloud" || category == "direct-cloud" || category == "delegated-sandbox" || category == "ci-proof-runner" {
			add(12, "provider class is useful for opt-in live validation")
		}
		if hasTarget(targetLinux) {
			add(8, "supports common Linux smoke workloads")
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
	case "mcp-sandbox":
		if !hasFeature(FeatureMCP) {
			break
		}
		add(80, "can attach MCP servers at sandbox creation")
		if entry.Kind == ProviderKindDelegatedRun {
			add(25, "provider owns sandbox command execution")
		}
		if category == "local-sandbox" {
			add(18, "local sandbox avoids cloud credentials")
		}
		if hasFeature(FeatureRunSession) {
			add(14, "returns reusable run sessions")
		}
		if hasTarget(targetLinux) {
			add(8, "supports common Linux MCP workloads")
		}
	case "network-isolation":
		if category != "delegated-sandbox" && category != "local-sandbox" {
			break
		}
		if category == "delegated-sandbox" {
			add(65, "delegated sandbox boundary for untrusted execution")
		}
		if category == "local-sandbox" {
			add(55, "local policy-constrained sandbox boundary")
		}
		if entry.Kind == ProviderKindDelegatedRun {
			add(28, "provider owns the command execution boundary")
		}
		if hasFeature(FeatureArchiveSync) {
			add(20, "accepts bounded archive sync instead of a long-lived SSH lease")
		}
		if hasFeature(FeatureCleanup) {
			add(18, "can clean up provider-owned sandbox state")
		}
		if capabilities.TailscaleEgress {
			add(12, "uses outbound-only tailnet egress")
		}
		if hasFeature(FeatureRunSession) {
			add(10, "returns sandbox sessions for later inspection")
		}
		if hasFeature(FeatureURLBridge) {
			add(8, "can expose provider-native URLs without SSH tunneling")
		}
		if hasFeature(FeatureMCP) {
			add(8, "can attach MCP servers within the sandbox boundary")
		}
		if hasTarget(targetLinux) {
			add(8, "supports common Linux sandbox workloads")
		}
	case "offline-validation":
		offlineCandidate := true
		switch {
		case category == "local-runtime":
			add(95, "local runtime can validate without provider credentials")
		case category == "local-sandbox":
			add(90, "local sandbox can run disposable validation without provider credentials")
		case category == "local-vm":
			add(82, "local VM can validate without cloud credentials")
		case category == "byo-ssh":
			add(55, "bring-your-own SSH host avoids provider API credentials")
		case category == "external-provider":
			add(35, "external provider can wrap private infrastructure")
		default:
			offlineCandidate = false
		}
		if !offlineCandidate {
			break
		}
		if strings.HasPrefix(category, "local-") && hasFeature(FeatureCacheVolume) {
			add(16, "can reuse local dependency/cache state")
		}
		if hasFeature(FeatureCrabboxSync) || hasFeature(FeatureArchiveSync) {
			add(16, "can sync the current checkout")
		}
		if hasFeature(FeatureCleanup) {
			add(14, "can clean up local validation resources")
		}
		if hasFeature(FeatureMCP) {
			add(10, "can exercise MCP-attached sandbox flows locally")
		}
		if hasFeature(FeatureCheckpoint) || hasFeature(FeatureFork) || hasFeature(FeatureRestore) || hasFeature(FeatureSnapshot) {
			add(8, "can reuse local workspace state across validation runs")
		}
		if hasTarget(targetLinux) {
			add(8, "supports common Linux validation workloads")
		}
	case "pause-resume":
		if !hasFeature(FeaturePauseResume) {
			break
		}
		add(80, "supports pausing and resuming provider-owned runtime state")
		if entry.Kind == ProviderKindDelegatedRun {
			add(25, "provider owns resumable sandbox execution")
		}
		if hasFeature(FeatureArchiveSync) || hasFeature(FeatureCrabboxSync) {
			add(18, "can seed resumable state from the current checkout")
		}
		if hasFeature(FeatureCleanup) {
			add(16, "can clean up paused runtime resources")
		}
		if hasFeature(FeatureRunSession) {
			add(14, "returns sessions for paused runtime inspection")
		}
		if hasFeature(FeatureURLBridge) {
			add(12, "can expose preview URLs before or after resume")
		}
		if hasFeature(FeatureRunDownloads) || hasFeature(FeatureRunArtifacts) {
			add(10, "can preserve outputs from resumable runs")
		}
		if hasTarget(targetLinux) {
			add(8, "supports common Linux resumable workloads")
		}
	case "preview-url":
		if !hasFeature(FeatureURLBridge) {
			break
		}
		add(80, "can expose provider-native preview URLs")
		if entry.Kind == ProviderKindDelegatedRun {
			add(30, "provider owns preview run execution")
		}
		if hasFeature(FeatureRunSession) {
			add(20, "returns reusable preview sessions")
		}
		if hasFeature(FeatureRunDownloads) || hasFeature(FeatureRunArtifacts) {
			add(14, "can pair previews with downloadable evidence")
		}
		if hasFeature(FeatureArchiveSync) || hasFeature(FeatureCrabboxSync) {
			add(12, "can sync a preview workload")
		}
		if hasFeature(FeaturePauseResume) {
			add(8, "supports pausing preview sandboxes")
		}
		if hasFeature(FeatureCleanup) {
			add(8, "can clean up preview resources")
		}
		if hasTarget(targetLinux) {
			add(8, "supports common Linux preview workloads")
		}
	case "reachability":
		if capabilities.Tailscale {
			add(45, "can join a tailnet as a bidirectional peer")
		}
		if capabilities.URLBridge {
			add(55, "can expose provider-native HTTPS endpoints")
		}
		if capabilities.SSHMesh {
			add(20, "can build operator-side SSH tunnels")
		}
		if capabilities.TailscaleEgress {
			add(10, "can use outbound-only tailnet egress")
		}
		if hasFeature(FeatureBrowser) || hasFeature(FeatureCode) {
			add(8, "can pair reachability with interactive browser or code surfaces")
		}
	case "remote-dev":
		if isRemoteDevProvider(entry.Provider) {
			add(65, "managed developer environment provider")
		}
		if entry.Kind == ProviderKindSSHLease && hasFeature(FeatureSSH) {
			add(28, "normal SSH developer access")
		}
		if hasFeature(FeatureCrabboxSync) || hasFeature(FeatureArchiveSync) {
			add(24, "can sync the current checkout")
		}
		if category == "direct-cloud" {
			add(20, "provider-managed remote development capacity")
		}
		if category == "delegated-sandbox" && hasFeature(FeatureArchiveSync) {
			add(16, "provider-owned workspace with archive sync")
		}
		if category == "brokerable-cloud" {
			add(12, "can use coordinator spend and cleanup controls")
		}
		if category == "self-hosted-virtualization" || category == "local-vm" || category == "byo-ssh" {
			add(10, "works as a private development environment target")
		}
		if hasFeature(FeaturePauseResume) {
			add(20, "supports pause and resume")
		}
		if hasFeature(FeatureCleanup) {
			add(12, "can clean up owned workspace resources")
		}
		if hasFeature(FeatureBrowser) || hasFeature(FeatureCode) || hasFeature(FeatureDesktop) {
			add(8, "can expose interactive development surfaces")
		}
		if hasTarget(targetLinux) {
			add(8, "supports common Linux development workloads")
		}
	case "resource-observability":
		hasTelemetrySignal := entry.Coordinator == string(CoordinatorSupported) ||
			(hasFeature(FeatureSSH) && hasFeature(FeatureCrabboxSync)) ||
			hasFeature(FeatureRunProof) || hasFeature(FeatureRunArtifacts) ||
			hasFeature(FeatureRunDownloads) || hasFeature(FeatureRunSession) ||
			hasFeature(FeatureURLBridge)
		if !hasTelemetrySignal {
			break
		}
		if entry.Coordinator == string(CoordinatorSupported) {
			add(45, "coordinator can centralize lease usage and cost visibility")
		}
		if hasFeature(FeatureSSH) && hasFeature(FeatureCrabboxSync) {
			add(35, "SSH sync providers can report host resource telemetry during runs")
		}
		if hasFeature(FeatureRunProof) {
			add(28, "retains proof suitable for run-level audit trails")
		}
		if hasFeature(FeatureRunArtifacts) || hasFeature(FeatureRunDownloads) {
			add(24, "retains artifacts or downloads for later inspection")
		}
		if hasFeature(FeatureRunSession) {
			add(20, "returns reusable sessions for post-run inspection")
		}
		if hasFeature(FeatureURLBridge) {
			add(12, "can expose preview URLs alongside run records")
		}
		if hasFeature(FeatureCleanup) {
			add(10, "can close the resource lifecycle after observation")
		}
		if strings.HasPrefix(category, "local-") {
			add(8, "local runtime makes resource inspection credentialless")
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
	case "run-session":
		if !hasFeature(FeatureRunSession) {
			break
		}
		add(80, "returns reusable run sessions for later inspection")
		if entry.Kind == ProviderKindDelegatedRun {
			add(25, "provider owns inspectable command execution")
		}
		if hasFeature(FeatureRunProof) {
			add(22, "can pair sessions with provider run proof")
		}
		if hasFeature(FeatureRunArtifacts) || hasFeature(FeatureRunDownloads) {
			add(18, "can pair sessions with retained run outputs")
		}
		if hasFeature(FeatureURLBridge) {
			add(16, "can pair sessions with provider-native preview URLs")
		}
		if hasFeature(FeatureArchiveSync) || hasFeature(FeatureCrabboxSync) {
			add(12, "can sync an inspectable run workload")
		}
		if hasFeature(FeatureMCP) {
			add(10, "can inspect MCP-attached sandbox sessions")
		}
		if hasFeature(FeatureCleanup) {
			add(8, "can clean up session-owned resources")
		}
		if hasTarget(targetLinux) || hasTarget(targetWorkerRuntime) {
			add(8, "supports common inspectable run targets")
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
	case "team-cloud":
		if category == "brokerable-cloud" {
			add(80, "brokerable cloud provider for shared coordinator control")
		}
		if entry.Coordinator == string(CoordinatorSupported) {
			add(35, "can use Crabbox coordinator spend and cleanup controls")
		}
		if category == "direct-cloud" {
			add(25, "direct cloud provider with owned-account lifecycle")
		}
		if hasFeature(FeatureCleanup) {
			add(20, "can clean up owned cloud resources")
		}
		if hasFeature(FeatureCrabboxSync) {
			add(15, "uses normal Crabbox sync and run")
		}
		if hasFeature(FeatureSSH) {
			add(10, "supports SSH debugging")
		}
		if hasTarget(targetLinux) {
			add(8, "supports common Linux team workloads")
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
	case "warm-start":
		hasWarmSignal := hasFeature(FeatureCacheVolume) || hasFeature(FeatureRunSession) ||
			hasFeature(FeaturePauseResume) || hasFeature(FeatureCheckpoint) ||
			hasFeature(FeatureFork) || hasFeature(FeatureRestore) ||
			hasFeature(FeatureSnapshot) || strings.HasPrefix(category, "local-")
		if !hasWarmSignal {
			break
		}
		if strings.HasPrefix(category, "local-") {
			add(45, "local runtime avoids cloud cold starts")
		}
		if hasFeature(FeatureCacheVolume) {
			add(35, "can reuse dependency/cache volumes")
		}
		if hasFeature(FeatureRunSession) {
			add(30, "can reuse retained run sessions")
		}
		if hasFeature(FeaturePauseResume) {
			add(28, "can pause and resume warm runtime state")
		}
		if hasFeature(FeatureCheckpoint) || hasFeature(FeatureFork) || hasFeature(FeatureRestore) || hasFeature(FeatureSnapshot) {
			add(25, "can reuse prepared workspace state")
		}
		if hasFeature(FeatureCrabboxSync) || hasFeature(FeatureArchiveSync) {
			add(16, "can seed warm state from the current checkout")
		}
		if category == "ci-proof-runner" {
			add(14, "CI proof runner is optimized for repeated validation loops")
		}
		if hasFeature(FeatureCleanup) {
			add(12, "can clean up warm-start resources")
		}
		if hasFeature(FeatureRunProof) || hasFeature(FeatureRunArtifacts) || hasFeature(FeatureRunDownloads) {
			add(8, "can retain evidence without keeping every run hot")
		}
		if hasTarget(targetLinux) || hasTarget(targetWorkerRuntime) {
			add(6, "supports common warm-start targets")
		}
	case "web-app-smoke":
		hasAccessPlane := capabilities.URLBridge || capabilities.SSHMesh ||
			capabilities.Tailscale || capabilities.TailscaleEgress ||
			hasFeature(FeatureBrowser) || hasFeature(FeatureCode) || hasFeature(FeatureDesktop)
		if !hasAccessPlane {
			break
		}
		if capabilities.URLBridge {
			add(70, "can expose provider-native app or service URLs")
		}
		if capabilities.SSHMesh && hasFeature(FeatureCrabboxSync) {
			add(32, "can smoke a synced web app through operator-side SSH tunnels")
		}
		if capabilities.Tailscale {
			add(28, "can reach services over a bidirectional tailnet plane")
		}
		if hasFeature(FeatureBrowser) {
			add(25, "can pair smoke tests with browser access")
		}
		if hasFeature(FeatureCode) {
			add(18, "can inspect web app state through code-server access")
		}
		if hasFeature(FeatureDesktop) {
			add(14, "can inspect interactive desktop app state")
		}
		if hasFeature(FeatureRunSession) {
			add(20, "returns reusable sessions for post-smoke inspection")
		}
		if hasFeature(FeatureRunArtifacts) || hasFeature(FeatureRunDownloads) {
			add(16, "can retain smoke outputs or downloads")
		}
		if hasFeature(FeatureCrabboxSync) || hasFeature(FeatureArchiveSync) {
			add(14, "can seed the web app workload from the current checkout")
		}
		if capabilities.TailscaleEgress {
			add(10, "can use outbound-only tailnet egress for web smoke flows")
		}
		if category == "delegated-sandbox" {
			add(18, "delegated sandbox can host app smoke workloads")
		}
		if hasFeature(FeatureCleanup) {
			add(10, "can clean up smoke resources")
		}
		if hasTarget(targetLinux) || hasTarget(targetWorkerRuntime) {
			add(8, "supports common web app smoke targets")
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

func isRemoteDevProvider(provider string) bool {
	switch provider {
	case "codesandbox", "daytona", "morph", "namespace-devbox", "opencomputer":
		return true
	default:
		return false
	}
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
	fmt.Fprintln(out, "  crabbox providers recommend artifact-download")
	fmt.Fprintln(out, "  crabbox providers recommend ci-proof")
	fmt.Fprintln(out, "  crabbox providers recommend code-interpreter")
	fmt.Fprintln(out, "  crabbox providers recommend cost-control")
	fmt.Fprintln(out, "  crabbox providers recommend disposable-execution")
	fmt.Fprintln(out, "  crabbox providers recommend agent-sandbox --json")
	fmt.Fprintln(out, "  crabbox providers recommend fast-feedback --feature cache-volume")
	fmt.Fprintln(out, "  crabbox providers recommend failure-diagnostics")
	fmt.Fprintln(out, "  crabbox providers recommend fanout-testing --workspace fork")
	fmt.Fprintln(out, "  crabbox providers recommend interactive-debug")
	fmt.Fprintln(out, "  crabbox providers recommend isolated-execution")
	fmt.Fprintln(out, "  crabbox providers recommend linux-vm --limit 8")
	fmt.Fprintln(out, "  crabbox providers recommend live-smoke")
	fmt.Fprintln(out, "  crabbox providers recommend mcp-sandbox")
	fmt.Fprintln(out, "  crabbox providers recommend network-isolation")
	fmt.Fprintln(out, "  crabbox providers recommend offline-validation")
	fmt.Fprintln(out, "  crabbox providers recommend pause-resume")
	fmt.Fprintln(out, "  crabbox providers recommend preview-url")
	fmt.Fprintln(out, "  crabbox providers recommend reachability")
	fmt.Fprintln(out, "  crabbox providers recommend remote-dev")
	fmt.Fprintln(out, "  crabbox providers recommend resource-observability")
	fmt.Fprintln(out, "  crabbox providers recommend run-evidence")
	fmt.Fprintln(out, "  crabbox providers recommend run-session")
	fmt.Fprintln(out, "  crabbox providers recommend team-cloud")
	fmt.Fprintln(out, "  crabbox providers recommend versioned-workspace")
	fmt.Fprintln(out, "  crabbox providers recommend warm-start")
	fmt.Fprintln(out, "  crabbox providers recommend web-app-smoke")
	fmt.Fprintln(out, "  crabbox providers recommend forkable-workspace --workspace fork")
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
		if len(entry.Runtime) > 0 {
			fmt.Fprintf(out, "  runtime: %s\n", commaOrDash(entry.Runtime))
		}
		if len(entry.Reachability) > 0 {
			fmt.Fprintf(out, "  reachability: %s\n", commaOrDash(entry.Reachability))
		}
		if len(entry.Workspace) > 0 {
			fmt.Fprintf(out, "  workspace: %s\n", commaOrDash(entry.Workspace))
		}
		if len(entry.Evidence) > 0 {
			fmt.Fprintf(out, "  evidence: %s\n", commaOrDash(entry.Evidence))
		}
		if len(entry.Lifecycle) > 0 {
			fmt.Fprintf(out, "  lifecycle: %s\n", commaOrDash(entry.Lifecycle))
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
		if len(entry.Runtime) > 0 {
			fmt.Fprintf(out, "  runtime: %s\n", commaOrDash(entry.Runtime))
		}
		if len(entry.Reachability) > 0 {
			fmt.Fprintf(out, "  reachability: %s\n", commaOrDash(entry.Reachability))
		}
		if len(entry.Workspace) > 0 {
			fmt.Fprintf(out, "  workspace: %s\n", commaOrDash(entry.Workspace))
		}
		if len(entry.Evidence) > 0 {
			fmt.Fprintf(out, "  evidence: %s\n", commaOrDash(entry.Evidence))
		}
		if len(entry.Lifecycle) > 0 {
			fmt.Fprintf(out, "  lifecycle: %s\n", commaOrDash(entry.Lifecycle))
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

func runtimeCapabilitiesForProvider(provider string, kind ProviderKind, category string, targets []string, features []Feature) []string {
	seen := map[string]bool{}
	var out []string
	add := func(capability string) {
		if capability == "" || seen[capability] {
			return
		}
		seen[capability] = true
		out = append(out, capability)
	}
	switch kind {
	case ProviderKindSSHLease:
		add("ssh-host")
	case ProviderKindDelegatedRun:
		add("delegated-command")
	case ProviderKindServiceControl:
		add("service-control")
	}
	if strings.HasPrefix(category, "local-") {
		add("local-runtime")
	}
	if category == "delegated-sandbox" {
		add("managed-sandbox")
	}
	if category == "local-sandbox" {
		add("local-sandbox")
	}
	if category == "ci-proof-runner" {
		add("ci-runner")
	}
	if isRemoteDevProvider(provider) {
		add("remote-dev")
	}
	if providerRecommendationHasString(targets, targetWorkerRuntime) || FeatureSet(features).Has(FeatureModuleRun) {
		add("worker-module")
	}
	if FeatureSet(features).Has(FeatureDesktop) || FeatureSet(features).Has(FeatureBrowser) || FeatureSet(features).Has(FeatureCode) {
		add("interactive")
	}
	return out
}

func reachabilityCapabilitiesForProvider(provider string) []string {
	capabilities := providerCapabilities(provider)
	var out []string
	if capabilities.Tailscale {
		out = append(out, "tailnet-peer")
	}
	if capabilities.TailscaleEgress {
		out = append(out, "tailnet-egress")
	}
	if capabilities.URLBridge {
		out = append(out, "provider-url")
	}
	if capabilities.SSHMesh {
		out = append(out, "ssh-tunnel")
	}
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

func lifecycleCapabilitiesForProvider(coordinator CoordinatorMode, features []Feature) []string {
	var out []string
	addFeature := func(feature Feature, capability string) {
		if FeatureSet(features).Has(feature) {
			out = append(out, capability)
		}
	}
	if coordinator == CoordinatorSupported {
		out = append(out, "coordinator-governed")
	}
	addFeature(FeatureCleanup, "cleanup")
	addFeature(FeaturePauseResume, "pause-resume")
	addFeature(FeatureRunSession, "run-session")
	if FeatureSet(features).Has(FeatureCheckpoint) || FeatureSet(features).Has(FeatureFork) ||
		FeatureSet(features).Has(FeatureRestore) || FeatureSet(features).Has(FeatureSnapshot) {
		out = append(out, "workspace-state")
	}
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
