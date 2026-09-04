package shared

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestPreparedShellEnvProfileRetainsPartialUploadCustody(t *testing.T) {
	profile, err := PrepareShellEnvProfile(map[string]string{"FIXTURE": "synthetic"}, "crabbox-fixture-env-")
	if err != nil {
		t.Fatal(err)
	}
	remoteFile := filepath.Join(t.TempDir(), "remote-profile")
	t.Cleanup(func() {
		_ = profile.Close(t.Context(), func(context.Context, string) error { return os.Remove(remoteFile) })
	})
	uploadErr := errors.New("partial upload")
	var local string
	err = profile.Upload(t.Context(), func(_ context.Context, localPath, remotePath string) error {
		local = localPath
		info, err := os.Stat(local)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("permissions=%v", info.Mode().Perm())
		}
		if remotePath != profile.RemotePath() {
			t.Fatal("wrong remote upload path")
		}
		if err := os.WriteFile(remoteFile, []byte("partial synthetic bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		return uploadErr
	})
	if !errors.Is(err, uploadErr) {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("local profile retained after upload: %v", err)
	}
	removed := 0
	remove := func(_ context.Context, path string) error {
		if path != profile.RemotePath() {
			t.Fatal("wrong cleanup path")
		}
		removed++
		return os.Remove(remoteFile)
	}
	if err := profile.Close(t.Context(), remove); err != nil {
		t.Fatal(err)
	}
	if err := profile.Close(t.Context(), remove); err != nil || removed != 1 {
		t.Fatalf("close=%v calls=%d", err, removed)
	}
	if _, err := os.Stat(remoteFile); !os.IsNotExist(err) {
		t.Fatalf("remote residue: %v", err)
	}
}

func TestPreparedShellEnvProfileBeforeUploadHasNoRemoteCustody(t *testing.T) {
	profile, err := PrepareShellEnvProfile(map[string]string{"FIXTURE": "value"}, "crabbox-fixture-env-")
	if err != nil {
		t.Fatal(err)
	}
	if err := profile.Close(t.Context(), func(context.Context, string) error { t.Fatal("unattempted remote upload cleaned"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := profile.Upload(t.Context(), func(context.Context, string, string) error { t.Fatal("closed profile uploaded"); return nil }); err == nil {
		t.Fatal("closed owner accepted upload")
	}
}

func TestShellEnvProfilesNativeIsolationAndSourceFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native fixture requires POSIX /tmp and Bash")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	root := t.TempDir()
	marker := filepath.Join(root, "must-not-run")
	values := []string{"quote' newline\n$(touch " + core.ShellQuote(marker) + ")", "second-profile"}
	profiles := make([]*PreparedShellEnvProfile, 0, len(values))
	for _, value := range values {
		profile, err := PrepareShellEnvProfile(map[string]string{"FIXTURE_VALUE": value, "EMPTY": "", "bad-name": "ignored"}, "crabbox-fixture-env-")
		if err != nil {
			t.Fatal(err)
		}
		profiles = append(profiles, profile)
		t.Cleanup(func() {
			_ = profile.Close(t.Context(), func(_ context.Context, path string) error { return os.Remove(path) })
		})
		if err := profile.Upload(t.Context(), func(_ context.Context, local, remote string) error {
			data, err := os.ReadFile(local)
			if err != nil {
				return err
			}
			return os.WriteFile(remote, data, 0o600)
		}); err != nil {
			t.Fatal(err)
		}
	}
	run := func(profilePath string, command []string) ([]byte, error) {
		wrapped := WrapCommandWithShellEnvProfile(command, profilePath)
		cmd := exec.Command(wrapped[0], wrapped[1:]...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "BASH_ENV=/dev/null"}
		return cmd.CombinedOutput()
	}
	for i, profile := range profiles {
		out, err := run(profile.RemotePath(), []string{"bash", "-lc", `printf '%s' "$FIXTURE_VALUE"`})
		if err != nil || string(out) != values[i] {
			t.Fatalf("profile%d execution failed: %v", i, err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("profile data executed as shell source")
	}
	if err := profiles[0].Close(t.Context(), func(_ context.Context, path string) error { return os.Remove(path) }); err != nil {
		t.Fatal(err)
	}
	if out, err := run(profiles[1].RemotePath(), []string{"bash", "-lc", `false; printf '%s' "$FIXTURE_VALUE"`}); err != nil || string(out) != values[1] {
		t.Fatalf("independent profile or user shell semantics lost: %v", err)
	}
	out, err := run(profiles[0].RemotePath(), []string{"bash", "-lc", "printf ran > " + core.ShellQuote(marker)})
	if err == nil || !strings.Contains(string(out), "No such file") {
		t.Fatalf("missing profile did not reject execution: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("command executed without required profile")
	}
}
