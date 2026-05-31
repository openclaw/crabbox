package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadHarnessDocumentParsesFrontmatterAndPlan(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(plan, []byte("plan body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "HARNESS.md")
	content := `---
version: "1"
template: regression
job: full-ci
plan_file: plan.md
scope:
  - internal/cli/**
compliance:
  require_plan: true
  require_junit: true
  required_artifacts:
    - junit
---

## Plan

Run the regression suite.
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := loadHarnessDocument(HarnessConfig{Path: path}, Repo{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Config.Template != "regression" || doc.Config.Job != "full-ci" || doc.Config.PlanFile != "plan.md" {
		t.Fatalf("unexpected config: %#v", doc.Config)
	}
	if !doc.Config.Compliance.RequirePlan || !doc.Config.Compliance.RequireJUnit || len(doc.Config.Compliance.RequiredArtifacts) != 1 {
		t.Fatalf("unexpected compliance: %#v", doc.Config.Compliance)
	}
	if doc.HarnessHash == "" || doc.PlanHash == "" {
		t.Fatalf("expected hashes: harness=%q plan=%q", doc.HarnessHash, doc.PlanHash)
	}
	if !strings.Contains(doc.Body, "Run the regression suite") {
		t.Fatalf("body not parsed: %q", doc.Body)
	}
}

func TestLoadHarnessDocumentRejectsUnsafePlanFile(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.md")
	if err := os.WriteFile(secret, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		planFile string
	}{
		{name: "absolute", planFile: secret},
		{name: "parent relative", planFile: "../secret.md"},
		{name: "home relative", planFile: "~/secret.md"},
		{name: "windows absolute", planFile: `C:\Users\secret.md`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harnessDir := filepath.Join(dir, tc.name)
			if err := os.MkdirAll(harnessDir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(harnessDir, "HARNESS.md")
			content := "---\nplan_file: '" + strings.ReplaceAll(tc.planFile, "'", "''") + "'\n---\nbody\n"
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := loadHarnessDocument(HarnessConfig{Path: path}, Repo{Root: dir})
			if err == nil || !strings.Contains(err.Error(), "harness plan_file must be repo-relative") {
				t.Fatalf("expected unsafe plan_file error, got %v", err)
			}
		})
	}
}

func TestHarnessValidateRejectsUnknownFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "HARNESS.md")
	if err := os.WriteFile(path, []byte("---\nunknown: true\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadHarnessDocument(HarnessConfig{Path: path}, Repo{Root: dir}); err == nil || !strings.Contains(err.Error(), "unknown frontmatter key") {
		t.Fatalf("expected unknown key error, got %v", err)
	}
}

func TestHarnessValidateRejectsUnknownNestedFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "HARNESS.md")
	if err := os.WriteFile(path, []byte("---\ncompliance:\n  require_junti: true\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadHarnessDocument(HarnessConfig{Path: path}, Repo{Root: dir}); err == nil || !strings.Contains(err.Error(), "unknown frontmatter key \"compliance.require_junti\"") {
		t.Fatalf("expected nested unknown key error, got %v", err)
	}
}

func TestHarnessValidateJSONCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "HARNESS.md")
	if err := os.WriteFile(path, []byte("---\nversion: \"1\"\ntemplate: smoke\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.harnessValidate(context.Background(), []string{"--json", path}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["ok"] != true || got["template"] != "smoke" {
		t.Fatalf("unexpected json: %#v", got)
	}
}

func TestHarnessValidateJSONFlagAfterPathFailsUsage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "HARNESS.md")
	if err := os.WriteFile(path, []byte("---\nversion: \"1\"\ntemplate: smoke\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.harnessValidate(context.Background(), []string{path, "--json"}); err == nil || !strings.Contains(err.Error(), "usage: crabbox harness validate [--json] HARNESS.md") {
		t.Fatalf("expected positional flag usage error, got %v", err)
	}
}

func TestBuildHarnessComplianceReportFailsMissingRequiredEvidence(t *testing.T) {
	doc := &harnessDocument{
		Config: HarnessConfig{
			Path: "HARNESS.md",
			Compliance: HarnessComplianceConfig{
				RequirePlan:       true,
				RequireJUnit:      true,
				RequiredArtifacts: []string{"junit"},
			},
		},
		HarnessHash: "abc",
	}
	report := buildHarnessComplianceReport(doc, HarnessMetadata{Path: "HARNESS.md", HarnessHash: "abc"}, 0, "go test ./...", "", nil, nil)
	if report.Status != "failed" {
		t.Fatalf("expected failed, got %#v", report)
	}
	if len(report.Missing) != 3 {
		t.Fatalf("expected three missing evidence entries, got %#v", report.Missing)
	}
}

func TestBuildHarnessComplianceReportMatchesRequiredArtifacts(t *testing.T) {
	doc := &harnessDocument{
		Config: HarnessConfig{
			Path: "HARNESS.md",
			Compliance: HarnessComplianceConfig{
				RequiredArtifacts: []string{"screenshots", "proof.md"},
			},
		},
		HarnessHash: "abc",
	}
	report := buildHarnessComplianceReport(
		doc,
		HarnessMetadata{Path: "HARNESS.md", HarnessHash: "abc"},
		0,
		"pnpm test",
		"",
		[]runArtifact{
			{Kind: "artifact-glob", Path: "/tmp/artifacts.tgz"},
			{Kind: "screenshots", Path: "/tmp/screenshots.tgz"},
			{Kind: "proof", Path: "/tmp/proof.md"},
		},
		nil,
	)
	if report.Status != "passed" || len(report.Missing) != 0 {
		t.Fatalf("expected matching artifacts to pass, got %#v", report)
	}
}

func TestBuildHarnessComplianceReportMatchesRequiredJUnitArtifact(t *testing.T) {
	doc := &harnessDocument{
		Config: HarnessConfig{
			Path: "HARNESS.md",
			Compliance: HarnessComplianceConfig{
				RequiredArtifacts: []string{"junit", "results.xml"},
			},
		},
		HarnessHash: "abc",
	}
	report := buildHarnessComplianceReport(
		doc,
		HarnessMetadata{Path: "HARNESS.md", HarnessHash: "abc"},
		0,
		"pnpm test",
		"",
		nil,
		&TestResultSummary{Format: "junit", Files: []string{"/work/results.xml"}},
	)
	if report.Status != "passed" || len(report.Missing) != 0 {
		t.Fatalf("expected junit artifact evidence to pass, got %#v", report)
	}
}

func TestBuildHarnessComplianceReportFailsSpecificMissingArtifact(t *testing.T) {
	doc := &harnessDocument{
		Config: HarnessConfig{
			Path: "HARNESS.md",
			Compliance: HarnessComplianceConfig{
				RequiredArtifacts: []string{"screenshots"},
			},
		},
		HarnessHash: "abc",
	}
	report := buildHarnessComplianceReport(
		doc,
		HarnessMetadata{Path: "HARNESS.md", HarnessHash: "abc"},
		0,
		"pnpm test",
		"",
		[]runArtifact{{Kind: "artifact-glob", Path: "/tmp/artifacts.tgz"}},
		nil,
	)
	if report.Status != "failed" || len(report.Missing) != 1 || report.Missing[0] != "artifact screenshots" {
		t.Fatalf("expected specific missing artifact, got %#v", report)
	}
}

func TestWriteHarnessLocalEvidenceWritesFilesAndFailedMetadata(t *testing.T) {
	dir := t.TempDir()
	doc := &harnessDocument{
		Config: HarnessConfig{
			Path: "HARNESS.md",
			Compliance: HarnessComplianceConfig{
				RequiredArtifacts: []string{"screenshots"},
			},
		},
		Content:     []byte("# Harness\n"),
		HarnessHash: "abc",
	}
	grounding := harnessGrounding{
		Version: "1",
		Harness: HarnessMetadata{
			Path:        "HARNESS.md",
			HarnessHash: "abc",
			Index:       "light",
			Status:      "pending",
		},
	}
	artifacts, meta, err := writeHarnessLocalEvidence(
		dir,
		"run_123",
		"",
		doc,
		grounding,
		0,
		"pnpm test",
		"",
		[]runArtifact{{Kind: "artifact-glob", Path: "/tmp/artifacts.tgz"}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil || meta.Status != "failed" {
		t.Fatalf("expected failed harness metadata, got %#v", meta)
	}
	expectedKinds := []string{"harness", "grounding", "compliance-json", "compliance-report"}
	if len(artifacts) != len(expectedKinds) {
		t.Fatalf("expected harness artifacts, got %#v", artifacts)
	}
	for i, kind := range expectedKinds {
		if artifacts[i].Kind != kind {
			t.Fatalf("artifact %d kind=%q want %q", i, artifacts[i].Kind, kind)
		}
	}
	reportPath := filepath.Join(dir, ".crabbox", "runs", "run_123", "compliance-report.json")
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report harnessComplianceReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "failed" || len(report.Missing) != 1 || report.Missing[0] != "artifact screenshots" {
		t.Fatalf("unexpected report: %#v", report)
	}
	if _, err := os.Stat(filepath.Join(dir, ".crabbox", "runs", "run_123", "harness.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".crabbox", "runs", "run_123", "grounding.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".crabbox", "runs", "run_123", "compliance-report.md")); err != nil {
		t.Fatal(err)
	}
}

func TestBuildHarnessComplianceReportFailsJUnitFailures(t *testing.T) {
	doc := &harnessDocument{
		Config: HarnessConfig{
			Path: "HARNESS.md",
			Compliance: HarnessComplianceConfig{
				RequireJUnit: true,
			},
		},
		HarnessHash: "abc",
	}
	report := buildHarnessComplianceReport(doc, HarnessMetadata{Path: "HARNESS.md", HarnessHash: "abc"}, 0, "go test ./...", "", nil, &TestResultSummary{Failures: 1})
	if report.Status != "failed" || len(report.Missing) != 1 || report.Missing[0] != "passing junit evidence" {
		t.Fatalf("expected junit failure compliance failure, got %#v", report)
	}
}
