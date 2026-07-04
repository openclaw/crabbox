//go:build darwin && arm64

package applevmhelper

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// The VMM daemon is the only binary that talks to Virtualization.framework
// and therefore the only binary that needs the virtualization entitlement.
// The helper installs a codesigned copy under the state root because the
// distributed binary cannot ship with entitlements applied (ad-hoc signatures
// do not survive download quarantine or Homebrew relocation).
const (
	managedDigestSuffix  = ".digests.json"
	managedKeepVersions  = 4
	managedRecentGrace   = time.Hour
	vmdProbeTimeout      = 2 * time.Minute
	vmdEntitlementsGlue  = "\x00"
	vmdInstallLockName   = ".install.lock"
	vmdEntitlementsExt   = ".entitlements"
	vmdEntitlementPrefix = "apple-vm-"
)

var (
	ensureVMDFunc  = ensureVMD
	codesignBinary = "codesign"
)

type managedVMDDigests struct {
	SourceSHA256       string `json:"sourceSHA256"`
	ManagedSHA256      string `json:"managedSHA256"`
	EntitlementsSHA256 string `json:"entitlementsSHA256"`
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
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("%s must be an absolute path", VMDPathEnv)
		}
		if _, err := os.Stat(override); err != nil {
			return "", fmt.Errorf("resolve %s: %w", VMDPathEnv, err)
		}
		return override, nil
	}
	source, sourceDigest, err := resolveVMDSource()
	if err != nil {
		return "", err
	}
	defer source.Close()
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
	managedPath := managedVMDInstallPath(helperDir, sourceDigest, entitlementsDigest)
	digestPath := managedPath + managedDigestSuffix
	if managedVMDCurrent(managedPath, digestPath, sourceDigest, entitlementsDigest) {
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
	if _, err := io.Copy(staged, source); err != nil {
		staged.Close()
		return "", fmt.Errorf("write staged daemon: %w", err)
	}
	if err := staged.Close(); err != nil {
		return "", fmt.Errorf("close staged daemon: %w", err)
	}
	entitlementsPath := filepath.Join(helperDir, vmdEntitlementPrefix+entitlementsDigest+vmdEntitlementsExt)
	if err := os.WriteFile(entitlementsPath, []byte(HelperEntitlements), 0o600); err != nil {
		return "", fmt.Errorf("write daemon entitlements: %w", err)
	}
	defer os.Remove(entitlementsPath)
	if out, err := exec.Command(codesignBinary, "--force", "--sign", "-", "--entitlements", entitlementsPath, stagedPath).CombinedOutput(); err != nil {
		return "", fmt.Errorf("codesign managed daemon: %w: %s", err, SanitizeDiagnosticText(strings.TrimSpace(string(out))))
	}
	managedDigest, err := fileSHA256Hex(stagedPath)
	if err != nil {
		return "", fmt.Errorf("hash managed daemon: %w", err)
	}
	if err := os.Rename(stagedPath, managedPath); err != nil {
		return "", fmt.Errorf("install managed daemon: %w", err)
	}
	digestData, err := json.MarshalIndent(managedVMDDigests{
		SourceSHA256:       sourceDigest,
		ManagedSHA256:      managedDigest,
		EntitlementsSHA256: entitlementsDigest,
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
// falls back to a sibling binary for source builds.
func resolveVMDSource() (io.ReadCloser, string, error) {
	if payload := embeddedVMDPayload(); len(payload) > 0 {
		return io.NopCloser(strings.NewReader(string(payload))), sha256Hex(payload), nil
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
	return nil, "", fmt.Errorf("%s binary not found. Reinstall Crabbox on Apple Silicon, put `%s` on PATH, or set %s for a source build (see `swift build` under vmd/)", ManagedVMDName, ManagedVMDName, VMDPathEnv)
}

func openVMDSourceFile(path string) (io.ReadCloser, string, error) {
	digest, err := fileSHA256Hex(path)
	if err != nil {
		return nil, "", fmt.Errorf("hash %s: %w", path, err)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("open %s: %w", path, err)
	}
	return file, digest, nil
}

func managedVMDInstallPath(helperDir, sourceDigest, entitlementsDigest string) string {
	key := sha256Hex([]byte(sourceDigest + vmdEntitlementsGlue + entitlementsDigest))
	return filepath.Join(helperDir, ManagedVMDName+"-"+key)
}

func managedVMDCurrent(managedPath, digestPath, sourceDigest, entitlementsDigest string) bool {
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
		digests.EntitlementsSHA256 != entitlementsDigest {
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
