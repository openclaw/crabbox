package runtimeartifact

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
)

// ArtifactInput names an already-built companion within the manifest directory.
// Path must be a slash-separated relative path with no symlink components.
type ArtifactInput struct {
	Target Target
	Path   string
}

// MarshalLocal prepares a v1 local manifest for already-built, finalized files.
// manifestDir is where the caller will write the manifest; every input Path is
// relative to that directory, so the resulting pack remains relocatable.
//
// This function reads and inspects files without modifying or executing them.
// The caller must freeze the controller and companions from one input set and
// keep them unchanged during preparation and use. Their hashes establish byte
// pairing, not common-source provenance or runtime protocol implementation.
//
// The result is deterministic JSON with targets sorted by OS and architecture,
// followed by a newline. The caller owns the destination and must atomically
// publish these bytes only after the pack's files have been finalized.
func MarshalLocal(ctx context.Context, manifestDir, controllerPath string, inputs []ArtifactInput, protocol string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if protocol == "" {
		return nil, fmt.Errorf("expected protocol must not be empty")
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("runtime manifest contains no artifacts")
	}
	controller, err := os.Open(controllerPath)
	if err != nil {
		return nil, fmt.Errorf("open controller: %w", err)
	}
	defer controller.Close()
	controllerHash, err := fileDigest(ctx, controller, 0)
	if err != nil {
		return nil, fmt.Errorf("hash controller: %w", err)
	}
	m := manifest{SchemaVersion: 1, ProtocolVersion: protocol, ControllerSHA256: controllerHash}
	for _, input := range inputs {
		e, err := prepareEntry(ctx, manifestDir, input)
		if err != nil {
			return nil, err
		}
		m.Artifacts = append(m.Artifacts, e)
	}
	if err := validateManifest(&m, protocol); err != nil {
		return nil, err
	}
	slices.SortFunc(m.Artifacts, func(a, b entry) int {
		if result := strings.Compare(a.OS, b.OS); result != 0 {
			return result
		}
		return strings.Compare(a.Arch, b.Arch)
	})
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if len(data) > maxManifestSize {
		return nil, fmt.Errorf("manifest exceeds %d bytes", maxManifestSize)
	}
	return data, nil
}

func prepareEntry(ctx context.Context, dir string, input ArtifactInput) (entry, error) {
	if !supported(input.Target) {
		return entry{}, fmt.Errorf("unsupported artifact target %s/%s", input.Target.OS, input.Target.Arch)
	}
	return prepareExecutableEntry(ctx, dir, input)
}

func prepareExecutableEntry(ctx context.Context, dir string, input ArtifactInput) (entry, error) {
	f, err := openPackFile(dir, input.Path)
	if err != nil {
		return entry{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return entry{}, err
	}
	if info.Size() <= 0 {
		return entry{}, fmt.Errorf("artifact size must be positive")
	}
	digest, err := fileDigest(ctx, f, info.Size())
	if err != nil {
		return entry{}, fmt.Errorf("hash artifact %q: %w", input.Path, err)
	}
	if err := inspectExecutable(ctx, f, input.Target); err != nil {
		return entry{}, fmt.Errorf("inspect artifact %q: %w", input.Path, err)
	}
	return entry{input.Target.OS, input.Target.Arch, input.Path, info.Size(), digest}, nil
}
