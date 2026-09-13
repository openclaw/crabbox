package cli

import (
	"errors"
	"os"
	"path/filepath"
)

// The explicit state root is operator input; the legacy default is deliberately
// not crabboxStateDir, which has an additional state component.
func leaseSSHRoot() (string, error) {
	if root := os.Getenv("XDG_STATE_HOME"); root != "" {
		if !filepath.IsAbs(root) {
			return "", Exit(2, "XDG_STATE_HOME must be absolute for generated lease SSH material")
		}
		return filepath.Clean(root), nil
	}
	return os.UserConfigDir()
}

func secureLeaseSSHDirectory(path string) error {
	if os.Getenv("XDG_STATE_HOME") != "" {
		return secureSelectedLeaseSSHDirectory(path)
	}
	return secureSSHTransportPath(path, true)
}

func secureSelectedLeaseSSHDirectory(path string) error {
	if err := verifySelectedLeaseSSHOwnership(path, true); err != nil {
		return err
	}
	return secureSSHTransportPath(path, true)
}

func verifySelectedLeaseSSHPath(path string, directory bool) error {
	if err := verifySelectedLeaseSSHOwnership(path, directory); err != nil {
		return err
	}
	return verifySSHTransportPathPrivate(path, directory)
}

// Existing generated keys need the same selected-root admission as new keys.
// Unset-root and user-supplied key policies remain with their existing owners.
func inspectSelectedLeaseSSHKey(leaseID string) error {
	if os.Getenv("XDG_STATE_HOME") == "" {
		return nil
	}
	if _, err := inspectTestboxLeaseDirectory(leaseID); err != nil {
		return err
	}
	key, err := testboxKeyPath(leaseID)
	if err != nil {
		return err
	}
	return inspectSelectedLeaseSSHFiles(key)
}

func inspectSelectedLeaseSSHFiles(key string) error {
	keyErr := verifySelectedLeaseSSHPath(key, false)
	if keyErr != nil && !errors.Is(keyErr, os.ErrNotExist) {
		return keyErr
	}
	// Check the public sibling even when the private file is absent.
	if err := verifySelectedLeaseSSHOwnership(key+".pub", false); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return keyErr
}

// PreflightLeaseSSHStorage admits managed connection storage before a provider
// can create a resource whose per-lease identity is not known yet.
func PreflightLeaseSSHStorage() error {
	if os.Getenv("XDG_STATE_HOME") == "" {
		return nil
	}
	return ensureLeaseSSHDirectories([]string{"crabbox", "testboxes"})
}

// PrepareStoredTestboxKeyPath leaves imported key contents and publication
// policy to the provider, but admits the canonical local destination first.
func PrepareStoredTestboxKeyPath(leaseID string) (string, error) {
	key, err := testboxKeyPath(leaseID)
	if err != nil || os.Getenv("XDG_STATE_HOME") == "" {
		return key, err
	}
	if _, err := ensureTestboxLeaseDirectory(leaseID); err != nil {
		return "", err
	}
	if err := inspectSelectedLeaseSSHFiles(key); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return key, nil
}

// SecureCreatedLeaseSSHFile is only for a newly created, caller-owned handle.
// Reuse must pass nonmutating admission instead of repairing existing keys.
func SecureCreatedLeaseSSHFile(file *os.File) error {
	if os.Getenv("XDG_STATE_HOME") == "" {
		return nil
	}
	return securePrivateFile(file)
}

func secureCreatedLeaseSSHKeyPair(key string) error {
	if os.Getenv("XDG_STATE_HOME") == "" {
		return nil
	}
	for _, path := range []string{key, key + ".pub"} {
		file, err := openCreatedLeaseSSHFile(path)
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err == nil && !info.Mode().IsRegular() {
			err = Exit(2, "created lease SSH material must be a regular file")
		}
		if err == nil {
			err = SecureCreatedLeaseSSHFile(file)
		}
		if err == nil && path != key {
			err = file.Chmod(info.Mode().Perm())
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// WritePreparedLeaseSSHKeyFile preserves the provider's direct write policy,
// but admits the opened handle before truncating or writing private bytes.
func WritePreparedLeaseSSHKeyFile(path string, data []byte) error {
	if os.Getenv("XDG_STATE_HOME") == "" {
		return os.WriteFile(path, data, 0o600)
	}
	file, err := openSelectedLeaseSSHWriteFile(path)
	if err != nil {
		return err
	}
	if err = securePrivateFile(file); err == nil {
		err = file.Truncate(0)
	}
	if err == nil {
		_, err = file.Write(data)
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}
