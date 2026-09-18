package shared

import (
	"errors"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestRejectServiceRunOptions(t *testing.T) {
	// Each rejection also enables every later guard, pinning the full precedence.
	for _, tc := range []struct {
		name string
		req  core.RunRequest
		want string
	}{
		{name: "keep", req: core.RunRequest{Keep: true, Reclaim: true, SyncOnly: true, ChecksumSync: true, ForceSyncLarge: true, FullResync: true, ShellMode: true, EnvSummary: true}, want: "lifecycle belongs to the service; --keep is not supported"},
		{name: "reclaim", req: core.RunRequest{Reclaim: true, SyncOnly: true, ChecksumSync: true, ForceSyncLarge: true, FullResync: true, ShellMode: true, EnvSummary: true}, want: "lifecycle belongs to the service; --reclaim is not supported"},
		{name: "no sync", req: core.RunRequest{SyncOnly: true, ChecksumSync: true, ForceSyncLarge: true, FullResync: true, ShellMode: true, EnvSummary: true}, want: "does not support workspace sync; pass --no-sync"},
		{name: "sync only", req: core.RunRequest{NoSync: true, SyncOnly: true, ChecksumSync: true, ForceSyncLarge: true, FullResync: true, ShellMode: true, EnvSummary: true}, want: "does not support sync; --sync-only is rejected"},
		{name: "checksum", req: core.RunRequest{NoSync: true, ChecksumSync: true, ForceSyncLarge: true, FullResync: true, ShellMode: true, EnvSummary: true}, want: "does not support sync; --checksum is rejected"},
		{name: "force large", req: core.RunRequest{NoSync: true, ForceSyncLarge: true, FullResync: true, ShellMode: true, EnvSummary: true}, want: "does not support sync; --force-sync-large is rejected"},
		{name: "full resync", req: core.RunRequest{NoSync: true, FullResync: true, ShellMode: true, EnvSummary: true}, want: "does not support sync; --full-resync is rejected"},
		{name: "shell", req: core.RunRequest{NoSync: true, ShellMode: true, EnvSummary: true}, want: "runs its configured command; --shell is not supported"},
		{name: "env summary without env", req: core.RunRequest{NoSync: true, EnvSummary: true}, want: "cannot forward per-run environment variables"},
		{name: "later validation belongs to caller", req: core.RunRequest{NoSync: true}},
		{name: "implicit env", req: core.RunRequest{NoSync: true, Env: map[string]string{"CI": "true"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := RejectServiceRunOptions(tc.req, "example-service", "lifecycle belongs to the service", "runs its configured command")
			if tc.want == "" {
				if err != nil {
					t.Fatalf("admitted options rejected: %v", err)
				}
				return
			}
			var public core.ExitError
			want := "provider=example-service " + tc.want
			if !errors.As(err, &public) || public.Code != 2 || public.Message != want {
				t.Fatalf("err=%v, want exit2 %q", err, want)
			}
		})
	}
}
