package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// DelegatedRunArtifactSelection uses the ordinary artifact patterns and literal
// download paths. Required selections are also collected for local validation.
func DelegatedRunArtifactSelection(req RunRequest) ([]string, error) {
	if err := ValidateRunArtifactGlobs(req.ArtifactGlobs); err != nil {
		return nil, err
	}
	if err := ValidateRequiredRunArtifactGlobs(req.RequiredArtifactGlobs); err != nil {
		return nil, err
	}
	selected := append(append([]string{}, req.ArtifactGlobs...), req.RequiredArtifactGlobs...)
	for _, value := range req.Downloads {
		spec, err := parseRunDownloadSpec(value)
		if err != nil {
			return nil, err
		}
		remote, err := normalizeDelegatedRunFilePath("--download", spec.Remote)
		if err != nil {
			return nil, err
		}
		selected = append(selected, remote)
	}
	return selected, nil
}

// ReadDelegatedRunArtifactArchive validates a complete bounded archive without
// writing files or extracting paths supplied by the runner.
func ReadDelegatedRunArtifactArchive(ctx context.Context, req RunRequest, archive []byte) (map[string][]byte, error) {
	selected, err := DelegatedRunArtifactSelection(req)
	if err != nil {
		return nil, err
	}
	fail := func(message string) (map[string][]byte, error) {
		return nil, Exit(7, "delegated artifacts: %s", message)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(archive)) > DelegatedRunArtifactDefaultMaxBytes {
		return fail("compressed byte limit exceeded")
	}
	compressed := bytes.NewReader(archive)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return fail("invalid gzip archive")
	}
	defer gz.Close()
	gz.Multistream(false)
	// Bound expanded bytes as well as the HTTP payload, including tar metadata.
	limited := &io.LimitedReader{R: &contextRunDownloadReader{ctx: ctx, reader: gz}, N: DelegatedRunArtifactDefaultMaxBytes + 1024*1024 + 1}
	tr := tar.NewReader(limited)
	files := map[string][]byte{}
	var total int64
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail("invalid or incomplete tar archive")
		}
		name := header.Name
		if name == "" || name == "." || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00") {
			return fail("unsafe archive member")
		}
		for _, component := range strings.Split(name, "/") {
			if component == ".git" || component == ".crabbox" {
				return fail("protected archive member")
			}
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fail("artifact must be a regular file; links are unsupported")
		}
		if _, duplicate := files[name]; duplicate || len(files) >= DelegatedRunArtifactDefaultMaxFiles {
			return fail("duplicate or excessive archive members")
		}
		allowed := false
		for _, glob := range selected {
			pattern, err := CompileRunArtifactGlob(glob)
			if err != nil {
				return fail("invalid selection")
			}
			allowed = allowed || pattern.MatchString(name) || pattern.MatchString("./"+name)
		}
		if !allowed {
			return fail("archive member was not selected")
		}
		if header.Size < 0 || header.Size > DelegatedRunArtifactDefaultMaxBytes-total {
			return fail("expanded byte limit exceeded")
		}
		data, err := io.ReadAll(tr)
		if err != nil || int64(len(data)) != header.Size {
			return fail("incomplete archive member")
		}
		total += header.Size
		files[name] = data
	}
	// Consume zero tar padding and the gzip trailer; reject concatenated payloads.
	var padding [4096]byte
	for {
		n, err := limited.Read(padding[:])
		for _, b := range padding[:n] {
			if b != 0 {
				return fail("content after tar end")
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail("invalid gzip trailer")
		}
	}
	if limited.N == 0 || compressed.Len() != 0 {
		return fail("excess archive content")
	}
	for _, glob := range req.RequiredArtifactGlobs {
		pattern, _ := CompileRunArtifactGlob(glob)
		matched := false
		for name := range files {
			matched = matched || pattern.MatchString(name) || pattern.MatchString("./"+name)
		}
		if !matched {
			return fail("missing required artifact: " + glob)
		}
	}
	for _, value := range req.Downloads {
		spec, _ := parseRunDownloadSpec(value)
		remote, _ := normalizeDelegatedRunFilePath("--download", spec.Remote)
		if _, ok := files[remote]; !ok {
			return fail("missing download: " + remote)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return files, nil
}

// MaterializeDelegatedRunArtifactArchive validates the complete bounded archive
// before publishing anything. It never extracts paths supplied by the runner.
func MaterializeDelegatedRunArtifactArchive(ctx context.Context, req RunRequest, leaseID string, archive []byte) ([]RunArtifact, error) {
	files, err := ReadDelegatedRunArtifactArchive(ctx, req, archive)
	if err != nil {
		return nil, err
	}
	var artifacts []RunArtifact
	for _, value := range req.Downloads {
		spec, _ := parseRunDownloadSpec(value)
		remote, _ := normalizeDelegatedRunFilePath("--download", spec.Remote)
		if err := writeRunDownloadFile(spec.Local, files[remote]); err != nil {
			return nil, Exit(2, "download %s: write %s: %v", remote, spec.Local, err)
		}
		artifacts = append(artifacts, RunArtifact{Kind: "delegated-download", Path: spec.Local, Bytes: len(files[remote])})
	}
	if len(req.ArtifactGlobs) > 0 {
		local := LocalRunArtifactPath(req.Repo.Root, req.RunID, leaseID, "delegated-artifacts.tgz")
		if err := writeRunDownloadFile(local, archive); err != nil {
			return nil, fmt.Errorf("write artifact archive: %w", err)
		}
		artifacts = append(artifacts, RunArtifact{Kind: "artifact-glob", Path: local, Bytes: len(archive)})
	}
	return artifacts, nil
}
