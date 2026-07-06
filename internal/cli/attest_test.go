package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func setAttestTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
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
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema_version", "generated_at", "provider", "lease_id", "slug", "run_id", "command", "exit_code", "command_ms", "actions_url", "log_sha256", "public_key", "signature"} {
		if _, ok := receipt[key]; !ok {
			t.Fatalf("receipt missing %s", key)
		}
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

func TestAttestDigestWriterSerializesConcurrentStreams(t *testing.T) {
	writer := newAttestDigestWriter()
	stdout := io.MultiWriter(io.Discard, writer)
	stderr := io.MultiWriter(io.Discard, writer)
	chunk := bytes.Repeat([]byte("a"), 1024)
	rounds := 64
	var wg sync.WaitGroup
	for _, stream := range []io.Writer{stdout, stderr} {
		wg.Add(1)
		go func(stream io.Writer) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if _, err := stream.Write(chunk); err != nil {
					t.Error(err)
				}
			}
		}(stream)
	}
	wg.Wait()
	expected := sha256.Sum256(bytes.Repeat([]byte("a"), 2*rounds*1024))
	if got := writer.sum(); got != "sha256:"+hex.EncodeToString(expected[:]) {
		t.Fatalf("unexpected digest %s", got)
	}
}

func TestAttestDigestWriterHashesMixedStreamsInArrivalOrder(t *testing.T) {
	writer := newAttestDigestWriter()
	stdout := io.MultiWriter(io.Discard, writer)
	stderr := io.MultiWriter(io.Discard, writer)
	if _, err := stdout.Write([]byte("out line\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.Write([]byte("err line\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := stdout.Write([]byte("done\n")); err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256([]byte("out line\nerr line\ndone\n"))
	if got := writer.sum(); got != "sha256:"+hex.EncodeToString(expected[:]) {
		t.Fatalf("unexpected digest %s", got)
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
