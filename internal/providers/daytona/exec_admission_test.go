package daytona

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestDaytonaExecAdmitsOnlyCompletedFixedClaims(t *testing.T) {
	for _, kind := range []string{"fixed", "ordinary", "ordinary with intent", "prepared fixed"} {
		t.Run(kind, func(t *testing.T) {
			f, b, req := newFixedDaytonaFixture(t)
			root, err := filepath.EvalSymlinks(req.Repo.Root)
			if err != nil {
				t.Fatal(err)
			}
			req.Repo.Root = root
			if strings.HasPrefix(kind, "ordinary") {
				req.RequestedLeaseID = ""
			}
			lease, err := b.Acquire(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "ordinary with intent" || kind == "prepared fixed" {
				claim, _, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
				if err != nil {
					t.Fatal(err)
				}
				changed := claim
				if kind == "ordinary with intent" {
					changed.FixedCreateIntent = &core.FixedCreateIntent{Version: 1, State: "acquired"}
				} else {
					intent := *claim.FixedCreateIntent
					intent.State = "prepared"
					changed.FixedCreateIntent = &intent
				}
				if _, err := core.ReplaceLeaseClaimIfUnchangedDurableAfter(lease.LeaseID, claim, changed, nil); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(req.Repo.Root)
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(configPath, []byte(fmt.Sprintf("provider: daytona\ntarget: linux\ndaytona:\n  apiUrl: %s\n  apiKey: test-credential\n", f.server.URL)), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			t.Setenv("CRABBOX_DAYTONA_API_KEY", "test-credential")
			bin := t.TempDir()
			// Parse private transport configuration with OpenSSH, but execute only
			// the caller's benign command locally. No SSH connection is opened.
			ssh := `#!/bin/sh
for arg do
  if [ "$arg" = -G ]; then exec /usr/bin/ssh "$@"; fi
done
for arg do command="$arg"; done
exec /bin/sh -c "$command"
`
			if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(ssh), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			before := len(f.paths)
			var out, diagnostic bytes.Buffer
			err = (core.App{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &diagnostic}).Run(t.Context(), []string{"exec", "--id", lease.LeaseID, "--", "printf", "fixed-exec"})
			if kind == "fixed" {
				if err != nil || out.String() != "fixed-exec" || len(f.paths) == before {
					t.Fatalf("fixed execution result=%q error=%v", out.String(), err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "exec requires a completed fixed-ID Daytona lease") {
				t.Fatalf("unsupported claim admission=%v", err)
			}
			if len(f.paths) != before || out.Len() != 0 {
				t.Fatal("unsupported claim reached provider access or SSH execution")
			}
		})
	}
}
