package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestValidateZedTarget(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		target  SSHTarget
		wantErr string
	}{
		{name: "linux", target: SSHTarget{TargetOS: targetLinux}},
		{name: "macos", target: SSHTarget{TargetOS: targetMacOS}},
		{name: "config target", cfg: Config{TargetOS: targetLinux}},
		{name: "windows", target: SSHTarget{TargetOS: targetWindows}, wantErr: "Linux or macOS"},
		{name: "secret auth", target: SSHTarget{TargetOS: targetLinux, AuthSecret: true}, wantErr: "key-based SSH provider"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateZedTarget(test.cfg, test.target)
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error=%v want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestZedSSHCommandLineKeepsExecutableUnquoted(t *testing.T) {
	got := zedSSHCommandLine(SSHTarget{
		User: "alice",
		Host: "203.0.113.10",
		Port: "2222",
		Key:  "/tmp/key with spaces",
	})
	if !strings.HasPrefix(got, "ssh ") {
		t.Fatalf("command=%q should start with an unquoted ssh executable", got)
	}
	for _, want := range []string{"'-i' '/tmp/key with spaces'", "'-p' '2222'", "'alice@203.0.113.10'"} {
		if !strings.Contains(got, want) {
			t.Fatalf("command=%q missing %q", got, want)
		}
	}
}

func TestWriteZedInstructionsIncludesConnectionFolderAndKeepalive(t *testing.T) {
	var out bytes.Buffer
	writeZedInstructions(&out, SSHTarget{
		User: "alice",
		Host: "example.com",
		Port: "22",
	}, "/work/crabbox/my-app")
	got := out.String()
	for _, want := range []string{
		"Connect New Server",
		"Paste: ssh ",
		"Open: /work/crabbox/my-app",
		"Keep this process running to maintain lease activity",
		"press Ctrl-C",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("instructions missing %q:\n%s", want, got)
		}
	}
}
