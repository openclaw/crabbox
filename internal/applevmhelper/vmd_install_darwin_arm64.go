//go:build darwin && arm64

package applevmhelper

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// The VMM daemon is the only binary that talks to Virtualization.framework
// and therefore the only binary that needs the virtualization entitlement.
// Official builds embed an already signed and notarized daemon. Its Mach-O
// bytes must remain unchanged when installed. Source builds retain the local
// ad-hoc signing path because their sibling/PATH daemon is not a release asset.
const (
	managedDigestSuffix      = ".digests.json"
	managedKeepVersions      = 4
	managedRecentGrace       = time.Hour
	vmdProbeTimeout          = 2 * time.Minute
	vmdInstallKeyGlue        = "\x00"
	vmdInstallLockName       = ".install.lock"
	vmdEntitlementsExt       = ".entitlements"
	vmdEntitlementPrefix     = "apple-vm-"
	vmdReleaseTeamID         = "FWJYW4S8P8"
	vmdReleaseAuthority      = "Developer ID Application: OpenClaw Foundation (FWJYW4S8P8)"
	vmdSourceEmbeddedRelease = vmdSourceMode("embedded-release")
	vmdSourceDevelopment     = vmdSourceMode("source-development")
)

var (
	ensureVMDFunc          = ensureVMD
	codesignBinary         = "/usr/bin/codesign"
	plutilBinary           = "/usr/bin/plutil"
	embeddedVMDPayloadFunc = embeddedVMDPayload
	embeddedVMDReleaseFunc = embeddedVMDIsReleasePayload
	runVMDCommand          = executeVMDCommand
)

type vmdSourceMode string

type resolvedVMDSource struct {
	Reader io.ReadCloser
	SHA256 string
	Mode   vmdSourceMode
}

type managedVMDDigests struct {
	SourceSHA256       string `json:"sourceSHA256"`
	ManagedSHA256      string `json:"managedSHA256"`
	EntitlementsSHA256 string `json:"entitlementsSHA256"`
	SourceMode         string `json:"sourceMode"`
}

// HelperDir hosts the managed, entitlement-signed daemon copies beneath the
// state root.
func HelperDir(stateRoot string) string {
	return filepath.Join(stateRoot, "helper")
}

// ensureVMD returns the path to a runnable, entitlement-signed VMM daemon,
// installing a managed copy under the state root when necessary.
func ensureVMD(stateRoot string) (_ string, returnErr error) {
	if override := strings.TrimSpace(os.Getenv(VMDPathEnv)); override != "" {
		if embeddedVMDReleaseFunc() {
			return "", fmt.Errorf("%s is not permitted by an official release helper", VMDPathEnv)
		}
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("%s must be an absolute path", VMDPathEnv)
		}
		if _, err := os.Stat(override); err != nil {
			return "", fmt.Errorf("resolve %s: %w", VMDPathEnv, err)
		}
		return override, nil
	}
	source, err := resolveVMDSource()
	if err != nil {
		return "", err
	}
	defer source.Reader.Close()
	helperDir := HelperDir(stateRoot)
	if err := ensurePrivateDir(helperDir); err != nil {
		return "", fmt.Errorf("create managed daemon directory: %w", err)
	}
	installLock := flock.New(filepath.Join(helperDir, vmdInstallLockName), flock.SetPermissions(0o600))
	if err := installLock.Lock(); err != nil {
		return "", fmt.Errorf("lock managed daemon directory: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, installLock.Unlock())
	}()
	entitlementsDigest := sha256Hex([]byte(HelperEntitlements))
	managedPath := managedVMDInstallPath(helperDir, source.SHA256, entitlementsDigest, source.Mode)
	digestPath := managedPath + managedDigestSuffix
	if managedVMDCurrent(managedPath, digestPath, source.SHA256, entitlementsDigest, source.Mode) {
		if source.Mode == vmdSourceEmbeddedRelease {
			if err := verifyEmbeddedReleaseVMD(managedPath); err != nil {
				return "", err
			}
		}
		if err := touchAndCleanupManagedVMDs(helperDir, managedPath); err != nil {
			return "", fmt.Errorf("clean managed daemon directory: %w", err)
		}
		return managedPath, nil
	}
	staged, err := os.CreateTemp(helperDir, "."+ManagedVMDName+"-*")
	if err != nil {
		return "", fmt.Errorf("stage managed daemon: %w", err)
	}
	stagedPath := staged.Name()
	defer os.Remove(stagedPath)
	if err := staged.Chmod(0o700); err != nil {
		staged.Close()
		return "", fmt.Errorf("secure staged daemon: %w", err)
	}
	if _, err := io.Copy(staged, source.Reader); err != nil {
		staged.Close()
		return "", fmt.Errorf("write staged daemon: %w", err)
	}
	if err := staged.Close(); err != nil {
		return "", fmt.Errorf("close staged daemon: %w", err)
	}
	if source.Mode == vmdSourceEmbeddedRelease {
		if err := verifyEmbeddedReleaseVMD(stagedPath); err != nil {
			return "", err
		}
	} else if err := signDevelopmentVMD(stagedPath, helperDir, entitlementsDigest); err != nil {
		return "", err
	}
	managedDigest, err := fileSHA256Hex(stagedPath)
	if err != nil {
		return "", fmt.Errorf("hash managed daemon: %w", err)
	}
	if source.Mode == vmdSourceEmbeddedRelease && managedDigest != source.SHA256 {
		return "", fmt.Errorf("embedded release daemon changed while installing")
	}
	if err := os.Rename(stagedPath, managedPath); err != nil {
		return "", fmt.Errorf("install managed daemon: %w", err)
	}
	digestData, err := json.MarshalIndent(managedVMDDigests{
		SourceSHA256:       source.SHA256,
		ManagedSHA256:      managedDigest,
		EntitlementsSHA256: entitlementsDigest,
		SourceMode:         string(source.Mode),
	}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode managed daemon digests: %w", err)
	}
	if err := os.WriteFile(digestPath, append(digestData, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write managed daemon digests: %w", err)
	}
	if err := touchAndCleanupManagedVMDs(helperDir, managedPath); err != nil {
		return "", fmt.Errorf("clean managed daemon directory: %w", err)
	}
	return managedPath, nil
}

// resolveVMDSource prefers the payload embedded at release build time and
// falls back to a sibling binary for source builds. An embedded payload never
// falls back to a development source when release verification fails.
func resolveVMDSource() (*resolvedVMDSource, error) {
	releasePayload := embeddedVMDReleaseFunc()
	payload := embeddedVMDPayloadFunc()
	if releasePayload && len(payload) == 0 {
		return nil, fmt.Errorf("official release helper has no embedded %s payload", ManagedVMDName)
	}
	if len(payload) > 0 {
		mode := vmdSourceDevelopment
		if releasePayload {
			mode = vmdSourceEmbeddedRelease
		}
		return &resolvedVMDSource{
			Reader: io.NopCloser(bytes.NewReader(payload)),
			SHA256: sha256Hex(payload),
			Mode:   mode,
		}, nil
	}
	if exe, err := helperExecutable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), ManagedVMDName)
		if _, err := os.Stat(sibling); err == nil {
			return openVMDSourceFile(sibling)
		}
	}
	if path, err := exec.LookPath(ManagedVMDName); err == nil {
		return openVMDSourceFile(path)
	}
	return nil, fmt.Errorf("%s binary not found. Reinstall Crabbox on Apple Silicon, put `%s` on PATH, or set %s for a source build (see `swift build` under vmd/)", ManagedVMDName, ManagedVMDName, VMDPathEnv)
}

func openVMDSourceFile(path string) (*resolvedVMDSource, error) {
	digest, err := fileSHA256Hex(path)
	if err != nil {
		return nil, fmt.Errorf("hash %s: %w", path, err)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &resolvedVMDSource{
		Reader: file,
		SHA256: digest,
		Mode:   vmdSourceDevelopment,
	}, nil
}

func signDevelopmentVMD(stagedPath, helperDir, entitlementsDigest string) error {
	entitlementsPath := filepath.Join(helperDir, vmdEntitlementPrefix+entitlementsDigest+vmdEntitlementsExt)
	if err := os.WriteFile(entitlementsPath, []byte(HelperEntitlements), 0o600); err != nil {
		return fmt.Errorf("write daemon entitlements: %w", err)
	}
	defer os.Remove(entitlementsPath)
	stdout, stderr, err := runVMDCommand(nil, codesignBinary,
		"--force", "--sign", "-", "--entitlements", entitlementsPath, stagedPath)
	if err != nil {
		return vmdCommandError("codesign development daemon", err, stdout, stderr)
	}
	return nil
}

// verifyEmbeddedReleaseVMD enforces the release trust boundary before an
// embedded daemon is installed or reused. --check-notarization deliberately
// performs the raw-CLI online ticket check; there is no stapled ticket fallback.
func verifyEmbeddedReleaseVMD(path string) error {
	requirement := fmt.Sprintf(
		`identifier %q and anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = %q`,
		ManagedVMDIdentifier,
		vmdReleaseTeamID,
	)
	stdout, stderr, err := runVMDCommand(nil, codesignBinary,
		"--verify", "--strict", "-R="+requirement, "--verbose=2", path)
	if err != nil {
		return vmdCommandError("verify embedded release daemon signature", err, stdout, stderr)
	}

	stdout, stderr, err = runVMDCommand(nil, codesignBinary, "-dvvv", path)
	if err != nil {
		return vmdCommandError("inspect embedded release daemon signature", err, stdout, stderr)
	}
	metadata := string(stdout) + "\n" + string(stderr)
	if !hasSingleVMDMetadataValue(metadata, "Identifier", ManagedVMDIdentifier) {
		return fmt.Errorf("embedded release daemon has unexpected signing identifier")
	}
	if !hasVMDMetadataLine(metadata, "Authority="+vmdReleaseAuthority) {
		return fmt.Errorf("embedded release daemon is not signed by %s", vmdReleaseAuthority)
	}
	if !hasSingleVMDMetadataValue(metadata, "TeamIdentifier", vmdReleaseTeamID) {
		return fmt.Errorf("embedded release daemon has unexpected signing team")
	}
	if !hasVMDHardenedRuntime(metadata) {
		return fmt.Errorf("embedded release daemon signature lacks hardened runtime")
	}
	timestamp, ok := singleVMDMetadataValue(metadata, "Timestamp")
	if !ok || timestamp == "" || strings.EqualFold(timestamp, "none") {
		return fmt.Errorf("embedded release daemon signature lacks a secure timestamp")
	}

	actualEntitlements, entitlementStderr, err := runVMDCommand(nil, codesignBinary,
		"-d", "--entitlements", "-", "--xml", path)
	if err != nil {
		return vmdCommandError("read embedded release daemon entitlements", err, actualEntitlements, entitlementStderr)
	}
	actual, err := canonicalVMDEntitlements(actualEntitlements)
	if err != nil {
		return fmt.Errorf("parse embedded release daemon entitlements: %w", err)
	}
	expected, err := canonicalVMDEntitlements([]byte(HelperEntitlements))
	if err != nil {
		return fmt.Errorf("parse expected daemon entitlements: %w", err)
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("embedded release daemon entitlements do not exactly match the release policy")
	}

	stdout, stderr, err = runVMDCommand(nil, codesignBinary,
		"--verify", "--strict", "--check-notarization", "-R=notarized", "--verbose=2", path)
	if err != nil {
		return vmdCommandError("verify embedded release daemon notarization online", err, stdout, stderr)
	}
	return nil
}

func executeVMDCommand(stdin []byte, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.Command(name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func vmdCommandError(action string, err error, stdout, stderr []byte) error {
	detail := strings.TrimSpace(string(stdout) + "\n" + string(stderr))
	if detail == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, SanitizeDiagnosticText(detail))
}

func canonicalVMDEntitlements(plist []byte) (map[string]bool, error) {
	stdout, stderr, err := runVMDCommand(plist, plutilBinary, "-convert", "json", "-o", "-", "-")
	if err != nil {
		return nil, vmdCommandError("convert entitlement plist", err, stdout, stderr)
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	var value map[string]bool
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("entitlement plist is not a dictionary")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("entitlement plist contains trailing data")
		}
		return nil, err
	}
	return value, nil
}

func hasSingleVMDMetadataValue(metadata, key, want string) bool {
	value, ok := singleVMDMetadataValue(metadata, key)
	return ok && value == want
}

func singleVMDMetadataValue(metadata, key string) (string, bool) {
	prefix := key + "="
	var value string
	count := 0
	for _, line := range strings.Split(metadata, "\n") {
		if strings.HasPrefix(line, prefix) {
			value = strings.TrimPrefix(line, prefix)
			count++
		}
	}
	return value, count == 1
}

func hasVMDMetadataLine(metadata, want string) bool {
	for _, line := range strings.Split(metadata, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

func hasVMDHardenedRuntime(metadata string) bool {
	for _, line := range strings.Split(metadata, "\n") {
		if strings.HasPrefix(line, "CodeDirectory ") &&
			strings.Contains(line, "flags=") &&
			strings.Contains(line, "(runtime)") {
			return true
		}
	}
	return false
}

func managedVMDInstallPath(helperDir, sourceDigest, entitlementsDigest string, mode vmdSourceMode) string {
	key := sha256Hex([]byte(sourceDigest + vmdInstallKeyGlue + entitlementsDigest + vmdInstallKeyGlue + string(mode)))
	return filepath.Join(helperDir, ManagedVMDName+"-"+key)
}

func managedVMDCurrent(managedPath, digestPath, sourceDigest, entitlementsDigest string, mode vmdSourceMode) bool {
	if _, err := os.Stat(managedPath); err != nil {
		return false
	}
	data, err := os.ReadFile(digestPath)
	if err != nil {
		return false
	}
	var digests managedVMDDigests
	if err := json.Unmarshal(data, &digests); err != nil ||
		digests.SourceSHA256 != sourceDigest ||
		digests.EntitlementsSHA256 != entitlementsDigest ||
		digests.SourceMode != string(mode) ||
		(mode == vmdSourceEmbeddedRelease && digests.ManagedSHA256 != sourceDigest) {
		return false
	}
	managedDigest, err := fileSHA256Hex(managedPath)
	return err == nil && managedDigest == digests.ManagedSHA256
}

func touchAndCleanupManagedVMDs(helperDir, currentPath string) error {
	now := time.Now()
	if err := os.Chtimes(currentPath, now, now); err != nil {
		return err
	}
	entries, err := os.ReadDir(helperDir)
	if err != nil {
		return err
	}
	type candidate struct {
		path    string
		modTime time.Time
	}
	var candidates []candidate
	prefix := ManagedVMDName + "-"
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, prefix) || strings.HasSuffix(name, managedDigestSuffix) {
			continue
		}
		if !isSHA256Hex(strings.TrimPrefix(name, prefix)) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate{path: filepath.Join(helperDir, name), modTime: info.ModTime()})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	keep := map[string]bool{currentPath: true}
	for _, candidate := range candidates {
		if len(keep) >= managedKeepVersions {
			break
		}
		keep[candidate.path] = true
	}
	cutoff := now.Add(-managedRecentGrace)
	for _, candidate := range candidates {
		if keep[candidate.path] || !candidate.modTime.Before(cutoff) {
			continue
		}
		if err := os.Remove(candidate.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Remove(candidate.path + managedDigestSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func isSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fileSHA256Hex(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
