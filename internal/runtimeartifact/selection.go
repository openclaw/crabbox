package runtimeartifact

import (
	"context"
	"errors"
	"fmt"
)

// ResolveSource selects one artifact source before guest access. An explicit or
// discovered pack is authoritative: any load/capability error is returned, never
// replaced by compilation. Only a development controller without a pack may
// select the explicitly supplied filesystem compiler. Supervisor selection never
// invokes that compiler and returns nil for its existing CLI-only shell route.
func ResolveSource(ctx context.Context, controllerPath, explicitManifest string, required Requirement, development Source) (Source, error) {
	return resolveSource(ctx, controllerPath, explicitManifest, distribution, required, development)
}

func resolveSource(ctx context.Context, controllerPath, explicitManifest, kind string, required Requirement, development Source) (Source, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if err := validateRequirement(required); err != nil {
		return nil, err
	}
	controller, manifest, err := discover(controllerPath, explicitManifest, kind)
	if err != nil {
		return nil, err
	}
	if manifest != "" {
		set, err := OpenCapabilitySet(ctx, manifest, controller, required)
		if err != nil {
			return nil, err
		}
		return set, nil
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if required.Capability == Supervisor {
		return nil, nil
	}
	if development == nil {
		return nil, fmt.Errorf("development filesystem runtime source is unavailable")
	}
	return &requiredSource{source: development, required: required}, nil
}

// The selected source must fulfill the same handshake expectation as a pack;
// selecting development policy does not let its producer change the request.
type requiredSource struct {
	source   Source
	required Requirement
}

func (s *requiredSource) Open(ctx context.Context, target Target) (*Artifact, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	artifact, err := s.source.Open(ctx, target)
	if err != nil {
		return nil, err
	}
	if artifact == nil {
		return nil, fmt.Errorf("runtime source returned no artifact")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, errors.Join(err, artifact.Close())
	}
	identity := artifact.Identity()
	if identity.Target != target || identity.Capability != s.required.Capability || identity.ProtocolVersion != s.required.ProtocolVersion || identity.BuildID != s.required.BuildID {
		return nil, errors.Join(fmt.Errorf("runtime source does not fulfill the requested capability identity"), artifact.Close())
	}
	return artifact, nil
}
