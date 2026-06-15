package cli

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
)

const DelegatedRunDownloadMaxBytes = 64 * 1024

type DelegatedRunDownloadRequest struct {
	LeaseID    string
	RemotePath string
	MaxBytes   int
}

func MaterializeDelegatedRunDownloads(ctx context.Context, backend DelegatedRunDownloadBackend, req RunRequest, leaseID string, stderr io.Writer) ([]RunArtifact, error) {
	if backend == nil {
		return nil, nil
	}
	cache := map[string][]byte{}
	fetch := func(remote string) ([]byte, error) {
		var err error
		remote, err = normalizeDelegatedRunFilePath("--require-artifact", remote)
		if err != nil {
			return nil, err
		}
		if data, ok := cache[remote]; ok {
			return data, nil
		}
		data, err := backend.FetchRunFile(ctx, DelegatedRunDownloadRequest{
			LeaseID:    leaseID,
			RemotePath: remote,
			MaxBytes:   DelegatedRunDownloadMaxBytes,
		})
		if err != nil {
			return nil, exit(7, "delegated artifact %s: %v", remote, err)
		}
		if len(data) > DelegatedRunDownloadMaxBytes {
			return nil, exit(7, "delegated artifact %s exceeds %d bytes", remote, DelegatedRunDownloadMaxBytes)
		}
		cache[remote] = data
		return data, nil
	}

	for _, remote := range req.RequiredArtifactGlobs {
		if _, err := fetch(remote); err != nil {
			return nil, err
		}
		fmt.Fprintf(stderr, "required artifact %s matched=1\n", remote)
	}

	artifacts := make([]RunArtifact, 0, len(req.Downloads))
	for _, value := range req.Downloads {
		spec, err := parseRunDownloadSpec(value)
		if err != nil {
			return nil, err
		}
		if err := validateDelegatedRunFilePath("--download", spec.Remote); err != nil {
			return nil, err
		}
		data, err := fetch(spec.Remote)
		if err != nil {
			return nil, err
		}
		if err := writeRunDownloadFile(spec.Local, data); err != nil {
			return nil, exit(2, "download %s: write %s: %v", spec.Remote, spec.Local, err)
		}
		fmt.Fprintf(stderr, "downloaded %s bytes=%d\n", spec.Local, len(data))
		artifacts = append(artifacts, RunArtifact{
			Kind:  "delegated-download",
			Path:  spec.Local,
			Bytes: len(data),
		})
	}
	return artifacts, nil
}

func HasDelegatedRunDownloadRequests(req RunRequest) bool {
	return len(req.RequiredArtifactGlobs) > 0 || len(req.Downloads) > 0
}

func validateDelegatedRequiredArtifacts(paths []string) error {
	for _, remote := range paths {
		if err := validateDelegatedRunFilePath("--require-artifact", remote); err != nil {
			return err
		}
	}
	return nil
}

func validateDelegatedDownloads(downloads []string) error {
	for _, value := range downloads {
		spec, err := parseRunDownloadSpec(value)
		if err != nil {
			return err
		}
		if err := validateDelegatedRunFilePath("--download", spec.Remote); err != nil {
			return err
		}
	}
	return nil
}

func validateDelegatedRunFilePath(flag, remote string) error {
	_, err := normalizeDelegatedRunFilePath(flag, remote)
	return err
}

func normalizeDelegatedRunFilePath(flag, remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", exit(2, "%s requires a non-empty remote path", flag)
	}
	remote = path.Clean(remote)
	protected := false
	for _, component := range strings.Split(remote, "/") {
		if component == ".git" || component == ".crabbox" {
			protected = true
			break
		}
	}
	if !safeArtifactGlob(remote) ||
		remote == "." ||
		strings.ContainsAny(remote, "*?:\\") ||
		strings.HasPrefix(remote, "/") ||
		protected {
		return "", exit(2, "%s for delegated providers requires a safe relative file path: %s", flag, remote)
	}
	return remote, nil
}
