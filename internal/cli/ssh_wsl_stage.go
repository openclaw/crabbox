package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pkg/sftp"
)

const (
	wslStageRoot             = ".crabbox/wsl-stage"
	wslStageHeaderSize       = 80
	wslStageMaxHelper        = 32 << 10
	wslStageMaxCommand       = 64 << 20
	wslStageMaxSize          = int64(1 << 40)
	wslStageLauncherMax      = 8191
	wslStagePrepareTimeout   = 47 * time.Second
	wslStageTransferFloor    = 2 * time.Minute
	wslStageIdleTimeout      = 15 * time.Second
	wslStageCleanupTimeout   = 15 * time.Second
	wslStageCancelCleanup    = 5 * time.Second
	wslStageSignalGrace      = 5 * time.Second
	wslStagePreparationReady = "CBX_WSL_STAGE_READY_V1"
)

//go:embed scripts/wsl-stage.ps1
var wslStageVerifier string

//go:embed scripts/wsl-owner.ps1
var wslStageWindowsOwner string

//go:embed scripts/wsl-supervisor.sh
var wslStageLinuxSupervisor string

const wslStageHelperBootstrap = `case $2 in
0) ;;
3) p=$(dd bs=1 count=3 2>/dev/null); [ "$p" = "$(printf '\357\273\277')" ] || exit 74;;
*) exit 74;;
esac
h=$(dd bs=1 count="$1" 2>/dev/null; printf .)
h=${h%.}
[ "$(printf %s "$h" | wc -c)" -eq "$1" ] || exit 74
export CBX_HELPER="$h"
mask=$(umask)
shift 2
exec bash -c "$h" sh "$@" "$mask"`

var (
	errWSLSFTPUnavailable           = errors.New("WSL2 staged transport requires the target OpenSSH SFTP subsystem")
	wslSFTPRejected                 = regexp.MustCompile(`(?m)^subsystem request failed on channel [0-9]+\r?$`)
	wslStageEntropy       io.Reader = rand.Reader
	newWSLStageSession              = newSSHTransportSession
	openWSLStageSFTP                = openWSLSFTP
	runWSLStageCommand              = runWSLStageSSH
	publishWSLStage                 = (*sftp.Client).Rename
)

type wslStageShell string

const (
	wslStageCMD        wslStageShell = "cmd"
	wslStagePowerShell wslStageShell = "powershell"
)

type retryableWSLStageError struct{ error }

func (e retryableWSLStageError) Unwrap() error { return e.error }

func (s wslStageShell) valid() bool {
	return s == wslStageCMD || s == wslStagePowerShell
}

type wslStageSpool struct {
	input  *replayableSSHInput
	size   int64
	digest [sha256.Size]byte
}

func newWSLStageSpool(command string, payload []byte, source io.ReadSeeker, payloadSize int64, wait time.Duration) (*wslStageSpool, error) {
	owner := strings.NewReplacer(
		"@BOOTSTRAP@", psQuote(wslStageHelperBootstrap),
		"@STARTUP@", fmt.Sprint(wslStageIdleTimeout.Milliseconds()),
	).Replace(wslStageWindowsOwner)
	if len(owner) == 0 || len(owner) > wslStageMaxHelper ||
		len(wslStageLinuxSupervisor) == 0 || len(wslStageLinuxSupervisor) > wslStageMaxHelper ||
		!utf8.ValidString(owner) || !utf8.ValidString(wslStageLinuxSupervisor) ||
		strings.ContainsRune(owner, 0) || strings.ContainsRune(wslStageLinuxSupervisor, 0) ||
		len(command) > wslStageMaxCommand || payloadSize < 0 || payloadSize > wslStageMaxSize || wait < 0 {
		return nil, errors.New("WSL2 envelope exceeds bounded descriptor limits")
	}
	if source == nil {
		source = bytes.NewReader(payload)
	}
	if size, err := source.Seek(0, io.SeekEnd); err != nil || size != payloadSize {
		return nil, errors.Join(err, errors.New("WSL2 stage input size changed"))
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	var header [wslStageHeaderSize]byte
	copy(header[:8], "CBXWSL3!")
	binary.LittleEndian.PutUint32(header[8:12], uint32(len(owner)))
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(wslStageLinuxSupervisor)))
	binary.LittleEndian.PutUint64(header[16:24], uint64(len(command)))
	binary.LittleEndian.PutUint64(header[24:32], uint64(payloadSize))
	binary.LittleEndian.PutUint64(header[32:40], uint64(wait.Milliseconds()))
	binary.LittleEndian.PutUint32(header[40:44], uint32(wslStageIdleTimeout.Milliseconds()))
	binary.LittleEndian.PutUint32(header[44:48], uint32(wslStageSignalGrace.Milliseconds()))
	if _, err := io.ReadFull(wslStageEntropy, header[48:]); err != nil {
		return nil, errors.New("generate private WSL2 envelope blinding failed")
	}
	prefix := append(header[:], []byte(owner+wslStageLinuxSupervisor+command)...)
	total := int64(len(prefix)) + payloadSize
	if total > wslStageMaxSize {
		return nil, errors.New("WSL2 envelope is too large")
	}
	input, err := newReplayableSSHInputStream(prefix, source, payloadSize)
	if err != nil {
		return nil, err
	}
	spool := &wslStageSpool{input: input, size: total}
	spool.digest, err = spool.prefixDigest(total)
	if err != nil {
		return nil, errors.Join(err, spool.close())
	}
	return spool, nil
}

func (s *wslStageSpool) close() error {
	if s == nil || s.input == nil {
		return nil
	}
	return s.input.close()
}

func (s *wslStageSpool) prefixDigest(size int64) (digest [sha256.Size]byte, err error) {
	return s.prefixDigestContext(context.Background(), size)
}

func (s *wslStageSpool) prefixDigestContext(ctx context.Context, size int64) (digest [sha256.Size]byte, err error) {
	if size < 0 || size > s.size {
		return digest, errors.New("invalid WSL2 stage prefix length")
	}
	if cause := context.Cause(ctx); cause != nil {
		return digest, cause
	}
	reader, err := s.input.reset()
	if err != nil {
		return digest, err
	}
	hash := sha256.New()
	buffer := make([]byte, 32<<10)
	for remaining := size; remaining > 0; {
		if cause := context.Cause(ctx); cause != nil {
			return digest, cause
		}
		count := min(int64(len(buffer)), remaining)
		if _, err := io.ReadFull(reader, buffer[:count]); err != nil {
			return digest, err
		}
		if _, err := hash.Write(buffer[:count]); err != nil {
			return digest, err
		}
		remaining -= count
	}
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

func (s *wslStageSpool) run(ctx context.Context, target SSHTarget, connectTimeout, attempts string, stdout, stderr io.Writer) (err error) {
	target.NoControlMaster = true
	target.FallbackPorts = []string{}
	session, err := newWSLStageSession(ctx, target, false)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := session.Close(); closeErr != nil {
			err = exit(7, "remove private WSL2 route: %v", errors.Join(err, closeErr))
		}
	}()

	nonce, err := randomHex(16)
	if err != nil {
		return err
	}
	prepareCtx, cancelPrepare := context.WithTimeout(ctx, wslStagePrepareTimeout)
	shell, err := prepareWSLStageRoute(prepareCtx, target, session, nonce, connectTimeout, attempts)
	cancelPrepare()
	if err != nil {
		cleanupErr := discardWSLStage(ctx, target, session, "."+nonce+".proof", int64(len(nonce)), sha256.Sum256([]byte(nonce)), connectTimeout)
		if cleanupErr != nil {
			return exit(7, "WSL2 route preparation and exact cleanup failed: %v", errors.Join(err, cleanupErr))
		}
		if shouldRetrySSHPort(err) {
			return err
		}
		return exit(7, "prepare WSL2 staged route: %v", err)
	}

	uploadDeadline, cancelDeadline := context.WithTimeout(ctx, wslStageBudget(s.size))
	uploadCtx, cancelUpload := context.WithCancelCause(uploadDeadline)
	published, err := s.upload(uploadCtx, cancelUpload, target, session, nonce, connectTimeout, attempts)
	cancelUpload(nil)
	cancelDeadline()
	if err != nil {
		if context.Cause(ctx) != nil {
			return exit(7, "WSL2 stage canceled after remote mutation: %v", errors.Join(err, context.Cause(ctx)))
		}
		return err
	}
	if !published {
		return exit(7, "WSL2 stage publication was not confirmed")
	}
	launcher := wslStageFileCommand(nonce, nonce+".ready", s.size, s.digest, false, shell)
	if launcher == "" || len(launcher) >= wslStageLauncherMax {
		return exit(7, "WSL2 stage launcher exceeds its fixed command budget")
	}
	err = runWSLStageCommand(ctx, target, session, launcher, connectTimeout, attempts, stdout, stderr)
	if err != nil && (shouldRetrySSHPort(err) || context.Cause(ctx) != nil) {
		return exit(7, "WSL2 staged command result is ambiguous: %v", err)
	}
	return err
}

func prepareWSLStageRoute(ctx context.Context, target SSHTarget, session *sshTransportSession, nonce, connectTimeout, attempts string) (wslStageShell, error) {
	var output bytes.Buffer
	err := runWSLStageCommand(ctx, target, session, wslStageRootPreparationCommand(nonce), connectTimeout, attempts, &output, io.Discard)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(output.String())
	if len(fields) != 3 || fields[0] != wslStagePreparationReady || fields[1] != nonce || !wslStageShell(fields[2]).valid() {
		return "", errors.New("private WSL2 stage returned an invalid route proof")
	}
	return wslStageShell(fields[2]), nil
}

func wslStageRootPreparationCommand(nonce string) string {
	script := `$ErrorActionPreference = 'Stop'
try {
  $parentPID = (Get-CimInstance Win32_Process -Filter "ProcessId=$PID").ParentProcessId
  $shell = (Get-Process -Id $parentPID).ProcessName.ToLowerInvariant()
  if ($shell -eq 'pwsh') { $shell = 'powershell' }
  if ($shell -notin 'cmd','powershell') { throw 'unsupported SSH shell' }
  $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User
  $allowed = @($sid.Value,'S-1-5-18','S-1-5-32-544' | Select-Object -Unique)
  function Test-PrivateDirectory($path, $strict = $false) {
    $item = Get-Item -LiteralPath $path -Force
    if (!$item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'invalid stage directory' }
    $acl = [IO.Directory]::GetAccessControl($path)
    if ($acl.GetOwner($sid.GetType()).Value -notin $allowed) { throw 'invalid stage owner' }
    $raw = [Security.AccessControl.RawSecurityDescriptor]::new($acl.GetSecurityDescriptorBinaryForm(),0)
    if ($null -eq $raw.DiscretionaryAcl) { throw 'null stage DACL' }
    $rules = @($acl.GetAccessRules($true,$true,$sid.GetType()))
    foreach ($rule in $rules) {
      if ($rule.AccessControlType -eq 'Allow' -and $rule.IdentityReference.Value -notin $allowed) { throw 'untrusted stage grant' }
    }
    if ($strict) {
      if (!$acl.AreAccessRulesProtected -or !$acl.AreAccessRulesCanonical -or $rules.Count -ne $allowed.Count) { throw 'invalid stage DACL' }
      foreach ($value in $allowed) {
        $grant = @($rules | Where-Object { $_.IdentityReference.Value -eq $value })
        if ($grant.Count -ne 1 -or $grant[0].IsInherited -or $grant[0].AccessControlType -ne 'Allow' -or
            $grant[0].FileSystemRights -ne 'FullControl' -or $grant[0].InheritanceFlags -ne 'ContainerInherit,ObjectInherit' -or
            $grant[0].PropagationFlags -ne 'None') { throw 'invalid stage grant' }
      }
    }
  }
  $parent = Join-Path $HOME '.crabbox'
  $root = Join-Path $parent 'wsl-stage'
  Test-PrivateDirectory $HOME
  foreach ($path in @($parent,$root)) { if (Test-Path -LiteralPath $path) { Test-PrivateDirectory $path } }
  foreach ($path in @($parent,$root)) {
    $acl = [Security.AccessControl.DirectorySecurity]::new()
    $acl.SetOwner($sid)
    $acl.SetAccessRuleProtection($true,$false)
    foreach ($value in $allowed) {
      $principal = [Security.Principal.SecurityIdentifier]::new($value)
      $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new($principal,'FullControl','ContainerInherit,ObjectInherit','None','Allow'))
    }
    if (!(Test-Path -LiteralPath $path)) { [void][IO.Directory]::CreateDirectory($path,$acl) }
    else { try { Test-PrivateDirectory $path $true } catch { [IO.Directory]::SetAccessControl($path,$acl) } }
    Test-PrivateDirectory $path $true
  }
  $proof = [IO.File]::Open((Join-Path $root '.@NONCE@.proof'),'CreateNew','Write','None')
  try { $bytes = [Text.Encoding]::ASCII.GetBytes('@NONCE@'); $proof.Write($bytes,0,$bytes.Length) } finally { $proof.Dispose() }
  [Console]::Out.WriteLine('@READY@ @NONCE@ ' + $shell)
} catch {
  [Console]::Error.WriteLine('WSL2 private stage preparation failed')
  exit 74
}`
	script = strings.NewReplacer("@NONCE@", nonce, "@READY@", wslStagePreparationReady).Replace(script)
	return wslStagePowerShellCommand(script, wslStageCMD)
}

func (s *wslStageSpool) upload(ctx context.Context, cancel context.CancelCauseFunc, target SSHTarget, session *sshTransportSession, nonce, connectTimeout, attempts string) (bool, error) {
	client, process, initErr := openWSLStageSFTP(ctx, target, session, connectTimeout, attempts)
	if initErr != nil {
		cleanupErr := discardWSLStage(ctx, target, session, "."+nonce+".proof", int64(len(nonce)), sha256.Sum256([]byte(nonce)), connectTimeout)
		if cleanupErr != nil {
			return false, exit(7, "WSL2 SFTP startup and route cleanup failed: %v", errors.Join(initErr, cleanupErr))
		}
		return false, initErr
	}

	closeClient := func() error { return errors.Join(client.Close(), process.wait()) }
	proof := wslStageRoot + "/." + nonce + ".proof"
	if err := verifyWSLStageProof(client, proof, nonce); err != nil {
		closeErr := closeClient()
		cleanupErr := discardWSLStage(ctx, target, session, "."+nonce+".proof", int64(len(nonce)), sha256.Sum256([]byte(nonce)), connectTimeout)
		if cleanupErr != nil || !isWSLStageConnectionLoss(errors.Join(err, closeErr)) {
			return false, exit(7, "validate private WSL2 route proof: %v", errors.Join(err, closeErr, cleanupErr))
		}
		return false, retryWSLStageTransport(errors.Join(err, closeErr))
	}
	if err := client.Remove(proof); err != nil {
		closeErr := closeClient()
		cleanupErr := discardWSLStage(ctx, target, session, "."+nonce+".proof", int64(len(nonce)), sha256.Sum256([]byte(nonce)), connectTimeout)
		result := errors.Join(err, closeErr)
		if cleanupErr == nil && isWSLStageConnectionLoss(result) {
			return false, retryWSLStageTransport(result)
		}
		return false, exit(7, "remove private WSL2 route proof: %v", errors.Join(result, cleanupErr))
	}
	if _, err := client.Lstat(proof); !errors.Is(err, os.ErrNotExist) {
		closeErr := closeClient()
		cleanupErr := discardWSLStage(ctx, target, session, "."+nonce+".proof", int64(len(nonce)), sha256.Sum256([]byte(nonce)), connectTimeout)
		result := errors.Join(err, closeErr)
		if cleanupErr == nil && isWSLStageConnectionLoss(result) {
			return false, retryWSLStageTransport(result)
		}
		return false, exit(7, "private WSL2 route proof removal is ambiguous: %v", errors.Join(result, cleanupErr))
	}

	part := wslStageRoot + "/" + nonce + ".part"
	ready := wslStageRoot + "/" + nonce + ".ready"
	remote, err := client.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		closeErr := closeClient()
		return false, exit(7, "WSL2 stage exclusive creation is ambiguous: %v", errors.Join(err, closeErr))
	}
	source, err := s.input.reset()
	if err == nil {
		err = copyWSLStage(ctx, remote, source, s.size, wslStageIdleTimeout, cancel)
	}
	err = errors.Join(err, remote.Close(), context.Cause(ctx))
	if err != nil {
		closeErr := closeClient()
		cleanupErr := s.cleanupPart(ctx, target, session, nonce, connectTimeout, attempts)
		if cleanupErr == nil && (context.Cause(ctx) != nil || isWSLStageConnectionLoss(errors.Join(err, closeErr))) {
			return false, retryWSLStageTransport(errors.Join(err, closeErr))
		}
		return false, exit(7, "upload WSL2 stage and exact partial cleanup failed: %v", errors.Join(err, closeErr, cleanupErr))
	}
	info, err := client.Lstat(part)
	if err != nil || !info.Mode().IsRegular() || info.Size() != s.size {
		closeErr := closeClient()
		cleanupErr := s.cleanupPart(ctx, target, session, nonce, connectTimeout, attempts)
		return false, exit(7, "verify WSL2 stage metadata: %v", errors.Join(err, closeErr, cleanupErr))
	}
	if _, err := client.Lstat(ready); err == nil {
		_ = closeClient()
		return false, exit(7, "publish WSL2 stage: nonce collision")
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = closeClient()
		return false, exit(7, "publish WSL2 stage: ready-file state is ambiguous: %v", err)
	}
	if err := publishWSLStage(client, part, ready); err != nil {
		_ = closeClient()
		return false, exit(7, "publish WSL2 stage is ambiguous: %v", err)
	}
	if err := closeClient(); err != nil {
		return false, exit(7, "published WSL2 stage transport close is ambiguous: %v", err)
	}
	return true, nil
}

func verifyWSLStageProof(client *sftp.Client, path, nonce string) error {
	info, err := client.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(nonce)) {
		return errors.Join(err, errors.New("invalid private stage route proof"))
	}
	file, err := client.Open(path)
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, int64(len(nonce)+1)))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return err
	}
	if !bytes.Equal(data, []byte(nonce)) {
		return errors.New("invalid private stage route proof")
	}
	return nil
}

func copyWSLStage(ctx context.Context, dst io.Writer, src io.Reader, remaining int64, idle time.Duration, cancel context.CancelCauseFunc) error {
	timer := time.AfterFunc(idle, func() {
		cancel(errors.New("upload WSL2 stage made no progress"))
	})
	defer timer.Stop()
	buffer := make([]byte, 32<<10)
	for remaining > 0 {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		size := min(int64(len(buffer)), remaining)
		if _, err := io.ReadFull(src, buffer[:size]); err != nil {
			return err
		}
		written, err := dst.Write(buffer[:size])
		if err != nil {
			return err
		}
		if written != int(size) {
			return io.ErrShortWrite
		}
		remaining -= int64(written)
		timer.Reset(idle)
	}
	return nil
}

func wslStageBudget(size int64) time.Duration {
	const step = int64(1 << 20)
	if size <= step {
		return wslStageTransferFloor
	}
	steps := (size + step - 1) / step
	maxExtra := int64((time.Duration(1<<63-1) - wslStageTransferFloor) / wslStageIdleTimeout)
	if steps-1 > maxExtra {
		return time.Duration(1<<63 - 1)
	}
	return wslStageTransferFloor + time.Duration(steps-1)*wslStageIdleTimeout
}

func (s *wslStageSpool) cleanupPart(ctx context.Context, target SSHTarget, session *sshTransportSession, nonce, connectTimeout, attempts string) error {
	cleanupCtx, cancel := wslStageCleanupContext(ctx)
	defer cancel()
	client, process, err := openWSLStageSFTP(cleanupCtx, target, session, connectTimeout, attempts)
	if err != nil {
		return err
	}
	info, statErr := client.Lstat(wslStageRoot + "/" + nonce + ".part")
	closeErr := errors.Join(client.Close(), process.wait())
	if errors.Is(statErr, os.ErrNotExist) {
		return closeErr
	}
	if err := errors.Join(statErr, closeErr); err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > s.size {
		return errors.New("refuse non-regular or oversized WSL2 partial")
	}
	digest, err := s.prefixDigestContext(cleanupCtx, info.Size())
	if err != nil {
		return err
	}
	return discardWSLStageWithinContext(cleanupCtx, target, session, nonce+".part", info.Size(), digest, connectTimeout)
}

func wslStageCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := wslStageCleanupTimeout
	if context.Cause(ctx) != nil {
		timeout = wslStageCancelCleanup
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

func discardWSLStage(ctx context.Context, target SSHTarget, session *sshTransportSession, name string, size int64, digest [sha256.Size]byte, connectTimeout string) error {
	cleanupCtx, cancel := wslStageCleanupContext(ctx)
	defer cancel()
	return discardWSLStageWithinContext(cleanupCtx, target, session, name, size, digest, connectTimeout)
}

func discardWSLStageWithinContext(ctx context.Context, target SSHTarget, session *sshTransportSession, name string, size int64, digest [sha256.Size]byte, connectTimeout string) error {
	nonce := strings.TrimSuffix(strings.TrimPrefix(name, "."), ".proof")
	nonce = strings.TrimSuffix(nonce, ".part")
	command := wslStageFileCommand(nonce, name, size, digest, true, wslStageCMD)
	if command == "" || len(command) >= wslStageLauncherMax {
		return errors.New("invalid WSL2 exact discard command")
	}
	return runWSLStageCommand(ctx, target, session, command, connectTimeout, "1", io.Discard, io.Discard)
}

func wslStageFileCommand(nonce, name string, size int64, digest [sha256.Size]byte, discard bool, shell wslStageShell) string {
	if len(nonce) != 32 || strings.Trim(nonce, "0123456789abcdef") != "" || size < 0 || size > wslStageMaxSize {
		return ""
	}
	if name != nonce+".ready" && name != nonce+".part" && name != "."+nonce+".proof" {
		return ""
	}
	flag := "$false"
	if discard {
		flag = "$true"
	}
	script := strings.NewReplacer(
		"@NONCE@", nonce,
		"@NAME@", name,
		"@SIZE@", fmt.Sprint(size),
		"@DIGEST@", base64.StdEncoding.EncodeToString(digest[:]),
		"@DISCARD@", flag,
	).Replace(wslStageVerifier)
	return wslStagePowerShellCommand(script, shell)
}

func wslStagePowerShellCommand(script string, shell wslStageShell) string {
	if !shell.valid() {
		return ""
	}
	invoke := "& ([ScriptBlock]::Create([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('" +
		base64.StdEncoding.EncodeToString([]byte(script)) + "'))))"
	if shell == wslStageCMD {
		return `powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "` + invoke + `"`
	}
	return invoke
}

type wslSFTPProcess struct {
	handle *pondMeshExecHandle
	ctx    context.Context
	waitFn func() error
	once   sync.Once
	err    error
}

func (p *wslSFTPProcess) wait() error {
	if p == nil {
		return nil
	}
	p.once.Do(func() {
		if p.waitFn != nil {
			p.err = p.waitFn()
			return
		}
		if p.handle == nil {
			return
		}
		p.err = p.handle.Wait()
		if cause := context.Cause(p.ctx); cause != nil && p.handle.WasTerminatedByOurCancel() {
			p.err = cause
		}
	})
	return p.err
}

func openWSLSFTP(ctx context.Context, target SSHTarget, session *sshTransportSession, connectTimeout, attempts string) (*sftp.Client, *wslSFTPProcess, error) {
	args := wslStageSubsystemArgs(session, connectTimeout, attempts)
	handle, ok := pondMeshExecCommand(ctx, target.ChildEnvDenylist, directSSHExecutable(), args...).(*pondMeshExecHandle)
	if !ok {
		return nil, nil, errors.New("start owned WSL2 SFTP process")
	}
	applyTargetChildEnvironment(handle.cmd, target)
	stdout, err := handle.cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stdin, err := handle.cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	var diagnostic bytes.Buffer
	handle.cmd.Stderr = &diagnostic
	if err := handle.Start(); err != nil {
		return nil, nil, err
	}
	process := &wslSFTPProcess{handle: handle, ctx: ctx}
	client, protocolErr := sftp.NewClientPipe(stdout, stdin)
	if protocolErr == nil {
		return client, process, nil
	}
	_ = stdin.Close()
	waitErr := process.wait()
	detail := redactSSHTransportDiagnostic(target, diagnostic.String())
	if exitCode(waitErr) == 255 && wslSFTPRejected.MatchString(detail) {
		return nil, nil, errors.Join(errWSLSFTPUnavailable, protocolErr, waitErr)
	}
	return nil, nil, errors.Join(protocolErr, waitErr)
}

func runWSLStageSSH(ctx context.Context, target SSHTarget, session *sshTransportSession, remote, connectTimeout, attempts string, stdout, stderr io.Writer) error {
	args := wslStageCommandArgs(session, remote, connectTimeout, attempts)
	handle, ok := pondMeshExecCommand(ctx, target.ChildEnvDenylist, directSSHExecutable(), args...).(*pondMeshExecHandle)
	if !ok {
		return errors.New("start owned WSL2 SSH process")
	}
	applyTargetChildEnvironment(handle.cmd, target)
	handle.cmd.Stdout = stdout
	handle.cmd.Stderr = stderr
	if err := handle.Start(); err != nil {
		return err
	}
	err := handle.Wait()
	if cause := context.Cause(ctx); cause != nil && handle.WasTerminatedByOurCancel() {
		return cause
	}
	return err
}

func wslStageSubsystemArgs(session *sshTransportSession, connectTimeout, attempts string) []string {
	return append(session.commandPrefixWithOptions(connectTimeout, attempts),
		"-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no",
		"-s", session.host(), "sftp")
}

func wslStageCommandArgs(session *sshTransportSession, remote, connectTimeout, attempts string) []string {
	return append(session.commandPrefixWithOptions(connectTimeout, attempts), "-n", session.host(), remote)
}

func isWSLStageConnectionLoss(err error) bool {
	var status *sftp.StatusError
	return shouldRetrySSHPort(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) || errors.Is(err, sftp.ErrSSHFxConnectionLost) ||
		errors.Is(err, sftp.ErrSSHFxNoConnection) ||
		errors.As(err, &status) && (status.FxCode() == sftp.ErrSSHFxConnectionLost || status.FxCode() == sftp.ErrSSHFxNoConnection)
}

func retryWSLStageTransport(err error) error {
	return retryableWSLStageError{fmt.Errorf("WSL2 staged route lost after exact cleanup: %w", err)}
}

func IsWSLSFTPUnavailable(err error) bool {
	return errors.Is(err, errWSLSFTPUnavailable)
}

// ProbeWSLSFTP performs only the protocol handshake and reaps its exact SSH child.
func ProbeWSLSFTP(ctx context.Context, target SSHTarget, connectTimeout, attempts string) (err error) {
	target.NoControlMaster = true
	target.FallbackPorts = []string{}
	session, err := newWSLStageSession(ctx, target, false)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	client, process, err := openWSLStageSFTP(ctx, target, session, connectTimeout, attempts)
	if err != nil {
		return err
	}
	return errors.Join(client.Close(), process.wait())
}
