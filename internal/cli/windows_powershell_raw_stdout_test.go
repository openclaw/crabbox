//go:build windows

package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsRawStdoutCommandPreservesBinaryBytes(t *testing.T) {
	want := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0xff, 0xfe, 0x00, 0x80, '\n'}
	script := filepath.Join(t.TempDir(), "emit.ps1")
	body := `$o = [Console]::OpenStandardOutput()
$b = [byte[]](0x89,0x50,0x4e,0x47,0x0d,0x0a,0x1a,0x0a,0xff,0xfe,0x00,0x80,0x0a)
$o.Write($b, 0, $b.Length)
$o.Flush()
exit 7
`
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(windowsPowerShellFileCommand(script, true))
	cmd := exec.Command(fields[0], fields[1:]...)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 7 {
		t.Fatalf("exit = %v, want 7", err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("stdout = % x, want % x", out.Bytes(), want)
	}
}
