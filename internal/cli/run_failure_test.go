package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestClassifyRunFailureStages(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		want  string
		retry string
	}{
		{name: "ssh", text: "timed out waiting for SSH on 127.0.0.1 during before command", want: "ssh", retry: "true"},
		{name: "provider auth", text: "<!doctype html><html><title>Cloudflare Access</title><body>login</body></html>", want: "provider_auth", retry: "false"},
		{name: "install", text: "pnpm install failed with ENOMEM", want: "install", retry: "unknown"},
		{name: "model", text: "model call failed: context window maximum tokens exceeded", want: "model_call", retry: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyRunFailure(1, tt.text, nil)
			if got.BlockedStage != tt.want || got.RetryLikely != tt.retry {
				t.Fatalf("ClassifyRunFailure()=%#v, want stage=%q retry=%q", got, tt.want, tt.retry)
			}
		})
	}
}

func TestClassifyRunFailureBlacksmithInfraStages(t *testing.T) {
	for _, tt := range []struct {
		name  string
		text  string
		stage string
	}{
		{
			name:  "shutdown dns",
			text:  "warning: blacksmith stop failed for tbx_1: request failed: Post https://backend.blacksmith.sh/api/shutdown: dial tcp: lookup backend.blacksmith.sh: i/o timeout",
			stage: "cleanup",
		},
		{
			name:  "sync guard",
			text:  "Blacksmith Testbox sync did not print a completion marker for 10m0s; terminating local runner.",
			stage: "sync",
		},
		{
			name:  "actions cancelled",
			text:  "Testbox ready\nGitHub Actions run cancelled",
			stage: "actions_cancelled",
		},
		{
			name:  "stalled after ready",
			text:  "Blacksmith Testbox ready\nBlacksmith post-ready stall: no output after ready",
			stage: "testbox_stalled_after_ready",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyRunFailure(255, tt.text, nil)
			if got.BlockedStage != tt.stage || got.RetryLikely != "true" {
				t.Fatalf("ClassifyRunFailure()=%#v, want stage=%q retry=true", got, tt.stage)
			}
		})
	}
}

func TestClassifyRunFailureDoesNotTreatUserTimeoutsAsBlacksmithInfra(t *testing.T) {
	got := ClassifyRunFailure(1, "Testbox ready\nFAIL ui.spec.ts\nError: Test timeout of 30000ms exceeded", nil)
	if got.BlockedStage != "unknown" {
		t.Fatalf("ClassifyRunFailure()=%#v, want unknown", got)
	}
}

func TestClassifyRunFailureDoesNotTreatUserCancellationAsBlacksmithInfra(t *testing.T) {
	got := ClassifyRunFailure(1, "Testbox ready\nFAIL queue.test.ts\nexpected canceled job state", nil)
	if got.BlockedStage != "unknown" {
		t.Fatalf("ClassifyRunFailure()=%#v, want unknown", got)
	}
}

func TestClassifyRunFailureUsesFinalPhaseAfterErrorSignatures(t *testing.T) {
	got := ClassifyRunFailure(1, "pnpm install completed\nunit tests failed", []TimingPhase{
		{Name: "install"},
		{Name: "test"},
	})
	if got.BlockedStage != "unknown" {
		t.Fatalf("ClassifyRunFailure()=%#v, want unknown", got)
	}
	got = ClassifyRunFailure(1, "test failed", []TimingPhase{
		{Name: "install"},
		{Name: "test"},
	})
	if got.BlockedStage != "unknown" {
		t.Fatalf("ClassifyRunFailure()=%#v, want unknown", got)
	}
	got = ClassifyRunFailure(1, "exit status 1", []TimingPhase{
		{Name: "test"},
		{Name: "install"},
	})
	if got.BlockedStage != "install" {
		t.Fatalf("ClassifyRunFailure()=%#v, want install", got)
	}
}

func TestClassifyRunFailureDoesNotTreatApplicationConnectionErrorsAsSSH(t *testing.T) {
	got := ClassifyRunFailure(1, "dial tcp 127.0.0.1:5432: connection refused", nil)
	if got.BlockedStage != "unknown" {
		t.Fatalf("ClassifyRunFailure()=%#v, want unknown", got)
	}
}

func TestClassifyRunFailureDoesNotTreatApplicationAuthFailuresAsProviderAuth(t *testing.T) {
	got := ClassifyRunFailure(1, "expected 200, got 401 Unauthorized", nil)
	if got.BlockedStage != "unknown" {
		t.Fatalf("ClassifyRunFailure()=%#v, want unknown", got)
	}
}

func TestClassifyRunFailureDoesNotTreatApplicationProviderAuthTextAsProviderAuth(t *testing.T) {
	for _, text := range []string{
		"FAIL src/provider-auth.test.ts\nexpected area provider_auth to be rendered",
		"normal test failure in provider auth settings panel",
	} {
		got := ClassifyRunFailure(1, text, nil)
		if got.BlockedStage != "unknown" {
			t.Fatalf("ClassifyRunFailure(%q)=%#v, want unknown", text, got)
		}
	}
}

func TestFormatRunSummaryIncludesFailureClassification(t *testing.T) {
	got := formatRunSummary(runTimings{
		sync:         time.Second,
		command:      2 * time.Second,
		blockedStage: "install",
		retryLikely:  "unknown",
	}, 3*time.Second, 1)
	for _, want := range []string{"blocked_stage=install", "retry_likely=unknown"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q in %q", want, got)
		}
	}
}

func TestTimingJSONIncludesFailureClassification(t *testing.T) {
	report := timingReportFromRun("aws", "cbx_123", "slug", runTimings{
		blockedStage: "ssh",
		retryLikely:  "true",
	}, time.Second, 1)
	var buf bytes.Buffer
	if err := writeTimingJSON(&buf, report); err != nil {
		t.Fatal(err)
	}
	var got TimingReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.BlockedStage != "ssh" || got.RetryLikely != "true" {
		t.Fatalf("classification not encoded: %#v", got)
	}
}

func TestPrintRunFailureDigest(t *testing.T) {
	stderrTail := newStreamTailBuffer(40)
	_, _ = stderrTail.Write([]byte("setup ok\nunit failed\n"))
	var buf bytes.Buffer
	printRunFailureDigest(&buf, runFailureDigestInput{
		LeaseID:        "cbx_123",
		Slug:           "blue-lobster",
		RunID:          "run_123",
		CommandDisplay: "go test ./...",
		Classification: FailureClassification{BlockedStage: "unknown", RetryLikely: "unknown"},
		Phases:         []TimingPhase{{Name: "test"}},
	}, newStreamTailBuffer(40), stderrTail, "", "")
	out := buf.String()
	for _, want := range []string{
		"failure digest",
		"phase: test",
		"area: user_command",
		"next: crabbox logs run_123 --tail 80",
		"next: crabbox doctor --from-run run_123",
		"next: crabbox run --id blue-lobster --fresh-sync -- go test ./...",
		"tail stderr:",
		"unit failed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("digest missing %q:\n%s", want, out)
		}
	}
}

func TestPrintRunFailureDigestExplainsUnavailableRunHistory(t *testing.T) {
	var buf bytes.Buffer
	printRunFailureDigest(&buf, runFailureDigestInput{
		LeaseID:               "cbx_123",
		Slug:                  "blue-lobster",
		RunHistoryUnavailable: true,
		CommandDisplay:        "go test ./...",
		Classification:        FailureClassification{BlockedStage: "unknown", RetryLikely: "unknown"},
	}, newStreamTailBuffer(40), newStreamTailBuffer(40), "", "")
	out := buf.String()
	for _, want := range []string{
		"run_history: unavailable; use lease-based recovery commands below",
		"next: crabbox ssh --id blue-lobster",
		"next: crabbox run --id blue-lobster --fresh-sync -- go test ./...",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("digest missing %q:\n%s", want, out)
		}
	}
	for _, unexpected := range []string{
		"crabbox logs run_",
		"crabbox doctor --from-run run_",
	} {
		if strings.Contains(out, unexpected) {
			t.Fatalf("digest should not include run-based command %q:\n%s", unexpected, out)
		}
	}
}

func TestRunFailureDigestRoutingArgsUseExternalRoutingFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	args := runFailureDigestRoutingArgs(Config{
		Provider: "external",
		External: ExternalConfig{
			Command: "provider-command",
			Args:    []string{"--token", "secret-value"},
			Config:  map[string]any{"token": "secret-config"},
		},
	}, "provider-id")
	got := strings.Join(args, " ")
	for _, want := range []string{"--provider external", "--external-routing-file"} {
		if !strings.Contains(got, want) {
			t.Fatalf("routing args missing %q: %s", want, got)
		}
	}
	for _, secret := range []string{"provider-command", "secret-value", "secret-config"} {
		if strings.Contains(got, secret) {
			t.Fatalf("routing args leaked %q: %s", secret, got)
		}
	}
}

func TestRunFailureDigestSSHRoutingArgsUseExternalRoutingFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	args := runFailureDigestSSHRoutingArgs(Config{
		Provider: "external",
		External: ExternalConfig{
			Command: "provider-command",
			Args:    []string{"--token", "secret-value"},
			Config:  map[string]any{"token": "secret-config"},
		},
	}, "cbx_abcdef123456")
	got := strings.Join(args, " ")
	for _, want := range []string{"--provider external", "--external-routing-file"} {
		if !strings.Contains(got, want) {
			t.Fatalf("ssh routing args missing %q: %s", want, got)
		}
	}
	for _, secret := range []string{"provider-command", "secret-value", "secret-config"} {
		if strings.Contains(got, secret) {
			t.Fatalf("ssh routing args leaked %q: %s", secret, got)
		}
	}
}

func TestPrintRunFailureDigestExplainsAndChainShortCircuit(t *testing.T) {
	stderrTail := newStreamTailBuffer(40)
	_, _ = stderrTail.Write([]byte("pnpm check failed\n"))
	var buf bytes.Buffer
	printRunFailureDigest(&buf, runFailureDigestInput{
		LeaseID:        "cbx_123",
		CommandDisplay: "pnpm check && pnpm test",
		ShellMode:      true,
		Classification: FailureClassification{BlockedStage: "unknown", RetryLikely: "unknown"},
	}, newStreamTailBuffer(40), stderrTail, "", "")
	out := buf.String()
	for _, want := range []string{
		"area: user_command",
		"shell_chain: pnpm check && pnpm test",
		"would_skip_if_left_failed: pnpm test",
		"chain_semantics: && only runs later segments if all earlier segments succeed",
		"next: crabbox run --id cbx_123 --fresh-sync --shell -- 'pnpm check && pnpm test'",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("digest missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "provider_auth") {
		t.Fatalf("digest should not mention provider_auth:\n%s", out)
	}
}

func TestPrintRunFailureDigestSuppressesMixedAndOrChain(t *testing.T) {
	var buf bytes.Buffer
	printRunFailureDigest(&buf, runFailureDigestInput{
		LeaseID:        "cbx_123",
		CommandDisplay: "pnpm build && pnpm test || pnpm cleanup",
		ShellMode:      true,
		Classification: FailureClassification{BlockedStage: "unknown", RetryLikely: "unknown"},
	}, newStreamTailBuffer(40), newStreamTailBuffer(40), "", "")
	out := buf.String()
	for _, unexpected := range []string{"shell_chain:", "would_skip_if_left_failed:", "chain_semantics:"} {
		if strings.Contains(out, unexpected) {
			t.Fatalf("digest should suppress mixed &&/|| chain note %q:\n%s", unexpected, out)
		}
	}
}

func TestPrintRunFailureDigestIncludesObservedPhases(t *testing.T) {
	var buf bytes.Buffer
	printRunFailureDigest(&buf, runFailureDigestInput{
		LeaseID:        "cbx_123",
		CommandDisplay: "pnpm verify",
		Classification: FailureClassification{BlockedStage: "unknown", RetryLikely: "unknown"},
		Phases: []TimingPhase{
			{Name: "user-command"},
			{Name: "check"},
			{Name: "test"},
		},
	}, newStreamTailBuffer(40), newStreamTailBuffer(40), "", "")
	out := buf.String()
	for _, want := range []string{
		"phase: test",
		"failed_phase: test",
		"observed_phases: user-command,check,test",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("digest missing %q:\n%s", want, out)
		}
	}
}

func TestPrintRunFailureDigestIncludesStructuredTestFailures(t *testing.T) {
	var buf bytes.Buffer
	printRunFailureDigest(&buf, runFailureDigestInput{
		LeaseID:        "cbx_123",
		CommandDisplay: "pnpm test",
		Classification: FailureClassification{BlockedStage: "unknown", RetryLikely: "unknown"},
		Results: &TestResultSummary{
			Files:    []string{"junit.xml"},
			Tests:    2,
			Failures: 1,
			Failed: []TestFailure{{
				File:    "src/example.test.ts",
				Name:    "renders",
				Kind:    "failure",
				Message: "expected true",
			}},
		},
	}, newStreamTailBuffer(40), newStreamTailBuffer(40), "", "")
	out := buf.String()
	for _, want := range []string{
		"test_results: files=1 tests=2 failures=1 errors=0 skipped=0",
		"failed_test: src/example.test.ts failure  renders - expected true",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("digest missing %q:\n%s", want, out)
		}
	}
}

func TestFailureDigestSuppressesScriptRetryCommand(t *testing.T) {
	commands := failureDigestNextCommands(runFailureDigestInput{
		LeaseID:        "cbx_123",
		CommandDisplay: "'--script=./smoke test.sh' arg",
		ScriptMode:     true,
		Classification: FailureClassification{RetryLikely: "unknown"},
	}, "unknown")
	for _, command := range commands {
		if strings.Contains(command, "crabbox run") {
			t.Fatalf("script retry command should be suppressed: %v", commands)
		}
	}
}

func TestFailureDigestRoutesNextCommands(t *testing.T) {
	commands := failureDigestNextCommands(runFailureDigestInput{
		Provider:       "aws",
		TargetOS:       targetWindows,
		WindowsMode:    windowsModeWSL2,
		LeaseID:        "cbx_123",
		CommandDisplay: "go test ./...",
		Classification: FailureClassification{RetryLikely: "unknown"},
		StopCommand:    "crabbox stop --provider aws --target windows --windows-mode wsl2 cbx_123",
	}, "unknown")
	joined := strings.Join(commands, "\n")
	for _, want := range []string{
		"crabbox ssh --provider aws --target windows --windows-mode wsl2 --id cbx_123",
		"crabbox run --provider aws --target windows --windows-mode wsl2 --id cbx_123 --fresh-sync -- go test ./...",
		"crabbox stop --provider aws --target windows --windows-mode wsl2 cbx_123",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("commands missing %q:\n%s", want, joined)
		}
	}
}

func TestFailureDigestRoutesProviderArgsToSSH(t *testing.T) {
	cfg := Config{Provider: "proxmox", Proxmox: ProxmoxConfig{APIURL: "https://pve.example"}}
	commands := failureDigestNextCommands(runFailureDigestInput{
		Provider:       "proxmox",
		LeaseID:        "cbx_123",
		CommandDisplay: "go test ./...",
		RoutingArgs:    runFailureDigestRoutingArgs(cfg, "cbx_123"),
		SSHRoutingArgs: runFailureDigestSSHRoutingArgs(cfg, "cbx_123"),
		Classification: FailureClassification{RetryLikely: "unknown"},
		StopCommand:    "crabbox stop --provider proxmox --proxmox-api-url https://pve.example cbx_123",
	}, "unknown")
	if len(commands) < 3 {
		t.Fatalf("commands=%v", commands)
	}
	for _, command := range commands[:3] {
		if !strings.Contains(command, "--proxmox-api-url") {
			t.Fatalf("command lost provider routing: %q\nall=%v", command, commands)
		}
	}
}

func TestFailureDigestPreservesInheritedKubeconfigForKubeVirt(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/base.yaml:/tmp/cluster.yaml")
	cfg := Config{
		Provider: "kubevirt",
		TargetOS: targetLinux,
		KubeVirt: KubeVirtConfig{
			Kubectl:   "kubectl",
			Virtctl:   "virtctl",
			Context:   "dev=west",
			Namespace: "team-vms",
		},
	}
	commands := failureDigestNextCommands(runFailureDigestInput{
		Provider:       "kubevirt",
		TargetOS:       targetLinux,
		LeaseID:        "cbx_123",
		CommandDisplay: "go test ./...",
		RoutingArgs:    runFailureDigestRoutingArgs(cfg, "cbx_123"),
		SSHRoutingArgs: runFailureDigestSSHRoutingArgs(cfg, "cbx_123"),
		Classification: FailureClassification{RetryLikely: "unknown"},
	}, "unknown")
	joined := strings.Join(commands, "\n")
	for _, want := range []string{
		"KUBECONFIG='/tmp/base.yaml:/tmp/cluster.yaml' crabbox ssh --provider kubevirt",
		"KUBECONFIG='/tmp/base.yaml:/tmp/cluster.yaml' crabbox run --provider kubevirt",
		"KUBECONFIG='/tmp/base.yaml:/tmp/cluster.yaml' crabbox stop --provider kubevirt",
		"--kubevirt-context dev=west",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("commands missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "dev='west' crabbox") {
		t.Fatalf("context value was hoisted as env assignment:\n%s", joined)
	}
}

func TestFailureDigestPreservesSealosRouting(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/base.yaml:/tmp/cluster.yaml")
	cfg := Config{
		Provider: "sealos-devbox",
		TargetOS: targetLinux,
		SealosDevbox: SealosDevboxConfig{
			Kubectl:        "/opt/bin/kubectl",
			Context:        "dev=west",
			Namespace:      "team-devboxes",
			Network:        "SSHGate",
			SSHGatewayHost: "ssh.example.test",
			SSHGatewayPort: "2222",
			SSHUser:        "devbox",
			WorkRoot:       "/home/devbox/project",
		},
	}
	commands := failureDigestNextCommands(runFailureDigestInput{
		Provider:       "sealos-devbox",
		TargetOS:       targetLinux,
		LeaseID:        "cbx_123",
		CommandDisplay: "go test ./...",
		RoutingArgs:    runFailureDigestRoutingArgs(cfg, "cbx_123"),
		SSHRoutingArgs: runFailureDigestSSHRoutingArgs(cfg, "cbx_123"),
		Classification: FailureClassification{RetryLikely: "unknown"},
	}, "unknown")
	joined := strings.Join(commands, "\n")
	for _, want := range []string{
		"KUBECONFIG='/tmp/base.yaml:/tmp/cluster.yaml' crabbox ssh --provider sealos-devbox",
		"KUBECONFIG='/tmp/base.yaml:/tmp/cluster.yaml' crabbox run --provider sealos-devbox",
		"KUBECONFIG='/tmp/base.yaml:/tmp/cluster.yaml' crabbox stop --provider sealos-devbox",
		"--sealos-devbox-context dev=west",
		"--sealos-devbox-ssh-gateway-host ssh.example.test",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("commands missing %q:\n%s", want, joined)
		}
	}
}

func TestRunFailureDigestIncludesXCPNgRoutingFlagsWithoutPassword(t *testing.T) {
	args := runFailureDigestRoutingArgs(Config{
		Provider: "xcp-ng",
		TargetOS: targetLinux,
		XCPNg: XCPNgConfig{
			APIURL:       "pool-user:pool-pass@xcp-ng.example.test/path?view=1",
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
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--provider xcp-ng",
		"--target linux",
		"--xcp-ng-api-url xcp-ng.example.test/path?view=1",
		"--xcp-ng-username root",
		"--xcp-ng-template ubuntu template",
		"--xcp-ng-template-uuid tpl-0001",
		"--xcp-ng-sr default sr",
		"--xcp-ng-sr-uuid sr-0001",
		"--xcp-ng-network pool network",
		"--xcp-ng-network-uuid net-0001",
		"--xcp-ng-host host-0001",
		"--xcp-ng-user runner",
		"--xcp-ng-work-root /work/xcp-ng",
		"--xcp-ng-insecure-tls",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("failure digest routing missing %q:\n%v", want, args)
		}
	}
	for _, secret := range []string{"xcp-ng-secret", "pool-user", "pool-pass", "password"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("failure digest routing leaked %q: %v", secret, args)
		}
	}
}

func TestFailureTailRedactsHTMLAuthBody(t *testing.T) {
	tail := newStreamTailBuffer(40)
	_, _ = tail.Write([]byte("<!doctype html><html><head><title>Cloudflare Access</title></head><body>login</body></html>\n"))
	var buf bytes.Buffer
	printFailureTail(&buf, "stderr", tail, "")
	out := buf.String()
	if !strings.Contains(out, "stderr tail redacted:") || !strings.Contains(out, "redacted auth_cloudflare_html response") {
		t.Fatalf("tail was not redacted: %q", out)
	}
	if strings.Contains(out, "<html>") || strings.Contains(out, "<body>") {
		t.Fatalf("tail leaked HTML body: %q", out)
	}
}

func TestFailureTailKeepsNonAuthHTMLBody(t *testing.T) {
	tail := newStreamTailBuffer(40)
	_, _ = tail.Write([]byte("<!doctype html><html><head><title>App Output</title></head><body>rendered page</body></html>\n"))
	var buf bytes.Buffer
	printFailureTail(&buf, "stdout", tail, "")
	out := buf.String()
	if strings.Contains(out, "tail redacted") || !strings.Contains(out, "<body>rendered page</body>") {
		t.Fatalf("non-auth HTML tail was changed: %q", out)
	}
}

func TestFailureTailKeepsApplicationAuthHTMLBody(t *testing.T) {
	tail := newStreamTailBuffer(40)
	_, _ = tail.Write([]byte("<!doctype html><html><head><title>App Login</title></head><body>401 Unauthorized access denied</body></html>\n"))
	var buf bytes.Buffer
	printFailureTail(&buf, "stdout", tail, "")
	out := buf.String()
	if strings.Contains(out, "tail redacted") || !strings.Contains(out, "401 Unauthorized") {
		t.Fatalf("application auth HTML tail was changed: %q", out)
	}
}

func TestSelectProofLogExcerptRedactsHTMLAuthBody(t *testing.T) {
	got := SelectProofLogExcerpt("<!doctype html><html><head><title>Cloudflare Access</title></head><body>login</body></html>")
	if !strings.Contains(got, "redacted auth_cloudflare_html response") {
		t.Fatalf("proof excerpt was not redacted: %q", got)
	}
}
