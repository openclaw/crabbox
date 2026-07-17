package cli

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gofrs/flock"
)

var externalRoutingMutationMutexes sync.Map

type externalRoutingState struct {
	Command                   string                     `json:"command,omitempty"`
	Args                      []string                   `json:"args,omitempty"`
	Config                    map[string]any             `json:"config,omitempty"`
	Capabilities              ExternalCapabilitiesConfig `json:"capabilities,omitempty"`
	Lifecycle                 ExternalLifecycleConfig    `json:"lifecycle,omitempty"`
	Connection                ExternalConnectionConfig   `json:"connection,omitempty"`
	WorkRoot                  string                     `json:"workRoot,omitempty"`
	TargetOS                  string                     `json:"targetOS,omitempty"`
	WindowsMode               string                     `json:"windowsMode,omitempty"`
	Architecture              string                     `json:"architecture,omitempty"`
	CredentialBoundaryVersion int                        `json:"credentialBoundaryVersion,omitempty"`
	Generation                string                     `json:"generation,omitempty"`
}

const externalRoutingCredentialVersion = 2

func externalRoutingStateForConfig(cfg ExternalConfig, credentialVersion int) externalRoutingState {
	targetOS, windowsMode := ExternalRoutingTarget(cfg)
	return externalRoutingState{
		Command:                   cfg.Command,
		Args:                      append([]string(nil), cfg.Args...),
		Config:                    cfg.Config,
		Capabilities:              cfg.Capabilities,
		Lifecycle:                 cfg.Lifecycle,
		Connection:                cfg.Connection,
		WorkRoot:                  cfg.WorkRoot,
		TargetOS:                  targetOS,
		WindowsMode:               windowsMode,
		Architecture:              cfg.routingArchitecture,
		CredentialBoundaryVersion: credentialVersion,
		Generation:                cfg.routingGeneration,
	}
}

func SetExternalRoutingTarget(cfg *ExternalConfig, targetOS, windowsMode string) {
	if cfg == nil {
		return
	}
	cfg.routingTargetOS = normalizeTargetOS(targetOS)
	cfg.routingWindowsMode = normalizeWindowsMode(windowsMode)
}

func ExternalRoutingTarget(cfg ExternalConfig) (string, string) {
	targetOS := normalizeTargetOS(cfg.routingTargetOS)
	windowsMode := normalizeWindowsMode(cfg.routingWindowsMode)
	if targetOS != targetWindows {
		windowsMode = windowsModeNormal
	}
	return targetOS, windowsMode
}

func SetExternalRoutingArchitecture(cfg *ExternalConfig, architecture string) {
	if cfg != nil {
		architecture = strings.ToLower(strings.TrimSpace(architecture))
		if architecture != "" {
			if normalized, err := normalizeArchitecture(architecture); err == nil {
				architecture = normalized
			}
		}
		cfg.routingArchitecture = architecture
	}
}

func ExternalRoutingArchitecture(cfg ExternalConfig) string {
	architecture := strings.ToLower(strings.TrimSpace(cfg.routingArchitecture))
	if normalized, err := normalizeArchitecture(architecture); architecture != "" && err == nil {
		return normalized
	}
	return architecture
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
	return persistExternalRouting(leaseID, cfg, 0)
}

// PersistValidatedExternalRouting records that the credential destination was
// validated before the routing state became authoritative for a lease.
func PersistValidatedExternalRouting(leaseID string, cfg ExternalConfig) (string, error) {
	return persistExternalRouting(leaseID, cfg, externalRoutingCredentialVersion)
}

func persistExternalRouting(leaseID string, cfg ExternalConfig, credentialVersion int) (string, error) {
	path, err := ExternalRoutingPath(leaseID)
	if err != nil {
		return "", err
	}
	state := externalRoutingStateForConfig(cfg, credentialVersion)
	err = withExternalRoutingLock(path, func() error {
		state.Generation = reusableExternalRoutingGeneration(path, cfg, state)
		if state.Generation == "" {
			generationBytes := make([]byte, 16)
			if _, err := rand.Read(generationBytes); err != nil {
				return fmt.Errorf("generate external routing generation: %w", err)
			}
			state.Generation = hex.EncodeToString(generationBytes)
		}
		data, err := marshalExternalRoutingState(state)
		if err != nil {
			return fmt.Errorf("encode external routing: %w", err)
		}
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

func reusableExternalRoutingGeneration(path string, cfg ExternalConfig, state externalRoutingState) string {
	if cfg.routingGeneration == "" || cfg.routingDigest == "" {
		return ""
	}
	state.Generation = cfg.routingGeneration
	data, err := marshalExternalRoutingState(state)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != cfg.routingDigest {
		return ""
	}
	current, err := LoadExternalRoutingWithDigest(path, cfg.routingDigest)
	if err != nil || current.routingGeneration != cfg.routingGeneration {
		return ""
	}
	return cfg.routingGeneration
}

func marshalExternalRoutingState(state externalRoutingState) ([]byte, error) {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
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
	return loadExternalRouting(path, "")
}

// LoadExternalRoutingWithDigest loads the same descriptor that it validates
// and rejects any route generation other than expectedDigest. Generated child
// commands use this to remain bound to the route their parent approved even if
// the deterministic path is atomically replaced before the child starts.
func LoadExternalRoutingWithDigest(path, expectedDigest string) (ExternalConfig, error) {
	expectedDigest = strings.TrimSpace(expectedDigest)
	if len(expectedDigest) != sha256.Size*2 {
		return ExternalConfig{}, fmt.Errorf("external routing digest must be a lowercase SHA-256 digest")
	}
	if _, err := hex.DecodeString(expectedDigest); err != nil || strings.ToLower(expectedDigest) != expectedDigest {
		return ExternalConfig{}, fmt.Errorf("external routing digest must be a lowercase SHA-256 digest")
	}
	return loadExternalRouting(path, expectedDigest)
}

func loadExternalRouting(path, expectedDigest string) (ExternalConfig, error) {
	path = expandUserPath(strings.TrimSpace(path))
	file, resolvedPath, err := openExternalRoutingFile(path)
	if err != nil {
		return ExternalConfig{}, fmt.Errorf("read external routing file: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return ExternalConfig{}, fmt.Errorf("read external routing file: %w", err)
	}
	digestBytes := sha256.Sum256(data)
	digest := hex.EncodeToString(digestBytes[:])
	if expectedDigest != "" && digest != expectedDigest {
		return ExternalConfig{}, fmt.Errorf("external routing file generation changed: expected digest %s, found %s", expectedDigest, digest)
	}
	var state externalRoutingState
	if err := json.Unmarshal(data, &state); err != nil {
		return ExternalConfig{}, fmt.Errorf("parse external routing file: %w", err)
	}
	if state.Generation != "" {
		if len(state.Generation) != 32 || strings.ToLower(state.Generation) != state.Generation {
			return ExternalConfig{}, fmt.Errorf("parse external routing file: invalid generation")
		}
		if _, err := hex.DecodeString(state.Generation); err != nil {
			return ExternalConfig{}, fmt.Errorf("parse external routing file: invalid generation")
		}
	}
	result := ExternalConfig{
		Command:                  state.Command,
		Args:                     append([]string(nil), state.Args...),
		Config:                   state.Config,
		Capabilities:             state.Capabilities,
		Lifecycle:                state.Lifecycle,
		Connection:               state.Connection,
		WorkRoot:                 state.WorkRoot,
		RoutingFile:              resolvedPath,
		routingLoaded:            true,
		routingCredentialVersion: state.CredentialBoundaryVersion,
		routingDigest:            digest,
		routingGeneration:        state.Generation,
	}
	SetExternalRoutingTarget(&result, state.TargetOS, state.WindowsMode)
	SetExternalRoutingArchitecture(&result, state.Architecture)
	return result, nil
}

// ExternalRoutingDigest returns the exact loaded routing-file generation.
// Generated children use it instead of placing routing contents on argv.
func ExternalRoutingDigest(cfg ExternalConfig) string {
	return cfg.routingDigest
}

// ExternalRoutingGeneration returns the unique generation persisted for this
// route. Unlike a semantic configuration hash, it changes on every rewrite.
func ExternalRoutingGeneration(cfg ExternalConfig) string {
	return cfg.routingGeneration
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

// RemoveExternalRoutingIfUnchanged removes only the exact routing record used
// for the confirmed absence check, including its Desktop credential route.
func RemoveExternalRoutingIfUnchanged(leaseID string, expected ExternalConfig) error {
	return removeExternalRoutingIfUnchangedWithSync(leaseID, expected, syncControllerDirectory)
}

func externalRoutingStateForCASComparison(cfg ExternalConfig) externalRoutingState {
	state := externalRoutingStateForConfig(cfg, 0)
	state.Generation = ""
	return state
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
		actualData, err := json.Marshal(externalRoutingStateForCASComparison(actual))
		if err != nil {
			return fmt.Errorf("encode current external routing state: %w", err)
		}
		expectedData, err := json.Marshal(externalRoutingStateForCASComparison(expected))
		if err != nil {
			return fmt.Errorf("encode expected external routing state: %w", err)
		}
		if !bytes.Equal(actualData, expectedData) {
			return exit(4, "external routing state changed for lease %s; refusing local cleanup", leaseID)
		}
		if expected.routingGeneration != actual.routingGeneration {
			return exit(4, "external routing generation changed for lease %s; refusing local cleanup", leaseID)
		}
		if expected.routingDigest != "" && actual.routingDigest != expected.routingDigest {
			return exit(4, "external routing generation changed for lease %s; refusing local cleanup", leaseID)
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
