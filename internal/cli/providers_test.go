package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestProviderMatrixIncludesCapabilities(t *testing.T) {
	entries := providerMatrix()
	var aws *providerMatrixEntry
	var incus *providerMatrixEntry
	var digitalOcean *providerMatrixEntry
	var nvidiaBrev *providerMatrixEntry
	var linode *providerMatrixEntry
	var nebius *providerMatrixEntry
	var scaleway *providerMatrixEntry
	var blacksmith *providerMatrixEntry
	var e2b *providerMatrixEntry
	var islo *providerMatrixEntry
	var localContainer *providerMatrixEntry
	var parallels *providerMatrixEntry
	var moduleRuntime *providerMatrixEntry
	for i := range entries {
		if entries[i].Provider == "aws" {
			aws = &entries[i]
		}
		if entries[i].Provider == "incus" {
			incus = &entries[i]
		}
		if entries[i].Provider == "digitalocean" {
			digitalOcean = &entries[i]
		}
		if entries[i].Provider == "nvidia-brev" {
			nvidiaBrev = &entries[i]
		}
		if entries[i].Provider == "linode" {
			linode = &entries[i]
		}
		if entries[i].Provider == "nebius" {
			nebius = &entries[i]
		}
		if entries[i].Provider == "scaleway" {
			scaleway = &entries[i]
		}
		if entries[i].Provider == "blacksmith-testbox" {
			blacksmith = &entries[i]
		}
		if entries[i].Provider == "e2b" {
			e2b = &entries[i]
		}
		if entries[i].Provider == "islo" {
			islo = &entries[i]
		}
		if entries[i].Provider == "local-container" {
			localContainer = &entries[i]
		}
		if entries[i].Provider == "parallels" {
			parallels = &entries[i]
		}
		if entries[i].Provider == "module-runtime-test" {
			moduleRuntime = &entries[i]
		}
	}
	if aws == nil {
		t.Fatal("aws provider not found")
	}
	if incus == nil {
		t.Fatal("incus provider not found")
	}
	if digitalOcean == nil {
		t.Fatal("digitalocean provider not found")
	}
	if nvidiaBrev == nil {
		t.Fatal("nvidia-brev provider not found")
	}
	if linode == nil {
		t.Fatal("linode provider not found")
	}
	if nebius == nil {
		t.Fatal("nebius provider not found")
	}
	if scaleway == nil {
		t.Fatal("scaleway provider not found")
	}
	if blacksmith == nil {
		t.Fatal("blacksmith-testbox provider not found")
	}
	if e2b == nil {
		t.Fatal("e2b provider not found")
	}
	if islo == nil {
		t.Fatal("islo provider not found")
	}
	if localContainer == nil {
		t.Fatal("local-container provider not found")
	}
	if parallels == nil {
		t.Fatal("parallels provider not found")
	}
	if aws.Kind != ProviderKindSSHLease {
		t.Fatalf("aws kind=%q", aws.Kind)
	}
	if aws.Family != "aws" {
		t.Fatalf("aws family=%q", aws.Family)
	}
	if aws.Category != "brokerable-cloud" {
		t.Fatalf("aws category=%q", aws.Category)
	}
	if !containsString(aws.Targets, targetLinux) || !containsString(aws.Targets, targetMacOS) {
		t.Fatalf("aws targets=%v", aws.Targets)
	}
	if !containsFeature(aws.Features, FeatureSSH) || !containsFeature(aws.Features, FeatureDesktop) {
		t.Fatalf("aws features=%v", aws.Features)
	}
	for _, capability := range []string{"ssh-host", "interactive"} {
		if !containsString(aws.Runtime, capability) {
			t.Fatalf("aws runtime=%v missing %s", aws.Runtime, capability)
		}
	}
	if !containsString(aws.Reachability, "ssh-tunnel") {
		t.Fatalf("aws reachability=%v missing ssh-tunnel", aws.Reachability)
	}
	for _, capability := range []string{"coordinator-governed", "cleanup"} {
		if !containsString(aws.Lifecycle, capability) {
			t.Fatalf("aws lifecycle=%v missing %s", aws.Lifecycle, capability)
		}
	}
	if incus.Kind != ProviderKindSSHLease || incus.Family != "local-vm" {
		t.Fatalf("incus kind/family=%q/%q", incus.Kind, incus.Family)
	}
	if !containsString(incus.Targets, targetLinux) {
		t.Fatalf("incus targets=%v", incus.Targets)
	}
	for _, feature := range []Feature{FeatureSSH, FeatureCrabboxSync, FeatureCleanup} {
		if !containsFeature(incus.Features, feature) {
			t.Fatalf("incus features=%v missing %s", incus.Features, feature)
		}
	}
	if digitalOcean.Kind != ProviderKindSSHLease || digitalOcean.Family != "digitalocean" || digitalOcean.Coordinator != string(CoordinatorNever) {
		t.Fatalf("digitalocean kind/family/coordinator=%q/%q/%q", digitalOcean.Kind, digitalOcean.Family, digitalOcean.Coordinator)
	}
	if !containsString(digitalOcean.Targets, targetLinux) {
		t.Fatalf("digitalocean targets=%v", digitalOcean.Targets)
	}
	if linode.Kind != ProviderKindSSHLease || linode.Family != "linode" || linode.Coordinator != string(CoordinatorNever) {
		t.Fatalf("linode kind/family/coordinator=%q/%q/%q", linode.Kind, linode.Family, linode.Coordinator)
	}
	if !containsString(linode.Targets, targetLinux) {
		t.Fatalf("linode targets=%v", linode.Targets)
	}
	if nebius.Kind != ProviderKindSSHLease || nebius.Family != "nebius" || nebius.Coordinator != string(CoordinatorNever) {
		t.Fatalf("nebius kind/family/coordinator=%q/%q/%q", nebius.Kind, nebius.Family, nebius.Coordinator)
	}
	if !containsString(nebius.Targets, targetLinux) {
		t.Fatalf("nebius targets=%v", nebius.Targets)
	}
	if !containsFeature(nebius.Features, FeatureSSH) || !containsFeature(nebius.Features, FeatureCrabboxSync) || !containsFeature(nebius.Features, FeatureCleanup) {
		t.Fatalf("nebius features=%v", nebius.Features)
	}
	if scaleway.Kind != ProviderKindSSHLease || scaleway.Family != "scaleway" || scaleway.Coordinator != string(CoordinatorNever) {
		t.Fatalf("scaleway kind/family/coordinator=%q/%q/%q", scaleway.Kind, scaleway.Family, scaleway.Coordinator)
	}
	if !containsString(scaleway.Targets, targetLinux) {
		t.Fatalf("scaleway targets=%v", scaleway.Targets)
	}
	for _, feature := range []Feature{FeatureSSH, FeatureCrabboxSync, FeatureCleanup, FeatureTailscale} {
		if !containsFeature(scaleway.Features, feature) {
			t.Fatalf("scaleway features=%v missing %s", scaleway.Features, feature)
		}
	}
	for _, capability := range []string{"tailnet-peer", "ssh-tunnel"} {
		if !containsString(scaleway.Reachability, capability) {
			t.Fatalf("scaleway reachability=%v missing %s", scaleway.Reachability, capability)
		}
	}
	if moduleRuntime == nil {
		t.Fatal("module-runtime-test provider not found")
	}
	if moduleRuntime.Kind != ProviderKindDelegatedRun || !containsString(moduleRuntime.Targets, targetWorkerRuntime) {
		t.Fatalf("module-runtime-test kind/targets=%q/%v", moduleRuntime.Kind, moduleRuntime.Targets)
	}
	if !containsFeature(moduleRuntime.Features, FeatureModuleRun) {
		t.Fatalf("module-runtime-test features=%v missing %s", moduleRuntime.Features, FeatureModuleRun)
	}
	for _, capability := range []string{"delegated-command", "worker-module"} {
		if !containsString(moduleRuntime.Runtime, capability) {
			t.Fatalf("module-runtime-test runtime=%v missing %s", moduleRuntime.Runtime, capability)
		}
	}
	if nvidiaBrev.Kind != ProviderKindSSHLease || nvidiaBrev.Family != "nvidia-brev" || nvidiaBrev.Coordinator != string(CoordinatorNever) {
		t.Fatalf("nvidia-brev kind/family/coordinator=%q/%q/%q", nvidiaBrev.Kind, nvidiaBrev.Family, nvidiaBrev.Coordinator)
	}
	if !containsString(nvidiaBrev.Targets, targetLinux) {
		t.Fatalf("nvidia-brev targets=%v", nvidiaBrev.Targets)
	}
	if !containsFeature(nvidiaBrev.Features, FeatureSSH) || !containsFeature(nvidiaBrev.Features, FeatureCrabboxSync) || !containsFeature(nvidiaBrev.Features, FeatureCleanup) {
		t.Fatalf("nvidia-brev features=%v", nvidiaBrev.Features)
	}
	if !containsString(nvidiaBrev.Aliases, "brev") || !containsString(nvidiaBrev.Aliases, "nvidia") {
		t.Fatalf("nvidia-brev aliases=%v", nvidiaBrev.Aliases)
	}
	for _, capability := range []string{"local-runtime", "ssh-host"} {
		if !containsString(localContainer.Runtime, capability) {
			t.Fatalf("local-container runtime=%v missing %s", localContainer.Runtime, capability)
		}
	}
	if !containsString(localContainer.Workspace, "checkpoint") || !containsString(localContainer.Workspace, "fork") {
		t.Fatalf("local-container workspace=%v", localContainer.Workspace)
	}
	for _, capability := range []string{"cleanup", "workspace-state"} {
		if !containsString(localContainer.Lifecycle, capability) {
			t.Fatalf("local-container lifecycle=%v missing %s", localContainer.Lifecycle, capability)
		}
	}
	for _, capability := range []string{"checkpoint", "fork", "restore", "snapshot-ref"} {
		if !containsString(parallels.Workspace, capability) {
			t.Fatalf("parallels workspace=%v missing %s", parallels.Workspace, capability)
		}
	}
	for _, capability := range []string{"proof", "artifacts", "session"} {
		if !containsString(blacksmith.Evidence, capability) {
			t.Fatalf("blacksmith evidence=%v missing %s", blacksmith.Evidence, capability)
		}
	}
	if !containsString(blacksmith.Lifecycle, "run-session") {
		t.Fatalf("blacksmith lifecycle=%v missing run-session", blacksmith.Lifecycle)
	}
	for _, capability := range []string{"delegated-command", "ci-runner"} {
		if !containsString(blacksmith.Runtime, capability) {
			t.Fatalf("blacksmith runtime=%v missing %s", blacksmith.Runtime, capability)
		}
	}
	for _, capability := range []string{"preview-url", "session"} {
		if !containsString(e2b.Evidence, capability) {
			t.Fatalf("e2b evidence=%v missing %s", e2b.Evidence, capability)
		}
	}
	if !containsString(e2b.Lifecycle, "run-session") {
		t.Fatalf("e2b lifecycle=%v missing run-session", e2b.Lifecycle)
	}
	if !containsString(e2b.Reachability, "provider-url") {
		t.Fatalf("e2b reachability=%v missing provider-url", e2b.Reachability)
	}
	for _, capability := range []string{"delegated-command", "managed-sandbox"} {
		if !containsString(e2b.Runtime, capability) {
			t.Fatalf("e2b runtime=%v missing %s", e2b.Runtime, capability)
		}
	}
	for _, capability := range []string{"downloads", "preview-url", "session"} {
		if !containsString(islo.Evidence, capability) {
			t.Fatalf("islo evidence=%v missing %s", islo.Evidence, capability)
		}
	}
	for _, capability := range []string{"pause-resume", "run-session"} {
		if !containsString(islo.Lifecycle, capability) {
			t.Fatalf("islo lifecycle=%v missing %s", islo.Lifecycle, capability)
		}
	}
	for _, capability := range []string{"tailnet-egress", "provider-url"} {
		if !containsString(islo.Reachability, capability) {
			t.Fatalf("islo reachability=%v missing %s", islo.Reachability, capability)
		}
	}
}

func TestProvidersCommandJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{"--json"})
	if err != nil {
		t.Fatalf("providers --json error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerMatrixEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) == 0 {
		t.Fatal("empty providers json")
	}
	for _, entry := range entries {
		if entry.Features == nil {
			t.Fatalf("provider %s encoded nil features", entry.Provider)
		}
		if entry.Provider == "aws" && entry.Category != "brokerable-cloud" {
			t.Fatalf("aws json category=%q", entry.Category)
		}
		if entry.Provider == "aws" && !containsString(entry.Runtime, "ssh-host") {
			t.Fatalf("aws json missing ssh-host runtime: %#v", entry)
		}
		if entry.Provider == "aws" && !containsString(entry.Reachability, "ssh-tunnel") {
			t.Fatalf("aws json missing ssh-tunnel reachability: %#v", entry)
		}
		if entry.Provider == "aws" && !containsString(entry.Lifecycle, "coordinator-governed") {
			t.Fatalf("aws json missing coordinator-governed lifecycle: %#v", entry)
		}
		if entry.Provider == "parallels" && !containsString(entry.Workspace, "snapshot-ref") {
			t.Fatalf("parallels json missing workspace snapshot-ref: %#v", entry)
		}
		if entry.Provider == "parallels" && !containsString(entry.Lifecycle, "workspace-state") {
			t.Fatalf("parallels json missing workspace-state lifecycle: %#v", entry)
		}
		if entry.Provider == "blacksmith-testbox" && !containsString(entry.Evidence, "proof") {
			t.Fatalf("blacksmith json missing evidence proof: %#v", entry)
		}
		if entry.Provider == "blacksmith-testbox" && !containsString(entry.Lifecycle, "run-session") {
			t.Fatalf("blacksmith json missing run-session lifecycle: %#v", entry)
		}
	}
}

func TestProvidersCommandHumanOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), nil)
	if err != nil {
		t.Fatalf("providers error=%v stderr=%q", err, stderr.String())
	}
	text := stdout.String()
	for _, want := range []string{"aws\n", "  family: aws\n", "  kind: ssh-lease\n", "  category: brokerable-cloud\n", "  features: ", "  runtime: ssh-host,interactive\n", "  reachability: ssh-tunnel\n", "  lifecycle: coordinator-governed,cleanup\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("providers output missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "module-runtime-test\n") || !strings.Contains(text, "  targets: worker-runtime\n") || !strings.Contains(text, "  features: module-run\n") {
		t.Fatalf("providers output missing module runtime contract:\n%s", text)
	}
	if !strings.Contains(text, "incus\n") {
		t.Fatalf("providers output missing incus:\n%s", text)
	}
	if !strings.Contains(text, "parallels\n") || !strings.Contains(text, "  workspace: checkpoint,fork,restore,snapshot-ref\n") {
		t.Fatalf("providers output missing workspace contract:\n%s", text)
	}
	if !strings.Contains(text, "parallels\n") || !strings.Contains(text, "  lifecycle: cleanup,workspace-state\n") {
		t.Fatalf("providers output missing lifecycle contract:\n%s", text)
	}
	if !strings.Contains(text, "blacksmith-testbox\n") || !strings.Contains(text, "  evidence: proof,artifacts,session\n") {
		t.Fatalf("providers output missing evidence contract:\n%s", text)
	}
	if !strings.Contains(text, "blacksmith-testbox\n") || !strings.Contains(text, "  lifecycle: run-session\n") {
		t.Fatalf("providers output missing run-session lifecycle:\n%s", text)
	}
}

func TestProvidersCommandFiltersJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"--kind", "delegated-run",
		"--category", "delegated-sandbox",
		"--target", "linux",
		"--runtime", "managed-sandbox",
		"--reachability", "provider-url",
		"--evidence", "preview-url",
		"--lifecycle", "run-session",
		"--json",
	})
	if err != nil {
		t.Fatalf("providers filtered --json error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerMatrixEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) == 0 {
		t.Fatal("expected delegated preview providers")
	}
	for _, entry := range entries {
		if entry.Kind != ProviderKindDelegatedRun || entry.Category != "delegated-sandbox" || !containsString(entry.Targets, targetLinux) || !containsString(entry.Runtime, "managed-sandbox") || !containsString(entry.Reachability, "provider-url") || !containsString(entry.Evidence, "preview-url") || !containsString(entry.Lifecycle, "run-session") {
			t.Fatalf("entry escaped filters: %#v", entry)
		}
	}
}

func TestProvidersCommandFiltersRequireAllCapabilities(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"--workspace", "checkpoint,fork",
		"--lifecycle", "cleanup,workspace-state",
	})
	if err != nil {
		t.Fatalf("providers workspace filter error=%v stderr=%q", err, stderr.String())
	}
	text := stdout.String()
	if !strings.Contains(text, "local-container\n") {
		t.Fatalf("workspace/lifecycle filter should include local-container:\n%s", text)
	}
	if !strings.Contains(text, "parallels\n") {
		t.Fatalf("workspace/lifecycle filter should include parallels:\n%s", text)
	}
	if strings.Contains(text, "blacksmith-testbox\n") {
		t.Fatalf("workspace/lifecycle filter should exclude providers without workspace capabilities:\n%s", text)
	}
}

func TestProvidersCommandRejectsUnknownFilter(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{"--runtime", "microvm-fork"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("providers unknown filter error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(err.Error(), `unknown provider runtime filter "microvm-fork"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProvidersCommandRejectsUnknownLifecycleFilter(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{"--lifecycle", "immortal"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("providers unknown lifecycle filter error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(err.Error(), `unknown provider lifecycle filter "immortal"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProvidersFiltersCommandHumanOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{"filters"})
	if err != nil {
		t.Fatalf("providers filters error=%v stderr=%q", err, stderr.String())
	}
	text := stdout.String()
	for _, want := range []string{
		"provider filter values:",
		"  kind: ",
		"delegated-run",
		"  category: ",
		"delegated-sandbox",
		"  runtime: ",
		"managed-sandbox",
		"  reachability: ",
		"provider-url",
		"  evidence: ",
		"preview-url",
		"  lifecycle: ",
		"run-session",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("providers filters output missing %q:\n%s", want, text)
		}
	}
}

func TestProvidersFiltersCommandJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{"filters", "--json"})
	if err != nil {
		t.Fatalf("providers filters --json error=%v stderr=%q", err, stderr.String())
	}
	var values providerFilterValuesEntry
	if err := json.Unmarshal(stdout.Bytes(), &values); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	for _, tc := range []struct {
		name   string
		values []string
		want   string
	}{
		{name: "kind", values: values.Kind, want: "delegated-run"},
		{name: "category", values: values.Category, want: "delegated-sandbox"},
		{name: "runtime", values: values.Runtime, want: "managed-sandbox"},
		{name: "reachability", values: values.Reachability, want: "provider-url"},
		{name: "evidence", values: values.Evidence, want: "preview-url"},
		{name: "workspace", values: values.Workspace, want: "fork"},
		{name: "lifecycle", values: values.Lifecycle, want: "workspace-state"},
	} {
		if !containsString(tc.values, tc.want) {
			t.Fatalf("%s values=%v missing %q", tc.name, tc.values, tc.want)
		}
	}
}

func TestProvidersFiltersRejectsArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{"filters", "kind"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("providers filters arg error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}

func TestProvidersRecommendListsUseCases(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{"recommend"})
	if err != nil {
		t.Fatalf("providers recommend error=%v stderr=%q", err, stderr.String())
	}
	text := stdout.String()
	for _, want := range []string{
		"provider recommendation use cases:",
		"artifact-download",
		"ci-proof",
		"cost-control",
		"agent-sandbox",
		"fast-feedback",
		"failure-diagnostics",
		"fanout-testing",
		"isolated-execution",
		"live-smoke",
		"mcp-sandbox",
		"network-isolation",
		"offline-validation",
		"pause-resume",
		"preview-url",
		"reachability",
		"remote-dev",
		"run-evidence",
		"run-session",
		"team-cloud",
		"versioned-workspace",
		"warm-start",
		"worker-runtime",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("providers recommend output missing %q:\n%s", want, text)
		}
	}
}

func TestProvidersRecommendArtifactDownloadPrefersArtifactProviders(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "artifact-download", 4)
	if len(recommendations) == 0 {
		t.Fatal("expected artifact-download recommendations")
	}
	found := map[string]bool{}
	for _, recommendation := range recommendations {
		hasArtifact := providerRecommendationHasFeature(recommendation.Features, FeatureRunArtifacts)
		hasDownload := providerRecommendationHasFeature(recommendation.Features, FeatureRunDownloads)
		if !hasArtifact && !hasDownload {
			t.Fatalf("artifact-download recommendation lacks artifact/download capability: %#v", recommendation)
		}
		if hasArtifact && !containsString(recommendation.Evidence, "artifacts") {
			t.Fatalf("artifact-download recommendation missing artifacts evidence: %#v", recommendation)
		}
		if hasDownload && !containsString(recommendation.Evidence, "downloads") {
			t.Fatalf("artifact-download recommendation missing downloads evidence: %#v", recommendation)
		}
		found[recommendation.Provider] = true
	}
	for _, provider := range []string{"blacksmith-testbox", "islo"} {
		if !found[provider] {
			t.Fatalf("artifact-download recommendations should include %s: %#v", provider, recommendations)
		}
	}
}

func TestProvidersRecommendArtifactDownloadAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"recommend", "run-artifacts",
		"--limit", "1",
		"--json",
	})
	if err != nil {
		t.Fatalf("providers recommend run-artifacts error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 1 || !providerRecommendationHasFeature(entries[0].Features, FeatureRunArtifacts) {
		t.Fatalf("run-artifacts alias entries=%#v", entries)
	}
}

func TestProvidersRecommendCostControlPrefersReusableOrGovernedCapacity(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "cost-control", 64)
	if len(recommendations) == 0 {
		t.Fatal("expected cost-control recommendations")
	}
	if !strings.HasPrefix(recommendations[0].Category, "local-") {
		t.Fatalf("top cost-control category=%q recommendations=%v", recommendations[0].Category, recommendations)
	}
	foundLocal := false
	foundCoordinator := false
	foundCleanup := false
	for _, recommendation := range recommendations {
		if strings.HasPrefix(recommendation.Category, "local-") {
			foundLocal = true
		}
		if recommendation.Provider == "aws" || recommendation.Provider == "azure" ||
			recommendation.Provider == "gcp" || recommendation.Provider == "hetzner" {
			foundCoordinator = true
		}
		if providerRecommendationHasFeature(recommendation.Features, FeatureCleanup) {
			foundCleanup = true
		}
	}
	if !foundLocal {
		t.Fatalf("cost-control recommendations should include local providers: %#v", recommendations)
	}
	if !foundCoordinator {
		t.Fatalf("cost-control recommendations should include coordinator-governed cloud providers: %#v", recommendations)
	}
	if !foundCleanup {
		t.Fatalf("cost-control recommendations should include cleanup-capable providers: %#v", recommendations)
	}
}

func TestProvidersRecommendCostControlAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"recommend", "budget",
		"--limit", "1",
		"--json",
	})
	if err != nil {
		t.Fatalf("providers recommend budget error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Category, "local-") {
		t.Fatalf("budget alias entries=%#v", entries)
	}
}

func TestProvidersRecommendOfflineValidationPrefersCredentiallessProviders(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "offline-validation", 16)
	if len(recommendations) == 0 {
		t.Fatal("expected offline-validation recommendations")
	}
	if !strings.HasPrefix(recommendations[0].Category, "local-") {
		t.Fatalf("top offline-validation category=%q recommendations=%v", recommendations[0].Category, recommendations)
	}
	foundLocalRuntime := false
	foundLocalSandbox := false
	foundLocalVM := false
	foundBYO := false
	for _, recommendation := range recommendations {
		switch {
		case strings.HasPrefix(recommendation.Category, "local-"):
			if recommendation.Category == "local-runtime" {
				foundLocalRuntime = true
			}
			if recommendation.Category == "local-sandbox" {
				foundLocalSandbox = true
			}
			if recommendation.Category == "local-vm" {
				foundLocalVM = true
			}
		case recommendation.Category == "byo-ssh":
			foundBYO = true
		case recommendation.Category == "external-provider":
		default:
			t.Fatalf("offline-validation recommendation requires provider credentials: %#v", recommendation)
		}
	}
	if !foundLocalRuntime || !foundLocalSandbox || !foundLocalVM {
		t.Fatalf("offline-validation should include local runtime, sandbox, and VM providers: %#v", recommendations)
	}
	if !foundBYO {
		t.Fatalf("offline-validation should include BYO SSH fallback: %#v", recommendations)
	}
}

func TestProvidersRecommendNoCredentialsAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"recommend", "no-credentials",
		"--limit", "1",
		"--json",
	})
	if err != nil {
		t.Fatalf("providers recommend no-credentials error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Category, "local-") {
		t.Fatalf("no-credentials alias entries=%#v", entries)
	}
}

func TestProvidersRecommendFastFeedbackPrefersCacheVolumes(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "fast-feedback", 5)
	if len(recommendations) == 0 {
		t.Fatal("expected fast-feedback recommendations")
	}
	if !strings.HasPrefix(recommendations[0].Category, "local-") {
		t.Fatalf("top fast-feedback category=%q recommendations=%v", recommendations[0].Category, recommendations)
	}
	if !providerRecommendationHasFeature(recommendations[0].Features, FeatureCacheVolume) {
		t.Fatalf("top fast-feedback recommendation missing cache-volume feature: %#v", recommendations[0])
	}
	foundProofRunner := false
	for _, recommendation := range recommendations {
		if recommendation.Provider == "blacksmith-testbox" {
			foundProofRunner = true
		}
		if !providerRecommendationHasFeature(recommendation.Features, FeatureCacheVolume) &&
			!strings.HasPrefix(recommendation.Category, "local-") &&
			recommendation.Category != "ci-proof-runner" {
			t.Fatalf("fast-feedback recommendation lacks cache, local runtime, or proof-runner signal: %#v", recommendation)
		}
	}
	if !foundProofRunner {
		t.Fatalf("fast-feedback recommendations should include CI proof runners: %#v", recommendations)
	}
}

func TestProvidersRecommendFailureDiagnosticsPrefersInspectableEvidence(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "failure-diagnostics", 8)
	if len(recommendations) == 0 {
		t.Fatal("expected failure-diagnostics recommendations")
	}
	if recommendations[0].Provider != "blacksmith-testbox" {
		t.Fatalf("top failure-diagnostics provider=%q recommendations=%v", recommendations[0].Provider, recommendations)
	}
	foundProof := false
	foundSession := false
	foundDownload := false
	foundPreview := false
	foundSSHDebug := false
	for _, recommendation := range recommendations {
		hasProof := providerRecommendationHasFeature(recommendation.Features, FeatureRunProof)
		hasSession := providerRecommendationHasFeature(recommendation.Features, FeatureRunSession)
		hasArtifact := providerRecommendationHasFeature(recommendation.Features, FeatureRunArtifacts)
		hasDownload := providerRecommendationHasFeature(recommendation.Features, FeatureRunDownloads)
		hasPreview := providerRecommendationHasFeature(recommendation.Features, FeatureURLBridge)
		hasSSHDebug := providerRecommendationHasFeature(recommendation.Features, FeatureSSH) &&
			providerRecommendationHasFeature(recommendation.Features, FeatureCrabboxSync)
		if !hasProof && !hasSession && !hasArtifact && !hasDownload && !hasPreview && !hasSSHDebug {
			t.Fatalf("failure-diagnostics recommendation lacks diagnostic signal: %#v", recommendation)
		}
		foundProof = foundProof || hasProof
		foundSession = foundSession || hasSession
		foundDownload = foundDownload || hasDownload
		foundPreview = foundPreview || hasPreview
		foundSSHDebug = foundSSHDebug || hasSSHDebug
	}
	if !foundProof || !foundSession || !foundDownload || !foundPreview || !foundSSHDebug {
		t.Fatalf("failure-diagnostics should include proof, session, download, preview, and SSH-debuggable providers: %#v", recommendations)
	}
}

func TestProvidersRecommendWarmStartPrefersReusableState(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "warm-start", 12)
	if len(recommendations) == 0 {
		t.Fatal("expected warm-start recommendations")
	}
	if recommendations[0].Provider != "local-container" && recommendations[0].Provider != "parallels" {
		t.Fatalf("top warm-start provider=%q recommendations=%v", recommendations[0].Provider, recommendations)
	}
	foundCache := false
	foundSession := false
	foundPause := false
	foundWorkspaceState := false
	foundLocalRuntime := false
	for _, recommendation := range recommendations {
		hasCache := providerRecommendationHasFeature(recommendation.Features, FeatureCacheVolume)
		hasSession := providerRecommendationHasFeature(recommendation.Features, FeatureRunSession)
		hasPause := providerRecommendationHasFeature(recommendation.Features, FeaturePauseResume)
		hasWorkspaceState := providerRecommendationHasFeature(recommendation.Features, FeatureCheckpoint) ||
			providerRecommendationHasFeature(recommendation.Features, FeatureFork) ||
			providerRecommendationHasFeature(recommendation.Features, FeatureRestore) ||
			providerRecommendationHasFeature(recommendation.Features, FeatureSnapshot)
		hasLocalRuntime := strings.HasPrefix(recommendation.Category, "local-")
		if !hasCache && !hasSession && !hasPause && !hasWorkspaceState && !hasLocalRuntime {
			t.Fatalf("warm-start recommendation lacks warm-state signal: %#v", recommendation)
		}
		foundCache = foundCache || hasCache
		foundSession = foundSession || hasSession
		foundPause = foundPause || hasPause
		foundWorkspaceState = foundWorkspaceState || hasWorkspaceState
		foundLocalRuntime = foundLocalRuntime || hasLocalRuntime
	}
	if !foundCache || !foundSession || !foundPause || !foundWorkspaceState || !foundLocalRuntime {
		t.Fatalf("warm-start should include cache, session, pause/resume, workspace-state, and local-runtime signals: %#v", recommendations)
	}
}

func TestProvidersRecommendWarmPoolAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"recommend", "warm-pool",
		"--limit", "1",
		"--json",
	})
	if err != nil {
		t.Fatalf("providers recommend warm-pool error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 1 {
		t.Fatalf("warm-pool alias entries=%#v", entries)
	}
	if entries[0].Provider != "local-container" && entries[0].Provider != "parallels" {
		t.Fatalf("warm-pool alias should prefer local reusable state providers: %#v", entries)
	}
}

func TestProvidersRecommendFailedRunAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"recommend", "failed-run",
		"--limit", "1",
		"--json",
	})
	if err != nil {
		t.Fatalf("providers recommend failed-run error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 1 || entries[0].Provider != "blacksmith-testbox" {
		t.Fatalf("failed-run alias entries=%#v", entries)
	}
}

func TestProvidersRecommendFanoutTestingRequiresForkableWorkspaces(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "fanout-testing", 8)
	if len(recommendations) == 0 {
		t.Fatal("expected fanout-testing recommendations")
	}
	if recommendations[0].Provider != "parallels" {
		t.Fatalf("top fanout-testing provider=%q recommendations=%v", recommendations[0].Provider, recommendations)
	}
	foundLocalContainer := false
	for _, recommendation := range recommendations {
		if !providerRecommendationHasFeature(recommendation.Features, FeatureFork) {
			t.Fatalf("fanout-testing recommendation lacks fork feature: %#v", recommendation)
		}
		if !containsString(recommendation.Workspace, "fork") {
			t.Fatalf("fanout-testing recommendation missing fork workspace capability: %#v", recommendation)
		}
		if recommendation.Provider == "local-container" {
			foundLocalContainer = true
		}
	}
	if !foundLocalContainer {
		t.Fatalf("fanout-testing recommendations should include local-container: %#v", recommendations)
	}
}

func TestProvidersRecommendBestOfNAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"recommend", "best-of-n",
		"--limit", "1",
		"--json",
	})
	if err != nil {
		t.Fatalf("providers recommend best-of-n error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 1 || !providerRecommendationHasFeature(entries[0].Features, FeatureFork) {
		t.Fatalf("best-of-n alias entries=%#v", entries)
	}
}

func TestProvidersRecommendRunEvidence(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "run-evidence", 5)
	if len(recommendations) == 0 {
		t.Fatal("expected run-evidence recommendations")
	}
	if recommendations[0].Provider != "blacksmith-testbox" {
		t.Fatalf("top run-evidence provider=%q recommendations=%v", recommendations[0].Provider, recommendations)
	}
	for _, capability := range []string{"proof", "artifacts", "session"} {
		if !containsString(recommendations[0].Evidence, capability) {
			t.Fatalf("top recommendation evidence=%v missing %s", recommendations[0].Evidence, capability)
		}
	}
	for _, recommendation := range recommendations {
		if len(recommendation.Evidence) == 0 {
			t.Fatalf("run-evidence recommendation lacks evidence capabilities: %#v", recommendation)
		}
		if recommendation.Provider == "wandb" {
			t.Fatalf("run-evidence should not recommend session-only providers: %#v", recommendation)
		}
	}
}

func TestProvidersRecommendRunSessionPrefersInspectableRuns(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "run-session", 8)
	if len(recommendations) == 0 {
		t.Fatal("expected run-session recommendations")
	}
	foundProof := false
	foundPreview := false
	foundWorker := false
	for _, recommendation := range recommendations {
		if !providerRecommendationHasFeature(recommendation.Features, FeatureRunSession) {
			t.Fatalf("run-session recommendation lacks run-session feature: %#v", recommendation)
		}
		if !containsString(recommendation.Evidence, "session") {
			t.Fatalf("run-session recommendation lacks session evidence: %#v", recommendation)
		}
		if providerRecommendationHasFeature(recommendation.Features, FeatureRunProof) {
			foundProof = true
		}
		if providerRecommendationHasFeature(recommendation.Features, FeatureURLBridge) {
			foundPreview = true
		}
		if providerRecommendationHasString(recommendation.Targets, targetWorkerRuntime) {
			foundWorker = true
		}
	}
	if !foundProof {
		t.Fatalf("run-session recommendations should include proof-capable sessions: %#v", recommendations)
	}
	if !foundPreview {
		t.Fatalf("run-session recommendations should include preview-capable sessions: %#v", recommendations)
	}
	if !foundWorker {
		t.Fatalf("run-session recommendations should include worker runtime sessions: %#v", recommendations)
	}
}

func TestProvidersRecommendRunSessionAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"recommend", "inspectable-run",
		"--limit", "1",
		"--json",
	})
	if err != nil {
		t.Fatalf("providers recommend inspectable-run error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 1 || !providerRecommendationHasFeature(entries[0].Features, FeatureRunSession) {
		t.Fatalf("inspectable-run alias entries=%#v", entries)
	}
}

func TestProvidersRecommendReachabilityUsesTransportCapabilities(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "reachability", 12)
	if len(recommendations) == 0 {
		t.Fatal("expected reachability recommendations")
	}
	top := providerCapabilities(recommendations[0].Provider)
	if !top.Tailscale && !top.URLBridge && !top.SSHMesh {
		t.Fatalf("top reachability recommendation lacks transport capabilities: %#v", recommendations[0])
	}
	foundTailnet := false
	foundURLBridge := false
	for _, recommendation := range recommendations {
		capabilities := providerCapabilities(recommendation.Provider)
		if capabilities.Tailscale {
			foundTailnet = true
		}
		if capabilities.URLBridge {
			foundURLBridge = true
		}
		if !capabilities.Tailscale && !capabilities.TailscaleEgress && !capabilities.URLBridge && !capabilities.SSHMesh {
			t.Fatalf("reachability recommendation lacks any transport plane: %#v", recommendation)
		}
		if len(recommendation.Reachability) == 0 {
			t.Fatalf("reachability recommendation missing normalized reachability capabilities: %#v", recommendation)
		}
	}
	if !foundTailnet {
		t.Fatalf("reachability recommendations should include tailnet peer providers: %#v", recommendations)
	}
	if !foundURLBridge {
		t.Fatalf("reachability recommendations should include URL bridge providers: %#v", recommendations)
	}
}

func TestProvidersRecommendIsolatedExecutionPrefersDelegatedSandboxes(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "isolated-execution", 8)
	if len(recommendations) == 0 {
		t.Fatal("expected isolated-execution recommendations")
	}
	if recommendations[0].Kind != ProviderKindDelegatedRun {
		t.Fatalf("top isolated-execution kind=%q recommendations=%v", recommendations[0].Kind, recommendations)
	}
	if recommendations[0].Category != "delegated-sandbox" && recommendations[0].Category != "local-sandbox" {
		t.Fatalf("top isolated-execution category=%q recommendations=%v", recommendations[0].Category, recommendations)
	}
	for _, recommendation := range recommendations {
		if recommendation.Category != "delegated-sandbox" && recommendation.Category != "local-sandbox" {
			t.Fatalf("isolated-execution recommendation escaped sandbox categories: %#v", recommendation)
		}
	}
}

func TestProvidersRecommendIsolatedExecutionIncludesLocalSandboxes(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "isolated-execution", 64)
	if len(recommendations) == 0 {
		t.Fatal("expected isolated-execution recommendations")
	}
	foundLocalSandbox := false
	foundDelegatedSandbox := false
	for _, recommendation := range recommendations {
		switch recommendation.Category {
		case "local-sandbox":
			foundLocalSandbox = true
		case "delegated-sandbox":
			foundDelegatedSandbox = true
		}
	}
	if !foundLocalSandbox {
		t.Fatalf("isolated-execution recommendations should include local sandbox providers: %#v", recommendations)
	}
	if !foundDelegatedSandbox {
		t.Fatalf("isolated-execution recommendations should include delegated sandbox providers: %#v", recommendations)
	}
}

func TestProvidersRecommendIsolatedExecutionAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{"recommend", "secure-sandbox", "--limit", "1", "--json"})
	if err != nil {
		t.Fatalf("providers recommend secure-sandbox error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 1 || entries[0].Kind != ProviderKindDelegatedRun {
		t.Fatalf("secure-sandbox alias entries=%#v", entries)
	}
}

func TestProvidersRecommendNetworkIsolationPrefersSandboxBoundaries(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "network-isolation", 64)
	if len(recommendations) == 0 {
		t.Fatal("expected network-isolation recommendations")
	}
	foundLocalSandbox := false
	foundDelegatedSandbox := false
	for _, recommendation := range recommendations {
		switch recommendation.Category {
		case "delegated-sandbox":
			foundDelegatedSandbox = true
		case "local-sandbox":
			foundLocalSandbox = true
		default:
			t.Fatalf("network-isolation recommendation escaped sandbox categories: %#v", recommendation)
		}
	}
	if !foundDelegatedSandbox {
		t.Fatalf("network-isolation recommendations should include delegated sandboxes: %#v", recommendations)
	}
	if !foundLocalSandbox {
		t.Fatalf("network-isolation recommendations should include local sandboxes: %#v", recommendations)
	}
}

func TestProvidersRecommendNetworkIsolationAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"recommend", "egress-control",
		"--limit", "1",
		"--json",
	})
	if err != nil {
		t.Fatalf("providers recommend egress-control error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 1 || entries[0].Category != "delegated-sandbox" {
		t.Fatalf("egress-control alias entries=%#v", entries)
	}
}

func TestProvidersRecommendTeamCloudPrefersBrokerableProviders(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "team-cloud", 4)
	if len(recommendations) == 0 {
		t.Fatal("expected team-cloud recommendations")
	}
	for _, recommendation := range recommendations {
		if recommendation.Category != "brokerable-cloud" {
			t.Fatalf("team-cloud top recommendations should be brokerable cloud providers: %#v", recommendations)
		}
		if recommendation.Kind != ProviderKindSSHLease {
			t.Fatalf("team-cloud recommendation should be SSH lease: %#v", recommendation)
		}
		if !providerRecommendationHasFeature(recommendation.Features, FeatureCleanup) {
			t.Fatalf("team-cloud recommendation missing cleanup feature: %#v", recommendation)
		}
		if !providerRecommendationHasFeature(recommendation.Features, FeatureCrabboxSync) {
			t.Fatalf("team-cloud recommendation missing crabbox-sync feature: %#v", recommendation)
		}
	}
}

func TestProvidersRecommendTeamCloudAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{"recommend", "brokered-cloud", "--limit", "1", "--json"})
	if err != nil {
		t.Fatalf("providers recommend brokered-cloud error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 1 || entries[0].Category != "brokerable-cloud" {
		t.Fatalf("brokered-cloud alias entries=%#v", entries)
	}
}

func TestProvidersRecommendVersionedWorkspace(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "versioned-workspace", 3)
	if len(recommendations) == 0 {
		t.Fatal("expected versioned-workspace recommendations")
	}
	if recommendations[0].Provider != "parallels" {
		t.Fatalf("top versioned-workspace provider=%q recommendations=%v", recommendations[0].Provider, recommendations)
	}
	for _, capability := range []string{"checkpoint", "fork", "restore", "snapshot-ref"} {
		if !containsString(recommendations[0].Workspace, capability) {
			t.Fatalf("top recommendation workspace=%v missing %s", recommendations[0].Workspace, capability)
		}
	}
	for _, recommendation := range recommendations {
		if len(recommendation.Workspace) == 0 {
			t.Fatalf("versioned-workspace recommendation lacks workspace capabilities: %#v", recommendation)
		}
	}
}

func TestProvidersRecommendForkableWorkspaceAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"recommend", "forkable-workspace",
		"--workspace", "fork",
		"--limit", "1",
		"--json",
	})
	if err != nil {
		t.Fatalf("providers recommend forkable-workspace error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 1 {
		t.Fatalf("entry count=%d entries=%#v", len(entries), entries)
	}
	if !containsString(entries[0].Workspace, "fork") {
		t.Fatalf("forkable-workspace alias entry missing fork workspace capability: %#v", entries[0])
	}
}

func TestProvidersRecommendWorkspaceReuseAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"recommend", "workspace-reuse",
		"--limit", "1",
		"--json",
	})
	if err != nil {
		t.Fatalf("providers recommend workspace-reuse error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 1 {
		t.Fatalf("entry count=%d entries=%#v", len(entries), entries)
	}
	if len(entries[0].Workspace) == 0 {
		t.Fatalf("workspace-reuse alias entry missing workspace capabilities: %#v", entries[0])
	}
}

func TestProvidersRecommendMCPSandbox(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "mcp-sandbox", 5)
	if len(recommendations) == 0 {
		t.Fatal("expected mcp-sandbox recommendations")
	}
	if recommendations[0].Provider != "docker-sandbox" {
		t.Fatalf("top mcp-sandbox provider=%q recommendations=%v", recommendations[0].Provider, recommendations)
	}
	for _, recommendation := range recommendations {
		if !providerRecommendationHasFeature(recommendation.Features, FeatureMCP) {
			t.Fatalf("mcp-sandbox recommendation lacks MCP attachments feature: %#v", recommendation)
		}
	}
}

func TestProvidersRecommendMCPSandboxAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"recommend", "mcp",
		"--limit", "1",
		"--json",
	})
	if err != nil {
		t.Fatalf("providers recommend mcp error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 1 || !providerRecommendationHasFeature(entries[0].Features, FeatureMCP) {
		t.Fatalf("mcp alias entries=%#v", entries)
	}
}

func TestProvidersRecommendRemoteDevPrefersManagedDevEnvironments(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "remote-dev", 3)
	if len(recommendations) == 0 {
		t.Fatal("expected remote-dev recommendations")
	}
	found := map[string]bool{}
	for _, recommendation := range recommendations {
		if !providerRecommendationHasFeature(recommendation.Features, FeatureCrabboxSync) &&
			!providerRecommendationHasFeature(recommendation.Features, FeatureArchiveSync) {
			t.Fatalf("remote-dev recommendation cannot sync workspace: %#v", recommendation)
		}
		found[recommendation.Provider] = true
	}
	for _, provider := range []string{"daytona", "morph", "namespace-devbox"} {
		if !found[provider] {
			t.Fatalf("remote-dev recommendations should include %s: %#v", provider, recommendations)
		}
	}
}

func TestProvidersRecommendRemoteDevAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"recommend", "codespaces",
		"--limit", "1",
		"--json",
	})
	if err != nil {
		t.Fatalf("providers recommend codespaces error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 1 || !isRemoteDevProvider(entries[0].Provider) {
		t.Fatalf("codespaces alias entries=%#v", entries)
	}
}

func TestProvidersRecommendPreviewURLPrefersURLBridgeProviders(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "preview-url", 3)
	if len(recommendations) == 0 {
		t.Fatal("expected preview-url recommendations")
	}
	found := map[string]bool{}
	for _, recommendation := range recommendations {
		if !providerRecommendationHasFeature(recommendation.Features, FeatureURLBridge) {
			t.Fatalf("preview-url recommendation lacks url-bridge feature: %#v", recommendation)
		}
		if !containsString(recommendation.Evidence, "preview-url") {
			t.Fatalf("preview-url recommendation lacks preview-url evidence: %#v", recommendation)
		}
		found[recommendation.Provider] = true
	}
	for _, provider := range []string{"e2b", "islo"} {
		if !found[provider] {
			t.Fatalf("preview-url recommendations should include %s: %#v", provider, recommendations)
		}
	}
}

func TestProvidersRecommendPreviewURLAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"recommend", "app-preview",
		"--limit", "1",
		"--json",
	})
	if err != nil {
		t.Fatalf("providers recommend app-preview error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 1 || !providerRecommendationHasFeature(entries[0].Features, FeatureURLBridge) {
		t.Fatalf("app-preview alias entries=%#v", entries)
	}
}

func TestProvidersRecommendPauseResumePrefersResumableProviders(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "pause-resume", 64)
	if len(recommendations) == 0 {
		t.Fatal("expected pause-resume recommendations")
	}
	found := map[string]bool{}
	for _, recommendation := range recommendations {
		if !providerRecommendationHasFeature(recommendation.Features, FeaturePauseResume) {
			t.Fatalf("pause-resume recommendation lacks pause-resume feature: %#v", recommendation)
		}
		found[recommendation.Provider] = true
	}
	for _, provider := range []string{"islo"} {
		if !found[provider] {
			t.Fatalf("pause-resume recommendations should include %s: %#v", provider, recommendations)
		}
	}
}

func TestProvidersRecommendPauseResumeAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"recommend", "resumable-workspace",
		"--limit", "1",
		"--json",
	})
	if err != nil {
		t.Fatalf("providers recommend resumable-workspace error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 1 || !providerRecommendationHasFeature(entries[0].Features, FeaturePauseResume) {
		t.Fatalf("resumable-workspace alias entries=%#v", entries)
	}
}

func TestProvidersRecommendLiveSmokePrefersProvableLifecycle(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "live-smoke", 8)
	if len(recommendations) == 0 {
		t.Fatal("expected live-smoke recommendations")
	}
	foundEvidence := false
	for _, recommendation := range recommendations {
		if recommendation.Kind == ProviderKindServiceControl {
			t.Fatalf("live-smoke should not prefer service-control providers: %#v", recommendation)
		}
		hasSync := providerRecommendationHasFeature(recommendation.Features, FeatureCrabboxSync) ||
			providerRecommendationHasFeature(recommendation.Features, FeatureArchiveSync)
		hasEvidence := providerRecommendationHasFeature(recommendation.Features, FeatureRunProof) ||
			providerRecommendationHasFeature(recommendation.Features, FeatureRunArtifacts) ||
			providerRecommendationHasFeature(recommendation.Features, FeatureRunDownloads) ||
			providerRecommendationHasFeature(recommendation.Features, FeatureURLBridge)
		if hasEvidence {
			foundEvidence = true
		}
		if !hasSync && !hasEvidence {
			t.Fatalf("live-smoke recommendation lacks sync or evidence capability: %#v", recommendation)
		}
	}
	if !foundEvidence {
		t.Fatalf("live-smoke recommendations should include at least one evidence-capable provider: %#v", recommendations)
	}
}

func TestProvidersRecommendLiveSmokeAlias(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"recommend", "provider-smoke",
		"--limit", "1",
		"--json",
	})
	if err != nil {
		t.Fatalf("providers recommend provider-smoke error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 1 || entries[0].Score <= 0 {
		t.Fatalf("provider-smoke alias entries=%#v", entries)
	}
}

func TestProvidersRecommendCIPrefersProofRunner(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "ci-proof", 3)
	if len(recommendations) == 0 {
		t.Fatal("expected ci-proof recommendations")
	}
	if recommendations[0].Provider != "blacksmith-testbox" {
		t.Fatalf("top ci-proof provider=%q recommendations=%v", recommendations[0].Provider, recommendations)
	}
	if !providerRecommendationHasFeature(recommendations[0].Features, FeatureRunProof) {
		t.Fatalf("top ci-proof recommendation missing run-proof feature: %#v", recommendations[0])
	}
}

func TestProvidersRecommendWorkerRuntimeFindsModuleProvider(t *testing.T) {
	recommendations := recommendProvidersForUseCase(providerMatrix(), "worker-runtime", 5)
	if len(recommendations) == 0 {
		t.Fatal("expected worker-runtime recommendations")
	}
	if !providerRecommendationHasString(recommendations[0].Targets, targetWorkerRuntime) {
		t.Fatalf("top worker-runtime recommendation lacks worker target: %#v", recommendations[0])
	}
	if !providerRecommendationHasFeature(recommendations[0].Features, FeatureModuleRun) {
		t.Fatalf("top worker-runtime recommendation lacks module-run feature: %#v", recommendations[0])
	}
}

func TestProvidersRecommendCommandJSONAndLimit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{"recommend", "linux-vm", "--limit", "2", "--json"})
	if err != nil {
		t.Fatalf("providers recommend --json error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) != 2 {
		t.Fatalf("recommendation count=%d want=2 entries=%#v", len(entries), entries)
	}
	for _, entry := range entries {
		if entry.Score <= 0 || len(entry.Reasons) == 0 {
			t.Fatalf("entry missing score/reasons: %#v", entry)
		}
	}
}

func TestProvidersRecommendCommandAppliesFilters(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{
		"recommend", "run-evidence",
		"--category", "delegated-sandbox",
		"--runtime", "managed-sandbox",
		"--reachability", "provider-url",
		"--evidence", "preview-url",
		"--json",
	})
	if err != nil {
		t.Fatalf("providers recommend filtered --json error=%v stderr=%q", err, stderr.String())
	}
	var entries []providerRecommendationEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(entries) == 0 {
		t.Fatal("expected filtered run-evidence recommendations")
	}
	for _, entry := range entries {
		if entry.Category != "delegated-sandbox" || !containsString(entry.Runtime, "managed-sandbox") || !containsString(entry.Reachability, "provider-url") || !containsString(entry.Evidence, "preview-url") {
			t.Fatalf("recommendation escaped filters: %#v", entry)
		}
	}
}

func TestProvidersRecommendFiltersRequireUseCase(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{"recommend", "--kind", "ssh-lease"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("providers recommend filter without use case error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(err.Error(), "provider recommendation filters require a use case") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProvidersRecommendFilteredNoMatch(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{"recommend", "run-evidence", "--workspace", "checkpoint"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("providers recommend filtered no-match error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(err.Error(), `no providers matched use case "run-evidence" with the requested filters`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProvidersRecommendRejectsUnknownUseCase(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{"recommend", "moon-base"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("providers recommend unknown error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}

func containsFeature(values []Feature, want Feature) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
