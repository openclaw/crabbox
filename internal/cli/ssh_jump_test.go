package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestSSHJumpPinsBothHopsAndQuotesProxy(t *testing.T) {
	// The project helper selects the same private lease directory as production.
	isolateTestUserDirs(t)
	id := "cbx_001122334455"
	inner := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 42))
	outer := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 43))
	key := filepath.Join(t.TempDir(), "lease's key %h $(touch should-not-exist)")
	target := SSHTarget{Host: id, Port: "22", User: "crabbox", Key: key, HostKeyAlias: id, SSHHostKey: inner, SSHJump: &SSHJump{Host: "127.0.0.1", Port: 2222, HostKey: outer}}
	if err := prepareLeaseSSHTrust(&target, id); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target.KnownHostsFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Split(strings.TrimSpace(string(data)), "\n")) != 2 {
		t.Fatalf("expected both host keys: %s", data)
	}
	verify, err := knownhosts.New(target.KnownHostsFile)
	if err != nil {
		t.Fatal(err)
	}
	for address, value := range map[string]string{id + ":22": inner, target.SSHJump.HostKeyAlias + ":22": outer} {
		pub, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(value))
		if err := verify(address, testSSHAddr(address), pub); err != nil {
			t.Fatalf("pin %s: %v", address, err)
		}
	}
	if !strings.Contains(target.ProxyCommand, "%%h") || !strings.Contains(target.ProxyCommand, "StrictHostKeyChecking=yes") || !strings.Contains(target.ProxyCommand, "IdentityAgent=none") || !strings.Contains(target.ProxyCommand, id+":22") {
		t.Fatal(target.ProxyCommand)
	}
	if !bytes.Contains(data, []byte(target.SSHJump.HostKeyAlias)) || !strings.Contains(target.ProxyCommand, "HostKeyAlias="+target.SSHJump.HostKeyAlias) {
		t.Fatal(string(data))
	}
	info, _ := os.Stat(target.KnownHostsFile)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	for _, option := range []string{"StrictHostKeyChecking=yes", "GlobalKnownHostsFile=none"} {
		if !strings.Contains(strings.Join(sshBaseArgs(target), " "), option) {
			t.Fatal(option)
		}
	}
}

type testSSHAddr string

func (a testSSHAddr) Network() string { return "tcp" }
func (a testSSHAddr) String() string  { return string(a) }

func TestSSHJumpRejectsInvalidPinsAndEndpoint(t *testing.T) {
	for _, jump := range []SSHJump{{Host: "bad host", Port: 2222}, {Host: "127.0.0.1", Port: 65536}, {Host: "127.0.0.1", Port: 2222, HostKey: "ssh-ed25519 AAAA"}, {Host: "127.0.0.1", Port: 2222, HostKey: testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 43)), Fingerprint: "SHA256:wrong"}} {
		target := SSHTarget{Key: "lease-key", SSHJump: &jump}
		if err := prepareSSHJump(&target); err == nil {
			t.Fatalf("accepted %+v", jump)
		}
	}
}
