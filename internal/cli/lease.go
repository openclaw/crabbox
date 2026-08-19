package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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

func newCreateAttemptID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		digits := strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
		return "cat_" + (digits + "00000000000000000000000000000000")[:32]
	}
	return "cat_" + hex.EncodeToString(b[:])
}

func NewLeaseID() string {
	return newLeaseID()
}

func newRunID() string {
	return "run_" + strings.TrimPrefix(newLeaseID(), "cbx_")
}

func PublicKeyFor(privatePath string) (string, error) {
	if strings.TrimSpace(privatePath) == "" {
		return "", exit(2, "ssh key path is not configured")
	}
	pub := privatePath + ".pub"
	data, err := os.ReadFile(pub)
	if err != nil {
		return "", exit(2, "read ssh public key %s: %v", pub, err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", exit(2, "ssh public key %s is empty", pub)
	}
	if !LooksLikeInlineSSHPublicKey(key) {
		return "", exit(2, "ssh public key %s is not a supported OpenSSH public key", pub)
	}
	return key, nil
}

func LooksLikeInlineSSHPublicKey(value string) bool {
	fields := strings.Fields(value)
	if len(fields) < 2 {
		return false
	}
	switch fields[0] {
	case "ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com":
		return true
	default:
		return false
	}
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

func ensureTestboxLeaseDirectory(leaseID string) (string, error) {
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		return "", err
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", exit(2, "user config directory is unavailable")
	}
	boundary, err := privateDirectoryDurabilityBoundary(configDir, configDir)
	if err != nil {
		return "", err
	}
	if err := ensureDirectoryPathWithoutSymlinks(configDir, boundary); err != nil {
		return "", exit(2, "create user config directory without symlinks: %v", err)
	}
	current := configDir
	for _, component := range []string{"crabbox", "testboxes", leaseID} {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil {
				return "", exit(2, "create private lease SSH directory: %v", err)
			}
			info, err = os.Lstat(current)
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", exit(2, "lease SSH directory has an unsafe path component")
		}
		if err := secureSSHTransportPath(current, true); err != nil {
			return "", exit(2, "secure lease SSH directory: %v", err)
		}
	}
	return filepath.Dir(keyPath), nil
}

func inspectTestboxLeaseDirectory(leaseID string) (string, error) {
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		return "", err
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", exit(2, "user config directory is unavailable")
	}
	boundary, err := privateDirectoryDurabilityBoundary(configDir, configDir)
	if err != nil {
		return "", err
	}
	if err := inspectDirectoryPathWithoutSymlinks(configDir, boundary); err != nil {
		return "", err
	}
	current := configDir
	for _, component := range []string{"crabbox", "testboxes", leaseID} {
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", exit(2, "lease SSH directory has an unsafe path component")
		}
		current = filepath.Join(current, component)
	}
	return filepath.Dir(keyPath), nil
}

func ensureDirectoryPathWithoutSymlinks(path, boundary string) error {
	return walkDirectoryPathWithoutSymlinks(path, boundary, true)
}

func inspectDirectoryPathWithoutSymlinks(path, boundary string) error {
	return walkDirectoryPathWithoutSymlinks(path, boundary, false)
}

func walkDirectoryPathWithoutSymlinks(path, boundary string, create bool) error {
	path = filepath.Clean(path)
	boundary = filepath.Clean(boundary)
	if !filepath.IsAbs(path) || !filepath.IsAbs(boundary) || !pathWithinRoot(path, boundary) {
		return fmt.Errorf("private directory path is outside its trusted boundary")
	}
	boundaryInfo, err := os.Stat(boundary)
	if err != nil || !boundaryInfo.IsDir() {
		return fmt.Errorf("trusted private directory boundary is unavailable")
	}
	relative, err := filepath.Rel(boundary, path)
	if err != nil {
		return err
	}
	current := boundary
	if relative == "." {
		return nil
	}
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("private directory path contains an unsafe component")
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && create {
			if err := os.Mkdir(current, 0o700); err != nil {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("private directory path contains a symlink or non-directory component")
		}
	}
	return nil
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

func ensureTestboxKeyWithType(leaseID, keyType string) (string, string, error) {
	privatePath, err := testboxKeyPath(leaseID)
	if err != nil {
		return "", "", err
	}
	if _, err := os.Stat(privatePath); err == nil {
		publicKey, err := PublicKeyFor(privatePath)
		return privatePath, publicKey, err
	}
	if _, err := ensureTestboxLeaseDirectory(leaseID); err != nil {
		return "", "", err
	}
	args := []string{"-q", "-t", keyType, "-N", "", "-C", "crabbox " + leaseID, "-f", privatePath}
	if keyType == "rsa" {
		args = []string{"-q", "-t", "rsa", "-b", "4096", "-N", "", "-C", "crabbox " + leaseID, "-f", privatePath}
	}
	cmd := exec.Command("ssh-keygen", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", "", exit(2, "generate ssh key for %s: %v: %s", leaseID, err, strings.TrimSpace(string(out)))
	}
	publicKey, err := PublicKeyFor(privatePath)
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
	dir, err := ensureTestboxLeaseDirectory(leaseID)
	if err != nil {
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

func removeStoredTestboxKey(leaseID string) {
	_ = removeStoredTestboxConnectionArtifacts(leaseID)
}

func RemoveStoredTestboxKey(leaseID string) {
	removeStoredTestboxKey(leaseID)
}

func providerKeyForLease(leaseID string) string {
	return strings.ReplaceAll("crabbox-"+leaseID, "_", "-")
}

func ProviderKeyForLease(leaseID string) string {
	return providerKeyForLease(leaseID)
}
