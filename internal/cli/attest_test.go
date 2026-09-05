package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func setAttestTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	return home
}

func writeTestReceipt(t *testing.T, keyPath string, in runReceiptInput) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "receipt.json")
	if _, err := writeRunReceipt(path, keyPath, in); err != nil {
		t.Fatalf("write receipt: %v", err)
	}
	return path
}

func runVerify(t *testing.T, path string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader("")}
	err := app.Run(context.Background(), []string{"verify", path})
	return stdout.String(), err
}

func fullReceiptInput() runReceiptInput {
	return runReceiptInput{
		Provider:   "hetzner",
		LeaseID:    "cbx_abc123",
		Slug:       "blue-lobster",
		RunID:      "run_42",
		Command:    "go test ./...",
		ExitCode:   0,
		CommandMs:  1234,
		ActionsURL: "https://github.com/example-org/my-app/actions/runs/7",
		LogSHA256:  "sha256:" + strings.Repeat("ab", 32),
	}
}

func TestAttestReceiptRoundTrip(t *testing.T) {
	setAttestTestHome(t)
	path := writeTestReceipt(t, "", fullReceiptInput())
	out, err := runVerify(t, path)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.HasPrefix(out, "PASS "+path+" signer=sha256:") {
		t.Fatalf("unexpected verify output: %q", out)
	}
	if !strings.Contains(out, " trust=self-signed") {
		t.Fatalf("verify output should expose the trust model: %q", out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt["schema_version"] != float64(attestReceiptSchemaVersion) {
		t.Fatalf("schema_version=%v, want existing v1 success format", receipt["schema_version"])
	}
	for _, key := range []string{"schema_version", "generated_at", "provider", "lease_id", "slug", "run_id", "command", "exit_code", "command_ms", "actions_url", "log_sha256", "public_key", "signature"} {
		if _, ok := receipt[key]; !ok {
			t.Fatalf("receipt missing %s", key)
		}
	}
}

func TestTerminalReceiptSignsNonzeroExitAndBindsCommand(t *testing.T) {
	setAttestTestHome(t)
	startedAt := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	endedAt := startedAt.Add(1500 * time.Millisecond)
	receipt, err := buildTerminalRunReceipt("", terminalRunReceiptInput{
		Provider:          "aws",
		LeaseID:           "cbx_abc123",
		Slug:              "blue-lobster",
		RunID:             "run_42",
		Command:           []string{"sh", "-c", "printf 'failed\n'; exit 17"},
		CommandDisplay:    "sh -c \"printf 'failed\\n'; exit 17\"",
		ExitCode:          17,
		SyncMs:            250,
		CommandMs:         1250,
		StartedAt:         startedAt,
		EndedAt:           endedAt,
		LogSHA256:         sha256Digest([]byte("failed\n")),
		RetainedLogSHA256: sha256Digest([]byte("failed\n")),
	})
	if err != nil {
		t.Fatalf("build terminal receipt: %v", err)
	}
	if receipt.ExitCode != 17 {
		t.Fatalf("exit_code=%d, want 17", receipt.ExitCode)
	}
	if receipt.CommandSHA256 != commandSHA256([]string{"sh", "-c", "printf 'failed\n'; exit 17"}) {
		t.Fatalf("command_sha256=%q", receipt.CommandSHA256)
	}
	path := filepath.Join(t.TempDir(), "terminal-receipt.json")
	if _, err := writeTerminalRunReceipt(path, receipt); err != nil {
		t.Fatal(err)
	}
	if out, err := runVerify(t, path); err != nil || !strings.Contains(out, " exit=17") {
		t.Fatalf("verify output=%q error=%v", out, err)
	}
	if err := verifyTerminalRunReceipt(receipt, terminalRunReceiptInput{
		Provider:          "aws",
		LeaseID:           "cbx_abc123",
		Slug:              "blue-lobster",
		RunID:             "run_42",
		Command:           []string{"sh", "-c", "printf 'failed\n'; exit 17"},
		ExitCode:          17,
		SyncMs:            250,
		CommandMs:         1250,
		StartedAt:         startedAt,
		EndedAt:           endedAt,
		LogSHA256:         sha256Digest([]byte("failed\n")),
		RetainedLogSHA256: sha256Digest([]byte("failed\n")),
	}); err != nil {
		t.Fatalf("verify terminal receipt: %v", err)
	}
}

func TestTerminalReceiptRejectsBindingMismatches(t *testing.T) {
	setAttestTestHome(t)
	startedAt := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	endedAt := startedAt.Add(time.Second)
	logDigest := sha256Digest([]byte("failed\n"))
	receipt, err := buildTerminalRunReceipt("", terminalRunReceiptInput{
		Provider:          "aws",
		LeaseID:           "cbx_abc123",
		Slug:              "blue-lobster",
		RunID:             "run_42",
		Command:           []string{"false"},
		CommandDisplay:    "false",
		ExitCode:          1,
		CommandMs:         1000,
		StartedAt:         startedAt,
		EndedAt:           endedAt,
		LogSHA256:         logDigest,
		RetainedLogSHA256: logDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := terminalRunReceiptInput{
		Provider:          "aws",
		LeaseID:           "cbx_abc123",
		Slug:              "blue-lobster",
		RunID:             "run_42",
		Command:           []string{"false"},
		ExitCode:          1,
		CommandMs:         1000,
		StartedAt:         startedAt,
		EndedAt:           endedAt,
		LogSHA256:         logDigest,
		RetainedLogSHA256: logDigest,
	}
	tests := map[string]func(*terminalRunReceiptInput){
		"provider":  func(binding *terminalRunReceiptInput) { binding.Provider = "gcp" },
		"lease":     func(binding *terminalRunReceiptInput) { binding.LeaseID = "cbx_other" },
		"no lease":  func(binding *terminalRunReceiptInput) { binding.LeaseID = "" },
		"no slug":   func(binding *terminalRunReceiptInput) { binding.Slug = "" },
		"run":       func(binding *terminalRunReceiptInput) { binding.RunID = "run_other" },
		"command":   func(binding *terminalRunReceiptInput) { binding.Command = []string{"true"} },
		"log":       func(binding *terminalRunReceiptInput) { binding.LogSHA256 = sha256Digest([]byte("other")) },
		"exit_code": func(binding *terminalRunReceiptInput) { binding.ExitCode = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			binding := base
			mutate(&binding)
			if err := verifyTerminalRunReceipt(receipt, binding); err == nil {
				t.Fatal("mismatched binding verified")
			}
		})
	}
}

func TestTerminalReceiptBoundsLongCommandDisplayWithoutLosingArgvBinding(t *testing.T) {
	setAttestTestHome(t)
	startedAt := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	command := []string{"printf", strings.Repeat("x", maxTerminalReceiptFieldBytes+1)}
	receipt, err := buildTerminalRunReceipt("", terminalRunReceiptInput{
		Provider:          "aws",
		LeaseID:           "cbx_abc123",
		Slug:              "blue-lobster",
		RunID:             "run_42",
		Command:           command,
		CommandDisplay:    strings.Repeat("x", maxTerminalReceiptFieldBytes+1),
		ExitCode:          1,
		CommandMs:         1000,
		StartedAt:         startedAt,
		EndedAt:           startedAt.Add(time.Second),
		LogSHA256:         sha256Digest(nil),
		RetainedLogSHA256: sha256Digest(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Command) > maxTerminalReceiptFieldBytes {
		t.Fatalf("command display bytes=%d", len(receipt.Command))
	}
	if receipt.CommandSHA256 != commandSHA256(command) {
		t.Fatalf("command_sha256=%q", receipt.CommandSHA256)
	}
	if !strings.Contains(receipt.Command, receipt.CommandSHA256) {
		t.Fatalf("bounded command display does not name exact digest: %q", receipt.Command)
	}
}

func TestTerminalReceiptCrossLanguageGolden(t *testing.T) {
	receipt := terminalRunReceipt{
		SchemaVersion:     2,
		ReceiptType:       "terminal",
		StartedAt:         "2026-08-23T10:00:00Z",
		EndedAt:           "2026-08-23T10:00:01Z",
		Provider:          "aws",
		LeaseID:           "cbx_abc123",
		Slug:              "blue-lobster",
		RunID:             "run_123",
		Command:           "sh -c \"printf '<ok>\\n'; exit 17\"",
		CommandSHA256:     "sha256:5bcb64bd81339235f6b76ab37c6f4e5febc1b787fcaf4e27783410b477c8ed5c",
		ExitCode:          17,
		SyncMs:            100,
		CommandMs:         900,
		DurationMs:        1000,
		LogSHA256:         "sha256:6da5b18878e110928643ebcc38dbb7552cf3a296eea56493e23edc2b93eecff8",
		RetainedLogSHA256: "sha256:6da5b18878e110928643ebcc38dbb7552cf3a296eea56493e23edc2b93eecff8",
		PublicKey:         "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg=",
		Signer:            "sha256:56475aa75463474c0285df5dbf2bcab73da651358839e9b77481b2eab107708c",
	}
	seed, err := base64.StdEncoding.DecodeString("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
	if err != nil {
		t.Fatal(err)
	}
	receipt.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), terminalReceiptSigningBytes(receipt)))
	if receipt.Signature != "3N80Q2zyXRS0g3V3phmRmegGwIkO6n9eiXXO+d36LKbfbAl7FDTAupodcy76YKk7WD5voQk9R5a9prqjNmxKDg==" {
		t.Fatalf("signature=%q", receipt.Signature)
	}
	if err := verifyTerminalRunReceiptSignature(receipt); err != nil {
		t.Fatal(err)
	}
}

func TestAttestReceiptOmitsEmptyOptionalFields(t *testing.T) {
	setAttestTestHome(t)
	path := writeTestReceipt(t, "", runReceiptInput{
		Provider:  "aws",
		LeaseID:   "cbx_def456",
		Command:   "true",
		ExitCode:  0,
		CommandMs: 5,
	})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"slug", "run_id", "actions_url", "log_sha256"} {
		if _, ok := receipt[key]; ok {
			t.Fatalf("receipt should omit empty %s", key)
		}
	}
	out, err := runVerify(t, path)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.HasPrefix(out, "PASS ") {
		t.Fatalf("unexpected verify output: %q", out)
	}
}

func TestVerifyRejectsTamperedReceipts(t *testing.T) {
	setAttestTestHome(t)
	cases := []struct {
		name     string
		mutate   func(t *testing.T, path string, receipt map[string]any) []byte
		wantCode int
		wantFail bool
	}{
		{
			name: "mutated exit code",
			mutate: func(t *testing.T, path string, receipt map[string]any) []byte {
				receipt["exit_code"] = 1
				return marshalReceipt(t, receipt)
			},
			wantCode: 1,
			wantFail: true,
		},
		{
			name: "mutated command",
			mutate: func(t *testing.T, path string, receipt map[string]any) []byte {
				receipt["command"] = "rm -rf ./dist"
				return marshalReceipt(t, receipt)
			},
			wantCode: 1,
			wantFail: true,
		},
		{
			name: "foreign public key",
			mutate: func(t *testing.T, path string, receipt map[string]any) []byte {
				pub, _, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				receipt["public_key"] = base64.StdEncoding.EncodeToString(pub)
				return marshalReceipt(t, receipt)
			},
			wantCode: 1,
			wantFail: true,
		},
		{
			name: "corrupt signature encoding",
			mutate: func(t *testing.T, path string, receipt map[string]any) []byte {
				receipt["signature"] = "!!!not-base64!!!"
				return marshalReceipt(t, receipt)
			},
			wantCode: 2,
		},
		{
			name: "missing signature",
			mutate: func(t *testing.T, path string, receipt map[string]any) []byte {
				delete(receipt, "signature")
				return marshalReceipt(t, receipt)
			},
			wantCode: 2,
		},
		{
			name: "unsupported schema version",
			mutate: func(t *testing.T, path string, receipt map[string]any) []byte {
				receipt["schema_version"] = 2
				return marshalReceipt(t, receipt)
			},
			wantCode: 2,
		},
		{
			name: "unknown field",
			mutate: func(t *testing.T, path string, receipt map[string]any) []byte {
				receipt["review_note"] = "not part of the receipt schema"
				return marshalReceipt(t, receipt)
			},
			wantCode: 2,
		},
		{
			name: "decimal exit code spelling",
			mutate: func(t *testing.T, path string, receipt map[string]any) []byte {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				return bytes.Replace(data, []byte(`"exit_code": 0`), []byte(`"exit_code": 0.0`), 1)
			},
			wantCode: 2,
		},
		{
			name: "invalid log digest",
			mutate: func(t *testing.T, path string, receipt map[string]any) []byte {
				receipt["log_sha256"] = "sha256:not-hex"
				return marshalReceipt(t, receipt)
			},
			wantCode: 2,
		},
		{
			name: "duplicate exit code key keeps signed value last",
			mutate: func(t *testing.T, path string, receipt map[string]any) []byte {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				return bytes.Replace(data, []byte(`"exit_code": 0`), []byte(`"exit_code": 1,
  "exit_code": 0`), 1)
			},
			wantCode: 2,
		},
		{
			name: "duplicate command key keeps signed value last",
			mutate: func(t *testing.T, path string, receipt map[string]any) []byte {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				return bytes.Replace(data, []byte(`"command": "go test ./..."`), []byte(`"command": "rm -rf ./dist",
  "command": "go test ./..."`), 1)
			},
			wantCode: 2,
		},
		{
			name: "truncated json",
			mutate: func(t *testing.T, path string, receipt map[string]any) []byte {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				return data[:len(data)/2]
			},
			wantCode: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTestReceipt(t, "", fullReceiptInput())
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var receipt map[string]any
			if err := json.Unmarshal(data, &receipt); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, tc.mutate(t, path, receipt), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := runVerify(t, path)
			var exitErr ExitError
			if !AsExitError(err, &exitErr) {
				t.Fatalf("expected ExitError, got %v", err)
			}
			if exitErr.Code != tc.wantCode {
				t.Fatalf("expected exit %d, got %d (%v)", tc.wantCode, exitErr.Code, err)
			}
			if tc.wantFail && !strings.Contains(out, "signature mismatch") {
				t.Fatalf("expected FAIL output, got %q", out)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		_, err := runVerify(t, filepath.Join(t.TempDir(), "absent.json"))
		var exitErr ExitError
		if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
			t.Fatalf("expected exit 2, got %v", err)
		}
	})
}

func TestVerifyRejectsSignedNonReceipt(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := key.Public().(ed25519.PublicKey)
	receipt := map[string]any{
		"payload":    "not a crabbox receipt",
		"public_key": base64.StdEncoding.EncodeToString(pub),
	}
	canonical, err := canonicalReceiptBytes(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt["signature"] = base64.StdEncoding.EncodeToString(ed25519.Sign(key, canonical))
	path := filepath.Join(t.TempDir(), "receipt.json")
	if err := os.WriteFile(path, marshalReceipt(t, receipt), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = runVerify(t, path)
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("expected exit 2 for signed non-receipt, got %v", err)
	}
}

func TestDelegatedRunReceiptOmitsMissingLeaseID(t *testing.T) {
	setAttestTestHome(t)
	path := filepath.Join(t.TempDir(), "receipt.json")
	result := RunResult{
		Provider:    "e2b",
		CommandText: "pnpm test",
		ExitCode:    0,
		Command:     1500 * time.Millisecond,
	}
	req := RunRequest{Command: []string{"pnpm", "test"}}
	artifact, err := writeDelegatedRunReceipt(path, "", Config{Provider: "e2b"}, result, req)
	if err != nil {
		t.Fatalf("write delegated receipt: %v", err)
	}
	if artifact.Kind != "receipt" {
		t.Fatalf("unexpected artifact kind %q", artifact.Kind)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"lease_id", "slug", "run_id", "actions_url", "log_sha256"} {
		if _, ok := receipt[key]; ok {
			t.Fatalf("delegated receipt should omit empty %s", key)
		}
	}
	if receipt["command"] != "pnpm test" {
		t.Fatalf("unexpected command %v", receipt["command"])
	}
	out, err := runVerify(t, path)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.HasPrefix(out, "PASS ") {
		t.Fatalf("unexpected verify output: %q", out)
	}
}

type attestOutcomeProvider struct {
	testStopReclaimProvider
	result RunResult
	err    error
}

func (p attestOutcomeProvider) Name() string { return "attest-outcome-test" }
func (p attestOutcomeProvider) Spec() ProviderSpec {
	spec := p.testStopReclaimProvider.Spec()
	spec.Name = p.Name()
	return spec
}
func (p attestOutcomeProvider) Configure(Config, Runtime) (Backend, error) {
	return attestOutcomeBackend{testDelegatedBackend{spec: p.Spec()}, p.result, p.err}, nil
}

type attestOutcomeBackend struct {
	testDelegatedBackend
	result RunResult
	err    error
}

func (b attestOutcomeBackend) Run(context.Context, RunRequest) (RunResult, error) {
	return b.result, b.err
}

func TestDelegatedAttestUsesNormalizedPrimaryOutcome(t *testing.T) {
	for _, scenario := range []string{"legacy command exit", "command plus cleanup cancellation", "command plus cleanup timeout", "provider timeout", "cleanup only"} {
		t.Run(scenario, func(t *testing.T) {
			clearConfigEnv(t)
			isolatedConfigPath(t)
			setAttestTestHome(t)
			t.Chdir(t.TempDir())
			result := RunResult{Provider: "attest-outcome-test", ExitCode: 42, CommandText: "exit 42"}
			var runErr error = ExitError{Code: 42, Message: "command exited 42"}
			wantReceipt := true
			switch scenario {
			case "command plus cleanup cancellation", "command plus cleanup timeout":
				result.Status, result.ErrorKind = RunStatusFailed, RunErrorCommandExit
				cleanupErr := context.Canceled
				if scenario == "command plus cleanup timeout" {
					cleanupErr = context.DeadlineExceeded
				}
				runErr = errors.Join(runErr, cleanupErr)
			case "provider timeout":
				runErr, wantReceipt = context.DeadlineExceeded, false
			case "cleanup only":
				result.Status, result.ErrorKind = RunStatusFailed, RunErrorProvider
				runErr, wantReceipt = errors.New("cleanup failed"), false
			}
			p := attestOutcomeProvider{result: result, err: runErr}
			RegisterProvider(p)
			t.Cleanup(func() { delete(providerRegistry, p.Name()) })
			receiptPath := filepath.Join(t.TempDir(), "receipt.json")
			app := App{Stdout: io.Discard, Stderr: io.Discard}
			err := app.Run(t.Context(), []string{"run", "--provider", p.Name(), "--no-sync", "--attest", receiptPath, "--", "true"})
			if !errors.Is(err, runErr) {
				t.Fatalf("primary error changed: %v", err)
			}
			data, err := os.ReadFile(receiptPath)
			if !wantReceipt {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unexpected provider-failure receipt: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("completed command receipt missing: %v", err)
			}
			receipt, err := decodeRunReceipt(data)
			if err != nil || receipt["exit_code"] != json.Number("42") {
				t.Fatalf("receipt=%#v err=%v", receipt, err)
			}
			if _, err := runVerify(t, receiptPath); err != nil {
				t.Fatalf("verify receipt: %v", err)
			}
		})
	}
}

func TestDelegatedRunReceiptRecordsNonzeroExit(t *testing.T) {
	setAttestTestHome(t)
	path := filepath.Join(t.TempDir(), "receipt.json")
	if _, err := writeDelegatedRunReceipt(path, "", Config{Provider: "e2b"}, RunResult{
		Provider:    "e2b",
		CommandText: "pnpm test",
		ExitCode:    17,
		Command:     1500 * time.Millisecond,
	}, RunRequest{Command: []string{"pnpm", "test"}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt["exit_code"] != float64(17) {
		t.Fatalf("exit_code=%v, want 17", receipt["exit_code"])
	}
	if out, err := runVerify(t, path); err != nil || !strings.HasPrefix(out, "PASS ") {
		t.Fatalf("verify output=%q error=%v", out, err)
	}
}

func TestDelegatedRunReceiptUsesSessionIdentityFallbacks(t *testing.T) {
	setAttestTestHome(t)
	path := filepath.Join(t.TempDir(), "receipt.json")
	result := RunResult{
		ExitCode:    0,
		CommandText: "pnpm test",
		Command:     1500 * time.Millisecond,
		Session: &RunSessionHandle{
			Provider:   "e2b",
			LeaseID:    "cbx_session",
			Slug:       "session-slug",
			RunID:      "run_session",
			ActionsURL: "https://github.com/example-org/my-app/actions/runs/7",
		},
	}
	artifact, err := writeDelegatedRunReceipt(path, "", Config{Provider: "fallback"}, result, RunRequest{})
	if err != nil {
		t.Fatalf("write delegated receipt: %v", err)
	}
	if artifact.Kind != "receipt" {
		t.Fatalf("unexpected artifact kind %q", artifact.Kind)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"provider":    "e2b",
		"lease_id":    "cbx_session",
		"slug":        "session-slug",
		"run_id":      "run_session",
		"actions_url": "https://github.com/example-org/my-app/actions/runs/7",
	}
	for key, value := range want {
		if receipt[key] != value {
			t.Fatalf("receipt[%q]=%v, want %q", key, receipt[key], value)
		}
	}
	if out, err := runVerify(t, path); err != nil || !strings.HasPrefix(out, "PASS ") {
		t.Fatalf("verify output=%q error=%v", out, err)
	}
}

func TestDelegatedRunReceiptPrefersExecutionRunID(t *testing.T) {
	setAttestTestHome(t)
	path := filepath.Join(t.TempDir(), "receipt.json")
	result := RunResult{
		Provider: "e2b",
		Session:  &RunSessionHandle{RunID: "run_provider"},
	}
	if _, err := writeDelegatedRunReceipt(path, "", Config{Provider: "e2b"}, result, RunRequest{RunID: "run_execution"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt["run_id"] != "run_execution" {
		t.Fatalf("run_id=%v, want execution metadata run ID", receipt["run_id"])
	}
}

func TestAttestReceiptCreatesMissingParentDirectories(t *testing.T) {
	setAttestTestHome(t)
	path := filepath.Join(t.TempDir(), "nested", "deeper", "receipt.json")
	if _, err := writeRunReceipt(path, "", fullReceiptInput()); err != nil {
		t.Fatalf("write receipt: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("expected receipt dir mode 0700, got %v", info.Mode().Perm())
		}
	}
	out, err := runVerify(t, path)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.HasPrefix(out, "PASS ") {
		t.Fatalf("unexpected verify output: %q", out)
	}
}

func TestAttestReceiptPathCollisions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipt.json")
	if err := preflightRunOutputCollisions("attest receipt", path, path, "", nil); err == nil {
		t.Fatal("expected capture stdout collision error")
	}
	if err := preflightRunOutputCollisions("attest receipt", path, "", path, nil); err == nil {
		t.Fatal("expected capture stderr collision error")
	}
	if err := preflightRunOutputCollisions("attest receipt", path, "", "", []string{"remote.log=" + path}); err == nil {
		t.Fatal("expected download collision error")
	}
}

func marshalReceipt(t *testing.T, receipt map[string]any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestEnsureAttestKeyMintsOncePrivately(t *testing.T) {
	setAttestTestHome(t)
	first, err := ensureAttestKey()
	if err != nil {
		t.Fatal(err)
	}
	second, err := ensureAttestKey()
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(second) {
		t.Fatal("expected the same key on repeated mint")
	}
	path, err := attestKeyPath()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("expected key mode 0600, got %v", info.Mode().Perm())
		}
		dirInfo, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if dirInfo.Mode().Perm() != 0o700 {
			t.Fatalf("expected key dir mode 0700, got %v", dirInfo.Mode().Perm())
		}
	}
}

func TestEnsureAttestKeyMintsOnceAcrossConcurrentCallers(t *testing.T) {
	setAttestTestHome(t)
	const callers = 32
	start := make(chan struct{})
	keys := make(chan ed25519.PrivateKey, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			key, err := ensureAttestKey()
			if err != nil {
				errs <- err
				return
			}
			keys <- key
		}()
	}
	close(start)
	wg.Wait()
	close(keys)
	close(errs)
	for err := range errs {
		t.Fatalf("ensure attest key: %v", err)
	}
	var first ed25519.PrivateKey
	count := 0
	for key := range keys {
		if first == nil {
			first = key
		} else if !first.Equal(key) {
			t.Fatal("concurrent callers returned different signing keys")
		}
		count++
	}
	if count != callers {
		t.Fatalf("received %d keys, want %d", count, callers)
	}
}

func TestPreflightAttestPathsProtectsReceiptAndSigningKey(t *testing.T) {
	setAttestTestHome(t)
	dir := t.TempDir()
	receiptPath := filepath.Join(dir, "receipt.json")
	keyPath := filepath.Join(dir, "signer.pem")
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	keySymlink := filepath.Join(dir, "signer-symlink.pem")
	symlinkAvailable := true
	if err := os.Symlink(keyPath, keySymlink); err != nil {
		symlinkAvailable = false
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
	}
	keyHardlink := filepath.Join(dir, "signer-hardlink.pem")
	if err := os.Link(keyPath, keyHardlink); err != nil {
		t.Fatal(err)
	}
	defaultKeyPath, err := attestKeyPath()
	if err != nil {
		t.Fatal(err)
	}
	danglingParentAlias := filepath.Join(dir, "future-key-dir")
	danglingAliasAvailable := true
	if err := os.Symlink(filepath.Dir(defaultKeyPath), danglingParentAlias); err != nil {
		danglingAliasAvailable = false
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
	}
	type pathCase struct {
		name string
		opts attestPathPreflight
		want string
	}
	cases := []pathCase{
		{
			name: "key requires receipt",
			opts: attestPathPreflight{KeyOverride: keyPath},
			want: "--attest-key requires --attest",
		},
		{
			name: "receipt cannot replace default key",
			opts: attestPathPreflight{Receipt: defaultKeyPath},
			want: "attest receipt and attest key paths must be different",
		},
		{
			name: "receipt cannot replace override key",
			opts: attestPathPreflight{Receipt: keyPath, KeyOverride: keyPath},
			want: "attest receipt and attest key paths must be different",
		},
		{
			name: "receipt cannot share timing store",
			opts: attestPathPreflight{Receipt: receiptPath, KeyOverride: keyPath, TimingRecord: receiptPath, TimingRecordEnabled: true},
			want: "attest receipt and timing record paths must be different",
		},
		{
			name: "capture cannot replace key",
			opts: attestPathPreflight{Receipt: receiptPath, KeyOverride: keyPath, CaptureStdout: keyPath},
			want: "attest key and capture stdout paths must be different",
		},
		{
			name: "download cannot replace key",
			opts: attestPathPreflight{Receipt: receiptPath, KeyOverride: keyPath, Downloads: []string{"build.log=" + keyPath}},
			want: "attest key and download build.log paths must be different",
		},
		{
			name: "hard link cannot alias key",
			opts: attestPathPreflight{Receipt: receiptPath, KeyOverride: keyPath, TimingRecord: keyHardlink, TimingRecordEnabled: true},
			want: "attest key and timing record paths must be different",
		},
		{
			name: "invalid override fails before run",
			opts: attestPathPreflight{Receipt: receiptPath, KeyOverride: filepath.Join(dir, "missing.pem")},
			want: "attest key:",
		},
	}
	if symlinkAvailable {
		cases = append(cases, pathCase{
			name: "symlink cannot alias key",
			opts: attestPathPreflight{Receipt: receiptPath, KeyOverride: keyPath, LeaseOutput: keySymlink},
			want: "attest key and lease output paths must be different",
		})
	}
	if danglingAliasAvailable {
		cases = append(cases, pathCase{
			name: "dangling symlink parent cannot alias future default key",
			opts: attestPathPreflight{Receipt: filepath.Join(danglingParentAlias, filepath.Base(defaultKeyPath))},
			want: "attest receipt and attest key paths must be different",
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := preflightAttestPaths(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
	if err := preflightAttestPaths(attestPathPreflight{
		Receipt:             receiptPath,
		KeyOverride:         keyPath,
		LeaseOutput:         filepath.Join(dir, "lease.json"),
		EmitProof:           filepath.Join(dir, "proof.md"),
		CaptureStdout:       filepath.Join(dir, "stdout.log"),
		CaptureStderr:       filepath.Join(dir, "stderr.log"),
		TimingRecord:        filepath.Join(dir, "timings.jsonl"),
		TimingRecordEnabled: true,
		Downloads:           []string{"build.log=" + filepath.Join(dir, "build.log")},
	}); err != nil {
		t.Fatalf("distinct attest paths: %v", err)
	}
}

func TestSameLocalOutputPathUsesFilesystemCaseSemantics(t *testing.T) {
	dir := t.TempDir()
	caseInsensitive, err := localPathCaseInsensitive(dir)
	if err != nil {
		t.Fatal(err)
	}
	same, err := sameLocalOutputPath(filepath.Join(dir, "Receipt.json"), filepath.Join(dir, "receipt.JSON"))
	if err != nil {
		t.Fatal(err)
	}
	if same != caseInsensitive {
		t.Fatalf("same=%t, filesystem case-insensitive=%t", same, caseInsensitive)
	}
}

func TestRunRejectsAttestKeyWithoutAttest(t *testing.T) {
	app := App{Stdout: io.Discard, Stderr: io.Discard, Stdin: strings.NewReader("")}
	err := app.runCommand(context.Background(), []string{"--attest-key", "signer.pem", "--sync-only"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(exitErr.Message, "--attest-key requires --attest") {
		t.Fatalf("expected --attest-key dependency error, got %v", err)
	}
}

func TestAttestKeyOverride(t *testing.T) {
	setAttestTestHome(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "signer.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeTestReceipt(t, keyPath, fullReceiptInput())
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt["public_key"] != base64.StdEncoding.EncodeToString(pub) {
		t.Fatal("receipt should embed the override public key")
	}
	out, err := runVerify(t, path)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.Contains(out, attestFingerprint(pub)) {
		t.Fatalf("expected fingerprint of override key, got %q", out)
	}
	defaultPath, err := attestKeyPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
		t.Fatal("override signing should not mint the default key")
	}
	if _, err := writeRunReceipt(filepath.Join(t.TempDir(), "r.json"), filepath.Join(t.TempDir(), "absent.pem"), fullReceiptInput()); err == nil {
		t.Fatal("expected error for missing override key")
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	ecPath := filepath.Join(t.TempDir(), "ec.pem")
	if err := os.WriteFile(ecPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeRunReceipt(filepath.Join(t.TempDir(), "r2.json"), ecPath, fullReceiptInput()); err == nil {
		t.Fatal("expected error for non ed25519 key")
	}
}
