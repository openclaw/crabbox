//go:build darwin && arm64 && !vmdembed

package applevmhelper

// Source builds without the vmdembed tag resolve the VMM daemon from a
// sibling binary, PATH, or the CRABBOX_APPLE_VM_VMD override instead.
func embeddedVMDPayload() []byte { return nil }
