package blacksmith

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"path"
	"regexp"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type artifactContextReader struct {
	ctx context.Context
	io.Reader
}

func (r *artifactContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func safeArtifactMember(name string) bool {
	if name == "" || name == "." || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
		return false
	}
	for _, component := range strings.Split(name, "/") {
		if component == ".git" || component == ".crabbox" {
			return false
		}
	}
	return true
}

func validateBlacksmithArtifactArchive(ctx context.Context, archive []byte, globs, required []string) (err error) {
	defer func() { err = blacksmithContextError(ctx, err) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if int64(len(archive)) > core.DelegatedRunArtifactDefaultMaxBytes {
		return errors.New("artifact archive exceeds compressed byte limit")
	}
	compressed := bytes.NewReader(archive)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return errors.New("invalid artifact gzip header")
	}
	defer gz.Close()
	gz.Multistream(false)
	reader := &artifactContextReader{ctx: ctx, Reader: gz}
	tr := tar.NewReader(reader)
	selected := append(append([]string{}, globs...), required...)
	patterns := make(map[string]*regexp.Regexp, len(selected))
	for _, glob := range selected {
		if err := ctx.Err(); err != nil {
			return err
		}
		if patterns[glob] == nil {
			pattern, err := core.CompileRunArtifactGlob(glob)
			if err != nil {
				return errors.New("invalid artifact selection pattern")
			}
			patterns[glob] = pattern
		}
	}
	matches := func(glob, name string) bool {
		pattern := patterns[glob]
		return pattern.MatchString(name) || pattern.MatchString("./"+name)
	}
	seen := make(map[string]bool)
	matched := make([]bool, len(required))
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.New("invalid or incomplete artifact tar stream")
		}
		if !safeArtifactMember(header.Name) || seen[header.Name] || len(seen) >= core.DelegatedRunArtifactDefaultMaxFiles {
			return errors.New("invalid, duplicate or excessive artifact members")
		}
		switch header.Typeflag {
		case tar.TypeReg, tar.TypeRegA, tar.TypeSymlink:
		case tar.TypeLink:
			if !safeArtifactMember(header.Linkname) || !seen[header.Linkname] {
				return errors.New("invalid artifact hardlink target")
			}
		default:
			return errors.New("unsupported artifact member type")
		}
		allowed := false
		for _, glob := range selected {
			allowed = allowed || matches(glob, header.Name)
		}
		if !allowed {
			return errors.New("artifact member was not selected")
		}
		for i, glob := range required {
			matched[i] = matched[i] || matches(glob, header.Name)
		}
		seen[header.Name] = true
		// Next skips physical contents. Reading the logical body would expand
		// sparse holes without passing through the cancellation-aware input.
	}
	// Tar may pad its end blocks, but no further content or gzip member is part
	// of this one finalized archive. Reading to EOF checks the gzip trailer.
	var padding [4096]byte
	for {
		n, err := reader.Read(padding[:])
		for _, b := range padding[:n] {
			if b != 0 {
				return errors.New("unexpected content after artifact tar end")
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.New("incomplete artifact gzip trailer")
		}
	}
	if compressed.Len() != 0 {
		return errors.New("unexpected data after artifact gzip stream")
	}
	for _, found := range matched {
		if !found {
			return errors.New("required artifact missing from archive")
		}
	}
	return nil
}
