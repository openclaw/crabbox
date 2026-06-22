package cli

import (
	"context"
	"encoding/json"
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

func (a App) providers(_ context.Context, args []string) error {
	if len(args) > 0 && args[0] == "recommend" {
		return a.providerRecommendations(args[1:])
	}
	fs := newFlagSet("providers", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return exit(2, "usage: crabbox providers [--json] OR crabbox providers recommend <use-case> [--limit N] [--json]")
	}
	entries := providerMatrix()
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(entries)
	}
	printProviderMatrix(a.Stdout, entries)
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
	recommendations := recommendProvidersForUseCase(providerMatrix(), canonical, *limit)
	if len(recommendations) == 0 {
		return exit(1, "no providers matched use case %q", canonical)
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
			Targets:     formatProviderTargets(spec.Targets),
			Features:    append(FeatureSet{}, spec.Features...),
			Workspace:   workspaceCapabilitiesForFeatures(spec.Features),
			Evidence:    evidenceCapabilitiesForFeatures(spec.Features),
			Coordinator: string(spec.Coordinator),
		})
	}
	return entries
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
