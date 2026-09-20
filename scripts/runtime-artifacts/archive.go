package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/openclaw/crabbox/internal/remoteruntime"
	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

type fileRecord struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type runtimeRecord struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
	fileRecord
	Capabilities []runtimeartifact.CapabilityClaim `json:"capabilities,omitempty"`
}

type packRecord struct {
	SchemaVersion    int             `json:"schemaVersion,omitempty"`
	ProtocolVersion  string          `json:"protocolVersion,omitempty"`
	ControllerSHA256 string          `json:"controllerSha256"`
	Manifest         *fileRecord     `json:"manifest,omitempty"`
	Artifacts        []runtimeRecord `json:"artifacts"`
}

type archiveRecord struct {
	Name        string      `json:"name"`
	Size        int64       `json:"size"`
	SHA256      string      `json:"sha256"`
	OS          string      `json:"os"`
	Arch        string      `json:"arch"`
	RuntimePack *packRecord `json:"runtimePack,omitempty"`
}

func validArchiveTarget(platform, arch string) bool {
	return (platform == "darwin" || platform == "linux" || platform == "windows") && (arch == "amd64" || arch == "arm64")
}

func validArchiveMode(mode string) bool {
	return mode == "none" || mode == "unsigned" || mode == "final" || mode == "unsigned-filesystem" || mode == "final-filesystem"
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func digestFile(ctx context.Context, file string) (fileRecord, error) {
	f, err := os.Open(file)
	if err != nil {
		return fileRecord{}, err
	}
	defer f.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, contextReader{ctx, f})
	return fileRecord{Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}, err
}

// The caller owns the archive's immutability. Extraction never executes its
// contents and creates only the enumerated regular files in a new directory.
func extractArchive(ctx context.Context, archive, directory, platform, arch, mode, buildID string) (report archiveRecord, err error) {
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if !validArchiveTarget(platform, arch) || !validArchiveMode(mode) {
		return report, fmt.Errorf("invalid release archive target or runtime mode")
	}
	filesystemPack := mode == "unsigned-filesystem" || mode == "final-filesystem"
	finalPack := mode == "final" || mode == "final-filesystem"
	unsignedPack := mode == "unsigned" || mode == "unsigned-filesystem"
	if filesystemPack && !validBuildID(buildID) || !filesystemPack && buildID != "" {
		return report, fmt.Errorf("filesystem archive mode requires its exact build ID; legacy modes do not accept it")
	}
	controller := "crabbox"
	if platform == "windows" {
		controller += ".exe"
	}
	// Limits cover normal release payloads while bounding decompression and disk use.
	type allowedEntry struct {
		path  string
		limit int64
	}
	allowed := map[string]allowedEntry{controller: {controller, 512 << 20}}
	if platform == "darwin" && arch == "arm64" {
		allowed["crabbox-apple-vm-helper"] = allowedEntry{"crabbox-apple-vm-helper", 512 << 20}
	}
	if mode != "none" {
		names := []string{"linux-amd64", "linux-arm64"}
		if filesystemPack {
			names = nil
			for _, input := range filesystemInputs(buildID) {
				names = append(names, input.Path)
			}
		}
		for _, name := range names {
			name = "crabbox-runtime/" + name
			allowed[name] = allowedEntry{name, 64 << 20}
		}
		if finalPack {
			allowed["crabbox-runtime/manifest.json"] = allowedEntry{"crabbox-runtime/manifest.json", 64 << 10}
		}
	}
	input, err := os.Open(archive)
	if err != nil {
		return report, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return report, fmt.Errorf("release archive must be a regular file")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx, input}); err != nil {
		return report, err
	}
	report = archiveRecord{Name: filepath.Base(archive), Size: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)), OS: platform, Arch: arch}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return report, err
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return report, fmt.Errorf("create new extraction directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(directory)
		}
	}()
	if mode != "none" {
		if err := os.Mkdir(filepath.Join(directory, "crabbox-runtime"), 0700); err != nil {
			return report, err
		}
	}
	seen := make(map[string]bool)
	directorySeen := false
	copyEntry := func(name string, size int64, kind os.FileMode, modified time.Time, reader io.Reader) error {
		if kind.IsDir() && size == 0 && unsignedPack && (name == "crabbox-runtime/" || name == "crabbox-runtime") && !directorySeen {
			directorySeen = true
			return nil
		}
		entry, ok := allowed[name]
		if !ok || seen[entry.path] || !kind.IsRegular() || size <= 0 || size > entry.limit {
			return fmt.Errorf("unexpected, duplicate, nonregular, or oversized archive member: %s", name)
		}
		seen[entry.path] = true
		// Filesystem paths come from the fixed inventory, never the archive header.
		file := filepath.Join(directory, filepath.FromSlash(entry.path))
		output, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		written, copyErr := io.Copy(output, contextReader{ctx, io.LimitReader(reader, size+1)})
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if written != size {
			return fmt.Errorf("archive member length mismatch: %s", name)
		}
		permissions := os.FileMode(0755)
		if entry.path == "crabbox-runtime/manifest.json" {
			permissions = 0644
		}
		if err := os.Chmod(file, permissions); err != nil {
			return err
		}
		if !modified.IsZero() {
			return os.Chtimes(file, modified, modified)
		}
		return nil
	}
	if platform == "windows" {
		reader, openErr := zip.NewReader(input, info.Size())
		if openErr != nil {
			return report, openErr
		}
		for _, entry := range reader.File {
			if entry.UncompressedSize64 > 1<<30 {
				return report, fmt.Errorf("oversized archive member: %s", entry.Name)
			}
			contents, openErr := entry.Open()
			if openErr != nil {
				return report, openErr
			}
			copyErr := copyEntry(entry.Name, int64(entry.UncompressedSize64), entry.Mode(), entry.Modified, contents)
			closeErr := contents.Close()
			if copyErr != nil {
				return report, copyErr
			}
			if closeErr != nil {
				return report, closeErr
			}
		}
	} else {
		compressed, openErr := gzip.NewReader(input)
		if openErr != nil {
			return report, openErr
		}
		defer compressed.Close()
		reader := tar.NewReader(compressed)
		for {
			header, readErr := reader.Next()
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return report, readErr
			}
			if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA && header.Typeflag != tar.TypeDir {
				return report, fmt.Errorf("nonregular archive member: %s", header.Name)
			}
			if err := copyEntry(header.Name, header.Size, header.FileInfo().Mode(), header.ModTime, reader); err != nil {
				return report, err
			}
		}
		trailing, readErr := io.ReadAll(io.LimitReader(contextReader{ctx, compressed}, (1<<20)+1))
		if readErr != nil {
			return report, readErr
		}
		if len(trailing) > 1<<20 || len(bytes.Trim(trailing, "\x00")) != 0 {
			return report, fmt.Errorf("unexpected trailing archive data")
		}
	}
	if len(seen) != len(allowed) {
		return report, fmt.Errorf("release archive member inventory is incomplete")
	}
	if mode != "none" {
		packDirectory := filepath.Join(directory, "crabbox-runtime")
		controllerPath := filepath.Join(directory, controller)
		inputs := []runtimeartifact.ArtifactInput{{Target: runtimeartifact.Target{OS: "linux", Arch: "amd64"}, Path: "linux-amd64"}, {Target: runtimeartifact.Target{OS: "linux", Arch: "arm64"}, Path: "linux-arm64"}}
		var data []byte
		if finalPack {
			var err error
			if filesystemPack {
				data, err = verifyFilesystemPack(ctx, packDirectory, controllerPath, buildID)
			} else {
				err = verify(ctx, packDirectory, controllerPath, inputs, io.Discard)
			}
			if err != nil {
				return report, err
			}
		}
		var err error
		if filesystemPack && !finalPack {
			data, err = prepareFilesystemPack(ctx, packDirectory, controllerPath, buildID)
		} else if !filesystemPack {
			data, err = runtimeartifact.MarshalLocal(ctx, packDirectory, controllerPath, inputs, remoteruntime.Protocol)
		}
		if err != nil {
			return report, err
		}
		pack := &packRecord{}
		if err := json.Unmarshal(data, pack); err != nil {
			return report, err
		}
		if !filesystemPack {
			pack.SchemaVersion = 0 // Preserve the historical schema-2 report shape.
		}
		for i := range pack.Artifacts {
			pack.Artifacts[i].Path = "crabbox-runtime/" + pack.Artifacts[i].Path
		}
		if finalPack {
			manifest, err := digestFile(ctx, filepath.Join(packDirectory, "manifest.json"))
			if err != nil {
				return report, err
			}
			manifest.Path = "crabbox-runtime/manifest.json"
			pack.Manifest = &manifest
		}
		report.RuntimePack = pack
	}
	return report, nil
}
