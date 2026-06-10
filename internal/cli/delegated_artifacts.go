package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const DelegatedArtifactMaxBytes = 64 * 1024

type DelegatedArtifactRequest struct {
	LeaseID    string
	Slug       string
	RemotePath string
	MaxBytes   int
}

type DelegatedArtifactBackend interface {
	Backend
	FetchDelegatedArtifact(ctx context.Context, req DelegatedArtifactRequest) ([]byte, error)
}

func CollectDelegatedRunArtifacts(ctx context.Context, backend DelegatedArtifactBackend, req RunRequest, repoRoot, runID, leaseID string, stderr io.Writer) ([]RunArtifact, error) {
	if backend == nil {
		return nil, nil
	}
	cache := map[string][]byte{}
	fetch := func(remote string) ([]byte, error) {
		remote = strings.TrimSpace(remote)
		if data, ok := cache[remote]; ok {
			return data, nil
		}
		if err := validateDelegatedArtifactPath("--require-artifact", remote); err != nil {
			return nil, err
		}
		data, err := backend.FetchDelegatedArtifact(ctx, DelegatedArtifactRequest{
			LeaseID:    leaseID,
			Slug:       firstNonBlank(req.RequestedSlug, leaseID),
			RemotePath: remote,
			MaxBytes:   DelegatedArtifactMaxBytes,
		})
		if err != nil {
			return nil, exit(7, "delegated artifact %s: %v", remote, err)
		}
		if len(data) > DelegatedArtifactMaxBytes {
			return nil, exit(7, "delegated artifact %s exceeds %d bytes", remote, DelegatedArtifactMaxBytes)
		}
		cache[remote] = data
		return data, nil
	}
	for _, remote := range req.RequiredArtifactGlobs {
		_, err := fetch(remote)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(stderr, "required artifact %s matched=1\n", remote)
	}
	artifacts := make([]RunArtifact, 0, len(req.Downloads))
	for _, specValue := range req.Downloads {
		spec, err := parseRunDownloadSpec(specValue)
		if err != nil {
			return nil, err
		}
		if err := validateDelegatedArtifactPath("--download", spec.Remote); err != nil {
			return nil, err
		}
		data, err := fetch(spec.Remote)
		if err != nil {
			return nil, err
		}
		if dir := filepath.Dir(spec.Local); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, exit(2, "download %s: create %s: %v", spec.Remote, dir, err)
			}
		}
		if err := os.WriteFile(spec.Local, data, 0o666); err != nil {
			return nil, exit(2, "download %s: write %s: %v", spec.Remote, spec.Local, err)
		}
		fmt.Fprintf(stderr, "downloaded %s bytes=%d\n", spec.Local, len(data))
		artifacts = append(artifacts, RunArtifact{Kind: "delegated-download", Path: spec.Local, Bytes: len(data)})
	}
	return artifacts, nil
}

func HasDelegatedArtifactRequests(req RunRequest) bool {
	return len(req.RequiredArtifactGlobs) > 0 || len(req.Downloads) > 0
}

func validateDelegatedArtifactFileRequests(paths []string) error {
	for _, remote := range paths {
		if err := validateDelegatedArtifactPath("--require-artifact", remote); err != nil {
			return err
		}
	}
	return nil
}

func validateDelegatedDownloadRequests(downloads []string) error {
	for _, specValue := range downloads {
		spec, err := parseRunDownloadSpec(specValue)
		if err != nil {
			return err
		}
		if err := validateDelegatedArtifactPath("--download", spec.Remote); err != nil {
			return err
		}
	}
	return nil
}

func validateDelegatedArtifactPath(flag, remote string) error {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return exit(2, "%s requires a non-empty remote path", flag)
	}
	if !safeArtifactGlob(remote) || strings.ContainsAny(remote, "*?") {
		return exit(2, "%s for delegated providers requires a safe relative file path: %s", flag, remote)
	}
	return nil
}
