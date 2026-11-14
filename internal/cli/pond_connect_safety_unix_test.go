//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func pondSafetyFixture(t *testing.T) forwardBoundaryFixture {
	t.Helper()
	f := newForwardBoundaryFixture(t, false, "ready")
	lease := LeaseTarget{LeaseID: "cbx_safety", SSH: SSHTarget{User: "runner", Host: "203.0.113.10", Port: "22"}, Server: Server{
		Name: "api", CloudID: "cbx_safety", Provider: "forward-boundary",
		Labels: map[string]string{"pond": "alpha", "lease": "cbx_safety", "slug": "api", pondExposedPortsLabelKey: "8080-8081"},
	}}
	p := forwardBoundaryProvider{lease: lease}
	providerRegistry[p.Spec().Name] = p
	t.Cleanup(func() { delete(providerRegistry, p.Spec().Name) })
	if err := ClaimLeaseForRepoProviderScopePond(lease.LeaseID, "api", p.Spec().Name, "", "alpha", f.root, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	return f
}

func pondSafetyCLI(t *testing.T, args ...string) string {
	t.Helper()
	var out, stderr bytes.Buffer
	if err := (App{Stdout: &out, Stderr: &stderr}).Run(context.Background(), args); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, stderr.String())
	}
	return out.String()
}

func pondSafetyFiles(t *testing.T, home string) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, name := range []string{"env", "hosts", "daemon.json"} {
		data, err := os.ReadFile(filepath.Join(home, pondMeshHostsRoot, "alpha", name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		result[name] = string(data)
	}
	return result
}

func pondSafetyReachable(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func TestPondConnectJSONPreservesRunningMapping(t *testing.T) {
	for _, daemon := range []bool{false, true} {
		name := "foreground"
		if daemon {
			name = "daemon"
		}
		t.Run(name, func(t *testing.T) {
			f := pondSafetyFixture(t)
			args := []string{"pond", "connect", "alpha", "--provider", "forward-boundary"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if daemon {
				pondSafetyCLI(t, append(args, "--export")...)
				t.Cleanup(func() { _, _ = stopPondMeshDaemonState(f.home, "alpha") })
			} else {
				done := make(chan error, 1)
				go func() { done <- (App{Stdout: io.Discard, Stderr: io.Discard}).Run(ctx, args) }()
				t.Cleanup(func() {
					cancel()
					if err := <-done; err != nil {
						t.Error(err)
					}
				})
			}
			record := f.waitRecord(t)
			portText := strings.Split(record.Forwards[0], ":")[1]
			port, _ := strconv.Atoi(portText)
			boundaryEventually(t, "forward listener", func() bool { return pondSafetyReachable(port) })
			before := pondSafetyFiles(t, f.home)
			var summary pondMeshSummary
			if err := json.Unmarshal([]byte(pondSafetyCLI(t, append(args, "--json")...)), &summary); err != nil {
				t.Fatal(err)
			}
			if after := pondSafetyFiles(t, f.home); !reflect.DeepEqual(before, after) {
				t.Errorf("JSON changed endpoint files: before=%v after=%v", before, after)
			}
			for _, fwd := range summary.Forwards {
				if !pondSafetyReachable(fwd.LocalPort) {
					t.Errorf("JSON reports unforwarded port %d", fwd.LocalPort)
				}
			}
		})
	}
}

func TestPondConnectJSONDoesNotCreateState(t *testing.T) {
	f := pondSafetyFixture(t)
	args := []string{"pond", "connect", "alpha", "--provider", "forward-boundary", "--json"}
	first := pondSafetyCLI(t, args...)
	if second := pondSafetyCLI(t, args...); second != first {
		t.Errorf("unstable preview: %s vs %s", first, second)
	}
	if _, err := os.Stat(filepath.Join(f.home, pondMeshHostsRoot, "alpha")); !os.IsNotExist(err) {
		t.Errorf("JSON created state directory: %v", err)
	}
}

func TestPondConnectExportRerunKeepsExports(t *testing.T) {
	f := pondSafetyFixture(t)
	args := []string{"pond", "connect", "alpha", "--provider", "forward-boundary", "--export"}
	first := pondSafetyCLI(t, args...)
	t.Cleanup(func() { _, _ = stopPondMeshDaemonState(f.home, "alpha") })
	record := f.waitRecord(t)
	before := pondSafetyFiles(t, f.home)
	second := pondSafetyCLI(t, args...)
	if first != second {
		t.Errorf("exports changed: %s vs %s", first, second)
	}
	if after := pondSafetyFiles(t, f.home); !reflect.DeepEqual(before, after) {
		t.Errorf("running state changed: before=%v after=%v", before, after)
	}
	for _, spec := range record.Forwards {
		port, _ := strconv.Atoi(strings.Split(spec, ":")[1])
		if !pondSafetyReachable(port) {
			t.Errorf("original port %d closed", port)
		}
	}
}

func TestPondConnectInspectionFailureRetainsState(t *testing.T) {
	for _, args := range [][]string{{"pond", "disconnect", "alpha"}, {"pond", "connect", "alpha", "--provider", "forward-boundary", "--export"}} {
		t.Run(args[1], func(t *testing.T) {
			f := pondSafetyFixture(t)
			pondSafetyCLI(t, "pond", "connect", "alpha", "--provider", "forward-boundary", "--export")
			record := f.waitRecord(t)
			before := pondSafetyFiles(t, f.home)
			originalPath := os.Getenv("PATH")
			t.Cleanup(func() { _ = os.Setenv("PATH", originalPath); _, _ = stopPondMeshDaemonState(f.home, "alpha") })
			t.Setenv("PATH", filepath.Join(f.root, "bin"))
			var out bytes.Buffer
			err := (App{Stdout: &out, Stderr: io.Discard}).Run(context.Background(), args)
			if err == nil {
				t.Error("expected visible process inspection error")
			}
			if after := pondSafetyFiles(t, f.home); !reflect.DeepEqual(before, after) {
				t.Errorf("inspection failure changed state: before=%v after=%v", before, after)
			}
			for _, spec := range record.Forwards {
				port, _ := strconv.Atoi(strings.Split(spec, ":")[1])
				if !pondSafetyReachable(port) {
					t.Errorf("unvalidated port %d closed", port)
				}
			}
		})
	}
}

func TestPondConnectExportSkipsUnrelatedListener(t *testing.T) {
	f := pondSafetyFixture(t)
	port, err := allocateLocalForwardPort(map[int]bool{})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	pondSafetyCLI(t, "pond", "connect", "alpha", "--provider", "forward-boundary", "--export")
	t.Cleanup(func() { _, _ = stopPondMeshDaemonState(f.home, "alpha") })
	record := f.waitRecord(t)
	for _, spec := range record.Forwards {
		if strings.Split(spec, ":")[1] == strconv.Itoa(port) {
			t.Fatalf("reused unrelated listener: %s", spec)
		}
	}
}

func TestStopPondMeshDaemonStateInspectionOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		wantError    bool
	}{
		{"confirmed absent", "exit 1", false},
		{"inspection failure", "exit 2", true},
		{"permission failure", "echo denied >&2; exit 1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			path, err := pondMeshDaemonStatePath(home, "alpha", true)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(pondMeshDaemonState{Pond: "alpha", Processes: []pondMeshDaemonProcess{{PID: 12345, Forward: pondMeshForward{LocalPort: 51820, RemotePort: 8080}}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			_, err = stopPondMeshDaemonState(home, "alpha")
			if (err != nil) != tc.wantError {
				t.Errorf("error=%v wantError=%v", err, tc.wantError)
			}
			after, readErr := os.ReadFile(path)
			if tc.wantError {
				if readErr != nil || !bytes.Equal(after, data) {
					t.Errorf("lost unresolved state: %v", readErr)
				}
			} else if !os.IsNotExist(readErr) {
				t.Errorf("stale state not removed: %v", readErr)
			}
		})
	}
}
