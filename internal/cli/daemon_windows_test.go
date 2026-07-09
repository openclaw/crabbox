//go:build windows

package cli

import (
	"reflect"
	"strings"
	"testing"
)

func TestWindowsTaskkillCommandUsesMinimalEnvironment(t *testing.T) {
	command := windowsTaskkillCommandWithEnvironment(42, []string{
		`SystemRoot=C:\Windows`,
		`windir=C:\Windows`,
		`ComSpec=C:\Windows\System32\cmd.exe`,
		`PATH=C:\untrusted`,
		`SCREEN_SHARING_PASSWORD=operator-secret`,
		`CRABBOX_EXTERNAL_API_KEY=provider-secret`,
	})
	wantArgs := []string{"taskkill", "/PID", "42", "/T", "/F"}
	if !reflect.DeepEqual(command.Args, wantArgs) {
		t.Fatalf("args=%v want %v", command.Args, wantArgs)
	}
	wantEnvironment := []string{
		`SYSTEMROOT=C:\Windows`,
		`WINDIR=C:\Windows`,
		`COMSPEC=C:\Windows\System32\cmd.exe`,
	}
	if !reflect.DeepEqual(command.Env, wantEnvironment) {
		t.Fatalf("environment=%v want %v", command.Env, wantEnvironment)
	}
	joined := strings.Join(command.Env, "\n")
	if strings.Contains(joined, "operator-secret") || strings.Contains(joined, "provider-secret") {
		t.Fatalf("taskkill environment leaked a secret: %v", command.Env)
	}
}

func TestWindowsTaskkillEnvironmentUsesLastCaseInsensitiveValue(t *testing.T) {
	got := windowsTaskkillEnvironment([]string{
		`SYSTEMROOT=C:\Old`,
		`systemroot=C:\Windows`,
		`WINDIR=`,
		`COMSPEC=C:\Windows\System32\cmd.exe`,
	})
	want := []string{
		`SYSTEMROOT=C:\Windows`,
		`COMSPEC=C:\Windows\System32\cmd.exe`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment=%v want %v", got, want)
	}
}
