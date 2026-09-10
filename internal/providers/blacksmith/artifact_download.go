package blacksmith

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

var blacksmithDownloadPath = regexp.MustCompile(`^\.crabbox/blacksmith-artifact-[0-9a-f]{64}/archive\.tgz$`)
var blacksmithDownloadHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// The caller retains the original claim and collection deadline through this
// transfer. A completed native command is required before inspecting its file.
func (b *blacksmithBackend) downloadArtifact(ctx context.Context, repoRoot, leaseID, keyPath, remotePath string, expectedBytes int64, expectedSHA256 string) (data []byte, err error) {
	defer func() {
		err = blacksmithContextError(ctx, err)
		if err != nil {
			data = nil
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := core.ValidateLocalCommandProcessGroupJoin(ctx); err != nil {
		return nil, err
	}
	if !blacksmithDownloadPath.MatchString(remotePath) || !blacksmithDownloadHash.MatchString(expectedSHA256) || expectedBytes <= 0 || expectedBytes > core.DelegatedRunArtifactDefaultMaxBytes {
		return nil, errors.New("invalid native artifact download metadata")
	}
	native, err := blacksmithDownloadExecutable("blacksmith")
	if err != nil {
		return nil, err
	}
	scp, err := blacksmithDownloadExecutable("scp")
	if err != nil {
		return nil, err
	}
	tempParent, err := filepath.Abs(os.TempDir())
	if err != nil {
		return nil, err
	}
	tempParent, err = filepath.EvalSymlinks(tempParent)
	if err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(tempParent, "crabbox-artifact-download-*")
	if err != nil {
		return nil, err
	}
	removeStage := true
	defer func() {
		if removeStage {
			if cleanupErr := os.RemoveAll(stage); cleanupErr != nil {
				data = nil
				err = errors.Join(err, fmt.Errorf("native artifact staging cleanup failed at %s: %w", stage, cleanupErr))
			}
		}
	}()
	stageInfo, err := os.Lstat(stage)
	if err != nil {
		return nil, err
	}
	binDir := filepath.Join(stage, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		return nil, err
	}
	// Keep the installed executable's loading context while controlling native
	// PATH lookup. The trusted installed path must remain stable during transfer.
	dispatcher, err := os.OpenFile(filepath.Join(binDir, "scp"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return nil, err
	}
	_, writeErr := dispatcher.WriteString("#!/bin/sh\nexec " + shellQuote(scp) + " \"$@\"\n")
	if err := errors.Join(writeErr, dispatcher.Close()); err != nil {
		return nil, err
	}
	destination := filepath.Join(stage, "archive.tgz")
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	created, err := file.Stat()
	if err != nil {
		return nil, err
	}
	args := append(blacksmithBaseArgs(b.cfg), "testbox", "download", "--id", leaseID)
	if keyPath != "" {
		args = append(args, "--ssh-private-key", keyPath)
	}
	if b.route != nil {
		args = append(args, "--api-url", b.route.API, "--org", b.route.Org)
	}
	args = append(args, remotePath, destination)
	command, err := blacksmithDownloadLimitArgs(native, args, core.DelegatedRunArtifactDefaultMaxBytes)
	if err != nil {
		return nil, err
	}
	env := make([]string, 0, len(os.Environ())+3)
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if name != "PATH" && name != "POSIXLY_CORRECT" && name != "BLACKSMITH_DISABLE_AUTO_UPDATE" {
			env = append(env, value)
		}
	}
	env = append(env, "PATH="+binDir+string(os.PathListSeparator)+"/usr/bin:/bin", "POSIXLY_CORRECT=1", "BLACKSMITH_DISABLE_AUTO_UPDATE=1")
	helper, err := inspectBlacksmithDownloadExecutable(ctx, scp)
	if err != nil {
		return nil, fmt.Errorf("inspect installed artifact scp helper: %w", err)
	}
	result, runErr := b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name: "/bin/sh", Args: command, Dir: repoRoot, Env: env,
		MaxCapturedOutputBytes: int(blacksmithArtifactDiagnosticCaptureBytes), CancelGracePeriod: time.Second,
		RequireProcessGroupJoin: true,
		OnCleanupPending: func(err error) {
			if b.rt.Stderr != nil {
				fmt.Fprintf(b.rt.Stderr, "blacksmith download cleanup pending: %v\n", err)
			}
		},
	})
	// Observe the selected pathname again, not just its former descriptor. These
	// finite checks detect drift; they cannot atomically pin a live installation.
	after, helperErr := inspectBlacksmithDownloadExecutable(ctx, scp)
	if helperErr == nil && !helper.same(after) {
		helperErr = errors.New("installed artifact scp helper identity or content changed")
	}
	if runErr != nil || result.ExitCode != 0 || ctx.Err() != nil || helperErr != nil {
		// The joined command owner has settled its group. Keep failed transfer
		// bytes as bounded evidence instead of publishing or silently discarding them.
		removeStage = false
		diagnostics, diagnosticErr := retainBlacksmithDownloadDiagnostics(stage, stageInfo, result)
		return nil, fmt.Errorf("native artifact download did not complete cleanly (exit %d); staging retained at %s%s: %w", result.ExitCode, stage, diagnostics, errors.Join(runErr, ctx.Err(), errors.New("artifact withheld"), helperErr, diagnosticErr))
	}
	checkFile := func() error {
		currentStage, err := os.Lstat(stage)
		if err != nil {
			removeStage = false
			return err
		}
		if !currentStage.IsDir() || !os.SameFile(stageInfo, currentStage) {
			removeStage = false
			return fmt.Errorf("native artifact staging directory identity changed; staging retained at %s", stage)
		}
		current, err := os.Lstat(destination)
		if err != nil {
			return err
		}
		opened, err := file.Stat()
		if err != nil {
			return err
		}
		if !current.Mode().IsRegular() || !os.SameFile(created, current) || !os.SameFile(created, opened) || opened.Size() != expectedBytes {
			return errors.New("native artifact destination identity or size changed")
		}
		return nil
	}
	if err := checkFile(); err != nil {
		return nil, err
	}
	data, err = io.ReadAll(io.LimitReader(blacksmithDownloadReader{ctx, file}, expectedBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != expectedBytes {
		return nil, errors.New("native artifact download size mismatch")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != expectedSHA256 {
		return nil, errors.New("native artifact download hash mismatch")
	}
	if err := checkFile(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

func retainBlacksmithDownloadDiagnostics(stage string, owned os.FileInfo, result LocalCommandResult) (metadata string, err error) {
	root, err := os.OpenRoot(stage)
	if err != nil {
		return "", fmt.Errorf("open native diagnostic staging: %w", err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	verifyStage := func() error {
		current, err := os.Lstat(stage)
		if err != nil {
			return err
		}
		opened, err := root.Stat(".")
		if err != nil {
			return err
		}
		if !current.IsDir() || !os.SameFile(owned, current) || !os.SameFile(owned, opened) {
			return errors.New("native diagnostic staging directory identity changed")
		}
		return nil
	}
	if err := verifyStage(); err != nil {
		return "", err
	}
	var summaries strings.Builder
	var failures []error
	for _, stream := range []struct{ name, text string }{{"stdout", result.Stdout}, {"stderr", result.Stderr}} {
		if int64(len(stream.text)) > blacksmithArtifactDiagnosticCaptureBytes {
			failures = append(failures, fmt.Errorf("native %s diagnostics exceed capture limit", stream.name))
			continue
		}
		name := "native-download." + stream.name + ".log"
		file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			failures = append(failures, fmt.Errorf("retain native %s diagnostics: %w", stream.name, err))
			continue
		}
		_, writeErr := file.WriteString(stream.text)
		if err := errors.Join(writeErr, file.Close()); err != nil {
			// Keep partial bytes; never replace the native failure or delete a
			// pathname that may have acquired another owner.
			failures = append(failures, fmt.Errorf("retain native %s diagnostics: %w", stream.name, err))
			continue
		}
		fmt.Fprintf(&summaries, "; %s=%q bytes=%d sha256=%x", stream.name, filepath.Join(stage, name), len(stream.text), sha256.Sum256([]byte(stream.text)))
	}
	if err := verifyStage(); err != nil {
		return "", errors.Join(append(failures, err)...)
	}
	return summaries.String(), errors.Join(failures...)
}

func blacksmithDownloadExecutable(name string) (string, error) {
	resolved, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("resolve native artifact helper %s: %w", name, err)
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

type blacksmithDownloadExecutableFingerprint struct {
	info   os.FileInfo
	digest [sha256.Size]byte
}

func (f blacksmithDownloadExecutableFingerprint) same(other blacksmithDownloadExecutableFingerprint) bool {
	return sameBlacksmithDownloadExecutableInfo(f.info, other.info) && f.digest == other.digest
}

func sameBlacksmithDownloadExecutableInfo(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime() == b.ModTime()
}

func inspectBlacksmithDownloadExecutable(ctx context.Context, path string) (fingerprint blacksmithDownloadExecutableFingerprint, err error) {
	if err := ctx.Err(); err != nil {
		return fingerprint, err
	}
	in, err := openBlacksmithDownloadExecutable(path)
	if err != nil {
		return fingerprint, err
	}
	defer func() { err = errors.Join(err, in.Close()) }()
	info, err := in.Stat()
	if err != nil {
		return fingerprint, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return fingerprint, errors.New("native artifact scp helper must be an unprivileged executable regular file")
	}
	if err := checkBlacksmithDownloadCapabilities(in); err != nil {
		return fingerprint, err
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(blacksmithDownloadReader{ctx, in}, info.Size()))
	if err != nil {
		return fingerprint, err
	}
	after, err := in.Stat()
	if err != nil {
		return fingerprint, err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return fingerprint, err
	}
	if n != info.Size() || !sameBlacksmithDownloadExecutableInfo(info, after) || !sameBlacksmithDownloadExecutableInfo(info, current) {
		return fingerprint, errors.New("installed artifact scp helper changed during inspection")
	}
	if err := checkBlacksmithDownloadCapabilities(in); err != nil {
		return fingerprint, err
	}
	fingerprint.info = info
	copy(fingerprint.digest[:], digest.Sum(nil))
	return fingerprint, ctx.Err()
}

func blacksmithDownloadLimitArgs(binary string, args []string, maxBytes int64) ([]string, error) {
	if maxBytes <= 0 || maxBytes%1024 != 0 {
		return nil, errors.New("invalid native artifact file limit")
	}
	// POSIX sh uses 512-byte blocks. Bash versions disagree in POSIX mode;
	// explicitly select its normal 1024-byte units before lowering child limits.
	const script = `unit=512
if [ -n "${BASH_VERSION-}" ]; then
  set +o posix || exit 7
  unit=1024
fi
limit=$(($1 / unit))
shift
hard=$(ulimit -H -f) || exit 7
case "$hard" in
  unlimited) ;;
  ''|*[!0-9]*) exit 7 ;;
  *) if [ "$hard" -lt "$limit" ]; then limit=$hard; fi ;;
esac
ulimit -S -f "$limit" && ulimit -H -f "$limit" || exit 7
exec "$@"`
	command := []string{"-c", script, "blacksmith-artifact-download", strconv.FormatInt(maxBytes, 10), binary}
	return append(command, args...), nil
}

type blacksmithDownloadReader struct {
	ctx context.Context
	io.Reader
}

func (r blacksmithDownloadReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(data)
}
