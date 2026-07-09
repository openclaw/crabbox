package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const attestReceiptSchemaVersion = 1

var errDuplicateReceiptKey = errors.New("duplicate key")

type runReceiptInput struct {
	Provider   string
	LeaseID    string
	Slug       string
	RunID      string
	Command    string
	ExitCode   int
	CommandMs  int64
	ActionsURL string
	LogSHA256  string
}

func attestKeyPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "crabbox", "attest", "id_ed25519.pem"), nil
}

func ensureAttestKey() (ed25519.PrivateKey, error) {
	path, err := attestKeyPath()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err == nil {
		return loadAttestKey(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := ensurePrivateRunOutputDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	created, err := writePrivateRunOutputFileIfAbsent(path, encoded)
	if err != nil {
		return nil, err
	}
	if !created {
		return loadAttestKey(path)
	}
	return key, nil
}

type attestPathPreflight struct {
	Receipt             string
	KeyOverride         string
	LeaseOutput         string
	EmitProof           string
	CaptureStdout       string
	CaptureStderr       string
	TimingRecord        string
	TimingRecordEnabled bool
	Downloads           []string
}

type attestLocalPath struct {
	label string
	path  string
}

func preflightAttestPaths(opts attestPathPreflight) error {
	receiptPath := strings.TrimSpace(opts.Receipt)
	keyOverride := strings.TrimSpace(opts.KeyOverride)
	if receiptPath == "" {
		if keyOverride != "" {
			return exit(2, "--attest-key requires --attest")
		}
		return nil
	}
	keyPath := keyOverride
	if keyPath == "" {
		var err error
		keyPath, err = attestKeyPath()
		if err != nil {
			return exit(2, "attest key path: %v", err)
		}
	}
	outputs := []attestLocalPath{
		{label: "lease output", path: strings.TrimSpace(opts.LeaseOutput)},
		{label: "emit proof", path: strings.TrimSpace(opts.EmitProof)},
		{label: "capture stdout", path: strings.TrimSpace(opts.CaptureStdout)},
		{label: "capture stderr", path: strings.TrimSpace(opts.CaptureStderr)},
	}
	if opts.TimingRecordEnabled {
		outputs = append(outputs, attestLocalPath{label: "timing record", path: strings.TrimSpace(opts.TimingRecord)})
	}
	for _, spec := range opts.Downloads {
		download, err := parseRunDownloadSpec(spec)
		if err != nil {
			return err
		}
		outputs = append(outputs, attestLocalPath{label: "download " + download.Remote, path: download.Local})
	}
	for _, left := range []attestLocalPath{
		{label: "attest receipt", path: receiptPath},
		{label: "attest key", path: keyPath},
	} {
		for _, right := range outputs {
			if right.path == "" {
				continue
			}
			same, err := sameLocalOutputPath(left.path, right.path)
			if err != nil {
				return err
			}
			if same {
				return exit(2, "%s and %s paths must be different", left.label, right.label)
			}
		}
	}
	same, err := sameLocalOutputPath(receiptPath, keyPath)
	if err != nil {
		return err
	}
	if same {
		return exit(2, "attest receipt and attest key paths must be different")
	}
	if keyOverride != "" {
		if _, err := loadAttestKey(keyOverride); err != nil {
			return exit(2, "attest key: %v", err)
		}
	}
	return nil
}

func loadAttestKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("attest key %s is not PEM encoded", path)
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("attest key %s is not a PKCS8 private key", path)
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("attest key %s has trailing data", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("attest key %s is not an ed25519 key", path)
	}
	return key, nil
}

func resolveAttestKey(override string) (ed25519.PrivateKey, error) {
	if override != "" {
		return loadAttestKey(override)
	}
	return ensureAttestKey()
}

func attestFingerprint(pub ed25519.PublicKey) string {
	digest := sha256.Sum256(pub)
	return "sha256:" + hex.EncodeToString(digest[:])
}

type attestDigestWriter struct {
	mu     sync.Mutex
	digest hash.Hash
}

func newAttestDigestWriter() *attestDigestWriter {
	return &attestDigestWriter{digest: sha256.New()}
}

func (w *attestDigestWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.digest.Write(p)
}

func (w *attestDigestWriter) sum() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return "sha256:" + hex.EncodeToString(w.digest.Sum(nil))
}

func jsonHasDuplicateKeys(dec *json.Decoder) (bool, error) {
	token, err := dec.Token()
	if err != nil {
		return false, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return false, nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return false, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return false, fmt.Errorf("invalid object key")
			}
			if seen[key] {
				return true, nil
			}
			seen[key] = true
			duplicate, err := jsonHasDuplicateKeys(dec)
			if duplicate || err != nil {
				return duplicate, err
			}
		}
		_, err = dec.Token()
		return false, err
	case '[':
		for dec.More() {
			duplicate, err := jsonHasDuplicateKeys(dec)
			if duplicate || err != nil {
				return duplicate, err
			}
		}
		_, err = dec.Token()
		return false, err
	}
	return false, nil
}

func canonicalReceiptBytes(receipt map[string]any) ([]byte, error) {
	unsigned := make(map[string]any, len(receipt))
	for key, value := range receipt {
		if key == "signature" {
			continue
		}
		unsigned[key] = value
	}
	return json.Marshal(unsigned)
}

var attestReceiptFields = map[string]bool{
	"schema_version": true,
	"generated_at":   true,
	"provider":       true,
	"lease_id":       true,
	"slug":           true,
	"run_id":         true,
	"command":        true,
	"exit_code":      true,
	"command_ms":     true,
	"actions_url":    true,
	"log_sha256":     true,
	"public_key":     true,
	"signature":      true,
}

var attestRequiredReceiptFields = []string{
	"schema_version",
	"generated_at",
	"provider",
	"command",
	"exit_code",
	"command_ms",
	"public_key",
	"signature",
}

func decodeRunReceipt(data []byte) (map[string]any, error) {
	duplicate, err := jsonHasDuplicateKeys(json.NewDecoder(bytes.NewReader(data)))
	if err != nil {
		return nil, err
	}
	if duplicate {
		return nil, errDuplicateReceiptKey
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var receipt map[string]any
	if err := dec.Decode(&receipt); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	if err := validateRunReceipt(receipt); err != nil {
		return nil, err
	}
	return receipt, nil
}

func validateRunReceipt(receipt map[string]any) error {
	if len(receipt) == 0 {
		return fmt.Errorf("empty receipt")
	}
	for key := range receipt {
		if !attestReceiptFields[key] {
			return fmt.Errorf("unknown field %q", key)
		}
	}
	for _, key := range attestRequiredReceiptFields {
		if _, ok := receipt[key]; !ok {
			return fmt.Errorf("missing %s", key)
		}
	}
	schemaVersion, err := receiptInt64(receipt, "schema_version")
	if err != nil {
		return err
	}
	if schemaVersion != attestReceiptSchemaVersion {
		return fmt.Errorf("unsupported schema_version %d", schemaVersion)
	}
	generatedAt, err := receiptString(receipt, "generated_at")
	if err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339, generatedAt); err != nil {
		return fmt.Errorf("invalid generated_at")
	}
	for _, key := range []string{"provider", "command", "public_key", "signature"} {
		if _, err := receiptString(receipt, key); err != nil {
			return err
		}
	}
	for _, key := range []string{"lease_id", "slug", "run_id", "actions_url"} {
		if _, ok := receipt[key]; ok {
			if _, err := receiptString(receipt, key); err != nil {
				return err
			}
		}
	}
	exitCode, err := receiptInt64(receipt, "exit_code")
	if err != nil {
		return err
	}
	if exitCode < 0 {
		return fmt.Errorf("invalid exit_code")
	}
	commandMs, err := receiptInt64(receipt, "command_ms")
	if err != nil {
		return err
	}
	if commandMs < 0 {
		return fmt.Errorf("invalid command_ms")
	}
	if _, ok := receipt["log_sha256"]; ok {
		logDigest, err := receiptString(receipt, "log_sha256")
		if err != nil {
			return err
		}
		if !validSHA256Digest(logDigest) {
			return fmt.Errorf("invalid log_sha256")
		}
	}
	return nil
}

func receiptString(receipt map[string]any, key string) (string, error) {
	value, ok := receipt[key].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("invalid %s", key)
	}
	return value, nil
}

func receiptInt64(receipt map[string]any, key string) (int64, error) {
	number, ok := receipt[key].(json.Number)
	if !ok {
		return 0, fmt.Errorf("invalid %s", key)
	}
	value, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return value, nil
}

func validSHA256Digest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == sha256.Size
}

func writeRunReceipt(path, keyPath string, in runReceiptInput) (runArtifact, error) {
	key, err := resolveAttestKey(keyPath)
	if err != nil {
		return runArtifact{}, exit(2, "attest key: %v", err)
	}
	pub := key.Public().(ed25519.PublicKey)
	receipt := map[string]any{
		"schema_version": attestReceiptSchemaVersion,
		"generated_at":   time.Now().UTC().Format(time.RFC3339),
		"provider":       in.Provider,
		"command":        in.Command,
		"exit_code":      in.ExitCode,
		"command_ms":     in.CommandMs,
		"public_key":     base64.StdEncoding.EncodeToString(pub),
	}
	if in.LeaseID != "" {
		receipt["lease_id"] = in.LeaseID
	}
	if in.Slug != "" {
		receipt["slug"] = in.Slug
	}
	if in.RunID != "" {
		receipt["run_id"] = in.RunID
	}
	if in.ActionsURL != "" {
		receipt["actions_url"] = in.ActionsURL
	}
	if in.LogSHA256 != "" {
		receipt["log_sha256"] = in.LogSHA256
	}
	canonical, err := canonicalReceiptBytes(receipt)
	if err != nil {
		return runArtifact{}, err
	}
	receipt["signature"] = base64.StdEncoding.EncodeToString(ed25519.Sign(key, canonical))
	encoded, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return runArtifact{}, err
	}
	encoded = append(encoded, '\n')
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := createPrivateRunOutputDir(dir); err != nil {
			return runArtifact{}, exit(2, "create receipt directory: %v", err)
		}
	}
	if err := writePrivateRunOutputFile(path, encoded); err != nil {
		return runArtifact{}, exit(2, "write receipt %s: %v", path, err)
	}
	return runArtifact{Kind: "receipt", Path: path, Bytes: len(encoded)}, nil
}

func (a App) verify(ctx context.Context, args []string) error {
	fs := newFlagSet("verify", a.Stderr)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox verify <receipt.json>")
	}
	path := fs.Arg(0)
	data, err := os.ReadFile(path)
	if err != nil {
		return exit(2, "read receipt: %v", err)
	}
	receipt, err := decodeRunReceipt(data)
	if errors.Is(err, errDuplicateReceiptKey) {
		return exit(2, "malformed receipt: duplicate key")
	}
	if err != nil {
		return exit(2, "malformed receipt: %v", err)
	}
	pubText, ok := receipt["public_key"].(string)
	if !ok {
		return exit(2, "malformed receipt: missing public_key")
	}
	pub, err := base64.StdEncoding.DecodeString(pubText)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return exit(2, "malformed receipt: invalid public_key")
	}
	sigText, ok := receipt["signature"].(string)
	if !ok {
		return exit(2, "malformed receipt: missing signature")
	}
	sig, err := base64.StdEncoding.DecodeString(sigText)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return exit(2, "malformed receipt: invalid signature")
	}
	canonical, err := canonicalReceiptBytes(receipt)
	if err != nil {
		return exit(2, "canonicalize receipt: %v", err)
	}
	fingerprint := attestFingerprint(ed25519.PublicKey(pub))
	if !ed25519.Verify(ed25519.PublicKey(pub), canonical, sig) {
		fmt.Fprintf(a.Stdout, "FAIL %s signer=%s trust=self-signed: signature mismatch\n", path, fingerprint)
		return ExitError{Code: 1}
	}
	fmt.Fprintf(a.Stdout, "PASS %s signer=%s trust=self-signed\n", path, fingerprint)
	return nil
}
