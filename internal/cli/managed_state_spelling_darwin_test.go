package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedStateDarwinDescriptorSpelling(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "MixedDirectory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "MixedFile.txt")
	if err := os.WriteFile(file, []byte("benign marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "OtherLink.txt")
	if err := os.Link(file, link); err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"MixedDirectory", "MixedDirectory/MixedFile.txt", "MixedDirectory/OtherLink.txt"} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		want := filepath.Join(canonicalRoot, filepath.FromSlash(rel))
		t.Run(rel, func(t *testing.T) {
			got, err := NormalizeManagedStateTransferRoot(path)
			if err != nil || got != want {
				t.Fatalf("normalization=%q want=%q error=%v", got, want, err)
			}
		})
		t.Run("case-folded/"+rel, func(t *testing.T) {
			input := filepath.Join(root, strings.ToLower(filepath.FromSlash(rel)))
			aliasInfo, err := os.Lstat(input)
			if os.IsNotExist(err) {
				t.Skip("fixture filesystem does not expose this case-folded alias")
			}
			if err != nil {
				t.Fatal(err)
			}
			originalInfo, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(originalInfo, aliasInfo) {
				t.Skip("fixture case-folded path names a distinct object")
			}
			got, err := NormalizeManagedStateTransferRoot(input)
			if err != nil || got != want {
				t.Fatalf("normalization=%q want=%q error=%v", got, want, err)
			}
		})
	}
	t.Run("backing-firmlink-path", func(t *testing.T) {
		backingRoot := filepath.Join("/System/Volumes/Data", canonicalRoot)
		backingInfo, err := os.Stat(backingRoot)
		if os.IsNotExist(err) {
			t.Skip("fixture filesystem has no boot Data backing alias")
		}
		if err != nil {
			t.Fatalf("owned backing-path fixture is unavailable: %v", err)
		}
		rootInfo, err := os.Stat(root)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(rootInfo, backingInfo) {
			t.Skip("backing path does not identify the owned fixture")
		}
		want := filepath.Join(backingRoot, "MixedDirectory", "OtherLink.txt")
		got, err := NormalizeManagedStateTransferRoot(want)
		if err != nil || got != want {
			t.Fatalf("backing namespace changed: got=%q want=%q error=%v", got, want, err)
		}
	})
	alias := filepath.Join(root, "parent-alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"MixedFile.txt", "absent/leaf"} {
		got, err := NormalizeManagedStateTransferRoot(filepath.Join(alias, filepath.FromSlash(rel)))
		want := filepath.Join(canonicalRoot, "MixedDirectory", filepath.FromSlash(rel))
		if err != nil || got != want {
			t.Fatalf("symlink-parent normalization=%q want=%q error=%v", got, want, err)
		}
	}
	for _, path := range []string{file, link} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "benign marker\n" {
			t.Fatal("metadata normalization changed an owned marker")
		}
	}
}
