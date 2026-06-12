package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
		"Evidence after fix: Copied live console output from Crabbox `run_123`",
		"scenario pass login-regression 33.8s",
		"What was not tested: No public service.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("proof missing %q:\n%s", want, got)
		}
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

func TestRunStopCommandIncludesRoutingFlags(t *testing.T) {
	got := runStopCommand(Config{
		Provider:    "ssh",
		TargetOS:    targetWindows,
		WindowsMode: windowsModeNormal,
		Static: StaticConfig{
			Host:     "win dev.local",
			User:     "runner",
			Port:     "2022",
			WorkRoot: `C:\crabbox`,
		},
	}, "static_win-dev")
	for _, want := range []string{
		"--provider ssh",
		"--target windows",
		"--windows-mode normal",
		"--static-host 'win dev.local'",
		"--static-user runner",
		"--static-port 2022",
		"--static-work-root 'C:\\crabbox'",
		"static_win-dev",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandIncludesProviderRoutingFlags(t *testing.T) {
	got := runStopCommand(Config{
		Provider: "proxmox",
		TargetOS: targetLinux,
		Proxmox: ProxmoxConfig{
			APIURL:      "https://pve.example.test:8006",
			Node:        "pve1",
			InsecureTLS: true,
		},
	}, "cbx_123")
	for _, want := range []string{
		"--provider proxmox",
		"--proxmox-api-url https://pve.example.test:8006",
		"--proxmox-node pve1",
		"--proxmox-insecure-tls",
		"cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandIncludesXCPNgRoutingFlagsWithoutPassword(t *testing.T) {
	got := runStopCommand(Config{
		Provider: "xcp-ng",
		TargetOS: targetLinux,
		XCPNg: XCPNgConfig{
			APIURL:       "https://pool-user:pool-pass@xcp-ng.example.test/path?view=1",
			Username:     "root",
			Password:     "xcp-ng-secret",
			Template:     "ubuntu template",
			TemplateUUID: "tpl-0001",
			SR:           "default sr",
			SRUUID:       "sr-0001",
			Network:      "pool network",
			NetworkUUID:  "net-0001",
			Host:         "host-0001",
			User:         "runner",
			WorkRoot:     "/work/xcp-ng",
			InsecureTLS:  true,
		},
	}, "cbx_123")
	for _, want := range []string{
		"--provider xcp-ng",
		"--target linux",
		"--xcp-ng-api-url 'https://xcp-ng.example.test/path?view=1'",
		"--xcp-ng-username root",
		"--xcp-ng-template 'ubuntu template'",
		"--xcp-ng-template-uuid tpl-0001",
		"--xcp-ng-sr 'default sr'",
		"--xcp-ng-sr-uuid sr-0001",
		"--xcp-ng-network 'pool network'",
		"--xcp-ng-network-uuid net-0001",
		"--xcp-ng-host host-0001",
		"--xcp-ng-user runner",
		"--xcp-ng-work-root /work/xcp-ng",
		"--xcp-ng-insecure-tls",
		"cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
	for _, secret := range []string{"xcp-ng-secret", "pool-user", "pool-pass", "password"} {
		if strings.Contains(got, secret) {
			t.Fatalf("stop command leaked %q:\n%s", secret, got)
		}
	}
}

func TestRunStopCommandIncludesSemaphoreRoutingFlags(t *testing.T) {
	got := runStopCommand(Config{
		Provider: "semaphore",
		TargetOS: targetLinux,
		Semaphore: SemaphoreConfig{
			Host: "example.semaphoreci.com",
		},
	}, "sem_123")
	for _, want := range []string{
		"--provider semaphore",
		"--semaphore-host example.semaphoreci.com",
		"sem_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandIncludesExeDevRoutingFlags(t *testing.T) {
	got := runStopCommand(Config{
		Provider: "exe-dev",
		TargetOS: targetLinux,
		ExeDev: ExeDevConfig{
			ControlHost: "staging.exe.dev",
		},
	}, "cbx_123")
	for _, want := range []string{
		"--provider exe-dev",
		"--exe-dev-control-host staging.exe.dev",
		"cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandIncludesMorphRoutingFlags(t *testing.T) {
	cfg := Config{
		Provider: "morph",
		TargetOS: targetLinux,
		Morph: MorphConfig{
			APIKey:          "secret-morph-key",
			APIURL:          "https://morph.example.test",
			DeleteOnRelease: true,
		},
	}
	MarkDeleteOnReleaseExplicit(&cfg, "morph")
	got := runStopCommand(cfg, "cbx_123")
	for _, want := range []string{
		"--provider morph",
		"--morph-api-url https://morph.example.test",
		"--morph-delete-on-release=true",
		"--id cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "secret-morph-key") {
		t.Fatalf("stop command leaked Morph API key:\n%s", got)
	}
}

func TestRunStopCommandIncludesHostingerRoutingFlags(t *testing.T) {
	got := runStopCommand(Config{
		Provider: "hostinger",
		TargetOS: targetLinux,
		Hostinger: HostingerConfig{
			APIURL: "https://hostinger.example.test/api",
		},
	}, "1750645")
	for _, want := range []string{
		"--provider hostinger",
		"--hostinger-url https://hostinger.example.test/api",
		"--id 1750645",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandIncludesKubeVirtRoutingFlags(t *testing.T) {
	cfg := Config{
		Provider: "kubevirt",
		TargetOS: targetLinux,
		KubeVirt: KubeVirtConfig{
			Kubectl:         "/opt/bin/kubectl",
			Virtctl:         "/opt/bin/virtctl",
			Kubeconfig:      "/tmp/kube config",
			Context:         "dev",
			Namespace:       "team-vms",
			Template:        "/tmp/vm template.yaml",
			DeleteOnRelease: false,
		},
	}
	MarkDeleteOnReleaseExplicit(&cfg, "kubevirt")
	got := runStopCommand(cfg, "cbx_123")
	for _, want := range []string{
		"--provider kubevirt",
		"--kubevirt-kubectl /opt/bin/kubectl",
		"--kubevirt-virtctl /opt/bin/virtctl",
		"--kubevirt-kubeconfig '/tmp/kube config'",
		"--kubevirt-context dev",
		"--kubevirt-namespace team-vms",
		"--kubevirt-template '/tmp/vm template.yaml'",
		"--kubevirt-delete-on-release=false",
		"--id cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandOmitsAmbientReleasePolicy(t *testing.T) {
	for _, cfg := range []Config{
		{Provider: "incus", Incus: IncusConfig{DeleteOnRelease: true}},
		{Provider: "kubevirt", KubeVirt: KubeVirtConfig{DeleteOnRelease: true}},
		{Provider: "morph", Morph: MorphConfig{DeleteOnRelease: true}},
		{Provider: "namespace-devbox", Namespace: NamespaceConfig{DeleteOnRelease: true}},
	} {
		if got := runStopCommand(cfg, "cbx_123"); strings.Contains(got, "delete-on-release") {
			t.Fatalf("ambient release policy leaked into stop command:\n%s", got)
		}
	}
}

func TestRunStopCommandIncludesInheritedKubeconfigForKubeVirt(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/base.yaml:/tmp/cluster.yaml")
	got := runStopCommand(Config{
		Provider: "kubevirt",
		TargetOS: targetLinux,
		KubeVirt: KubeVirtConfig{
			Kubectl:   "kubectl",
			Virtctl:   "virtctl",
			Context:   "dev",
			Namespace: "team-vms",
		},
	}, "cbx_123")
	for _, want := range []string{
		"KUBECONFIG='/tmp/base.yaml:/tmp/cluster.yaml' crabbox stop",
		"--provider kubevirt",
		"--kubevirt-context dev",
		"--id cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
}

func TestRunStopCommandIncludesExternalRoutingFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got := runStopCommand(Config{
		Provider: "external",
		TargetOS: targetLinux,
		External: ExternalConfig{
			Command:  "node",
			Args:     []string{"/tmp/provider script.mjs", "--token", "secret-arg"},
			Config:   map[string]any{"namespace": "team-vms", "token": "secret-config"},
			WorkRoot: "/home/dev/crabbox",
		},
	}, "cbx_123")
	for _, want := range []string{
		"--provider external",
		"--external-routing-file",
		"--id cbx_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q:\n%s", want, got)
		}
	}
	for _, secret := range []string{"provider script.mjs", "secret-arg", "secret-config"} {
		if strings.Contains(got, secret) {
			t.Fatalf("stop command leaked %q:\n%s", secret, got)
		}
	}
}

func TestPopulateRunTimingMetadata(t *testing.T) {
	report := timingReportFromRunWithActionsURL("aws", "cbx_123", "blue-lobster", runTimings{}, 0, 0, "https://example.test/actions")
	populateRunTimingMetadata(&report,
		Config{Provider: "aws", IdleTimeout: 30},
		Repo{Root: "/repo/app"},
		Server{ServerType: struct {
			Name string `json:"name"`
		}{Name: "cpx31"}},
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
	if !safeArtifactGlob(".artifacts/qa-e2e/**") {
		t.Fatal("expected QA artifact glob to be accepted")
	}
	for _, glob := range []string{"", "../secret", "/etc/passwd", ".//etc/passwd", ".artifacts/$(id)", "-C/tmp", "{/,}etc/passwd", ".{.,}/secret"} {
		if safeArtifactGlob(glob) {
			t.Fatalf("expected unsafe glob %q to be rejected", glob)
		}
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
	script := runArtifactRequireScript(dir, []string{"reports/data/manifest.json", "reports/data/**/*.json"})
	out, err := exec.Command("bash", "-lc", script).CombinedOutput()
	if err != nil {
		t.Fatalf("require script failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"required artifact reports/data/manifest.json matched=1",
		"required artifact reports/data/**/*.json matched=2",
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
	err := validateRunArtifactGlobTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, []string{".artifacts/**"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, "native Windows") {
		t.Fatalf("error=%v, want native Windows artifact rejection", err)
	}
	if err := validateRunArtifactGlobTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, []string{".artifacts/**"}); err != nil {
		t.Fatalf("wsl2 artifact glob rejected: %v", err)
	}
}

func TestValidateRunArtifactGlobTargetRejectsMacOS(t *testing.T) {
	err := validateRunArtifactGlobTarget(SSHTarget{TargetOS: targetMacOS}, []string{".artifacts/**"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, "macOS") {
		t.Fatalf("error=%v, want macOS artifact rejection", err)
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
	script := `#!/bin/sh
cmd=""
for arg do cmd="$arg"; done
printf '%s\n---\n' "$cmd" >> "$CRABBOX_FAKE_SSH_LOG"
case "$cmd" in
  *"base64 <"*) printf 'YXJ0aWZhY3RzCg=='; exit 0 ;;
  *"base64 -d >"*) printf 'ok      node             v22.1.0\nok      pnpm             9.0.0\nok      docker-compose   Docker Compose version v2.27.0\n'; exit 0 ;;
  *"artifacts.tgz"*) printf 'warning: no artifact matches\n'; exit 0 ;;
esac
printf 'Live harness ready: baseUrl=http://127.0.0.1:28008/\nscenario pass login-regression 33.8s\nsuite pass 4/4 total=81.2s\n'
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
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
