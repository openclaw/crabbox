package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrepareLeaseSSHTrustInstallsAuthoritativeED25519Pin(t *testing.T) {
	isolateTestUserDirs(t)
	const leaseID = "cbx_abcdef123456"
	hostKey := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 41))
	target := SSHTarget{
		User:       "crabbox",
		Host:       "192.0.2.25",
		Port:       "2222",
		SSHHostKey: hostKey + " coordinator-comment",
	}
	if err := prepareLeaseSSHTrust(&target, leaseID); err != nil {
		t.Fatal(err)
	}
	wantAlias, err := leaseHostKeyAlias(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if target.HostKeyAlias != wantAlias {
		t.Fatalf("HostKeyAlias=%q want %q", target.HostKeyAlias, wantAlias)
	}
	data, err := os.ReadFile(target.KnownHostsFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := wantAlias + " " + sshKeyWithoutComment(hostKey) + "\n"; string(data) != want {
		t.Fatalf("known_hosts=%q want %q", data, want)
	}
	args := strings.Join(sshBaseArgs(target), " ")
	for _, want := range []string{
		"StrictHostKeyChecking=yes",
		"UserKnownHostsFile=" + sshConfigFileValue(target.KnownHostsFile),
		"HostKeyAlias=" + wantAlias,
		"HostKeyAlgorithms=ssh-ed25519",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("SSH args %q missing %q", args, want)
		}
	}
	if strings.Contains(args, "StrictHostKeyChecking=accept-new") {
		t.Fatalf("authoritative target retained TOFU: %q", args)
	}
}

func TestAuthoritativeKnownHostTokenFormatting(t *testing.T) {
	tests := []struct {
		name   string
		target SSHTarget
		want   string
	}{
		{name: "default port", target: SSHTarget{Host: "host.example", Port: "22"}, want: "host.example"},
		{name: "nondefault port", target: SSHTarget{Host: "host.example", Port: "2222"}, want: "[host.example]:2222"},
		{name: "IPv6", target: SSHTarget{Host: "2001:db8::25", Port: "2222"}, want: "[2001:db8::25]:2222"},
		{name: "stable alias", target: SSHTarget{Host: "192.0.2.25", Port: "2222", HostKeyAlias: "crabbox-lease-stable"}, want: "crabbox-lease-stable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := authoritativeKnownHostToken(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("token=%q want %q", got, tc.want)
			}
		})
	}
}

func TestInstallAuthoritativeSSHHostKeyAtomicallyReplacesPriorPin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	first := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 42))
	second := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 43))
	target := SSHTarget{Host: "192.0.2.26", Port: "22", HostKeyAlias: "stable", KnownHostsFile: path, SSHHostKey: first}
	if err := installAuthoritativeSSHHostKey(&target); err != nil {
		t.Fatal(err)
	}
	target.SSHHostKey = second
	if err := installAuthoritativeSSHHostKey(&target); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "stable " + sshKeyWithoutComment(second) + "\n"; string(data) != want {
		t.Fatalf("known_hosts=%q want %q", data, want)
	}
	if strings.Contains(string(data), sshKeyWithoutComment(first)) || bytes.Count(data, []byte("\n")) != 1 {
		t.Fatalf("known_hosts retained a stale pin: %q", data)
	}
}

func TestInstallAuthoritativeSSHHostKeyRejectsUnsafeTargetsAndRepairsModes(t *testing.T) {
	hostKey := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 44))
	t.Run("symlink target", func(t *testing.T) {
		dir := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "known_hosts")
		if err := os.Symlink(outside, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		target := SSHTarget{Host: "host.example", Port: "22", KnownHostsFile: path, SSHHostKey: hostKey}
		if err := installAuthoritativeSSHHostKey(&target); err == nil {
			t.Fatal("symlink target accepted")
		}
		if data, err := os.ReadFile(outside); err != nil || string(data) != "unchanged" {
			t.Fatalf("outside target changed: data=%q err=%v", data, err)
		}
	})
	t.Run("directory target", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "known_hosts")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		target := SSHTarget{Host: "host.example", Port: "22", KnownHostsFile: path, SSHHostKey: hostKey}
		if err := installAuthoritativeSSHHostKey(&target); err == nil {
			t.Fatal("directory target accepted")
		}
	})
	t.Run("symlink parent", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		parent := filepath.Join(root, "lease")
		if err := os.Symlink(outside, parent); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		target := SSHTarget{Host: "host.example", Port: "22", KnownHostsFile: filepath.Join(parent, "known_hosts"), SSHHostKey: hostKey}
		if err := installAuthoritativeSSHHostKey(&target); err == nil {
			t.Fatal("symlink parent accepted")
		}
	})
	t.Run("permissive paths repaired", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "lease")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, 0o777); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(parent, "known_hosts")
		if err := os.WriteFile(path, []byte("stale\n"), 0o666); err != nil {
			t.Fatal(err)
		}
		target := SSHTarget{Host: "host.example", Port: "22", KnownHostsFile: path, SSHHostKey: hostKey}
		if err := installAuthoritativeSSHHostKey(&target); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(parent); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("parent mode=%v err=%v", infoMode(info), err)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("known_hosts mode=%v err=%v", infoMode(info), err)
		}
	})
}

func TestCoordinatorMalformedAuthoritativeHostKeyFailsBeforeSSHAndRedactsKey(t *testing.T) {
	isolateTestUserDirs(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "ssh-invoked")
	sshPath := filepath.Join(dir, "ssh")
	if err := os.WriteFile(sshPath, []byte("#!/bin/sh\ntouch \"$SSH_MARKER\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("SSH_MARKER", marker)
	const secretKeyText = "ssh-ed25519 SECRET-AUTHORITATIVE-MATERIAL"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
			ID: "cbx_abcdef123456", Provider: "aws", State: "active", Host: "192.0.2.27", SSHUser: "crabbox", SSHPort: "22", SSHHostKey: secretKeyText,
		}})
	}))
	t.Cleanup(server.Close)
	backend := &coordinatorLeaseBackend{
		cfg:   Config{Provider: "aws", TargetOS: targetLinux},
		spec:  ProviderSpec{Name: "aws"},
		coord: &CoordinatorClient{BaseURL: server.URL, Client: server.Client()},
		rt:    Runtime{Stderr: io.Discard},
	}
	_, err := backend.Status(context.Background(), StatusRequest{ID: "cbx_abcdef123456"})
	if err == nil {
		t.Fatal("malformed authoritative host key accepted")
	}
	if strings.Contains(err.Error(), secretKeyText) || strings.Contains(err.Error(), "SECRET-AUTHORITATIVE-MATERIAL") {
		t.Fatalf("host key leaked in error: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SSH ran before trust preparation failed: %v", err)
	}
}

func TestPrepareLeaseSSHTrustRejectsSymlinkedLeaseNamespace(t *testing.T) {
	isolateTestUserDirs(t)
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	crabboxDir := filepath.Join(configDir, "crabbox")
	if err := os.MkdirAll(crabboxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(crabboxDir, "testboxes")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	target := SSHTarget{
		User: "crabbox", Host: "192.0.2.30", Port: "22",
		SSHHostKey: testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 46)),
	}
	if err := prepareLeaseSSHTrust(&target, "cbx_abcdef123456"); err == nil {
		t.Fatal("symlinked lease namespace accepted")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("symlink target was populated: entries=%v err=%v", entries, err)
	}
}

func TestEnsureTestboxLeaseDirectoryDoesNotCreateThroughConfigPathSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "redirect")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("HOME", filepath.Join(root, "redirect", "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "redirect", "config"))
	if _, err := ensureTestboxLeaseDirectory("cbx_abcdef123456"); err == nil {
		t.Fatal("symlinked config path accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("outside directory was modified through config symlink: %v", entries)
	}
}

func TestWriteAuthoritativeKnownHostsDoesNotCreateThroughIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, "lease"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "redirect")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	path := filepath.Join(root, "redirect", "lease", "known_hosts")
	if err := writeAuthoritativeKnownHosts(path, []byte("entry\n")); err == nil {
		t.Fatal("intermediate symlink path accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "lease", "known_hosts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside known_hosts was created through symlink: %v", err)
	}
}

func TestPrepareLeaseSSHTrustRejectsSharedKnownHostsWithoutOverwriting(t *testing.T) {
	isolateTestUserDirs(t)
	shared := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(shared, []byte("shared trust\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := SSHTarget{
		User: "crabbox", Host: "192.0.2.32", Port: "22", KnownHostsFile: shared,
		SSHHostKey: testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 49)),
	}
	if err := prepareLeaseSSHTrust(&target, "cbx_abcdef123456"); err == nil {
		t.Fatal("shared known_hosts path accepted")
	}
	if data, err := os.ReadFile(shared); err != nil || string(data) != "shared trust\n" {
		t.Fatalf("shared known_hosts changed: data=%q err=%v", data, err)
	}
}

func TestPrepareLeaseSSHTrustRejectsLegacyHostKeyAlgorithm(t *testing.T) {
	isolateTestUserDirs(t)
	legacy := testOpenSSHPublicKey("ssh-dss", []byte{1}, []byte{2}, []byte{3}, []byte{4})
	target := SSHTarget{User: "crabbox", Host: "192.0.2.33", Port: "22", SSHHostKey: legacy}
	err := prepareLeaseSSHTrust(&target, "cbx_abcdef123456")
	if err == nil {
		t.Fatal("legacy DSA host key accepted")
	}
	if strings.Contains(err.Error(), legacy) {
		t.Fatalf("legacy host key leaked in error: %v", err)
	}
}

func TestNoAuthoritativeSSHHostKeyKeepsAcceptNewBehavior(t *testing.T) {
	isolateTestUserDirs(t)
	target := SSHTarget{User: "crabbox", Host: "192.0.2.28", Port: "22"}
	if err := useLeaseKnownHosts(&target, "cbx_abcdef123456"); err != nil {
		t.Fatal(err)
	}
	if err := prepareLeaseSSHTrust(&target, "cbx_abcdef123456"); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(sshBaseArgs(target), " ")
	if !strings.Contains(args, "StrictHostKeyChecking=accept-new") || strings.Contains(args, "HostKeyAlias=") {
		t.Fatalf("no-key target behavior changed: %q", args)
	}
}

func TestAuthoritativeTrustOptionsCoverSSHEntryPaths(t *testing.T) {
	isolateTestUserDirs(t)
	target := SSHTarget{
		User:       "crabbox",
		Host:       "192.0.2.29",
		Port:       "2222",
		SSHHostKey: testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 45)),
	}
	if err := prepareLeaseSSHTrust(&target, "cbx_abcdef123456"); err != nil {
		t.Fatal(err)
	}
	transportConfig, err := renderSSHTransportConfig(target, true)
	if err != nil {
		t.Fatal(err)
	}
	vncArgs := strings.Join(vncTunnelArgs(target, "5901", "127.0.0.1", "5900"), " ")
	for _, control := range []string{"ControlMaster=no", "ControlPath=none", "ControlPersist=no"} {
		if !strings.Contains(vncArgs, control) {
			t.Fatalf("authoritative VNC args %q missing %q", vncArgs, control)
		}
	}
	paths := map[string]string{
		"regular/readiness/code/multiplexing":    strings.Join(sshBaseArgs(target), " "),
		"VNC/desktop":                            vncArgs,
		"sync/scp/copy/tunnel transport session": transportConfig,
	}
	for name, rendered := range paths {
		t.Run(name, func(t *testing.T) {
			if strings.Contains(rendered, "StrictHostKeyChecking=accept-new") || strings.Contains(rendered, "StrictHostKeyChecking accept-new") {
				t.Fatalf("authoritative transport retained TOFU: %q", rendered)
			}
			for _, want := range []string{"StrictHostKeyChecking", target.HostKeyAlias, target.KnownHostsFile} {
				if !strings.Contains(rendered, want) {
					t.Fatalf("transport %q missing %q", rendered, want)
				}
			}
			for _, exclusive := range []string{"GlobalKnownHostsFile", "KnownHostsCommand", "VerifyHostKeyDNS", "UpdateHostKeys", "CheckHostIP"} {
				if !strings.Contains(rendered, exclusive) {
					t.Fatalf("authoritative transport %q missing exclusive trust option %s", rendered, exclusive)
				}
			}
		})
	}
}

func TestAuthoritativeRSAUsesSHA2HostKeyAlgorithms(t *testing.T) {
	isolateTestUserDirs(t)
	target := SSHTarget{
		User: "crabbox", Host: "192.0.2.31", Port: "22",
		SSHHostKey: testOpenSSHPublicKey("ssh-rsa", []byte{1, 0, 1}, testBytes(128, 48)),
	}
	if err := prepareLeaseSSHTrust(&target, "cbx_abcdef123456"); err != nil {
		t.Fatal(err)
	}
	want := "HostKeyAlgorithms=rsa-sha2-512,rsa-sha2-256"
	if args := strings.Join(sshBaseArgs(target), " "); !strings.Contains(args, want) || strings.Contains(args, "HostKeyAlgorithms=ssh-rsa") {
		t.Fatalf("RSA SSH args=%q", args)
	}
	config, err := renderSSHTransportConfig(target, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(config, `HostKeyAlgorithms "rsa-sha2-512,rsa-sha2-256"`) || strings.Contains(config, `HostKeyAlgorithms "ssh-rsa"`) {
		t.Fatalf("RSA transport config=%q", config)
	}
}

func TestCoordinatorReleaseRemovesOnlyPerLeaseConnectionArtifacts(t *testing.T) {
	isolateTestUserDirs(t)
	configureCoordinatorReleaseTestTiming(t, time.Second, 0)
	const leaseID = "cbx_abcdef123456"
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"id_ed25519", "id_ed25519.pub", "known_hosts", "certificate.pub", "control"} {
		if err := os.WriteFile(filepath.Join(filepath.Dir(keyPath), name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sharedKey := filepath.Join(t.TempDir(), "shared-key")
	if err := os.WriteFile(sharedKey, []byte("shared"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseTargetForConfig(leaseID, "release-test", Config{Provider: "aws"}, Server{Provider: "aws"}, SSHTarget{}, time.Hour); err != nil {
		t.Fatal(err)
	}
	var releasePosts, observations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/release"):
			releasePosts.Add(1)
			deleting := true
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: leaseID, Provider: "aws", State: "released", CleanupStartedAt: "2026-08-19T00:00:00Z", ReleaseDeletesServer: &deleting,
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+leaseID:
			observation := observations.Add(1)
			lease := CoordinatorLease{ID: leaseID, Provider: "aws", State: "released", CleanupStartedAt: "2026-08-19T00:00:00Z"}
			if observation == 2 {
				lease.CleanupStartedAt = ""
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := coordinatorReleaseTestBackend(server, io.Discard)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{
		LeaseID: leaseID, Server: Server{Provider: "aws"}, SSH: SSHTarget{Key: sharedKey},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(keyPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("per-lease connection directory remains: %v", err)
	}
	if data, err := os.ReadFile(sharedKey); err != nil || string(data) != "shared" {
		t.Fatalf("shared key changed: data=%q err=%v", data, err)
	}
	if err := removeStoredTestboxConnectionArtifacts(context.Background(), leaseID); err != nil {
		t.Fatalf("idempotent cleanup: %v", err)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("claim exists=%t err=%v, want removed after final cleanup", exists, err)
	}
	if posts, observed := releasePosts.Load(), observations.Load(); posts != 1 || observed != 2 {
		t.Fatalf("release POSTs=%d observations=%d, want 1/2", posts, observed)
	}
}

func TestCoordinatorReleaseObservesPendingCreation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cleanupFails bool
	}{
		{name: "late allocation cleanup completes"},
		{name: "late allocation cleanup fails", cleanupFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanupFails := tc.cleanupFails
			isolateTestUserDirs(t)
			configureCoordinatorReleaseTestTiming(t, 5*time.Minute, 0)
			const leaseID = "cbx_abcdef123456"
			keyPath, err := testboxKeyPath(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := claimLeaseTargetForConfig(leaseID, "release-test", Config{Provider: "aws"}, Server{Provider: "aws"}, SSHTarget{}, time.Hour); err != nil {
				t.Fatal(err)
			}
			var releasePosts, observations atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				lease := map[string]any{
					"id": leaseID, "provider": "aws", "state": "released", "cloudID": "",
					"releaseDeletesServer": true, "cleanupStatus": "pending",
					"cleanupError": "provider creation is unresolved; cleanup has not been confirmed",
				}
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/"+leaseID+"/release":
					releasePosts.Add(1)
				case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+leaseID:
					observation := observations.Add(1)
					if _, err := os.Stat(keyPath); err != nil {
						t.Errorf("SSH artifacts removed before terminal observation: %v", err)
					}
					if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || !exists {
						t.Errorf("claim removed before terminal observation: exists=%t err=%v", exists, err)
					}
					lease["cloudID"] = "i-late-allocation"
					lease["cleanupStartedAt"] = "2026-08-30T00:01:00Z"
					if observation >= 2 {
						delete(lease, "cleanupStartedAt")
						if cleanupFails {
							lease["cleanupStatus"] = "failed"
							lease["cleanupError"] = "redacted provider delete failure"
							lease["cleanupRetryAt"] = "2026-08-30T00:06:00Z"
						} else {
							lease["cleanupStatus"] = "complete"
							delete(lease, "cleanupError")
						}
					}
				default:
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
			}))
			t.Cleanup(server.Close)
			backend := coordinatorReleaseTestBackend(server, io.Discard)
			err = backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{
				LeaseID: leaseID, Server: Server{Provider: "aws"},
			}})
			if (err != nil) != cleanupFails {
				t.Fatalf("release error=%v cleanupFails=%t", err, cleanupFails)
			}
			if cleanupFails && !strings.Contains(err.Error(), "reported a cleanup failure") {
				t.Fatalf("cleanup failure was not reported: %v", err)
			}
			var exitErr ExitError
			if cleanupFails && (!AsExitError(err, &exitErr) || exitErr.Code != 5) {
				t.Fatalf("cleanup error=%v, want exit 5", err)
			}
			if posts, observed := releasePosts.Load(), observations.Load(); posts != 1 || observed != 2 {
				t.Fatalf("release POSTs=%d observations=%d, want 1/2", posts, observed)
			}
			_, statErr := os.Stat(keyPath)
			if cleanupFails && statErr != nil || !cleanupFails && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("artifact state does not match confirmed cleanup: %v", statErr)
			}
			if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists != cleanupFails {
				t.Fatalf("claim exists=%t err=%v cleanupFails=%t", exists, err, cleanupFails)
			}
		})
	}
}

func TestCoordinatorReleasePreservesArtifactsWithoutConfirmedDestroy(t *testing.T) {
	configureCoordinatorReleaseTestTiming(t, 20*time.Millisecond, 2*time.Millisecond)
	deleting := true
	retained := false
	tests := []struct {
		name             string
		provider         string
		postLease        CoordinatorLease
		getLease         CoordinatorLease
		failHTTP         bool
		getNotFound      bool
		deferObservation bool
		wantErr          bool
		wantObservations bool
		wantRemoved      bool
		wantClaim        bool
	}{
		{name: "provider failure before acceptance", provider: "aws", failHTTP: true, wantErr: true, wantClaim: true},
		{name: "retained release", provider: "aws", postLease: CoordinatorLease{Provider: "aws", State: "released", ReleaseDeletesServer: &retained}, wantClaim: true},
		{name: "provider cleanup pending past wait", provider: "aws", postLease: CoordinatorLease{Provider: "aws", State: "released", CleanupStartedAt: "2026-08-19T00:00:00Z", ReleaseDeletesServer: &deleting}, getLease: CoordinatorLease{Provider: "aws", State: "released", CleanupStartedAt: "2026-08-19T00:00:00Z", ReleaseDeletesServer: &deleting}, wantErr: true, wantObservations: true, wantClaim: true},
		{name: "automatic release defers pending observation", provider: "aws", postLease: CoordinatorLease{Provider: "aws", State: "released", CleanupStartedAt: "2026-08-19T00:00:00Z", ReleaseDeletesServer: &deleting}, deferObservation: true, wantClaim: true},
		{name: "provider cleanup error", provider: "aws", postLease: CoordinatorLease{Provider: "aws", State: "released", CleanupError: "redacted failure", ReleaseDeletesServer: &deleting}, wantErr: true, wantClaim: true},
		{name: "provider cleanup retry scheduled", provider: "aws", postLease: CoordinatorLease{Provider: "aws", State: "released", CleanupRetryAt: "2026-08-19T00:05:00Z", ReleaseDeletesServer: &deleting}, wantErr: true, wantClaim: true},
		{name: "unknown cleanup status", provider: "aws", postLease: CoordinatorLease{Provider: "aws", State: "released", CleanupStatus: "unknown"}, wantErr: true, wantClaim: true},
		{name: "complete status cannot hide cleanup debt", provider: "aws", postLease: CoordinatorLease{Provider: "aws", State: "released", CleanupStatus: "complete", CleanupError: "provider cleanup remains unconfirmed"}, wantErr: true, wantClaim: true},
		{name: "pending creation never confirms deletion", provider: "aws", postLease: CoordinatorLease{Provider: "aws", State: "released", CleanupStatus: "pending", ReleaseDeletesServer: &deleting}, getLease: CoordinatorLease{Provider: "aws", State: "released", CleanupStatus: "pending", ReleaseDeletesServer: &deleting}, wantErr: true, wantObservations: true, wantClaim: true},
		{name: "automatic release defers pending creation", provider: "aws", postLease: CoordinatorLease{Provider: "aws", State: "released", CleanupStatus: "pending", ReleaseDeletesServer: &deleting}, deferObservation: true, wantClaim: true},
		{name: "accepted release observation not found", provider: "aws", postLease: CoordinatorLease{Provider: "aws", State: "released", CleanupStartedAt: "2026-08-19T00:00:00Z", ReleaseDeletesServer: &deleting}, getNotFound: true, wantErr: true, wantObservations: true, wantClaim: true},
		{name: "immediate final deletion", provider: "aws", postLease: CoordinatorLease{Provider: "aws", State: "released"}, wantRemoved: true},
		{name: "ownership mismatch", provider: "external", wantErr: true, wantClaim: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateTestUserDirs(t)
			const leaseID = "cbx_abcdef123456"
			keyPath, err := testboxKeyPath(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := claimLeaseTargetForConfig(leaseID, "release-test", Config{Provider: "aws"}, Server{Provider: "aws"}, SSHTarget{}, time.Hour); err != nil {
				t.Fatal(err)
			}
			var releasePosts, observations atomic.Int32
			var cancel context.CancelFunc
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/release"):
					releasePosts.Add(1)
					if tc.failHTTP {
						cancel()
						http.Error(w, "provider failed", http.StatusBadRequest)
						return
					}
					lease := tc.postLease
					lease.ID = leaseID
					_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
				case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+leaseID:
					observations.Add(1)
					if tc.getNotFound {
						http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
						return
					}
					lease := tc.getLease
					lease.ID = leaseID
					_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			backend := coordinatorReleaseTestBackend(server, io.Discard)
			ctx := context.Background()
			if tc.failHTTP {
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
			}
			err = backend.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: LeaseTarget{
				LeaseID: leaseID, Server: Server{Provider: tc.provider},
			}, DeferProviderCleanupObservation: tc.deferObservation})
			if (err != nil) != tc.wantErr {
				t.Fatalf("release error=%v wantErr=%t", err, tc.wantErr)
			}
			if tc.wantErr && !tc.failHTTP && tc.provider == "aws" && !strings.Contains(err.Error(), "coordinator accepted release") {
				t.Fatalf("post-acceptance error is ambiguous: %v", err)
			}
			_, statErr := os.Stat(keyPath)
			if tc.wantRemoved && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("lease artifacts remain after confirmed release: %v", statErr)
			}
			if !tc.wantRemoved && statErr != nil {
				t.Fatalf("lease artifacts removed after unconfirmed/retained release: %v", statErr)
			}
			_, claimExists, claimErr := readLeaseClaimWithPresence(leaseID)
			if claimErr != nil || claimExists != tc.wantClaim {
				t.Fatalf("claim exists=%t err=%v want=%t", claimExists, claimErr, tc.wantClaim)
			}
			posts, observed := releasePosts.Load(), observations.Load()
			if tc.provider == "external" && (posts != 0 || observed != 0) {
				t.Fatalf("ownership mismatch sent release POSTs=%d observations=%d", posts, observed)
			}
			if tc.provider != "external" && posts != 1 {
				t.Fatalf("release POSTs=%d want 1", posts)
			}
			if (observed > 0) != tc.wantObservations {
				t.Fatalf("observations=%d wantObservations=%t", observed, tc.wantObservations)
			}
		})
	}
}

func TestCoordinatorReleaseCancellationDuringObservationPreservesLocalState(t *testing.T) {
	isolateTestUserDirs(t)
	configureCoordinatorReleaseTestTiming(t, time.Second, 0)
	const leaseID = "cbx_abcdef123456"
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseTargetForConfig(leaseID, "release-test", Config{Provider: "aws"}, Server{Provider: "aws"}, SSHTarget{}, time.Hour); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var releasePosts, observations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deleting := true
		lease := CoordinatorLease{ID: leaseID, Provider: "aws", State: "released", CleanupStartedAt: "2026-08-19T00:00:00Z", ReleaseDeletesServer: &deleting}
		switch r.Method {
		case http.MethodPost:
			releasePosts.Add(1)
		case http.MethodGet:
			observations.Add(1)
			cancel()
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
	}))
	t.Cleanup(server.Close)
	backend := coordinatorReleaseTestBackend(server, io.Discard)
	err = backend.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: Server{Provider: "aws"}}})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "coordinator accepted release") {
		t.Fatalf("error=%v, want accepted-release cancellation", err)
	}
	if posts, observed := releasePosts.Load(), observations.Load(); posts != 1 || observed != 1 {
		t.Fatalf("release POSTs=%d observations=%d want 1/1", posts, observed)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("artifacts removed after canceled observation: %v", err)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || !exists {
		t.Fatalf("claim exists=%t err=%v, want retained", exists, err)
	}
}

func TestCoordinatorReleaseObservationProviderMismatchFailsClosed(t *testing.T) {
	isolateTestUserDirs(t)
	configureCoordinatorReleaseTestTiming(t, time.Second, 0)
	const leaseID = "cbx_abcdef123456"
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseTargetForConfig(leaseID, "release-test", Config{Provider: "aws"}, Server{Provider: "aws"}, SSHTarget{}, time.Hour); err != nil {
		t.Fatal(err)
	}
	var releasePosts, observations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deleting := true
		lease := CoordinatorLease{ID: leaseID, Provider: "aws", State: "released", CleanupStartedAt: "2026-08-19T00:00:00Z", ReleaseDeletesServer: &deleting}
		if r.Method == http.MethodPost {
			releasePosts.Add(1)
		} else if r.Method == http.MethodGet {
			observations.Add(1)
			lease.Provider = "external"
		} else {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
	}))
	t.Cleanup(server.Close)
	backend := coordinatorReleaseTestBackend(server, io.Discard)
	err = backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: Server{Provider: "aws"}}})
	assertCoordinatorProviderIdentityError(t, err, "external", leaseID)
	if posts, observed := releasePosts.Load(), observations.Load(); posts != 1 || observed != 1 {
		t.Fatalf("release POSTs=%d observations=%d want 1/1", posts, observed)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("artifacts removed after observation mismatch: %v", err)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || !exists {
		t.Fatalf("claim exists=%t err=%v, want retained", exists, err)
	}
}

func TestCoordinatorReleasePreservesRemoteOutcomeWhenLocalArtifactCleanupFails(t *testing.T) {
	isolateTestUserDirs(t)
	const leaseID = "cbx_abcdef123456"
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(keyPath)), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Dir(keyPath)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := claimLeaseTargetForConfig(leaseID, "release-test", Config{Provider: "aws"}, Server{Provider: "aws"}, SSHTarget{}, time.Hour); err != nil {
		t.Fatal(err)
	}
	var releasePosts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/leases/"+leaseID+"/release" {
			releasePosts.Add(1)
		} else if r.Method != http.MethodGet || r.URL.Path != "/v1/leases/"+leaseID {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: leaseID, Provider: "aws", State: "released"}})
	}))
	t.Cleanup(server.Close)
	var stderr bytes.Buffer
	backend := coordinatorReleaseTestBackend(server, &stderr)
	outcome, err := backend.ReleaseLeaseWithOutcome(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{
		LeaseID: leaseID, Server: Server{Provider: "aws"},
	}})
	if !outcome.Terminal || err == nil || !strings.Contains(err.Error(), "remote deletion is confirmed") {
		t.Errorf("remote outcome=%+v error=%v, want confirmed deletion with visible local cleanup debt", outcome, err)
	}
	if !strings.Contains(stderr.String(), "local SSH artifact cleanup failed") {
		t.Fatalf("cleanup failure warning missing: %q", stderr.String())
	}
	if info, err := os.Lstat(filepath.Dir(keyPath)); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("unsafe path was followed or removed: info=%v err=%v", info, err)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || !exists {
		t.Fatalf("claim exists=%t err=%v, want retained for local cleanup retry", exists, err)
	}
	if err := os.Remove(filepath.Dir(keyPath)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: resolved}); err != nil {
		t.Fatalf("retry local cleanup: %v", err)
	}
	if releasePosts.Load() != 1 {
		t.Fatalf("local cleanup retry repeated provider release: requests=%d", releasePosts.Load())
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("local cleanup retry left claim: exists=%t err=%v", exists, err)
	}
}

func TestCoordinatorReleaseConfirmationIsBoundToItsLookup(t *testing.T) {
	for _, change := range []string{"unchanged", "backend", "lease"} {
		t.Run(change, func(t *testing.T) {
			isolateTestUserDirs(t)
			const original = "cbx_001122334455"
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/leases/"), "/release")
				if r.Method == http.MethodPost {
					posts.Add(1)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: id, Provider: "aws", State: "released", CleanupStatus: "complete"}})
			}))
			t.Cleanup(server.Close)
			backend := coordinatorReleaseTestBackend(server, io.Discard)
			lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: original, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "backend":
				backend = coordinatorReleaseTestBackend(server, io.Discard)
			case "lease":
				lease.LeaseID = "cbx_66778899aabb"
			}
			if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
				t.Fatal(err)
			}
			want := int32(1)
			if change == "unchanged" {
				want = 0
			}
			if posts.Load() != want {
				t.Fatalf("provider release requests=%d want=%d", posts.Load(), want)
			}
		})
	}
}

func coordinatorReleaseTestServer(t *testing.T, lease func() CoordinatorLease) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/release") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease()})
	}))
	t.Cleanup(server.Close)
	return server
}

func coordinatorReleaseTestBackend(server *httptest.Server, stderr io.Writer) *coordinatorLeaseBackend {
	cfg := Config{Provider: "aws", TargetOS: targetLinux}
	return &coordinatorLeaseBackend{
		cfg: cfg, spec: ProviderSpec{Name: "aws"},
		coord: &CoordinatorClient{BaseURL: server.URL, Client: server.Client()},
		rt:    Runtime{Stderr: stderr},
	}
}

func configureCoordinatorReleaseTestTiming(t *testing.T, timeout, cadence time.Duration) {
	t.Helper()
	// Coordinator requests otherwise infer the owner with a git subprocess. Keep
	// short synthetic observation deadlines independent of process startup time.
	t.Setenv("CRABBOX_OWNER", "test@example.com")
	originalBackoff := coordinatorReleaseBackoff
	originalTimeout := coordinatorReleaseCompletionTimeout
	originalCadence := coordinatorReleaseObservationCadence
	coordinatorReleaseBackoff = func(int) time.Duration { return 0 }
	coordinatorReleaseCompletionTimeout = timeout
	coordinatorReleaseObservationCadence = func(int) time.Duration { return cadence }
	t.Cleanup(func() {
		coordinatorReleaseBackoff = originalBackoff
		coordinatorReleaseCompletionTimeout = originalTimeout
		coordinatorReleaseObservationCadence = originalCadence
	})
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}

func sshKeyWithoutComment(value string) string {
	fields := strings.Fields(value)
	return strings.Join(fields[:2], " ")
}
