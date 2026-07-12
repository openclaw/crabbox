//go:build darwin && arm64 && vmdembed && vmdrelease

package applevmhelper

// embeddedVMDIsReleasePayload is true only for the explicit official packaging
// tag pair. A bare vmdrelease tag intentionally has no implementation and fails
// the build rather than weakening the release trust boundary.
func embeddedVMDIsReleasePayload() bool { return true }
