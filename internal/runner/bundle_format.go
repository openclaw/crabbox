package runner

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/openclaw/crabbox/internal/runner/runnerwire"
)

const bundleMagic = "CBXRPK01"
const maxRunnerBytes = 32 << 20
const ReleaseRunnerTrustPolicyVersion = 1

// BundleBuildID is pinned to the source commit by protected release tooling.
// It is deliberately independent of the developer embedded-source fingerprint.
var BundleBuildID string

type bundleEntry struct {
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	PackedSHA256 string `json:"packedSha256"`
	PackedSize   int64  `json:"packedSize"`
	Offset       int64  `json:"offset"`
}

type bundleManifest struct {
	Version int           `json:"version"`
	BuildID string        `json:"buildId"`
	Entries []bundleEntry `json:"entries"`
}

func ArtifactFor(ctx context.Context, target Target) (Artifact, error) {
	if err := target.validate(); err != nil {
		return Artifact{}, err
	}
	if bundle := embeddedRunnerBundle(); bundle != nil {
		return artifactFromBundle(bundle, BundleBuildID, target)
	}
	if BundleBuildID != "" {
		return Artifact{}, errors.New("official runner build has no embedded bundle")
	}
	return DevelopmentArtifact(ctx, target)
}

func artifactFromBundle(data []byte, buildID string, target Target) (Artifact, error) {
	if len(data) < 12 || len(data) > 6*maxRunnerBytes+65548 || string(data[:8]) != bundleMagic {
		return Artifact{}, errors.New("invalid runner bundle magic")
	}
	headerSize := int(binary.BigEndian.Uint32(data[8:12]))
	if headerSize < 1 || headerSize > 64<<10 || headerSize > len(data)-12 {
		return Artifact{}, errors.New("invalid runner bundle header size")
	}
	var manifest bundleManifest
	decoder := json.NewDecoder(bytes.NewReader(data[12 : 12+headerSize]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Artifact{}, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Artifact{}, errors.New("trailing runner bundle metadata")
	}
	canonical, err := json.Marshal(manifest)
	if err != nil || !bytes.Equal(canonical, data[12:12+headerSize]) {
		return Artifact{}, errors.New("runner bundle metadata is not canonical")
	}
	idBytes, idErr := hex.DecodeString(buildID)
	if manifest.Version != 1 || idErr != nil || (len(idBytes) != 20 && len(idBytes) != 32) || strings.ToLower(buildID) != buildID || manifest.BuildID != buildID || len(manifest.Entries) != 6 {
		return Artifact{}, errors.New("runner bundle identity or inventory mismatch")
	}
	payload := data[12+headerSize:]
	var selected Artifact
	var offset int64
	for index := range manifest.Entries {
		entry := &manifest.Entries[index]
		entryTarget := Target{OS: entry.OS, Arch: entry.Arch}
		if err := entryTarget.validate(); err != nil {
			return Artifact{}, err
		}
		expected := Target{OS: []string{"darwin", "linux", "windows"}[index/2], Arch: []string{"amd64", "arm64"}[index%2]}
		if entryTarget != expected || entry.Offset != offset || entry.Size < 1 || entry.Size > maxRunnerBytes || entry.PackedSize < 18 || entry.PackedSize > maxRunnerBytes || entry.PackedSize > int64(len(payload))-offset {
			return Artifact{}, errors.New("invalid runner bundle member")
		}
		for _, digest := range []string{entry.SHA256, entry.PackedSHA256} {
			decoded, err := hex.DecodeString(digest)
			if err != nil || len(decoded) != sha256.Size || strings.ToLower(digest) != digest {
				return Artifact{}, errors.New("invalid runner bundle digest")
			}
		}
		packedData := payload[offset : offset+entry.PackedSize]
		packedDigest := sha256.Sum256(packedData)
		if hex.EncodeToString(packedDigest[:]) != entry.PackedSHA256 {
			return Artifact{}, errors.New("runner bundle member digest mismatch")
		}
		unpacked, err := unpackBundleMember(packedData, entry)
		if err != nil {
			return Artifact{}, err
		}
		if entryTarget == target {
			selected = Artifact{Identity: Identity{BuildID: manifest.BuildID, OS: target.OS, Arch: target.Arch, Protocol: runnerwire.Version}, SHA256: entry.SHA256, Data: unpacked}
		}
		offset += entry.PackedSize
	}
	if offset != int64(len(payload)) || selected.Data == nil {
		return Artifact{}, errors.New("runner bundle has trailing bytes or a missing target")
	}
	return selected, nil
}

func unpackBundleMember(data []byte, entry *bundleEntry) ([]byte, error) {
	if len(data) < 18 || binary.LittleEndian.Uint32(data[:4]) != 0x00088b1f {
		return nil, errors.New("noncanonical runner gzip header")
	}
	packed := bytes.NewReader(data)
	gz, err := gzip.NewReader(packed)
	if err != nil {
		return nil, err
	}
	gz.Multistream(false)
	unpacked, readErr := io.ReadAll(io.LimitReader(gz, entry.Size+1))
	closeErr := gz.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("decode runner bundle: %w", err)
	}
	if int64(len(unpacked)) != entry.Size || packed.Len() != 0 {
		return nil, errors.New("runner bundle decompressed size or trailer mismatch")
	}
	digest := sha256.Sum256(unpacked)
	if hex.EncodeToString(digest[:]) != entry.SHA256 {
		return nil, errors.New("runner raw identity mismatch")
	}
	return unpacked, nil
}
