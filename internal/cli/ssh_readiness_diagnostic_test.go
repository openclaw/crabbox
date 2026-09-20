package cli

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A guest missing the readiness baseline reaches SSH and authenticates; only
// the readiness command fails. Reporting that as an unknown-authentication
// transport timeout sends operators after the wrong layer.
func TestWaitForSSHReadyTimeoutReportsProvenAuthentication(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell ssh fixture")
	}
	// A Parallels lease with a configured host reaches its guest through a
	// ProxyCommand, so the proxy route has to report this as accurately as the
	// direct one.
	for _, proxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "proxy"}[proxy], func(t *testing.T) {
			waitForSSHReadyProvenAuthentication(t, proxy)
		})
	}
}

func waitForSSHReadyProvenAuthentication(t *testing.T, proxy bool) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
for remote; do :; done
if [ "$remote" = "exit 0" ]; then exit 0; fi
printf 'sh: node: command not found\n' >&2
exit 127
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	// A Parallels macOS lease probes exactly one port, so no fallback noise.
	target := SSHTarget{User: "runner", Host: host, Port: port, FallbackPorts: []string{}, TargetOS: targetMacOS, NoControlMaster: true}
	wantPorts := port + ":ready"
	if proxy {
		target.SSHConfigProxy = true
		wantPorts = "proxy:ready"
	}
	var progress bytes.Buffer
	// Long enough for one full probe iteration to complete and record
	// its evidence before the deadline fires.
	err = waitForSSHReady(t.Context(), &target, &progress, "bootstrap", 2*time.Second)
	if err == nil {
		t.Fatal("readiness wait unexpectedly succeeded")
	}
	message := err.Error()

	// Transport answered on this port, so readiness is the outstanding stage.
	if !strings.Contains(message, "probe=readiness") {
		t.Fatalf("timeout does not blame readiness: %s", message)
	}
	// The transport probe returned 0 on this port, so authentication is proven,
	// not unknown.
	if strings.Contains(message, "authentication=unknown") {
		t.Fatalf("timeout still reports authentication as unknown after a successful transport probe: %s", message)
	}
	if !strings.Contains(message, "authentication=ok") {
		t.Fatalf("timeout does not record proven authentication: %s", message)
	}
	if !strings.Contains(message, wantPorts) {
		t.Fatalf("port status does not mark readiness as the failing stage: %s", message)
	}
}

// The proxy route's transport probe must follow the same contract the direct
// route already does: it owns the active stage while it runs, and a fatal
// result from it stops the wait instead of being retried into a timeout.
func TestWaitForSSHReadyProxyTransportProbeFollowsDirectContract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell ssh fixture")
	}
	for _, tc := range []struct {
		name      string
		transport string
		wantErr   string
		notErr    string
	}{
		{
			// Transport, not readiness, consumed the deadline.
			name:      "blocked transport owns the reported stage",
			transport: "exec sleep 30\n",
			wantErr:   "probe=transport",
			notErr:    "probe=readiness",
		},
		{
			// A host-key rejection cannot recover by waiting.
			name:      "fatal transport error stops the wait",
			transport: "printf 'Host key verification failed.\\n' >&2\nexit 255\n",
			wantErr:   "SSH host-key verification failed",
			notErr:    "timed out waiting for SSH",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := "#!/bin/sh\nfor remote; do :; done\nif [ \"$remote\" = \"exit 0\" ]; then\n" + tc.transport + "fi\nprintf 'sh: node: command not found\\n' >&2\nexit 127\n"
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					conn.Close()
				}
			}()
			host, port, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}

			target := SSHTarget{
				User: "runner", Host: host, Port: port, FallbackPorts: []string{},
				TargetOS: targetMacOS, SSHConfigProxy: true, NoControlMaster: true,
			}
			var progress bytes.Buffer
			err = waitForSSHReady(t.Context(), &target, &progress, "bootstrap", 2*time.Second)
			if err == nil {
				t.Fatal("readiness wait unexpectedly succeeded")
			}
			if got := err.Error(); !strings.Contains(got, tc.wantErr) {
				t.Fatalf("error does not contain %q: %s", tc.wantErr, got)
			} else if strings.Contains(got, tc.notErr) {
				t.Fatalf("error should not contain %q: %s", tc.notErr, got)
			}
		})
	}
}
