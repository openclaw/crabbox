//go:build darwin || linux

package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const coldTestToken = "0123456789abcdef0123456789abcdef"

func prepareColdTestManifest(t *testing.T, workdir string, files []string) string {
	t.Helper()
	manifest := SyncManifest{Files: files}
	command := remoteWriteSyncManifestsNewWithMetadataMode(workdir, coldTestToken,
		remotePlainManifestGitFunction()+remotePlainManifestSyncMetaDirScript(), true, true)
	cmd := exec.CommandContext(t.Context(), "sh", "-c", command)
	cmd.Stdin = strings.NewReader(syncManifestInputForTarget(SSHTarget{}, manifest.NUL(), nil))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("prepare cold manifest: %v: %s", err, output)
	}
	return string(output)
}

func TestColdSyncProbePreservesNonemptyWorkspaces(t *testing.T) {
	for _, kind := range []string{"absent", "empty", "file", "hidden", "newline", "git", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			workdir := filepath.Join(root, "work space's")
			if kind != "absent" {
				if err := os.Mkdir(workdir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "file", "hidden", "newline", "git":
				name := map[string]string{"file": "keep", "hidden": ".keep", "newline": "\n", "git": ".git"}[kind]
				writeFile(t, filepath.Join(workdir, name), "unmanaged")
			case "symlink":
				writeFile(t, filepath.Join(workdir, "keep"), "unmanaged")
				link := filepath.Join(root, "alias")
				if err := os.Symlink(workdir, link); err != nil {
					t.Fatal(err)
				}
				workdir = link
			}
			output := prepareColdTestManifest(t, workdir, []string{"payload"})
			want := kind == "absent" || kind == "empty"
			if (output == coldSyncReady) != want {
				t.Fatalf("probe output=%q want cold=%t", output, want)
			}
		})
	}
}

func TestColdSyncRoundTripAndFinalize(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "gzip"}[compressed], func(t *testing.T) {
			root, workdir := t.TempDir(), filepath.Join(t.TempDir(), "remote's workspace")
			if err := os.MkdirAll(filepath.Join(root, "target"), 0o710); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(root, "target", "keep name\n.txt"), "selected contents\n")
			writeFile(t, filepath.Join(root, "target", "ignored"), "not selected\n")
			writeFile(t, filepath.Join(root, "executable"), "#!/bin/sh\nexit 0\n")
			if err := os.Chmod(filepath.Join(root, "executable"), 0o751); err != nil {
				t.Fatal(err)
			}
			stamp := time.Unix(1700000000, 123456789)
			if err := os.Chtimes(filepath.Join(root, "executable"), stamp, stamp); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("missing target", filepath.Join(root, "link")); err != nil {
				t.Fatal(err)
			}
			files := []string{"executable", "link", "target/keep name\n.txt"}
			if got := prepareColdTestManifest(t, workdir, files); got != coldSyncReady {
				t.Fatalf("probe=%q", got)
			}
			if err := os.Chmod(workdir, 0o711); err != nil {
				t.Fatal(err)
			}
			dirs, eligible, err := coldSyncDirectories(root, files)
			if err != nil || !eligible {
				t.Fatalf("plan: %t %v", eligible, err)
			}
			var archive bytes.Buffer
			if err := writeColdSyncArchive(t.Context(), &archive, root, files, dirs, compressed); err != nil {
				t.Fatal(err)
			}
			// Inspect the exact stream independently of the receiver.
			var reader io.Reader = bytes.NewReader(archive.Bytes())
			if compressed {
				gz, err := gzip.NewReader(reader)
				if err != nil {
					t.Fatal(err)
				}
				defer gz.Close()
				reader = gz
			}
			tr := tar.NewReader(reader)
			seen := map[string]bool{}
			for {
				h, err := tr.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				seen[h.Name] = true
			}
			if len(seen) != 4 || seen["target/ignored"] || seen["."] {
				t.Fatalf("archive membership: %v", seen)
			}
			cmd := exec.CommandContext(t.Context(), "sh", "-c", remoteColdSyncExtract(workdir, coldTestToken, compressed))
			cmd.Stdin = bytes.NewReader(archive.Bytes())
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("extract: %v: %s", err, out)
			}
			got, err := os.ReadFile(filepath.Join(workdir, files[2]))
			if err != nil || string(got) != "selected contents\n" {
				t.Fatalf("contents: %q %v", got, err)
			}
			if _, err := os.Stat(filepath.Join(workdir, "target", "ignored")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unselected file copied: %v", err)
			}
			link, err := os.Readlink(filepath.Join(workdir, "link"))
			if err != nil || link != "missing target" {
				t.Fatalf("symlink: %q %v", link, err)
			}
			info, err := os.Stat(filepath.Join(workdir, "executable"))
			if err != nil || info.Mode().Perm() != 0o751 || !info.ModTime().Equal(stamp) {
				t.Fatalf("file metadata: %v %v", info, err)
			}
			info, err = os.Stat(filepath.Join(workdir, "target"))
			if err != nil || info.Mode().Perm() != 0o710 {
				t.Fatalf("directory mode: %v %v", info, err)
			}
			info, err = os.Stat(workdir)
			if err != nil || info.Mode().Perm() != 0o711 {
				t.Fatalf("root mode changed: %v %v", info, err)
			}
			cmd = exec.CommandContext(t.Context(), "sh", "-c", remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{PlainManifest: true, Token: coldTestToken}))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("finalize: %v: %s", err, out)
			}
			got, err = os.ReadFile(filepath.Join(workdir, ".crabbox", "sync-manifest"))
			if err != nil || string(got) != string((SyncManifest{Files: files}).NUL()) {
				t.Fatalf("final manifest: %q %v", got, err)
			}
		})
	}
}

func TestColdSyncPreservesNonUTF8Paths(t *testing.T) {
	for _, kind := range []string{"filename", "symlink-target"} {
		for _, compressed := range []bool{false, true} {
			t.Run(kind+"/gzip="+strconv.FormatBool(compressed), func(t *testing.T) {
				root, workdir := t.TempDir(), filepath.Join(t.TempDir(), "workspace")
				name, linkname := "legacy-\xff.txt", ""
				if kind == "symlink-target" {
					name, linkname = "link", name
					if err := os.Symlink(linkname, filepath.Join(root, name)); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(filepath.Join(root, name), []byte("contents"), 0o751); err != nil {
					if errors.Is(err, syscall.EILSEQ) {
						t.Skip("source filesystem requires UTF-8 filenames")
					}
					t.Fatal(err)
				}
				files := []string{name}
				if got := prepareColdTestManifest(t, workdir, files); got != coldSyncReady {
					t.Fatalf("probe=%q", got)
				}
				var archive bytes.Buffer
				if err := writeColdSyncArchive(t.Context(), &archive, root, files, nil, compressed); err != nil {
					t.Fatal(err)
				}
				cmd := exec.CommandContext(t.Context(), "sh", "-c", remoteColdSyncExtract(workdir, coldTestToken, compressed))
				cmd.Stdin = &archive
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("extract: %v: %s", err, out)
				}
				if linkname != "" {
					got, err := os.Readlink(filepath.Join(workdir, name))
					if err != nil || got != linkname {
						t.Fatalf("link=%q, want %q: %v", got, linkname, err)
					}
				} else {
					got, err := os.ReadFile(filepath.Join(workdir, name))
					if err != nil || string(got) != "contents" {
						t.Fatalf("contents=%q: %v", got, err)
					}
				}
			})
		}
	}
}

func TestColdSyncDirectoryRestorationOrder(t *testing.T) {
	root := t.TempDir()
	stamp := time.Unix(1700000000, 123456789)
	for _, dir := range []string{"a", "b"} {
		writeFile(t, filepath.Join(root, dir, "file"), "contents")
		if err := os.Chmod(filepath.Join(root, dir), 0o555); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(root, dir), stamp, stamp); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, dir), 0o700) })
	}
	writeFile(t, filepath.Join(root, "a!"), "prefix sibling")
	files := []string{"a!", "a/file", "b/file"}
	dirs, _, err := coldSyncDirectories(root, files)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := writeColdSyncArchive(t.Context(), &archive, root, files, dirs, false); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(archive.Bytes()))
	var order []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, strings.TrimSuffix(h.Name, "/"))
	}
	if strings.Join(order, "|") != "a!|a|a/file|b|b/file" {
		t.Fatalf("directory metadata can be restored before its children: %v", order)
	}
	workdir := filepath.Join(t.TempDir(), "workspace")
	prepareColdTestManifest(t, workdir, files)
	for _, dir := range []string{"a", "b"} {
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(workdir, dir), 0o700) })
	}
	cmd := exec.CommandContext(t.Context(), "sh", "-c", remoteColdSyncExtract(workdir, coldTestToken, false))
	cmd.Stdin = bytes.NewReader(archive.Bytes())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("read-only directories: %v: %s", err, out)
	}
	for _, dir := range []string{"a", "b"} {
		info, err := os.Stat(filepath.Join(workdir, dir))
		if err != nil || info.Mode().Perm() != 0o555 || !info.ModTime().Equal(stamp) {
			t.Fatalf("directory %s metadata: %v %v", dir, info, err)
		}
	}
}

func TestColdSyncRechecksWorkspaceBeforeExtraction(t *testing.T) {
	for _, name := range []string{"unmanaged", ".hidden", ".crabbox/config"} {
		t.Run(name, func(t *testing.T) {
			workdir := filepath.Join(t.TempDir(), "workspace")
			prepareColdTestManifest(t, workdir, []string{"payload"})
			writeFile(t, filepath.Join(workdir, name), "preserved")
			cmd := exec.CommandContext(t.Context(), "sh", "-c", remoteColdSyncExtract(workdir, coldTestToken, false))
			cmd.Stdin = strings.NewReader("not an archive")
			out, err := cmd.CombinedOutput()
			if exitCode(err) != coldSyncSkippedCode || string(out) != coldSyncSkipped {
				t.Fatalf("recheck: %v %q", err, out)
			}
			got, err := os.ReadFile(filepath.Join(workdir, name))
			if err != nil || string(got) != "preserved" {
				t.Fatalf("unmanaged content: %q %v", got, err)
			}
		})
	}
}

func TestColdSyncAncestorAndCancellation(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(outside, "file"), "outside")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, eligible, err := coldSyncDirectories(root, []string{"link/file"}); err != nil || eligible {
		t.Fatalf("symlink ancestor: %t %v", eligible, err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	dirs, eligible, err := coldSyncDirectories(root, []string{"dir/file"})
	if err != nil || !eligible {
		t.Fatalf("plan: %t %v", eligible, err)
	}
	if err := os.Remove(filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := writeColdSyncArchive(t.Context(), &output, root, []string{"dir/file"}, dirs, false); err == nil || output.Len() != 0 {
		t.Fatalf("changed parent accepted: %v bytes=%d", err, output.Len())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := writeColdSyncArchive(ctx, io.Discard, outside, []string{"file"}, nil, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestColdSyncSpecialFilesRetainRsync(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	managed, err := newManagedSyncScope(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := projectSyncManifest(root, SyncExcludeRules{}, nil, []string{"pipe"}, syncManifestScope{}, managed)
	if err != nil || !manifest.hasSpecialFiles || len(manifest.Files) != 1 {
		t.Fatalf("special-file manifest: %+v %v", manifest, err)
	}
}

func TestColdSyncStreamJoinsProducerAndSSH(t *testing.T) {
	for _, scenario := range []string{"complete", "nonempty", "source-error", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			root, workdir, bin := t.TempDir(), filepath.Join(t.TempDir(), "workspace"), t.TempDir()
			writeFile(t, filepath.Join(root, "payload"), strings.Repeat("x", 4<<20))
			files := []string{"payload"}
			prepareColdTestManifest(t, workdir, files)
			if scenario == "nonempty" || scenario == "source-error" {
				writeFile(t, filepath.Join(workdir, "unmanaged"), "keep")
			}
			if scenario == "source-error" {
				files = []string{"missing"}
			}
			pidFile := filepath.Join(bin, "pid")
			script := "#!/bin/sh\nfor last do :; done\nexec /bin/sh -c \"$last\"\n"
			if scenario == "cancel" {
				script = "#!/bin/sh\nprintf '%s' $$ > " + shellQuote(pidFile+".tmp") + "\nmv " + shellQuote(pidFile+".tmp") + " " + shellQuote(pidFile) + "\nexec sleep 30\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type outcome struct {
				used bool
				err  error
			}
			done := make(chan outcome, 1)
			go func() {
				used, err := streamColdSync(ctx, SSHTarget{Host: "fixture.invalid", User: "runner", Port: "22"}, root, workdir, coldTestToken, files, "never", 20*time.Second, io.Discard)
				done <- outcome{used, err}
			}()
			if scenario == "cancel" {
				deadline := time.Now().Add(10 * time.Second)
				for {
					if _, err := os.Stat(pidFile); err == nil {
						break
					}
					if time.Now().After(deadline) {
						cancel()
						<-done
						t.Fatal("SSH did not start")
					}
					time.Sleep(10 * time.Millisecond)
				}
				cancel()
			}
			var result outcome
			select {
			case result = <-done:
			case <-time.After(25 * time.Second):
				cancel()
				t.Fatal("producer or SSH was not joined")
			}
			switch scenario {
			case "complete":
				if !result.used || result.err != nil {
					t.Fatalf("transfer: %+v", result)
				}
				got, err := os.ReadFile(filepath.Join(workdir, "payload"))
				if err != nil || len(got) != 4<<20 {
					t.Fatalf("payload: bytes=%d err=%v", len(got), err)
				}
			case "nonempty":
				if result.used || result.err != nil {
					t.Fatalf("fallback: %+v", result)
				}
				if _, err := os.Stat(filepath.Join(workdir, "payload")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("fallback wrote files: %v", err)
				}
			case "source-error":
				if !errors.Is(result.err, os.ErrNotExist) {
					t.Fatalf("fallback hid producer failure: %+v", result)
				}
			case "cancel":
				if !result.used || !errors.Is(result.err, context.Canceled) {
					t.Fatalf("cancel: %+v", result)
				}
				data, err := os.ReadFile(pidFile)
				if err != nil {
					t.Fatal(err)
				}
				pid, err := strconv.Atoi(string(data))
				if err != nil {
					t.Fatal(err)
				}
				if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
					t.Fatalf("SSH survived cancellation: %v", err)
				}
			}
		})
	}
}
