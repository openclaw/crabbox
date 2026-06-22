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
	if !containsString(aws.Targets, targetLinux) || !containsString(aws.Targets, targetMacOS) {
		t.Fatalf("aws targets=%v", aws.Targets)
	}
	if !containsFeature(aws.Features, FeatureSSH) || !containsFeature(aws.Features, FeatureDesktop) {
		t.Fatalf("aws features=%v", aws.Features)
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
	if moduleRuntime == nil {
		t.Fatal("module-runtime-test provider not found")
	}
	if moduleRuntime.Kind != ProviderKindDelegatedRun || !containsString(moduleRuntime.Targets, targetWorkerRuntime) {
		t.Fatalf("module-runtime-test kind/targets=%q/%v", moduleRuntime.Kind, moduleRuntime.Targets)
	}
	if !containsFeature(moduleRuntime.Features, FeatureModuleRun) {
		t.Fatalf("module-runtime-test features=%v missing %s", moduleRuntime.Features, FeatureModuleRun)
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
	if !containsString(localContainer.Workspace, "checkpoint") || !containsString(localContainer.Workspace, "fork") {
		t.Fatalf("local-container workspace=%v", localContainer.Workspace)
	}
	for _, capability := range []string{"checkpoint", "fork", "restore", "snapshot-ref"} {
		if !containsString(parallels.Workspace, capability) {
			t.Fatalf("parallels workspace=%v missing %s", parallels.Workspace, capability)
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
		if entry.Provider == "parallels" && !containsString(entry.Workspace, "snapshot-ref") {
			t.Fatalf("parallels json missing workspace snapshot-ref: %#v", entry)
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
	for _, want := range []string{"aws\n", "  family: aws\n", "  kind: ssh-lease\n", "  features: "} {
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
}

func TestProvidersRecommendListsUseCases(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{"recommend"})
	if err != nil {
		t.Fatalf("providers recommend error=%v stderr=%q", err, stderr.String())
	}
	text := stdout.String()
	for _, want := range []string{"provider recommendation use cases:", "ci-proof", "agent-sandbox", "versioned-workspace", "worker-runtime"} {
		if !strings.Contains(text, want) {
			t.Fatalf("providers recommend output missing %q:\n%s", want, text)
		}
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
