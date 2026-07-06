package sealosdevbox

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	devboxPublicKeyField  = "SEALOS_DEVBOX_PUBLIC_KEY"
	devboxPrivateKeyField = "SEALOS_DEVBOX_PRIVATE_KEY"
)

type devboxSecret struct {
	Metadata   devboxMeta        `json:"metadata"`
	Data       map[string]string `json:"data"`
	StringData map[string]string `json:"stringData"`
}

type devboxSecretKeys struct {
	PublicKey  string
	PrivateKey string
}

func parseDevboxSecretKeys(secret devboxSecret) (devboxSecretKeys, error) {
	publicKey, err := secretField(secret, devboxPublicKeyField)
	if err != nil {
		return devboxSecretKeys{}, err
	}
	privateKey, err := secretField(secret, devboxPrivateKeyField)
	if err != nil {
		return devboxSecretKeys{}, err
	}
	if strings.TrimSpace(publicKey) == "" {
		return devboxSecretKeys{}, core.Exit(5, "sealos-devbox Secret is missing public key data")
	}
	if strings.TrimSpace(privateKey) == "" {
		return devboxSecretKeys{}, core.Exit(5, "sealos-devbox Secret is missing private key data")
	}
	return devboxSecretKeys{PublicKey: strings.TrimSpace(publicKey), PrivateKey: ensureTrailingNewline(privateKey)}, nil
}

func validateDevboxSecretOwner(secret devboxSecret, item devboxItem) error {
	expectedName := strings.TrimSpace(item.Metadata.Name)
	expectedUID := strings.TrimSpace(item.Metadata.UID)
	if expectedName == "" || expectedUID == "" {
		return core.Exit(4, "cannot verify Sealos DevBox Secret without exact DevBox name and UID")
	}
	if strings.TrimSpace(secret.Metadata.Name) != expectedName || strings.TrimSpace(secret.Metadata.Namespace) != strings.TrimSpace(item.Metadata.Namespace) {
		return core.Exit(4, "refusing Sealos DevBox Secret that does not match DevBox %s/%s", item.Metadata.Namespace, expectedName)
	}
	for _, owner := range secret.Metadata.OwnerReferences {
		if owner.Controller && owner.APIVersion == devboxGroupVersion && owner.Kind == devboxKind && owner.Name == expectedName && owner.UID == expectedUID {
			return nil
		}
	}
	return core.Exit(4, "refusing Sealos DevBox Secret without the exact DevBox controller owner")
}

func secretField(secret devboxSecret, key string) (string, error) {
	if value := strings.TrimSpace(secret.StringData[key]); value != "" {
		return value, nil
	}
	encoded := strings.TrimSpace(secret.Data[key])
	if encoded == "" {
		return "", core.Exit(5, "sealos-devbox Secret is missing %s", key)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", core.Exit(5, "sealos-devbox Secret contains invalid %s", key)
	}
	return string(decoded), nil
}

func persistDevboxKey(leaseID string, keys devboxSecretKeys) (string, error) {
	path, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", core.Exit(2, "create sealos-devbox key directory: %v", err)
	}
	publicPath := path + ".pub"
	previousPublic, previousPublicMode, hadPublic, err := readExistingDevboxKey(publicPath)
	if err != nil {
		return "", core.Exit(2, "read existing sealos-devbox public key: %v", err)
	}
	if err := writeDevboxKeyFile(publicPath, []byte(ensureTrailingNewline(keys.PublicKey)), 0o644); err != nil {
		return "", core.Exit(2, "write sealos-devbox public key: %v", err)
	}
	if err := writeDevboxKeyFile(path, []byte(keys.PrivateKey), 0o600); err != nil {
		rollbackErr := os.Remove(publicPath)
		if hadPublic {
			rollbackErr = writeDevboxKeyFile(publicPath, previousPublic, previousPublicMode)
		}
		if rollbackErr != nil {
			return "", core.Exit(2, "write sealos-devbox private key: %v (restore public key: %v)", err, rollbackErr)
		}
		return "", core.Exit(2, "write sealos-devbox private key: %v", err)
	}
	return path, nil
}

func persistDevboxKeyIfClaimUnchanged(leaseID string, expected core.LeaseClaim, server core.Server, keys devboxSecretKeys) (core.LeaseClaim, string, error) {
	var keyPath string
	updated, err := core.UpdateLeaseClaimEndpointIfUnchangedAfter(leaseID, expected, server, core.SSHTarget{}, func() error {
		var persistErr error
		keyPath, persistErr = persistDevboxKey(leaseID, keys)
		return persistErr
	})
	return updated, keyPath, err
}

func readExistingDevboxKey(path string) ([]byte, os.FileMode, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, false, err
	}
	return data, info.Mode().Perm(), true, nil
}

func writeDevboxKeyFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".crabbox-key-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func ensureTrailingNewline(value string) string {
	if strings.HasSuffix(value, "\n") {
		return value
	}
	return value + "\n"
}
