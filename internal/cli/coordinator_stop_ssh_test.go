package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCoordinatorStopGuestCleanupSSHIdentity(t *testing.T) {
	for _, command := range []string{"stop", "release", "automatic"} {
		for _, tc := range []struct {
			name         string
			missingKey   bool
			malformedPin bool
		}{
			{name: "stored lease key"},
			{name: "missing local key", missingKey: true},
			{name: "invalid host pin", malformedPin: true},
		} {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				clearConfigEnv(t)
				dir := t.TempDir()
				argsPath := filepath.Join(dir, "ssh-args.log")
				installRecordingSSH(t, dir, `printf '%s\n' "$@" "$match" '---' >> "$CRABBOX_TEST_SSH_ARGS"
if [ "$CRABBOX_TEST_SSH_FAIL" = yes ]; then exit 255; fi`)
				t.Setenv("CRABBOX_TEST_SSH_ARGS", argsPath)
				t.Setenv("CRABBOX_TEST_SSH_FAIL", "no")
				t.Setenv("CRABBOX_OWNER", "test@example.com")
				t.Setenv("CRABBOX_NETWORK", "public")
				const id = "cbx_abcdef123456"
				keyPath, err := TestboxKeyPath(id)
				if err != nil {
					t.Fatal(err)
				}
				defaultKey := filepath.Join(os.Getenv("HOME"), ".ssh", "id_ed25519")
				t.Setenv("CRABBOX_SSH_KEY", defaultKey)
				wantKey := keyPath
				if tc.missingKey {
					wantKey = defaultKey
					t.Setenv("CRABBOX_TEST_SSH_FAIL", "yes")
				} else {
					if _, err := ensureTestboxLeaseDirectory(id); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(keyPath, []byte("synthetic lease key fixture\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				knownHosts := filepath.Join(filepath.Dir(keyPath), "known_hosts")
				alias, err := leaseHostKeyAlias(id)
				if err != nil {
					t.Fatal(err)
				}
				hostKey := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 41))
				if tc.malformedPin {
					hostKey = "invalid host key"
				}
				var reads, releases atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+id:
						read := reads.Add(1)
						lease := CoordinatorLease{
							ID: id, Provider: "aws", State: "active", TargetOS: targetLinux,
							Host: "192.0.2.70", SSHPort: "22", SSHUser: "crabbox", SSHHostKey: hostKey,
						}
						if read == 1 {
							lease.Host = "192.0.2.69"
							lease.SSHHostKey = testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 42))
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
					case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/"+id+"/release":
						releases.Add(1)
						if !tc.malformedPin {
							pin, err := os.ReadFile(knownHosts)
							if want := alias + " " + sshKeyWithoutComment(hostKey) + "\n"; err != nil || string(pin) != want {
								t.Errorf("guest cleanup host pin=%q err=%v, want fresh pin %q", pin, err, want)
							}
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"lease": confirmedCoordinatorRelease(id, "aws")})
					default:
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				t.Setenv("CRABBOX_COORDINATOR", server.URL)
				t.Setenv("CRABBOX_COORDINATOR_TOKEN", "synthetic-stop-token")
				var stderr bytes.Buffer
				app := App{Stdout: io.Discard, Stderr: &stderr}
				if command == "automatic" {
					backend := coordinatorReleaseTestBackend(server, &stderr)
					backend.cfg.SSHKey = defaultKey
					// Normal run acquisition prepares identity before automatic release
					// refreshes the lease again under the claim fence.
					lease, resolveErr := backend.Resolve(t.Context(), ResolveRequest{ID: id})
					if resolveErr != nil {
						t.Fatal(resolveErr)
					}
					err = app.releaseBackendLeaseBestEffort(t.Context(), backend, backend.cfg, lease)
				} else {
					err = app.Run(t.Context(), []string{command, "--provider", "aws", "--id", id})
				}
				if err != nil || releases.Load() != 1 || reads.Load() != 2 {
					t.Fatalf("err=%v releases=%d reads=%d stderr=%s", err, releases.Load(), reads.Load(), stderr.String())
				}
				args, readErr := os.ReadFile(argsPath)
				if tc.malformedPin {
					if readErr != nil && !os.IsNotExist(readErr) {
						t.Fatal(readErr)
					}
					if len(args) != 0 || strings.Count(stderr.String(), "warning:") != 1 || !strings.Contains(stderr.String(), "could not prepare guest SSH before release") {
						t.Fatalf("unsafe SSH attempted or preparation warning missing: args=%s stderr=%s", args, stderr.String())
					}
					return
				}
				if readErr != nil {
					t.Fatal(readErr)
				}
				calls := strings.Split(strings.TrimSuffix(string(args), "---\n"), "---\n")
				if len(calls) != 3 {
					t.Fatalf("SSH calls=%d, want hydration read, stop marker and egress cleanup: %s", len(calls), args)
				}
				for i, phase := range []string{id + ".env", id + ".stop", "pkill -f"} {
					call := calls[i]
					if !strings.Contains(call, phase) {
						t.Errorf("SSH call %d missing guest phase %q: %s", i, phase, call)
					}
					for _, want := range []string{
						"-i\n" + wantKey, "UserKnownHostsFile=" + sshConfigFileValue(knownHosts),
						"HostKeyAlias=" + alias, "HostKeyAlgorithms=ssh-ed25519",
						"StrictHostKeyChecking=yes", "GlobalKnownHostsFile=none", "crabbox@192.0.2.70",
					} {
						if !strings.Contains(call, want+"\n") {
							t.Errorf("SSH call %d missing %q", i, want)
						}
					}
					for _, unwanted := range []string{"StrictHostKeyChecking=accept-new", "StrictHostKeyChecking=no", "UserKnownHostsFile=" + sshConfigFileValue(filepath.Join(filepath.Dir(defaultKey), "known_hosts")), "crabbox@192.0.2.69"} {
						if strings.Contains(call, unwanted+"\n") {
							t.Errorf("SSH call %d contains unsafe/stale option %q", i, unwanted)
						}
					}
				}
				if tc.missingKey {
					if strings.Count(stderr.String(), "warning:") != 2 || !strings.Contains(stderr.String(), "could not stop GitHub Actions hydration") || !strings.Contains(stderr.String(), "egress remote client cleanup failed") {
						t.Fatalf("missing-key cleanup did not attempt SSH and warn: %s", stderr.String())
					}
				} else if strings.Contains(stderr.String(), "warning:") {
					t.Fatalf("successful guest cleanup warned: %s", stderr.String())
				}
			})
		}
	}
}
