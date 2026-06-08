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
	for i := range entries {
		if entries[i].Provider == "aws" {
			aws = &entries[i]
		}
		if entries[i].Provider == "incus" {
			incus = &entries[i]
		}
	}
	if aws == nil {
		t.Fatal("aws provider not found")
	}
	if incus == nil {
		t.Fatal("incus provider not found")
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
	if !strings.Contains(text, "incus\n") {
		t.Fatalf("providers output missing incus:\n%s", text)
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
