package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// provideFixtureTimeout puts a stand-in timeout on a POSIX fixture's PATH when
// the host has no GNU timeout (stock macOS), so fixtures that run the runner
// installer locally reach its later steps. Guests keep the real deadlines.
func provideFixtureTimeout(t *testing.T, bin string) {
	t.Helper()
	if exec.Command("timeout", "--kill-after=1s", "10s", "true").Run() == nil {
		return
	}
	shim := "#!/bin/sh\n# Fixture stand-in for GNU timeout: drop its options and duration, then run the command.\n" +
		"while [ \"$#\" -gt 0 ]; do\n  case \"$1\" in\n    --*) shift ;;\n    *) break ;;\n  esac\ndone\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "timeout"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
}

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
			provideFixtureTimeout(t, bin)
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
 receipt=json.loads((r/'runner-receipt.json').read_text())
 assert receipt['runnerID']==guest['agentId'], 'listener started before portable receipt'
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
(r/'api').write_text(json.dumps(dict(id=ident,name=data['agentName'],labels=[dict(name=x,type='custom') for x in value('--labels').split(',')],status='online',busy=False)))
with (r/'operations').open('a') as f: f.write('configured\n')
if os.environ.get('FIXTURE_CANCEL_STAGE')=='configure':
 import http.client
 conn=http.client.HTTPConnection(os.environ['FIXTURE_CANCEL_ADDRESS'],timeout=5)
 conn.request('GET','/'); conn.getresponse().read(); conn.close()
if os.environ.get('FIXTURE_PERSIST_FAIL'):
 claim=Path(os.environ['FIXTURE_CLAIM']); claim.rename(str(claim)+'.saved'); claim.mkdir()
if os.environ.get('FIXTURE_CONFIG_GATE'):
 gate=Path(os.environ['FIXTURE_CONFIG_GATE'])
 with (gate/'entered').open('w') as f: f.write('configured\n')
 with (gate/'resume').open() as f: f.read()
`)
			var out, stderr bytes.Buffer
			app := App{Stdout: &out, Stderr: &stderr}
			receiptPath := filepath.Join(root, "runner-receipt.json")
			args := []string{"actions", "register", "--id", id, "--version", "2.337.0", "--receipt", receiptPath}
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
				// The guest install runs without the claim lock: a lease renewal
				// commits mid-install, and a reassignment mid-install refuses the
				// receipt, leaves the listener stopped and deletes the exact runner.
				// In the second case, hydration's heartbeat sees the reassignment
				// and cancels the operation before the receipt is rechecked.
				gate := filepath.Join(root, "gate")
				if err := os.Mkdir(gate, 0o700); err != nil {
					t.Fatal(err)
				}
				for _, fifo := range []string{"entered", "resume"} {
					if output, err := exec.Command("mkfifo", filepath.Join(gate, fifo)).CombinedOutput(); err != nil {
						t.Fatalf("mkfifo: %v %s", err, output)
					}
				}
				resume := func() {
					// A read-write open never blocks and lets the waiting installer see EOF.
					if f, err := os.OpenFile(filepath.Join(gate, "resume"), os.O_RDWR, 0); err == nil {
						_ = f.Close()
					}
				}
				t.Cleanup(resume)
				t.Setenv("FIXTURE_CONFIG_GATE", gate)
				for _, heartbeatCancels := range []bool{false, true} {
					entered := make(chan error, 1)
					go func() {
						_, err := os.ReadFile(filepath.Join(gate, "entered"))
						entered <- err
					}()
					done := make(chan error, 1)
					runCtx, cancelRun := context.WithCancelCause(t.Context())
					go func() { done <- app.Run(runCtx, args) }()
					select {
					case err := <-entered:
						if err != nil {
							resume()
							t.Fatal(err)
						}
					case err := <-done:
						t.Fatalf("registration finished before configuring: %v\n%s", err, stderr.String())
					case <-time.After(30 * time.Second):
						resume()
						t.Fatal("registration did not reach configuration")
					}
					renewCtx, cancelRenew := context.WithTimeout(t.Context(), 5*time.Second)
					renewErr := WithDurableLeaseClaimLockContext(renewCtx, id, func(c *leaseClaim, _ bool, persist func() error) error {
						c.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
						return persist()
					})
					cancelRenew()
					reclaimErr := ClaimLeaseForRepoProvider(id, "", claimRoutingUsableProvider, filepath.Join(repo, "other-owner"), time.Minute, true)
					if heartbeatCancels {
						cancelRun(Exit(4, "lease %s claim changed ownership during heartbeat", id))
					}
					resume()
					err := <-done
					cancelRun(nil)
					if renewErr != nil {
						t.Fatalf("lease renewal blocked while the guest install ran: %v", renewErr)
					}
					if reclaimErr != nil {
						t.Fatal(reclaimErr)
					}
					wantCause, wantCode := "claim changed during runner setup", 2
					if heartbeatCancels {
						wantCause, wantCode = "claim changed ownership during heartbeat", 4
					}
					if err == nil || !strings.Contains(err.Error(), wantCause) || !strings.Contains(err.Error(), "listener was not started") || ExitCodeForError(err, 0) != wantCode {
						t.Fatalf("heartbeat cancels=%t: registration after reassignment: %v", heartbeatCancels, err)
					}
					if _, err := os.Stat(filepath.Join(root, "active")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("listener started for a reassigned lease: %v", err)
					}
					if _, err := os.Stat(filepath.Join(root, "api")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("configured runner was not deleted: %v", err)
					}
					if data, _ := os.ReadFile(filepath.Join(root, "operations")); strings.Count(string(data), "DELETE") != 1 {
						t.Fatalf("rollback must delete the exact runner once: %s", data)
					}
					if err := ClaimLeaseForRepoProvider(id, "", claimRoutingUsableProvider, repo, time.Minute, true); err != nil {
						t.Fatal(err)
					}
					for _, path := range []string{filepath.Join(root, "operations"), filepath.Join(root, "next"), filepath.Join(guest, "actions-runner", ".runner"), filepath.Join(guest, "actions-runner", ".credentials")} {
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
					}
				}
				t.Setenv("FIXTURE_CONFIG_GATE", "")
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
			receipt, err := os.ReadFile(receiptPath)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(receipt, &fields); err != nil || len(fields) != 5 || fields["version"] != float64(1) || fields["leaseID"] != id || fields["scope"] != scope || fields["runnerID"] != float64(42) || fields["name"] == "" {
				t.Fatalf("portable receipt exposed unexpected fields: %s (%v)", receipt, err)
			}
			conflictPath := filepath.Join(root, "conflicting-receipt.json")
			write(conflictPath, strings.Replace(string(receipt), `"runnerID":42`, `"runnerID":99`, 1))
			if err := app.Run(t.Context(), []string{"actions", "unregister", "--receipt", conflictPath}); err == nil || !strings.Contains(err.Error(), "conflict") {
				t.Fatalf("conflicting local custody accepted: %v", err)
			}
			originalState := os.Getenv("XDG_STATE_HOME")
			t.Setenv("XDG_STATE_HOME", filepath.Join(root, "fresh-cleanup-state"))
			run("actions", "unregister", "--receipt", receiptPath)
			fresh, err := ReadLeaseClaim(id)
			if err != nil || fresh.LeaseID != "" {
				t.Fatalf("portable cleanup imported a lease claim: %+v %v", fresh, err)
			}
			t.Setenv("FIXTURE_READ_DENIED", "1")
			if err := app.Run(t.Context(), []string{"actions", "unregister", "--receipt", receiptPath}); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
				t.Fatalf("masked scope denial treated as absence: %v", err)
			}
			t.Setenv("FIXTURE_READ_DENIED", "")
			run("actions", "unregister", "--receipt", receiptPath)
			t.Setenv("XDG_STATE_HOME", originalState)
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
			unwritable := append(append([]string{}, args...), "--receipt", filepath.Join(root, "missing", "receipt.json"))
			if err := app.Run(t.Context(), unwritable); err == nil || !strings.Contains(err.Error(), "write cleanup receipt") {
				t.Fatalf("receipt publication failure: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "active")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("listener started without portable receipt: %v", err)
			}
			run("actions", "unregister", "--id", id)
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
			if err == nil || !strings.Contains(err.Error(), "persist runner ") || !strings.Contains(err.Error(), "/46") {
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
func actionsRunnerFixtureDirectory(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".actions-runner-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	dir, err = filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunActionsRunnerSetupReportsLastStageWithoutPrintingDiagnostics(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Time{})
	defer cancel()

	const fixtureValue = "fixture"
	var output bytes.Buffer
	result, err := runActionsRunnerSetup(ctx, &output, func(_, stderr io.Writer) error {
		fmt.Fprintln(stderr, actionsRunnerStagePrefix+"archive-download")
		fmt.Fprintln(stderr, "download failed with "+fixtureValue)
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want deadline exceeded", err)
	}
	if result.LastStage != "archive-download" {
		t.Fatalf("last stage=%q", result.LastStage)
	}
	if strings.Contains(output.String(), fixtureValue) {
		t.Fatalf("progress output exposed fixture: %q", output.String())
	}
	if got := redactActionsRunnerText(result.Diagnostics, fixtureValue); strings.Contains(got, fixtureValue) || got == result.Diagnostics {
		t.Fatalf("redacted diagnostics=%q", got)
	}
}

func TestRunGitHubRunnerHydrationDeadlineDuringRegistrationPreventsDispatch(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Time{})
	defer cancel()
	dispatched := false
	_, err := runGitHubRunnerHydration(
		ctx,
		"hydrate.yml",
		"main",
		"hydrate",
		[]string{"crabbox_job=hydrate"},
		time.Hour,
		io.Discard,
		githubRunnerHydrationHooks{
			Register: func(stageCtx context.Context) error {
				<-stageCtx.Done()
				return stageCtx.Err()
			},
			ClearState: func(context.Context) error {
				t.Fatal("clear ran after registration timeout")
				return nil
			},
			Inputs: func(context.Context) (map[string]bool, bool, error) {
				t.Fatal("input inspection ran after registration timeout")
				return nil, false, nil
			},
			Dispatch: func(context.Context, []string) error {
				dispatched = true
				return nil
			},
			Wait: func(context.Context, string) (actionsHydrationState, error) {
				t.Fatal("marker wait ran after registration timeout")
				return actionsHydrationState{}, nil
			},
		},
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want deadline exceeded", err)
	}
	if dispatched {
		t.Fatal("workflow dispatched after registration timeout")
	}
}

// hydrateWithClock drives the shared GitHub-runner orchestration, used by Linux,
// WSL2, and native Windows targets alike, through the real marker-wait loop on
// a fake clock. Setup takes setup; the marker appears markerAfter dispatch.
func hydrateWithClock(t *testing.T, waitTimeout, setup, markerAfter time.Duration) (actionsHydrationState, []time.Time, time.Time, error) {
	t.Helper()
	start := time.Now()
	clock := start
	var setupDeadlines []time.Time
	recordSetup := func(stageCtx context.Context) {
		deadline, ok := stageCtx.Deadline()
		if !ok {
			t.Fatal("setup stage ran without a deadline")
		}
		setupDeadlines = append(setupDeadlines, deadline)
	}
	state, err := runGitHubRunnerHydration(
		context.Background(),
		"hydrate.yml",
		"main",
		"hydrate",
		[]string{"crabbox_id=cbx_test", "crabbox_runner_label=runner", "crabbox_keep_alive_minutes=90", "crabbox_job=hydrate"},
		waitTimeout,
		io.Discard,
		githubRunnerHydrationHooks{
			Now: func() time.Time { return clock },
			Register: func(stageCtx context.Context) error {
				recordSetup(stageCtx)
				clock = clock.Add(setup)
				return nil
			},
			ClearState: func(stageCtx context.Context) error { recordSetup(stageCtx); return nil },
			Inputs: func(stageCtx context.Context) (map[string]bool, bool, error) {
				recordSetup(stageCtx)
				return map[string]bool{"crabbox_id": true, "crabbox_runner_label": true, "crabbox_keep_alive_minutes": true, "crabbox_job": true}, true, nil
			},
			Dispatch: func(stageCtx context.Context, _ []string) error { recordSetup(stageCtx); return nil },
			Wait: func(stageCtx context.Context, expectedJob string) (actionsHydrationState, error) {
				markerAt := clock.Add(markerAfter)
				deadline, ok := stageCtx.Deadline()
				if !ok {
					t.Fatal("marker wait ran without a deadline")
				}
				return waitForActionsHydrationWithPoll(stageCtx, "cbx_test", expectedJob, io.Discard,
					func(context.Context) (actionsHydrationState, error) {
						if clock.Before(markerAt) {
							return actionsHydrationState{}, nil
						}
						return actionsHydrationState{Workspace: "/work/cbx_test/repo", Job: expectedJob}, nil
					},
					// Mirrors waitForActionsPoll: one 10s interval, or the deadline if it comes first.
					func(context.Context) error {
						if next := clock.Add(10 * time.Second); next.Before(deadline) {
							clock = next
							return nil
						}
						clock = deadline
						return context.DeadlineExceeded
					},
				)
			},
		},
	)
	return state, setupDeadlines, start, err
}

func TestRunGitHubRunnerHydrationSlowSetupKeepsFullMarkerWait(t *testing.T) {
	const waitTimeout = 20 * time.Minute
	// Eight minutes of setup plus a marker 15 minutes after dispatch succeeds
	// when --wait-timeout bounds the marker wait, as it always has.
	state, setupDeadlines, start, err := hydrateWithClock(t, waitTimeout, 8*time.Minute, 15*time.Minute)
	if err != nil {
		t.Fatalf("8m setup + 15m marker wait failed under a 20m --wait-timeout: %v", err)
	}
	if state.Workspace != "/work/cbx_test/repo" {
		t.Fatalf("state=%+v", state)
	}
	if len(setupDeadlines) != 4 {
		t.Fatalf("setup stages=%d want 4", len(setupDeadlines))
	}
	for _, deadline := range setupDeadlines {
		if !deadline.Equal(start.Add(waitTimeout)) {
			t.Fatalf("setup deadline=%s want %s: setup must stay bounded by one --wait-timeout", deadline, start.Add(waitTimeout))
		}
	}
}

func TestRunGitHubRunnerHydrationMarkerWaitStillTimesOut(t *testing.T) {
	_, _, _, err := hydrateWithClock(t, 20*time.Minute, time.Minute, 21*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for GitHub Actions hydration marker") {
		t.Fatalf("error=%v, want marker-wait timeout", err)
	}
}

func TestRunGitHubRunnerHydrationSetupBoundedByWaitTimeout(t *testing.T) {
	dispatched := false
	_, err := runGitHubRunnerHydration(context.Background(), "hydrate.yml", "main", "hydrate", nil, time.Nanosecond, io.Discard, githubRunnerHydrationHooks{
		Register: func(stageCtx context.Context) error {
			<-stageCtx.Done()
			return stageCtx.Err()
		},
		ClearState: func(context.Context) error { t.Fatal("clear ran after setup timeout"); return nil },
		Inputs: func(context.Context) (map[string]bool, bool, error) {
			t.Fatal("input inspection ran after setup timeout")
			return nil, false, nil
		},
		Dispatch: func(context.Context, []string) error { dispatched = true; return nil },
		Wait: func(context.Context, string) (actionsHydrationState, error) {
			t.Fatal("marker wait ran after setup timeout")
			return actionsHydrationState{}, nil
		},
	})
	if !errors.Is(err, context.DeadlineExceeded) || dispatched {
		t.Fatalf("error=%v dispatched=%t, want setup deadline before dispatch", err, dispatched)
	}
}

func TestActionsCommandsRejectNonPositiveWaitTimeout(t *testing.T) {
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	for name, run := range map[string]func() error{
		"hydrate": func() error {
			return app.actionsHydrate(context.Background(), []string{"--wait-timeout", "0"})
		},
		"register": func() error {
			return app.actionsRegister(context.Background(), []string{"--wait-timeout", "0"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := run()
			if err == nil || !strings.Contains(err.Error(), "--wait-timeout must be greater than zero") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestGitHubActionsRunnerInstallPowerShellScriptParses(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell parser is required")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell is required")
	}
	scriptPath := filepath.Join(actionsRunnerFixtureDirectory(t), "runner-install.ps1")
	if err := os.WriteFile(scriptPath, []byte(githubActionsRunnerInstallPowerShellScript("latest", true)), 0o600); err != nil {
		t.Fatal(err)
	}
	command := `$syntaxItems = $null
$errors = $null
[System.Management.Automation.Language.Parser]::ParseFile($env:CRABBOX_RUNNER_SCRIPT, [ref]$syntaxItems, [ref]$errors) | Out-Null
if ($errors.Count -gt 0) {
  $errors | ForEach-Object { $_.Message }
  exit 1
}`
	cmd := exec.Command(powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command)
	cmd.Env = append(os.Environ(), "CRABBOX_RUNNER_SCRIPT="+scriptPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated Windows runner installer does not parse: %v\n%s", err, output)
	}
}

// A wedged config.cmd must not outlive its deadline: the timeout stops the job
// and every process it started (here a grandchild that would otherwise sleep
// for ten minutes), reports the timeout, and keeps a log without the token.
func TestGitHubActionsRunnerConfigPowerShellTimeoutStopsProcessTreeAndRedactsLog(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell is required")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell is required")
	}
	runnerDir := actionsRunnerFixtureDirectory(t)
	childPIDPath := filepath.Join(runnerDir, "child.pid")
	t.Cleanup(func() {
		// Never leave the sleeping grandchild behind, even when the test fails.
		if data, err := os.ReadFile(childPIDPath); err == nil {
			_ = exec.Command("taskkill", "/PID", strings.TrimSpace(string(data)), "/T", "/F").Run()
		}
	})
	configCmd := "@echo off\r\necho config-started %RUNNER_TOKEN%\r\n" +
		"powershell.exe -NoLogo -NoProfile -NonInteractive -Command \"[IO.File]::WriteAllText($env:CHILD_PID_PATH, [string]$PID); Start-Sleep -Seconds 600\"\r\n"
	if err := os.WriteFile(filepath.Join(runnerDir, "config.cmd"), []byte(configCmd), 0o600); err != nil {
		t.Fatal(err)
	}
	testScript := `$ErrorActionPreference = "Stop"
$runnerDir = $env:CRABBOX_RUNNER_TEST_DIR
Set-Location -LiteralPath $runnerDir
$env:RUNNER_TOKEN = "fixture-registration-value"
` + githubActionsRunnerConfigPowerShell() + `
$failure = ""
try {
  Invoke-CrabboxRunnerConfig -Arguments @("--unattended") -TimeoutSeconds 20 -LogName "crabbox-config.log"
} catch {
  $failure = $_.Exception.Message
}
"failure=$failure"
$childPid = if (Test-Path -LiteralPath $env:CHILD_PID_PATH) { [int](Get-Content -Raw -LiteralPath $env:CHILD_PID_PATH) } else { 0 }
"childStarted=$($childPid -ne 0)"
"childAlive=$($childPid -ne 0 -and $null -ne (Get-Process -Id $childPid -ErrorAction SilentlyContinue))"
"witnesses=$(@(Get-ChildItem -LiteralPath $runnerDir -Filter 'crabbox-config-*.pid').Count)"
`
	scriptPath := filepath.Join(runnerDir, "runner-config-timeout.ps1")
	if err := os.WriteFile(scriptPath, []byte(testScript), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath)
	// A PowerShell 7 PSModulePath inherited from the test host breaks Windows
	// PowerShell's job cmdlets.
	cmd.Env = append(withoutEnv(os.Environ(), "PSModulePath"), "CRABBOX_RUNNER_TEST_DIR="+runnerDir, "CHILD_PID_PATH="+childPIDPath)
	out, err := cmd.CombinedOutput()
	output := strings.ReplaceAll(string(out), "\r\n", "\n")
	if err != nil {
		t.Fatalf("configuration timeout run failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, "childStarted=True\n") {
		t.Fatalf("config.cmd did not start its child before the deadline:\n%s", output)
	}
	for _, want := range []string{"failure=config.cmd timed out after 20 seconds; log=", "childAlive=False\n", "witnesses=0\n"} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q after the configuration timeout:\n%s", want, output)
		}
	}
	log, err := os.ReadFile(filepath.Join(runnerDir, "crabbox-config.log"))
	if err != nil {
		t.Fatalf("read configuration timeout log: %v", err)
	}
	if !strings.Contains(string(log), "config-started") || strings.Contains(string(log), "fixture-registration-value") {
		t.Fatalf("configuration timeout log lost output or kept the token: %q", log)
	}
}

func TestActionsRunnerBootstrapContextReservesDiagnosticTime(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelParent()
	bootstrap, cancelBootstrap := actionsRunnerBootstrapContext(parent)
	defer cancelBootstrap()
	parentDeadline, parentOK := parent.Deadline()
	bootstrapDeadline, bootstrapOK := bootstrap.Deadline()
	if !parentOK || !bootstrapOK {
		t.Fatal("expected both contexts to have deadlines")
	}
	if !bootstrapDeadline.Before(parentDeadline) {
		t.Fatalf("bootstrap deadline=%s parent deadline=%s", bootstrapDeadline, parentDeadline)
	}
}

func TestReadGitHubActionsRunnerDiagnosticsSkipsExpiredBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := readGitHubActionsRunnerDiagnosticsBestEffort(ctx, SSHTarget{})
	if !strings.Contains(got, "timeout budget exhausted") {
		t.Fatalf("diagnostics=%q", got)
	}
}

func TestWaitForGitHubActionsRunnerOnlineRetriesUntilOnline(t *testing.T) {
	repo := "repos/example-org/my-app"
	calls := 0
	var output bytes.Buffer
	err := waitForGitHubActionsRunnerOnlineWithPoll(
		context.Background(),
		repo,
		"runner-one",
		func(context.Context) (githubActionsRunnerState, error) {
			calls++
			if calls < 3 {
				return githubActionsRunnerState{Found: true, Status: "offline"}, nil
			}
			return githubActionsRunnerState{Found: true, Status: "online"}, nil
		},
		&output,
		func(context.Context) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("lookup calls=%d, want 3", calls)
	}
	if !strings.Contains(output.String(), "status=offline") || !strings.Contains(output.String(), "actions runner online") {
		t.Fatalf("unexpected progress output: %q", output.String())
	}
}

func TestWaitForGitHubActionsRunnerOnlineAcceptsBusyRunner(t *testing.T) {
	repo := "repos/example-org/my-app"
	calls := 0
	var output bytes.Buffer
	err := waitForGitHubActionsRunnerOnlineWithPoll(
		context.Background(),
		repo,
		"runner-busy",
		func(context.Context) (githubActionsRunnerState, error) {
			calls++
			return githubActionsRunnerState{Found: true, Status: "online", Busy: true}, nil
		},
		&output,
		func(context.Context) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("lookup calls=%d, want 1: a busy online runner already accepted work", calls)
	}
	if !strings.Contains(output.String(), "busy=true") {
		t.Fatalf("busy runner was not reported: %q", output.String())
	}
}

func TestWaitForGitHubActionsRunnerOnlineTimesOutWithLastStatus(t *testing.T) {
	err := waitForGitHubActionsRunnerOnlineWithPoll(
		context.Background(),
		"repos/example-org/my-app",
		"runner-two",
		func(context.Context) (githubActionsRunnerState, error) {
			return githubActionsRunnerState{Found: true, Status: "offline"}, nil
		},
		io.Discard,
		func(context.Context) error { return context.DeadlineExceeded },
	)
	if err == nil || !strings.Contains(err.Error(), "last status=offline") {
		t.Fatalf("error=%v, want offline timeout", err)
	}
}

func TestWaitForActionsHydrationIncludesLastProbeError(t *testing.T) {
	_, err := waitForActionsHydrationWithPoll(
		context.Background(),
		"cbx_test",
		"hydrate",
		io.Discard,
		func(context.Context) (actionsHydrationState, error) {
			return actionsHydrationState{}, errors.New("ssh probe failed")
		},
		func(context.Context) error { return context.DeadlineExceeded },
	)
	if err == nil || !strings.Contains(err.Error(), "last SSH probe error: ssh probe failed") {
		t.Fatalf("error=%v", err)
	}
}

func TestWaitForActionsHydrationClearsRecoveredProbeError(t *testing.T) {
	calls := 0
	_, err := waitForActionsHydrationWithPoll(
		context.Background(),
		"cbx_test",
		"hydrate",
		io.Discard,
		func(context.Context) (actionsHydrationState, error) {
			calls++
			if calls == 1 {
				return actionsHydrationState{}, errors.New("temporary ssh failure")
			}
			return actionsHydrationState{}, nil
		},
		func(context.Context) error {
			if calls > 1 {
				return context.DeadlineExceeded
			}
			return nil
		},
	)
	if err == nil {
		t.Fatal("expected marker timeout")
	}
	if strings.Contains(err.Error(), "temporary ssh failure") {
		t.Fatalf("timeout retained recovered probe error: %v", err)
	}
}

func TestRunGitHubRunnerHydrationCancellationBetweenStagesStopsFurtherWork(t *testing.T) {
	for _, stage := range []string{"register", "clear", "inputs", "dispatch"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls []string
			record := func(name string) {
				if ctx.Err() != nil {
					t.Fatalf("stage %s ran after cancellation during %s", name, stage)
				}
				calls = append(calls, name)
				if name == stage {
					cancel()
				}
			}
			_, err := runGitHubRunnerHydration(ctx, "hydrate.yml", "main", "hydrate", nil, time.Hour, io.Discard, githubRunnerHydrationHooks{
				Register:   func(context.Context) error { record("register"); return nil },
				ClearState: func(context.Context) error { record("clear"); return nil },
				Inputs: func(context.Context) (map[string]bool, bool, error) {
					record("inputs")
					return nil, false, nil
				},
				Dispatch: func(context.Context, []string) error { record("dispatch"); return nil },
				Wait: func(context.Context, string) (actionsHydrationState, error) {
					t.Fatal("marker wait ran after cancellation")
					return actionsHydrationState{}, nil
				},
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v, calls=%v", err, calls)
			}
			if len(calls) == 0 || calls[len(calls)-1] != stage {
				t.Fatalf("unexpected stages: %v", calls)
			}
		})
	}
}

func TestBoundedActionsRunnerOnlineContextAddsDeadlineOnlyWhenMissing(t *testing.T) {
	ctx, cancel := boundedActionsRunnerOnlineContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > defaultActionsWaitTimeout {
		t.Fatalf("deadline = %v, %t", deadline, ok)
	}
	parent, cancelParent := context.WithTimeout(context.Background(), time.Minute)
	defer cancelParent()
	want, _ := parent.Deadline()
	child, cancelChild := boundedActionsRunnerOnlineContext(parent)
	defer cancelChild()
	if got, _ := child.Deadline(); !got.Equal(want) {
		t.Fatalf("existing deadline replaced: %v != %v", got, want)
	}
}

func TestActionsRunnerReservationHoldsAcrossLeaseRenewalOnly(t *testing.T) {
	binding := &ActionsRunnerBinding{Scope: "repos/example-org/my-app", RunnerID: 7, Name: "old"}
	reserved := leaseClaim{
		LeaseID: "cbx_reserve000001", RepoRoot: "/repo", Provider: "hyperv", ProviderScope: "instance:test",
		LastUsedAt: "2026-01-01T00:00:00Z", Revision: "r1", ActionsRunner: binding,
		Labels: map[string]string{"state": "ready", "expires_at": "2026-01-01T00:30:00Z", "slug": "pace"},
	}
	change := func(edit func(*leaseClaim)) leaseClaim {
		next := cloneLeaseClaim(reserved)
		edit(&next)
		return next
	}
	renewed := change(func(c *leaseClaim) {
		c.LastUsedAt, c.Revision = "2026-01-01T00:05:00Z", "r2"
		c.Labels["expires_at"], c.Labels["last_touched_at"] = "2026-01-01T00:35:00Z", "2026-01-01T00:05:00Z"
	})
	for _, tc := range []struct {
		name    string
		current leaseClaim
		exists  bool
		strict  bool
		want    bool
	}{
		{"unchanged strict", reserved, true, true, true},
		{"heartbeat renewal strict", renewed, true, true, true},
		{"claim released", reserved, false, false, false},
		{"reassigned workspace", change(func(c *leaseClaim) { c.RepoRoot = "/other" }), true, false, false},
		{"replaced VM", change(func(c *leaseClaim) { c.CloudImmutableID = "vm-2" }), true, false, false},
		{"competing receipt", change(func(c *leaseClaim) {
			c.ActionsRunner = &ActionsRunnerBinding{Scope: binding.Scope, RunnerID: 8, Name: "other"}
		}), true, false, false},
		{"non-renewal label strict", change(func(c *leaseClaim) { c.Labels["slug"] = "renamed" }), true, true, false},
		{"non-renewal label relaxed", change(func(c *leaseClaim) { c.Labels["slug"] = "renamed" }), true, false, true},
		{"cleanup started strict", change(func(c *leaseClaim) { c.Labels["state"] = "cleanup" }), true, true, false},
		{"cleanup started relaxed", change(func(c *leaseClaim) { c.Labels["state"] = "cleanup" }), true, false, false},
		{"lease stopped", change(func(c *leaseClaim) { c.Labels["state"] = "stopped" }), true, true, false},
		{"lease released", change(func(c *leaseClaim) { c.Labels["state"] = "released" }), true, false, false},
	} {
		if got := actionsRunnerReservationHolds(reserved, tc.current, tc.exists, tc.strict); got != tc.want {
			t.Errorf("%s: holds = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestActionsRunnerRegistrationRefusesLeaseRetiredAfterSnapshot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// Any guest contact would fail on a missing ssh, not with the refusal below.
	t.Setenv("PATH", t.TempDir())
	const leaseID = "cbx_retiring00001"
	captured, err := ClaimLeaseForRepoProviderScopePondWithLabels(leaseID, "retiring", "hyperv", "instance:test", "", "/repo", 30*time.Minute, map[string]string{"state": "ready"})
	if err != nil {
		t.Fatal(err)
	}
	// Cleanup changes only the state label after hydration captured its snapshot,
	// which the renewal-only snapshot check accepts.
	labels := cloneStringMap(captured.Labels)
	labels["state"] = "cleanup"
	if _, err := UpdateLeaseClaimLabelsAndLastUsedIfUnchanged(leaseID, captured, labels, time.Now()); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	app := App{Stdout: io.Discard, Stderr: &stderr}
	target := SSHTarget{Host: "guest.invalid", User: "runner", Port: "22", TargetOS: targetLinux}
	_, err = app.registerActionsRunner(t.Context(), Config{}, target, leaseID, "retiring", "repos/example-org/my-app", "", "", nil, captured, true, "")
	if err == nil || !strings.Contains(err.Error(), "lease is cleanup; refusing to activate") {
		t.Fatalf("registration on a retiring lease: %v\n%s", err, stderr.String())
	}
}

func TestActionsOperationFailureKeepsRunnerReconciliationDetail(t *testing.T) {
	cause := Exit(4, "lease cbx_x claim changed ownership during heartbeat")
	canceled, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	left := Exit(7, "persist runner repos/example-org/my-app/41; listener was not started: HTTP 403")
	got := actionsOperationFailure(canceled, left)
	if got == nil || !strings.Contains(got.Error(), "changed ownership") || !strings.Contains(got.Error(), "/41; listener was not started: HTTP 403") || ExitCodeForError(got, 0) != 4 {
		t.Fatalf("canceled hydration hid the runner left to reconcile: %v", got)
	}
	if got := actionsOperationFailure(canceled, context.Canceled); got == nil || got.Error() != cause.Error() {
		t.Fatalf("bare cancellation: %v", got)
	}
	// Registration already joins the cause with what is left to reconcile.
	reported := errors.Join(cause, left)
	if got := actionsOperationFailure(canceled, reported); got == nil || !strings.Contains(got.Error(), "/41; listener was not started: HTTP 403") || ExitCodeForError(got, 0) != 4 {
		t.Fatalf("cause-carrying failure lost its detail: %v", got)
	}
	plain := errors.New("dispatch failed")
	if got := actionsOperationFailure(context.Background(), plain); got != plain {
		t.Fatalf("uncanceled failure: %v", got)
	}
}

func TestActionsRunnerGuestReconcileReadOutlastsSlowGuest(t *testing.T) {
	identity := `{"settings":{"agentId":30,"agentName":"crabbox-lease","gitHubUrl":"https://github.com/example-org/my-app"},"active":false}`
	// A guest busy after a first install misses two control deadlines, then answers.
	calls := 0
	guest, err := readActionsRunnerGuestUntil(t.Context(), time.Millisecond, func(context.Context) (string, error) {
		calls++
		if calls < 3 {
			return "", context.DeadlineExceeded
		}
		return identity, nil
	})
	if err != nil || calls != 3 || guest.Settings == nil || guest.Settings.ID != 30 {
		t.Fatalf("slow guest: calls=%d guest=%+v err=%v", calls, guest.Settings, err)
	}
	// An answer that is not a usable identity is final, never retried.
	calls = 0
	if _, err := readActionsRunnerGuestUntil(t.Context(), time.Millisecond, func(context.Context) (string, error) {
		calls++
		return `{"settings":null}`, nil
	}); err == nil || !strings.Contains(err.Error(), "listener state is unknown") || calls != 1 {
		t.Fatalf("invalid identity: calls=%d err=%v", calls, err)
	}
	// The budget still ends an unreachable guest with the transport error.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := readActionsRunnerGuestUntil(ctx, 5*time.Millisecond, func(context.Context) (string, error) {
		return "", errors.New("exit status 255")
	}); err == nil || !strings.Contains(err.Error(), "inspect existing runner registration: exit status 255") || ExitCodeForError(err, 0) != 7 {
		t.Fatalf("unreachable guest: %v", err)
	}
}

func TestActionsUnregisterRejectsInvalidReceipt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	root := t.TempDir()
	isolateRunTestUserDirs(t, root)
	marker := filepath.Join(root, "called")
	if err := os.WriteFile(filepath.Join(root, "gh"), []byte("#!/bin/sh\ntouch "+shellQuote(marker)+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	valid := `{"version":1,"leaseID":"cbx_1234abcd5678","scope":"orgs/example-org","runnerID":42,"name":"runner"}`
	for _, invalid := range []string{
		strings.Replace(valid, `"version":1`, `"version":2`, 1),
		strings.Replace(valid, "cbx_1234abcd5678", "../escape", 1),
		strings.Replace(valid, "orgs/example-org", "orgs/../escape", 1),
		strings.Replace(valid, `"runnerID":42`, `"runnerID":0`, 1),
		strings.Replace(valid, `"name":"runner"`, `"token":"private"`, 1),
		valid + valid,
		valid + strings.Repeat(" ", 8192),
	} {
		path := filepath.Join(root, "receipt.json")
		if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := (App{Stdout: &out, Stderr: &out}).Run(t.Context(), []string{"actions", "unregister", "--receipt", path}); err == nil {
			t.Fatalf("invalid receipt accepted: %q", invalid[:min(len(invalid), 128)])
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid receipt reached GitHub: %v", err)
	}
}
