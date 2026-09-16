package localcontainer

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const imageEvidenceOutputLimit = 16 * 1024
const imageEvidenceObservationTimeout = 2 * time.Second

func (b *backend) observeImageEvidence(ctx context.Context, cfg core.Config, container inspectContainer, claim core.LeaseClaim) (*core.ImageEvidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if claim.CloudID == container.ID && container.Image != "" && claim.ImageEvidence != nil && claim.ImageEvidence.RuntimeImageID == container.Image {
		return core.CloneImageEvidence(claim.ImageEvidence), nil
	}
	evidence := &core.ImageEvidence{
		ConfiguredReference:    shared.FirstNonBlank(container.Config.Labels["image"], container.Config.Image),
		RuntimeImageID:         container.Image,
		RepositoryDigests:      []string{},
		RepositoryDigestStatus: "unknown",
	}
	if container.Image == "" {
		return evidence, nil
	}
	bounded, cancel := context.WithTimeout(ctx, imageEvidenceObservationTimeout)
	defer cancel()
	request := containerRuntimeRequest(cfg, []string{"image", "inspect", container.Image, "--format", "{{json .RepoDigests}}"})
	request.MaxCapturedOutputBytes = imageEvidenceOutputLimit
	result, err := b.rt.Exec.Run(bounded, request)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var digests []string
	if err != nil || bounded.Err() != nil || result.ExitCode != 0 || len(result.Stdout) > imageEvidenceOutputLimit || json.Unmarshal([]byte(result.Stdout), &digests) != nil || !canonicalRepositoryDigests(digests) {
		if b.rt.Stderr != nil {
			fmt.Fprintln(b.rt.Stderr, "warning: local-container repository digest observation failed; image evidence records unknown")
		}
		return evidence, nil
	}
	slices.Sort(digests)
	evidence.RepositoryDigests = append([]string{}, slices.Compact(digests)...)
	evidence.RepositoryDigestStatus = "unavailable"
	if len(evidence.RepositoryDigests) > 0 {
		evidence.RepositoryDigestStatus = "available"
	}
	return evidence, nil
}

func canonicalRepositoryDigests(digests []string) bool {
	for _, digest := range digests {
		name, value, ok := strings.Cut(digest, "@")
		algorithm, encoded, hasAlgorithm := strings.Cut(value, ":")
		if !ok || name == "" || !hasAlgorithm || algorithm == "" || encoded == "" || strings.Contains(value, "@") || strings.ContainsAny(digest, " \t\r\n\x00") {
			return false
		}
	}
	return true
}
