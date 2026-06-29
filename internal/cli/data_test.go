package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigDataRuns(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`dataRuns:
  normalize-events:
    provider: aws
    target: linux
    ttl: 90m
    source:
      kind: s3
      mode: read
      uri: s3://example-raw/events/
      watermark: updated_at
    sink:
      kind: s3
      mode: write-staging
      uri: s3://example-clean/events-staging/
    identity:
      aws:
        instanceProfile: crabbox-data-normalize-events
    policy:
      requireDryRun: true
      maxBytes: 500GiB
      maxRows: 200000000
      piiLogging: forbid
      egress:
        allow:
          - s3.amazonaws.com
    manifest:
      path: crabbox-data/custom.json
      required: true
    shell: true
    command: python pipelines/normalize_events.py
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	run := cfg.DataRuns["normalize-events"]
	if run.Job.Provider != "aws" || run.Job.Target != targetLinux || run.Job.TTL.String() != "1h30m0s" || !run.Job.Shell {
		t.Fatalf("job fields not loaded: %#v", run.Job)
	}
	if run.Source.Kind != "s3" || run.Source.URI != "s3://example-raw/events/" || run.Source.Watermark != "updated_at" {
		t.Fatalf("source not loaded: %#v", run.Source)
	}
	if run.Sink.Mode != "write-staging" || run.Sink.URI != "s3://example-clean/events-staging/" {
		t.Fatalf("sink not loaded: %#v", run.Sink)
	}
	if run.Identity.AWS.InstanceProfile != "crabbox-data-normalize-events" {
		t.Fatalf("identity not loaded: %#v", run.Identity)
	}
	if !run.Policy.RequireDryRun || run.Policy.MaxBytes != "500GiB" || run.Policy.MaxRows != 200000000 || len(run.Policy.EgressAllow) != 1 {
		t.Fatalf("policy not loaded: %#v", run.Policy)
	}
	if run.Manifest.Path != "crabbox-data/custom.json" || !run.Manifest.Required || !run.Manifest.RequiredSet {
		t.Fatalf("manifest not loaded: %#v", run.Manifest)
	}
}

func TestDataPlanPrintsEffectivePolicyAndRunCommand(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`dataRuns:
  normalize-events:
    provider: aws
    target: linux
    ttl: 90m
    source:
      kind: s3
      uri: s3://example-raw/events/
    sink:
      kind: s3
      uri: s3://example-clean/events-staging/
    policy:
      requireDryRun: true
      maxRows: 100
    command: python pipelines/normalize_events.py
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"data", "plan", "normalize-events"}); err != nil {
		t.Fatalf("data plan failed: %v\nstderr=%s", err, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{
		"data run normalize-events mode=execute status=poc",
		"policy requireDryRun=true enforced=execute-gate",
		"policy maxRows=100 declared=only",
		"CRABBOX_DATA_MANIFEST=crabbox-data/normalize-events/execute-manifest.json",
		"crabbox run --provider aws --target linux --ttl 1h30m0s --id '<lease>' --no-hydrate --label data:normalize-events:execute --allow-env",
		"--require-artifact crabbox-data/normalize-events/execute-manifest.json",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("plan output missing %q:\n%s", want, got)
		}
	}
}

func TestDataRunRejectsExecuteWhenDryRunRequired(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`dataRuns:
  normalize-events:
    source:
      kind: s3
      uri: s3://example-raw/events/
    sink:
      kind: s3
      uri: s3://example-clean/events-staging/
    policy:
      requireDryRun: true
    command: python pipelines/normalize_events.py
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"data", "run", "normalize-events"})
	if err == nil || !strings.Contains(err.Error(), "requires dry-run first") {
		t.Fatalf("data run err=%v, want requireDryRun gate", err)
	}
}

func TestDataRunArgsRequireAndDownloadExecuteManifest(t *testing.T) {
	cfg := defaultConfig()
	run := DataRunConfig{
		Job: JobConfig{
			Provider: "aws",
			Target:   targetLinux,
			Command:  "python pipelines/normalize_events.py",
		},
		Source: DataRunEndpointConfig{Kind: "s3", URI: "s3://example-raw/events/"},
		Sink:   DataRunEndpointConfig{Kind: "s3", URI: "s3://example-clean/events-staging/"},
	}
	args := dataRunRunArgs(cfg, "normalize-events", run, "blue-lobster", true, dataRunModeExecute, "crabbox-data/normalize-events/execute-manifest.json", "/tmp/manifest.json")
	got := strings.Join(args, " ")
	for _, want := range []string{
		"--allow-env",
		"CRABBOX_DATA_MANIFEST",
		"--require-artifact crabbox-data/normalize-events/execute-manifest.json",
		"--download crabbox-data/normalize-events/execute-manifest.json=/tmp/manifest.json",
		"-- python pipelines/normalize_events.py",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("run args missing %q:\n%s", want, got)
		}
	}
}

func TestValidateDataRunExecutionSupportRejectsDelegatedWithoutDownloads(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "e2b"
	run := DataRunConfig{}

	err := validateDataRunExecutionSupport(cfg, "normalize-events", run, dataRunModeExecute)
	if err == nil || !strings.Contains(err.Error(), "requires manifest download support") {
		t.Fatalf("execute support err=%v, want delegated manifest download rejection", err)
	}
	if err := validateDataRunExecutionSupport(cfg, "normalize-events", run, dataRunModeDryRun); err != nil {
		t.Fatalf("dry-run should not require manifest download support: %v", err)
	}
	run.Manifest.Required = false
	run.Manifest.RequiredSet = true
	if err := validateDataRunExecutionSupport(cfg, "normalize-events", run, dataRunModeExecute); err != nil {
		t.Fatalf("manifest.required=false should not require manifest download support: %v", err)
	}
}

func TestValidateDataRunExecutionSupportRejectsIgnoredManifestPath(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	run := DataRunConfig{Manifest: DataRunManifestConfig{Path: ".crabbox/data/manifest.json"}}

	err := validateDataRunExecutionSupport(cfg, "normalize-events", run, dataRunModeExecute)
	if err == nil || !strings.Contains(err.Error(), "manifest.path must not be under .crabbox") {
		t.Fatalf("execute support err=%v, want ignored manifest path rejection", err)
	}
}

func TestDataRunLeaseSlugIsRequestedSlugSafe(t *testing.T) {
	slug := dataRunLeaseSlug("Normalize Events With A Very Long Name That Needs Truncation")
	if slug != normalizeLeaseSlug(slug) {
		t.Fatalf("slug %q is not normalized", slug)
	}
	if len(slug) > maxRequestedLeaseSlugLength {
		t.Fatalf("slug length=%d, want <= %d: %q", len(slug), maxRequestedLeaseSlugLength, slug)
	}
	if !strings.HasPrefix(slug, "data-normalize-events") {
		t.Fatalf("slug %q missing data-run prefix", slug)
	}
}

func TestValidateDataRunManifestFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"dataRun":"normalize-events","mode":"execute","source":{"kind":"s3"},"sink":{"kind":"s3"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateDataRunManifestFile(path, "normalize-events", dataRunModeExecute); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"dataRun":"other","mode":"execute","source":{},"sink":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateDataRunManifestFile(path, "normalize-events", dataRunModeExecute); err == nil || !strings.Contains(err.Error(), `dataRun="other"`) {
		t.Fatalf("mismatched manifest err=%v", err)
	}
}
