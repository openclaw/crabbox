//go:build windows

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestClaimMetadataOpenReaderAllowsReplacementAndCleanup(t *testing.T) {
	isolateTestUserDirs(t)
	before := seedClaimContract(t)
	path, err := leaseClaimPath(before.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	type snapshot struct {
		reader io.Reader
		want   leaseClaim
	}
	var snapshots []snapshot
	// Retain every reader through repeated replacement, cleanup and recreation,
	// including reuse of the deterministic .deleted namespace.
	for cycle := range 3 {
		if cycle > 0 {
			if err := claimLeaseForRepoProvider(before.LeaseID, "contract", "aws", fmt.Sprintf("/repo/%d", cycle), time.Minute, false); err != nil {
				t.Fatalf("recreate with prior readers open: %v", err)
			}
			before, err = readLeaseClaim(before.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
		}
		// Hold the same OS opener used by claim reads across namespace changes.
		oldFile, err := openArtifactReadOnly(path)
		if err != nil {
			t.Fatal(err)
		}
		defer oldFile.Close()
		replacement := cloneLeaseClaim(before)
		replacement.RepoRoot = fmt.Sprintf("/repo/replacement/%d", cycle)
		updated, err := replaceLeaseClaimIfUnchangedDurableReturning(before.LeaseID, before, replacement)
		if err != nil {
			t.Fatalf("replace with old reader open: %v", err)
		}
		currentFile, err := openArtifactReadOnly(path)
		if err != nil {
			t.Fatal(err)
		}
		defer currentFile.Close()
		called := false
		err = removeLeaseClaimIfUnchangedAfter(before.LeaseID, before, func() error { called = true; return nil })
		if err == nil || !strings.Contains(err.Error(), "claim changed") || called {
			t.Fatalf("stale cleanup passed exact guard: called=%t err=%v", called, err)
		}
		assertClaimContractStored(t, updated.LeaseID, updated)
		if err := removeLeaseClaimIfUnchanged(updated.LeaseID, updated); err != nil {
			t.Fatalf("guarded cleanup with current reader open: %v", err)
		}
		if _, exists, err := readLeaseClaimWithPresence(updated.LeaseID); err != nil || exists {
			t.Fatalf("claim remains after cleanup: exists=%t err=%v", exists, err)
		}
		if _, err := os.Lstat(path + ".deleted"); !os.IsNotExist(err) {
			t.Fatalf("tombstone namespace not released: %v", err)
		}
		snapshots = append(snapshots, snapshot{oldFile, before}, snapshot{currentFile, updated})
	}
	for _, snapshot := range snapshots {
		data, err := io.ReadAll(snapshot.reader)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeLeaseClaim(path, data)
		if err != nil || !reflect.DeepEqual(got, snapshot.want) {
			t.Fatalf("open reader lost published snapshot: got=%#v err=%v", got, err)
		}
	}
}

func TestClaimMetadataWindowsConfigSnapshots(t *testing.T) {
	for _, kind := range []string{"relative", "unicode", "long", "extended"} {
		t.Run(kind, func(t *testing.T) {
			isolateTestUserDirs(t)
			dir := t.TempDir()
			if kind == "long" || kind == "extended" {
				for len(dir) < 300 {
					dir = filepath.Join(dir, strings.Repeat("nested", 8))
				}
			}
			path := filepath.Join(dir, "配置-🦀.yaml")
			if kind == "relative" {
				t.Chdir(dir)
				path = filepath.Join("nested", "config.yaml")
			} else if kind == "extended" {
				path = `\\?\` + path
			}
			t.Setenv("CRABBOX_CONFIG", path)
			var readers []*os.File
			for generation := range 3 {
				profile := fmt.Sprintf("snapshot-%d", generation)
				if _, err := writeUserFileConfig(fileConfig{Profile: profile}); err != nil {
					t.Fatalf("publish config: %v", err)
				}
				cfg, err := readFileConfig(path)
				if err != nil || cfg.Profile != profile {
					t.Fatalf("new open did not see config %q: profile=%q err=%v", profile, cfg.Profile, err)
				}
				reader, err := openArtifactReadOnly(path)
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				readers = append(readers, reader)
			}
			for generation, reader := range readers {
				data, err := io.ReadAll(reader)
				if err != nil || !strings.Contains(string(data), fmt.Sprintf("profile: snapshot-%d\n", generation)) {
					t.Fatalf("lost config snapshot %d: %q err=%v", generation, data, err)
				}
			}
		})
	}
}

func TestClaimMetadataWindowsNamespaceLinkEntries(t *testing.T) {
	for _, operation := range []string{"rename source", "replace target", "cleanup", "recover tombstone", "config referent"} {
		t.Run(operation, func(t *testing.T) {
			dir := t.TempDir()
			target, link, destination := filepath.Join(dir, "referent"), filepath.Join(dir, "link"), filepath.Join(dir, "destination")
			if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
					t.Skipf("symlink privilege unavailable: %v", err)
				}
				t.Fatal(err)
			}
			reader, err := openArtifactReadOnly(target)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			switch operation {
			case "rename source":
				if err := replaceControllerFile(link, destination); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(destination)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("renamed referent instead of link: %v", err)
				}
			case "replace target":
				if err := os.WriteFile(destination, []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := replaceClaimFile(destination, link); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(link)
				if err != nil || !info.Mode().IsRegular() {
					t.Fatalf("did not replace link entry: %v", err)
				}
			case "cleanup":
				if err := removeControllerFile(link); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(link); !os.IsNotExist(err) {
					t.Fatalf("link entry remains after cleanup: %v", err)
				}
			case "recover tombstone":
				if err := os.Rename(link, destination+".deleted"); err != nil {
					t.Fatal(err)
				}
				if err := removeControllerFile(destination); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(destination + ".deleted"); !os.IsNotExist(err) {
					t.Fatalf("link tombstone remains after recovery: %v", err)
				}
			case "config referent":
				if err := writeUserFileConfigAtomic(link, []byte("updated"), replaceClaimFile, fsyncDir); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(link)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("config writer did not preserve link: %v", err)
				}
			}
			want := "original"
			if operation == "config referent" {
				want = "updated"
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != want {
				t.Fatalf("unexpected referent mutation: %q err=%v", data, err)
			}
			data, err = io.ReadAll(reader)
			if err != nil || string(data) != "original" {
				t.Fatalf("lost referent snapshot: %q err=%v", data, err)
			}
		})
	}
}

func TestClaimMetadataWindowsNamespaceFailuresPreserveFiles(t *testing.T) {
	for _, kind := range []string{"source NUL", "target NUL", "missing source", "missing target directory", "readonly target", "readonly tombstone"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			source, target := filepath.Join(dir, "source"), filepath.Join(dir, "target")
			if kind == "readonly tombstone" {
				target = source + ".deleted"
			}
			for _, path := range []string{source, target} {
				if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			from, to := source, target
			switch kind {
			case "source NUL":
				from += "\x00suffix"
			case "target NUL":
				to += "\x00suffix"
			case "missing source":
				from = filepath.Join(dir, "missing-source")
			case "missing target directory":
				to = filepath.Join(dir, "missing", "target")
			default:
				if err := os.Chmod(target, 0o400); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(target, 0o600) })
			}
			var err error
			if kind == "readonly tombstone" {
				err = removeControllerFile(source)
			} else {
				err = replaceClaimFile(from, to)
			}
			if err == nil {
				t.Fatal("invalid/readonly namespace mutation succeeded")
			}
			if errors.Is(err, os.ErrNotExist) != (kind == "missing source") {
				t.Fatalf("only a missing source open may report absence: %v", err)
			}
			for _, path := range []string{source, target} {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != filepath.Base(path) {
					t.Fatalf("failure lost recoverable file %q: %q err=%v", path, data, err)
				}
			}
		})
	}
}
