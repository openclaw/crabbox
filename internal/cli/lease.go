package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func newLeaseID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "cbx_" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000"), ".", "")
	}
	return "cbx_" + hex.EncodeToString(b[:])
}

func NewLeaseID() string {
	return newLeaseID()
}

func publicKeyFor(privatePath string) (string, error) {
	pub := privatePath + ".pub"
	data, err := os.ReadFile(pub)
	if err != nil {
		return "", exit(2, "read ssh public key %s: %v", pub, err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", exit(2, "ssh public key %s is empty", pub)
	}
	return key, nil
}

func testboxKeyPath(leaseID string) (string, error) {
	if leaseID != strings.TrimSpace(leaseID) || !validLeaseClaimID(leaseID) {
		return "", invalidLeaseClaimIDError{id: leaseID}
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", exit(2, "user config directory is unavailable")
	}
	return filepath.Join(dir, "crabbox", "testboxes", leaseID, "id_ed25519"), nil
}

func TestboxKeyPath(leaseID string) (string, error) {
	return testboxKeyPath(leaseID)
}

func ensureTestboxKey(leaseID string) (string, string, error) {
	return ensureTestboxKeyWithType(leaseID, "ed25519")
}

func EnsureTestboxKey(leaseID string) (string, string, error) {
	return ensureTestboxKey(leaseID)
}

func ensureTestboxKeyForConfig(cfg Config, leaseID string) (string, string, error) {
	if (cfg.Provider == "aws" || cfg.Provider == "azure") && cfg.TargetOS == targetWindows {
		return ensureTestboxKeyWithType(leaseID, "rsa")
	}
	return ensureTestboxKey(leaseID)
}

func EnsureTestboxKeyForConfig(cfg Config, leaseID string) (string, string, error) {
	return ensureTestboxKeyForConfig(cfg, leaseID)
}

func syncStoredTestboxKey(leaseID string) error {
	return syncStoredTestboxKeyWithSync(leaseID, syncControllerDirectory)
}

func syncStoredTestboxKeyWithSync(leaseID string, syncDirectory func(string) error) error {
	privatePath, err := testboxKeyPath(leaseID)
	if err != nil {
		return err
	}
	for _, path := range []string{privatePath, privatePath + ".pub"} {
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return exit(2, "user config directory is unavailable")
	}
	return syncTestboxKeyDirectoriesWithSync(filepath.Dir(privatePath), configDir, syncDirectory)
}

func syncTestboxKeyDirectoriesWithSync(keyDir, configDir string, syncDirectory func(string) error) error {
	keyDir = filepath.Clean(keyDir)
	configDir = filepath.Clean(configDir)
	relative, err := filepath.Rel(configDir, keyDir)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return exit(2, "testbox key directory %s is outside user config directory %s", keyDir, configDir)
	}
	for current := keyDir; ; current = filepath.Dir(current) {
		if err := syncDirectory(current); err != nil {
			return exit(2, "sync testbox key directory %s: %v", current, err)
		}
		if current == configDir {
			return nil
		}
		if parent := filepath.Dir(current); parent == current {
			return exit(2, "sync testbox key directory: boundary %s is not an ancestor of %s", configDir, keyDir)
		}
	}
}

func SyncStoredTestboxKey(leaseID string) error {
	return syncStoredTestboxKey(leaseID)
}

func ensureTestboxKeyWithType(leaseID, keyType string) (string, string, error) {
	privatePath, err := testboxKeyPath(leaseID)
	if err != nil {
		return "", "", err
	}
	if _, err := os.Stat(privatePath); err == nil {
		publicKey, err := publicKeyFor(privatePath)
		return privatePath, publicKey, err
	}
	if err := os.MkdirAll(filepath.Dir(privatePath), 0o700); err != nil {
		return "", "", exit(2, "create testbox key directory: %v", err)
	}
	args := []string{"-q", "-t", keyType, "-N", "", "-C", "crabbox " + leaseID, "-f", privatePath}
	if keyType == "rsa" {
		args = []string{"-q", "-t", "rsa", "-b", "4096", "-N", "", "-C", "crabbox " + leaseID, "-f", privatePath}
	}
	cmd := exec.Command("ssh-keygen", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", "", exit(2, "generate ssh key for %s: %v: %s", leaseID, err, strings.TrimSpace(string(out)))
	}
	publicKey, err := publicKeyFor(privatePath)
	return privatePath, publicKey, err
}

func useStoredTestboxKey(target *SSHTarget, leaseID string) {
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		return
	}
	if _, err := os.Stat(keyPath); err == nil {
		target.Key = keyPath
	}
}

func UseStoredTestboxKey(target *SSHTarget, leaseID string) {
	useStoredTestboxKey(target, leaseID)
}

func useLeaseKnownHosts(target *SSHTarget, leaseID string) error {
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		return err
	}
	dir := filepath.Dir(keyPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return exit(2, "prepare lease SSH host-key directory for %s: %v", leaseID, err)
	}
	// Keep the verified host identity beside Crabbox's lease credentials so
	// cleanup removes both and identical provider hostnames cannot share trust.
	target.KnownHostsFile = filepath.Join(dir, "known_hosts")
	return nil
}

func UseLeaseKnownHosts(target *SSHTarget, leaseID string) error {
	return useLeaseKnownHosts(target, leaseID)
}

func moveStoredTestboxKey(oldLeaseID, newLeaseID string) error {
	if oldLeaseID == "" || newLeaseID == "" || oldLeaseID == newLeaseID {
		return nil
	}
	oldPath, err := testboxKeyPath(oldLeaseID)
	if err != nil {
		return err
	}
	newPath, err := testboxKeyPath(newLeaseID)
	if err != nil {
		return err
	}
	oldDir := filepath.Dir(oldPath)
	newDir := filepath.Dir(newPath)
	if _, err := os.Stat(oldPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if _, err := os.Stat(newPath); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(newDir), 0o700); err != nil {
		return err
	}
	return os.Rename(oldDir, newDir)
}

func MoveStoredTestboxKey(oldLeaseID, newLeaseID string) error {
	return moveStoredTestboxKey(oldLeaseID, newLeaseID)
}

func removeStoredTestboxKeyWithError(leaseID string) error {
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		return err
	}
	keyDir := filepath.Dir(keyPath)
	if _, err := os.Stat(keyDir); errors.Is(err, os.ErrNotExist) {
		return syncNearestExistingDirectory(filepath.Dir(keyDir))
	} else if err != nil {
		return err
	}
	if err := os.RemoveAll(keyDir); err != nil {
		return err
	}
	return syncNearestExistingDirectory(filepath.Dir(keyDir))
}

func syncNearestExistingDirectory(path string) error {
	return syncNearestExistingDirectoryWithSync(path, syncControllerDirectory)
}

func syncNearestExistingDirectoryWithSync(path string, syncDirectory func(string) error) error {
	for {
		info, err := os.Stat(path)
		if err == nil {
			if !info.IsDir() {
				return exit(2, "state path %s is not a directory", path)
			}
			return syncDirectory(path)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return err
		}
		path = parent
	}
}

func removeStoredTestboxKey(leaseID string) {
	_ = removeStoredTestboxKeyWithError(leaseID)
}

func RemoveStoredTestboxKey(leaseID string) {
	removeStoredTestboxKey(leaseID)
}

func RemoveStoredTestboxKeyWithError(leaseID string) error {
	return removeStoredTestboxKeyWithError(leaseID)
}

func providerKeyForLease(leaseID string) string {
	return strings.ReplaceAll("crabbox-"+leaseID, "_", "-")
}

func ProviderKeyForLease(leaseID string) string {
	return providerKeyForLease(leaseID)
}
