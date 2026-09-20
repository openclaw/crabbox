package runtimeartifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Capability names the operation family an artifact source was selected for.
// Installation must still verify that family's runtime handshake; neither the
// operating system nor executable metadata establishes runnable capabilities.
type Capability string

const (
	Supervisor Capability = "supervisor"
	Filesystem Capability = "filesystem"
)

// Source opens independently positioned, validated artifact streams. Selection
// policy belongs to the caller: an invalid or incomplete official installation
// must not silently fall back to development compilation. In particular, merely
// lacking a pack does not establish that a controller is a development build.
type Source interface {
	Open(context.Context, Target) (*Artifact, error)
}

var _ Source = (*LocalSet)(nil)

// NewFilesystemBytes admits output from the dependency-free development helper
// compiler. It copies the input, checks the fixed helper package and executable
// metadata without running it, and hashes the exact returned bytes. Each call
// owns its stream and cursor; closing it never removes an operator pack file.
//
// buildID is the producer's embedded-source fingerprint and expected filesystem
// handshake value. It is not attested by executable metadata. The caller must
// verify it, the protocol, and the target during the authenticated handshake.
// This constructor never grants supervisor capability.
func NewFilesystemBytes(ctx context.Context, data []byte, target Target, buildID, protocol string) (*Artifact, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if !validDigest(buildID) || protocol == "" {
		return nil, fmt.Errorf("filesystem source fingerprint and protocol are required")
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("filesystem artifact is empty")
	}
	owned := bytes.Clone(data)
	reader := bytes.NewReader(owned)
	if err := inspectPackage(ctx, reader, target, "github.com/openclaw/crabbox/cmd/crabbox-runner"); err != nil {
		return nil, fmt.Errorf("inspect filesystem artifact: %w", err)
	}
	digest := sha256.Sum256(owned)
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	return &Artifact{stream: memoryArtifact{reader}, identity: Identity{
		Target: target, ProtocolVersion: protocol, Size: int64(len(owned)),
		SHA256: hex.EncodeToString(digest[:]), Capability: Filesystem, BuildID: buildID,
	}}, nil
}

type memoryArtifact struct{ *bytes.Reader }

func (memoryArtifact) Close() error { return nil }
