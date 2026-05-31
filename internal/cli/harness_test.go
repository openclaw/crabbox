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
