package cli

import (
	"bytes"
	"compress/gzip"
)

// GzipCloudInit preserves cloud-init's binary gzip transport, without a timestamp.
// Callers apply their provider's size limit and outer encoding after compression.
func GzipCloudInit(cloudConfig string) ([]byte, error) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(cloudConfig)); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return compressed.Bytes(), nil
}
