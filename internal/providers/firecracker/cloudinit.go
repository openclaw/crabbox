package firecracker

import (
	"fmt"
	"os"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type cloudInitPayload struct {
	UserData string
	MetaData string
}

type fatFile = shared.FATFile

func buildCloudInitPayload(cfg Config, leaseID, slug, publicKey string) (cloudInitPayload, error) {
	publicKey = strings.TrimSpace(publicKey)
	if publicKey == "" {
		return cloudInitPayload{}, exit(2, "firecracker cloud-init public key is required")
	}
	userData := core.CloudInitUserData(cfg, publicKey)
	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: crabbox-%s\n", leaseID, slug)
	return cloudInitPayload{UserData: userData, MetaData: metaData}, nil
}

func writeCloudInitDrive(path string, payload cloudInitPayload) error {
	image, err := buildFAT16Image("cidata", []fatFile{
		{Name: "user-data", Data: []byte(payload.UserData)},
		{Name: "meta-data", Data: []byte(payload.MetaData)},
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, image, 0o600); err != nil {
		return exit(2, "write firecracker cloud-init drive %s: %v", path, err)
	}
	return nil
}

func buildFAT16Image(label string, files []fatFile) ([]byte, error) {
	return shared.BuildFAT16Image(label, files, "FC%06dTXT", "firecracker cloud-init")
}
