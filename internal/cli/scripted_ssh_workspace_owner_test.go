package cli

import "testing"

func installWorkspaceOwnerScriptedSSH(t *testing.T, dir string, behavior scriptedSSHBehavior) string {
	t.Helper()
	behavior.ConfigQueryPassthrough = true
	behavior.DecodePayload = true
	behavior.WorkspaceOwnerProtocol = true
	return installScriptedSSHExecutable(t, dir, behavior)
}
