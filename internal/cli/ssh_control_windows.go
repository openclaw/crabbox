//go:build windows

package cli

import "context"

func sshControlLeaseDirectory(SSHTarget) string                 { return "" }
func sshControlDirectory(string) string                         { return "" }
func ensureSSHControlDirectory(SSHTarget) error                 { return nil }
func closeLeaseSSHControlMasters(context.Context, string) error { return nil }
