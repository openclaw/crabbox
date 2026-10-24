package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Exercise the actual helper's files through the common production reader and
// its shell framing. Only tailscale, root ownership and the remote filesystem
// location are substituted; no privileged or live guest operations occur.
func TestProxmoxTailnetActualHelperCommonReader(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	dir := t.TempDir()
	script := `import importlib.util, io, json, os, pathlib, subprocess, sys
from unittest.mock import patch
spec = importlib.util.spec_from_file_location('enroll', '../../scripts/proxmox-enroll.py')
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
m.METADATA = pathlib.Path(sys.argv[1])
calls = []
def run(argv, **kwargs):
    calls.append(argv)
    state = {'BackendState':'NeedsLogin', 'Self':None}
    if len(calls) > 1:
        state = {'BackendState':'Running','Self':{'ID':'nFixtureDevice','TailscaleIPs':['100.100.1.2']}}
    return subprocess.CompletedProcess(argv, 0, stdout=json.dumps(state).encode())
os.umask(0o077)
real_fstat = os.fstat
def root_fstat(descriptor):
    values = list(real_fstat(descriptor))
    values[4] = 0
    return os.stat_result(values)
with patch.object(m.os, 'fstat', root_fstat), patch.object(m.os, 'geteuid', return_value=0), patch.object(m.subprocess, 'run', side_effect=run):
    m.enroll(io.StringIO(json.dumps({'authKey':'tskey-auth-fixture','hostname':'cbx-fixture','tags':['tag:worker']})))
`
	if out, err := exec.Command("python3", "-c", script, dir).CombinedOutput(); err != nil {
		t.Fatalf("helper: %v %s", err, out)
	}
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{
		{dir, 0755},
		{filepath.Join(dir, "tailscale-ipv4"), 0644},
		{filepath.Join(dir, "tailscale-hostname"), 0644},
		{filepath.Join(dir, "tailscale-device-id"), 0644},
	} {
		info, err := os.Stat(item.path)
		if err != nil || info.Mode().Perm() != item.mode {
			t.Fatalf("helper receipt must be readable under QGA umask: path=%s info=%v err=%v", item.path, info, err)
		}
	}
	run := func(ctx context.Context, _ SSHTarget, remote string) (string, error) {
		out, err := exec.CommandContext(ctx, "sh", "-c", strings.ReplaceAll(remote, "/var/lib/crabbox", dir)).Output()
		// Match RunSSHOutput's trimming at the transport boundary.
		return strings.TrimSpace(string(out)), err
	}
	for _, ending := range []string{"\n", ""} {
		if ending == "" { // Older writers need not add a trailing newline.
			for _, field := range []string{"ipv4", "hostname", "device-id"} {
				path := filepath.Join(dir, "tailscale-"+field)
				data, _ := os.ReadFile(path)
				if err := os.WriteFile(path, []byte(strings.TrimSpace(string(data))), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		meta, err := readRemoteTailscaleMetadataWith(t.Context(), SSHTarget{}, run)
		if err != nil || meta.IPv4 != "100.100.1.2" || meta.Hostname != "cbx-fixture" || meta.FQDN != "" || meta.ExitNode != "" || meta.Version != "" || meta.DeviceID != "nFixtureDevice" {
			t.Fatalf("metadata %+v err=%v", meta, err)
		}
	}
	if err := os.Remove(filepath.Join(dir, "tailscale-ipv4")); err != nil {
		t.Fatal(err)
	}
	if _, err := readRemoteTailscaleMetadataWith(t.Context(), SSHTarget{}, run); err == nil {
		t.Fatal("missing ipv4 shifted hostname into address")
	}
}

func TestProxmoxTailnetAuthoritativeRouteIgnoresMetadataDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH fixture")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "calls")
	// The common bootstrap, run and status network resolver all use this route.
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+capture+"'\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := tailnetTestConfig()
	server := tailnetTestServer()
	server.PublicNet.IPv4.IP = "100.100.1.2"
	server.Labels["tailscale_ipv4"] = "100.100.1.2"
	for _, fqdn := range []string{"10.77.1.32", "stale.example.test", "100.70.0.9"} {
		server.Labels["tailscale_fqdn"] = fqdn
		server.Labels["tailscale_hostname"] = "10.77.1.33"
		target := SSHTarget{Host: "100.100.1.2", Port: "22", AuthoritativeNetwork: true, NetworkKind: NetworkTailscale, SSHConfigProxy: true}
		bootstrap := bootstrapNetworkTarget(cfg, server, target)
		if bootstrap.Host != target.Host {
			t.Fatalf("bootstrap selected %s", bootstrap.Host)
		}
		for _, mode := range []NetworkMode{NetworkTailscale, NetworkAuto} {
			cfg.Network = mode
			got, err := resolveNetworkTarget(t.Context(), cfg, server, bootstrap)
			if err != nil || got.Target.Host != target.Host || got.Network != NetworkTailscale || got.FallbackReason != "" {
				t.Fatalf("mode=%s target=%+v err=%v", mode, got, err)
			}
		}
		view, err := statusViewFromLeaseTarget(t.Context(), cfg, LeaseTarget{Server: server, SSH: target, LeaseID: server.Labels["lease"]})
		if err != nil || view.SSHHost != target.Host || view.Network != NetworkTailscale {
			t.Fatalf("status=%+v err=%v", view, err)
		}
	}
	data, _ := os.ReadFile(capture)
	if strings.Contains(string(data), "10.77.") || strings.Contains(string(data), "stale.example.test") || strings.Contains(string(data), "100.70.0.9") {
		t.Fatalf("untrusted metadata reached SSH: %s", data)
	}
	cfg.Network = NetworkPublic
	if _, err := resolveNetworkTarget(t.Context(), cfg, server, SSHTarget{Host: "100.100.1.2", AuthoritativeNetwork: true, NetworkKind: NetworkTailscale, SSHConfigProxy: true}); err == nil {
		t.Fatal("public downgrade")
	}
}
