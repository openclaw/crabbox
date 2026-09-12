package blacksmith

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func isolateArtifactOwnership(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("bounded native artifact download requires macOS or Linux")
	}
	isolateBlacksmithOwnership(t)
	bin := t.TempDir()
	for _, name := range []string{"blacksmith", "scp"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 99\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMPDIR", t.TempDir())
}

// Fake guest runners do not parse keys, but reuse still requires the private
// canonical file that a successful warmup would have published.
func prepareBlacksmithGuestKey(t *testing.T, id string) {
	t.Helper()
	path, err := core.PrepareStoredTestboxKeyPath(id)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.SecureCreatedLeaseSSHFile(f); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// Intercept only capability discovery and the bounded download argv. Never run
// either native binary, and preserve the exclusively precreated destination.
func testBlacksmithArtifactTransfer(req LocalCommandRequest) (bool, LocalCommandResult, error) {
	if req.Name == "blacksmith" && len(req.Args) >= 3 && req.Args[0] == "testbox" && req.Args[1] == "download" && req.Args[2] == "--help" {
		for i := 3; i < len(req.Args); i += 2 {
			if i+1 >= len(req.Args) || (req.Args[i] != "--api-url" && req.Args[i] != "--org") {
				return true, LocalCommandResult{}, errors.New("unexpected download capability arguments")
			}
		}
		return true, LocalCommandResult{Stdout: "testbox download --id <id> [--ssh-private-key <path>] <remote> <local>\n"}, nil
	}
	if req.Name != "/bin/sh" || len(req.Args) < 3 || req.Args[2] != "blacksmith-artifact-download" {
		return false, LocalCommandResult{}, nil
	}
	if len(req.Args) < 11 || req.Args[0] != "-c" {
		return true, LocalCommandResult{}, errors.New("invalid bounded download invocation")
	}
	args := req.Args[5:]
	if len(args) >= 2 && args[0] == "--org" {
		args = args[2:]
	}
	if len(args) < 6 || args[0] != "testbox" || args[1] != "download" || args[2] != "--id" || args[3] == "" {
		return true, LocalCommandResult{}, errors.New("unexpected native download arguments")
	}
	for i := 4; i < len(args)-2; i += 2 {
		if i+1 >= len(args)-2 || (args[i] != "--ssh-private-key" && args[i] != "--api-url" && args[i] != "--org") {
			return true, LocalCommandResult{}, errors.New("unexpected native download option")
		}
	}
	remote, destination := args[len(args)-2], args[len(args)-1]
	stage := filepath.Dir(destination)
	if !regexp.MustCompile(`^\.crabbox/blacksmith-artifact-[0-9a-f]{64}/archive\.tgz$`).MatchString(remote) || !filepath.IsAbs(destination) || destination != filepath.Join(stage, "archive.tgz") || stage == req.Dir || !strings.HasPrefix(filepath.Base(stage), "crabbox-artifact-download-") {
		return true, LocalCommandResult{}, errors.New("unexpected native artifact source or destination")
	}
	stageInfo, err := os.Lstat(stage)
	if err != nil || !stageInfo.IsDir() || stageInfo.Mode().Perm() != 0o700 {
		return true, LocalCommandResult{}, errors.New("native artifact stage is not a private real directory")
	}
	if !filepath.IsAbs(req.Dir) || !containsString(req.Env, "PATH="+filepath.Join(stage, "bin")+":/usr/bin:/bin") {
		return true, LocalCommandResult{}, errors.New("native artifact command lost original cwd or private helper resolution")
	}
	cwd, err := filepath.EvalSymlinks(req.Dir)
	if err != nil {
		return true, LocalCommandResult{}, err
	}
	source := filepath.Join(cwd, filepath.FromSlash(remote))
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil || resolved != source {
		return true, LocalCommandResult{}, errors.New("synthetic artifact source is missing or redirected")
	}
	info, err := os.Lstat(destination)
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 || info.Mode().Perm() != 0o600 {
		return true, LocalCommandResult{}, errors.New("native download destination was not privately precreated")
	}
	in, err := os.Open(source)
	if err != nil {
		return true, LocalCommandResult{}, err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return true, LocalCommandResult{}, err
	}
	_, copyErr := io.Copy(out, io.LimitReader(in, core.DelegatedRunArtifactDefaultMaxBytes+1))
	return true, LocalCommandResult{}, errors.Join(copyErr, out.Close())
}

func artifactTestRunner(t *testing.T, fn ownershipRunner) ownershipRunner {
	t.Helper()
	return func(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
		if handled, result, err := testBlacksmithArtifactTransfer(req); handled {
			return result, err
		}
		return fn(ctx, req)
	}
}

func testBlacksmithArtifactMetadata(t *testing.T, req LocalCommandRequest, archive []byte) (metadata, archivePath string) {
	t.Helper()
	start, _, _ := testBlacksmithReceiptFrames(t, syntheticBlacksmithCommand(t, req), 0)
	prefix := strings.TrimSuffix(start, "start\x1f")
	nonce := strings.TrimSuffix(strings.TrimPrefix(prefix, "\x1eCRABBOX_BS_"), ":")
	archivePath = filepath.Join(req.Dir, ".crabbox", "blacksmith-artifact-"+nonce, "archive.tgz")
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(archive)
	hexDigest := hex.EncodeToString(digest[:])
	return fmt.Sprintf("%sbytes:%d\x1f%sdigest0:%s\x1f%sdigest1:%s\x1f", prefix, len(archive), prefix, hexDigest[:32], prefix, hexDigest[32:]), archivePath
}

func assertArtifactTransferCalls(t *testing.T, calls [][]string, wantDownloads int) {
	t.Helper()
	help, download := 0, 0
	for _, args := range calls {
		if len(args) >= 3 && args[0] == "testbox" && args[1] == "download" && args[2] == "--help" {
			help++
		}
		if len(args) >= 3 && args[0] == "-c" && args[2] == "blacksmith-artifact-download" {
			download++
		}
	}
	if help != 1 || download != wantDownloads {
		t.Fatalf("artifact capability calls=%d, download calls=%d; want 1 and %d", help, download, wantDownloads)
	}
}

func assertNoBlacksmithArtifactPublication(t *testing.T, repo, lease string) {
	t.Helper()
	parent := filepath.Dir(core.LocalRunArtifactPath(repo, "", lease, "blacksmith-artifacts.tgz"))
	err := filepath.WalkDir(parent, func(path string, entry os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Name() == "blacksmith-artifacts.tgz" {
			return errors.New("failed collection published an archive")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
