package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWritePrivatePublishesCompletePrivateFileAndCleansFailures(t *testing.T) {
	for _, mode := range []string{"success", "before-replace-failure", "after-replace-failure"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state")
			if err := os.WriteFile(path, []byte("previous"), 0o600); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("replacement failed")
			var staged string
			err := WritePrivate(path, ".state-*.tmp", []byte("complete replacement"), func(from, to string) error {
				staged = from
				if to != path || filepath.Dir(from) != dir {
					t.Fatalf("publication paths %q -> %q", from, to)
				}
				if matches, _ := filepath.Match(".state-*.tmp", filepath.Base(from)); !matches {
					t.Fatalf("staged name=%s", from)
				}
				data, err := os.ReadFile(from)
				if err != nil || string(data) != "complete replacement" {
					t.Fatalf("staged contents=%q error=%v", data, err)
				}
				info, err := os.Stat(from)
				if err != nil {
					t.Fatal(err)
				}
				if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
					t.Fatalf("staged permissions=%v", info.Mode())
				}
				prior, err := os.ReadFile(to)
				if err != nil || string(prior) != "previous" {
					t.Fatalf("destination changed before publication: %q %v", prior, err)
				}
				if mode == "before-replace-failure" {
					return failure
				}
				if err := os.Rename(from, to); err != nil {
					return err
				}
				if mode == "after-replace-failure" {
					return failure
				}
				return nil
			})
			if mode == "success" && err != nil || mode != "success" && err != failure {
				t.Fatalf("error=%v", err)
			}
			if _, err := os.Stat(staged); !os.IsNotExist(err) {
				t.Fatalf("staged path remains: %v", err)
			}
			want := "complete replacement"
			if mode == "before-replace-failure" {
				want = "previous"
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != want {
				t.Fatalf("destination=%q want=%q error=%v", data, want, err)
			}
		})
	}
}

func TestWritePrivateDoesNotCreateParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "state")
	err := WritePrivate(path, ".state-*", nil, func(string, string) error {
		t.Fatal("replacement reached without an existing parent")
		return nil
	})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error=%v", err)
	}
}
