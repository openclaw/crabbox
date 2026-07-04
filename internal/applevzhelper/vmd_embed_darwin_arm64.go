//go:build darwin && arm64 && vmdembed

package applevzhelper

import _ "embed"

// The release build embeds the Swift VMM daemon so a single helper binary is
// self-contained. Build the payload first (see vmd/) and pass -tags vmdembed:
//
//	swift build -c release --package-path vmd
//	cp vmd/.build/release/crabbox-apple-vm-vmd internal/applevzhelper/embedded/
//	go build -tags vmdembed ./cmd/crabbox-apple-vz-helper
//
//go:embed embedded/crabbox-apple-vm-vmd
var embeddedVMD []byte

func embeddedVMDPayload() []byte { return embeddedVMD }
