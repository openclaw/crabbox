package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFailureBundleWritableProjectKeepsDefault(t *testing.T) {
	t.Chdir(t.TempDir())
	file, path, err := openFailureBundleDestination("bundle.tar.gz", func() (string, error) {
		t.Fatal("writable project must not resolve a fallback")
		return "", nil
	}, openFailureBundleFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(".crabbox", "captures", "bundle.tar.gz"); path != want {
		t.Fatalf("path=%q want=%q", path, want)
	}
}

func TestFailureBundleUnsafeDestinationDoesNotFallback(t *testing.T) {
	for _, kind := range []string{"symlink", "file"} {
		for _, destination := range []string{".crabbox", filepath.Join(".crabbox", "captures"), filepath.Join(".crabbox", "captures", "bundle.tar.gz")} {
			t.Run(kind+"/"+destination, func(t *testing.T) {
				t.Chdir(t.TempDir())
				if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink" {
					if err := os.Symlink(t.TempDir(), destination); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
				} else if filepath.Ext(destination) == ".gz" {
					if err := os.Mkdir(destination, 0o700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(destination, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				file, _, err := openFailureBundleDestination("bundle.tar.gz", func() (string, error) {
					t.Fatal("unsafe destination must not resolve a fallback")
					return "", nil
				}, openFailureBundleFile)
				if err == nil || file != nil || !strings.Contains(err.Error(), "unsafe failure bundle destination") {
					t.Fatalf("file=%v err=%v", file, err)
				}
			})
		}
	}
}

func TestFailureBundleRejectsNonLocalName(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../bundle.tar.gz", "nested/bundle.tar.gz", string(os.PathSeparator) + "bundle.tar.gz"} {
		t.Run(name, func(t *testing.T) {
			file, path, err := openFailureBundleDestination(name, func() (string, error) {
				t.Fatal("invalid name must not resolve a fallback")
				return "", nil
			}, openFailureBundleFile)
			if err == nil || file != nil || path != "" {
				t.Fatalf("file=%v path=%q err=%v", file, path, err)
			}
		})
	}
}

func TestFailureBundleReadErrorsDoNotFallback(t *testing.T) {
	for _, source := range []string{"remote archive", "stdout"} {
		t.Run(source, func(t *testing.T) {
			isolateTestUserDirs(t)
			t.Chdir(t.TempDir())
			meta := FailureCaptureMetadata{}
			remotePath := ""
			if source == "remote archive" {
				remotePath = "broken.tar.gz"
				if err := os.WriteFile(remotePath, []byte("not gzip"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				meta.StdoutPath = t.TempDir()
			}
			path, _, err := writeLocalFailureBundle("bundle.tar.gz", remotePath, meta)
			if err == nil || path != filepath.Join(".crabbox", "captures", "bundle.tar.gz") {
				t.Fatalf("path=%q err=%v", path, err)
			}
			assertNoFailureBundleFallback(t)
		})
	}
}

func assertNoFailureBundleFallback(t *testing.T) {
	t.Helper()
	state, err := crabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "captures")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected fallback directory: %v", err)
	}
}
