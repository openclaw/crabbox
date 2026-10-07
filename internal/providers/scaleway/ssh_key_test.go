package scaleway

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestSameSSHPublicKey(t *testing.T) {
	key := func() string {
		t.Helper()
		public, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ssh.NewPublicKey(public)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(parsed)))
	}
	first, second := key(), key()
	for _, tt := range []struct {
		name, left, right string
		want              bool
	}{
		{"same", first, first, true},
		{"comment removed", first + " crabbox lease", first, true},
		{"comment changed", first + " before", first + " after", true},
		{"whitespace", " \t" + strings.Replace(first, " ", "\t", 1) + " comment\n", first, true},
		{"different key", first, second, false},
		{"missing key", first, "", false},
		{"malformed keys", "ssh-ed25519 invalid", "ssh-ed25519 invalid", false},
		{"additional key", first + "\n" + second, first, false},
		{"authorized key options", "restrict " + first, first, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if sameSSHPublicKey(tt.left, tt.right) != tt.want || sameSSHPublicKey(tt.right, tt.left) != tt.want {
				t.Fatalf("key equivalence differs from want=%t", tt.want)
			}
		})
	}
}
