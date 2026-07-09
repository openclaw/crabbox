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
	return preflightRunOutputCollisions("emit proof", proofPath, captureStdout, captureStderr, downloads)
}

func preflightRunOutputCollisions(label, path, captureStdout, captureStderr string, downloads []string) error {
	if path == "" {
		return nil
	}
	if captureStdout != "" {
		same, err := sameLocalOutputPath(path, captureStdout)
		if err != nil {
			return err
		}
		if same {
			return exit(2, "%s/capture stdout: paths must be different", label)
		}
	}
	if captureStderr != "" {
		same, err := sameLocalOutputPath(path, captureStderr)
		if err != nil {
			return err
		}
		if same {
			return exit(2, "%s/capture stderr: paths must be different", label)
		}
	}
	for _, spec := range downloads {
		download, err := parseRunDownloadSpec(spec)
		if err != nil {
			return err
		}
		same, err := sameLocalOutputPath(path, download.Local)
		if err != nil {
			return err
		}
		if same {
			return exit(2, "%s/download %s: paths must be different", label, download.Remote)
		}
	}
	return nil
}

func sameLocalOutputPath(left, right string) (bool, error) {
	leftCanonical, err := canonicalLocalOutputPath(left)
	if err != nil {
		return false, exit(2, "local output path: %v", err)
	}
	rightCanonical, err := canonicalLocalOutputPath(right)
	if err != nil {
		return false, exit(2, "local output path: %v", err)
	}
	if leftCanonical == rightCanonical {
		return true, nil
	}
	leftInfo, leftErr := os.Stat(leftCanonical)
	rightInfo, rightErr := os.Stat(rightCanonical)
	if leftErr == nil && rightErr == nil {
		return os.SameFile(leftInfo, rightInfo), nil
	}
	for _, statErr := range []error{leftErr, rightErr} {
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return false, exit(2, "local output path: %v", statErr)
		}
	}
	if strings.EqualFold(leftCanonical, rightCanonical) {
		caseInsensitive, err := localPathCaseInsensitive(leftCanonical)
		if err != nil {
			return false, exit(2, "local output path case probe: %v", err)
		}
		if caseInsensitive {
			return true, nil
		}
	}
	return false, nil
}

func canonicalLocalOutputPath(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	return resolveLocalOutputSymlinks(abs, 0)
}

func resolveLocalOutputSymlinks(path string, depth int) (string, error) {
	// Resolve each existing link even when its target or a later component is not created yet.
	if depth > 64 {
		return "", fmt.Errorf("too many symlinks in %s", path)
	}
	volume := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path, volume)
	rest = strings.TrimLeft(rest, string(filepath.Separator))
	current := volume + string(filepath.Separator)
	if volume == "" {
		current = string(filepath.Separator)
	}
	parts := strings.FieldsFunc(rest, func(r rune) bool { return r == rune(filepath.Separator) })
	for i, part := range parts {
		candidate := filepath.Join(current, part)
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			return filepath.Join(append([]string{candidate}, parts[i+1:]...)...), nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			current = candidate
			continue
		}
		target, err := os.Readlink(candidate)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(candidate), target)
		}
		if i+1 < len(parts) {
			target = filepath.Join(append([]string{target}, parts[i+1:]...)...)
		}
		return resolveLocalOutputSymlinks(filepath.Clean(target), depth+1)
	}
	return current, nil
}

func localPathCaseInsensitive(path string) (bool, error) {
	dir := path
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				dir = filepath.Dir(dir)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false, err
		}
		dir = parent
	}
	// Probe the directory because supported platforms can host both case modes.
	probe, err := os.CreateTemp(dir, ".crabbox-case-Aa-*")
	if err != nil {
		return false, err
	}
	probePath := probe.Name()
	probeInfo, statErr := probe.Stat()
	closeErr := probe.Close()
	defer os.Remove(probePath)
	if statErr != nil {
		return false, statErr
	}
	if closeErr != nil {
		return false, closeErr
	}
	foldedPath := filepath.Join(dir, flipASCIICase(filepath.Base(probePath)))
	foldedInfo, err := os.Stat(foldedPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(probeInfo, foldedInfo), nil
}

func flipASCIICase(value string) string {
	flipped := []byte(value)
	for i, char := range flipped {
		switch {
		case char >= 'a' && char <= 'z':
			flipped[i] = char - ('a' - 'A')
		case char >= 'A' && char <= 'Z':
			flipped[i] = char + ('a' - 'A')
		}
	}
	return string(flipped)
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
