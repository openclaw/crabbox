package cli

import "regexp"

// CompileRunArtifactGlob shares the collector's Bash matcher semantics,
// including newlines in filenames selected by a recursive glob.
func CompileRunArtifactGlob(glob string) (*regexp.Regexp, error) {
	return regexp.Compile("(?s)" + artifactGlobRegex(glob))
}
