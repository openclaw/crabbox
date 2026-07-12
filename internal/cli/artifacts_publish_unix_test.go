//go:build !windows

package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestArtifactsPublishRejectsSummaryFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "safe.txt"), "safe")
	summaryPath := filepath.Join(dir, "summary.md")
	if err := unix.Mkfifo(summaryPath, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- (App{Stdout: io.Discard, Stderr: io.Discard}).artifactsPublish(context.Background(), []string{
			"--dir", dir,
			"--storage", "local",
			"--summary-file", summaryPath,
		})
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("error=%v, want non-regular summary rejection", err)
		}
	case <-time.After(time.Second):
		t.Fatal("summary FIFO validation blocked while opening the file")
	}
}

func TestSnapshotArtifactFilesRejectsFIFOSwapWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.txt")
	mustWriteFile(t, path, "safe")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := listArtifactBundleFilesRoot(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, cleanup, err := snapshotArtifactFiles(root, files)
		cleanup()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "changed after validation") {
			t.Fatalf("error=%v, want FIFO swap rejection", err)
		}
	case <-time.After(time.Second):
		t.Fatal("artifact FIFO swap blocked while opening the validated path")
	}
}

func TestOpenArtifactSnapshotRejectsPathSwap(t *testing.T) {
	bundle := t.TempDir()
	bundleRoot, err := os.OpenRoot(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer bundleRoot.Close()
	snapshot, cleanup, err := createArtifactSnapshotFile(bundleRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := snapshot.WriteString("safe-bytes"); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-secret")
	mustWriteFile(t, outside, "leak-bytes")
	if err := os.Remove(snapshot.Name()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, snapshot.Name()); err != nil {
		t.Fatal(err)
	}
	file := validatedArtifactData(artifactFile{Name: "result.txt"}, []byte("safe-bytes"))
	file.snapshotFile = snapshot
	data, err := io.ReadAll(io.NewSectionReader(file.snapshotFile, file.snapshotOffset, file.snapshotSize))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "safe-bytes" {
		t.Fatalf("snapshot=%q, want handle-bound bytes", data)
	}
}

func TestArtifactPublishSummaryRejectsResolvedSuffixAfterDotDot(t *testing.T) {
	base := t.TempDir()
	bundle := filepath.Join(base, "bundle")
	nested := filepath.Join(bundle, "nested")
	bridge := filepath.Join(base, "bridge")
	outside := filepath.Join(base, "outside")
	secret := filepath.Join(base, "secret")
	for _, dir := range []string{nested, bridge, filepath.Join(outside, "child"), secret} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWriteFile(t, filepath.Join(nested, "summary.md"), "safe-summary")
	mustWriteFile(t, filepath.Join(secret, "summary.md"), "outside-secret")
	if err := os.Symlink(filepath.Join(outside, "child"), filepath.Join(bridge, "pivot")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink(nested, filepath.Join(outside, "next")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	alias := filepath.Join(base, "summary-alias.md")
	if err := os.Symlink("bridge/pivot/../next/summary.md", alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	originalNested := nested + ".original"
	if err := os.Rename(nested, originalNested); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, nested); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	binding, err := bindArtifactSummaryFile(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer binding.file.Close()
	if err := os.Remove(nested); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(originalNested, nested); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := listArtifactBundleRoot(root, bundle)
	if err != nil {
		t.Fatal(err)
	}
	inside := artifactSummaryInsideBundle(bundle, bundle, mustArtifactRootInfo(t, root), binding, files)
	if !inside {
		t.Fatalf("resolved suffix alias classified external; targets=%#v", binding.symlinkTargets)
	}
	_, cleanup, err := artifactPublishSummaryText("", binding, inside, root, files)
	defer cleanup()
	if err == nil || !strings.Contains(err.Error(), "summary file changed") {
		t.Fatalf("error=%v, want outside identity rejection", err)
	}
}

func TestHostedArtifactUploadsStreamValidatedSnapshot(t *testing.T) {
	for _, storage := range []string{"s3", "cloudflare"} {
		t.Run(storage, func(t *testing.T) {
			bundle := t.TempDir()
			path := filepath.Join(bundle, "result.txt")
			mustWriteFile(t, path, "safe-bytes")
			root, err := os.OpenRoot(bundle)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			files, err := listArtifactBundleFilesRoot(root, bundle)
			if err != nil {
				t.Fatal(err)
			}
			snapshots, cleanup, err := snapshotArtifactFiles(root, files)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			outside := filepath.Join(t.TempDir(), "outside-secret")
			mustWriteFile(t, outside, "leak-bytes")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}

			binDir := t.TempDir()
			bodyPath := filepath.Join(t.TempDir(), "body")
			argsPath := filepath.Join(t.TempDir(), "args")
			tool := "aws"
			if storage == "cloudflare" {
				tool = "wrangler"
			}
			script := filepath.Join(binDir, tool)
			if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CRABBOX_TEST_ARGS\"\n/bin/cat > \"$CRABBOX_TEST_BODY\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CRABBOX_TEST_BODY", bodyPath)
			t.Setenv("CRABBOX_TEST_ARGS", argsPath)
			opts := artifactPublishOptions{Storage: storage, Bucket: "qa", Prefix: "runs/abc"}
			if _, err := publishArtifactFiles(context.Background(), opts, snapshots); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(bodyPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != "safe-bytes" {
				t.Fatalf("stdin=%q, want validated snapshot", body)
			}
			args, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatal(err)
			}
			if storage == "s3" && !strings.Contains(string(args), "\n-\n") {
				t.Fatalf("aws args=%q, want stdin source", args)
			}
			if storage == "cloudflare" && !strings.Contains(string(args), "--pipe") {
				t.Fatalf("wrangler args=%q, want --pipe", args)
			}
		})
	}
}
