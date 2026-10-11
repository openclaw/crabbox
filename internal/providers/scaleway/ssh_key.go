package scaleway

import (
	"bytes"

	"golang.org/x/crypto/ssh"
)

// Scaleway drops authorized-key comments; compare the parsed key material.
func sameSSHPublicKey(left, right string) bool {
	l, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(left))
	if err != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return false
	}
	r, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(right))
	if err != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return false
	}
	return bytes.Equal(l.Marshal(), r.Marshal())
}
