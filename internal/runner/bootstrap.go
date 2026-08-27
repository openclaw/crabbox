package runner

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type Runtime struct {
	Target Target
	Home   string
}

func ProbeCommand(windows bool) string {
	if windows {
		return `$ErrorActionPreference='Stop'; [Console]::Out.WriteLine('windows'); [Console]::Out.WriteLine([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()); [Console]::Out.WriteLine($env:USERPROFILE)`
	}
	return `sh -c 'set -eu; uname -s; uname -m; printf "%s\n" "${HOME:?}"'`
}

func ParseProbe(output string) (Runtime, error) {
	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(output, "\r\n", "\n"), "\n"), "\n")
	if len(lines) != 3 {
		return Runtime{}, errors.New("runner probe returned invalid runtime metadata")
	}
	osName := strings.ToLower(strings.TrimSpace(lines[0]))
	arch := strings.ToLower(strings.TrimSpace(lines[1]))
	switch arch {
	case "x86_64", "x64", "amd64":
		arch = "amd64"
	case "aarch64", "arm64":
		arch = "arm64"
	}
	target := Target{OS: osName, Arch: arch}
	if err := target.validate(); err != nil {
		return Runtime{}, err
	}
	home := lines[2]
	if home == "" || strings.ContainsAny(home, "\x00\r\n") {
		return Runtime{}, errors.New("runner probe returned invalid home directory")
	}
	if osName == "windows" {
		if len(home) < 3 || home[1] != ':' || (home[2] != '\\' && home[2] != '/') {
			return Runtime{}, errors.New("runner home is not an absolute Windows path")
		}
	} else if !strings.HasPrefix(home, "/") {
		return Runtime{}, errors.New("runner home is not absolute")
	}
	return Runtime{Target: target, Home: home}, nil
}

func (artifact Artifact) RemotePath(runtime Runtime) (string, error) {
	if artifact.Identity.OS != runtime.Target.OS || artifact.Identity.Arch != runtime.Target.Arch {
		return "", errors.New("runner artifact does not match target")
	}
	digest, err := hex.DecodeString(artifact.SHA256)
	if err != nil || len(digest) != sha256.Size {
		return "", errors.New("invalid runner artifact digest")
	}
	separators := "/"
	if runtime.Target.OS == "windows" {
		separators += "\\"
	}
	name := strings.TrimRight(runtime.Home, separators) + "/.cache/crabbox/runners/" + artifact.SHA256
	if runtime.Target.OS == "windows" {
		name += ".exe"
	}
	return name, nil
}

type Installation struct {
	Command string
	Input   []byte
}

// PrepareInstallation binds the install command to its wire bytes. Windows
// PowerShell cannot reliably consume large SSH stdin streams; native tar reads
// this single trusted executable, never a user-supplied archive.
func PrepareInstallation(runtime Runtime, artifact Artifact) (Installation, error) {
	if err := ValidateArtifact(artifact); err != nil {
		return Installation{}, err
	}
	command, err := installCommand(runtime, artifact)
	if err != nil {
		return Installation{}, err
	}
	input := artifact.Data
	if runtime.Target.OS == "windows" {
		var archive bytes.Buffer
		writer := tar.NewWriter(&archive)
		if err := writer.WriteHeader(&tar.Header{Name: "runner.exe", Mode: 0o600, Size: int64(len(input))}); err != nil {
			return Installation{}, err
		}
		if _, err := writer.Write(input); err != nil {
			return Installation{}, err
		}
		if err := writer.Close(); err != nil {
			return Installation{}, err
		}
		input = archive.Bytes()
	}
	return Installation{Command: command, Input: input}, nil
}

// Bootstrap scripts only install/verify bytes and launch the helper; all user
// file collection, archive validation and publication algorithms live in Go.
func installCommand(runtime Runtime, artifact Artifact) (string, error) {
	name, err := artifact.RemotePath(runtime)
	if err != nil {
		return "", err
	}
	if runtime.Target.OS == "windows" {
		return `$ErrorActionPreference='Stop'
$path=` + powerShellLiteral(name) + `
$dir=Split-Path -Parent $path
[System.IO.Directory]::CreateDirectory($dir) | Out-Null
$stage=Join-Path $dir ([Guid]::NewGuid().ToString('N')+'.install')
if (Test-Path -LiteralPath $stage) { throw 'runner staging collision' }
[System.IO.Directory]::CreateDirectory($stage) | Out-Null
$temp=Join-Path $stage 'runner.exe'
` + windowsDigestFunction + `
try {
  & tar.exe -xf - -C $stage runner.exe
  if ($LASTEXITCODE -ne 0) { throw 'runner bootstrap transfer failed' }
  if ((Get-Item -LiteralPath $temp).Length -ne [Int64]` + strconv.Itoa(len(artifact.Data)) + `) { throw 'runner artifact size mismatch' }
  if ((Get-CrabboxRunnerHash $temp) -ne '` + artifact.SHA256 + `') { throw 'runner digest mismatch' }
  $file=[IO.File]::Open($temp,[IO.FileMode]::Open,[IO.FileAccess]::ReadWrite,[IO.FileShare]::None)
  try { $file.Flush($true) } finally { $file.Dispose() }
  if (Test-Path -LiteralPath $path) {
    if ((Get-CrabboxRunnerHash $path) -ne '` + artifact.SHA256 + `') { throw 'existing runner digest mismatch' }
  } else {
    try { [System.IO.File]::Move($temp,$path) }
    catch {
      if (-not (Test-Path -LiteralPath $path) -or (Get-CrabboxRunnerHash $path) -ne '` + artifact.SHA256 + `') { throw }
    }
  }
} finally { if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue } }
`, nil
	}
	script := `set -eu
umask 077
dest=` + shellLiteral(name) + `
dir=${dest%/*}
mkdir -p "$dir"
[ ! -L "$dir" ] || { echo 'runner cache is a symlink' >&2; exit 2; }
chmod 700 "$dir"
temp=$(mktemp "$dir/.install.XXXXXX")
trap 'rm -f -- "$temp"' EXIT HUP INT TERM
cat > "$temp"
` + digestFunction + `
[ "$(digest "$temp")" = '` + artifact.SHA256 + `' ] || { echo 'runner digest mismatch' >&2; exit 2; }
chmod 700 "$temp"
if [ -e "$dest" ] || [ -L "$dest" ]; then
  [ ! -L "$dest" ] && [ -f "$dest" ] && [ "$(digest "$dest")" = '` + artifact.SHA256 + `' ] || { echo 'existing runner digest mismatch' >&2; exit 2; }
else
  mv "$temp" "$dest"
fi
`
	return "sh -c " + shellLiteral(script), nil
}

// PrepareInstallCommand allows file-upload-only APIs to stage a uniquely named
// payload in the same private directory before invoking InstallUploadedCommand.
func PrepareInstallCommand(runtime Runtime, artifact Artifact) (string, error) {
	name, err := artifact.RemotePath(runtime)
	if err != nil {
		return "", err
	}
	if runtime.Target.OS == "windows" {
		return "", errors.New("file-upload runner bootstrap currently requires a POSIX target")
	}
	return "sh -c " + shellLiteral("set -eu\numask 077\ndest="+shellLiteral(name)+"\ndir=${dest%/*}\nmkdir -p \"$dir\"\n[ ! -L \"$dir\" ] || exit 2\nchmod 700 \"$dir\""), nil
}

func InstallUploadedCommand(runtime Runtime, artifact Artifact, uploaded string) (string, error) {
	if runtime.Target.OS == "windows" {
		return "", errors.New("file-upload runner bootstrap currently requires a POSIX target")
	}
	if uploaded == "" || strings.ContainsAny(uploaded, "\x00\r\n") {
		return "", errors.New("invalid uploaded runner path")
	}
	command, err := installCommand(runtime, artifact)
	if err != nil {
		return "", err
	}
	return command + " < " + shellLiteral(uploaded), nil
}

func InvokeCommand(runtime Runtime, artifact Artifact, textOnly bool) (string, error) {
	return invokeCommand(runtime, artifact, textOnly, "")
}

// InvokeCommandWithInputSize uses the caller's exact transport frame size when
// that exec transport does not close stdin after sending its payload.
func InvokeCommandWithInputSize(runtime Runtime, artifact Artifact, textOnly bool, size int64) (string, error) {
	if size < 1 || size > MaxRequestBytes() {
		return "", errors.New("invalid runner input byte count")
	}
	return invokeCommand(runtime, artifact, textOnly, " --input-bytes "+strconv.FormatInt(size, 10))
}

func invokeCommand(runtime Runtime, artifact Artifact, textOnly bool, inputBound string) (string, error) {
	name, err := artifact.RemotePath(runtime)
	if err != nil {
		return "", err
	}
	mode := "serve"
	if textOnly {
		mode = "serve-base64"
	}
	mode += inputBound
	if runtime.Target.OS == "windows" {
		if !textOnly {
			return "", errors.New("native Windows shell transport requires base64 framing")
		}
		return `$ErrorActionPreference='Stop'
` + windowsDigestFunction + `
$path=` + powerShellLiteral(name) + `
if ((Get-CrabboxRunnerHash $path) -ne '` + artifact.SHA256 + `') { throw 'runner digest mismatch' }
& $path ` + mode + `
exit $LASTEXITCODE`, nil
	}
	return "sh -c " + shellLiteral("set -eu\n"+digestFunction+"\ndest="+shellLiteral(name)+"\n[ ! -L \"$dest\" ] && [ -f \"$dest\" ] && [ \"$(digest \"$dest\")\" = '"+artifact.SHA256+"' ] || { echo 'runner digest mismatch' >&2; exit 2; }\nexec \"$dest\" "+mode), nil
}

const windowsDigestFunction = `function Get-CrabboxRunnerHash([string]$Name) {
  $file=[IO.File]::OpenRead($Name)
  $hash=[Security.Cryptography.SHA256]::Create()
  try { return [BitConverter]::ToString($hash.ComputeHash($file)).Replace('-','').ToLowerInvariant() }
  finally { $hash.Dispose(); $file.Dispose() }
}`

const digestFunction = `digest() {
  if command -v sha256sum >/dev/null 2>&1; then value=$(sha256sum < "$1");
  elif command -v shasum >/dev/null 2>&1; then value=$(shasum -a 256 < "$1");
  else echo 'runner installation requires sha256sum or shasum' >&2; return 2; fi
  printf '%s\n' "${value%% *}"
}`

func shellLiteral(value string) string      { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
func powerShellLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func ValidateArtifact(artifact Artifact) error {
	digest := sha256.Sum256(artifact.Data)
	if hex.EncodeToString(digest[:]) != artifact.SHA256 {
		return fmt.Errorf("runner artifact digest mismatch")
	}
	return nil
}
