package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDoctorPrepareCheckBlacksmithRequiresRunnableWorkflow(t *testing.T) {
	check := doctorPrepareCheckWithConfig(t, `
provider: blacksmith-testbox
actions:
  workflow: .github/workflows/crabbox-hydrate.yml
  job: hydrate
`)
	if check.Status != "missing" {
		t.Fatalf("status = %q, want missing", check.Status)
	}
	if !doctorPrepareChecksFail([]doctorJSONCheck{check}) {
		t.Fatalf("missing Blacksmith workflow did not fail prepare checks")
	}
	if check.Details["hydrate"] != "missing" {
		t.Fatalf("hydrate detail = %q, want missing", check.Details["hydrate"])
	}
}

func TestDoctorPrepareCheckBlacksmithFallsBackToNonGenericActionsWorkflow(t *testing.T) {
	check := doctorPrepareCheckWithConfig(t, `
provider: blacksmith-testbox
actions:
  workflow: .github/workflows/ci-check-testbox.yml
  job: check
`)
	if check.Status != "ok" {
		t.Fatalf("status = %q, want ok; message=%s", check.Status, check.Message)
	}
	if check.Details["workflow"] != ".github/workflows/ci-check-testbox.yml" {
		t.Fatalf("workflow detail = %q", check.Details["workflow"])
	}
	if check.Details["job"] != "check" {
		t.Fatalf("job detail = %q", check.Details["job"])
	}
}

func TestDoctorPrepareCheckDoesNotReuseExplicitTypeAcrossProviders(t *testing.T) {
	check := doctorPrepareCheckWithConfigForProvider(t, "aws", `
provider: gcp
serverType: c4-standard-192
actions:
  workflow: hydrate.yml
  job: hydrate
`)
	if check.Details["type"] == "c4-standard-192" {
		t.Fatalf("aws prepare reused gcp serverType: %#v", check.Details)
	}
}

func doctorPrepareCheckWithConfig(t *testing.T, body string) doctorJSONCheck {
	return doctorPrepareCheckWithConfigForProvider(t, "blacksmith-testbox", body)
}

func doctorPrepareCheckWithConfigForProvider(t *testing.T, provider, body string) doctorJSONCheck {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	path := filepath.Join(dir, ".crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", path)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	checks := doctorPrepareCheck(provider)
	if len(checks) != 1 {
		t.Fatalf("checks len = %d, want 1", len(checks))
	}
	return checks[0]
}
