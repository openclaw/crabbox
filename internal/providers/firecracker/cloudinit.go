package firecracker

import (
	"fmt"
	"os"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/fat16"
)

type cloudInitPayload struct {
	UserData string
	MetaData string
}

func buildCloudInitPayload(cfg core.Config, leaseID, slug, publicKey string) (cloudInitPayload, error) {
	publicKey = strings.TrimSpace(publicKey)
	if publicKey == "" {
		return cloudInitPayload{}, core.Exit(2, "firecracker cloud-init public key is required")
	}
	userData := core.CloudInitUserData(cfg, publicKey)
	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: crabbox-%s\n", leaseID, slug)
	return cloudInitPayload{UserData: userData, MetaData: metaData}, nil
}

func writeCloudInitDrive(path string, payload cloudInitPayload) error {
	image, err := buildFAT16Image("cidata", []fat16.File{
		{Name: "user-data", Data: []byte(payload.UserData)},
		{Name: "meta-data", Data: []byte(payload.MetaData)},
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, image, 0o600); err != nil {
		return core.Exit(2, "write firecracker cloud-init drive %s: %v", path, err)
	}
	return nil
}

func buildFAT16Image(label string, files []fat16.File) ([]byte, error) {
	image, err := fat16.Build(label, files, "FC%06dTXT")
	if err != nil {
		return nil, core.Exit(2, "firecracker cloud-init %v", err)
	}
	return image, nil
}
