package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	jjsource "github.com/openclaw/crabbox/tools/jj-source"
)

type jjInstalledReceipt struct {
	SchemaVersion    int    `json:"schemaVersion"`
	NativeVersion    string `json:"nativeVersion"`
	SourceTreeSHA256 string `json:"sourceTreeSHA256"`
	BinarySHA256     string `json:"binarySHA256"`
	TargetOS         string `json:"targetOS"`
	TargetArch       string `json:"targetArch"`
	TargetTriple     string `json:"targetTriple"`
	Profile          string `json:"profile"`
	Rustc            string `json:"rustc"`
	Cargo            string `json:"cargo"`
}

func installedJJSourceHelper(ctx context.Context) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return installedJJSourceHelperForExecutable(ctx, executable, runtime.GOOS, runtime.GOARCH)
}

// The release/install owner supplies both files. These checks detect mismatched
// installations; a sibling receipt is not a signature or a sandbox against an
// untrusted local writer. No repository-relative or PATH fallback participates.
func installedJJSourceHelperForExecutable(ctx context.Context, executable, goos, goarch string) (string, error) {
	if !filepath.IsAbs(executable) {
		return "", fmt.Errorf("native JJ companion requires an absolute Crabbox executable path")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve Crabbox installation: %w", err)
	}
	identity, err := jjsource.ReadIdentity()
	if err != nil {
		return "", err
	}
	directory := filepath.Dir(resolved)
	receiptPath := filepath.Join(directory, "crabbox-jj-source.json")
	info, err := os.Lstat(receiptPath)
	if err != nil {
		return "", fmt.Errorf("native JJ companion receipt unavailable; install the matching helper beside Crabbox: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > 8192 {
		return "", fmt.Errorf("invalid native JJ companion receipt file")
	}
	var data bytes.Buffer
	if _, err := copyObservedSourceFileBytes(ctx, &data, receiptPath, info); err != nil {
		return "", fmt.Errorf("read native JJ companion receipt: %w", err)
	}
	decoder := json.NewDecoder(&data)
	decoder.DisallowUnknownFields()
	var receipt jjInstalledReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return "", fmt.Errorf("decode native JJ companion receipt: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", fmt.Errorf("native JJ companion receipt has trailing data")
	}
	if receipt.SchemaVersion != 1 || receipt.NativeVersion != identity.NativeVersion || receipt.SourceTreeSHA256 != identity.SourceTreeSHA256 || receipt.TargetOS != goos || receipt.TargetArch != goarch || len(receipt.BinarySHA256) != 64 || !nativeHexID(receipt.BinarySHA256) {
		return "", fmt.Errorf("native JJ companion does not match this Crabbox source package and host")
	}
	name := "crabbox-jj-source"
	if goos == "windows" {
		name += ".exe"
	}
	helper := filepath.Join(directory, name)
	info, err = os.Lstat(helper)
	if err != nil {
		return "", fmt.Errorf("native JJ companion binary unavailable: %w", err)
	}
	if !info.Mode().IsRegular() || (goos != "windows" && info.Mode().Perm()&0111 == 0) {
		return "", fmt.Errorf("native JJ companion must be a regular executable file")
	}
	hash := sha256.New()
	if _, err := copyObservedSourceFileBytes(ctx, hash, helper, info); err != nil {
		return "", fmt.Errorf("hash native JJ companion: %w", err)
	}
	if hex.EncodeToString(hash.Sum(nil)) != receipt.BinarySHA256 {
		return "", fmt.Errorf("native JJ companion binary does not match its build receipt")
	}
	return helper, nil
}
