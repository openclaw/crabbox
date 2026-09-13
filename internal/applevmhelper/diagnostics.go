package applevmhelper

import (
	"io"
	"os"
	"strings"
)

// ReadDiagnosticTail reads a byte-bounded file tail and trims its surrounding
// whitespace. Callers retain diagnostic labels, error handling, and redaction.
func ReadDiagnosticTail(path string, limit int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	offset := info.Size() - limit
	if offset < 0 {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
