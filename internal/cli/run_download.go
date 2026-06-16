package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type runDownloadSpec struct {
	Remote string
	Local  string
}

func parseRunDownloadSpec(value string) (runDownloadSpec, error) {
	remote, local, ok := strings.Cut(strings.TrimSpace(value), "=")
	remote = strings.TrimSpace(remote)
	local = strings.TrimSpace(local)
	if !ok || remote == "" || local == "" {
		return runDownloadSpec{}, exit(2, "--download expects remote=local")
	}
	return runDownloadSpec{Remote: remote, Local: local}, nil
}

func preflightRunLocalOutputs(captureStdout, captureStderr string, downloads []string) error {
	if captureStdout != "" && captureStderr != "" {
		same, err := sameLocalOutputPath(captureStdout, captureStderr)
		if err != nil {
			return err
		}
		if same {
			return exit(2, "capture stdout/stderr: paths must be different")
		}
	}
	parsedDownloads := make([]runDownloadSpec, 0, len(downloads))
	for _, spec := range downloads {
		download, err := parseRunDownloadSpec(spec)
		if err != nil {
			return err
		}
		parsedDownloads = append(parsedDownloads, download)
	}
	for _, download := range parsedDownloads {
		if err := rejectCaptureDownloadCollision("capture stdout", captureStdout, download); err != nil {
			return err
		}
		if err := rejectCaptureDownloadCollision("capture stderr", captureStderr, download); err != nil {
			return err
		}
	}
	for i := range parsedDownloads {
		for j := i + 1; j < len(parsedDownloads); j++ {
			same, err := sameLocalOutputPath(parsedDownloads[i].Local, parsedDownloads[j].Local)
			if err != nil {
				return err
			}
			if same {
				return exit(2, "download %s/download %s: paths must be different", parsedDownloads[i].Remote, parsedDownloads[j].Remote)
			}
		}
	}
	for _, download := range parsedDownloads {
		if err := preflightLocalOutputPath("download "+download.Remote, download.Local, true, true); err != nil {
			return err
		}
	}
	if captureStdout != "" {
		if err := preflightLocalOutputPath("capture stdout", captureStdout, false, true); err != nil {
			return err
		}
	}
	if captureStderr != "" {
		if err := preflightLocalOutputPath("capture stderr", captureStderr, false, true); err != nil {
			return err
		}
	}
	return nil
}

func rejectCaptureDownloadCollision(label, capturePath string, download runDownloadSpec) error {
	if capturePath == "" {
		return nil
	}
	same, err := sameLocalOutputPath(capturePath, download.Local)
	if err != nil {
		return err
	}
	if same {
		return exit(2, "%s/download %s: paths must be different", label, download.Remote)
	}
	return nil
}

func preflightProofOutputPath(proofPath, captureStdout, captureStderr string, downloads []string) error {
	if proofPath == "" {
		return nil
	}
	if captureStdout != "" {
		same, err := sameLocalOutputPath(proofPath, captureStdout)
		if err != nil {
			return err
		}
		if same {
			return exit(2, "emit proof/capture stdout: paths must be different")
		}
	}
	if captureStderr != "" {
		same, err := sameLocalOutputPath(proofPath, captureStderr)
		if err != nil {
			return err
		}
		if same {
			return exit(2, "emit proof/capture stderr: paths must be different")
		}
	}
	for _, spec := range downloads {
		download, err := parseRunDownloadSpec(spec)
		if err != nil {
			return err
		}
		same, err := sameLocalOutputPath(proofPath, download.Local)
		if err != nil {
			return err
		}
		if same {
			return exit(2, "emit proof/download %s: paths must be different", download.Remote)
		}
	}
	return nil
}

func sameLocalOutputPath(left, right string) (bool, error) {
	leftAbs, err := filepath.Abs(filepath.Clean(left))
	if err != nil {
		return false, exit(2, "capture stdout/stderr: %v", err)
	}
	rightAbs, err := filepath.Abs(filepath.Clean(right))
	if err != nil {
		return false, exit(2, "capture stdout/stderr: %v", err)
	}
	return leftAbs == rightAbs, nil
}

func preflightLocalOutputPath(label, path string, allowMissingDirs, replaceExisting bool) error {
	dir := filepath.Dir(path)
	info, err := os.Stat(path)
	if replaceExisting {
		info, err = os.Lstat(path)
	}
	if err == nil {
		if info.IsDir() {
			return exit(2, "%s: %s is a directory", label, path)
		}
		if replaceExisting {
			if err := checkWritableDir(label, firstNonBlank(dir, ".")); err != nil {
				return err
			}
			return checkPrivateRunOutputReplaceable(label, path)
		}
		return checkWritableFile(label, path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return exit(2, "%s: %v", label, err)
	}
	if dir == "." || dir == "" {
		return checkWritableDir(label, ".")
	}
	if !allowMissingDirs {
		return checkWritableDir(label, dir)
	}
	existing := dir
	for {
		info, err := os.Stat(existing)
		if err == nil {
			if !info.IsDir() {
				return exit(2, "%s: %s is not a directory", label, existing)
			}
			return checkWritableDir(label, existing)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return exit(2, "%s: %v", label, err)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return exit(2, "%s: %v", label, err)
		}
		existing = parent
	}
}

func checkWritableFile(label, path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return exit(2, "%s: %v", label, err)
	}
	if err := file.Close(); err != nil {
		return exit(2, "%s close: %v", label, err)
	}
	return nil
}

func checkWritableDir(label, dir string) error {
	temp, err := os.CreateTemp(dir, ".crabbox-output-*")
	if err != nil {
		return exit(2, "%s: %v", label, err)
	}
	name := temp.Name()
	closeErr := temp.Close()
	removeErr := os.Remove(name)
	if closeErr != nil {
		return exit(2, "%s close: %v", label, closeErr)
	}
	if removeErr != nil {
		return exit(2, "%s cleanup: %v", label, removeErr)
	}
	return nil
}

func downloadRemoteFile(ctx context.Context, target SSHTarget, workdir, specValue string) (int, string, error) {
	spec, err := parseRunDownloadSpec(specValue)
	if err != nil {
		return 0, "", err
	}
	encoded, err := runSSHOutput(ctx, target, remoteDownloadBase64Command(target, workdir, spec.Remote))
	if err != nil {
		return 0, spec.Local, exit(7, "download %s: %v", spec.Remote, err)
	}
	data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(encoded), ""))
	if err != nil {
		return 0, spec.Local, exit(7, "download %s: decode base64: %v", spec.Remote, err)
	}
	if err := writeRunDownloadFile(spec.Local, data); err != nil {
		return 0, spec.Local, exit(2, "download %s: write %s: %v", spec.Remote, spec.Local, err)
	}
	return len(data), spec.Local, nil
}

func writeRunDownloadFile(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := createPrivateRunOutputDir(dir); err != nil {
			return err
		}
	}
	return writePrivateRunOutputFile(path, data)
}

func remoteDownloadBase64Command(target SSHTarget, workdir, remotePath string) string {
	if isWindowsNativeTarget(target) {
		return powershellCommand(`$ErrorActionPreference = "Stop"
Set-Location -LiteralPath ` + psQuote(workdir) + `
$path = ` + psQuote(remotePath) + `
if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "download file not found: $path" }
[Convert]::ToBase64String([System.IO.File]::ReadAllBytes((Resolve-Path -LiteralPath $path).Path))`)
	}
	return fmt.Sprintf("cd %s && test -f %s && base64 < %s", shellQuote(workdir), shellQuote(remotePath), shellQuote(remotePath))
}
