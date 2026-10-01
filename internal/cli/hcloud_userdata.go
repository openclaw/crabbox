package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"strings"
)

func hetznerUserData(cloudConfig string) (string, error) {
	const limit = 32768
	if len(cloudConfig) <= limit {
		return cloudConfig, nil
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(cloudConfig)); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	// Cloud-init decodes MIME transfer encoding, then detects the gzipped cloud-config.
	var envelope strings.Builder
	envelope.WriteString("MIME-Version: 1.0\nContent-Type: application/gzip\nContent-Transfer-Encoding: base64\n\n")
	encoded := base64.StdEncoding.EncodeToString(compressed.Bytes())
	for len(encoded) > 76 {
		envelope.WriteString(encoded[:76] + "\n")
		encoded = encoded[76:]
	}
	envelope.WriteString(encoded + "\n")
	if envelope.Len() > limit {
		return "", fmt.Errorf("Hetzner user_data limit is %d bytes; compressed cloud-init MIME is %d bytes", limit, envelope.Len())
	}
	return envelope.String(), nil
}
