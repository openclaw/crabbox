package cli

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/openclaw/crabbox/internal/runner"
	"github.com/openclaw/crabbox/internal/runner/runnerfs"
	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

// Native Windows shares operation ownership with POSIX installations. Only the
// byte installer and invocation syntax differ; the filesystem protocol does not.
func prepareWindowsFilesystemRuntime(ctx context.Context, target SSHTarget, source runtimeartifact.Source, required runtimeartifact.Requirement) (installed *remoteNativeRuntime, err error) {
	if !isWindowsNativeTarget(target) || required.Capability != runtimeartifact.Filesystem || required.ProtocolVersion != "1" || required.BuildID == "" || source == nil {
		return nil, errors.New("invalid native Windows filesystem installation")
	}
	probeCtx, cancel := context.WithTimeout(ctx, sshTransportPreparationTimeout)
	defer cancel()
	metadata := newSynchronizedBuffer(4097)
	remote := &remoteNativeRuntime{target: target}
	if err := remote.runWindows(probeCtx, `$ErrorActionPreference='Stop'; [Console]::Out.WriteLine([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()); [Console]::Out.WriteLine([IO.Path]::GetTempPath())`, &metadata); err != nil {
		return nil, fmt.Errorf("discover Windows filesystem runtime: %w", err)
	}
	artifactTarget, home, err := parseWindowsFilesystemProbe(metadata.String())
	if err != nil {
		return nil, err
	}
	artifact, err := source.Open(ctx, artifactTarget)
	if err != nil {
		return nil, err
	}
	if artifact == nil {
		return nil, errors.New("runtime source returned no artifact")
	}
	defer func() { err = errors.Join(err, artifact.Close()) }()
	identity := artifact.Identity()
	if identity.Target != artifactTarget || identity.Capability != required.Capability || identity.ProtocolVersion != required.ProtocolVersion || identity.BuildID != required.BuildID {
		return nil, errors.New("runtime source does not fulfill the requested capability identity")
	}
	payload, size, err := windowsFilesystemArchive(ctx, artifact, identity.Size)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, payload.Close(), os.Remove(payload.Name())) }()
	nonce, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	remote.nonce, remote.path, remote.identity = nonce, strings.TrimRight(home, `/\`)+`\crabbox-runtime-`+nonce+`\crabbox.exe`, identity
	budget := wslStageBudget(wslStageTimeout, wslStageIdleTimeout, size)
	installCtx, cancelInstall := context.WithTimeout(ctx, budget)
	defer cancelInstall()
	transport := sshTransportPreparation{command: PowershellCommand(windowsFilesystemInstallScript(remote)), direct: payload}
	if _, err := transport.runOnce(installCtx, target, "2", "1", io.Discard, io.Discard, false); err != nil {
		return nil, errors.Join(fmt.Errorf("upload Windows filesystem runtime: %w", err), remote.close(ctx))
	}
	return remote, nil
}

func parseWindowsFilesystemProbe(output string) (runtimeartifact.Target, string, error) {
	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(output, "\r\n", "\n"), "\n"), "\n")
	if len(lines) != 2 {
		return runtimeartifact.Target{}, "", errors.New("invalid Windows filesystem runtime metadata")
	}
	arch := strings.ToLower(strings.TrimSpace(lines[0]))
	if arch == "x64" {
		arch = "amd64"
	}
	home := lines[1]
	if (arch != "amd64" && arch != "arm64") || len(home) < 3 || home[1] != ':' || (home[2] != '\\' && home[2] != '/') || strings.ContainsAny(home, "\x00\r\n") {
		return runtimeartifact.Target{}, "", errors.New("unsupported Windows filesystem runtime metadata")
	}
	return runtimeartifact.Target{OS: "windows", Arch: arch}, home, nil
}

// tar.exe reads binary SSH stdin directly. The archive contains only our already
// validated executable and is spooled locally to keep memory bounded.
func windowsFilesystemArchive(ctx context.Context, artifact io.Reader, size int64) (file *os.File, total int64, err error) {
	file, err = os.CreateTemp("", "crabbox-filesystem-windows-*.tar")
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, file.Close(), os.Remove(file.Name()))
			file = nil
		}
	}()
	archive := tar.NewWriter(file)
	if err = archive.WriteHeader(&tar.Header{Name: "crabbox.exe", Mode: 0600, Size: size}); err != nil {
		return file, 0, err
	}
	if err = runnerfs.Copy(ctx, archive, artifact); err != nil {
		return file, 0, err
	}
	if err = archive.Close(); err != nil {
		return file, 0, err
	}
	total, err = file.Seek(0, io.SeekCurrent)
	if err == nil {
		_, err = file.Seek(0, io.SeekStart)
	}
	return file, total, err
}

func (r *remoteNativeRuntime) runWindows(ctx context.Context, script string, stdout io.Writer) error {
	transport := sshTransportPreparation{command: PowershellCommand(script)}
	_, err := transport.runOnce(ctx, r.target, "2", "1", stdout, io.Discard, false)
	if err == nil {
		err = context.Cause(ctx)
	}
	return err
}

const windowsFilesystemDigest = `function Get-CrabboxRunnerHash([string]$Name) {
  $file=[IO.File]::OpenRead($Name)
  $hash=[Security.Cryptography.SHA256]::Create()
  try { return [BitConverter]::ToString($hash.ComputeHash($file)).Replace('-','').ToLowerInvariant() }
  finally { $hash.Dispose(); $file.Dispose() }
}`

func windowsFilesystemInstallScript(r *remoteNativeRuntime) string {
	return `$ErrorActionPreference='Stop'
$path=` + psQuote(r.path) + `
$stage=Split-Path -Parent $path
if (Test-Path -LiteralPath $stage) { throw 'runtime staging collision' }
[IO.Directory]::CreateDirectory($stage) | Out-Null
[IO.File]::WriteAllText((Join-Path $stage '.nonce'),` + psQuote(r.nonce) + `)
` + windowsFilesystemDigest + `
& tar.exe -xf - -C $stage crabbox.exe
if ($LASTEXITCODE -ne 0) { throw 'runtime bootstrap transfer failed' }
if ((Get-Item -LiteralPath $path).Length -ne [Int64]` + strconv.FormatInt(r.identity.Size, 10) + `) { throw 'runtime artifact size mismatch' }
if ((Get-CrabboxRunnerHash $path) -ne ` + psQuote(r.identity.SHA256) + `) { throw 'runtime digest mismatch' }
$file=[IO.File]::Open($path,[IO.FileMode]::Open,[IO.FileAccess]::ReadWrite,[IO.FileShare]::None)
try { $file.Flush($true) } finally { $file.Dispose() }
`
}

func windowsFilesystemRemoveScript(r *remoteNativeRuntime) string {
	return `$ErrorActionPreference='Stop'
$stage=Split-Path -Parent ` + psQuote(r.path) + `
if (-not (Test-Path -LiteralPath $stage)) { exit 0 }
$item=Get-Item -LiteralPath $stage -Force
if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'invalid runtime stage' }
$witness=Join-Path $stage '.nonce'
if ([IO.File]::ReadAllText($witness) -cne ` + psQuote(r.nonce) + `) { throw 'runtime ownership mismatch' }
Remove-Item -LiteralPath $stage -Recurse -Force
if (Test-Path -LiteralPath $stage) { throw 'runtime cleanup incomplete' }
`
}

func windowsFilesystemInvokeCommand(r *remoteNativeRuntime, size int64) string {
	return `$ErrorActionPreference='Stop'
` + windowsFilesystemDigest + `
$path=` + psQuote(r.path) + `
if ((Get-CrabboxRunnerHash $path) -ne ` + psQuote(r.identity.SHA256) + `) { throw 'runtime digest mismatch' }
& $path ` + runner.Command + ` serve-base64 --input-bytes ` + strconv.FormatInt(size, 10) + `
exit $LASTEXITCODE`
}
