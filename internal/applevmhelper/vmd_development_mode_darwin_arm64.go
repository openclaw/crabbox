//go:build darwin && arm64 && !vmdrelease

package applevmhelper

// embeddedVMDIsReleasePayload is false for ordinary source and snapshot builds,
// including credential-free builds that use vmdembed for self-containment.
func embeddedVMDIsReleasePayload() bool { return false }
