package applemachine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const machineOwnershipFile = ".crabbox-owner"

// Query the daemon: the calling process's CONTAINER_APP_ROOT can point elsewhere.
func (b *backend) storageRoot(ctx context.Context) (string, error) {
	result, err := b.control(ctx, []string{"system", "status", "--format", "json"})
	if err != nil {
		return "", fmt.Errorf("read Apple container daemon storage: %w", err)
	}
	var health struct{ Status, AppRoot string }
	if err := json.Unmarshal([]byte(result.Stdout), &health); err != nil || health.Status != "running" || !filepath.IsAbs(health.AppRoot) {
		return "", fmt.Errorf("Apple container daemon did not report a running service with an absolute appRoot")
	}
	root, err := filepath.EvalSymlinks(health.AppRoot)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("Apple container appRoot is not a directory")
	}
	return root, nil
}

func validMachineName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsAny(name, `/\\`)
}

func machineDirectory(root, name string) (string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !validMachineName(name) {
		return "", fmt.Errorf("invalid Apple Machine storage binding")
	}
	dir := filepath.Join(root, "plugin-state", "machine-apiserver", "machines", name)
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	if resolved != dir {
		return "", fmt.Errorf("Apple Machine bundle is not in its exact storage directory")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("Apple Machine bundle is not a directory")
	}
	return dir, nil
}

// Only successful acquisition may mint a witness; inspection and legacy reuse may not.
func createMachineIdentity(root, name string) (string, error) {
	dir, err := machineDirectory(root, name)
	if err != nil {
		return "", err
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	identity := hex.EncodeToString(random[:])
	file, err := os.OpenFile(filepath.Join(dir, machineOwnershipFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, writeErr := file.WriteString(identity + "\n")
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return "", writeErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	directory, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	syncErr := directory.Sync()
	closeErr = directory.Close()
	if syncErr != nil {
		return "", syncErr
	}
	return identity, closeErr
}

func readMachineIdentity(root, name string) (string, error) {
	dir, err := machineDirectory(root, name)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, machineOwnershipFile)
	before, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() || before.Size() != 65 {
		return "", fmt.Errorf("Apple Machine bundle has no valid ownership marker")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", fmt.Errorf("Apple Machine ownership marker changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, 66))
	if err != nil {
		return "", err
	}
	if len(data) != 65 || data[64] != '\n' || !validMachineIdentity(string(data[:64])) {
		return "", fmt.Errorf("Apple Machine ownership marker is malformed")
	}
	return string(data[:64]), nil
}

func validMachineIdentity(identity string) bool {
	_, err := hex.DecodeString(identity)
	return len(identity) == 64 && err == nil
}

func machineClaimBinding(claim core.LeaseClaim) (shared.ClaimBinding, error) {
	root := claim.Labels["apple_machine_storage"]
	want := shared.ClaimBinding{
		Provider: providerName, ProviderScope: root, ExactProviderScope: true,
		LeaseID: claim.LeaseID, Slug: claim.Slug, CloudID: machineName(claim.LeaseID),
		RequiredLabels: map[string]string{"apple_machine_storage": root},
	}
	if !core.IsCanonicalLeaseID(claim.LeaseID) || claim.Slug == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || !validMachineIdentity(claim.CloudImmutableID) {
		return want, exit(2, "apple-machine lease %q has no exact storage/ownership binding; retain the machine and inspect it with container machine inspect before manual cleanup or creating a new lease", claim.LeaseID)
	}
	return want, shared.ValidateClaimBinding(claim, want)
}

func (b *backend) verifyMachineIdentity(ctx context.Context, claim core.LeaseClaim) (machine, error) {
	if _, err := machineClaimBinding(claim); err != nil {
		return machine{}, err
	}
	root, err := b.storageRoot(ctx)
	if err != nil {
		return machine{}, err
	}
	if root != claim.ProviderScope {
		return machine{}, fmt.Errorf("Apple Machine daemon storage changed; retaining lease=%s", claim.LeaseID)
	}
	item, err := b.inspectMachine(ctx, claim.CloudID)
	if err != nil {
		return machine{}, err
	}
	identity, err := readMachineIdentity(root, claim.CloudID)
	if err != nil {
		return machine{}, err
	}
	if identity != claim.CloudImmutableID {
		return machine{}, fmt.Errorf("Apple Machine ownership marker changed; retaining lease=%s", claim.LeaseID)
	}
	return item, nil
}

func (b *backend) deleteBoundMachine(ctx context.Context, claim core.LeaseClaim) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// A prior delete may have succeeded before confirmation failed. Absence can
	// retire the unchanged claim, but must never authorize another name-only rm.
	present, err := b.boundMachinePresent(ctx, claim)
	if err != nil || !present {
		return err
	}
	if _, err := b.verifyMachineIdentity(ctx, claim); err != nil {
		return err
	}
	if err := b.removeMachine(ctx, claim.CloudID); err != nil {
		return err
	}
	present, err = b.boundMachinePresent(ctx, claim)
	if err != nil {
		return err
	}
	if present {
		return fmt.Errorf("Apple Machine %q is still present after deletion", claim.CloudID)
	}
	return nil
}

func (b *backend) boundMachinePresent(ctx context.Context, claim core.LeaseClaim) (bool, error) {
	if _, err := machineClaimBinding(claim); err != nil {
		return false, err
	}
	root, err := b.storageRoot(ctx)
	if err != nil {
		return false, err
	}
	if root != claim.ProviderScope {
		return false, fmt.Errorf("Apple Machine daemon storage changed during deletion")
	}
	items, err := b.listMachines(ctx)
	if err != nil {
		return false, err
	}
	for _, item := range items {
		if item.ID == claim.CloudID {
			return true, nil
		}
	}
	// Inventory alone cannot prove persistent storage was removed.
	path := filepath.Join(root, "plugin-state", "machine-apiserver", "machines", claim.CloudID)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return false, fmt.Errorf("Apple Machine %q storage absence is unconfirmed: %v", claim.CloudID, err)
	}
	return false, nil
}

func (b *backend) removeBoundLease(ctx context.Context, claim core.LeaseClaim) error {
	want, err := machineClaimBinding(claim)
	if err != nil {
		return err
	}
	return shared.RemoveExactClaimAfter(claim, want, func() error { return b.deleteBoundMachine(ctx, claim) })
}
