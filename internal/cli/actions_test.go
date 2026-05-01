package cli

import (
	"strings"
	"testing"
)

func TestParseGitHubRepo(t *testing.T) {
	tests := map[string]string{
		"openclaw/crabbox":                         "openclaw/crabbox",
		"https://github.com/openclaw/crabbox.git":  "openclaw/crabbox",
		"git@github.com:openclaw/crabbox.git":      "openclaw/crabbox",
		"ssh://git@github.com/openclaw/crabbox":    "openclaw/crabbox",
		"https://github.com/openclaw/crabbox/pull": "openclaw/crabbox",
	}
	for input, want := range tests {
		got, err := parseGitHubRepo(input)
		if err != nil {
			t.Fatalf("parseGitHubRepo(%q): %v", input, err)
		}
		if got.Slug() != want {
			t.Fatalf("parseGitHubRepo(%q)=%q want %q", input, got.Slug(), want)
		}
	}
}

func TestActionsHydrateFieldsIncludesExpectedJob(t *testing.T) {
	got := strings.Join(actionsHydrateFields("cbx_123", "crabbox-cbx-123", "hydrate", 90, []string{"extra=value"}), "\n")
	for _, want := range []string{
		"crabbox_id=cbx_123",
		"crabbox_runner_label=crabbox-cbx-123",
		"crabbox_keep_alive_minutes=90",
		"crabbox_job=hydrate",
		"extra=value",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("hydrate fields missing %q in %q", want, got)
		}
	}
}

func TestActionsHydrateFieldsOmitsEmptyJobForOldWorkflows(t *testing.T) {
	got := strings.Join(actionsHydrateFields("cbx_123", "crabbox-cbx-123", "", 90, nil), "\n")
	if strings.Contains(got, "crabbox_job=") {
		t.Fatalf("hydrate fields should not send undeclared job input to older workflows: %q", got)
	}
}

func TestMergeWorkflowInputFieldsLetsFlagsOverrideConfig(t *testing.T) {
	got := mergeWorkflowInputFields(
		[]string{"crabbox_docker_cache=false", "crabbox_prepare_images=1"},
		[]string{"crabbox_docker_cache=true", "custom=value"},
	)
	want := []string{"crabbox_docker_cache=true", "crabbox_prepare_images=1", "custom=value"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("fields=%#v want %#v", got, want)
	}
}

func TestFilterWorkflowInputsDropsUndeclaredOptionalInputs(t *testing.T) {
	fields := actionsHydrateFields("cbx_123", "crabbox-cbx-123", "hydrate", 90, []string{"custom=value"})
	filtered, dropped := filterWorkflowInputs(fields, map[string]bool{
		"crabbox_id":                 true,
		"crabbox_runner_label":       true,
		"crabbox_keep_alive_minutes": true,
	})
	joined := strings.Join(filtered, "\n")
	if strings.Contains(joined, "crabbox_job=") || strings.Contains(joined, "custom=value") {
		t.Fatalf("unexpected undeclared fields kept: %q", joined)
	}
	if len(dropped) != 2 || !workflowFieldsContain(dropped, "crabbox_job") {
		t.Fatalf("unexpected dropped fields: %v", dropped)
	}
}

func TestParseWorkflowDispatchInputs(t *testing.T) {
	inputs, ok, err := parseWorkflowDispatchInputs([]byte(`name: Crabbox
on:
  workflow_dispatch:
    inputs:
      crabbox_id:
        required: true
      crabbox_runner_label:
        required: true
      crabbox_keep_alive_minutes:
        required: false
`))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !inputs["crabbox_id"] || !inputs["crabbox_runner_label"] || !inputs["crabbox_keep_alive_minutes"] {
		t.Fatalf("unexpected inputs ok=%t inputs=%v", ok, inputs)
	}
	if inputs["crabbox_job"] {
		t.Fatal("unexpected crabbox_job input")
	}
}

func TestGitHubActionsRunnerLabels(t *testing.T) {
	cfg := baseConfig()
	cfg.Profile = "Project Check"
	cfg.Class = "beast"
	cfg.Actions.RunnerLabels = []string{"linux-large", "crabbox"}
	got := githubActionsRunnerLabels(cfg, "cbx_123", "blue-lobster", []string{"extra"})
	joined := strings.Join(got, ",")
	for _, want := range []string{
		"crabbox",
		"crabbox-cbx-123",
		"crabbox-blue-lobster",
		"crabbox-profile-project-check",
		"crabbox-class-beast",
		"linux-large",
		"extra",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("labels %q missing %q", joined, want)
		}
	}
	if strings.Count(joined, "crabbox") < 1 {
		t.Fatalf("labels should keep crabbox label: %q", joined)
	}
}

func TestGitHubActionsRunnerInstallScriptUsesOfficialRunner(t *testing.T) {
	got := githubActionsRunnerInstallScript("latest", true)
	for _, want := range []string{
		"https://api.github.com/repos/actions/runner/releases/latest",
		"https://github.com/actions/runner/releases/download/",
		"./config.sh --unattended --replace --ephemeral",
		"crabbox-actions-runner.service",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("install script missing %q", want)
		}
	}
}

func TestParseActionsHydrationState(t *testing.T) {
	got := parseActionsHydrationState("WORKSPACE=/home/runner/work/repo/repo\nRUN_ID=123\nJOB=hydrate\nENV_FILE=/home/runner/.crabbox/actions/cbx-123.env.sh\nSERVICES_FILE=/home/runner/.crabbox/actions/cbx-123.services\nREADY_AT=2026-05-01T00:00:00Z\n")
	if got.Workspace != "/home/runner/work/repo/repo" || got.RunID != "123" || got.Job != "hydrate" || got.EnvFile == "" || got.ServicesFile == "" || got.ReadyAt == "" {
		t.Fatalf("unexpected hydration state: %#v", got)
	}
}

func TestActionsHydrationStatePathMatchesWorkflowInput(t *testing.T) {
	got := actionsHydrationStatePath("cbx_123")
	if got != ".crabbox/actions/cbx_123.env" {
		t.Fatalf("state path=%q", got)
	}
}

func TestRemoteClearActionsHydrationStateRemovesReadyAndStop(t *testing.T) {
	got := remoteClearActionsHydrationState("cbx_123")
	for _, want := range []string{
		".crabbox/actions/cbx_123.env",
		".crabbox/actions/cbx_123.env.sh",
		".crabbox/actions/cbx_123.services",
		".crabbox/actions/cbx_123.stop",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("clear command %q missing %q", got, want)
		}
	}
}

func TestRemoteWriteActionsHydrationStopMatchesWorkflowInput(t *testing.T) {
	got := remoteWriteActionsHydrationStop("cbx_123")
	for _, want := range []string{
		".crabbox/actions",
		".crabbox/actions/cbx_123.stop",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command %q missing %q", got, want)
		}
	}
}
