package codesandbox

import (
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCodeSandboxWorkdirValidation(t *testing.T) {
	tests := []struct {
		name    string
		workdir string
		want    string
	}{
		{name: "default root allowed", workdir: "/project/workspace"},
		{name: "subdir allowed", workdir: "/project/workspace/app"},
		{name: "relative rejected", workdir: "workspace/app", want: "absolute"},
		{name: "outside workspace rejected", workdir: "/tmp/app", want: "under /project/workspace"},
		{name: "dotdot outside rejected", workdir: "/project/workspace/../secrets", want: "under /project/workspace"},
		{name: "broad project rejected", workdir: "/project", want: "too broad"},
		{name: "control rejected", workdir: "/project/workspace/app\nbad", want: "control"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newTestConfig()
			cfg.CodeSandbox.Workdir = tc.workdir
			got, err := codeSandboxWorkdir(cfg)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("codeSandboxWorkdir err=%v", err)
				}
				if got != strings.TrimRight(tc.workdir, "/") {
					t.Fatalf("got=%q want %q", got, tc.workdir)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestSpecAllowsArchiveSyncOptionsButRejectsUnsupportedDelegatedOptions(t *testing.T) {
	spec := Provider{}.Spec()
	if err := core.RejectDelegatedSyncOptionsForSpec(spec, RunRequest{SyncOnly: true}); err != nil {
		t.Fatalf("--sync-only should be allowed: %v", err)
	}
	if err := core.RejectDelegatedSyncOptionsForSpec(spec, RunRequest{ForceSyncLarge: true}); err != nil {
		t.Fatalf("--force-sync-large should be allowed: %v", err)
	}
	if err := core.RejectDelegatedSyncOptionsForSpec(spec, RunRequest{ChecksumSync: true}); err == nil {
		t.Fatal("--checksum should be rejected for delegated archive sync")
	}
	if err := core.RejectDelegatedSyncOptionsForSpec(spec, RunRequest{FullResync: true}); err == nil {
		t.Fatal("--full-resync should be rejected for delegated archive sync")
	}
}
