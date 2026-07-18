package lume

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	osuser "os/user"
	"path/filepath"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"gopkg.in/yaml.v3"
)

type lumeSettingsFile struct {
	DefaultLocationName string           `yaml:"defaultLocationName"`
	VMLocations         []lumeVMLocation `yaml:"vmLocations"`
}

type lumeVMLocation struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

type lumeVMConfigFile struct {
	MachineIdentifier string `json:"machineIdentifier"`
}

func lumeVMImmutableID(cfg Config, inst lumeVM) (string, error) {
	root, err := lumeStorageRoot(cfg, inst.LocationName)
	if err != nil {
		return "", err
	}
	if filepath.Base(inst.Name) != inst.Name || inst.Name == "." || inst.Name == ".." {
		return "", exit(5, "refusing unsafe Lume VM name %q", inst.Name)
	}
	return lumeVMImmutableIDAtPath(filepath.Join(root, inst.Name), inst.Name)
}

func lumeVMImmutableIDAtPath(vmPath, name string) (string, error) {
	configPath := filepath.Join(vmPath, "config.json")
	info, err := os.Lstat(configPath)
	if err != nil {
		return "", exit(5, "inspect Lume VM identity for %s: %v", name, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 1<<20 {
		return "", exit(5, "Lume VM identity config for %s is not a small regular file", name)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", exit(5, "read Lume VM identity for %s: %v", name, err)
	}
	var vmConfig lumeVMConfigFile
	if err := json.Unmarshal(data, &vmConfig); err != nil {
		return "", exit(5, "parse Lume VM identity for %s: %v", name, err)
	}
	machineID, err := base64.StdEncoding.DecodeString(strings.TrimSpace(vmConfig.MachineIdentifier))
	if err != nil || len(machineID) == 0 || len(machineID) > 1024 {
		return "", exit(5, "Lume VM %s has an invalid machine identifier", name)
	}
	sum := sha256.Sum256(machineID)
	return "lume-machine-" + hex.EncodeToString(sum[:]), nil
}

func lumeStorageRoot(cfg Config, reportedLocation string) (string, error) {
	storage := strings.TrimSpace(cfg.Lume.Storage)
	if storage == "" {
		storage = strings.TrimSpace(reportedLocation)
	}
	if storage == "ephemeral" {
		return filepath.Clean(os.TempDir()), nil
	}
	if strings.ContainsAny(storage, `/\\`) {
		return filepath.Clean(expandLumePath(storage)), nil
	}
	settings, err := loadLumeSettings()
	if err != nil {
		return "", err
	}
	if storage == "" {
		storage = strings.TrimSpace(settings.DefaultLocationName)
	}
	if storage == "" {
		storage = "home"
	}
	for _, location := range settings.VMLocations {
		if location.Name == storage && strings.TrimSpace(location.Path) != "" {
			return filepath.Clean(expandLumePath(location.Path)), nil
		}
	}
	if storage == "home" {
		return filepath.Clean(core.ExpandUserPath("~/.lume")), nil
	}
	return "", exit(5, "Lume storage location %q is not configured", storage)
}

func expandLumePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "~" || strings.HasPrefix(value, "~/") {
		return core.ExpandUserPath(value)
	}
	if !strings.HasPrefix(value, "~") {
		return value
	}
	slash := strings.IndexByte(value, '/')
	username := strings.TrimPrefix(value, "~")
	suffix := ""
	if slash >= 0 {
		username = value[1:slash]
		suffix = value[slash+1:]
	}
	if username == "" || strings.ContainsAny(username, `/\\`) {
		return value
	}
	account, err := osuser.Lookup(username)
	if err != nil || strings.TrimSpace(account.HomeDir) == "" {
		return value
	}
	if suffix == "" {
		return account.HomeDir
	}
	return filepath.Join(account.HomeDir, suffix)
}

func loadLumeSettings() (lumeSettingsFile, error) {
	settings := lumeSettingsFile{DefaultLocationName: "home"}
	settings.VMLocations = append(settings.VMLocations, lumeVMLocation{Name: "home", Path: "~/.lume"})
	configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configHome == "" {
		configHome = core.ExpandUserPath("~/.config")
	}
	configPath := filepath.Join(configHome, "lume", "config.yaml")
	data, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return lumeSettingsFile{}, exit(5, "read Lume storage settings: %v", err)
	}
	if len(data) > 1<<20 {
		return lumeSettingsFile{}, exit(5, "Lume storage settings are too large")
	}
	var configured lumeSettingsFile
	if err := yaml.Unmarshal(data, &configured); err != nil {
		return lumeSettingsFile{}, exit(5, "parse Lume storage settings: %v", err)
	}
	if strings.TrimSpace(configured.DefaultLocationName) == "" {
		configured.DefaultLocationName = "home"
	}
	if len(configured.VMLocations) == 0 {
		configured.VMLocations = append(configured.VMLocations, lumeVMLocation{Name: "home", Path: "~/.lume"})
	}
	return configured, nil
}
