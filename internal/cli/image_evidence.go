package cli

import (
	"fmt"
	"io"
	"strings"
)

// ImageEvidence records the initial runtime image, not an attestation of the
// container's later writable filesystem.
type ImageEvidence struct {
	ConfiguredReference    string   `json:"configuredReference"`
	RuntimeImageID         string   `json:"runtimeImageId"`
	RepositoryDigests      []string `json:"repositoryDigests"`
	RepositoryDigestStatus string   `json:"repositoryDigestStatus"`
}

func CloneImageEvidence(evidence *ImageEvidence) *ImageEvidence {
	if evidence == nil {
		return nil
	}
	copy := *evidence
	copy.RepositoryDigests = append([]string{}, evidence.RepositoryDigests...)
	return &copy
}

func imageEvidenceSummary(evidence *ImageEvidence) string {
	if evidence == nil {
		return ""
	}
	return fmt.Sprintf("image configured_reference=%q runtime_image_id=%q repository_digest_status=%q repository_digests=%q", evidence.ConfiguredReference, evidence.RuntimeImageID, evidence.RepositoryDigestStatus, strings.Join(evidence.RepositoryDigests, ","))
}

func printImageEvidence(w io.Writer, evidence *ImageEvidence) {
	if w != nil && evidence != nil {
		fmt.Fprintln(w, imageEvidenceSummary(evidence))
	}
}
