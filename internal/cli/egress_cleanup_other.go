//go:build !linux

package cli

import "context"

func stopLocalEgressClientSession(context.Context, string, string) error {
	return Exit(2, "egress client session cleanup requires Linux")
}
