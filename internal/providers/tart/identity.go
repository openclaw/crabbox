package tart

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const tartOwnershipFile = ".crabbox-owner"

func tartStoragePath() (string, error) {
	root, explicit := os.LookupEnv("TART_HOME")
	if !explicit {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".tart")
	}
	return filepath.Abs(root)
}

func tartStorageRoot() (string, error) {
	root, err := tartStoragePath()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(root)
}

func tartEnvironment() ([]string, error) {
	root, err := tartStoragePath()
	if err != nil {
		return nil, err
	}
	return append(os.Environ(), "TART_HOME="+root), nil
}

func tartVMDirectory(root, name string) (string, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return "", fmt.Errorf("unsafe Tart instance name %q", name)
	}
	dir := filepath.Join(root, "vms", name)
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	if resolved != dir {
		return "", fmt.Errorf("Tart instance %q is not in its exact storage directory", name)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("Tart instance %q is not a directory", name)
	}
	return dir, nil
}

// Only acquisition may create this witness, after a successful clone. Inventory
// and legacy claims must never manufacture authority for an existing VM.
func createTartVMIdentity(name string) (string, string, error) {
	root, err := tartStorageRoot()
	if err != nil {
		return "", "", err
	}
	dir, err := tartVMDirectory(root, name)
	if err != nil {
		return "", "", err
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", "", err
	}
	identity := hex.EncodeToString(random[:])
	file, err := os.OpenFile(filepath.Join(dir, tartOwnershipFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", "", err
	}
	_, writeErr := file.WriteString(identity + "\n")
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return "", "", writeErr
	}
	if closeErr != nil {
		return "", "", closeErr
	}
	directory, err := os.Open(dir)
	if err != nil {
		return "", "", err
	}
	syncErr := directory.Sync()
	closeErr = directory.Close()
	if syncErr != nil {
		return "", "", syncErr
	}
	return root, identity, closeErr
}

func readTartVMIdentity(root, name string) (string, error) {
	dir, err := tartVMDirectory(root, name)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, tartOwnershipFile)
	before, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() || before.Size() != 65 {
		return "", fmt.Errorf("Tart instance %q has no valid ownership marker", name)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", fmt.Errorf("Tart instance %q ownership marker changed while opening", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, 66))
	if err != nil {
		return "", err
	}
	if len(data) != 65 || data[64] != '\n' {
		return "", fmt.Errorf("Tart instance %q has an incomplete ownership marker", name)
	}
	identity := string(data[:64])
	if _, err := hex.DecodeString(identity); err != nil {
		return "", fmt.Errorf("Tart instance %q has an invalid ownership marker", name)
	}
	return identity, nil
}

func tartCleanupBinding(claim core.LeaseClaim, name, root string) (shared.ClaimBinding, error) {
	want := shared.ClaimBinding{
		Provider: providerName, LeaseID: claim.LeaseID, Slug: claim.Slug,
		CloudID: name, ProviderScope: instanceScope(name), ExactProviderScope: true,
		RequiredLabels: map[string]string{"instance": name, "tart_storage": root},
	}
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) || claim.LeaseID == "" || claim.Slug == "" || len(claim.CloudImmutableID) != 64 {
		return want, fmt.Errorf("missing exact claim or Tart ownership marker binding")
	}
	if _, err := hex.DecodeString(claim.CloudImmutableID); err != nil {
		return want, fmt.Errorf("invalid Tart ownership marker binding")
	}
	return want, shared.ValidateClaimBinding(claim, want)
}

func verifyTartVMIdentity(name, expectedRoot, expectedID string) error {
	root, err := tartStorageRoot()
	if err != nil {
		return err
	}
	if root != expectedRoot || expectedID == "" {
		return fmt.Errorf("Tart instance %q storage or ownership binding changed", name)
	}
	identity, err := readTartVMIdentity(root, name)
	if err != nil {
		return err
	}
	if identity != expectedID {
		return fmt.Errorf("Tart instance %q ownership marker changed", name)
	}
	return nil
}
