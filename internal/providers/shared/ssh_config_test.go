package shared

import (
	"bufio"
	"reflect"
	"strings"
	"testing"
)

func testSSHDirective(line string) (string, string) {
	key, value, _ := strings.Cut(strings.TrimSpace(line), " ")
	return key, strings.TrimSpace(value)
}

func TestGeneratedSSHConfigConnectionFieldsAndHostBoundaries(t *testing.T) {
	entries, err := ParseGeneratedSSHConfig(`User ignored
Host first 'second'
 HostName old
 hostname "new host"
 Port 2222
 User alice
 IdentityFile '/tmp/key file'
 UserKnownHostsFile "/tmp/hosts file"
 ProxyCommand tool --arg='raw value'
 Unknown ignored
Host
 User also-ignored
HOST last
 User bob
`, testSSHDirective)
	if err != nil {
		t.Fatal(err)
	}
	want := []GeneratedSSHConfigEntry{
		{Aliases: []string{"first", "second"}, HostName: "new host", Port: "2222", User: "alice", IdentityFile: "/tmp/key file", KnownHostsFile: "/tmp/hosts file", ProxyCommand: "tool --arg='raw value'"},
		{Aliases: []string{"last"}, User: "bob"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries = %#v, want %#v", entries, want)
	}
}

func TestGeneratedSSHConfigPreservesEmptyAndScannerFailureResults(t *testing.T) {
	entries, err := ParseGeneratedSSHConfig("User ignored\n", testSSHDirective)
	if err != nil || entries != nil {
		t.Fatalf("no hosts: %#v, %v", entries, err)
	}
	entries, err = ParseGeneratedSSHConfig("Host first\n"+strings.Repeat("x", bufio.MaxScanTokenSize+1), testSSHDirective)
	if err == nil || entries != nil {
		t.Fatalf("oversized line: %#v, %v", entries, err)
	}
}

func TestGeneratedSSHConfigUserValidation(t *testing.T) {
	for _, user := range []string{"alice", "_user", "a-b", "é"} {
		if !ValidSSHConfigUser(user) {
			t.Errorf("rejected user %q", user)
		}
	}
	for _, user := range []string{"", "-option", "a@host", "a b", "a\n", "a\x00", "a\u2003b"} {
		if ValidSSHConfigUser(user) {
			t.Errorf("accepted invalid user %q", user)
		}
	}
}
