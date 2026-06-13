package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gofrs/flock"
)

var externalRoutingMutationMutexes sync.Map

type externalRoutingState struct {
	Command      string                     `json:"command,omitempty"`
	Args         []string                   `json:"args,omitempty"`
	Config       map[string]any             `json:"config,omitempty"`
	Capabilities ExternalCapabilitiesConfig `json:"capabilities,omitempty"`
	Lifecycle    ExternalLifecycleConfig    `json:"lifecycle,omitempty"`
	Connection   ExternalConnectionConfig   `json:"connection,omitempty"`
	WorkRoot     string                     `json:"workRoot,omitempty"`
}

func externalRoutingStateForConfig(cfg ExternalConfig) externalRoutingState {
	return externalRoutingState{
		Command:      cfg.Command,
		Args:         append([]string(nil), cfg.Args...),
		Config:       cfg.Config,
		Capabilities: cfg.Capabilities,
		Lifecycle:    cfg.Lifecycle,
		Connection:   cfg.Connection,
		WorkRoot:     cfg.WorkRoot,
	}
}

func ExternalRoutingPath(leaseID string) (string, error) {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return "", fmt.Errorf("external routing requires a lease ID")
	}
	dir, err := externalRoutingConfigDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(leaseID))
	name := hex.EncodeToString(sum[:16]) + ".json"
	return filepath.Join(dir, "crabbox", "external", name), nil
}

func externalRoutingConfigDir() (string, error) {
	if dir, ok := os.LookupEnv("XDG_CONFIG_HOME"); ok && dir != "" {
		if dir != strings.TrimSpace(dir) || !filepath.IsAbs(dir) {
			return "", fmt.Errorf("XDG_CONFIG_HOME must be an absolute path without surrounding whitespace")
		}
		return filepath.Clean(dir), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	if strings.TrimSpace(dir) == "" || !filepath.IsAbs(dir) {
		return "", fmt.Errorf("resolved user config directory must be absolute")
	}
	return filepath.Clean(dir), nil
}

func PersistExternalRouting(leaseID string, cfg ExternalConfig) (string, error) {
	path, err := ExternalRoutingPath(leaseID)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(externalRoutingStateForConfig(cfg), "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode external routing: %w", err)
	}
	data = append(data, '\n')
	err = withExternalRoutingLock(path, func() error {
		return writeExternalRoutingAtomic(
			path,
			data,
			func(file *os.File) error { return file.Sync() },
			replaceControllerFile,
			syncControllerDirectory,
		)
	})
	if err != nil {
		return "", err
	}
	return path, nil
}

func writeExternalRoutingAtomic(
	path string,
	data []byte,
	syncFile func(*os.File) error,
	renameFile func(string, string) error,
	syncDirectory func(string) error,
) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".routing-*.json")
	if err != nil {
		return fmt.Errorf("create external routing file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure external routing file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write external routing file: %w", err)
	}
	if err := syncFile(tmp); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync external routing file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close external routing file: %w", err)
	}
	if err := renameFile(tmpPath, path); err != nil {
		return fmt.Errorf("install external routing file: %w", err)
	}
	if err := syncExternalRoutingDirectoryChain(filepath.Dir(path), syncDirectory); err != nil {
		return err
	}
	return nil
}

func syncExternalRoutingDirectoryChain(dir string, syncDirectory func(string) error) error {
	for current := filepath.Clean(dir); ; {
		if err := syncDirectory(current); err != nil {
			return fmt.Errorf("sync external routing directory %s: %w", current, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func LoadExternalRouting(path string) (ExternalConfig, error) {
	path = expandUserPath(strings.TrimSpace(path))
	info, err := os.Stat(path)
	if err != nil {
		return ExternalConfig{}, fmt.Errorf("read external routing file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return ExternalConfig{}, fmt.Errorf("external routing file %s must not be accessible by group or others", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ExternalConfig{}, fmt.Errorf("read external routing file: %w", err)
	}
	var state externalRoutingState
	if err := json.Unmarshal(data, &state); err != nil {
		return ExternalConfig{}, fmt.Errorf("parse external routing file: %w", err)
	}
	return ExternalConfig{
		Command:       state.Command,
		Args:          append([]string(nil), state.Args...),
		Config:        state.Config,
		Capabilities:  state.Capabilities,
		Lifecycle:     state.Lifecycle,
		Connection:    state.Connection,
		WorkRoot:      state.WorkRoot,
		RoutingFile:   path,
		routingLoaded: true,
	}, nil
}

func ExternalRoutingLoaded(cfg ExternalConfig) bool {
	return cfg.routingLoaded
}

func RemoveExternalRouting(leaseID string) {
	path, err := ExternalRoutingPath(leaseID)
	if err == nil {
		_ = withExternalRoutingLock(path, func() error {
			if err := removeControllerFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		})
	}
}

// RemoveExternalRoutingIfUnchanged removes only the routing record that
// exactly matches the provider configuration used for the confirmed absence
// check. A replaced lifecycle route is never deleted.
func RemoveExternalRoutingIfUnchanged(leaseID string, expected ExternalConfig) error {
	return removeExternalRoutingIfUnchangedWithSync(leaseID, expected, syncControllerDirectory)
}

func removeExternalRoutingIfUnchangedWithSync(leaseID string, expected ExternalConfig, syncDirectory func(string) error) error {
	path, err := ExternalRoutingPath(leaseID)
	if err != nil {
		return err
	}
	return withExternalRoutingLock(path, func() error {
		actual, err := LoadExternalRouting(path)
		if errors.Is(err, os.ErrNotExist) {
			// Windows may have completed the write-through rename into the
			// deterministic deletion tombstone before a prior remove failed.
			// Always let the platform file operation finish that recovery.
			if removeErr := removeControllerFile(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return exit(2, "remove external routing tombstone for lease %s: %v", leaseID, removeErr)
			}
			if syncErr := syncDirectory(filepath.Dir(path)); syncErr != nil && !errors.Is(syncErr, os.ErrNotExist) {
				return exit(2, "sync confirmed-absent external routing directory %s: %v", filepath.Dir(path), syncErr)
			}
			return nil
		}
		if err != nil {
			return err
		}
		actualData, err := json.Marshal(externalRoutingStateForConfig(actual))
		if err != nil {
			return fmt.Errorf("encode current external routing state: %w", err)
		}
		expectedData, err := json.Marshal(externalRoutingStateForConfig(expected))
		if err != nil {
			return fmt.Errorf("encode expected external routing state: %w", err)
		}
		if !bytes.Equal(actualData, expectedData) {
			return exit(4, "external routing state changed for lease %s; refusing local cleanup", leaseID)
		}
		if err := removeControllerFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return exit(2, "remove external routing state for lease %s: %v", leaseID, err)
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return exit(2, "sync removed external routing directory %s: %v", filepath.Dir(path), err)
		}
		return nil
	})
}

func withExternalRoutingLock(path string, action func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create external routing directory: %w", err)
	}
	lockPath := path + ".lock"
	value, _ := externalRoutingMutationMutexes.LoadOrStore(lockPath, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	lock := flock.New(lockPath, flock.SetPermissions(0o600))
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("lock external routing state: %w", err)
	}
	defer lock.Unlock()
	return action()
}
