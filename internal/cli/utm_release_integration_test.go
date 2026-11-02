//go:build darwin

package cli_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/utm"
	"golang.org/x/crypto/ssh"
)

const utmReleaseID = "cbx_abcdef123456"
const utmReleaseVMID = "aaaaaaaa-aaaa-4aaa-8aaa-000000000001"

// Only the real backend's command transport is replaced. No Apple event,
// plist utility, SSH connection or remote command is executed by this fixture.
type utmReleaseFixture struct {
	cfg                  core.Config
	backend              core.SSHLeaseBackend
	lease                core.LeaseTarget
	claim                core.LeaseClaim
	journal, claimPath   string
	log, entered, resume string
	vm                   map[string]any
	record               map[string]any
	actions              []string
}

func newUTMReleaseFixture(t *testing.T, target string) *utmReleaseFixture {
	t.Helper()
	core.PrepareUTMCoreReleaseTest(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	f := &utmReleaseFixture{cfg: core.BaseConfig(), log: filepath.Join(root, "ssh.log"), entered: filepath.Join(root, "entered"), resume: filepath.Join(root, "resume")}
	f.cfg.Provider, f.cfg.TargetOS, f.cfg.Architecture = "utm", target, "arm64"
	f.cfg.UTM.AppPath = filepath.Join(root, "UTM.app")
	f.cfg.UTM.StorageRoot = filepath.Join(root, "bundles")
	f.cfg.UTM.Acquisition, f.cfg.UTM.Template = "template", "11111111-1111-4111-8111-111111111111"
	f.cfg.UTM.User, f.cfg.UTM.WorkRoot = "crabbox", "/work/crabbox"
	state, err := core.CrabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	f.journal = filepath.Join(state, "utm", utmReleaseID+".json")
	f.claimPath = filepath.Join(state, "claims", utmReleaseID+".json")
	keyDir := filepath.Join(state, "utm", "keys-"+utmReleaseID)
	f.cfg.UTM.SSHKey = filepath.Join(keyDir, "id_ed25519")
	for _, dir := range []string{filepath.Dir(f.journal), keyDir, f.cfg.UTM.StorageRoot} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(f.cfg.UTM.SSHKey, []byte("inert key fixture; never used by SSH\n"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := "crabbox:utm:v1:" + utmReleaseID + ":fixture-token"
	backend, mode, port, version := "qemu", "emulated", 22022, 1
	forwards := []map[string]any{{"protocol": "TCP", "hostAddress": "127.0.0.1", "hostPort": port, "guestPort": 22}}
	if target == core.TargetMacOS {
		backend, mode, port, version = "apple", "shared", 22, 2
		forwards = nil
		f.cfg.UTM.Acquisition, f.cfg.UTM.Template = "ipsw", ""
	}
	net := map[string]any{"mode": mode, "address": "02:00:00:00:00:01", "portForwards": forwards}
	f.vm = map[string]any{"id": utmReleaseVMID, "name": "crabbox-utm-fixture-token", "status": "started", "backend": backend, "configuration": map[string]any{"notes": marker, "architecture": "aarch64", "networkInterfaces": []any{net}}}
	f.record = map[string]any{"version": version, "acquisition": f.cfg.UTM.Acquisition, "backend": backend, "targetOS": target, "leaseId": utmReleaseID, "slug": "fixture", "appPath": f.cfg.UTM.AppPath, "sourceId": f.cfg.UTM.Template, "ownedName": "crabbox-utm-fixture-token", "token": "fixture-token", "vmId": utmReleaseVMID, "architecture": "aarch64", "mac": "02:00:00:00:00:01", "port": port, "user": "crabbox", "sshKey": f.cfg.UTM.SSHKey, "workRoot": "/work/crabbox", "configured": true, "state": "ready", "labels": map[string]string{"lease_id": utmReleaseID, "slug": "fixture", "provider": "utm", "state": "ready", "target": target}}
	if target == core.TargetMacOS {
		bundle := filepath.Join(f.cfg.UTM.StorageRoot, "owned.utm")
		if err := os.MkdirAll(bundle, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundle, "config.plist"), []byte("inert plist fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		key, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize)).Public())
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range map[string]any{"bundlePath": bundle, "address": "192.0.2.10", "sshHostKey": strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), "generatedKey": true, "installComplete": true, "firstBootSubmitted": true, "accountComplete": true, "sshReady": true, "imageVersion": "27.0.1", "imageBuild": "fixture-build"} {
			f.record[key] = value
		}
	}
	data, _ := json.Marshal(f.record)
	if err := os.WriteFile(f.journal, data, 0600); err != nil {
		t.Fatal(err)
	}
	configured, err := (utm.Provider{}).Configure(f.cfg, core.Runtime{Exec: f, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	f.backend = configured.(core.SSHLeaseBackend)
	f.lease, err = f.backend.Resolve(t.Context(), core.ResolveRequest{ID: utmReleaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	f.claim, err = core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(utmReleaseID, "fixture", f.cfg, (utm.Provider{}).ClaimScope(f.cfg), f.lease.Server, f.lease.SSH, root, time.Hour, false, core.LeaseClaim{}, false)
	if err != nil {
		t.Fatal(err)
	}
	core.SetServerLeaseClaimSnapshot(&f.lease.Server, f.claim, true)
	config := fmt.Sprintf("provider: utm\ntarget: %s\narchitecture: arm64\nutm:\n  acquisition: %s\n  appPath: %q\n  storageRoot: %q\n  sshKey: %q\n", target, f.cfg.UTM.Acquisition, f.cfg.UTM.AppPath, f.cfg.UTM.StorageRoot, f.cfg.UTM.SSHKey)
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	// Record arguments only. The synthetic key, address and remote command are
	// never passed to an SSH implementation or evaluated by a shell.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$UTM_RELEASE_SSH_LOG\"\nif [ -n \"${UTM_RELEASE_WAIT:-}\" ]; then\n : > \"$UTM_RELEASE_ENTERED\"\n while [ ! -f \"$UTM_RELEASE_RESUME\" ]; do /bin/sleep 0.01; done\nfi\n"
	if err := os.WriteFile(filepath.Join(root, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	t.Setenv("UTM_RELEASE_SSH_LOG", f.log)
	t.Setenv("UTM_RELEASE_ENTERED", f.entered)
	t.Setenv("UTM_RELEASE_RESUME", f.resume)
	return f
}

func (f *utmReleaseFixture) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	if req.Name == "/usr/bin/plutil" {
		var value any
		switch req.Args[1] {
		case "Information.UUID":
			value = utmReleaseVMID
		case "Information":
			value = map[string]any{"UUID": utmReleaseVMID, "Name": f.vm["name"], "Notes": f.vm["configuration"].(map[string]any)["notes"]}
		case "Backend":
			value = "Apple"
		case "System.Architecture":
			value = "aarch64"
		case "System.Boot.OperatingSystem":
			value = "macOS"
		case "Network":
			value = []map[string]string{{"Mode": "Shared", "MacAddress": "02:00:00:00:00:01"}}
		default:
			return core.LocalCommandResult{}, fmt.Errorf("unexpected plist key %q", req.Args[1])
		}
		if req.Args[2] == "raw" {
			return core.LocalCommandResult{Stdout: fmt.Sprint(value)}, nil
		}
		data, _ := json.Marshal(value)
		return core.LocalCommandResult{Stdout: string(data)}, nil
	}
	if req.Name != "/usr/bin/osascript" {
		return core.LocalCommandResult{}, fmt.Errorf("unexpected executable %q", req.Name)
	}
	if req.Args[0] == "-l" {
		vms := []map[string]any{}
		if f.vm != nil {
			vms = append(vms, f.vm)
		}
		data, _ := json.Marshal(vms)
		return core.LocalCommandResult{Stdout: string(data)}, nil
	}
	script, err := io.ReadAll(req.Stdin)
	if err != nil {
		return core.LocalCommandResult{}, err
	}
	if !strings.Contains(string(script), utmReleaseVMID) {
		return core.LocalCommandResult{}, errors.New("unexpected VM identity")
	}
	switch {
	case strings.Contains(string(script), "stop targetVM"):
		f.actions = append(f.actions, "stop")
		f.vm["status"] = "stopped"
	case strings.Contains(string(script), "delete targetVM"):
		f.actions = append(f.actions, "delete")
		f.vm = nil
	default:
		return core.LocalCommandResult{}, fmt.Errorf("unexpected lifecycle operation %s", script)
	}
	// Useful teardown must already have happened when the provider mutates.
	if strings.Count(f.sshLog(), utmReleaseID+".stop") != 1 {
		return core.LocalCommandResult{}, errors.New("provider mutation before exactly one admitted guest teardown")
	}
	return core.LocalCommandResult{}, nil
}

func (f *utmReleaseFixture) sshLog() string { data, _ := os.ReadFile(f.log); return string(data) }

func (f *utmReleaseFixture) release(t *testing.T, ctx context.Context, path string, afterResolve func() error) error {
	t.Helper()
	if path == "automatic" {
		return core.AutomaticUTMReleaseForTest(ctx, f.backend, f.cfg, f.lease)
	}
	return core.ExplicitUTMReleaseForTest(t, ctx, f.backend, utmReleaseID, afterResolve)
}

func TestUTMCoreReleaseRejectsStaleBeforeGuestCleanup(t *testing.T) {
	// These tests change environment and the provider registry; do not parallelize.
	for _, target := range []string{core.TargetLinux, core.TargetMacOS} {
		for _, path := range []string{"automatic", "stop"} {
			for _, scenario := range []string{"reclaimed", "advanced", "removed", "marker changed"} {
				t.Run(target+"/"+path+"/"+scenario, func(t *testing.T) {
					f := newUTMReleaseFixture(t, target)
					var claimBefore, journalBefore []byte
					mutate := func() error {
						var err error
						switch scenario {
						case "reclaimed":
							_, err = core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(utmReleaseID, "fixture", f.cfg, (utm.Provider{}).ClaimScope(f.cfg), f.lease.Server, f.lease.SSH, t.TempDir(), time.Hour, true, f.claim, true)
						case "advanced":
							_, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(utmReleaseID, f.claim, f.claim)
						case "removed":
							err = core.RemoveLeaseClaimIfUnchanged(utmReleaseID, f.claim)
						case "marker changed":
							f.vm["configuration"].(map[string]any)["notes"] = "another ownership marker"
						}
						claimBefore, _ = os.ReadFile(f.claimPath)
						journalBefore, _ = os.ReadFile(f.journal)
						return err
					}
					var afterResolve func() error
					if path == "automatic" {
						if err := mutate(); err != nil {
							t.Fatal(err)
						}
					} else {
						afterResolve = mutate
					}
					ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
					defer cancel()
					err := f.release(t, ctx, path, afterResolve)
					if err == nil {
						t.Fatal("stale release accepted")
					}
					if log := f.sshLog(); log != "" {
						t.Fatalf("rejected authority reached guest SSH cleanup before release guard: %s", log)
					}
					if len(f.actions) != 0 || f.vm == nil {
						t.Fatalf("rejected authority mutated VM: %v", f.actions)
					}
					for name, want := range map[string][]byte{f.claimPath: claimBefore, f.journal: journalBefore} {
						got, _ := os.ReadFile(name)
						if !bytes.Equal(got, want) {
							t.Fatalf("rejected authority changed %s", filepath.Base(name))
						}
					}
					if _, err := os.Stat(f.cfg.UTM.SSHKey); err != nil {
						t.Fatal("rejected authority removed key fixture", err)
					}
				})
			}
		}
	}
}

func TestUTMCoreReleaseCurrentCleanupHoldsFence(t *testing.T) {
	for _, target := range []string{core.TargetLinux, core.TargetMacOS} {
		for _, path := range []string{"automatic", "stop"} {
			t.Run(target+"/"+path, func(t *testing.T) {
				f := newUTMReleaseFixture(t, target)
				t.Setenv("UTM_RELEASE_WAIT", "1")
				// Generic cleanup reserves ten seconds of its parent budget for
				// later stages; a shorter deadline would mask SSH dispatch entirely.
				ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- f.release(t, ctx, path, nil) }()
				joined := false
				defer func() {
					cancel()
					_ = os.WriteFile(f.resume, nil, 0600)
					if !joined {
						select {
						case <-done:
						case <-time.After(10 * time.Second):
							t.Error("inert release did not join after cancellation")
						}
					}
				}()
				// The first inert SSH child pauses inside generic teardown. A real
				// competing claim operation must not enter that interval.
				for {
					if _, err := os.Stat(f.entered); err == nil {
						break
					}
					select {
					case err := <-done:
						joined = true
						t.Fatalf("release returned before cleanup: %v", err)
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-time.After(5 * time.Millisecond):
					}
				}
				if target == core.TargetMacOS {
					// The actual PrepareConnection callback must recreate the
					// recorded pin while the release owns both local fences.
					pin, err := os.ReadFile(f.lease.SSH.KnownHostsFile)
					want := "crabbox-utm-" + utmReleaseID + " " + f.record["sshHostKey"].(string) + "\n"
					if err != nil || string(pin) != want {
						t.Fatalf("guarded macOS cleanup lost its recorded host pin: %q error=%v", pin, err)
					}
				}
				lockCtx, lockCancel := context.WithTimeout(ctx, 40*time.Millisecond)
				entered := false
				err := core.WithLeaseClaimUnchangedContext(lockCtx, utmReleaseID, f.claim, func() error { entered = true; return nil })
				lockCancel()
				// Always unblock the inert child, including on the pre-fix failure.
				if writeErr := os.WriteFile(f.resume, nil, 0600); writeErr != nil {
					t.Fatal(writeErr)
				}
				releaseErr := <-done
				joined = true
				if entered || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("guest teardown ran outside claim fence: entered=%t error=%v", entered, err)
				}
				if releaseErr != nil {
					t.Fatal(releaseErr)
				}
				if !reflect.DeepEqual(f.actions, []string{"stop", "delete"}) {
					t.Fatalf("provider actions=%v", f.actions)
				}
				log := f.sshLog()
				if strings.Count(log, utmReleaseID+".stop") != 1 {
					t.Fatalf("guest teardown not exactly once: %s", log)
				}
				wantEgress := 0
				if target == core.TargetLinux {
					wantEgress = 1
				}
				if strings.Count(log, "[c]rabbox-egress-client egress client") != wantEgress {
					t.Fatalf("incorrect target-specific cleanup: %s", log)
				}
				for _, name := range []string{f.claimPath, f.journal} {
					if _, err := os.Stat(name); !os.IsNotExist(err) {
						t.Fatalf("successful release retained %s: %v", name, err)
					}
				}
			})
		}
	}
}
