package cli

func DelegatedRunArtifactScript(requiredGlobs, artifactGlobs []string, maxFiles int, maxBytes int64) string {
	return delegatedRunArtifactScript(requiredGlobs, artifactGlobs, maxFiles, maxBytes, false)
}
