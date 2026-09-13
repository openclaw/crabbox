package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestProfilePresetConfigExpansion(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte(`
profile: liveqa
profiles:
  liveqa:
    env:
      CI: "1"
    envAllow:
      - NODE_OPTIONS
    artifactGlobs:
      - ".artifacts/qa-e2e/**"
    doctor:
      enabled: true
      tools: [node, corepack, pnpm]
      nodeMajor: 22
      minDiskGB: 40
      requireDocker: true
      requireCompose: true
    presets:
      qa-live:
        command: "pnpm qa live --scenario {{scenario}} --fail-fast"
        env:
          QA_VITEST_NO_OUTPUT_TIMEOUT_MS: "900000"
        preflight: true
        proofTemplate: real-behavior-pr
    proofTemplates:
      real-behavior-pr:
        behaviorAddressed: "Live QA scenario {{scenario}}"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := applySelectedProfileConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	expansion, err := expandRunProfile(cfg, "qa-live", "login-regression", nil, nil, false, false, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(expansion.Command, " "); got != "pnpm qa live --scenario login-regression --fail-fast" {
		t.Fatalf("command = %q", got)
	}
	if expansion.Env["CI"] != "1" || expansion.Env["QA_VITEST_NO_OUTPUT_TIMEOUT_MS"] != "900000" {
		t.Fatalf("env not merged: %#v", expansion.Env)
	}
	if !expansion.Preflight || expansion.ProofTemplate != "real-behavior-pr" {
		t.Fatalf("preset metadata not applied: %#v", expansion)
	}
	if !expansion.Profile.Doctor.Enabled || expansion.Profile.Doctor.NodeMajor != 22 || !expansion.Profile.Doctor.RequireCompose {
		t.Fatalf("doctor not parsed: %#v", expansion.Profile.Doctor)
	}
}

func TestUnknownProfileRemainsALabel(t *testing.T) {
	cfg := Config{Profile: "project-check"}
	if err := applySelectedProfileConfig(&cfg); err != nil {
		t.Fatalf("unknown profile should remain a label: %v", err)
	}
	expansion, err := expandRunProfile(cfg, "", "", nil, []string{"true"}, false, false, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(expansion.Command) != 1 || expansion.Command[0] != "true" {
		t.Fatalf("command=%#v", expansion.Command)
	}
}

func TestDefaultProfilePresetsApply(t *testing.T) {
	cfg := Config{
		Profile: "default",
		Profiles: map[string]ProfileConfig{
			"default": {
				EnvAllow: []string{"CI"},
				Presets: map[string]PresetConfig{
					"smoke": {Command: "go test ./..."},
				},
				ProofTemplates: map[string]ProofTemplateConfig{
					"real": {BehaviorAddressed: "default profile proof"},
				},
			},
		},
	}
	if err := applySelectedProfileConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	expansion, err := expandRunProfile(cfg, "smoke", "", nil, nil, false, false, nil, "real")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(expansion.Command, " ") != "go test ./..." {
		t.Fatalf("command=%#v", expansion.Command)
	}
	if strings.Join(cfg.EnvAllow, ",") != "CI" {
		t.Fatalf("env allow=%#v", cfg.EnvAllow)
	}
	if _, ok := cfg.ProofTemplates["real"]; !ok {
		t.Fatalf("proof templates not merged: %#v", cfg.ProofTemplates)
	}
}

func TestProfileLegacyEnvAllowShape(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte(`
profiles:
  project-check:
    env:
      allow:
        - PROJECT_*
      CI: "1"
    envAllow:
      - NODE_OPTIONS
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	profile := cfg.Profiles["project-check"]
	if profile.Env["CI"] != "1" {
		t.Fatalf("env defaults not loaded: %#v", profile.Env)
	}
	if strings.Join(profile.EnvAllow, ",") != "PROJECT_*,NODE_OPTIONS" {
		t.Fatalf("env allow not merged: %#v", profile.EnvAllow)
	}
}

func TestProfileEnvAllowRespectsEnvironmentAndCLIPrecedence(t *testing.T) {
	for _, tt := range []struct {
		name    string
		profile string
	}{
		{
			name: "env allow",
			profile: `env:
      allow:
        - PROFILE_SECRET`,
		},
		{
			name: "legacy envAllow",
			profile: `envAllow:
      - PROFILE_SECRET`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, ".crabbox.yaml")
			t.Setenv("CRABBOX_CONFIG", cfgPath)
			t.Setenv("CRABBOX_ENV_ALLOW", "ENV_ONLY")
			contents := "profile: qa\nenv:\n  allow:\n    - GLOBAL_SECRET\nprofiles:\n  qa:\n    " + tt.profile + "\n"
			if err := os.WriteFile(cfgPath, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg, err := loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if err := applySelectedProfileConfig(&cfg); err != nil {
				t.Fatal(err)
			}
			applyRunEnvAllowFlags(&cfg, []string{"CLI_ONLY,CLI_SECOND"})

			if got := strings.Join(cfg.EnvAllow, ","); got != "ENV_ONLY,CLI_ONLY,CLI_SECOND" {
				t.Fatalf("env allow=%q", got)
			}
		})
	}
}

func TestEmptyReplacementListsPreserveAdditiveProfileAndSyncPolicy(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_CONFIG", "")
	t.Chdir(t.TempDir())
	writeReplacementListConfig(t, userConfigPath(), `profile: qa
env:
  allow: [BUILD_FLAVOR]
sync:
  exclude: [user-cache]
profiles:
  qa:
    env:
      allow: [PROFILE_FIRST]
    envAllow: [PROFILE_SECOND]
`)
	writeReplacementListConfig(t, "crabbox.yaml", `env:
  allow: []
sync:
  exclude: [repo-cache]
profiles:
  qa:
    envAllow: [PROFILE_THIRD, PROFILE_FIRST]
`)
	writeReplacementListConfig(t, ".crabbox.yaml", `sync:
  exclude: []
profiles:
  qa:
    env:
      allow: []
    envAllow: []
`)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := applySelectedProfileConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.EnvAllow, ","); got != "PROFILE_FIRST,PROFILE_SECOND,PROFILE_THIRD" {
		t.Fatalf("profile additions after top-level clear=%q", got)
	}
	wantExcludes := appendOrderedStrings(baseConfig().Sync.Excludes, "user-cache", "repo-cache")
	if strings.Join(cfg.Sync.Excludes, ",") != strings.Join(wantExcludes, ",") {
		t.Fatalf("empty sync.exclude cleared additive exclusions: %v", cfg.Sync.Excludes)
	}
	t.Setenv("CRABBOX_ENV_ALLOW", "BUILD_FLAVOR")
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := applySelectedProfileConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	applyRunEnvAllowFlags(&cfg, []string{"CI"})
	if got := strings.Join(cfg.EnvAllow, ","); got != "BUILD_FLAVOR,CI" {
		t.Fatalf("env replacement followed by CLI append=%q", got)
	}
}

func TestPresetCommandPreservesQuotedArguments(t *testing.T) {
	cfg := Config{
		Profile: "qa",
		Profiles: map[string]ProfileConfig{
			"qa": {
				Presets: map[string]PresetConfig{
					"grep": {Command: `pnpm test --grep "foo bar" 'baz qux' escaped\ arg`},
				},
			},
		},
	}
	if err := applySelectedProfileConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	expansion, err := expandRunProfile(cfg, "grep", "", nil, nil, false, false, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pnpm", "test", "--grep", "foo bar", "baz qux", "escaped arg"}
	if strings.Join(expansion.Command, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("command=%#v want=%#v", expansion.Command, want)
	}
}

func TestPresetCommandPreservesInlineEnvAssignment(t *testing.T) {
	cfg := Config{
		Profile: "qa",
		Profiles: map[string]ProfileConfig{
			"qa": {
				Presets: map[string]PresetConfig{
					"test": {Command: "NODE_OPTIONS=--max-old-space-size=4096 pnpm test"},
				},
			},
		},
	}
	if err := applySelectedProfileConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	expansion, err := expandRunProfile(cfg, "test", "", nil, nil, false, false, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"NODE_OPTIONS=--max-old-space-size=4096", "pnpm", "test"}
	if strings.Join(expansion.Command, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("command=%#v want=%#v", expansion.Command, want)
	}
	if !shouldUseShell(expansion.Command) {
		t.Fatalf("inline env command should run through shell: %#v", expansion.Command)
	}
	if got := runCommandShellString(expansion.Command, false); got != "NODE_OPTIONS='--max-old-space-size=4096' 'pnpm' 'test'" {
		t.Fatalf("shell command=%q", got)
	}
}

func TestPresetCommandTreatsVariablesAsArgValues(t *testing.T) {
	cfg := Config{
		Profile: "qa",
		Profiles: map[string]ProfileConfig{
			"qa": {
				Presets: map[string]PresetConfig{
					"lane":   {Command: "pnpm qa --scenario {{scenario}} --fail-fast"},
					"single": {Command: "{{scenario}}"},
					"quoted": {Command: `printf '%s\n' "&&"`},
				},
			},
		},
	}
	if err := applySelectedProfileConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	expansion, err := expandRunProfile(cfg, "lane", "&&", nil, nil, false, false, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pnpm", "qa", "--scenario", "&&", "--fail-fast"}
	if strings.Join(expansion.Command, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("command=%#v want=%#v", expansion.Command, want)
	}
	if !expansion.LiteralArgs[3] {
		t.Fatalf("literal args=%#v, want scenario arg marked literal", expansion.LiteralArgs)
	}
	if shouldUseShellWithLiteralArgs(expansion.Command, expansion.LiteralArgs) {
		t.Fatalf("placeholder value should not introduce shell operators: %#v", expansion.Command)
	}
	intent, err := ParseCommandIntent(expansion.Command, false, expansion.LiteralArgs)
	if err != nil || !slices.Equal(intent.Argv("bash", "-lc"), want) {
		t.Fatalf("profile command intent=%q err=%v", intent.Argv("bash", "-lc"), err)
	}
	if got := runCommandDisplayWithLiteralArgs(expansion.Command, false, expansion.LiteralArgs); got != "pnpm qa --scenario '&&' --fail-fast" {
		t.Fatalf("display=%q", got)
	}
	single, err := expandRunProfile(cfg, "single", "echo ok && false", nil, nil, false, false, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(single.Command) != 1 || single.Command[0] != "echo ok && false" || !single.LiteralArgs[0] {
		t.Fatalf("single command=%#v literal=%#v", single.Command, single.LiteralArgs)
	}
	if shouldUseShellWithLiteralArgs(single.Command, single.LiteralArgs) {
		t.Fatalf("single placeholder value should remain a literal arg: %#v", single.Command)
	}
	singleIntent, err := ParseCommandIntent(single.Command, false, single.LiteralArgs)
	if err != nil || !slices.Equal(singleIntent.Argv("bash", "-lc"), single.Command) {
		t.Fatalf("single profile intent=%q err=%v", singleIntent.Argv("bash", "-lc"), err)
	}
	if got := runCommandShellStringWithLiteralArgs(single.Command, false, single.LiteralArgs); got != "'echo ok && false'" {
		t.Fatalf("single shell command=%q", got)
	}
	quoted, err := expandRunProfile(cfg, "quoted", "", nil, nil, false, false, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	quotedWant := []string{"printf", `%s\n`, "&&"}
	if strings.Join(quoted.Command, "\x00") != strings.Join(quotedWant, "\x00") {
		t.Fatalf("quoted command=%#v want=%#v", quoted.Command, quotedWant)
	}
	if !quoted.LiteralArgs[1] || !quoted.LiteralArgs[2] {
		t.Fatalf("quoted literal args=%#v, want quoted words marked literal", quoted.LiteralArgs)
	}
	if shouldUseShellWithLiteralArgs(quoted.Command, quoted.LiteralArgs) {
		t.Fatalf("quoted operator should remain a literal arg: %#v", quoted.Command)
	}
}

func TestPresetCommandRejectsUnresolvedVariables(t *testing.T) {
	cfg := Config{
		Profile: "qa",
		Profiles: map[string]ProfileConfig{
			"qa": {
				Presets: map[string]PresetConfig{
					"lane": {Command: "pnpm qa --scenario {{scenario}}"},
				},
			},
		},
	}
	if err := applySelectedProfileConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	_, err := expandRunProfile(cfg, "lane", "", nil, nil, false, false, nil, "")
	if err == nil || !strings.Contains(err.Error(), "unresolved preset variable") {
		t.Fatalf("err=%v, want unresolved preset variable", err)
	}
}

func TestPresetEnvRejectsUnresolvedVariables(t *testing.T) {
	cfg := Config{
		Profile: "qa",
		Profiles: map[string]ProfileConfig{
			"qa": {
				Presets: map[string]PresetConfig{
					"lane": {
						Command: "pnpm qa",
						Env:     map[string]string{"QA_SCENARIO": "{{scenario}}"},
					},
				},
			},
		},
	}
	if err := applySelectedProfileConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	_, err := expandRunProfile(cfg, "lane", "", nil, nil, false, false, nil, "")
	if err == nil || !strings.Contains(err.Error(), "preset lane env QA_SCENARIO") {
		t.Fatalf("err=%v, want unresolved preset env variable", err)
	}
}

func TestPresetCommandPreservesDoubleQuotedLiteralBackslash(t *testing.T) {
	words, err := splitShellWords(`pnpm test --grep "\d+" "\\literal" "\$value"`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pnpm", "test", "--grep", `\d+`, `\literal`, "$value"}
	if strings.Join(words, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("words=%#v want=%#v", words, want)
	}
}

func TestPresetCommandPreservesConfiguredStringInShellMode(t *testing.T) {
	cfg := Config{
		Profile: "qa",
		Profiles: map[string]ProfileConfig{
			"qa": {
				Presets: map[string]PresetConfig{
					"grep": {Command: `pnpm test --grep "foo bar"`},
				},
			},
		},
	}
	if err := applySelectedProfileConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	expansion, err := expandRunProfile(cfg, "grep", "", nil, nil, true, false, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !expansion.Shell || len(expansion.Command) != 1 || expansion.Command[0] != `pnpm test --grep "foo bar"` {
		t.Fatalf("expansion=%#v", expansion)
	}
}

func TestRunCommandDisplayQuotesArgv(t *testing.T) {
	got := runCommandDisplay([]string{"pnpm", "test", "--grep", "foo bar", "a'b"}, false)
	want := `pnpm test --grep 'foo bar' 'a'\''b'`
	if got != want {
		t.Fatalf("display=%q want=%q", got, want)
	}
	autoShell := runCommandDisplay([]string{"pnpm", "install", "&&", "pnpm", "test"}, false)
	if autoShell != "'pnpm' 'install' && 'pnpm' 'test'" {
		t.Fatalf("auto shell display=%q", autoShell)
	}
	singleShell := runCommandDisplay([]string{`pnpm install && pnpm test`}, false)
	if singleShell != `pnpm install && pnpm test` {
		t.Fatalf("single shell display=%q", singleShell)
	}
	shell := runCommandDisplay([]string{`echo "$HOME" && printf ok`}, true)
	if shell != `echo "$HOME" && printf ok` {
		t.Fatalf("shell display=%q", shell)
	}
}

func TestProfileEnvNamesMustBeShellIdentifiers(t *testing.T) {
	cfg := Config{
		Profile: "qa",
		Profiles: map[string]ProfileConfig{
			"qa": {Env: map[string]string{"NODE-OPTIONS": "bad"}},
		},
	}
	err := applySelectedProfileConfig(&cfg)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, "valid shell environment name") {
		t.Fatalf("error=%v, want invalid env name", err)
	}
}

func TestExpandedPresetCommandRedactsEnvValues(t *testing.T) {
	got := formatExpandedPresetCommand("qa", []string{"pnpm", "test"}, false, map[string]string{
		"API_TOKEN": "secret-value",
		"CI":        "1",
	}, nil)
	if strings.Contains(got, "secret-value") || strings.Contains(got, "CI=1") {
		t.Fatalf("expanded command leaked env value: %s", got)
	}
	for _, want := range []string{"API_TOKEN=set len=12 secret=true", "CI=set", "pnpm test"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expanded command missing %q: %s", want, got)
		}
	}
}

func TestExpandedPresetCommandUsesShellDisplay(t *testing.T) {
	shell := formatExpandedPresetCommand("qa", []string{"pnpm install && pnpm test"}, true, nil, nil)
	if !strings.Contains(shell, "pnpm install && pnpm test") || strings.Contains(shell, "'&&'") {
		t.Fatalf("shell preset display=%q", shell)
	}
	autoShell := formatExpandedPresetCommand("qa", []string{"pnpm", "install", "&&", "pnpm", "test"}, false, nil, nil)
	if !strings.Contains(autoShell, "'pnpm' 'install' && 'pnpm' 'test'") || strings.Contains(autoShell, "'&&'") {
		t.Fatalf("auto-shell preset display=%q", autoShell)
	}
}

func TestRenderRunProofUsesTemplateAndLiveOutput(t *testing.T) {
	got, err := renderRunProof(proofRenderInput{
		Template: ProofTemplateConfig{
			BehaviorAddressed:     "Live QA {{scenario}}",
			RealEnvironmentTested: "AWS Crabbox `{{leaseId}}` (`{{slug}}`) with disposable services.",
			ExactSteps:            "{{command}}",
			ObservedResult:        "The scenario passed.",
			NotTested:             "No public service.",
		},
		Provider:   "aws",
		LeaseID:    "cbx_123",
		Slug:       "golden-barnacle",
		RunID:      "run_123",
		Command:    "pnpm qa live --scenario login-regression",
		LogExcerpt: "scenario pass login-regression 33.8s\nsuite pass 4/4 total=81.2s",
		Variables:  map[string]string{"scenario": "login-regression"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Real behavior proof",
		"Behavior addressed: Live QA login-regression",
		"Exact steps or command run:",
		"Evidence: Copied live console output from Crabbox `run_123`",
		"Observed result: The scenario passed.",
		"scenario pass login-regression 33.8s",
		"What was not tested: No public service.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("proof missing %q:\n%s", want, got)
		}
	}
}

func TestRenderRunProofUsesContextNeutralDefaultLabels(t *testing.T) {
	got, err := renderRunProof(proofRenderInput{Command: "make test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Exact steps or command run:",
		"Evidence: Copied live console output from Crabbox",
		"Observed result: The command completed successfully on the remote environment.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("proof missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"after this patch", "after fix"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("proof contains patch-specific claim %q:\n%s", forbidden, got)
		}
	}
}

func TestDelegatedRunProofUsesExecutionRunID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proof.md")
	_, err := writeDelegatedRunProof(path, "", Config{Provider: "e2b"}, RunResult{
		Provider:   "e2b",
		LeaseID:    "cbx_123",
		LogExcerpt: "passed",
		Session:    &RunSessionHandle{RunID: "run_provider"},
	}, RunRequest{RunID: "run_execution", Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Crabbox `run_execution`") || strings.Contains(string(data), "run_provider") {
		t.Fatalf("delegated proof did not correlate execution metadata run ID:\n%s", data)
	}
}

func TestRenderRunProofUsesSafeMarkdownFence(t *testing.T) {
	got, err := renderRunProof(proofRenderInput{
		Provider:   "aws",
		LeaseID:    "cbx_123",
		Command:    "make docs",
		LogExcerpt: "before\n```text\nnested\n```\nafter",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "````text\nbefore\n```text\nnested\n```\nafter\n````") {
		t.Fatalf("proof did not use a safe fence:\n%s", got)
	}
}

func TestRenderRunProofIncludesCaptureMetadataWithoutCapturedContent(t *testing.T) {
	unsafePath := "captures/stdout`\n# forged heading\x1b.md"
	got, err := renderRunProof(proofRenderInput{
		Provider:   "aws",
		LeaseID:    "cbx_123",
		Command:    "make test",
		LogExcerpt: "live stderr remains",
		Captures: []streamCaptureMetadata{
			{Label: "stdout", Path: unsafePath, Bytes: 19},
			{Label: "stderr", Path: "captures/stderr.bin", Bytes: 7},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"live stderr remains",
		`captured stream=stdout path="captures/stdout\x60\x0a\x23 forged heading\x1b.md" bytes=19`,
		`captured stream=stderr path="captures/stderr.bin" bytes=7`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("proof missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"stdout`", "\n# forged heading", "\x1b"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("proof contains unsafe capture path fragment %q:\n%s", forbidden, got)
		}
	}
}

func TestRenderRunProofUsesCaptureMetadataInsteadOfNoOutputSentinel(t *testing.T) {
	got, err := renderRunProof(proofRenderInput{
		LogExcerpt: "(no console output captured)",
		Captures: []streamCaptureMetadata{
			{Label: "stdout", Path: "/tmp/stdout.bin", Bytes: 0},
			{Label: "stderr", Path: "/tmp/stderr.bin", Bytes: 12},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stdout := `captured stream=stdout path="/tmp/stdout.bin" bytes=0`
	stderr := `captured stream=stderr path="/tmp/stderr.bin" bytes=12`
	if !strings.Contains(got, stdout+"\n"+stderr) {
		t.Fatalf("capture metadata order is not deterministic:\n%s", got)
	}
	if strings.Contains(got, "(no console output captured)") {
		t.Fatalf("proof kept no-output sentinel with explicit captures:\n%s", got)
	}
}

func TestRenderRunProofKeepsNoOutputSentinelWithoutCaptures(t *testing.T) {
	got, err := renderRunProof(proofRenderInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "(no console output captured)") {
		t.Fatalf("proof lost no-output sentinel:\n%s", got)
	}
}

func TestSelectProofLogExcerptStripsTerminalControl(t *testing.T) {
	got := selectProofLogExcerpt("\x1b[2KWaiting for testbox... queued\r\x1b[2KTestbox ready!\nblacksmith-proof-live-ok\n")
	for _, want := range []string{"Waiting for testbox... queued", "Testbox ready!", "blacksmith-proof-live-ok"} {
		if !strings.Contains(got, want) {
			t.Fatalf("excerpt missing %q:\n%s", want, got)
		}
	}
	if strings.ContainsAny(got, "\x1b\r") {
		t.Fatalf("excerpt contains terminal control bytes: %q", got)
	}
}

func TestRenderRunProofRejectsUnresolvedTemplateVariables(t *testing.T) {
	_, err := renderRunProof(proofRenderInput{
		Template: ProofTemplateConfig{
			BehaviorAddressed: "Live QA {{scenario}}",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "unresolved preset variable") {
		t.Fatalf("err=%v, want unresolved template variable", err)
	}
}

func TestRenderRunProofBuiltinsOverridePresetVariables(t *testing.T) {
	got, err := renderRunProof(proofRenderInput{
		Template: ProofTemplateConfig{
			RealEnvironmentTested: "{{provider}} {{leaseId}}",
			ExactSteps:            "{{command}}",
		},
		Provider: "aws",
		LeaseID:  "cbx_real",
		Command:  "pnpm qa live",
		Variables: map[string]string{
			"provider": "fake-provider",
			"leaseId":  "fake-lease",
			"command":  "fake command",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Real environment tested: aws cbx_real", "```sh\npnpm qa live\n```"} {
		if !strings.Contains(got, want) {
			t.Fatalf("proof missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "fake-provider") || strings.Contains(got, "fake command") {
		t.Fatalf("proof used preset variables over builtins:\n%s", got)
	}
}

func TestRenderRunProofAllowsLiteralHandlebarsFromCommand(t *testing.T) {
	got, err := renderRunProof(proofRenderInput{
		Template: ProofTemplateConfig{
			ExactSteps: "{{command}}",
		},
		Command: "echo '{{id}}'",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "echo '{{id}}'") {
		t.Fatalf("proof missing literal handlebars command:\n%s", got)
	}
}

func TestRenderRunProofFencesExactSteps(t *testing.T) {
	got, err := renderRunProof(proofRenderInput{
		Template: ProofTemplateConfig{
			ExactSteps: "{{command}}",
		},
		Command: "printf '`tick`\\n'\nmake test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "```sh\nprintf '`tick`\\n'\nmake test\n```") {
		t.Fatalf("proof did not fence exact steps safely:\n%s", got)
	}
}

func TestRunStopAfterPolicy(t *testing.T) {
	if err := validateRunStopAfterPolicy("sometimes"); err == nil {
		t.Fatal("expected invalid policy to fail")
	}
	if shouldReleaseRunLease(true, false, false, "", nil) != true {
		t.Fatal("default should release a new non-kept lease")
	}
	if shouldReleaseRunLease(false, false, false, "success", nil) != true {
		t.Fatal("success should release existing lease after success")
	}
	if shouldReleaseRunLease(true, false, false, "success", assertErr{}) != false {
		t.Fatal("success should keep failed lease")
	}
	if shouldReleaseRunLease(true, false, false, "never", nil) != false {
		t.Fatal("never should keep lease")
	}
}

func TestPopulateRunTimingMetadata(t *testing.T) {
	report := timingReportFromRunWithActionsURL("aws", "cbx_123", "blue-lobster", runTimings{}, 0, 0, "https://example.test/actions")
	populateRunTimingMetadata(&report,
		Config{Provider: "aws", IdleTimeout: 30},
		Repo{Root: "/repo/app"},
		Server{ServerType: ServerTypeInfo{Name: "cpx31"}},
		"cbx_123",
		"run_123",
		"/work/cbx_123/app",
		[]runArtifact{{Kind: "proof", Path: "/tmp/proof.md"}},
	)
	if report.RunID != "run_123" || report.MachineType != "cpx31" || report.RepoPath != "/repo/app" || report.Workdir != "/work/cbx_123/app" {
		t.Fatalf("metadata not populated: %#v", report)
	}
	if report.StopCommand == "" || !strings.Contains(report.StopCommand, "crabbox stop --provider aws --id cbx_123") {
		t.Fatalf("stop command=%q", report.StopCommand)
	}
	if len(report.Artifacts) != 1 || report.Artifacts[0].Path != "/tmp/proof.md" {
		t.Fatalf("artifacts=%#v", report.Artifacts)
	}
}

func TestProfileDoctorWorkdirUsesConfiguredWorkRoot(t *testing.T) {
	got := profileDoctorWorkdirForLease(Config{WorkRoot: "/work/crabbox", TargetOS: targetLinux}, "cbx_123")
	if got != "/work/crabbox" {
		t.Fatalf("workdir=%q", got)
	}
}

func TestSafeArtifactGlob(t *testing.T) {
	for _, glob := range []string{".artifacts/qa-e2e/**", "reports/result..json", "reports/.../proof.json", ".gitignore"} {
		if !safeArtifactGlob(glob) {
			t.Fatalf("expected safe glob %q to be accepted", glob)
		}
	}
	for _, glob := range []string{"", "..", "../secret", "reports/../secret", "/etc/passwd", ".//etc/passwd", ".artifacts/$(id)", "-C/tmp", "{/,}etc/passwd", ".{.,}/secret"} {
		if safeArtifactGlob(glob) {
			t.Fatalf("expected unsafe glob %q to be rejected", glob)
		}
	}
}

func TestRunArtifactGlobsValidateComponents(t *testing.T) {
	for flag, validate := range map[string]func([]string) error{
		"--artifact-glob":    ValidateRunArtifactGlobs,
		"--require-artifact": ValidateRequiredRunArtifactGlobs,
	} {
		t.Run(flag, func(t *testing.T) {
			for _, glob := range []string{"reports/result..json", "reports/.gitignore", "reports/.crabbox-report.json", "reports/.git-backup/**", "**/*"} {
				if err := validate([]string{glob}); err != nil {
					t.Fatalf("safe glob %q rejected: %v", glob, err)
				}
			}
			for _, glob := range []string{"..", "reports/../proof.json", ".git", ".git/config", ".crabbox/evidence/proof.json", "reports/.git/config", "./reports/.crabbox/**", "reports/**/.git/config"} {
				err := validate([]string{glob})
				var exitErr ExitError
				if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, flag) {
					t.Fatalf("unsafe glob %q error=%v, want flag-specific exit 2", glob, err)
				}
			}
		})
	}
}

func TestLocalRunArtifactPathUsesRepoRoot(t *testing.T) {
	repoRoot := filepath.Join(t.TempDir(), "repo")
	subdir := filepath.Join(repoRoot, "pkg")
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(subdir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	got := localRunArtifactPath(repoRoot, "run_123", "cbx_456", "run_123-artifacts.tgz")
	want := filepath.Join(repoRoot, ".crabbox", "runs", "run_123", "run_123-artifacts.tgz")
	if got != want {
		t.Fatalf("path=%q want %q", got, want)
	}
}

func TestRunArtifactCollectScriptExcludesEnvProfiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".crabbox", "env"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".crabbox", "env", "live.env"), []byte("API_TOKEN=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".crabbox", "metadata.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[remote]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".artifacts", "result.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	proof := []byte("{\"ok\":true}\n")
	if err := os.WriteFile(filepath.Join(dir, ".artifacts", "result..json"), proof, 0o644); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(dir, ".crabbox", "artifacts.tgz")
	script := runArtifactCollectScript(dir, ".crabbox/artifacts.tgz", []string{"./**"})
	if out, err := exec.Command("bash", "-lc", script).CombinedOutput(); err != nil {
		t.Fatalf("collect script failed: %v\n%s", err, out)
	}
	names := tarGzNames(t, archivePath)
	for _, name := range names {
		if strings.HasPrefix(name, ".crabbox") || strings.HasPrefix(name, ".git") || strings.Contains(name, "live.env") {
			t.Fatalf("archive included control state %q in %#v", name, names)
		}
	}
	if !stringSliceContains(names, ".artifacts/result.txt") {
		t.Fatalf("archive missing artifact file: %#v", names)
	}
	if got := readTarGzContents(t, archivePath)[".artifacts/result..json"]; !bytes.Equal(got, proof) {
		t.Fatalf("archive lost consecutive-dot artifact bytes: %q", got)
	}
}

func TestRunArtifactCollectScriptWarnsOnEmptyMatches(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, ".crabbox", "artifacts.tgz")
	script := runArtifactCollectScript(dir, ".crabbox/artifacts.tgz", []string{".artifacts/missing/**"})
	out, err := exec.Command("bash", "-lc", script).CombinedOutput()
	if err != nil {
		t.Fatalf("collect script failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "warning: no artifact matches") {
		t.Fatalf("missing empty artifact warning:\n%s", out)
	}
	if _, err := os.Stat(archivePath); err != nil {
		t.Fatalf("empty artifact archive was not written: %v", err)
	}
}

func TestRunArtifactRequireScriptMatchesRequiredArtifacts(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "reports", "data", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reports", "data", "manifest.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reports", "data", "nested", "quality.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reports", "data", "result..json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	globs := []string{"reports/data/manifest.json", "reports/data/result..json", "reports/data/**/*.json"}
	if err := validateRequiredRunArtifactGlobs(globs); err != nil {
		t.Fatal(err)
	}
	script := runArtifactRequireScript(dir, globs)
	out, err := exec.Command("bash", "-lc", script).CombinedOutput()
	if err != nil {
		t.Fatalf("require script failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"required artifact reports/data/manifest.json matched=1",
		"required artifact reports/data/result..json matched=1",
		"required artifact reports/data/**/*.json matched=3",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunArtifactRequireScriptFailsOnMissingArtifacts(t *testing.T) {
	dir := t.TempDir()
	script := runArtifactRequireScript(dir, []string{"reports/data/manifest.json"})
	out, err := exec.Command("bash", "-lc", script).CombinedOutput()
	if err == nil {
		t.Fatalf("require script unexpectedly passed:\n%s", out)
	}
	if !strings.Contains(string(out), "missing required artifact: reports/data/manifest.json") {
		t.Fatalf("missing required artifact output:\n%s", out)
	}
}

func TestRunArtifactRequireScriptSymlinkPolicy(t *testing.T) {
	for _, tt := range []struct {
		name     string
		glob     string
		setup    func(t *testing.T, dir, proof string)
		wantPass bool
	}{
		{
			name: "symlink to regular file",
			glob: "reports/data/proof.json",
			setup: func(t *testing.T, dir, proof string) {
				target := filepath.Join(dir, "reports", "data", "target.json")
				if err := os.WriteFile(target, []byte("{}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, proof); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			},
			wantPass: true,
		},
		{
			name: "dangling symlink",
			glob: "reports/data/proof.json",
			setup: func(t *testing.T, dir, proof string) {
				if err := os.Symlink(filepath.Join(dir, "missing.json"), proof); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			},
		},
		{
			name: "symlink to directory",
			glob: "reports/data/proof.json",
			setup: func(t *testing.T, dir, proof string) {
				target := filepath.Join(dir, "proof-dir")
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, proof); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			},
		},
		{
			name: "recursive dangling symlink",
			glob: "reports/data/**/*.json",
			setup: func(t *testing.T, dir, proof string) {
				if err := os.Symlink(filepath.Join(dir, "missing.json"), proof); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			proofDir := filepath.Join(dir, "reports", "data")
			if err := os.MkdirAll(proofDir, 0o755); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, dir, filepath.Join(proofDir, "proof.json"))

			out, err := exec.Command("bash", "-lc", runArtifactRequireScript(dir, []string{tt.glob})).CombinedOutput()
			if tt.wantPass {
				if err != nil || !strings.Contains(string(out), "matched=1") {
					t.Fatalf("error=%v output=%s", err, out)
				}
				return
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 8 {
				t.Fatalf("error=%v, want exit 8; output=%s", err, out)
			}
			if !strings.Contains(string(out), "missing required artifact: "+tt.glob) {
				t.Fatalf("missing required artifact output:\n%s", out)
			}
		})
	}
}

func TestRunArtifactCollectScriptSkipsDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	artifactDir := filepath.Join(dir, ".artifacts")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing.txt"), filepath.Join(artifactDir, "proof.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	archivePath := filepath.Join(dir, ".crabbox", "artifacts.tgz")
	out, err := exec.Command("bash", "-lc", runArtifactCollectScript(dir, ".crabbox/artifacts.tgz", []string{".artifacts/**"})).CombinedOutput()
	if err != nil {
		t.Fatalf("collect script failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "warning: no artifact matches") {
		t.Fatalf("missing empty artifact warning:\n%s", out)
	}
	if names := tarGzNames(t, archivePath); len(names) != 0 {
		t.Fatalf("archive contains dangling symlink: %#v", names)
	}
}

func TestRunArtifactCollectScriptLeafSymlinkPolicy(t *testing.T) {
	for _, tt := range []struct {
		name    string
		setup   func(t *testing.T, dir, link string)
		wantHit bool
	}{
		{
			name: "symlink to regular file",
			setup: func(t *testing.T, dir, link string) {
				target := filepath.Join(dir, "target.txt")
				if err := os.WriteFile(target, []byte("proof\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, link); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			},
			wantHit: true,
		},
		{
			name: "symlink to directory",
			setup: func(t *testing.T, dir, link string) {
				target := filepath.Join(dir, "target-dir")
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, link); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o755); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, dir, filepath.Join(dir, "artifacts", "proof.txt"))
			archivePath := filepath.Join(dir, ".crabbox", "artifacts.tgz")
			out, err := exec.Command("bash", "-lc", runArtifactCollectScript(dir, ".crabbox/artifacts.tgz", []string{"artifacts/proof.txt"})).CombinedOutput()
			if err != nil {
				t.Fatalf("collect script failed: %v\n%s", err, out)
			}
			names := tarGzNames(t, archivePath)
			if got := stringSliceContains(names, "artifacts/proof.txt"); got != tt.wantHit {
				t.Fatalf("archive names=%#v, matched=%t want %t; output=%s", names, got, tt.wantHit, out)
			}
		})
	}
}

func TestRunArtifactScriptsDoNotFollowIntermediateDirectorySymlinks(t *testing.T) {
	for _, glob := range []string{"safe/link/*.txt", "safe/**/*.txt"} {
		t.Run(glob, func(t *testing.T) {
			dir := t.TempDir()
			outside := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "safe"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "proof.txt"), []byte("outside\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dir, "safe", "link")); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}

			requireOut, requireErr := exec.Command("bash", "-lc", runArtifactRequireScript(dir, []string{glob})).CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(requireErr, &exitErr) || exitErr.ExitCode() != 8 {
				t.Fatalf("require error=%v, want exit 8; output=%s", requireErr, requireOut)
			}

			archivePath := filepath.Join(dir, ".crabbox", "artifacts.tgz")
			collectOut, collectErr := exec.Command("bash", "-lc", runArtifactCollectScript(dir, ".crabbox/artifacts.tgz", []string{glob})).CombinedOutput()
			if collectErr != nil {
				t.Fatalf("collect script failed: %v\n%s", collectErr, collectOut)
			}
			if names := tarGzNames(t, archivePath); len(names) != 0 {
				t.Fatalf("archive followed intermediate symlink for %q: %#v", glob, names)
			}
		})
	}
}

func TestRunArtifactScriptsExcludeProtectedComponentsAtAnyDepth(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{
		filepath.Join("output", ".git"),
		filepath.Join("output", ".crabbox"),
		filepath.Join("output", "visible"),
	} {
		if err := os.MkdirAll(filepath.Join(dir, path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{
		filepath.Join("output", ".git", "proof.txt"),
		filepath.Join("output", ".crabbox", "proof.txt"),
		filepath.Join("output", "visible", "proof.txt"),
	} {
		if err := os.WriteFile(filepath.Join(dir, path), []byte("proof\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	archivePath := filepath.Join(dir, ".crabbox", "artifacts.tgz")
	if out, err := exec.Command("bash", "-lc", runArtifactCollectScript(dir, ".crabbox/artifacts.tgz", []string{"output/**"})).CombinedOutput(); err != nil {
		t.Fatalf("collect script failed: %v\n%s", err, out)
	}
	names := tarGzNames(t, archivePath)
	if len(names) != 1 || names[0] != "output/visible/proof.txt" {
		t.Fatalf("protected paths were not excluded: %#v", names)
	}

	for _, glob := range []string{"output/.git/*.txt", "output/.crabbox/*.txt"} {
		out, err := exec.Command("bash", "-lc", runArtifactRequireScript(dir, []string{glob})).CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 8 {
			t.Fatalf("protected require glob %q error=%v, want exit 8; output=%s", glob, err, out)
		}
	}
}

func TestRunArtifactCollectScriptRecursiveGlobIncludesZeroDepthMatches(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".artifacts", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".artifacts", "result.xml"), []byte("<root />\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".artifacts", "nested", "result.xml"), []byte("<nested />\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(dir, ".crabbox", "artifacts.tgz")
	script := runArtifactCollectScript(dir, ".crabbox/artifacts.tgz", []string{".artifacts/**/*.xml"})
	if out, err := exec.Command("bash", "-lc", script).CombinedOutput(); err != nil {
		t.Fatalf("collect script failed: %v\n%s", err, out)
	}
	names := tarGzNames(t, archivePath)
	if !stringSliceContains(names, ".artifacts/result.xml") {
		t.Fatalf("archive missing zero-depth recursive match: %#v", names)
	}
	if !stringSliceContains(names, ".artifacts/nested/result.xml") {
		t.Fatalf("archive missing nested recursive match: %#v", names)
	}
}

func TestRunArtifactCollectScriptRecursiveGlobPreservesPathSegments(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{
		filepath.Join("foo", "bar"),
		filepath.Join("foo", "x", "bar"),
		filepath.Join("foo", "bar", "baz"),
	} {
		if err := os.MkdirAll(filepath.Join(dir, path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join("foo", "bar", "file.txt"):       "zero depth\n",
		filepath.Join("foo", "x", "bar", "file.txt"):  "nested\n",
		filepath.Join("foo", "bar", "baz", "qux.txt"): "outside\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	archivePath := filepath.Join(dir, ".crabbox", "artifacts.tgz")
	script := runArtifactCollectScript(dir, ".crabbox/artifacts.tgz", []string{"foo/**/bar/*.txt"})
	if out, err := exec.Command("bash", "-lc", script).CombinedOutput(); err != nil {
		t.Fatalf("collect script failed: %v\n%s", err, out)
	}
	names := tarGzNames(t, archivePath)
	if !stringSliceContains(names, "foo/bar/file.txt") {
		t.Fatalf("archive missing zero-depth path match: %#v", names)
	}
	if !stringSliceContains(names, "foo/x/bar/file.txt") {
		t.Fatalf("archive missing nested path match: %#v", names)
	}
	if stringSliceContains(names, "foo/bar/baz/qux.txt") {
		t.Fatalf("archive included path outside requested segment glob: %#v", names)
	}
}

func TestArtifactGlobSearchRootUsesLiteralPrefix(t *testing.T) {
	tests := map[string]string{
		".artifacts/**/*.xml": ".artifacts",
		"foo/**/bar/*.txt":    "foo",
		"foo/bar*.txt":        "foo",
		"foo*/*.txt":          ".",
		"**/*.xml":            ".",
		"result.xml":          ".",
	}
	for glob, want := range tests {
		if got := artifactGlobSearchRoot(glob); got != want {
			t.Fatalf("artifactGlobSearchRoot(%q)=%q, want %q", glob, got, want)
		}
	}
}

func TestRunArtifactNestedGlobAvoidsSiblingEnumeration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("artifact glob scripts require a POSIX target")
	}
	for _, mode := range []string{"required", "collect", "delegated", "delegated-file"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{
				"reports/data/proof.txt",
				"reports/data/nested/result.txt",
				"reports/sibling/unrelated.txt",
			} {
				writeFile(t, filepath.Join(dir, name), name)
			}
			globs := []string{"reports/data/**/*.txt"}
			script := runArtifactRequireScript(dir, globs)
			archive := filepath.Join(t.TempDir(), "archive.tgz")
			args := []string(nil)
			switch mode {
			case "collect":
				archive = filepath.Join(dir, ".crabbox", "artifacts.tgz")
				script = runArtifactCollectScript(dir, ".crabbox/artifacts.tgz", globs)
			case "delegated":
				script = DelegatedRunArtifactScript(globs, globs, 2, 1024*1024)
			case "delegated-file":
				parent, err := filepath.EvalSymlinks(filepath.Dir(archive))
				if err != nil {
					t.Fatal(err)
				}
				archive = filepath.Join(parent, "archive.tgz")
				script = DelegatedRunArtifactFileScript(globs, globs, 2, 1024*1024)
				args = []string{archive}
			}
			trace := filepath.Join(t.TempDir(), "enumerated")
			prefix := "find() { command find \"$@\" | tee -a " + shellQuote(trace) + "; }\n"
			cmd := exec.CommandContext(t.Context(), "bash", append([]string{"-c", prefix + script, "--"}, args...)...)
			cmd.Dir = dir
			cmd.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + t.TempDir()}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("generated %s script failed: %v\n%s", mode, err, out)
			}
			if mode != "collect" && !strings.Contains(string(out), "required artifact reports/data/**/*.txt matched=2") {
				t.Fatalf("required membership changed: %s", out)
			}
			if mode == "delegated" {
				_, encoded, begin := strings.Cut(string(out), DelegatedRunArtifactBeginMarker+"\n")
				encoded, _, end := strings.Cut(encoded, DelegatedRunArtifactEndMarker+"\n")
				if !begin || !end {
					t.Fatalf("missing archive framing: %s", out)
				}
				data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(encoded), ""))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(archive, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "required" {
				names := tarGzNames(t, archive)
				slices.Sort(names)
				want := []string{"reports/data/nested/result.txt", "reports/data/proof.txt"}
				if !slices.Equal(names, want) {
					t.Fatalf("archive membership=%q, want %q", names, want)
				}
			}
			enumerated, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(enumerated, []byte("reports/data/proof.txt\x00")) {
				t.Fatalf("real find did not enumerate the selected file: %q", enumerated)
			}
			if bytes.Contains(enumerated, []byte("reports/sibling/unrelated.txt\x00")) {
				t.Fatalf("generated %s script unnecessarily enumerated sibling file; selected membership is correct; find output=%q", mode, enumerated)
			}
		})
	}
}

func TestRunArtifactNestedGlobRequiresExactDirectoryEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("artifact glob scripts require a POSIX target")
	}
	for _, tt := range []struct {
		name, entry, wantRoot string
	}{
		{name: "mismatched spelling", entry: "reports/DATA", wantRoot: "reports"},
		{name: "exact spelling", entry: "reports/data", wantRoot: "reports/data"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "reports/data/proof.txt"), "selected")
			writeFile(t, filepath.Join(dir, "reports/sibling/unrelated.txt"), "unrelated")
			traceDir := t.TempDir()
			probe := filepath.Join(traceDir, "probe")
			roots := filepath.Join(traceDir, "roots")
			enumerated := filepath.Join(traceDir, "enumerated")
			// Only the shallow entry spelling is controlled. Both directories and
			// recursive discovery are real; this is not a case-insensitive volume.
			prefix := `find() {
  if [ "$#" -eq 8 ] && [ "$*" = 'reports -mindepth 1 -maxdepth 1 -type d -print0' ]; then
    printf 'probe\n' >> ` + shellQuote(probe) + `
    printf '%s\0' ` + shellQuote(tt.entry) + `
  else
    printf '%s\n' "$1" >> ` + shellQuote(roots) + `
    command find "$@" | tee -a ` + shellQuote(enumerated) + `
  fi
}
`
			script := runArtifactCollectScript(dir, ".crabbox/artifacts.tgz", []string{"reports/data/**/*.txt"})
			cmd := exec.CommandContext(t.Context(), "bash", "-c", prefix+script)
			cmd.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + t.TempDir()}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("controlled entry probe failed: %v\n%s", err, out)
			}
			probeCalls, err := os.ReadFile(probe)
			if err != nil || string(probeCalls) != "probe\n" {
				t.Fatalf("real candidate safety checks did not reach one shallow probe: %q, %v", probeCalls, err)
			}
			archive := filepath.Join(dir, ".crabbox", "artifacts.tgz")
			if names := tarGzNames(t, archive); !slices.Equal(names, []string{"reports/data/proof.txt"}) {
				t.Fatalf("controlled probe changed membership: %q", names)
			}
			if got := readTarGzContents(t, archive)["reports/data/proof.txt"]; string(got) != "selected" {
				t.Fatalf("selected file content changed: %q", got)
			}
			actualRoots, err := os.ReadFile(roots)
			if err != nil || string(actualRoots) != tt.wantRoot+"\n" {
				t.Fatalf("recursive find root=%q, want %q; error=%v", actualRoots, tt.wantRoot+"\n", err)
			}
			paths, err := os.ReadFile(enumerated)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(paths, []byte("reports/data/proof.txt\x00")) {
				t.Fatalf("selected file was not discovered by real find: %q", paths)
			}
			if got := bytes.Contains(paths, []byte("reports/sibling/unrelated.txt\x00")); got != (tt.wantRoot == "reports") {
				t.Fatalf("real sibling discovery=%t, want legacy traversal=%t: %q", got, tt.wantRoot == "reports", paths)
			}
		})
	}
}

func TestArtifactGlobNarrowSearchRootAddsOneLiteralDirectory(t *testing.T) {
	for glob, want := range map[string]string{
		"reports/data/**/*.txt":       "reports/data",
		"./reports/data/*.txt":        "reports/data",
		".artifacts/run/**":           ".artifacts/run",
		"reports/data/nested/?.txt":   "reports/data/nested",
		"reports/**":                  "",
		"**/*.txt":                    "",
		"reports/da*/**/*.txt":        "",
		"reports/data/pro*.txt":       "",
		"reports/data/proof.txt":      "",
		"reports/data//**/*.txt":      "",
		"reports/./data/**/*.txt":     "",
		"reports/data/./*.txt":        "",
		" reports/data/**/*.txt":      "",
		"reports/data/**/*.txt ":      "",
		"reports/../data/**/*.txt":    "",
		"reports/\u00a0data/**/*.txt": "",
	} {
		if got := artifactGlobNarrowSearchRoot(glob); got != want {
			t.Errorf("artifactGlobNarrowSearchRoot(%q)=%q, want %q", glob, got, want)
		}
	}
}

func TestRunArtifactNestedGlobPreservesSelection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("artifact glob scripts require a POSIX target")
	}
	dir := t.TempDir()
	for _, name := range []string{
		"reports/data/proof.txt",
		"reports/data/nested/result.txt",
		"reports/data/UPPER.TXT",
		"reports/Mixed/proof.txt",
		"reports/sibling/unrelated.txt",
		"reports/data/.git/private.txt",
		"reports/data/.crabbox/private.txt",
	} {
		writeFile(t, filepath.Join(dir, name), name)
	}
	for link, target := range map[string]string{
		"reports/data/leaf.txt":     "proof.txt",
		"reports/data/dangling.txt": "missing.txt",
		"reports/link":              "data",
	} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatalf("symlink fixture unavailable: %v", err)
		}
	}
	recursive := []string{"reports/data/leaf.txt", "reports/data/nested/result.txt", "reports/data/proof.txt"}
	for _, tt := range []struct {
		name, glob, setup, fallbackRoot string
		want                            []string
	}{
		{name: "recursive", glob: "reports/data/**/*.txt", want: recursive},
		{name: "relative prefix", glob: "./reports/data/**/*.txt", want: recursive},
		{name: "single level", glob: "reports/data/*.txt", want: []string{"reports/data/leaf.txt", "reports/data/proof.txt"}},
		{name: "partial filename", glob: "reports/data/pro*.txt", fallbackRoot: "reports/data", want: []string{"reports/data/proof.txt"}},
		{name: "partial directory", glob: "reports/da*/**/*.txt", fallbackRoot: "reports", want: recursive},
		{name: "double slash", glob: "reports/data//**/*.txt", fallbackRoot: "reports"},
		{name: "dot component", glob: "reports/./data/**/*.txt", fallbackRoot: "reports"},
		{name: "leading space", glob: " reports/data/**/*.txt", fallbackRoot: "reports"},
		{name: "case different directory", glob: "reports/mixed/**/*.txt", fallbackRoot: "reports"},
		{name: "case folded directory", glob: "reports/mixed/**/*.txt", setup: "shopt -s nocasematch\n", fallbackRoot: "reports", want: []string{"reports/Mixed/proof.txt"}},
		{name: "case folded filenames", glob: "reports/data/**/*.txt", setup: "shopt -s nocasematch\n", fallbackRoot: "reports", want: []string{"reports/data/UPPER.TXT", "reports/data/leaf.txt", "reports/data/nested/result.txt", "reports/data/proof.txt"}},
		{name: "shell glob settings", glob: "reports/data/**/*.txt", setup: "GLOBIGNORE='data:*.txt'\nshopt -s nocaseglob\nset -f\n", want: recursive},
		{name: "nocaseglob only", glob: "reports/data/**/*.txt", setup: "shopt -s nocaseglob\n", want: recursive},
		{name: "directory symlink", glob: "reports/link/**/*.txt", fallbackRoot: "reports"},
		{name: "missing directory", glob: "reports/absent/**/*.txt", fallbackRoot: "reports"},
		{name: "git component", glob: "reports/data/.git/**/*.txt", fallbackRoot: "reports/data"},
		{name: "control component", glob: "reports/data/.crabbox/**/*.txt", fallbackRoot: "reports/data"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			trace := filepath.Join(t.TempDir(), "roots")
			prefix := tt.setup + "find() { printf '%s\\n' \"$1\" >> " + shellQuote(trace) + "; command find \"$@\"; }\n"
			globs := []string{tt.glob, tt.glob}
			archive := filepath.Join(dir, ".crabbox", "artifacts.tgz")
			cmd := exec.CommandContext(t.Context(), "bash", "-c", prefix+runArtifactCollectScript(dir, ".crabbox/artifacts.tgz", globs))
			cmd.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + t.TempDir()}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("collect failed: %v\n%s", err, out)
			}
			names := tarGzNames(t, archive)
			slices.Sort(names)
			if !slices.Equal(names, tt.want) {
				t.Fatalf("archive membership=%q, want %q", names, tt.want)
			}
			if slices.Contains(names, "reports/data/leaf.txt") {
				header := readTarGzHeaders(t, archive)["reports/data/leaf.txt"]
				if header.Typeflag != tar.TypeSymlink || header.Linkname != "proof.txt" {
					t.Fatalf("leaf symlink changed: %+v", header)
				}
				if got := readTarGzContents(t, archive)["reports/data/proof.txt"]; string(got) != "reports/data/proof.txt" {
					t.Fatalf("selected contents changed: %q", got)
				}
			}
			if tt.fallbackRoot != "" {
				roots, err := os.ReadFile(trace)
				if err != nil {
					t.Fatal(err)
				}
				for _, root := range strings.Split(strings.TrimSpace(string(roots)), "\n") {
					if root != tt.fallbackRoot {
						t.Fatalf("legacy search root changed: %q, want %q", root, tt.fallbackRoot)
					}
				}
			}
			cmd = exec.CommandContext(t.Context(), "bash", "-c", tt.setup+runArtifactRequireScript(dir, globs))
			cmd.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + t.TempDir()}
			out, err = cmd.CombinedOutput()
			if len(tt.want) > 0 {
				want := fmt.Sprintf("required artifact %s matched=%d", tt.glob, len(tt.want))
				if err != nil || strings.Count(string(out), want) != 2 {
					t.Fatalf("required membership changed: %v\n%s", err, out)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 8 {
					t.Fatalf("missing required artifact: error=%v, want exit 8; output=%s", err, out)
				}
			}
		})
	}
}

func TestRunArtifactNestedGlobKeepsFilenameNormalization(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("artifact glob scripts require a POSIX target")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "reports/data/proof.txt\n"), "proof")
	cmd := exec.CommandContext(t.Context(), "bash", "-c", runArtifactRequireScript(dir, []string{"reports/data/*.txt"}))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "required artifact reports/data/*.txt matched=1") {
		t.Fatalf("required filename normalization changed: %v\n%s", err, out)
	}
}

func TestRunArtifactNestedGlobPreservesDelegatedLimits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("artifact glob scripts require a POSIX target")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "reports/data/a.txt"), "first")
	writeFile(t, filepath.Join(dir, "reports/data/b.txt"), "second")
	globs := []string{"reports/data/**/*.txt", "reports/data/*.txt"}
	for _, tt := range []struct {
		name     string
		required []string
		maxFiles int
		maxBytes int64
		wantCode int
	}{
		{name: "deduplicated at file cap", maxFiles: 2, maxBytes: 1024 * 1024},
		{name: "file cap", maxFiles: 1, maxBytes: 1024 * 1024, wantCode: 9},
		{name: "byte cap", maxFiles: 2, maxBytes: 1, wantCode: 9},
		{name: "required missing", required: []string{"reports/absent/**/*.txt"}, maxFiles: 2, maxBytes: 1024 * 1024, wantCode: 8},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, fileMode := range []bool{false, true} {
				script := DelegatedRunArtifactScript(tt.required, globs, tt.maxFiles, tt.maxBytes)
				args := []string(nil)
				directory, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				archive := filepath.Join(directory, "archive.tgz")
				if fileMode {
					script = DelegatedRunArtifactFileScript(tt.required, globs, tt.maxFiles, tt.maxBytes)
					args = []string{archive}
				}
				cmd := exec.CommandContext(t.Context(), "bash", append([]string{"-c", script, "--"}, args...)...)
				cmd.Dir = dir
				cmd.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + t.TempDir()}
				out, err := cmd.CombinedOutput()
				code := 0
				if err != nil {
					var exitErr *exec.ExitError
					if !errors.As(err, &exitErr) {
						t.Fatal(err)
					}
					code = exitErr.ExitCode()
				}
				if code != tt.wantCode {
					t.Fatalf("file mode=%t: exit=%d want=%d; output=%s", fileMode, code, tt.wantCode, out)
				}
				if code != 0 {
					if _, err := os.Stat(archive); !errors.Is(err, os.ErrNotExist) || strings.Contains(string(out), DelegatedRunArtifactBeginMarker) {
						t.Fatalf("failed collection published an archive: file mode=%t stat=%v output=%s", fileMode, err, out)
					}
				}
			}
		})
	}
}

func TestArtifactScriptGeneratorsNeverExpandUserGlobs(t *testing.T) {
	glob := "reports/*.json"
	scripts := []string{
		runArtifactRequireScript("/work", []string{glob}),
		runArtifactCollectScript("/work", ".crabbox/artifacts.tgz", []string{glob}),
		DelegatedRunArtifactScript([]string{glob}, []string{glob}, 16, 1024*1024),
	}
	for _, script := range scripts {
		if strings.Contains(script, "for f in "+glob) {
			t.Fatalf("generated script directly expands the artifact glob:\n%s", script)
		}
		if !strings.Contains(script, `find "$artifact_root"`) || !strings.Contains(script, "-print0") {
			t.Fatalf("generated script does not use shared find enumeration:\n%s", script)
		}
	}
}

func TestRemoteRunArtifactCommandsUseTargetTools(t *testing.T) {
	for _, tt := range []struct {
		name     string
		target   SSHTarget
		wantBash string
		wantRM   string
	}{
		{name: "linux", target: SSHTarget{TargetOS: targetLinux}, wantBash: "bash", wantRM: "rm"},
		{name: "macos", target: SSHTarget{TargetOS: targetMacOS}, wantBash: "/bin/bash", wantRM: "/bin/rm"},
		{name: "wsl2", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, wantBash: "bash", wantRM: "rm"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			commands := []string{
				remoteRequireArtifactGlobsCommand(tt.target, "/work", []string{"reports/proof.json"}),
				remoteCollectArtifactGlobsCommand(tt.target, "/work", ".crabbox/artifacts.tgz", []string{"reports/**"}),
			}
			wantCommands := []string{
				tt.wantBash + " -lc " + shellQuote(runArtifactRequireScript("/work", []string{"reports/proof.json"})),
				tt.wantBash + " -lc " + shellQuote(runArtifactCollectScript("/work", ".crabbox/artifacts.tgz", []string{"reports/**"})),
			}
			for i, command := range commands {
				if command != wantCommands[i] {
					t.Fatalf("command=%q, want %q", command, wantCommands[i])
				}
			}
			inputCommand := remoteRunArtifactShellInputCommand(tt.target)
			if want := tt.wantBash + " -lc " + shellQuote(tt.wantBash+" -s"); inputCommand != want {
				t.Fatalf("input command=%q, want %q", inputCommand, want)
			}

			remove := remoteRemoveRunArtifactCommand(tt.target, "/work", ".crabbox/artifacts.tgz")
			removeScript := "set -eu\ncd '/work'\n" + tt.wantRM + " -f -- '.crabbox/artifacts.tgz'"
			wantRemove := tt.wantBash + " -lc " + shellQuote(removeScript)
			if remove != wantRemove {
				t.Fatalf("remove command=%q, want %q", remove, wantRemove)
			}
		})
	}
}

func tarGzNames(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
	return names
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestValidateRunArtifactGlobTargetRejectsNativeWindows(t *testing.T) {
	target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	for _, err := range []error{
		validateRunArtifactGlobTarget(target, []string{".artifacts/**"}),
		validateRequiredRunArtifactGlobTarget(target, []string{"reports/proof.json"}),
	} {
		var exitErr ExitError
		if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, "native Windows") {
			t.Fatalf("error=%v, want native Windows artifact rejection", err)
		}
	}
	if err := validateRunArtifactGlobTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, []string{".artifacts/**"}); err != nil {
		t.Fatalf("wsl2 artifact glob rejected: %v", err)
	}
}

func TestValidateRunArtifactGlobTargetAllowsPOSIXTargets(t *testing.T) {
	targets := []SSHTarget{
		{TargetOS: targetLinux},
		{TargetOS: targetMacOS},
		{TargetOS: targetWindows, WindowsMode: windowsModeWSL2},
	}
	for _, target := range targets {
		if err := validateRunArtifactGlobTarget(target, []string{".artifacts/**"}); err != nil {
			t.Fatalf("artifact glob rejected for target=%+v: %v", target, err)
		}
		if err := validateRequiredRunArtifactGlobTarget(target, []string{"reports/proof.json"}); err != nil {
			t.Fatalf("required artifact rejected for target=%+v: %v", target, err)
		}
	}
}

func TestProfileValidationRejectsUnknownDoctorTool(t *testing.T) {
	cfg := Config{
		Profile: "qa",
		Profiles: map[string]ProfileConfig{
			"qa": {Doctor: DoctorProfileConfig{Enabled: true, Tools: []string{"pnppm"}}},
		},
	}
	err := applySelectedProfileConfig(&cfg)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, "unknown preflight tool") {
		t.Fatalf("error=%v, want unknown tool config error", err)
	}
}

func TestProfileValidationRejectsWindowsOnlyDoctorTool(t *testing.T) {
	cfg := Config{
		Profile: "qa",
		Profiles: map[string]ProfileConfig{
			"qa": {Doctor: DoctorProfileConfig{Enabled: true, Tools: []string{"pwsh"}}},
		},
	}
	err := applySelectedProfileConfig(&cfg)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, "not supported for POSIX profile doctor") {
		t.Fatalf("error=%v, want POSIX profile doctor rejection", err)
	}
}

func TestRemoteProfileDoctorCommandChecksSudoAndDecode(t *testing.T) {
	got := remoteProfileDoctorCommand("qa", DoctorProfileConfig{Enabled: true, Tools: []string{"sudo"}}, "/work/repo")
	for _, want := range []string{"base64 -d >", "|| exit 1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("doctor command missing %q:\n%s", want, got)
		}
	}
	script := profileDoctorScript(DoctorProfileConfig{Enabled: true, Tools: []string{"sudo"}, MinDiskGB: 40}, "/work/repo")
	for _, want := range []string{"sudo -n true", "missing or requires password", "df -Pk '/work/repo'", "'/work/repo'"} {
		if !strings.Contains(script, want) {
			t.Fatalf("doctor script missing %q:\n%s", want, script)
		}
	}
	dockerCLIScript := profileDoctorScript(DoctorProfileConfig{Enabled: true, Tools: []string{"docker"}}, "/work/repo")
	if !strings.Contains(dockerCLIScript, "check_cmd docker docker --version") || strings.Contains(dockerCLIScript, "docker-daemon") {
		t.Fatalf("docker tool should only check CLI availability:\n%s", dockerCLIScript)
	}
	dockerDaemonScript := profileDoctorScript(DoctorProfileConfig{Enabled: true, RequireDocker: true}, "/work/repo")
	for _, want := range []string{"check_cmd docker docker --version", "docker version >/tmp/crabbox-doctor.docker-daemon"} {
		if !strings.Contains(dockerDaemonScript, want) {
			t.Fatalf("Docker daemon script missing %q:\n%s", want, dockerDaemonScript)
		}
	}
}

func TestPreflightProofOutputPathRejectsCollisions(t *testing.T) {
	dir := t.TempDir()
	proof := filepath.Join(dir, "proof.md")
	if err := preflightProofOutputPath(proof, proof, "", nil); err == nil {
		t.Fatal("expected proof/stdout collision")
	}
	if err := preflightProofOutputPath(proof, "", proof, nil); err == nil {
		t.Fatal("expected proof/stderr collision")
	}
	if err := preflightProofOutputPath(proof, "", "", []string{"remote.log=" + proof}); err == nil {
		t.Fatal("expected proof/download collision")
	}
}

func TestRunCommandRejectsUnknownProofTemplate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(os.Getenv("CRABBOX_CONFIG"), []byte(`
profiles:
  qa:
    presets:
      smoke:
        command: "true"
        proofTemplate: missing
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--profile", "qa",
		"--preset", "smoke",
		"--emit-proof", filepath.Join(dir, "proof.md"),
	})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, "proof template") {
		t.Fatalf("error=%v, want proof template config error", err)
	}
}

func TestRunCommandDelegatedProviderEmitsProof(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`provider: blacksmith-testbox
`), 0o600); err != nil {
		t.Fatal(err)
	}
	proofPath := filepath.Join(dir, "proof.md")
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "blacksmith-testbox",
		"--emit-proof", proofPath,
		"--",
		"pnpm", "test",
	})
	if err != nil {
		t.Fatalf("runCommand error=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	proofData, err := os.ReadFile(proofPath)
	if err != nil {
		t.Fatal(err)
	}
	proof := string(proofData)
	for _, want := range []string{"## Real behavior proof", "blacksmith-testbox", "delegated test output", "suite pass"} {
		if !strings.Contains(proof, want) {
			t.Fatalf("proof missing %q:\n%s", want, proof)
		}
	}
	if !strings.Contains(stderr.String(), "artifact kind=proof") {
		t.Fatalf("stderr missing proof artifact line:\n%s", stderr.String())
	}
}

func TestRunCommandPresetProofArtifactE2E(t *testing.T) {
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	cfgPath := filepath.Join(dir, ".crabbox.yaml")
	proofPath := filepath.Join(dir, "proof.md")
	sshPath := filepath.Join(dir, "ssh")
	logPath := filepath.Join(dir, "ssh.log")
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	t.Setenv("CRABBOX_FAKE_SSH_LOG", logPath)
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
	t.Setenv("CRABBOX_FAKE_SSH_PORT", sshPort)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(cfgPath, []byte(`
profiles:
  liveqa:
    artifactGlobs:
      - ".artifacts/qa-e2e/**"
    doctor:
      enabled: true
      tools: [node, corepack, pnpm]
      nodeMajor: 22
      requireDocker: true
      requireCompose: true
    presets:
      qa-live:
        command: "pnpm qa live --scenario {{scenario}} --fail-fast"
        env:
          CI: "1"
        proofTemplate: real-behavior-pr
    proofTemplates:
      real-behavior-pr:
        behaviorAddressed: "Live QA scenario {{scenario}}"
        realEnvironmentTested: "AWS Crabbox {{leaseId}} ({{slug}}) with disposable services."
        observedResult: "The live QA scenario passed."
        notTested: "No public external service."
`), 0o600); err != nil {
		t.Fatal(err)
	}
	downloadedArtifacts := []byte("artifacts\n")
	script := `#!/bin/sh
cmd=""
for arg do cmd="$arg"; done
input="$(cat)"
printf '%s\n%s\n---\n' "$cmd" "$input" >> "$CRABBOX_FAKE_SSH_LOG"
case "$cmd
$input" in
  *"base64 <"*) printf '%s' ` + shellQuote(encodedRunDownloadPayload(int64(len(downloadedArtifacts)), downloadedArtifacts)) + `; exit 0 ;;
  *"base64 -d >"*) printf 'ok      node             v22.1.0\nok      pnpm             9.0.0\nok      docker-compose   Docker Compose version v2.27.0\n'; exit 0 ;;
  *"artifacts.tgz"*) printf 'warning: no artifact matches\n'; exit 0 ;;
esac
printf 'Live harness ready: baseUrl=http://127.0.0.1:28008/\nscenario pass login-regression 33.8s\nsuite pass 4/4 total=81.2s\n'
exit 0
`
	installWorkspaceOwnerAwareSSH(t, sshPath, script)
	var stdout, stderr bytes.Buffer
	err = (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), []string{
		"--provider", "run-env-profile-test",
		"--profile", "liveqa",
		"--no-sync",
		"--preset", "qa-live",
		"--scenario", "login-regression",
		"--emit-proof", proofPath,
		"--stop-after", "success",
	})
	if err != nil {
		t.Fatalf("runCommand error=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	proofData, err := os.ReadFile(proofPath)
	if err != nil {
		t.Fatal(err)
	}
	proof := string(proofData)
	for _, want := range []string{
		"## Real behavior proof",
		"Live QA scenario login-regression",
		"scenario pass login-regression 33.8s",
		"suite pass 4/4 total=81.2s",
	} {
		if !strings.Contains(proof, want) {
			t.Fatalf("proof missing %q:\n%s", want, proof)
		}
	}
	for _, want := range []string{"warning: no artifact matches", "artifact kind=artifact-glob", "lease cleanup stopped=true policy=success"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

type assertErr struct{}

func (assertErr) Error() string { return "boom" }
