package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const (
	providerHistorySchemaVersion = 1
	providerHistoryLimit         = 8
	providerHistoryMaxBytes      = 32 << 10
)

type providerHistoryEntry struct {
	Provider       string    `json:"provider"`
	LastSelectedAt time.Time `json:"lastSelectedAt"`
}

type providerHistoryRecord struct {
	SchemaVersion int                    `json:"schemaVersion"`
	WorkspaceRoot string                 `json:"workspaceRoot"`
	Providers     []providerHistoryEntry `json:"providers"`
}

func providerHistoryWorkspaceRoot() (string, error) {
	boundary, err := findRepositoryBoundary()
	if err != nil {
		return "", err
	}
	root := canonicalRepositoryPath(strings.TrimSpace(boundary.root))
	if root == "" || !filepath.IsAbs(root) {
		return "", Exit(2, "provider history requires an absolute workspace root")
	}
	return root, nil
}

func providerHistoryPath(root string) (string, error) {
	stateDir, err := CrabboxStateDir()
	if err != nil {
		return "", err
	}
	stateDir = filepath.Clean(stateDir)
	if !filepath.IsAbs(stateDir) {
		return "", Exit(2, "provider history state directory must be absolute")
	}
	identity := canonicalRepositoryPath(root)
	if runtime.GOOS == "windows" {
		identity = strings.ToLower(identity)
	}
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(stateDir, "provider-history", hex.EncodeToString(sum[:])+".json"), nil
}

func readProviderHistory() (providerHistoryRecord, bool, error) {
	root, err := providerHistoryWorkspaceRoot()
	if err != nil {
		return providerHistoryRecord{}, false, err
	}
	return readProviderHistoryForRoot(root)
}

func readProviderHistoryForRoot(root string) (providerHistoryRecord, bool, error) {
	var record providerHistoryRecord
	path, err := providerHistoryPath(root)
	if err != nil {
		return record, false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return record, false, nil
	}
	if err != nil {
		return record, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > providerHistoryMaxBytes {
		return record, false, fmt.Errorf("invalid provider history state %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return record, false, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return record, false, err
	}
	if !os.SameFile(info, opened) {
		return record, false, fmt.Errorf("provider history state changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, providerHistoryMaxBytes+1))
	if err != nil {
		return record, false, err
	}
	if len(data) > providerHistoryMaxBytes {
		return record, false, fmt.Errorf("provider history state is oversized")
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, false, fmt.Errorf("decode provider history state: %w", err)
	}
	if record.SchemaVersion != providerHistorySchemaVersion {
		return record, false, fmt.Errorf("unsupported provider history schema version %d", record.SchemaVersion)
	}
	if !sameCanonicalRepositoryPath(canonicalRepositoryPath(record.WorkspaceRoot), canonicalRepositoryPath(root)) {
		return record, false, fmt.Errorf("provider history workspace identity mismatch")
	}
	if len(record.Providers) > providerHistoryLimit {
		record.Providers = record.Providers[:providerHistoryLimit]
	}
	return record, true, nil
}

func recentProviderFallbackAllowed() bool {
	if strings.TrimSpace(os.Getenv("CRABBOX_CONFIG")) != "" || truthyEnv(os.Getenv("CI")) {
		return false
	}
	if strings.TrimSpace(os.Getenv(controllerProviderScopeEnv)) != "" ||
		strings.TrimSpace(os.Getenv(controllerWorkspaceIDEnv)) != "" {
		return false
	}
	return true
}

func applyRecentProviderFallback(cfg *Config) {
	if cfg == nil || providerSelectionIsActionable(*cfg) || !recentProviderFallbackAllowed() {
		return
	}
	record, ok, err := readProviderHistory()
	if err != nil || !ok {
		return
	}
	for _, entry := range record.Providers {
		provider, err := ProviderFor(strings.TrimSpace(entry.Provider))
		if err != nil {
			continue
		}
		spec := provider.Spec()
		switch spec.Kind {
		case ProviderKindSSHLease, ProviderKindDelegatedRun:
		default:
			continue
		}
		if IsTargetExplicit(cfg) && !providerSpecSupportsTarget(spec, normalizeTargetOS(cfg.TargetOS), normalizeWindowsMode(cfg.WindowsMode)) {
			continue
		}
		setProviderSelection(cfg, spec.Name, providerSelectionRecentHistory)
		cfg.brokerProvider = ""
		return
	}
}

func rememberExplicitProviderBestEffort(cfg Config, stderr io.Writer) {
	if cfg.providerSelectionSource != providerSelectionFlag ||
		!cfg.providerExplicit ||
		cfg.synthesizedFlagInputs ||
		strings.TrimSpace(cfg.Provider) == "" ||
		!recentProviderFallbackAllowed() {
		return
	}
	if err := rememberProviderForCurrentWorkspace(cfg.Provider, time.Now().UTC()); err != nil && stderr != nil {
		fmt.Fprintf(stderr, "warning: remember provider history: %v\n", err)
	}
}

func rememberProviderForCurrentWorkspace(providerName string, when time.Time) error {
	root, err := providerHistoryWorkspaceRoot()
	if err != nil {
		return err
	}
	provider, err := ProviderFor(strings.TrimSpace(providerName))
	if err != nil {
		return err
	}
	canonical := provider.Spec().Name
	path, err := providerHistoryPath(root)
	if err != nil {
		return err
	}
	stateRoot, err := crabboxStateRootDir()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := ensurePrivateDirectoryDurableWithSync(dir, stateRoot, syncControllerDirectory); err != nil {
		return err
	}
	lock := flock.New(path+".lock", flock.SetPermissions(0o600))
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("lock provider history: %w", err)
	}
	defer func() {
		_ = lock.Unlock()
		_ = lock.Close()
	}()

	record, ok, err := readProviderHistoryForRoot(root)
	if err != nil {
		return err
	}
	if !ok {
		record = providerHistoryRecord{
			SchemaVersion: providerHistorySchemaVersion,
			WorkspaceRoot: root,
		}
	}
	next := make([]providerHistoryEntry, 0, providerHistoryLimit)
	next = append(next, providerHistoryEntry{Provider: canonical, LastSelectedAt: when})
	for _, entry := range record.Providers {
		if normalizeProviderName(entry.Provider) == normalizeProviderName(canonical) {
			continue
		}
		if _, err := ProviderFor(entry.Provider); err != nil {
			continue
		}
		next = append(next, entry)
		if len(next) == providerHistoryLimit {
			break
		}
	}
	record.SchemaVersion = providerHistorySchemaVersion
	record.WorkspaceRoot = root
	record.Providers = next
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := writeStateFileAtomic(path, data, syncControllerDirectory); err != nil {
		return fmt.Errorf("write provider history: %w", err)
	}
	return nil
}

func clearProviderHistoryForCurrentWorkspace() (string, bool, error) {
	root, err := providerHistoryWorkspaceRoot()
	if err != nil {
		return "", false, err
	}
	path, err := providerHistoryPath(root)
	if err != nil {
		return "", false, err
	}
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return root, false, nil
		}
		return "", false, err
	}
	lock := flock.New(path+".lock", flock.SetPermissions(0o600))
	if err := lock.Lock(); err != nil {
		return "", false, fmt.Errorf("lock provider history: %w", err)
	}
	defer func() {
		_ = lock.Unlock()
		_ = lock.Close()
	}()
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return root, false, nil
		}
		return "", false, err
	}
	if err := syncControllerDirectory(filepath.Dir(path)); err != nil {
		return "", false, err
	}
	return root, true, nil
}
