package sealosdevbox

import (
	"encoding/base64"
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
	Metadata devboxMeta         `json:"metadata"`
	Data     map[string]string  `json:"data"`
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
	if err := os.WriteFile(path, []byte(keys.PrivateKey), 0o600); err != nil {
		return "", core.Exit(2, "write sealos-devbox private key: %v", err)
	}
	_ = os.Chmod(path, 0o600)
	if err := os.WriteFile(path+".pub", []byte(ensureTrailingNewline(keys.PublicKey)), 0o644); err != nil {
		return "", core.Exit(2, "write sealos-devbox public key: %v", err)
	}
	return path, nil
}

func ensureTrailingNewline(value string) string {
	if strings.HasSuffix(value, "\n") {
		return value
	}
	return value + "\n"
}
