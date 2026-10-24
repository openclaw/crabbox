package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type receiptRunProvider struct {
	runEnvProfileTestProvider
	backend *receiptRunBackend
}

func (p receiptRunProvider) Spec() ProviderSpec {
	spec := p.runEnvProfileTestProvider.Spec()
	spec.Name = "receipt-run-test"
	return spec
}
func (p receiptRunProvider) Configure(Config, Runtime) (Backend, error) { return p.backend, nil }

type receiptRunBackend struct {
	runEnvProfileTestBackend
	required   bool
	updates    int
	persistErr error
}

func (b *receiptRunBackend) TailscaleMetadataRequired(LeaseTarget) bool { return b.required }
func (b *receiptRunBackend) UpdateTailscaleMetadata(_ context.Context, lease LeaseTarget, meta TailscaleMetadata) (Server, error) {
	b.updates++
	if b.persistErr != nil {
		return Server{}, b.persistErr
	}
	if meta.DeviceID != "nFixtureDevice" || meta.IPv4 != lease.SSH.Host {
		return Server{}, Exit(4, "lease_id_conflict: bound receipt identity changed")
	}
	return lease.Server, nil
}

func setupReceiptRun(t *testing.T, required bool) (*receiptRunBackend, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH fixture")
	}
	dir := setupRunCleanupWorkspaceOwnerTest(t)
	if err := os.WriteFile(filepath.Join(dir, "payload.txt"), []byte("sync payload"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "payload.txt"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false", "commit", "-qm", "fixture"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git fixture: %v %s", err, out)
		}
	}
	backend := &receiptRunBackend{required: required}
	provider := receiptRunProvider{backend: backend}
	backend.spec = provider.Spec()
	RegisterProvider(provider)
	t.Cleanup(func() { delete(providerRegistry, provider.Spec().Name) })
	runEnvProfileTestAcquireLease = func(AcquireRequest) (LeaseTarget, error) {
		return LeaseTarget{
			LeaseID: "cbx_env_profile_test",
			Server: Server{Provider: provider.Spec().Name, Labels: map[string]string{
				"tailscale": "true", "tailscale_ipv4": "100.100.1.2", "tailscale_device_id": "nFixtureDevice",
			}},
			SSH: SSHTarget{Host: "100.100.1.2", User: "fixture", Port: "22", TargetOS: targetLinux,
				SSHConfigProxy: true, AuthoritativeNetwork: true, NetworkKind: NetworkTailscale},
		}, nil
	}
	receipt := filepath.Join(dir, "receipt")
	log := filepath.Join(dir, "ssh.log")
	t.Setenv("CRABBOX_TEST_RECEIPT", receipt)
	// The production reader still runs through SSH and parses the NUL framing.
	installWorkspaceOwnerAwareSSH(t, filepath.Join(dir, "ssh"), `#!/bin/sh
printf '%s\n' "$1" >> "$CRABBOX_FAKE_SSH_LOG"
case "$1" in
  *'for field in ipv4 hostname'*) cat "$CRABBOX_TEST_RECEIPT"; exit $? ;;
esac
exit 0
`)
	if err := os.WriteFile(filepath.Join(dir, "rsync"), []byte("#!/bin/sh\nprintf 'rsync\\n' >> \"$CRABBOX_FAKE_SSH_LOG\"\nexit 91\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return backend, receipt, log
}

func writeTailscaleRunReceipt(t *testing.T, path, ip, device string) {
	t.Helper()
	data := strings.Join([]string{ip, "cbx-fixture", "", "", "", "", device, ""}, "\x00")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

// Fail each real common caller independently: the owner check passes before a
// later sync/command receipt drifts. This cannot pass by testing only the backend.
func TestRunRequiredTailscaleReceiptGatesCommonCallers(t *testing.T) {
	for _, phase := range []string{"workspace owner", "before sync", "before command"} {
		for _, failure := range []string{"changed device", "missing device", "wrong address", "unreadable", "malformed", "persist failure"} {
			t.Run(phase+"/"+failure, func(t *testing.T) {
				backend, receipt, log := setupReceiptRun(t, true)
				writeTailscaleRunReceipt(t, receipt, "100.100.1.2", "nFixtureDevice")
				var output bytes.Buffer
				var phases []string
				app := App{Stdout: &output, Stderr: &output,
					sshReadinessWaiter: func(_ context.Context, _ *SSHTarget, _ io.Writer, current string, _ time.Duration) error {
						phases = append(phases, current)
						if current == phase {
							switch failure {
							case "changed device":
								writeTailscaleRunReceipt(t, receipt, "100.100.1.2", "nReplacement")
							case "missing device":
								writeTailscaleRunReceipt(t, receipt, "100.100.1.2", "")
							case "wrong address":
								writeTailscaleRunReceipt(t, receipt, "100.100.1.3", "nFixtureDevice")
							case "unreadable":
								if err := os.Remove(receipt); err != nil {
									t.Fatal(err)
								}
							case "malformed":
								if err := os.WriteFile(receipt, []byte("bad framing"), 0600); err != nil {
									t.Fatal(err)
								}
							case "persist failure":
								backend.persistErr = errors.New("receipt CAS refused")
							}
						}
						return nil
					},
					workspaceOwnerAcquirer: func(ctx context.Context, target SSHTarget, id string, _ io.Writer) (*workspaceOwner, error) {
						ownerCtx, cancel := context.WithCancel(ctx)
						done := make(chan struct{})
						close(done)
						return &workspaceOwner{target: target, transport: runRecoveryOwnerTransport{}, key: workspaceOwnerKey(id),
							token: strings.Repeat("a", 64), ttl: time.Minute, ctx: ownerCtx, cancel: cancel, stop: make(chan struct{}), done: done}, nil
					},
				}
				args := []string{"--provider", backend.spec.Name, "--id", "cbx_env_profile_test", "--no-hydrate", "--timing-record", "off"}
				if phase == "before sync" {
					args = append(args, "--sync-only")
				} else {
					args = append(args, "--no-sync")
				}
				if phase != "before sync" {
					args = append(args, "--", "receipt-user-payload")
				}
				err := app.runCommand(t.Context(), args)
				if err == nil || !strings.Contains(err.Error(), "required tailscale receipt") {
					t.Fatalf("receipt failure did not propagate: %v phases=%v\n%s", err, phases, output.String())
				}
				if len(phases) == 0 || phases[len(phases)-1] != phase {
					t.Fatalf("did not reach %s: %v", phase, phases)
				}
				wantUpdates := 1
				if phase != "workspace owner" {
					wantUpdates++
				}
				if failure == "unreadable" || failure == "malformed" {
					wantUpdates--
				}
				if backend.updates != wantUpdates {
					t.Fatalf("updates=%d want=%d", backend.updates, wantUpdates)
				}
				claim, err := ReadLeaseClaim("cbx_env_profile_test")
				if err != nil || claim.Labels["tailscale_device_id"] != "nFixtureDevice" {
					t.Fatalf("receipt failure changed cached binding: %v %+v", err, claim)
				}
				calls, _ := os.ReadFile(log)
				for _, forbidden := range []string{"receipt-user-payload", "crabbox-manifest", "rsync", "nReplacement"} {
					if strings.Contains(string(calls), forbidden) {
						t.Fatalf("work escaped receipt gate (%s): %s", forbidden, calls)
					}
				}
			})
		}
	}
}

func TestRunOptionalTailscaleMetadataRemainsBestEffort(t *testing.T) {
	for _, failure := range []string{"changed device", "unreadable"} {
		t.Run(failure, func(t *testing.T) {
			backend, receipt, log := setupReceiptRun(t, false)
			if failure == "changed device" {
				writeTailscaleRunReceipt(t, receipt, "100.100.1.2", "nReplacement")
			}
			var output bytes.Buffer
			app := App{Stdout: &output, Stderr: &output, sshReadinessWaiter: func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error { return nil }}
			err := app.runCommand(t.Context(), []string{"--provider", backend.spec.Name, "--no-sync", "--no-hydrate", "--timing-record", "off", "--", "receipt-user-payload"})
			calls, _ := os.ReadFile(log)
			if err != nil || !strings.Contains(output.String(), "warning: tailscale metadata") || !strings.Contains(string(calls), "receipt-user-payload") {
				t.Fatalf("optional metadata changed behavior: %v\n%s\n%s", err, output.String(), calls)
			}
		})
	}
}
