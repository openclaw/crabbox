package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type externalRoutingState struct {
	Command    string                   `json:"command,omitempty"`
	Args       []string                 `json:"args,omitempty"`
	Config     map[string]any           `json:"config,omitempty"`
	Lifecycle  ExternalLifecycleConfig  `json:"lifecycle,omitempty"`
	Connection ExternalConnectionConfig `json:"connection,omitempty"`
	WorkRoot   string                   `json:"workRoot,omitempty"`
}

func ExternalRoutingPath(leaseID string) (string, error) {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return "", fmt.Errorf("external routing requires a lease ID")
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	sum := sha256.Sum256([]byte(leaseID))
	name := hex.EncodeToString(sum[:16]) + ".json"
	return filepath.Join(dir, "crabbox", "external", name), nil
}

func PersistExternalRouting(leaseID string, cfg ExternalConfig) (string, error) {
	path, err := ExternalRoutingPath(leaseID)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(externalRoutingState{
		Command:    cfg.Command,
		Args:       append([]string(nil), cfg.Args...),
		Config:     cfg.Config,
		Lifecycle:  cfg.Lifecycle,
		Connection: cfg.Connection,
		WorkRoot:   cfg.WorkRoot,
	}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode external routing: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create external routing directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".routing-*.json")
	if err != nil {
		return "", fmt.Errorf("create external routing file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("secure external routing file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write external routing file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close external routing file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", fmt.Errorf("install external routing file: %w", err)
	}
	return path, nil
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
		_ = os.Remove(path)
	}
}
