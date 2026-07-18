//go:build windows

package cli

import "testing"

func TestSSHProxyCommandWordsUseWindowsQuoting(t *testing.T) {
	got := sshProxyCommandWords([]string{"ssh", "-F", `C:\Users\O'Brien\ssh config`})
	want := []string{`"ssh"`, `"-F"`, `"C:\Users\O'Brien\ssh config"`}
	if len(got) != len(want) {
		t.Fatalf("words=%#v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("word %d=%q, want %q", index, got[index], want[index])
		}
	}
}
