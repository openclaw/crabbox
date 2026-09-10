package testutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func RequireRedactedProviderError(t *testing.T, err error, secret string) {
	t.Helper()
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("error=%v, want redacted useful provider error", err)
	}
}

func GRPCWebEnvelope(flags byte, value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var out bytes.Buffer
	out.WriteByte(flags)
	out.Write([]byte{byte(len(data) >> 24), byte(len(data) >> 16), byte(len(data) >> 8), byte(len(data))})
	out.Write(data)
	return out.Bytes()
}

func TarGzipContains(t *testing.T, data []byte, name string) bool {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == name {
			return true
		}
	}
}
