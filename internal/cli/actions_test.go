package cli

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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
	data := []byte(`name: Crabbox
on:
  workflow_dispatch:
    inputs:
      crabbox_id:
        required: true
      crabbox_runner_label:
        required: true
      crabbox_keep_alive_minutes:
        required: false
      suite:
        required: false
        default: smoke
      token:
        required: true
`)
	inputs, ok, err := parseWorkflowDispatchInputs(data)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !inputs["crabbox_id"] || !inputs["crabbox_runner_label"] || !inputs["crabbox_keep_alive_minutes"] {
		t.Fatalf("unexpected inputs ok=%t inputs=%v", ok, inputs)
	}
	if inputs["crabbox_job"] {
		t.Fatal("unexpected crabbox_job input")
	}
	_, defaults, required, ok, err := parseWorkflowDispatchInputSpec(data)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || defaults["suite"] != "smoke" || !required["token"] {
		t.Fatalf("unexpected input spec ok=%t defaults=%v required=%v", ok, defaults, required)
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
		"https://api.github.com/repos/actions/runner/releases/tags/v${version}",
		"https://github.com/actions/runner/releases/download/",
		".assets[] | select(.name == $name) | .digest",
		"sha256sum \"$archive\"",
		"runner archive checksum mismatch",
		"RUNNER_ALLOW_RUNASROOT=1",
		"grep -qi microsoft /proc/version",
		"sudo rm -rf /var/lib/apt/lists/*",
		"sudo apt-get update >/tmp/crabbox-actions-runner-apt-update.log",
		"sudo mkdir -p \"$HOME/.cache/node/corepack/v1\"",
		"sudo chown -R \"$(id -u):$(id -g)\" \"$HOME/.cache\"",
		"./config.sh --unattended --replace --ephemeral",
		"crabbox-actions-runner.service",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("install script missing %q", want)
		}
	}
	checksum := strings.Index(got, "actual_sha=")
	clearRunner := strings.Index(got, "rm -rf ./*")
	extract := strings.Index(got, "tar xzf")
	if checksum < 0 || clearRunner < checksum || extract < clearRunner {
		t.Fatalf("runner installer must verify checksum before replacing/extracting: checksum=%d clear=%d extract=%d", checksum, clearRunner, extract)
	}
}

func TestGitHubActionsRunnerInstallPowerShellScriptUsesOfficialWindowsRunner(t *testing.T) {
	got := githubActionsRunnerInstallScriptForTarget("latest", true, SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal})
	for _, want := range []string{
		"https://api.github.com/repos/actions/runner/releases/latest",
		"https://api.github.com/repos/actions/runner/releases/tags/v$version",
		"actions-runner-win-$runnerArch-$version.zip",
		"$release.assets | Where-Object { $_.name -eq $archiveName }",
		"^sha256:(?<sha>[0-9a-fA-F]{64})$",
		"Get-FileHash -LiteralPath $zip -Algorithm SHA256",
		"runner archive checksum mismatch",
		".\\config.cmd",
		"--ephemeral",
		"New-ScheduledTaskAction",
		"Start-ScheduledTask",
		"C:\\ProgramData\\crabbox\\windows.password",
		"Start-Process",
		"run-crabbox.ps1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("windows install script missing %q", want)
		}
	}
	checksum := strings.Index(got, "Get-FileHash")
	clearRunner := strings.Index(got, "Get-ChildItem -Force -LiteralPath $runnerDir | Remove-Item")
	extract := strings.Index(got, "Expand-Archive")
	if checksum < 0 || clearRunner < checksum || extract < clearRunner {
		t.Fatalf("Windows runner installer must verify checksum before replacing/extracting: checksum=%d clear=%d extract=%d", checksum, clearRunner, extract)
	}
}

func TestGitHubActionsRunnerInstallScriptRejectsChecksumMismatchBeforeExtraction(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	commands := map[string]string{
		"curl": `#!/bin/sh
out=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then shift; out="$1"; fi
  shift
done
if [ -n "$out" ]; then printf archive >"$out"; else printf '{}'; fi
`,
		"jq": `#!/bin/sh
case "$*" in
  *tag_name*) printf '2.335.1' ;;
  *digest*) printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' ;;
  *) exit 2 ;;
esac
`,
		"sha256sum": `#!/bin/sh
printf 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  %s\n' "$1"
`,
		"tar": `#!/bin/sh
touch "$TAR_CALLED"
exit 99
`,
	}
	for name, script := range commands {
		commandPath := filepath.Join(binDir, name)
		if err := os.WriteFile(commandPath, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tarCalled := filepath.Join(root, "tar-called")
	cmd := exec.Command("bash", "-c", githubActionsRunnerInstallScript("2.335.1", true))
	cmd.Env = append(os.Environ(),
		"HOME="+root,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RUNNER_REPO=example-org/my-app",
		"RUNNER_NAME=test-runner",
		"RUNNER_TOKEN=test-token",
		"RUNNER_LABELS=test",
		"TAR_CALLED="+tarCalled,
	)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "runner archive checksum mismatch") {
		t.Fatalf("install err=%v output=%q, want checksum rejection", err, output)
	}
	if _, err := os.Stat(tarCalled); !os.IsNotExist(err) {
		t.Fatalf("archive extraction ran before checksum rejection: %v", err)
	}
}

func TestGitHubActionsRunnerInstallTransportKeepsValuesOffRemoteCommand(t *testing.T) {
	values := []string{"example-org/my-app", "runner name", "linux,x64", "sentinel-registration-token"}
	script := "printf transport-ok\\n"
	input := githubActionsRunnerInstallInput(values[0], values[1], values[2], values[3], script)
	parts := strings.SplitN(input, "\n", 5)
	if len(parts) != 5 || parts[4] != script {
		t.Fatalf("install input framing failed: %#v", parts)
	}
	for i, encoded := range parts[:4] {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("decode field %d: %v", i, err)
		}
		if string(decoded) != values[i] {
			t.Fatalf("field %d=%q, want %q", i, decoded, values[i])
		}
	}

	for _, target := range []SSHTarget{
		{TargetOS: targetLinux},
		{TargetOS: targetWindows, WindowsMode: windowsModeWSL2},
		{TargetOS: targetWindows, WindowsMode: windowsModeNormal},
	} {
		remote := githubActionsRunnerInstallRemoteCommand(target)
		for _, value := range values {
			if strings.Contains(remote, value) {
				t.Fatalf("target=%#v remote command leaked %q: %s", target, value, remote)
			}
		}
	}
}

func TestGitHubActionsRunnerInstallTransportExecutesLinuxPayload(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required")
	}
	if _, err := exec.LookPath("base64"); err != nil {
		t.Skip("base64 is required")
	}
	script := `printf 'repo=%s\nname=%s\nlabels=%s\ntoken=%s\n' "$RUNNER_REPO" "$RUNNER_NAME" "$RUNNER_LABELS" "$RUNNER_TOKEN"`
	input := githubActionsRunnerInstallInput("example-org/my-app", "runner name", "linux,x64", "sentinel-registration-token", script)
	cmd := exec.Command("bash", "-c", githubActionsRunnerInstallRemoteCommand(SSHTarget{TargetOS: targetLinux}))
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("execute install transport: %v\n%s", err, out)
	}
	want := "repo=example-org/my-app\nname=runner name\nlabels=linux,x64\ntoken=sentinel-registration-token\n"
	if string(out) != want {
		t.Fatalf("transport output=%q, want %q", out, want)
	}
}

func TestActionsHydrateTargetSupport(t *testing.T) {
	tests := map[string]struct {
		target     SSHTarget
		wantLocal  bool
		wantGitHub bool
	}{
		"default":        {target: SSHTarget{}, wantLocal: true, wantGitHub: true},
		"linux":          {target: SSHTarget{TargetOS: targetLinux}, wantLocal: true, wantGitHub: true},
		"windows wsl2":   {target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, wantLocal: true, wantGitHub: true},
		"windows native": {target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, wantLocal: false, wantGitHub: true},
		"macos":          {target: SSHTarget{TargetOS: targetMacOS}, wantLocal: false, wantGitHub: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := supportsLocalActionsHydrateTarget(tt.target); got != tt.wantLocal {
				t.Fatalf("supportsLocalActionsHydrateTarget(%#v)=%t want %t", tt.target, got, tt.wantLocal)
			}
			if got := supportsGitHubActionsRunnerTarget(tt.target); got != tt.wantGitHub {
				t.Fatalf("supportsGitHubActionsRunnerTarget(%#v)=%t want %t", tt.target, got, tt.wantGitHub)
			}
		})
	}
}

func TestShouldSkipBlacksmithActionsHydrateForTestboxID(t *testing.T) {
	skipped, id, err := shouldSkipBlacksmithActionsHydrate("tbx_123", "aws")
	if err != nil {
		t.Fatal(err)
	}
	if !skipped || id != "tbx_123" {
		t.Fatalf("skipped=%t id=%q", skipped, id)
	}
}

func TestShouldSkipBlacksmithActionsHydrateForProvider(t *testing.T) {
	skipped, id, err := shouldSkipBlacksmithActionsHydrate("blue-lobster", "blacksmith-testbox")
	if err != nil {
		t.Fatal(err)
	}
	if !skipped || id != "blue-lobster" {
		t.Fatalf("skipped=%t id=%q", skipped, id)
	}
}

func TestGitHubRunnerRegistrationPermissionError(t *testing.T) {
	err := exit(3, "gh api: exit status 1\n%s", "You must have repository write permissions or have the repository runners fine-grained permission. (HTTP 403)")
	if !isGitHubRunnerRegistrationPermissionError(err) {
		t.Fatalf("permission error not detected: %v", err)
	}
}

func TestValidateActionsRunnerCapabilityAllowsWSL2(t *testing.T) {
	backend := testSSHBackend{}
	if err := validateActionsRunnerCapability(backend, Config{TargetOS: targetLinux}); err != nil {
		t.Fatalf("linux actions runner rejected: %v", err)
	}
	if err := validateActionsRunnerCapability(backend, Config{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}); err != nil {
		t.Fatalf("wsl2 actions runner rejected: %v", err)
	}
	if err := validateActionsRunnerCapability(backend, Config{TargetOS: targetWindows, WindowsMode: windowsModeNormal}); err != nil {
		t.Fatalf("native windows actions runner rejected: %v", err)
	}
}

func TestValidateActionsRunnerCapabilityRejectsLocalContainer(t *testing.T) {
	backend := testSSHBackend{spec: ProviderSpec{Name: "local-container"}}
	err := validateActionsRunnerCapability(backend, Config{Provider: "local-container", TargetOS: targetLinux})
	if err == nil || !strings.Contains(err.Error(), "provider=local-container") {
		t.Fatalf("local-container actions runner error=%v", err)
	}
}

func TestValidateActionsRunnerCapabilityRejectsAppleContainer(t *testing.T) {
	backend := testSSHBackend{spec: ProviderSpec{Name: "apple-container"}}
	err := validateActionsRunnerCapability(backend, Config{Provider: "apple-container", TargetOS: targetLinux})
	if err == nil || !strings.Contains(err.Error(), "provider=apple-container") {
		t.Fatalf("apple-container actions runner error=%v", err)
	}
}

func TestValidateActionsRunnerCapabilityRejectsMultipass(t *testing.T) {
	backend := testSSHBackend{spec: ProviderSpec{Name: "multipass"}}
	err := validateActionsRunnerCapability(backend, Config{Provider: "multipass", TargetOS: targetLinux})
	if err == nil || !strings.Contains(err.Error(), "provider=multipass") {
		t.Fatalf("multipass actions runner error=%v", err)
	}
}

func TestLocalActionsWorkflowPathRequiresRepoWorkflowFile(t *testing.T) {
	root := t.TempDir()
	workflow := filepath.Join(root, ".github", "workflows", "hydrate.yml")
	if err := os.MkdirAll(filepath.Dir(workflow), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflow, []byte("name: hydrate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := localActionsWorkflowPath(root, ".github/workflows/hydrate.yml")
	if err != nil {
		t.Fatal(err)
	}
	if got != workflow {
		t.Fatalf("workflow path=%q want %q", got, workflow)
	}
	got, err = localActionsWorkflowPath(root, "hydrate")
	if err != nil {
		t.Fatal(err)
	}
	if got != workflow {
		t.Fatalf("bare workflow path=%q want %q", got, workflow)
	}
	if _, err := localActionsWorkflowPath(root, "missing.yml"); err == nil {
		t.Fatal("missing bare workflow accepted")
	}
	if _, err := localActionsWorkflowPath(root, ".github/workflows/../../hydrate.yml"); err == nil {
		t.Fatal("workflow traversal accepted")
	}
}

func TestSelectLocalHydrateJobPrefersHydrateOverMarkerJob(t *testing.T) {
	workflow := localHydrateWorkflow{Jobs: map[string]localHydrateJob{
		"hydrate": {Name: "Hydrate"},
		"smoke":   {Name: "Smoke"},
	}}
	name, job, err := selectLocalHydrateJob(workflow, "smoke")
	if err != nil {
		t.Fatal(err)
	}
	if name != "hydrate" || job.Name != "Hydrate" {
		t.Fatalf("selected job %q %#v, want hydrate", name, job)
	}
}

func TestSelectLocalHydrateJobAllowsSingleJobWorkflow(t *testing.T) {
	workflow := localHydrateWorkflow{Jobs: map[string]localHydrateJob{
		"setup": {Name: "Setup"},
	}}
	name, job, err := selectLocalHydrateJob(workflow, "")
	if err != nil {
		t.Fatal(err)
	}
	if name != "setup" || job.Name != "Setup" {
		t.Fatalf("selected job %q %#v, want setup", name, job)
	}
}

func TestLocalActionsHydrateScriptTranslatesCoreSteps(t *testing.T) {
	cfg := defaultConfig()
	cfg.Actions.Ref = "main"
	cfg.Actions.Repo = "example-org/configured"
	repo := Repo{
		Name:      "my-app",
		RemoteURL: "https://github.com/example-org/my-app.git",
		Head:      "abc123",
	}
	workflow := localHydrateWorkflow{
		Env: map[string]string{"WORKFLOW_ENV": "${{github.workspace}}"},
	}
	job := localHydrateJob{
		Env: map[string]string{"JOB_ENV": "${{inputs.crabbox_id}}"},
		Steps: []localHydrateStep{
			{Name: "Checkout", Uses: "actions/checkout@v4"},
			{Name: "Node", Uses: "actions/setup-node@v4", With: map[string]string{"node-version": "22"}},
			{Name: "Install", Env: map[string]string{"STEP_ENV": "${{env.JOB_ENV}}", "STEP_TEMP": "${{ runner.temp }}"}, Run: "echo ${{inputs.crabbox_id}}\necho ${{ runner.temp }}\nprintf 'NEXT=1\\n' >> \"$GITHUB_ENV\""},
			{Name: "Skipped", If: "${{ false }}", Run: "exit 99"},
		},
	}
	workdir := "/work/cbx_123/my-app"
	got, err := localActionsHydrateScript(cfg, repo, workflow, job, "hydrate", "cbx_123", actionsHydrateFields("cbx_123", "crabbox-cbx-123", "hydrate", 0, nil), workdir)
	if err != nil {
		t.Fatal(err)
	}
	runnerRoot := path.Join(path.Dir(workdir), ".crabbox-local-actions", localActionsRunnerRootName("cbx_123"))
	for _, want := range []string{
		"export GITHUB_ACTIONS='true'",
		"export GITHUB_JOB='hydrate'",
		"export GITHUB_REF='refs/heads/main'",
		"export GITHUB_REF_NAME='main'",
		"export GITHUB_REPOSITORY='example-org/configured'",
		"export GITHUB_WORKSPACE='/work/cbx_123/my-app'",
		"export INPUT_CRABBOX_ID='cbx_123'",
		"rm -rf " + shellQuote(runnerRoot),
		"chmod 700 " + shellQuote(runnerRoot),
		"$(stat -c %u " + shellQuote(runnerRoot) + ")",
		"export RUNNER_TEMP=" + shellQuote(runnerRoot+"/tmp"),
		"export RUNNER_TOOL_CACHE=" + shellQuote(runnerRoot+"/tools"),
		"x86_64|amd64) export RUNNER_ARCH='X64'",
		"aarch64|arm64) export RUNNER_ARCH='ARM64'",
		"# actions/checkout handled by Crabbox sync/git seed",
		"__crabbox_setup_node '22'",
		"__crabbox_run_bash '/work/cbx_123/my-app'",
		"echo cbx_123",
		"echo " + runnerRoot + "/tmp",
		"printf 'NEXT=1\\n' >> \"$GITHUB_ENV\"",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("local hydrate script missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "exit 99") {
		t.Fatalf("skipped step emitted:\n%s", got)
	}
	if strings.Contains(got, "'$HOME/.crabbox") {
		t.Fatalf("runner paths should not be exported as literal HOME paths:\n%s", got)
	}
}

func TestLocalActionsHydrateScriptTracksCacheRestoreOutputs(t *testing.T) {
	job := localHydrateJob{Steps: []localHydrateStep{
		{ID: "deps", Uses: "actions/cache/restore@v5"},
		{Name: "Install on miss", If: "steps.deps.outputs.cache-hit == 'false'", Run: "echo install"},
		{Name: "Skip on hit", If: "steps.deps.outputs.cache-hit != 'false'", Run: "exit 99"},
		{Name: "Report", Run: "echo ${{ steps.deps.outputs.cache-hit }}"},
	}}
	got, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, job, "hydrate", "cbx_123", nil, "/work/cbx_123/repo")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"printf 'cache-hit=false\\ncache-matched-key=\\n' >> \"$GITHUB_OUTPUT\"",
		"echo install",
		"echo false",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("local hydrate script missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "exit 99") || strings.Contains(got, "${{ steps.deps.outputs.cache-hit }}") {
		t.Fatalf("cache output condition or interpolation failed:\n%s", got)
	}
}

func TestLocalActionsHydrateScriptExpandsLocalCompositeActions(t *testing.T) {
	root := t.TempDir()
	actionDir := filepath.Join(root, ".github", "actions", "setup-node-env")
	if err := os.MkdirAll(actionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	action := []byte(`name: Setup Node
inputs:
  node-version:
    default: "24.x"
  install-bun:
    default: "true"
outputs:
  cache-hit:
    value: ${{ steps.pnpm-cache-restore.outputs.cache-hit }}
runs:
  using: composite
  steps:
    - name: Setup Node.js
      uses: actions/setup-node@v6
      with:
        node-version: ${{ inputs.node-version }}
    - name: Cache restore
      id: pnpm-cache-restore
      if: inputs.install-bun == 'false'
      uses: actions/cache/restore@v5
      with:
        key: ${{ hashFiles('pnpm-lock.yaml') }}
    - name: Skipped Bun
      if: inputs.install-bun == 'true'
      run: exit 99
      shell: bash
    - name: Install
      env:
        NODE_VERSION: ${{ inputs.node-version }}
        ACTION_SCRIPT: ${{ github.action_path }}/setup.sh
      run: |
        echo "$GITHUB_ACTION_PATH"
        echo "$ACTION_SCRIPT"
        echo "$NODE_VERSION"
        printf 'NEXT=1\n' >> "$GITHUB_ENV"
      shell: bash
`)
	if err := os.WriteFile(filepath.Join(actionDir, "action.yml"), action, 0o644); err != nil {
		t.Fatal(err)
	}
	job := localHydrateJob{Steps: []localHydrateStep{
		{ID: "setup", Uses: "./.github/actions/setup-node-env", With: map[string]string{"install-bun": "false"}},
		{If: "steps.setup.outputs.cache-hit == 'false'", Run: "echo outer install"},
	}}
	got, err := localActionsHydrateScript(defaultConfig(), Repo{Root: root, Name: "repo"}, localHydrateWorkflow{}, job, "hydrate", "cbx_123", actionsHydrateFields("cbx_123", "crabbox-cbx-123", "", 0, nil), "/work/cbx_123/repo")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"export INPUT_NODE_VERSION='24.x'",
		"__crabbox_setup_node '24'",
		"printf 'cache-hit=false\\ncache-matched-key=\\n' >> \"$GITHUB_OUTPUT\"",
		"export GITHUB_ACTION_PATH='/work/cbx_123/repo/.github/actions/setup-node-env'",
		"export ACTION_SCRIPT='/work/cbx_123/repo/.github/actions/setup-node-env/setup.sh'",
		"echo \"$GITHUB_ACTION_PATH\"",
		"echo \"$NODE_VERSION\"",
		"echo outer install",
		"printf 'NEXT=1\\n' >> \"$GITHUB_ENV\"",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("local composite script missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "exit 99") || strings.Contains(got, "${{") {
		t.Fatalf("local composite script kept skipped or unresolved content:\n%s", got)
	}
}

func TestLocalActionsHydrateScriptTracksCompositeRunStepOutputs(t *testing.T) {
	root := t.TempDir()
	actionDir := filepath.Join(root, ".github", "actions", "pnpm-cache")
	if err := os.MkdirAll(actionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	action := []byte(`name: pnpm cache
inputs:
  use-actions-cache:
    default: "true"
outputs:
  cache-enabled:
    value: ${{ steps.config.outputs.enabled }}
  primary-key:
    value: ${{ steps.config.outputs.primary-key }}
runs:
  using: composite
  steps:
    - name: Resolve cache keys
      id: config
      shell: bash
      env:
        CACHE_KEY_SUFFIX: node24-pnpm11
      run: |
        echo "enabled=$INPUT_USE_ACTIONS_CACHE" >> "$GITHUB_OUTPUT"
        echo "primary-key=${RUNNER_OS}-pnpm-store-${CACHE_KEY_SUFFIX}" >> "$GITHUB_OUTPUT"
`)
	if err := os.WriteFile(filepath.Join(actionDir, "action.yml"), action, 0o644); err != nil {
		t.Fatal(err)
	}
	job := localHydrateJob{Steps: []localHydrateStep{
		{ID: "cache", Uses: "./.github/actions/pnpm-cache"},
		{If: "steps.cache.outputs.cache-enabled == 'true'", Run: "echo cache is enabled"},
		{Run: "echo ${{ steps.cache.outputs.primary-key }}"},
	}}
	got, err := localActionsHydrateScript(defaultConfig(), Repo{Root: root, Name: "repo"}, localHydrateWorkflow{}, job, "hydrate", "cbx_123", nil, "/work/cbx_123/repo")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`echo "enabled=$INPUT_USE_ACTIONS_CACHE" >> "$GITHUB_OUTPUT"`,
		"echo cache is enabled",
		"echo Linux-pnpm-store-node24-pnpm11",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("local composite run output script missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "${{") {
		t.Fatalf("local composite run output script kept unresolved content:\n%s", got)
	}
}

func TestLocalActionsHydrateScriptRejectsDynamicCompositeRunStepOutputs(t *testing.T) {
	root := t.TempDir()
	actionDir := filepath.Join(root, ".github", "actions", "dynamic-output")
	if err := os.MkdirAll(actionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	action := []byte(`name: dynamic output
outputs:
  value:
    value: ${{ steps.config.outputs.value }}
runs:
  using: composite
  steps:
    - id: config
      shell: bash
      run: |
        value=computed-at-runtime
        echo "value=$value" >> "$GITHUB_OUTPUT"
`)
	if err := os.WriteFile(filepath.Join(actionDir, "action.yml"), action, 0o644); err != nil {
		t.Fatal(err)
	}
	job := localHydrateJob{Steps: []localHydrateStep{
		{ID: "dynamic", Uses: "./.github/actions/dynamic-output"},
		{Run: "echo ${{ steps.dynamic.outputs.value }}"},
	}}
	_, err := localActionsHydrateScript(defaultConfig(), Repo{Root: root, Name: "repo"}, localHydrateWorkflow{}, job, "hydrate", "cbx_123", nil, "/work/cbx_123/repo")
	if err == nil || !strings.Contains(err.Error(), "--github-runner") {
		t.Fatalf("dynamic composite output should require GitHub fallback: %v", err)
	}
}

func TestLocalActionsHydrateScriptRejectsConditionalCompositeRunStepOutputs(t *testing.T) {
	root := t.TempDir()
	actionDir := filepath.Join(root, ".github", "actions", "conditional-output")
	if err := os.MkdirAll(actionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	action := []byte(`name: conditional output
outputs:
  enabled:
    value: ${{ steps.config.outputs.enabled }}
runs:
  using: composite
  steps:
    - id: config
      shell: bash
      run: |
        if [ "$INPUT_ENABLED" = true ]; then
          echo "enabled=true" >> "$GITHUB_OUTPUT"
        fi
`)
	if err := os.WriteFile(filepath.Join(actionDir, "action.yml"), action, 0o644); err != nil {
		t.Fatal(err)
	}
	job := localHydrateJob{Steps: []localHydrateStep{
		{ID: "conditional", Uses: "./.github/actions/conditional-output"},
		{If: "steps.conditional.outputs.enabled == 'true'", Run: "echo enabled"},
	}}
	_, err := localActionsHydrateScript(defaultConfig(), Repo{Root: root, Name: "repo"}, localHydrateWorkflow{}, job, "hydrate", "cbx_123", nil, "/work/cbx_123/repo")
	if err == nil || !strings.Contains(err.Error(), "--github-runner") {
		t.Fatalf("conditional composite output should require GitHub fallback: %v", err)
	}
}

func TestInferLocalRunStepOutputsPreservesSingleQuotedPayload(t *testing.T) {
	got := inferLocalRunStepOutputs(`echo 'value=$FOO' >> "$GITHUB_OUTPUT"`, map[string]string{"FOO": "expanded"})
	if got["value"] != "$FOO" {
		t.Fatalf("single-quoted output expanded unexpectedly: %#v", got)
	}
}

func TestInferLocalRunStepOutputsRejectsParameterExpansion(t *testing.T) {
	got := inferLocalRunStepOutputs(`echo "value=${FOO:-fallback}" >> "$GITHUB_OUTPUT"`, map[string]string{"FOO": "expanded"})
	if len(got) != 0 {
		t.Fatalf("parameter expansion inferred unexpectedly: %#v", got)
	}
}

func TestInferLocalRunStepOutputsRejectsEscapedDollar(t *testing.T) {
	got := inferLocalRunStepOutputs(`echo "value=\$FOO" >> "$GITHUB_OUTPUT"`, map[string]string{"FOO": "expanded"})
	if len(got) != 0 {
		t.Fatalf("escaped dollar inferred unexpectedly: %#v", got)
	}
}

func TestInferLocalRunStepOutputsRejectsUnmodeledOverwrite(t *testing.T) {
	got := inferLocalRunStepOutputs(`echo "enabled=false" >> "$GITHUB_OUTPUT"
echo "enabled=$RUNTIME_VALUE" >> "$GITHUB_OUTPUT"`, nil)
	if len(got) != 0 {
		t.Fatalf("unmodeled overwrite inferred unexpectedly: %#v", got)
	}
}

func TestLocalActionsHydrateScriptAllowsEmptySecrets(t *testing.T) {
	job := localHydrateJob{Env: map[string]string{
		"OPENAI_API_KEY": "${{ secrets.OPENAI_API_KEY }}",
	}, Steps: []localHydrateStep{
		{Run: "test -z \"$OPENAI_API_KEY\""},
	}}
	got, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, job, "hydrate", "cbx_123", nil, "/work/cbx_123/repo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "export OPENAI_API_KEY=''") || strings.Contains(got, "${{") {
		t.Fatalf("local hydrate script did not empty secret expression:\n%s", got)
	}
}

func TestLocalActionsHydrateScriptRejectsUnknownStepOutputs(t *testing.T) {
	_, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, localHydrateJob{
		Steps: []localHydrateStep{
			{ID: "build", Run: "printf 'artifact=app\\n' >> \"$GITHUB_OUTPUT\""},
			{Run: "echo ${{ steps.build.outputs.artifact }}"},
		},
	}, "hydrate", "cbx_123", nil, "/work/cbx_123/repo")
	if err == nil {
		t.Fatal("runtime step output expression accepted")
	}
	if !strings.Contains(err.Error(), "steps.build.outputs.artifact") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLocalActionsHashFilesMatchesRecursiveGlobs(t *testing.T) {
	root := t.TempDir()
	for _, file := range []string{"pnpm-lock.yaml", filepath.Join("packages", "app", "pnpm-lock.yaml")} {
		path := filepath.Join(root, file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hash, ok := localActionsHashFiles("hashFiles('**/pnpm-lock.yaml')", root)
	if !ok || hash == "" {
		t.Fatalf("recursive hashFiles did not match: ok=%v hash=%q", ok, hash)
	}
	got, err := localActionsHydrateScript(defaultConfig(), Repo{Root: root, Name: "repo"}, localHydrateWorkflow{}, localHydrateJob{
		Steps: []localHydrateStep{{Run: "echo ${{ hashFiles('**/pnpm-lock.yaml') }}"}},
	}, "hydrate", "cbx_123", nil, "/work/cbx_123/repo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "echo "+hash) {
		t.Fatalf("recursive hash not interpolated:\n%s", got)
	}
}

func TestLocalActionsHashFilesSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "outside-lock.yaml")
	if err := os.WriteFile(target, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "pnpm-lock.yaml")); err != nil {
		t.Fatal(err)
	}
	hash, ok := localActionsHashFiles("hashFiles('pnpm-lock.yaml')", root)
	if !ok {
		t.Fatal("hashFiles expression was not handled")
	}
	if hash != "" {
		t.Fatalf("symlink was hashed: %q", hash)
	}
}

func TestLocalActionsHydrateScriptUsesFullGitHubRef(t *testing.T) {
	cfg := defaultConfig()
	cfg.Actions.Ref = "refs/tags/v1.2.3"
	got, err := localActionsHydrateScript(cfg, Repo{Name: "repo"}, localHydrateWorkflow{}, localHydrateJob{}, "hydrate", "cbx_123", actionsHydrateFields("cbx_123", "crabbox-cbx-123", "hydrate", 0, nil), "/work/cbx_123/repo")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"export GITHUB_REF='refs/tags/v1.2.3'",
		"export GITHUB_REF_NAME='v1.2.3'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("local hydrate script missing %q in:\n%s", want, got)
		}
	}
}

func TestLocalActionsHydrateScriptInterpolatesSetupInputs(t *testing.T) {
	job := localHydrateJob{
		Steps: []localHydrateStep{
			{Uses: "actions/setup-node@v4", With: map[string]string{"node-version": "${{ inputs.node }}"}},
			{Uses: "actions/setup-go@v5", With: map[string]string{"go-version-file": "${{ inputs.go_file }}"}},
			{Uses: "actions/setup-python@v5", With: map[string]string{"python-version": "${{ env.PYTHON_VERSION }}"}, Env: map[string]string{"PYTHON_VERSION": "${{ inputs.python }}"}},
		},
	}
	fields := actionsHydrateFields("cbx_123", "crabbox-cbx-123", "", 0, []string{"node=24", "go_file=go.mod", "python=3.12"})
	got, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, job, "hydrate", "cbx_123", fields, "/work/cbx_123/repo")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"__crabbox_setup_node '24'",
		"__crabbox_setup_go 'go.mod'",
		"__crabbox_setup_python '3.12'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("local hydrate script missing %q in:\n%s", want, got)
		}
	}
}

func TestLocalActionsHydrateScriptDoesNotDefaultSetupNodeVersion(t *testing.T) {
	job := localHydrateJob{Steps: []localHydrateStep{{Uses: "actions/setup-node@v4"}}}
	got, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, job, "hydrate", "cbx_123", actionsHydrateFields("cbx_123", "crabbox-cbx-123", "", 0, nil), "/work/cbx_123/repo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "__crabbox_setup_node ''") {
		t.Fatalf("setup-node without a version should not default to Node 22:\n%s", got)
	}
}

func TestLocalActionsHydrateScriptRejectsUnsupportedSetupNodeVersion(t *testing.T) {
	job := localHydrateJob{Steps: []localHydrateStep{{Uses: "actions/setup-node@v4", With: map[string]string{"node-version": "lts/*"}}}}
	_, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, job, "hydrate", "cbx_123", actionsHydrateFields("cbx_123", "crabbox-cbx-123", "", 0, nil), "/work/cbx_123/repo")
	if err == nil {
		t.Fatal("expected unsupported setup-node version to fail")
	}
}

func TestLocalActionsHydrateScriptRejectsUnsupportedSetupNodeOptions(t *testing.T) {
	job := localHydrateJob{Steps: []localHydrateStep{{Uses: "actions/setup-node@v4", With: map[string]string{"node-version": "22", "registry-url": "https://registry.npmjs.org"}}}}
	_, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, job, "hydrate", "cbx_123", actionsHydrateFields("cbx_123", "crabbox-cbx-123", "", 0, nil), "/work/cbx_123/repo")
	if err == nil {
		t.Fatal("expected unsupported setup-node option to fail")
	}
	if !strings.Contains(err.Error(), "--github-runner") {
		t.Fatalf("unsupported setup-node option error should suggest GitHub fallback: %v", err)
	}
}

func TestLocalActionsHydrateScriptAllowsSetupNodeCheckLatest(t *testing.T) {
	job := localHydrateJob{Steps: []localHydrateStep{{Uses: "actions/setup-node@v4", With: map[string]string{"node-version": "22", "check-latest": "true"}}}}
	got, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, job, "hydrate", "cbx_123", actionsHydrateFields("cbx_123", "crabbox-cbx-123", "", 0, nil), "/work/cbx_123/repo")
	if err != nil {
		t.Fatalf("setup-node check-latest should be allowed: %v", err)
	}
	if !strings.Contains(got, "__crabbox_setup_node '22'") {
		t.Fatalf("setup-node script missing version:\n%s", got)
	}
	if !strings.Contains(got, "xz-utils") {
		t.Fatalf("setup-node script should ensure xz-utils:\n%s", got)
	}
}

func TestLocalActionsSetupNodeVerifiesArchiveBeforeExtraction(t *testing.T) {
	got := localActionsRuntimeShell()
	for _, want := range []string{
		`SHASUMS256.txt`,
		`Node release checksums did not contain a valid digest`,
		`Node archive checksum mismatch`,
		`.crabbox-node-sha256`,
		`tar -xJf "$tmp" -C "$extract"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("setup-node runtime missing %q", want)
		}
	}
	checksum := strings.Index(got, `actual="$(sha256sum "$tmp"`)
	extract := strings.Index(got, `tar -xJf "$tmp"`)
	if checksum < 0 || extract < checksum {
		t.Fatalf("setup-node must verify the archive before extraction: checksum=%d extract=%d", checksum, extract)
	}
}

func TestLocalActionsSetupNodeRejectsMissingOrMismatchedChecksum(t *testing.T) {
	for _, tc := range []struct {
		name      string
		checksums string
		want      string
	}{
		{name: "missing", checksums: "", want: "did not contain a valid digest"},
		{name: "mismatch", checksums: strings.Repeat("a", 64) + "  node-v88.0.0-linux-x64.tar.xz\n", want: "checksum mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			binDir := filepath.Join(root, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			commands := map[string]string{
				"curl": `#!/bin/sh
out=
url=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then shift; out="$1"; else url="$1"; fi
  shift
done
case "$url" in
  */index.tab) printf 'version\n%s\n' 'v88.0.0' ;;
  */SHASUMS256.txt) printf '%s' "$TEST_CHECKSUMS" >"$out" ;;
  *) printf archive >"$out" ;;
esac
`,
				"sha256sum": `#!/bin/sh
printf '%s  %s\n' 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' "$1"
`,
				"tar": `#!/bin/sh
touch "$TAR_CALLED"
exit 99
`,
				"node":  "#!/bin/sh\nprintf '22.0.0\\n'\n",
				"uname": "#!/bin/sh\nprintf 'x86_64\\n'\n",
			}
			for name, script := range commands {
				if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			tarCalled := filepath.Join(root, "tar-called")
			script := localActionsRuntimeShell() + "\n__crabbox_setup_node 88\n"
			cmd := exec.Command("bash", "-c", script)
			cmd.Env = append(os.Environ(),
				"GITHUB_WORKSPACE="+root,
				"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"RUNNER_TEMP="+filepath.Join(root, "tmp"),
				"RUNNER_TOOL_CACHE="+filepath.Join(root, "tools"),
				"TAR_CALLED="+tarCalled,
				"TEST_CHECKSUMS="+tc.checksums,
			)
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), tc.want) {
				t.Fatalf("setup-node err=%v output=%q, want %q", err, output, tc.want)
			}
			if _, err := os.Stat(tarCalled); !os.IsNotExist(err) {
				t.Fatalf("archive extraction ran before checksum rejection: %v", err)
			}
		})
	}
}

func TestLocalActionsHydrateScriptKeepsToolCacheOffWorkRoot(t *testing.T) {
	job := localHydrateJob{Steps: []localHydrateStep{{Uses: "actions/setup-node@v4", With: map[string]string{"node-version": "22"}}}}
	workdir := "/work/cbx_123/repo"
	got, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, job, "hydrate", "cbx_123", nil, workdir)
	if err != nil {
		t.Fatal(err)
	}
	runnerRoot := path.Join(path.Dir(workdir), ".crabbox-local-actions", localActionsRunnerRootName("cbx_123"))
	for _, want := range []string{
		"rm -rf " + shellQuote(runnerRoot),
		"chmod 700 " + shellQuote(runnerRoot),
		"$(stat -c %u " + shellQuote(runnerRoot) + ")",
		"export RUNNER_TEMP=" + shellQuote(runnerRoot+"/tmp"),
		"export RUNNER_TOOL_CACHE=" + shellQuote(runnerRoot+"/tools"),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("local hydrate script missing %q in:\n%s", want, got)
		}
	}
	for _, notWant := range []string{
		"RUNNER_TEMP='/tmp/",
		"RUNNER_TOOL_CACHE='/tmp/",
		"RUNNER_TEMP='/work/cbx_123/repo/",
		"RUNNER_TOOL_CACHE='/work/cbx_123/repo/",
	} {
		if strings.Contains(got, notWant) {
			t.Fatalf("local hydrate script should not place runner cache under work root %q:\n%s", notWant, got)
		}
	}
}

func TestLocalActionsHydrateScriptUsesSafeRunnerRootName(t *testing.T) {
	job := localHydrateJob{Steps: []localHydrateStep{{Run: "echo ok"}}}
	workdir := "/work/cbx_123/repo"
	got, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, job, "hydrate", "../cbx_123/../../bad", nil, workdir)
	if err != nil {
		t.Fatal(err)
	}
	runnerRoot := path.Join(path.Dir(workdir), ".crabbox-local-actions", localActionsRunnerRootName("../cbx_123/../../bad"))
	for _, want := range []string{
		"rm -rf " + shellQuote(runnerRoot),
		"mkdir -p " + shellQuote(runnerRoot),
		"export RUNNER_TEMP=" + shellQuote(runnerRoot+"/tmp"),
		"export RUNNER_TOOL_CACHE=" + shellQuote(runnerRoot+"/tools"),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("local hydrate script missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, ".crabbox-local-actions/..") {
		t.Fatalf("local hydrate script used raw lease id for runner root:\n%s", got)
	}
}

func TestLocalActionsHydrateScriptRejectsMalformedFields(t *testing.T) {
	_, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, localHydrateJob{
		Steps: []localHydrateStep{{Run: "echo ok"}},
	}, "hydrate", "cbx_123", []string{"node"}, "/work/cbx_123/repo")
	if err == nil {
		t.Fatal("malformed field accepted")
	}
	if !strings.Contains(err.Error(), "workflow input must be key=value") {
		t.Fatalf("unexpected malformed field error: %v", err)
	}
}

func TestLocalActionsHydrateScriptRestoresStepEnvBeforeApplyingGITHUBENV(t *testing.T) {
	got, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, localHydrateJob{
		Env: map[string]string{"FLAG": "job"},
		Steps: []localHydrateStep{
			{Env: map[string]string{"FLAG": "step"}, Run: "printf 'FLAG=next\\n' >> \"$GITHUB_ENV\""},
			{Run: "echo \"$FLAG\""},
		},
	}, "hydrate", "cbx_123", actionsHydrateFields("cbx_123", "crabbox-cbx-123", "", 0, nil), "/work/cbx_123/repo")
	if err != nil {
		t.Fatal(err)
	}
	save := strings.Index(got, "__crabbox_save_step_env 'FLAG'")
	exportStep := strings.Index(got, "export FLAG='step'")
	restore := strings.Index(got, "__crabbox_restore_step_env 'FLAG'")
	apply := -1
	if restore >= 0 {
		if relative := strings.Index(got[restore:], "__crabbox_apply_step_files"); relative >= 0 {
			apply = restore + relative
		}
	}
	if save < 0 || exportStep < 0 || restore < 0 || apply < 0 {
		t.Fatalf("script missing scoped step env handling:\n%s", got)
	}
	if !(save < exportStep && exportStep < restore && restore < apply) {
		t.Fatalf("step env should be restored before applying GITHUB_ENV:\n%s", got)
	}
}

func TestLocalActionsHydrateScriptClearsMissingOptionalInputs(t *testing.T) {
	got, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, localHydrateJob{
		Steps: []localHydrateStep{{Run: "job=\"${{inputs.crabbox_job}}\"\necho \"job=$job\""}},
	}, "hydrate", "cbx_123", actionsHydrateFields("cbx_123", "crabbox-cbx-123", "", 0, nil), "/work/cbx_123/repo")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "${{") {
		t.Fatalf("unresolved Actions expression emitted:\n%s", got)
	}
	if !strings.Contains(got, "job=\"\"") {
		t.Fatalf("missing optional input should become empty string:\n%s", got)
	}
}

func TestApplyWorkflowInputDefaultsPreservesExplicitFields(t *testing.T) {
	fields := applyWorkflowInputDefaults([]string{"suite=full"}, map[string]string{"suite": "smoke", "node": "22"})
	joined := strings.Join(fields, "\n")
	if !strings.Contains(joined, "suite=full") || !strings.Contains(joined, "node=22") || strings.Contains(joined, "suite=smoke") {
		t.Fatalf("unexpected fields with defaults: %v", fields)
	}
}

func TestMissingRequiredWorkflowInputs(t *testing.T) {
	missing := missingRequiredWorkflowInputs([]string{"suite=full"}, map[string]bool{"suite": true, "token": true})
	if len(missing) != 1 || missing[0] != "token" {
		t.Fatalf("missing=%v, want token", missing)
	}
}

func TestLocalActionsHydrateScriptRejectsUnsupportedUses(t *testing.T) {
	_, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, localHydrateJob{
		Steps: []localHydrateStep{{Uses: "docker/login-action@v3"}},
	}, "hydrate", "cbx_123", nil, "/work/cbx_123/repo")
	if err == nil {
		t.Fatal("unsupported uses step accepted")
	}
}

func TestLocalActionsHydrateScriptRejectsUnsupportedCheckoutOptions(t *testing.T) {
	_, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, localHydrateJob{
		Steps: []localHydrateStep{{Uses: "actions/checkout@v4", With: map[string]string{"submodules": "true"}}},
	}, "hydrate", "cbx_123", nil, "/work/cbx_123/repo")
	if err == nil {
		t.Fatal("unsupported checkout option accepted")
	}
	if !strings.Contains(err.Error(), "--github-runner") {
		t.Fatalf("unsupported checkout option error should suggest GitHub fallback: %v", err)
	}
}

func TestLocalActionsHydrateScriptIgnoresCheckoutRefExpression(t *testing.T) {
	_, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, localHydrateJob{
		Steps: []localHydrateStep{{Uses: "actions/checkout@v4", With: map[string]string{"ref": "${{ inputs.ref || github.ref }}"}}},
	}, "hydrate", "cbx_123", nil, "/work/cbx_123/repo")
	if err != nil {
		t.Fatalf("checkout ref expression should be ignored locally: %v", err)
	}
}

func TestLocalActionsHydrateScriptRejectsUnsupportedIf(t *testing.T) {
	_, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, localHydrateJob{
		Steps: []localHydrateStep{{If: "${{ inputs.enabled }}", Run: "echo nope"}},
	}, "hydrate", "cbx_123", nil, "/work/cbx_123/repo")
	if err == nil {
		t.Fatal("unsupported if expression accepted")
	}
}

func TestLocalActionsHydrateScriptRejectsEnvIf(t *testing.T) {
	_, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, localHydrateJob{
		Steps: []localHydrateStep{
			{Run: "printf 'RUN_TESTS=true\\n' >> \"$GITHUB_ENV\""},
			{If: "env.RUN_TESTS == 'true'", Run: "echo nope"},
		},
	}, "hydrate", "cbx_123", nil, "/work/cbx_123/repo")
	if err == nil {
		t.Fatal("env if expression accepted")
	}
}

func TestLocalActionsHydrateScriptEmptiesSecretsExpressions(t *testing.T) {
	got, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, localHydrateJob{
		Steps: []localHydrateStep{{Run: "echo '${{ secrets.NPM_TOKEN }}' '${{ secrets.MISSING_TOKEN }}'"}},
	}, "hydrate", "cbx_123", nil, "/work/cbx_123/repo")
	if err != nil {
		t.Fatalf("secret expression should render empty locally: %v", err)
	}
	if strings.Contains(got, "${{") || !strings.Contains(got, "echo '' ''") {
		t.Fatalf("local hydrate script did not empty secret expressions:\n%s", got)
	}
}

func TestLocalActionsHydrateScriptRejectsComplexSecretsExpressions(t *testing.T) {
	_, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, localHydrateJob{
		Steps: []localHydrateStep{{Run: "echo '${{ secrets.NPM_TOKEN != '' }}'"}},
	}, "hydrate", "cbx_123", nil, "/work/cbx_123/repo")
	if err == nil || !strings.Contains(err.Error(), "--github-runner") {
		t.Fatalf("complex secret expression should require GitHub fallback: %v", err)
	}
}

func TestLocalActionsHydrateScriptRejectsUnsupportedExpression(t *testing.T) {
	_, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, localHydrateJob{
		Steps: []localHydrateStep{{Run: "echo '${{ matrix.node }}'"}},
	}, "hydrate", "cbx_123", nil, "/work/cbx_123/repo")
	if err == nil {
		t.Fatal("unsupported expression accepted")
	}
	if !strings.Contains(err.Error(), "--github-runner") {
		t.Fatalf("unsupported expression error should suggest GitHub fallback: %v", err)
	}
}

func TestLocalActionsHydrateScriptRejectsServicesAndContainers(t *testing.T) {
	serviceJob := localHydrateJob{
		Services: map[string]yaml.Node{"postgres": {}},
		Steps:    []localHydrateStep{{Run: "echo ok"}},
	}
	if _, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, serviceJob, "hydrate", "cbx_123", nil, "/work/cbx_123/repo"); err == nil {
		t.Fatal("service container job accepted")
	}
	containerJob := localHydrateJob{
		Container: yaml.Node{Kind: yaml.ScalarNode, Value: "node:22"},
		Steps:     []localHydrateStep{{Run: "echo ok"}},
	}
	if _, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "repo"}, localHydrateWorkflow{}, containerJob, "hydrate", "cbx_123", nil, "/work/cbx_123/repo"); err == nil {
		t.Fatal("container job accepted")
	}
}

func TestAppendLocalHydrateRunStepAvoidsHeredocDelimiterCollision(t *testing.T) {
	var b strings.Builder
	if err := appendLocalHydrateRunStep(&b, "bash", "/work/repo", "cat <<'CRABBOX_STEP'\nhello\nCRABBOX_STEP"); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	if !strings.Contains(got, "<<'CRABBOX_STEP_2'") {
		t.Fatalf("expected alternate heredoc delimiter in:\n%s", got)
	}
}

func TestLocalActionsRuntimeValidatesGoAndPythonVersions(t *testing.T) {
	got := localActionsRuntimeShell()
	for _, want := range []string{
		`awk '$1 == "go" { print $2; exit }' "$GITHUB_WORKSPACE/$requested"`,
		`case "$actual" in "go${major}.${minor}.${patch}") return 0 ;; esac`,
		`1) case "$actual" in "$want"|"$want".*) corepack enable >/dev/null 2>&1 || true; return 0 ;; esac ;;`,
		`found == "" && ($1 == selector || index($1, selector)==1) { found=$1 } END { if (found != "") print found }`,
		"actions/setup-go requested ${requested}, but installed",
		"actions/setup-python requested ${requested}, but installed",
		"rerun with --github-runner",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("runtime shell missing %q", want)
		}
	}
}

func TestParseActionsHydrationState(t *testing.T) {
	got := parseActionsHydrationState("WORKSPACE=/home/runner/work/repo/repo\nRUN_ID=123\nJOB=hydrate\nENV_FILE=/home/runner/.crabbox/actions/cbx-123.env.sh\nSERVICES_FILE=/home/runner/.crabbox/actions/cbx-123.services\nREADY_AT=2026-05-01T00:00:00Z\n")
	if got.Workspace != "/home/runner/work/repo/repo" || got.RunID != "123" || got.Job != "hydrate" || got.EnvFile == "" || got.ServicesFile == "" || got.ReadyAt == "" {
		t.Fatalf("unexpected hydration state: %#v", got)
	}
}

func TestNormalizeActionsHydrationStateForNativeWindows(t *testing.T) {
	got := normalizeActionsHydrationStateForTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, actionsHydrationState{
		Workspace:    "/c/Users/runner/actions-runner/_work/openclaw/openclaw",
		EnvFile:      "/c/Users/runner/.crabbox/actions/cbx-123.env.sh",
		ServicesFile: "/c/Users/runner/.crabbox/actions/cbx-123.services",
	})
	if got.Workspace != `C:\Users\runner\actions-runner\_work\openclaw\openclaw` || got.EnvFile != `C:\Users\runner\.crabbox\actions\cbx-123.env.sh` || got.ServicesFile != `C:\Users\runner\.crabbox\actions\cbx-123.services` {
		t.Fatalf("unexpected normalized state: %#v", got)
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
		".crabbox/actions/cbx_123.local.sh",
		".crabbox/actions/cbx_123.local.log",
		".crabbox/actions/cbx_123.local.exit",
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

func TestWindowsActionsHydrationMarkerCommandsUseProgramData(t *testing.T) {
	target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	for name, command := range map[string]string{
		"read":  remoteReadActionsHydrationStateForTarget(target, "cbx_123"),
		"clear": remoteClearActionsHydrationStateForTarget(target, "cbx_123"),
		"stop":  remoteWriteActionsHydrationStopForTarget(target, "cbx_123"),
	} {
		decoded := decodePowerShellCommand(t, command)
		for _, want := range []string{`C:\ProgramData\crabbox\actions`, `cbx_123`} {
			if !strings.Contains(decoded, want) {
				t.Fatalf("%s command missing %q in:\n%s", name, want, decoded)
			}
		}
		if strings.Contains(decoded, `$HOME`) {
			t.Fatalf("%s command should not use profile-relative HOME paths:\n%s", name, decoded)
		}
	}
}

func TestActionsHydrationStopIsWrittenForNativeWindowsTargets(t *testing.T) {
	target := SSHTarget{Host: "203.0.113.10", TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	if !shouldWriteActionsHydrationStop("cbx_123", target) {
		t.Fatal("native Windows hydrated runners must receive the stop marker")
	}
	if shouldWriteActionsHydrationStop("", target) {
		t.Fatal("empty lease id should not write a stop marker")
	}
	target.Host = ""
	if shouldWriteActionsHydrationStop("cbx_123", target) {
		t.Fatal("missing SSH target should not write a stop marker")
	}
}

func TestLocalActionsRemoteCommandsQuoteLeasePaths(t *testing.T) {
	leaseID := "lease id;touch nope"
	for name, tc := range map[string]struct {
		got  string
		want string
	}{
		"install":    {remoteInstallLocalActionsHydrateScript(leaseID), "\"$HOME\"/" + shellQuote(actionsHydrationLocalScriptPath(leaseID))},
		"start":      {remoteStartLocalActionsHydrateScript(leaseID), "\"$HOME\"/" + shellQuote(actionsHydrationLocalLogPath(leaseID))},
		"foreground": {remoteRunLocalActionsHydrateScriptForeground(leaseID, time.Minute), "\"$HOME\"/" + shellQuote(actionsHydrationLocalLogPath(leaseID))},
		"status":     {remoteLocalActionsHydrateStatus(leaseID, "123"), "\"$HOME\"/" + shellQuote(actionsHydrationLocalExitPath(leaseID))},
	} {
		if strings.Contains(tc.got, "$HOME/.crabbox") {
			t.Fatalf("%s command should quote HOME-relative paths:\n%s", name, tc.got)
		}
		if !strings.Contains(tc.got, tc.want) {
			t.Fatalf("%s command missing quoted path %q:\n%s", name, tc.want, tc.got)
		}
	}
}

func TestRemoteRunLocalActionsHydrateScriptForegroundUsesTimeoutAndLog(t *testing.T) {
	got := remoteRunLocalActionsHydrateScriptForeground("cbx_123", 90*time.Second)
	for _, want := range []string{
		"timeout --signal=TERM --kill-after=10s \"$1\"",
		"'90s'",
		"tee",
		"PIPESTATUS",
		actionsHydrationLocalLogPath("cbx_123"),
		actionsHydrationLocalExitPath("cbx_123"),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("foreground hydrate command missing %q:\n%s", want, got)
		}
	}
}

func TestRemoteEnsureLocalActionsRunEnvPersistsNodePath(t *testing.T) {
	got := remoteEnsureLocalActionsRunEnv("cbx_123", "/home/crabbox/.crabbox/actions/cbx_123.env.sh")
	for _, want := range []string{
		"CRABBOX_LOCAL_ACTIONS_NODE_PATH",
		`export PATH="${RUNNER_TOOL_CACHE}/node/bin:$PATH"`,
		"/home/crabbox/.crabbox/actions/cbx_123.env.sh",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("ensure env command missing %q:\n%s", want, got)
		}
	}
}

func TestActionsRunURLIgnoresLocalRunIDs(t *testing.T) {
	repo := GitHubRepo{Owner: "example-org", Name: "my-app"}
	if got := actionsRunURL(repo, "local-cbx_123"); got != "" {
		t.Fatalf("actionsRunURL(local)=%q, want empty", got)
	}
	if got := actionsRunURL(repo, "123456"); got != "https://github.com/example-org/my-app/actions/runs/123456" {
		t.Fatalf("actionsRunURL=%q", got)
	}
}
