//go:build windows

package cli

import "os"

// Windows needs an audited ACL-based policy source before guest admission.
func guestPolicyFileOwnerTrusted(os.FileInfo) bool { return false }
