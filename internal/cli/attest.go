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
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const attestReceiptSchemaVersion = 1

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
	}
	if err := createPrivateRunOutputDir(filepath.Dir(path)); err != nil {
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
	if err := writePrivateRunOutputFile(path, encoded); err != nil {
		return nil, err
	}
	return key, nil
}

func loadAttestKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("attest key %s is not PEM encoded", path)
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
		"lease_id":       in.LeaseID,
		"command":        in.Command,
		"exit_code":      in.ExitCode,
		"command_ms":     in.CommandMs,
		"public_key":     base64.StdEncoding.EncodeToString(pub),
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
	if err := writePrivateRunOutputFile(path, encoded); err != nil {
		return runArtifact{}, err
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
	var receipt map[string]any
	if err := json.Unmarshal(data, &receipt); err != nil {
		return exit(2, "parse receipt: %v", err)
	}
	duplicate, err := jsonHasDuplicateKeys(json.NewDecoder(bytes.NewReader(data)))
	if err != nil {
		return exit(2, "parse receipt: %v", err)
	}
	if duplicate {
		return exit(2, "malformed receipt: duplicate key")
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
		fmt.Fprintf(a.Stdout, "FAIL %s signer=%s: signature mismatch\n", path, fingerprint)
		return ExitError{Code: 1}
	}
	fmt.Fprintf(a.Stdout, "PASS %s signer=%s\n", path, fingerprint)
	return nil
}
