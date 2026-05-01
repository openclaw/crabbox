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
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", exit(2, "user config directory is unavailable")
	}
	return filepath.Join(dir, "crabbox", "testboxes", leaseID, "id_ed25519"), nil
}

func ensureTestboxKey(leaseID string) (string, string, error) {
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
	cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "crabbox "+leaseID, "-f", privatePath)
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

func removeStoredTestboxKey(leaseID string) {
	keyPath, err := testboxKeyPath(leaseID)
	if err == nil {
		_ = os.RemoveAll(filepath.Dir(keyPath))
	}
}

func providerKeyForLease(leaseID string) string {
	return strings.ReplaceAll("crabbox-"+leaseID, "_", "-")
}
