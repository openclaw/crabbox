package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestActionsRunnerRegistrationLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX guest fixture")
	}
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is required by the runner installer")
	}
	for _, org := range []bool{false, true} {
		t.Run(fmt.Sprintf("org=%t", org), func(t *testing.T) {
			root := t.TempDir()
			isolateRunTestUserDirs(t, root)
			setupClaimRoutingCommandTest(t, claimRoutingUsableProvider)
			repo := newWatchGitRepo(t)
			repo, err := filepath.EvalSymlinks(repo)
			if err != nil {
				t.Fatal(err)
			}
			t.Chdir(repo)
			const id = "cbx_1234abcd5678"
			if err := ClaimLeaseForRepoProvider(id, "", claimRoutingUsableProvider, repo, time.Minute, false); err != nil {
				t.Fatal(err)
			}
			claimPath, err := leaseClaimPath(id)
			if err != nil {
				t.Fatal(err)
			}
			bin, guest := filepath.Join(root, "bin"), filepath.Join(root, "guest")
			for _, dir := range []string{bin, filepath.Join(guest, "actions-runner", "bin")} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, body string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("FIXTURE_ROOT", root)
			t.Setenv("FIXTURE_CLAIM", claimPath)
			scope := "repos/example-org/my-app"
			if org {
				scope = "orgs/example-org"
			}
			t.Setenv("FIXTURE_SCOPE", scope)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			write(filepath.Join(bin, "ssh"), `#!/bin/sh
for arg do remote="$arg"; done
printf 'ssh\n' >>"$FIXTURE_ROOT/operations"
export HOME="$FIXTURE_ROOT/guest"
/bin/bash -c "$remote"
result=$?
case "$remote" in *'exec bash -s'*) if [ "${FIXTURE_LOST_ACK:-}" = 1 ]; then exit 255; fi;; esac
exit "$result"
`)
			write(filepath.Join(bin, "gh"), `#!/usr/bin/env python3
import json,os,sys
from pathlib import Path
r=Path(os.environ['FIXTURE_ROOT']); args=sys.argv[1:]
endpoint=next(a for a in args if a.startswith(('orgs/','repos/')))
assert endpoint.startswith(os.environ['FIXTURE_SCOPE']+'/actions/runners/') or endpoint==os.environ['FIXTURE_SCOPE']+'/actions/runners?per_page=1'
with (r/'operations').open('a') as f: f.write('gh '+' '.join(args)+'\n')
if 'registration-token' in ' '.join(args): print('fixture-secret-token'); sys.exit(0)
if '?per_page=1' in ' '.join(args):
 if os.environ.get('FIXTURE_READ_DENIED'): print('HTTP 403',file=sys.stderr); sys.exit(1)
 print('0'); sys.exit(0)
if not (r/'api').exists(): print('HTTP 404: Not Found',file=sys.stderr); sys.exit(1)
if 'DELETE' in args:
 if os.environ.get('FIXTURE_DELETE_DENIED'): print('HTTP 403',file=sys.stderr); sys.exit(1)
 (r/'api').unlink(); sys.exit(0)
if os.environ.get('FIXTURE_CANCEL_STAGE')=='verify':
 import http.client
 conn=http.client.HTTPConnection(os.environ['FIXTURE_CANCEL_ADDRESS'],timeout=5)
 conn.request('GET','/'); conn.getresponse().read(); conn.close()
print((r/'api').read_text())
`)
			write(filepath.Join(bin, "curl"), "#!/bin/sh\nprintf '{}'\n")
			write(filepath.Join(bin, "jq"), "#!/bin/sh\ncase \"$*\" in *tag_name*) printf 2.337.0;; *digest*) printf '"+strings.Repeat("a", 64)+"';; *) exec "+shellQuote(jq)+" \"$@\";; esac\n")
			write(filepath.Join(bin, "uname"), "#!/bin/sh\nprintf 'x86_64\\n'\n")
			write(filepath.Join(bin, "sudo"), "#!/bin/sh\nif [ \"$1\" = tee ]; then cat >/dev/null; elif [ \"$1\" = systemctl ]; then shift; exec systemctl \"$@\"; fi\n")
			write(filepath.Join(bin, "systemctl"), `#!/usr/bin/env python3
import json,os,sys
from pathlib import Path
r=Path(os.environ['FIXTURE_ROOT']); args=sys.argv[1:]
if args[0]=='show':
 print('LoadState=loaded\nActiveState='+('active' if (r/'active').exists() else 'inactive')+'\nEnvironment=\nEnvironmentFiles=\nDropInPaths=')
elif args[0] in ('enable','start'):
 guest=json.loads((r/'guest/actions-runner/.runner').read_text()); claim=json.loads(Path(os.environ['FIXTURE_CLAIM']).read_text())
 assert claim['actionsRunner']['runnerID']==guest['agentId'], 'listener started before durable receipt'
 (r/'active').touch()
 with (r/'operations').open('a') as f: f.write('started\n')
`)
			write(filepath.Join(guest, "actions-runner", ".crabbox-runner-version-2.337.0-x64-sha256-"+strings.Repeat("a", 64)), "")
			write(filepath.Join(guest, "actions-runner", "bin", "Runner.Listener"), "#!/bin/sh\nprintf '2.337.0\\n'\n")
			write(filepath.Join(guest, "actions-runner", "config.sh"), `#!/usr/bin/env python3
import json,os,sys
from pathlib import Path
r=Path(os.environ['FIXTURE_ROOT']); args=sys.argv[1:]
assert '--replace' not in args
assert not Path('.runner').exists()
ident=int((r/'next').read_text()) if (r/'next').exists() else 41
(r/'next').write_text(str(ident+1))
def value(flag,default=''): return args[args.index(flag)+1] if flag in args else default
data=dict(agentId=ident,agentName=value('--name'),gitHubUrl=value('--url'),poolName=value('--runnergroup','Default'),ephemeral='--ephemeral' in args,workFolder='_work')
Path('.runner').write_text(json.dumps(data)); Path('.credentials').write_text('{}')
(r/'api').write_text(json.dumps(dict(id=ident,name=data['agentName'],labels=[dict(name=x,type='custom') for x in value('--labels').split(',')],busy=False)))
with (r/'operations').open('a') as f: f.write('configured\n')
if os.environ.get('FIXTURE_CANCEL_STAGE')=='configure':
 import http.client
 conn=http.client.HTTPConnection(os.environ['FIXTURE_CANCEL_ADDRESS'],timeout=5)
 conn.request('GET','/'); conn.getresponse().read(); conn.close()
if os.environ.get('FIXTURE_PERSIST_FAIL'):
 claim=Path(os.environ['FIXTURE_CLAIM']); claim.rename(str(claim)+'.saved'); claim.mkdir()
`)
			var out, stderr bytes.Buffer
			app := App{Stdout: &out, Stderr: &stderr}
			args := []string{"actions", "register", "--id", id, "--version", "2.337.0"}
			if org {
				args = append(args, "--org", "example-org", "--runner-group", "release controls", "--ephemeral=false")
			} else {
				args = append(args, "--repo", "example-org/my-app")
			}
			run := func(argv ...string) {
				t.Helper()
				if err := app.Run(t.Context(), argv); err != nil {
					t.Fatalf("%v: %v\n%s", argv, err, stderr.String())
				}
			}
			invalid := append(append([]string{}, args...), "--runner-group", "invalid-without-org")
			wantError := "--runner-group requires --org"
			if org {
				invalid = append(append([]string{}, args...), "--repo", "example-org/my-app")
				wantError = "either --repo"
			}
			if err := app.Run(t.Context(), invalid); err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("scope flags: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "operations")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid scope reached transport: %v", err)
			}
			if !org {
				for _, reclaim := range []bool{true, false} {
					entered, resume := make(chan struct{}), make(chan struct{})
					waitCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					t.Cleanup(cancel)
					ctx := context.WithValue(waitCtx, claimRoutingTouchHookKey{}, func(req TouchRequest) (Server, error) {
						close(entered)
						select {
						case <-resume:
							return req.Lease.Server, nil
						case <-waitCtx.Done():
							return Server{}, waitCtx.Err()
						}
					})
					done := make(chan error, 1)
					go func() { done <- app.Run(ctx, args) }()
					select {
					case <-entered:
					case <-waitCtx.Done():
						t.Fatal("registration did not reach touch")
					}
					if reclaim {
						if err := ClaimLeaseForRepoProvider(id, "", claimRoutingUsableProvider, filepath.Join(repo, "other-owner"), time.Minute, true); err != nil {
							close(resume)
							<-done
							t.Fatal(err)
						}
					} else if err := WithDurableLeaseClaimLockContext(t.Context(), id, func(c *leaseClaim, _ bool, persist func() error) error {
						c.LastUsedAt = "2026-01-01T00:00:00Z"
						return persist()
					}); err != nil {
						close(resume)
						<-done
						t.Fatal(err)
					}
					close(resume)
					err := <-done
					cancel()
					if reclaim {
						if err == nil || !strings.Contains(err.Error(), "claim changed") {
							t.Fatalf("registration accepted a reclaimed target: %v", err)
						}
						if _, err := os.Stat(filepath.Join(root, "operations")); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("stale target reached SSH/GitHub: %v", err)
						}
						if err := ClaimLeaseForRepoProvider(id, "", claimRoutingUsableProvider, repo, time.Minute, true); err != nil {
							t.Fatal(err)
						}
					} else if err != nil {
						t.Fatalf("benign touch blocked registration: %v", err)
					}
				}
			}
			run(args...)
			created, err := ReadLeaseClaim(id)
			if err != nil || created.ActionsRunner == nil || created.ActionsRunner.Scope != scope || created.ActionsRunner.Ephemeral == org || org && created.ActionsRunner.Group != "release controls" {
				t.Fatalf("scope/group/mode receipt: %+v %v", created.ActionsRunner, err)
			}
			// Shipped claims did not record runner identity. Reconcile, do not replace.
			if err := WithDurableLeaseClaimLockContext(t.Context(), id, func(claim *leaseClaim, _ bool, persist func() error) error {
				claim.ActionsRunner = nil
				return persist()
			}); err != nil {
				t.Fatal(err)
			}
			run(args...) // Existing owned registration is reused, never replaced.
			data, _ := os.ReadFile(filepath.Join(root, "operations"))
			if strings.Count(string(data), "configured\n") != 1 {
				t.Fatalf("unexpected reconfiguration: %s", data)
			}
			if err := app.Run(t.Context(), append(append([]string{}, args...), "--labels", "different")); err == nil || !strings.Contains(err.Error(), "labels differ") {
				t.Fatalf("changed labels: %v", err)
			}
			if err := app.Run(t.Context(), append(append([]string{}, args...), "--version", "2.338.0")); err == nil || !strings.Contains(err.Error(), "requested version") {
				t.Fatalf("changed version: %v", err)
			}
			listener := filepath.Join(guest, "actions-runner", "bin", "Runner.Listener")
			if err := os.Chmod(listener, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := app.Run(t.Context(), args); err == nil || !strings.Contains(err.Error(), "requested version") {
				t.Fatalf("unreadable requested version: %v", err)
			}
			run(append(append([]string{}, args...), "--version", "latest")...)
			if err := os.Chmod(listener, 0o700); err != nil {
				t.Fatal(err)
			}
			// A consumed runner is reusable only once its owned listener has stopped.
			if err := os.Remove(filepath.Join(root, "api")); err != nil {
				t.Fatal(err)
			}
			if err := app.Run(t.Context(), args); err == nil || !strings.Contains(err.Error(), "still active") {
				t.Fatalf("active consumed runner: %v", err)
			}
			if err := os.Remove(filepath.Join(root, "active")); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FIXTURE_LOST_ACK", "1")
			run(args...) // Recover the exact configured ID after a lost SSH acknowledgement.
			t.Setenv("FIXTURE_LOST_ACK", "")
			data, _ = os.ReadFile(claimPath)
			var claim map[string]json.RawMessage
			if err := json.Unmarshal(data, &claim); err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(claim["actionsRunner"], []byte(`"runnerID": 42`)) {
				t.Fatalf("receipt not replaced: %s", claim["actionsRunner"])
			}
			apiPath := filepath.Join(root, "api")
			api, err := os.ReadFile(apiPath)
			if err != nil {
				t.Fatal(err)
			}
			write(apiPath, strings.Replace(string(api), `"id": 42`, `"id": 99`, 1))
			if err := app.Run(t.Context(), []string{"actions", "unregister", "--id", id}); err == nil || !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("different API identity: %v", err)
			}
			write(apiPath, string(api))
			if err := WithDurableLeaseClaimLockContext(t.Context(), id, func(c *leaseClaim, _ bool, persist func() error) error {
				c.ActionsRunner.LeaseFingerprint = "previous-resource"
				return persist()
			}); err != nil {
				t.Fatal(err)
			}
			if err := app.Run(t.Context(), args); err == nil || !strings.Contains(err.Error(), "still present") {
				t.Fatalf("changed resource reused registration: %v", err)
			}
			before, _ := os.ReadFile(filepath.Join(root, "operations"))
			run("actions", "unregister", "--id", id)
			t.Setenv("FIXTURE_READ_DENIED", "1")
			if err := app.Run(t.Context(), []string{"actions", "unregister", "--id", id}); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
				t.Fatalf("masked scope denial treated as absence: %v", err)
			}
			t.Setenv("FIXTURE_READ_DENIED", "")
			run("actions", "unregister", "--id", id)
			after, _ := os.ReadFile(filepath.Join(root, "operations"))
			if strings.Contains(string(after[len(before):]), "ssh\n") || strings.Count(string(after[len(before):]), "DELETE") != 1 {
				t.Fatalf("cleanup must use one exact API deletion: %s", after[len(before):])
			}
			retained, err := ReadLeaseClaim(id)
			if err != nil || retained.ActionsRunner == nil || retained.ActionsRunner.RunnerID != 42 {
				t.Fatalf("cleanup lost receipt: %v", err)
			}
			if err := os.Remove(filepath.Join(root, "active")); err != nil {
				t.Fatal(err)
			}
			for _, stage := range []string{"configure", "verify"} {
				ctx, cancel := context.WithCancel(t.Context())
				called := make(chan struct{}, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					called <- struct{}{}
					cancel()
					w.WriteHeader(http.StatusNoContent)
				}))
				t.Setenv("FIXTURE_CANCEL_STAGE", stage)
				t.Setenv("FIXTURE_CANCEL_ADDRESS", strings.TrimPrefix(server.URL, "http://"))
				err := app.Run(ctx, args)
				cancel()
				server.Close()
				t.Setenv("FIXTURE_CANCEL_STAGE", "")
				select {
				case <-called:
				default:
					t.Fatalf("%s fixture did not trigger cancellation: %v", stage, err)
				}
				canceled, readErr := ReadLeaseClaim(id)
				var registration struct {
					ID int64 `json:"id"`
				}
				api, apiErr := os.ReadFile(apiPath)
				if apiErr != nil || json.Unmarshal(api, &registration) != nil || readErr != nil || canceled.ActionsRunner == nil || canceled.ActionsRunner.RunnerID != registration.ID {
					t.Fatalf("cancel during %s lost cleanup receipt: claim=%+v read=%v api=%v", stage, canceled.ActionsRunner, readErr, apiErr)
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel during %s returned %v", stage, err)
				}
				if _, err := os.Stat(filepath.Join(root, "active")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cancel during %s started listener: %v", stage, err)
				}
				before, _ := os.ReadFile(filepath.Join(root, "operations"))
				run("actions", "unregister", "--id", id)
				after, _ := os.ReadFile(filepath.Join(root, "operations"))
				if strings.Contains(string(after[len(before):]), "ssh\n") || strings.Count(string(after[len(before):]), "DELETE") != 1 {
					t.Fatalf("canceled runner cleanup was not one exact API deletion: %s", after[len(before):])
				}
			}
			t.Setenv("FIXTURE_PERSIST_FAIL", "1")
			if org {
				t.Setenv("FIXTURE_DELETE_DENIED", "1")
			}
			t.Cleanup(func() {
				if _, err := os.Stat(claimPath + ".saved"); err == nil {
					_ = os.Remove(claimPath)
					_ = os.Rename(claimPath+".saved", claimPath)
				}
			})
			err = app.Run(t.Context(), args)
			if err == nil || !strings.Contains(err.Error(), "persist runner ") || !strings.Contains(err.Error(), "/45") {
				t.Fatalf("durable failure: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "active")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("listener started without receipt: %v", err)
			}
			_, apiErr := os.Stat(filepath.Join(root, "api"))
			if org && apiErr != nil || !org && !errors.Is(apiErr, os.ErrNotExist) {
				t.Fatalf("exact rollback outcome: %v", apiErr)
			}
			if org && !strings.Contains(err.Error(), "HTTP 403") {
				t.Fatalf("rollback error lost: %v", err)
			}
			for _, value := range []string{"fixture-secret-token"} {
				if strings.Contains(out.String()+stderr.String()+string(data), value) {
					t.Fatal("secret escaped transport")
				}
			}
		})
	}
}
