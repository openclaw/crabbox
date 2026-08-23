package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
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
const (
	terminalReceiptSchemaVersion    = 2
	terminalReceiptType             = "terminal"
	maxTerminalReceiptBytes         = 16 * 1024
	maxTerminalReceiptFieldBytes    = 4 * 1024
	maxTerminalReceiptIdentityBytes = 256
	maxTerminalReceiptClockSkew     = 30 * time.Second
)

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

type terminalRunReceipt struct {
	SchemaVersion     int    `json:"schema_version"`
	ReceiptType       string `json:"receipt_type"`
	StartedAt         string `json:"started_at"`
	EndedAt           string `json:"ended_at"`
	Provider          string `json:"provider"`
	LeaseID           string `json:"lease_id,omitempty"`
	Slug              string `json:"slug,omitempty"`
	RunID             string `json:"run_id"`
	Command           string `json:"command"`
	CommandSHA256     string `json:"command_sha256"`
	ExitCode          int    `json:"exit_code"`
	SyncMs            int64  `json:"sync_ms"`
	CommandMs         int64  `json:"command_ms"`
	DurationMs        int64  `json:"duration_ms"`
	LogSHA256         string `json:"log_sha256"`
	RetainedLogSHA256 string `json:"retained_log_sha256"`
	LogTruncated      bool   `json:"log_truncated"`
	PublicKey         string `json:"public_key"`
	Signer            string `json:"signer"`
	Signature         string `json:"signature"`
}

type terminalRunReceiptInput struct {
	Provider          string
	LeaseID           string
	Slug              string
	RunID             string
	Command           []string
	CommandDisplay    string
	ExitCode          int
	SyncMs            int64
	CommandMs         int64
	StartedAt         time.Time
	EndedAt           time.Time
	LogSHA256         string
	RetainedLogSHA256 string
	LogTruncated      bool
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
	if err := ensurePrivateRunOutputDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err == nil {
		return loadManagedAttestKey(path)
	} else if !errors.Is(err, os.ErrNotExist) {
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
		return loadManagedAttestKey(path)
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

func loadManagedAttestKey(path string) (ed25519.PrivateKey, error) {
	file, err := openExistingPrivateRunOutputFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	return parseAttestKey(path, data)
}

func loadAttestKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseAttestKey(path, data)
}

func parseAttestKey(path string, data []byte) (ed25519.PrivateKey, error) {
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
	return sha256Digest(pub)
}

func sha256Digest(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func commandSHA256(command []string) string {
	return sha256Digest(lengthPrefixedBytes("crabbox-command-v1\x00", command))
}

func terminalReceiptCommandDisplay(display, digest string) string {
	if len(display) <= maxTerminalReceiptFieldBytes {
		return display
	}
	return "[command display omitted; exact argv bound by " + digest + "]"
}

func terminalReceiptSigningBytes(receipt terminalRunReceipt) []byte {
	return lengthPrefixedBytes("crabbox-terminal-receipt-v2\x00", []string{
		strconv.Itoa(receipt.SchemaVersion),
		receipt.ReceiptType,
		receipt.StartedAt,
		receipt.EndedAt,
		receipt.Provider,
		receipt.LeaseID,
		receipt.Slug,
		receipt.RunID,
		receipt.Command,
		receipt.CommandSHA256,
		strconv.Itoa(receipt.ExitCode),
		strconv.FormatInt(receipt.SyncMs, 10),
		strconv.FormatInt(receipt.CommandMs, 10),
		strconv.FormatInt(receipt.DurationMs, 10),
		receipt.LogSHA256,
		receipt.RetainedLogSHA256,
		strconv.FormatBool(receipt.LogTruncated),
		receipt.PublicKey,
		receipt.Signer,
	})
}

func lengthPrefixedBytes(prefix string, values []string) []byte {
	var payload bytes.Buffer
	payload.WriteString(prefix)
	var length [4]byte
	for _, value := range values {
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		payload.Write(length[:])
		payload.WriteString(value)
	}
	return payload.Bytes()
}

func buildTerminalRunReceipt(keyPath string, in terminalRunReceiptInput) (terminalRunReceipt, error) {
	key, err := resolveAttestKey(keyPath)
	if err != nil {
		return terminalRunReceipt{}, exit(2, "attest key: %v", err)
	}
	return buildTerminalRunReceiptWithKey(key, in)
}

func buildTerminalRunReceiptWithKey(key ed25519.PrivateKey, in terminalRunReceiptInput) (terminalRunReceipt, error) {
	pub := key.Public().(ed25519.PublicKey)
	commandDigest := commandSHA256(in.Command)
	receipt := terminalRunReceipt{
		SchemaVersion:     terminalReceiptSchemaVersion,
		ReceiptType:       terminalReceiptType,
		StartedAt:         in.StartedAt.UTC().Format(time.RFC3339Nano),
		EndedAt:           in.EndedAt.UTC().Format(time.RFC3339Nano),
		Provider:          in.Provider,
		LeaseID:           in.LeaseID,
		Slug:              in.Slug,
		RunID:             in.RunID,
		Command:           terminalReceiptCommandDisplay(in.CommandDisplay, commandDigest),
		CommandSHA256:     commandDigest,
		ExitCode:          in.ExitCode,
		SyncMs:            in.SyncMs,
		CommandMs:         in.CommandMs,
		DurationMs:        in.EndedAt.Sub(in.StartedAt).Milliseconds(),
		LogSHA256:         in.LogSHA256,
		RetainedLogSHA256: in.RetainedLogSHA256,
		LogTruncated:      in.LogTruncated,
		PublicKey:         base64.StdEncoding.EncodeToString(pub),
		Signer:            attestFingerprint(pub),
	}
	if err := validateTerminalRunReceipt(receipt); err != nil {
		return terminalRunReceipt{}, err
	}
	receipt.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, terminalReceiptSigningBytes(receipt)))
	if encoded, err := json.Marshal(receipt); err != nil {
		return terminalRunReceipt{}, err
	} else if len(encoded) > maxTerminalReceiptBytes {
		return terminalRunReceipt{}, fmt.Errorf("terminal receipt exceeds %d bytes", maxTerminalReceiptBytes)
	}
	return receipt, nil
}

func validateTerminalRunReceipt(receipt terminalRunReceipt) error {
	if receipt.SchemaVersion != terminalReceiptSchemaVersion || receipt.ReceiptType != terminalReceiptType {
		return fmt.Errorf("unsupported terminal receipt")
	}
	for name, value := range map[string]string{
		"started_at":          receipt.StartedAt,
		"ended_at":            receipt.EndedAt,
		"provider":            receipt.Provider,
		"run_id":              receipt.RunID,
		"command":             receipt.Command,
		"command_sha256":      receipt.CommandSHA256,
		"log_sha256":          receipt.LogSHA256,
		"retained_log_sha256": receipt.RetainedLogSHA256,
		"public_key":          receipt.PublicKey,
		"signer":              receipt.Signer,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("invalid %s", name)
		}
		if len(value) > maxTerminalReceiptFieldBytes {
			return fmt.Errorf("%s exceeds %d bytes", name, maxTerminalReceiptFieldBytes)
		}
	}
	for name, value := range map[string]string{
		"lease_id": receipt.LeaseID,
		"slug":     receipt.Slug,
	} {
		if len(value) > maxTerminalReceiptIdentityBytes {
			return fmt.Errorf("%s exceeds %d bytes", name, maxTerminalReceiptIdentityBytes)
		}
	}
	startedAt, err := time.Parse(time.RFC3339Nano, receipt.StartedAt)
	if err != nil {
		return fmt.Errorf("invalid started_at")
	}
	endedAt, err := time.Parse(time.RFC3339Nano, receipt.EndedAt)
	if err != nil || endedAt.Before(startedAt) {
		return fmt.Errorf("invalid ended_at")
	}
	if receipt.ExitCode < 0 || receipt.SyncMs < 0 || receipt.CommandMs < 0 || receipt.DurationMs < 0 {
		return fmt.Errorf("invalid terminal timing or exit code")
	}
	if receipt.DurationMs != endedAt.Sub(startedAt).Milliseconds() {
		return fmt.Errorf("duration_ms does not match timestamps")
	}
	for name, digest := range map[string]string{
		"command_sha256":      receipt.CommandSHA256,
		"log_sha256":          receipt.LogSHA256,
		"retained_log_sha256": receipt.RetainedLogSHA256,
		"signer":              receipt.Signer,
	} {
		if !validSHA256Digest(digest) {
			return fmt.Errorf("invalid %s", name)
		}
	}
	pub, err := base64.StdEncoding.DecodeString(receipt.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid public_key")
	}
	if receipt.Signer != attestFingerprint(ed25519.PublicKey(pub)) {
		return fmt.Errorf("signer does not match public_key")
	}
	if receipt.Signature != "" {
		signature, err := base64.StdEncoding.DecodeString(receipt.Signature)
		if err != nil || len(signature) != ed25519.SignatureSize {
			return fmt.Errorf("invalid signature")
		}
	}
	return nil
}

func verifyTerminalRunReceiptSignature(receipt terminalRunReceipt) error {
	if err := validateTerminalRunReceipt(receipt); err != nil {
		return err
	}
	if receipt.Signature == "" {
		return fmt.Errorf("missing signature")
	}
	pub, _ := base64.StdEncoding.DecodeString(receipt.PublicKey)
	signature, _ := base64.StdEncoding.DecodeString(receipt.Signature)
	if !ed25519.Verify(ed25519.PublicKey(pub), terminalReceiptSigningBytes(receipt), signature) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

func verifyTerminalRunReceipt(receipt terminalRunReceipt, binding terminalRunReceiptInput) error {
	if err := verifyTerminalRunReceiptSignature(receipt); err != nil {
		return err
	}
	startedAt, _ := time.Parse(time.RFC3339Nano, receipt.StartedAt)
	endedAt, _ := time.Parse(time.RFC3339Nano, receipt.EndedAt)
	if receipt.Provider != binding.Provider ||
		receipt.LeaseID != binding.LeaseID ||
		receipt.Slug != binding.Slug ||
		receipt.RunID != binding.RunID ||
		receipt.CommandSHA256 != commandSHA256(binding.Command) ||
		receipt.ExitCode != binding.ExitCode ||
		receipt.SyncMs != binding.SyncMs ||
		receipt.CommandMs != binding.CommandMs ||
		!startedAt.Equal(binding.StartedAt) ||
		!binding.EndedAt.IsZero() && endedAt.After(binding.EndedAt.Add(maxTerminalReceiptClockSkew)) ||
		receipt.DurationMs < binding.SyncMs+binding.CommandMs ||
		binding.LogSHA256 != "" && receipt.LogSHA256 != binding.LogSHA256 ||
		binding.RetainedLogSHA256 != "" && receipt.RetainedLogSHA256 != binding.RetainedLogSHA256 ||
		receipt.LogTruncated != binding.LogTruncated {
		return fmt.Errorf("terminal receipt binding mismatch")
	}
	return nil
}

func decodeTerminalRunReceipt(data []byte) (terminalRunReceipt, error) {
	if len(data) > maxTerminalReceiptBytes {
		return terminalRunReceipt{}, fmt.Errorf("terminal receipt exceeds %d bytes", maxTerminalReceiptBytes)
	}
	duplicate, err := jsonHasDuplicateKeys(json.NewDecoder(bytes.NewReader(data)))
	if err != nil {
		return terminalRunReceipt{}, err
	}
	if duplicate {
		return terminalRunReceipt{}, errDuplicateReceiptKey
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var receipt terminalRunReceipt
	if err := dec.Decode(&receipt); err != nil {
		return terminalRunReceipt{}, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return terminalRunReceipt{}, fmt.Errorf("multiple JSON values")
		}
		return terminalRunReceipt{}, err
	}
	if err := verifyTerminalRunReceiptSignature(receipt); err != nil {
		return terminalRunReceipt{}, err
	}
	return receipt, nil
}

func writeTerminalRunReceipt(path string, receipt terminalRunReceipt) (runArtifact, error) {
	return writeReceiptFile(path, receipt, maxTerminalReceiptBytes)
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
	return writeReceiptFile(path, receipt, 0)
}

func writeReceiptFile(path string, receipt any, maxBytes int) (runArtifact, error) {
	encoded, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return runArtifact{}, err
	}
	encoded = append(encoded, '\n')
	if maxBytes > 0 && len(encoded) > maxBytes {
		return runArtifact{}, fmt.Errorf("terminal receipt exceeds %d bytes", maxBytes)
	}
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
	var envelope struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return exit(2, "malformed receipt: %v", err)
	}
	if envelope.SchemaVersion == terminalReceiptSchemaVersion {
		receipt, err := decodeTerminalRunReceipt(data)
		if errors.Is(err, errDuplicateReceiptKey) {
			return exit(2, "malformed receipt: duplicate key")
		}
		if err != nil {
			return exit(2, "malformed receipt: %v", err)
		}
		fmt.Fprintf(a.Stdout, "PASS %s signer=%s trust=self-signed exit=%d\n", path, receipt.Signer, receipt.ExitCode)
		return nil
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
