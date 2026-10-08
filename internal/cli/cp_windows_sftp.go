package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/openclaw/crabbox/internal/runner/runnerfs"
)

const (
	windowsSFTPTimeout          = 9 * time.Minute
	windowsSFTPCleanup          = 30 * time.Second
	windowsSFTPInheritedFDState = "c28fc6f98a2c44abbbd89d6a3037d0d9_POSIX_FD_STATE"
)

type windowsSFTPMetadata struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type windowsSFTPRequest struct {
	local, remote string
	upload        bool
}

type windowsSFTPTransport interface {
	guest(context.Context, string) ([]byte, error)
	copy(context.Context, string, string, bool) error
}

func copyWindowsSFTP(parent context.Context, target SSHTarget, src, dst string, followLink bool, _, _ anyWriter) (err error) {
	ctx, cancel := context.WithTimeout(parent, windowsSFTPTimeout)
	defer cancel()
	req, err := parseWindowsSFTPRequest(src, dst, followLink)
	if err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return Exit(2, "native Windows file cp requires a Windows operator with native OpenSSH 9 or newer")
	}
	if target.DisableHostKeyChecking || target.Key == "" {
		return Exit(2, "native Windows file cp requires a resolved SSH key and strict host-key checking")
	}
	client, err := newWindowsSFTPClient(ctx, target)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, client.session.Close()) }()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	return copyWindowsSFTPWithTransport(ctx, req, client, hex.EncodeToString(random[:]))
}

func parseWindowsSFTPRequest(src, dst string, followLink bool) (windowsSFTPRequest, error) {
	if followLink {
		return windowsSFTPRequest{}, Exit(2, "native Windows file cp does not support -L or links")
	}
	fromRemote, from := sandboxCopyPath(src)
	toRemote, to := sandboxCopyPath(dst)
	if fromRemote == toRemote || from == "" || to == "" {
		return windowsSFTPRequest{}, Exit(2, "native Windows file cp requires exactly one SANDBOX path")
	}
	local, remote := from, to
	if fromRemote {
		local, remote = to, from
	}
	local, err := filepath.Abs(local)
	if err != nil {
		return windowsSFTPRequest{}, err
	}
	if strings.ContainsAny(local, "\x00\r\n") {
		return windowsSFTPRequest{}, Exit(2, "local copy path contains control characters")
	}
	remote, err = normalizeWindowsSFTPPath(remote)
	if err != nil {
		return windowsSFTPRequest{}, err
	}
	return windowsSFTPRequest{local: local, remote: remote, upload: !fromRemote}, nil
}

func normalizeWindowsSFTPPath(value string) (string, error) {
	normalized := strings.ReplaceAll(value, "\\", "/")
	if len(normalized) < 4 || !isASCIIAlpha(normalized[0]) || normalized[1:3] != ":/" {
		return "", Exit(2, "Windows cp needs a drive-qualified absolute guest filename")
	}
	relative := normalized[3:]
	if !runnerfs.ValidWindowsPathComponents(relative) || strings.ContainsAny(relative, "*?[]\"<>|") {
		return "", Exit(2, "Windows cp does not support wildcard, device or special paths")
	}
	for _, part := range strings.Split(relative, "/") {
		if part == "" || part == "." || part == ".." {
			return "", Exit(2, "Windows cp needs an explicit file path without empty, . or .. components")
		}
		for _, c := range part {
			if c < 32 || c == 127 {
				return "", Exit(2, "Windows copy path contains control characters")
			}
		}
	}
	if runtime.GOOS == "windows" && !filepath.IsLocal(strings.ReplaceAll(relative, "/", "\\")) {
		return "", Exit(2, "Windows copy path is not a regular filesystem path")
	}
	return strings.ReplaceAll(normalized, "/", "\\"), nil
}

func isASCIIAlpha(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func windowsSFTPWirePath(native string) string {
	return "/" + strings.ReplaceAll(native, "\\", "/")
}

func windowsSFTPRegularFile(name string) (os.FileInfo, error) {
	info, err := fileSnapshotPathInfo(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, Exit(2, "Windows cp supports regular files, not directories, links or devices")
	}
	return info, nil
}

func windowsSFTPHash(ctx context.Context, name string) (windowsSFTPMetadata, error) {
	info, err := windowsSFTPRegularFile(name)
	if err != nil {
		return windowsSFTPMetadata{}, err
	}
	f, err := os.Open(name)
	if err != nil {
		return windowsSFTPMetadata{}, err
	}
	defer f.Close()
	opened, err := fileSnapshotHandleInfo(f)
	if err != nil {
		return windowsSFTPMetadata{}, err
	}
	if !sameFileSnapshotIdentity(info, opened) {
		return windowsSFTPMetadata{}, errors.New("copy file changed while opening it")
	}
	h := sha256.New()
	n, err := io.Copy(h, &contextRunDownloadReader{ctx: ctx, reader: f})
	if err != nil {
		return windowsSFTPMetadata{}, err
	}
	return windowsSFTPMetadata{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func windowsSFTPLocalStage(parent string) (string, error) {
	root, err := os.MkdirTemp(parent, ".crabbox-cp-windows-")
	if err != nil {
		return "", err
	}
	if err := secureSSHTransportPath(root, true); err != nil {
		return "", errors.Join(err, os.Remove(root))
	}
	return root, nil
}

func snapshotWindowsSFTPSource(ctx context.Context, source, destination string) (_ windowsSFTPMetadata, err error) {
	info, err := windowsSFTPRegularFile(source)
	if err != nil {
		return windowsSFTPMetadata{}, err
	}
	if info.Size() > runnerfs.DefaultArchiveLimits().MaxFileBytes {
		return windowsSFTPMetadata{}, Exit(2, "Windows copy file exceeds the 64 GiB limit")
	}
	if err := checkRunDownloadDiskReserve(filepath.Dir(destination), info.Size(), defaultRunDownloadLimits.DiskReserveBytes, availableRunDownloadBytes); err != nil {
		return windowsSFTPMetadata{}, err
	}
	input, err := os.Open(source)
	if err != nil {
		return windowsSFTPMetadata{}, err
	}
	defer func() { err = errors.Join(err, input.Close()) }()
	current, err := fileSnapshotHandleInfo(input)
	if err != nil || !sameFileSnapshotIdentity(info, current) {
		return windowsSFTPMetadata{}, errors.Join(errors.New("copy source changed while opening it"), err)
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return windowsSFTPMetadata{}, err
	}
	defer func() { err = errors.Join(err, output.Close()) }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(output, h), &contextRunDownloadReader{ctx: ctx, reader: io.LimitReader(input, info.Size()+1)})
	if err != nil || n != info.Size() {
		return windowsSFTPMetadata{}, errors.Join(errors.New("copy source changed size during snapshot"), err)
	}
	if err := output.Sync(); err != nil {
		return windowsSFTPMetadata{}, err
	}
	return windowsSFTPMetadata{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func copyWindowsSFTPWithTransport(ctx context.Context, req windowsSFTPRequest, transport windowsSFTPTransport, nonce string) (err error) {
	if req.upload {
		return uploadWindowsSFTP(ctx, req, transport, nonce)
	}
	return downloadWindowsSFTP(ctx, req, transport)
}

func uploadWindowsSFTP(ctx context.Context, req windowsSFTPRequest, transport windowsSFTPTransport, nonce string) (err error) {
	random, err := hex.DecodeString(nonce)
	if err != nil || len(random) != 16 {
		return errors.New("invalid copy staging identity")
	}
	root, err := windowsSFTPLocalStage("")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(root)) }()
	local := filepath.Join(root, "payload")
	metadata, err := snapshotWindowsSFTPSource(ctx, req.local, local)
	if err != nil {
		return err
	}
	parent := req.remote[:strings.LastIndex(req.remote, "\\")+1]
	stage := parent + ".crabbox-cp-" + nonce
	published := false
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), windowsSFTPCleanup)
		defer cancel()
		if _, cleanupErr := transport.guest(cleanupCtx, windowsSFTPCleanupScript(stage, nonce)); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("Windows copy staging cleanup failed (published=%t, stage=%s): %w", published, stage, cleanupErr))
		}
	}()
	if _, err := transport.guest(ctx, windowsSFTPPrepareScript(req.remote, stage, nonce)); err != nil {
		return fmt.Errorf("prepare Windows copy staging: %w", err)
	}
	if err := transport.copy(ctx, local, windowsSFTPWirePath(stage+`\payload`), true); err != nil {
		return fmt.Errorf("SFTP upload failed: %w", err)
	}
	if _, err := transport.guest(ctx, windowsSFTPPublishScript(req.remote, stage, nonce, metadata)); err != nil {
		return fmt.Errorf("verify/publish Windows copy: %w", err)
	}
	published = true
	return nil
}

func downloadWindowsSFTP(ctx context.Context, req windowsSFTPRequest, transport windowsSFTPTransport) (err error) {
	if _, err := os.Lstat(req.local); err == nil {
		if _, err := windowsSFTPRegularFile(req.local); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(req.local)
	if err := preflightLocalOutputPath("Windows copy destination", req.local, false, true); err != nil {
		return err
	}
	data, err := transport.guest(ctx, windowsSFTPMetadataScript(req.remote))
	if err != nil {
		return fmt.Errorf("inspect Windows evidence: %w", err)
	}
	var expected windowsSFTPMetadata
	if err := json.Unmarshal(data, &expected); err != nil {
		return fmt.Errorf("invalid Windows evidence metadata: %w", err)
	}
	digest, err := hex.DecodeString(expected.SHA256)
	if err != nil || len(digest) != sha256.Size || expected.Size < 0 || expected.Size > runnerfs.DefaultArchiveLimits().MaxFileBytes {
		return errors.New("Windows evidence metadata has an invalid size or SHA-256")
	}
	if err := checkRunDownloadDiskReserve(parent, expected.Size, defaultRunDownloadLimits.DiskReserveBytes, availableRunDownloadBytes); err != nil {
		return err
	}
	root, err := windowsSFTPLocalStage(parent)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(root)) }()
	local := filepath.Join(root, "payload")
	if err := transport.copy(ctx, local, windowsSFTPWirePath(req.remote), false); err != nil {
		return fmt.Errorf("SFTP evidence download failed: %w", err)
	}
	info, err := windowsSFTPRegularFile(local)
	if err != nil {
		return err
	}
	if info.Size() != expected.Size {
		return errors.New("downloaded Windows evidence has an unexpected size")
	}
	actual, err := windowsSFTPHash(ctx, local)
	if err != nil {
		return err
	}
	if actual.Size != expected.Size || !strings.EqualFold(actual.SHA256, expected.SHA256) {
		return errors.New("downloaded Windows evidence failed size/SHA-256 verification")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return publishWindowsSFTPFile(ctx, local, req.local)
}

type windowsSFTPClient struct {
	session  *sshTransportSession
	scp, ssh string
	target   SSHTarget
}

func newWindowsSFTPClient(ctx context.Context, target SSHTarget) (*windowsSFTPClient, error) {
	ssh, err := exec.LookPath("ssh.exe")
	if err != nil {
		return nil, Exit(2, "Windows file cp requires native OpenSSH 9 or newer on PATH: %v", err)
	}
	ssh, err = filepath.Abs(ssh)
	if err != nil {
		return nil, err
	}
	scp := filepath.Join(filepath.Dir(ssh), "scp.exe")
	sshVersion, err := windowsSFTPFileVersion(ssh)
	if err != nil {
		return nil, Exit(2, "inspect native ssh.exe version: %v", err)
	}
	scpVersion, err := windowsSFTPFileVersion(scp)
	if err != nil {
		return nil, Exit(2, "inspect paired native scp.exe version: %v", err)
	}
	if sshVersion[0] < 9 || sshVersion != scpVersion {
		return nil, Exit(2, "Windows file cp requires matching native scp.exe/ssh.exe version 9 or newer; put that installation before other SSH tools on PATH")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := runWindowsSFTPProcess(probeCtx, target, ssh, "-V")
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(out, []byte("OpenSSH_for_Windows_")) {
		return nil, Exit(2, "Windows file cp requires native Win32 OpenSSH, not a WSL/MSYS client")
	}
	session, err := newSSHTransportSession(ctx, target, false)
	if err != nil {
		return nil, err
	}
	return &windowsSFTPClient{session: session, scp: scp, ssh: ssh, target: target}, nil
}

func windowsSFTPArgs(session *sshTransportSession, ssh, local, remote string, upload bool) []string {
	source, destination := local, session.host()+":"+remote
	if !upload {
		source, destination = destination, source
	}
	args := append(session.commandPrefixWithOptions("10", "1"), "-S", ssh, "-B", "-q",
		"-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "NumberOfPasswordPrompts=0", "-o", "ForwardAgent=no")
	return append(args, "--", source, destination)
}

func (client *windowsSFTPClient) guest(ctx context.Context, script string) ([]byte, error) {
	args := append(client.session.commandPrefixWithOptions("10", "1"), "-T", "-o", "BatchMode=yes",
		client.session.host(), PowershellCommand(script))
	return runWindowsSFTPProcess(ctx, client.target, client.ssh, args...)
}

func (client *windowsSFTPClient) copy(ctx context.Context, local, remote string, upload bool) error {
	_, err := runWindowsSFTPProcess(ctx, client.target, client.scp, windowsSFTPArgs(client.session, client.ssh, local, remote, upload)...)
	return err
}

type windowsSFTPOutput struct {
	buffer   bytes.Buffer
	overflow context.CancelCauseFunc
}

func (out *windowsSFTPOutput) Write(data []byte) (int, error) {
	if len(data) > 64*1024-out.buffer.Len() {
		err := errors.New("Windows copy command exceeded its metadata output limit")
		out.overflow(err)
		return 0, err
	}
	return out.buffer.Write(data)
}

func runWindowsSFTPProcess(ctx context.Context, target SSHTarget, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	handle := pondMeshExecCommand(ctx, target, name, args...)
	if runtime.GOOS == "windows" {
		env := handle.cmd.Env
		if env == nil {
			env = os.Environ()
		}
		// Go supplies synchronous file/pipe handles. An inherited Win32
		// OpenSSH socket-mode hint would make the child misread those handles.
		handle.cmd.Env = append(childEnvironmentWithout(env, "OPENSSH_STDIO_MODE", windowsSFTPInheritedFDState), "OPENSSH_STDIO_MODE=nonsock_sync")
	}
	handle.cmd.WaitDelay = sshCommandWaitDelay
	stdout := windowsSFTPOutput{overflow: cancel}
	stderr := newSynchronizedBuffer(4096)
	handle.cmd.Stdout, handle.cmd.Stderr = &stdout, &stderr
	if err := handle.Start(); err != nil {
		return nil, err
	}
	if err := handle.Wait(); err != nil {
		if cause := context.Cause(ctx); cause != nil && handle.WasTerminatedByOurCancel() {
			return nil, fmt.Errorf("%w: %s", cause, redactSSHTransportDiagnostic(target, stderr.String()))
		}
		return nil, fmt.Errorf("%s: %w: %s", filepath.Base(name), err, redactSSHTransportDiagnostic(target, stderr.String()))
	}
	// ssh -V writes its version to stderr, while guest metadata belongs only
	// to stdout. Never merge guest diagnostics into the metadata stream.
	if len(args) == 1 && args[0] == "-V" {
		return []byte(stderr.String()), nil
	}
	return stdout.buffer.Bytes(), nil
}
