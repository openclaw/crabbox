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

func filesystemInputs(buildID string, egress bool) []runtimeartifact.CapabilityInput {
	var inputs []runtimeartifact.CapabilityInput
	for _, platform := range []string{"darwin", "linux", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			name := platform + "-" + arch
			if platform == "windows" {
				name += ".exe"
			}
			claims := []runtimeartifact.CapabilityClaim{{Name: runtimeartifact.Filesystem, ProtocolVersion: "1", BuildID: buildID}}
			if platform == "linux" {
				if egress {
					claims = append([]runtimeartifact.CapabilityClaim{{Name: runtimeartifact.Egress, ProtocolVersion: runtimeartifact.EgressProtocol}}, claims...)
				}
				claims = append(claims, runtimeartifact.CapabilityClaim{Name: runtimeartifact.Supervisor, ProtocolVersion: remoteruntime.Protocol})
			}
			inputs = append(inputs, runtimeartifact.CapabilityInput{Target: runtimeartifact.Target{OS: platform, Arch: arch}, Path: name, Capabilities: claims})
		}
	}
	return inputs
}

func prepareFilesystemPack(ctx context.Context, directory, controller, buildID string, egress bool) ([]byte, error) {
	if !validBuildID(buildID) {
		return nil, fmt.Errorf("filesystem build ID must be a lowercase SHA-256")
	}
	return runtimeartifact.MarshalCapabilities(ctx, directory, controller, filesystemInputs(buildID, egress))
}

func verifyFilesystemPack(ctx context.Context, directory, controller, buildID string, egress bool) ([]byte, error) {
	expected, err := prepareFilesystemPack(ctx, directory, controller, buildID, egress)
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

func prepareEgressPack(ctx context.Context, directory, controller string) ([]byte, error) {
	var inputs []runtimeartifact.CapabilityInput
	for _, arch := range []string{"amd64", "arm64"} {
		inputs = append(inputs, runtimeartifact.CapabilityInput{
			Target: runtimeartifact.Target{OS: "linux", Arch: arch}, Path: "linux-" + arch,
			Capabilities: []runtimeartifact.CapabilityClaim{{Name: runtimeartifact.Egress, ProtocolVersion: runtimeartifact.EgressProtocol}, {Name: runtimeartifact.Supervisor, ProtocolVersion: remoteruntime.Protocol}},
		})
	}
	return runtimeartifact.MarshalCapabilities(ctx, directory, controller, inputs)
}

func verifyEgressPack(ctx context.Context, directory, controller string, stdout io.Writer) error {
	set, err := runtimeartifact.OpenCapabilitySet(ctx, filepath.Join(directory, "manifest.json"), controller,
		runtimeartifact.Requirement{Capability: runtimeartifact.Egress, ProtocolVersion: runtimeartifact.EgressProtocol})
	if err != nil {
		return err
	}
	for _, arch := range []string{"amd64", "arm64"} {
		artifact, err := set.Open(ctx, runtimeartifact.Target{OS: "linux", Arch: arch})
		if err != nil {
			return err
		}
		if err := artifact.Close(); err != nil {
			return err
		}
	}
	_, err = io.WriteString(stdout, "{\"egressProtocol\":\""+runtimeartifact.EgressProtocol+"\"}\n")
	return err
}
