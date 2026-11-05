package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type oversizedSFTPTransport struct {
	admitted int64
	finished chan struct{}
}

func (oversizedSFTPTransport) metadata(size int64) []byte {
	sum := sha256.Sum256(nil)
	data, _ := json.Marshal(windowsSFTPMetadata{Size: size, SHA256: hex.EncodeToString(sum[:])})
	return data
}

func (t oversizedSFTPTransport) guest(context.Context, string) ([]byte, error) {
	return t.metadata(t.admitted), nil
}

// copy streams past the admitted size and keeps going until it is canceled.
func (t oversizedSFTPTransport) copy(ctx context.Context, local, _ string, _ bool) error {
	defer close(t.finished)
	file, err := os.Create(local)
	if err != nil {
		return err
	}
	defer file.Close()
	chunk := make([]byte, 4096)
	for ctx.Err() == nil {
		if _, err := file.Write(chunk); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func TestWindowsSFTPDownloadAbortsWhenGuestFileExceedsAdmittedSize(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "evidence.bin")
	transport := oversizedSFTPTransport{admitted: 8192, finished: make(chan struct{})}
	err := downloadWindowsSFTP(context.Background(), windowsSFTPRequest{local: dest, remote: `C:\evidence\out.bin`}, transport)
	if !errors.Is(err, errWindowsSFTPExceededAdmittedSize) {
		t.Fatalf("download error = %v, want admitted-size abort", err)
	}
	<-transport.finished
	if _, statErr := os.Lstat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("oversized download was published: %v", statErr)
	}
}
