package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/openclaw/crabbox/internal/remoteruntime"
	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

func validBuildID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 32 && id == strings.ToLower(id)
}

func filesystemInputs(buildID string) []runtimeartifact.CapabilityInput {
	var inputs []runtimeartifact.CapabilityInput
	for _, platform := range []string{"darwin", "linux", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			name := platform + "-" + arch
			if platform == "windows" {
				name += ".exe"
			}
			claims := []runtimeartifact.CapabilityClaim{{Name: runtimeartifact.Filesystem, ProtocolVersion: "1", BuildID: buildID}}
			if platform == "linux" {
				claims = append(claims, runtimeartifact.CapabilityClaim{Name: runtimeartifact.Supervisor, ProtocolVersion: remoteruntime.Protocol})
			}
			inputs = append(inputs, runtimeartifact.CapabilityInput{Target: runtimeartifact.Target{OS: platform, Arch: arch}, Path: name, Capabilities: claims})
		}
	}
	return inputs
}

func prepareFilesystemPack(ctx context.Context, directory, controller, buildID string) ([]byte, error) {
	if !validBuildID(buildID) {
		return nil, fmt.Errorf("filesystem build ID must be a lowercase SHA-256")
	}
	return runtimeartifact.MarshalCapabilities(ctx, directory, controller, filesystemInputs(buildID))
}

func verifyFilesystemPack(ctx context.Context, directory, controller, buildID string) ([]byte, error) {
	expected, err := prepareFilesystemPack(ctx, directory, controller, buildID)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(directory, "manifest.json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	actual, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil {
		return nil, err
	}
	// Official producers write canonical bytes after signing. Comparing with the
	// fresh inspection binds every claim and rejects partial or extra inventories.
	if !bytes.Equal(actual, expected) {
		return nil, fmt.Errorf("finalized capability manifest does not match exact inventory and controller")
	}
	return expected, nil
}
