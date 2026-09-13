package shared

import (
	"errors"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCleanPOSIXWorkspacePathPreservesDedicatedSubdirectories(t *testing.T) {
	for _, tc := range []struct {
		input string
		roots []string
		want  string
	}{
		{" /home/alice/repo/../work ", nil, "/home/alice/work"},
		{"/workspace", nil, "/workspace"},
		{"/workspace/repo", []string{"/workspace"}, "/workspace/repo"},
		{"/mnt/data/repo", []string{"/mnt", "/mnt/data"}, "/mnt/data/repo"},
		{"/tmp/repo", nil, "/tmp/repo"},
		{"/tmpish", nil, "/tmpish"},
	} {
		got, err := CleanPOSIXWorkspacePath("example workspace", tc.input, tc.roots...)
		if err != nil || got != tc.want {
			t.Fatalf("input=%q roots=%v got=%q err=%v", tc.input, tc.roots, got, err)
		}
	}
}

func TestCleanPOSIXWorkspacePathRejectsRootsWithExistingDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		input string
		roots []string
		want  string
	}{
		{"  ", nil, "example workspace is empty"},
		{" relative ", nil, `example workspace " relative " must resolve to an absolute path`},
		{"/tmp/..", nil, `example workspace "/" is too broad; choose a dedicated subdirectory`},
		{"/workspace/../root", nil, `example workspace "/root" is too broad; choose a dedicated subdirectory`},
		{"/workspace/home/", []string{"/workspace/home"}, `example workspace "/workspace/home" is too broad; choose a dedicated subdirectory`},
	} {
		got, err := CleanPOSIXWorkspacePath("example workspace", tc.input, tc.roots...)
		var exitErr core.ExitError
		if got != "" || !errors.As(err, &exitErr) || exitErr.Code != 2 || err.Error() != tc.want {
			t.Fatalf("input=%q got=%q err=%v", tc.input, got, err)
		}
	}
}
