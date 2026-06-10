package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func init() {
	RegisterProvider(dataRunPolicyTestProvider{})
}

type dataRunPolicyTestProvider struct{}

func (dataRunPolicyTestProvider) Name() string      { return "data-run-policy-test" }
func (dataRunPolicyTestProvider) Aliases() []string { return nil }
func (dataRunPolicyTestProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:        "data-run-policy-test",
		Kind:        ProviderKindDelegatedRun,
		Targets:     []TargetSpec{{OS: targetLinux}},
		Coordinator: CoordinatorNever,
	}
}
func (dataRunPolicyTestProvider) RegisterFlags(*flag.FlagSet, Config) any {
	return noProviderFlags{}
}
func (dataRunPolicyTestProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p dataRunPolicyTestProvider) Configure(Config, Runtime) (Backend, error) {
	return dataRunPolicyTestBackend{spec: p.Spec()}, nil
}

type dataRunPolicyTestBackend struct {
	spec ProviderSpec
}

func (b dataRunPolicyTestBackend) Spec() ProviderSpec { return b.spec }
func (b dataRunPolicyTestBackend) DataRunPolicy(context.Context, DataRunPolicyRequest) (DataRunPolicyResult, error) {
	return DataRunPolicyResult{Enforcement: "unsupported"}, nil
}

func TestLoadConfigDataRuns(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`dataRuns:
  import-users:
    provider: run-env-profile-test
    target: linux
    class: beast
    shell: true
    noSync: true
    command: ./scripts/import-users.sh
    manifest: reports/data/manifest.json
    requiredArtifacts:
      - reports/data/quality.json
    artifactGlobs:
      - reports/data/**
    junit:
      - reports/data/junit.xml
    downloads:
      - reports/data/quality.json=.crabbox/data-runs/import-users/quality.json
    policy:
      sourceIdentity: service-account:data-reader
      sinkIdentity: service-account:data-writer
      egress: restricted
      promotion: manual
      enforcement: declared-only
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	run := cfg.DataRuns["import-users"]
	if run.Provider != "run-env-profile-test" || run.Target != "linux" || run.Class != "beast" || !run.Shell || !run.NoSync {
		t.Fatalf("data run target/options not loaded: %#v", run)
	}
	if run.Manifest != "reports/data/manifest.json" || len(run.RequiredArtifacts) != 1 || len(run.ArtifactGlobs) != 1 || len(run.JUnit) != 1 || len(run.Downloads) != 1 {
		t.Fatalf("data run evidence fields not loaded: %#v", run)
	}
	if run.Policy.SourceIdentity == "" || run.Policy.SinkIdentity == "" || run.Policy.Enforcement != "declared-only" {
		t.Fatalf("data run policy not loaded: %#v", run.Policy)
	}
}

func TestDataRunDryRunBuildsRunAndValidationCommands(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`dataRuns:
  import-users:
    provider: run-env-profile-test
    noSync: true
    shell: true
    command: mkdir -p reports/data && ./scripts/import-users.sh
    manifest: reports/data/manifest.json
    requiredArtifacts:
      - reports/data/quality.json
    artifactGlobs:
      - reports/data/**
    policy:
      sourceIdentity: service-account:data-reader
      sinkIdentity: service-account:data-writer
      egress: restricted
      promotion: manual
      enforcement: declared-only
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run(context.Background(), []string{"data", "run", "--dry-run", "--id", "blue-lobster", "--no-hydrate", "import-users"})
	if err != nil {
		t.Fatalf("data run dry-run failed: %v\nstderr=%s", err, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{
		"# data policy source=service-account:data-reader sink=service-account:data-writer egress=restricted promotion=manual enforcement=declared-only",
		"crabbox run --provider run-env-profile-test --id blue-lobster --no-hydrate --no-sync --label data:import-users --require-artifact reports/data/manifest.json --require-artifact reports/data/quality.json --artifact-glob 'reports/data/**' --download reports/data/manifest.json=.crabbox/data-runs/import-users/manifest.json --shell -- 'mkdir -p reports/data && ./scripts/import-users.sh'",
		"crabbox data validate-manifest --name import-users --summary-output .crabbox/data-runs/import-users/summary.json .crabbox/data-runs/import-users/manifest.json",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, got)
		}
	}
}

func TestDataRunDryRunUsesCommandArgsWithoutShell(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`dataRuns:
  wandb-import:
    provider: wandb
    noSync: true
    commandArgs:
      - sh
      - -lc
      - mkdir -p reports/data && python run.py
    manifest: reports/data/manifest.json
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run(context.Background(), []string{"data", "run", "--dry-run", "wandb-import"})
	if err != nil {
		t.Fatalf("data run dry-run failed: %v\nstderr=%s", err, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "crabbox run --provider wandb --no-sync --label data:wandb-import --require-artifact reports/data/manifest.json --download reports/data/manifest.json=.crabbox/data-runs/wandb-import/manifest.json -- sh -lc 'mkdir -p reports/data && python run.py'") {
		t.Fatalf("dry-run output did not preserve commandArgs:\n%s", got)
	}
	if strings.Contains(got, "--shell") {
		t.Fatalf("commandArgs must not use --shell:\n%s", got)
	}
}

func TestDataRunExecutesAndWritesSummaryE2E(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	t.Chdir(dir)

	sshPath := filepath.Join(dir, "ssh")
	remoteRoot := filepath.Join(dir, "remote-root")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, sshPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
cmd=""
for arg do cmd="$arg"; done
case "$cmd" in
  mkdir\ -p*|cd\ *|bash\ -lc*) exec sh -c "$cmd" ;;
esac
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	t.Setenv("CRABBOX_FAKE_SSH_PORT", sshPort)
	t.Setenv("CRABBOX_WORK_ROOT", remoteRoot)
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`dataRuns:
  import-users:
    provider: run-env-profile-test
    noSync: true
    shell: true
    command: |
      mkdir -p reports/data &&
      printf '%s\n' '{"schemaVersion":"crabbox.data-run.v1","status":"success","outputs":[{"name":"warehouse.users","rows":3,"bytes":12}],"summary":{"provider":"fake","kind":"test"}}' > reports/data/manifest.json &&
      : > reports/data/empty.marker
    manifest: reports/data/manifest.json
    requiredArtifacts:
      - reports/data/empty.marker
    policy:
      enforcement: declared-only
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err = app.Run(context.Background(), []string{"data", "run", "import-users"})
	if err != nil {
		t.Fatalf("data run failed: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"required artifact reports/data/manifest.json matched=1",
		"required artifact reports/data/empty.marker matched=1",
		"data run import-users manifest ok status=success inputs=0 outputs=1",
	} {
		if !strings.Contains(stdout.String()+stderr.String(), want) {
			t.Fatalf("missing %q\nstdout=%s\nstderr=%s", want, stdout.String(), stderr.String())
		}
	}
	summaryData, err := os.ReadFile(filepath.Join(dir, ".crabbox", "data-runs", "import-users", "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary DataRunSummary
	if err := json.Unmarshal(summaryData, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Status != "success" || summary.Outputs != 1 || summary.OutputRows != 3 || summary.OutputBytes != 12 {
		t.Fatalf("summary=%#v", summary)
	}
}

func TestDataRunRejectsUnsafeLocalOutputs(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`dataRuns:
  unsafe:
    provider: run-env-profile-test
    noSync: true
    command: "true"
    downloads:
      - reports/data/manifest.json=../outside.json
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run(context.Background(), []string{"data", "run", "--dry-run", "unsafe"})
	if err == nil || !strings.Contains(err.Error(), "safe repo-relative path") {
		t.Fatalf("err=%v, want safe repo-relative rejection", err)
	}
}

func TestDataValidateManifestWritesBoundedSummary(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	summaryPath := filepath.Join(dir, "summary.json")
	writeDataRunManifest(t, manifestPath, map[string]any{
		"schemaVersion": dataRunManifestSchema,
		"name":          "import-users",
		"status":        "success",
		"inputs": []map[string]any{{
			"name":     "source.users",
			"identity": "service-account:data-reader",
		}},
		"outputs": []map[string]any{{
			"name":     "warehouse.users",
			"identity": "service-account:data-writer",
			"rows":     12,
			"bytes":    2048,
		}},
		"summary": map[string]any{
			"tables":  1,
			"changed": true,
		},
		"artifacts": []map[string]any{{
			"path":  "reports/data/manifest.json",
			"kind":  "manifest",
			"bytes": 512,
		}},
		"policy": map[string]any{
			"sourceIdentity": "service-account:data-reader",
			"sinkIdentity":   "service-account:data-writer",
			"egress":         "restricted",
			"enforcement":    "declared-only",
		},
		"promotion": map[string]any{
			"mode":        "manual",
			"target":      "staging-to-prod",
			"enforcement": "declared-only",
		},
	})

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run(context.Background(), []string{"data", "validate-manifest", "--name", "import-users", "--summary-output", summaryPath, "--json", manifestPath})
	if err != nil {
		t.Fatalf("validate-manifest failed: %v\nstderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"schemaVersion": "crabbox.data-run-summary.v1"`) {
		t.Fatalf("json summary not printed:\n%s", stdout.String())
	}
	data, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatal(err)
	}
	var summary DataRunSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Name != "import-users" || summary.Status != "success" || summary.Inputs != 1 || summary.Outputs != 1 || summary.OutputRows != 12 || summary.OutputBytes != 2048 || summary.Artifacts != 1 {
		t.Fatalf("unexpected summary: %#v", summary)
	}
	if summary.Policy.Enforcement != "declared-only" || summary.Promotion.Mode != "manual" {
		t.Fatalf("policy/promotion missing from summary: %#v %#v", summary.Policy, summary.Promotion)
	}
}

func TestDataValidateManifestRejectsUnknownFields(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	writeDataRunManifest(t, manifestPath, map[string]any{
		"schemaVersion": dataRunManifestSchema,
		"status":        "success",
		"outputs": []map[string]any{{
			"name": "warehouse.users",
		}},
		"examples": []map[string]any{{"email": "alice@example.com"}},
	})

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run(context.Background(), []string{"data", "validate-manifest", manifestPath})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err=%v, want unknown field rejection\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
}

func TestDataValidateManifestRejectsTrailingContent(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	valid := `{"schemaVersion":"crabbox.data-run.v1","status":"success","outputs":[{"name":"warehouse.users"}]}`
	if err := os.WriteFile(manifestPath, []byte(valid+"\nrawRows=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run(context.Background(), []string{"data", "validate-manifest", manifestPath})
	if err == nil || !strings.Contains(err.Error(), "trailing content") {
		t.Fatalf("err=%v, want trailing content rejection\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
}

func TestDataValidateManifestAllowsTrailingWhitespace(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	valid := `{"schemaVersion":"crabbox.data-run.v1","status":"success","outputs":[{"name":"warehouse.users"}]}`
	if err := os.WriteFile(manifestPath, []byte(valid+"\n\t \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"data", "validate-manifest", manifestPath}); err != nil {
		t.Fatalf("err=%v, want trailing whitespace accepted\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
}

func TestDataValidateManifestRejectsEnforcedPolicyWithoutHook(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	writeDataRunManifest(t, manifestPath, map[string]any{
		"schemaVersion": dataRunManifestSchema,
		"status":        "success",
		"outputs": []map[string]any{{
			"name": "warehouse.users",
		}},
		"policy": map[string]any{
			"enforcement": "enforced",
		},
	})

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run(context.Background(), []string{"data", "validate-manifest", manifestPath})
	if err == nil || !strings.Contains(err.Error(), "declared-only or unsupported") {
		t.Fatalf("err=%v, want enforced rejection\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
}

func TestDataRunProviderPolicyUsesSelectedRunProvider(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "run-env-profile-test"
	run := DataRunConfig{Provider: "data-run-policy-test"}
	result, err := dataRunProviderPolicy(context.Background(), dataRunPolicyConfig(cfg, run), DataRunPolicyConfig{
		SourceIdentity: "declared-reader",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Enforcement != "unsupported" {
		t.Fatalf("enforcement=%q, want provider-selected unsupported", result.Enforcement)
	}
}

func TestDataRunRecordedRunIDParsesDetailsAndTimingJSON(t *testing.T) {
	if got := dataRunRecordedRunID("run details provider=aws lease=cbx slug=blue run=run_123 type=t3\n"); got != "run_123" {
		t.Fatalf("details run id=%q", got)
	}
	line := `{"provider":"aws","runId":"run_456","exitCode":0}`
	if got := dataRunRecordedRunID(line + "\n"); got != "run_456" {
		t.Fatalf("json run id=%q", got)
	}
	if got := dataRunRecordedRunID("run details provider=aws run=-\n"); got != "" {
		t.Fatalf("empty run id=%q", got)
	}
}

func TestDataValidateManifestEnforcesSizeBounds(t *testing.T) {
	baseManifest := func() map[string]any {
		return map[string]any{
			"schemaVersion": dataRunManifestSchema,
			"status":        "success",
			"outputs": []map[string]any{{
				"name": "warehouse.users",
			}},
		}
	}
	tests := []struct {
		name     string
		manifest func(t *testing.T, path string)
		want     string
	}{
		{
			name: "manifest file over byte cap",
			manifest: func(t *testing.T, path string) {
				manifest := baseManifest()
				manifest["name"] = "import-users"
				data, err := json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				padding := bytes.Repeat([]byte(" "), dataRunMaxManifestBytes)
				if err := os.WriteFile(path, append(data, padding...), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "exceeds 65536 bytes",
		},
		{
			name: "summary string over byte cap",
			manifest: func(t *testing.T, path string) {
				manifest := baseManifest()
				manifest["summary"] = map[string]any{
					"note": strings.Repeat("x", dataRunMaxStringBytes+1),
				}
				writeDataRunManifest(t, path, manifest)
			},
			want: "exceeds 2048 bytes",
		},
		{
			name: "outputs over collection cap",
			manifest: func(t *testing.T, path string) {
				manifest := baseManifest()
				outputs := make([]map[string]any, 0, dataRunMaxCollectionSize+1)
				for i := 0; i <= dataRunMaxCollectionSize; i++ {
					outputs = append(outputs, map[string]any{"name": "warehouse.users"})
				}
				manifest["outputs"] = outputs
				writeDataRunManifest(t, path, manifest)
			},
			want: "too many entries",
		},
		{
			name: "summary over key cap",
			manifest: func(t *testing.T, path string) {
				manifest := baseManifest()
				summary := map[string]any{}
				for i := 0; i <= dataRunMaxSummaryKeys; i++ {
					summary["metric"+strings.Repeat("a", i)] = i
				}
				manifest["summary"] = summary
				writeDataRunManifest(t, path, manifest)
			},
			want: "at most 32 keys",
		},
		{
			name: "summary value must be scalar",
			manifest: func(t *testing.T, path string) {
				manifest := baseManifest()
				manifest["summary"] = map[string]any{
					"details": map[string]any{"tables": 1},
				}
				writeDataRunManifest(t, path, manifest)
			},
			want: "must be a scalar",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnv(t)
			manifestPath := filepath.Join(t.TempDir(), "manifest.json")
			test.manifest(t, manifestPath)
			var stdout, stderr bytes.Buffer
			app := App{Stdout: &stdout, Stderr: &stderr}
			err := app.Run(context.Background(), []string{"data", "validate-manifest", manifestPath})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %q\nstdout=%s\nstderr=%s", err, test.want, stdout.String(), stderr.String())
			}
		})
	}
}

func TestDataValidateManifestRejectsUnsafeProofFields(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	writeDataRunManifest(t, manifestPath, map[string]any{
		"schemaVersion": dataRunManifestSchema,
		"status":        "success",
		"outputs": []map[string]any{{
			"name": "warehouse.users",
		}},
		"summary": map[string]any{
			"rawRows": []map[string]any{{"email": "alice@example.com"}},
		},
	})

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run(context.Background(), []string{"data", "validate-manifest", manifestPath})
	if err == nil || !strings.Contains(err.Error(), "unsafe proof field") {
		t.Fatalf("err=%v, want unsafe proof field\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
}

func writeDataRunManifest(t *testing.T, path string, manifest map[string]any) {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
