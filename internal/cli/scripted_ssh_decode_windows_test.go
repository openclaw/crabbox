//go:build windows

package cli

import (
	"encoding/base64"
	"strings"
)

// decodeScriptedSSHPayloads unwraps nested launcher payloads, outermost first.
func decodeScriptedSSHPayloads(command string) []string {
	var decoded []string
	current := command
	for len(decoded) < remoteWorkspaceOwnerPayloadDepth {
		start := strings.Index(current, remoteWorkspaceOwnerPayloadPrefix)
		if start < 0 {
			break
		}
		rest := current[start+len(remoteWorkspaceOwnerPayloadPrefix):]
		end := strings.Index(rest, remoteWorkspaceOwnerPayloadSuffix)
		if end < 0 {
			break
		}
		payload, err := base64.StdEncoding.DecodeString(rest[:end])
		if err != nil {
			break
		}
		current = string(payload)
		decoded = append(decoded, current)
	}
	return decoded
}
