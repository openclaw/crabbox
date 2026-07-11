//go:build darwin && arm64 && vmdembed

package applevmhelper

import _ "embed"

// The release build embeds the Swift VMM daemon so a single helper binary is
// self-contained. Credential-free source/snapshot builds use vmdembed and retain
// development ad-hoc signing. Official packaging must first sign and notarize
// the VMD, then pass both vmdembed and vmdrelease so runtime preserves its bytes:
//
//	swift build -c release --package-path vmd
//	cp vmd/.build/release/crabbox-apple-vm-vmd internal/applevmhelper/embedded/
//	go build -tags "vmdembed,vmdrelease" ./cmd/crabbox-apple-vm-helper
//
//go:embed embedded/crabbox-apple-vm-vmd
var embeddedVMD []byte

func embeddedVMDPayload() []byte { return embeddedVMD }
