package runtimeartifact

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Requirement is the handshake identity required by the consumer. Static
// inspection validates bytes and packaging, not implementation of the claim.
type Requirement struct {
	Capability      Capability
	ProtocolVersion string
	BuildID         string
}

// CapabilityClaim is an explicit producer assertion, verified again by the
// consumer's authenticated runtime handshake.
type CapabilityClaim struct {
	Name            Capability `json:"name"`
	ProtocolVersion string     `json:"protocolVersion"`
	BuildID         string     `json:"buildId,omitempty"`
}

type capabilityEntry struct {
	entry
	Capabilities []CapabilityClaim `json:"capabilities"`
}
type capabilityManifest struct {
	SchemaVersion    int               `json:"schemaVersion"`
	ControllerSHA256 string            `json:"controllerSha256"`
	Artifacts        []capabilityEntry `json:"artifacts"`
}

func validateRequirement(r Requirement) error {
	if r.ProtocolVersion == "" {
		return fmt.Errorf("expected protocol must not be empty")
	}
	switch r.Capability {
	case Supervisor:
		if r.BuildID != "" {
			return fmt.Errorf("supervisor build ID must be empty")
		}
	case Filesystem:
		if !validDigest(r.BuildID) {
			return fmt.Errorf("filesystem build ID must be a lowercase SHA-256")
		}
	default:
		return fmt.Errorf("unknown runtime capability %q", r.Capability)
	}
	return nil
}
func filesystemTarget(t Target) bool {
	return (t.OS == "linux" || t.OS == "darwin" || t.OS == "windows") && (t.Arch == "amd64" || t.Arch == "arm64")
}
func validateCapabilities(m *capabilityManifest) error {
	if m.SchemaVersion != 2 {
		return fmt.Errorf("unsupported runtime schema version %d", m.SchemaVersion)
	}
	if !validDigest(m.ControllerSHA256) {
		return fmt.Errorf("invalid controller SHA-256")
	}
	if len(m.Artifacts) == 0 {
		return fmt.Errorf("runtime manifest contains no artifacts")
	}
	seen := map[Target]bool{}
	for _, e := range m.Artifacts {
		t := Target{e.OS, e.Arch}
		if !filesystemTarget(t) {
			return fmt.Errorf("unsupported artifact target %s/%s", t.OS, t.Arch)
		}
		if seen[t] {
			return fmt.Errorf("duplicate artifact target %s/%s", t.OS, t.Arch)
		}
		seen[t] = true
		if !validPackPath(e.Path) {
			return fmt.Errorf("invalid artifact path %q", e.Path)
		}
		if e.Size <= 0 || !validDigest(e.SHA256) {
			return fmt.Errorf("invalid artifact size or SHA-256 for %s/%s", t.OS, t.Arch)
		}
		if len(e.Capabilities) == 0 {
			return fmt.Errorf("artifact contains no capabilities")
		}
		claims := map[Capability]bool{}
		for _, c := range e.Capabilities {
			if err := validateRequirement(Requirement{c.Name, c.ProtocolVersion, c.BuildID}); err != nil {
				return err
			}
			if claims[c.Name] {
				return fmt.Errorf("duplicate artifact capability %q", c.Name)
			}
			claims[c.Name] = true
			if c.Name == Supervisor && !supported(t) {
				return fmt.Errorf("supervisor requires a Linux target")
			}
		}
	}
	return nil
}

func parseCapabilityManifest(data []byte) (*capabilityManifest, error) {
	if err := exactFields(data, "schemaVersion", "controllerSha256", "artifacts"); err != nil {
		return nil, err
	}
	var raw struct {
		Artifacts []json.RawMessage `json:"artifacts"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	for _, e := range raw.Artifacts {
		if err := exactFields(e, "os", "arch", "path", "size", "sha256", "capabilities"); err != nil {
			return nil, err
		}
		var r struct {
			Capabilities []json.RawMessage `json:"capabilities"`
		}
		if err := json.Unmarshal(e, &r); err != nil {
			return nil, err
		}
		for _, c := range r.Capabilities {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(c, &fields); err != nil {
				return nil, err
			}
			names := []string{"name", "protocolVersion"}
			if _, ok := fields["buildId"]; ok {
				names = append(names, "buildId")
			}
			if err := exactFields(c, names...); err != nil {
				return nil, err
			}
		}
	}
	var m capabilityManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if err := validateCapabilities(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// OpenCapabilitySet snapshots a v2 local pack, binding it to this controller and
// selecting only explicit matching claims. It performs no executable invocation.
// Legacy v1 packs are admitted only for supervisor requests, under the same
// strict contract as OpenLocalSet.
func OpenCapabilitySet(ctx context.Context, manifestPath, controllerPath string, required Requirement) (*LocalSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRequirement(required); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, err
	}
	data, err := readManifestData(absolute)
	if err != nil {
		return nil, fmt.Errorf("runtime manifest: %w", err)
	}
	var header struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	if header.SchemaVersion == 1 {
		if required.Capability != Supervisor {
			return nil, fmt.Errorf("v1 runtime manifest does not claim filesystem capability")
		}
		legacy, err := parseManifest(data)
		if err != nil {
			return nil, err
		}
		if err := validateManifest(legacy, required.ProtocolVersion); err != nil {
			return nil, err
		}
		return bindLocalSet(ctx, absolute, controllerPath, legacy, required)
	}
	m, err := parseCapabilityManifest(data)
	if err != nil {
		return nil, fmt.Errorf("runtime manifest: %w", err)
	}
	normalized := &manifest{SchemaVersion: 2, ProtocolVersion: required.ProtocolVersion, ControllerSHA256: m.ControllerSHA256}
	for _, e := range m.Artifacts {
		for _, c := range e.Capabilities {
			if c.Name == required.Capability && c.ProtocolVersion == required.ProtocolVersion && c.BuildID == required.BuildID {
				normalized.Artifacts = append(normalized.Artifacts, e.entry)
			}
		}
	}
	if len(normalized.Artifacts) == 0 {
		return nil, fmt.Errorf("runtime manifest has no matching %s capability", required.Capability)
	}
	return bindLocalSet(ctx, absolute, controllerPath, normalized, required)
}

// CapabilityInput describes a finalized fixed-package runtime and its trusted
// producer claims. Partial operator packs are supported; official inventory
// completeness is enforced separately by the installation producer.
type CapabilityInput struct {
	Target       Target
	Path         string
	Capabilities []CapabilityClaim
}

// MarshalCapabilities prepares deterministic v2 local manifest bytes without
// executing files. Claims are not inferred from executable metadata or OS.
func MarshalCapabilities(ctx context.Context, manifestDir, controllerPath string, inputs []CapabilityInput) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(controllerPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	hash, err := fileDigest(ctx, f, 0)
	if err != nil {
		return nil, err
	}
	m := capabilityManifest{SchemaVersion: 2, ControllerSHA256: hash}
	for _, in := range inputs {
		if !filesystemTarget(in.Target) {
			return nil, fmt.Errorf("unsupported artifact target %s/%s", in.Target.OS, in.Target.Arch)
		}
		e, err := prepareExecutableEntry(ctx, manifestDir, ArtifactInput{in.Target, in.Path})
		if err != nil {
			return nil, err
		}
		claims := slices.Clone(in.Capabilities)
		slices.SortFunc(claims, func(a, b CapabilityClaim) int { return strings.Compare(string(a.Name), string(b.Name)) })
		m.Artifacts = append(m.Artifacts, capabilityEntry{e, claims})
	}
	if err := validateCapabilities(&m); err != nil {
		return nil, err
	}
	slices.SortFunc(m.Artifacts, func(a, b capabilityEntry) int {
		if n := strings.Compare(a.OS, b.OS); n != 0 {
			return n
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
