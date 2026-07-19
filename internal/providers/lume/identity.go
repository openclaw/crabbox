package lume

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	osuser "os/user"
	"path/filepath"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"gopkg.in/yaml.v3"
)

const lumeStorageIdentityFile = ".crabbox-lume-storage-id"

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

func ensureLumeStorageIdentity(root string) (string, error) {
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		return "", exit(5, "inspect Lume storage %q: %v", root, err)
	}
	if !info.IsDir() {
		return "", exit(5, "Lume storage %q is not a directory", root)
	}
	marker := filepath.Join(root, lumeStorageIdentityFile)
	if _, err := os.Lstat(marker); err == nil {
		identity, err := readLumeStorageIdentity(root)
		if err != nil {
			return "", err
		}
		if err := syncLumeStorageDirectory(root); err != nil {
			return "", err
		}
		return identity, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", exit(5, "inspect Lume storage identity %q: %v", root, err)
	}

	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", exit(5, "generate Lume storage identity: %v", err)
	}
	identity := hex.EncodeToString(random)
	file, err := os.CreateTemp(root, "."+lumeStorageIdentityFile+".tmp-")
	if err != nil {
		return "", exit(5, "create temporary Lume storage identity %q: %v", root, err)
	}
	temporary := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(temporary)
	}()
	if _, err := file.WriteString(identity + "\n"); err != nil {
		return "", exit(5, "write Lume storage identity %q: %v", root, err)
	}
	if err := file.Sync(); err != nil {
		return "", exit(5, "sync Lume storage identity %q: %v", root, err)
	}
	if err := file.Close(); err != nil {
		return "", exit(5, "close Lume storage identity %q: %v", root, err)
	}
	if err := os.Link(temporary, marker); errors.Is(err, os.ErrExist) {
		installed, readErr := readLumeStorageIdentity(root)
		if readErr != nil {
			return "", readErr
		}
		if syncErr := syncLumeStorageDirectory(root); syncErr != nil {
			return "", syncErr
		}
		return installed, nil
	} else if err != nil {
		return "", exit(5, "publish Lume storage identity %q: %v", root, err)
	}
	if err := os.Remove(temporary); err != nil {
		return "", exit(5, "remove temporary Lume storage identity %q: %v", root, err)
	}
	if err := syncLumeStorageDirectory(root); err != nil {
		return "", err
	}
	return identity, nil
}

func syncLumeStorageDirectory(root string) error {
	dir, err := os.Open(root)
	if err != nil {
		return exit(5, "open Lume storage %q for identity sync: %v", root, err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return exit(5, "sync Lume storage identity directory %q: %v", root, syncErr)
	}
	if closeErr != nil {
		return exit(5, "close Lume storage identity directory %q: %v", root, closeErr)
	}
	return nil
}

func readLumeStorageIdentity(root string) (string, error) {
	root = filepath.Clean(root)
	marker := filepath.Join(root, lumeStorageIdentityFile)
	before, err := os.Lstat(marker)
	if err != nil {
		return "", exit(5, "read Lume storage identity %q: %v", root, err)
	}
	if !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > 128 {
		return "", exit(5, "Lume storage identity %q is not a small regular file", marker)
	}
	file, err := os.Open(marker)
	if err != nil {
		return "", exit(5, "open Lume storage identity %q: %v", root, err)
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return "", exit(5, "Lume storage identity %q changed while opening", root)
	}
	data, err := io.ReadAll(io.LimitReader(file, 129))
	if err != nil {
		return "", exit(5, "read Lume storage identity %q: %v", root, err)
	}
	identity := strings.TrimSpace(string(data))
	decoded, decodeErr := hex.DecodeString(identity)
	if decodeErr != nil || len(decoded) != 32 {
		return "", exit(5, "Lume storage identity %q is invalid", root)
	}
	return identity, nil
}

func lumeStorageRoot(cfg Config, reportedLocation string) (string, error) {
	storage := strings.TrimSpace(cfg.Lume.Storage)
	if storage == "" {
		storage = strings.TrimSpace(reportedLocation)
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
