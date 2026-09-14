package applevmhelper

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadDiagnosticTail(t *testing.T) {
	for _, tt := range []struct {
		name, contents string
		limit          int64
		want           string
	}{
		{"empty", "", 8, ""},
		{"whole file", " message\n", 100, "message"},
		{"bounded tail", "prefix-tail", 4, "tail"},
		{"trim after bounding", "prefix end \n", 5, "end"},
		{"raw bytes", "\xfe\xffx", 2, "\xffx"},
		{"zero limit", "message", 0, ""},
		{"negative limit", "message", -1, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "diagnostic.log")
			if err := os.WriteFile(path, []byte(tt.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := ReadDiagnosticTail(path, tt.limit)
			if err != nil || got != tt.want {
				t.Fatalf("tail=(%q, %v), want %q", got, err, tt.want)
			}
		})
	}
}

func TestReadDiagnosticTailPreservesFileError(t *testing.T) {
	_, err := ReadDiagnosticTail(filepath.Join(t.TempDir(), "missing.log"), 100)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error=%v", err)
	}
}
