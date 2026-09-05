//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/openclaw/crabbox/internal/testutil"
)

const forwardBoundaryUser = "synthetic-forward-token-7bd019"
const forwardBoundaryPassword = "synthetic-desktop-password-32a118"

type forwardBoundaryRecord struct {
	PID                int
	ArgvSecret         bool
	EnvSecret          bool
	ConfigPath         string
	ConfigPrivate      bool
	ConfigContainsUser bool
	Resolved           map[string][]string
	Forwards           []string
	Error              string
	OverrideOK         bool
}

// The test binary itself is the ssh recorder: no shell, scrubber, or simulated
// in-process spawn. Never relay secret-bearing argv to another executable.
func init() {
	if filepath.Base(os.Args[0]) == "ssh" && os.Getenv("CRABBOX_FORWARD_BOUNDARY_FIXTURE") == "1" {
		os.Exit(runForwardBoundaryRecorder())
	}
}

func runForwardBoundaryRecorder() int {
	root := os.Getenv("CRABBOX_FORWARD_BOUNDARY_ROOT")
	r := forwardBoundaryRecord{PID: os.Getpid()}
	r.OverrideOK = os.Getenv("TEST_FORWARD_OVERRIDE") == "target-value"
	for _, arg := range os.Args {
		r.ArgvSecret = r.ArgvSecret || strings.Contains(arg, forwardBoundaryUser) || strings.Contains(arg, forwardBoundaryPassword)
	}
	// Only persist a synthetic-token presence bit, never the inherited environment.
	for _, entry := range os.Environ() {
		r.EnvSecret = r.EnvSecret || strings.Contains(entry, forwardBoundaryUser) || strings.Contains(entry, forwardBoundaryPassword)
	}
	for i, arg := range os.Args[1:] {
		if i+2 >= len(os.Args) {
			continue
		}
		switch arg {
		case "-F":
			r.ConfigPath = os.Args[i+2]
		case "-L":
			r.Forwards = append(r.Forwards, os.Args[i+2])
		}
	}
	if r.ConfigPath != "" {
		rel, err := filepath.Rel(root, r.ConfigPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return 91 // Refuse to read anything outside this fixture.
		}
		data, readErr := os.ReadFile(r.ConfigPath)
		fileInfo, fileErr := os.Stat(r.ConfigPath)
		dirInfo, dirErr := os.Stat(filepath.Dir(r.ConfigPath))
		r.ConfigPrivate = readErr == nil && fileErr == nil && dirErr == nil && fileInfo.Mode().Perm() == 0o600 && dirInfo.Mode().Perm() == 0o700
		r.ConfigContainsUser = strings.Contains(string(data), forwardBoundaryUser)
	}
	probe := slices.Contains(os.Args[1:], "-G")
	// Real OpenSSH parses only explicit fixture-owned configs. -G never connects
	// or executes ProxyCommand. Fixtures contain no Match exec or external Include.
	if r.ConfigPath != "" && !r.ArgvSecret && !r.EnvSecret {
		args := append([]string{"-G"}, os.Args[1:]...)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/usr/bin/ssh", args...)
		cmd.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=/usr/bin:/bin", "LC_ALL=C"}
		out, err := cmd.Output()
		if err != nil {
			r.Error = "isolated OpenSSH -G failed"
		} else {
			r.Resolved = make(map[string][]string)
			for _, line := range strings.Split(string(out), "\n") {
				key, value, ok := strings.Cut(line, " ")
				if ok {
					r.Resolved[key] = append(r.Resolved[key], value)
				}
			}
			if probe {
				_, _ = os.Stdout.Write(out)
			}
		}
	}
	write := func(name string, value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		path := filepath.Join(root, name)
		if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
			return err
		}
		return os.Rename(path+".tmp", path)
	}
	if probe {
		mode := os.Getenv("CRABBOX_FORWARD_BOUNDARY_MODE")
		if strings.HasPrefix(mode, "partial-") {
			deadline := time.Now().Add(8 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(root, "started.json")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					return 97
				}
				time.Sleep(10 * time.Millisecond)
			}
			if mode == "partial-start" && filepath.Base(r.ConfigPath) == "ssh_config" {
				// Remove only this fixture's executable symlink after the final
				// route probe. Its next real exec.Start must fail, after a prior
				// group has definitely entered the recorder.
				if err := os.Remove(filepath.Join(root, "bin", "ssh")); err != nil {
					return 98
				}
			}
		}
		_ = write(fmt.Sprintf("probe-%d.json", r.PID), r)
		if r.ConfigPath == "" || r.Error != "" || r.ArgvSecret || r.EnvSecret {
			return 92
		}
		return 0
	}
	mode := os.Getenv("CRABBOX_FORWARD_BOUNDARY_MODE")
	if mode == "gate" {
		_ = write("started.json", r)
		for {
			if _, err := os.Stat(filepath.Join(root, "authenticate")); err == nil {
				break
			}
			if _, err := os.Stat(r.ConfigPath); err != nil {
				_ = write("deleted-before-ready.json", true)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if mode == "descendant" || mode == "leader-exit" {
		child := exec.Command(os.Args[0], os.Args[1:]...)
		child.Env = append(childEnvironmentWithout(os.Environ(), "CRABBOX_FORWARD_BOUNDARY_MODE"), "CRABBOX_FORWARD_BOUNDARY_MODE=descendant-child")
		if err := child.Start(); err != nil {
			return 96
		}
		go func() { _ = child.Wait() }()
	}
	if mode != "unready" && mode != "descendant" && mode != "leader-exit" {
		for index, forward := range r.Forwards {
			if mode == "incomplete" && index > 0 {
				break
			}
			parts := strings.Split(forward, ":")
			if len(parts) != 4 || parts[0] != "127.0.0.1" || parts[2] != "127.0.0.1" {
				return 93 // No public listeners, remote traffic, or DNS.
			}
			listener, err := net.Listen("tcp4", net.JoinHostPort(parts[0], parts[1]))
			if err != nil {
				r.Error = "fixture loopback bind failed"
				break
			}
			defer listener.Close()
			go func() {
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					_ = conn.Close()
				}
			}()
		}
	}
	recordName := "started.json"
	if mode == "descendant-child" {
		recordName = "descendant.json"
	}
	if err := write(recordName, r); err != nil {
		return 94
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if r.ConfigPath != "" {
			if _, err := os.Stat(r.ConfigPath); err != nil {
				_ = write("deleted-while-alive.json", true)
			}
		}
		if _, err := os.Stat(filepath.Join(root, "exit")); err == nil && mode != "descendant-child" {
			_ = write("exiting.json", r)
			fmt.Fprintln(os.Stderr, "synthetic SSH failure for "+forwardBoundaryUser)
			return 23
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 95
}

type forwardBoundaryFixture struct {
	root   string
	home   string
	target SSHTarget
}

func newForwardBoundaryFixture(t *testing.T, configRoute bool, mode string) forwardBoundaryFixture {
	t.Helper()
	if _, err := os.Stat("/usr/bin/ssh"); err != nil {
		t.Fatal("this boundary proof requires real local /usr/bin/ssh")
	}
	dirs := testutil.IsolateUserDirs(t)
	root := dirs.Root
	bin := filepath.Join(root, "bin")
	tmp := filepath.Join(root, "tmp")
	for _, dir := range []string{bin, tmp, filepath.Join(dirs.Home, ".ssh")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(bin, "ssh")); err != nil {
		t.Fatal(err)
	}
	// Each route probe starts a race-instrumented test binary. Keep detection
	// enabled without charging its default one-second exit sleep to startup.
	for name, value := range map[string]string{
		"GORACE": strings.TrimSpace(os.Getenv("GORACE") + " atexit_sleep_ms=0"),
		"PATH":   bin + ":/usr/bin:/bin:/usr/sbin:/sbin", "TMPDIR": tmp,
		"CRABBOX_CONFIG": filepath.Join(root, "config.yaml"), "CRABBOX_LIVE": "0",
		"CRABBOX_COORDINATOR": "", "CRABBOX_COORDINATOR_TOKEN": "", "CRABBOX_COORDINATOR_ADMIN_TOKEN": "",
		"CRABBOX_FORWARD_BOUNDARY_FIXTURE": "1", "CRABBOX_FORWARD_BOUNDARY_ROOT": root,
		"CRABBOX_FORWARD_BOUNDARY_MODE": mode, "TEST_FORWARD_DESKTOP_PASSWORD": forwardBoundaryPassword,
		"SSH_AUTH_SOCK": "", "SSH_AGENT_PID": "",
	} {
		t.Setenv(name, value)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("provider: ssh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := SSHTarget{
		Host: "127.0.0.1", Port: "22022", User: forwardBoundaryUser, AuthSecret: true,
		KnownHostsFile:   filepath.Join(root, "known_hosts"),
		ChildEnvDenylist: []string{"TEST_FORWARD_DESKTOP_PASSWORD"},
	}
	for _, name := range []string{"identity", "certificate.pub", "known_hosts"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("synthetic fixture only\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if configRoute {
		target.Host, target.Key, target.SSHConfigProxy = "fixture-route", "", true
		config := "Host fixture-route\n" +
			"  HostName 127.0.0.1\n  Port 22299\n  User ignored-config-user\n" +
			"  IdentityFile " + filepath.Join(root, "identity") + "\n" +
			"  CertificateFile " + filepath.Join(root, "certificate.pub") + "\n" +
			"  IdentityAgent " + filepath.Join(root, "agent.sock") + "\n" +
			"  HostKeyAlias fixture-host-key\n  IdentitiesOnly yes\n" +
			"  ProxyCommand /usr/bin/false %n %h %p\n" +
			"  LocalForward 127.0.0.1:1 127.0.0.1:2\n  RequestTTY force\n" +
			"  RemoteCommand fixture-command-must-not-run\n  ForkAfterAuthentication yes\n" +
			"  ControlMaster auto\n  ControlPersist 10m\n"
		if err := os.WriteFile(filepath.Join(dirs.Home, ".ssh", "config"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return forwardBoundaryFixture{root: root, home: dirs.Home, target: target}
}

func (f forwardBoundaryFixture) waitRecord(t *testing.T) forwardBoundaryRecord {
	t.Helper()
	var record forwardBoundaryRecord
	boundaryEventually(t, "recorder subprocess start", func() bool {
		data, err := os.ReadFile(filepath.Join(f.root, "started.json"))
		return err == nil && json.Unmarshal(data, &record) == nil
	})
	started, identityErr := webVNCDaemonProcessStartIdentity(record.PID)
	t.Cleanup(func() {
		// Last-resort cleanup touches only this recorded test child. Normal tests
		// use the production owner's cancel/stop/Wait path first.
		if current, err := webVNCDaemonProcessStartIdentity(record.PID); identityErr == nil && err == nil && current == started {
			_ = syscall.Kill(record.PID, syscall.SIGKILL)
		}
	})
	if record.Error != "" {
		t.Fatal(record.Error)
	}
	return record
}

func boundaryEventually(t *testing.T, label string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", label)
}

func boundaryPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	_ = listener.Close()
	return port
}

func (f forwardBoundaryFixture) assertStartedBoundary(t *testing.T, r forwardBoundaryRecord) {
	t.Helper()
	if r.ArgvSecret {
		t.Error("recorder subprocess argv exposed the synthetic credential")
	}
	if r.EnvSecret {
		t.Error("recorder subprocess environment exposed the synthetic credential")
	}
	if r.ConfigPath == "" {
		t.Error("recorder subprocess has no private -F config; credential lifetime is unowned")
		return
	}
	if !r.ConfigPrivate || !r.ConfigContainsUser {
		t.Error("ssh did not receive a 0600 config in a 0700 directory containing the resolved synthetic user")
	}
	want := map[string]string{
		"user": forwardBoundaryUser, "hostname": "127.0.0.1", "port": f.target.Port,
		"controlmaster": "false", "controlpersist": "no",
		"forwardagent": "no", "forwardx11": "no", "gatewayports": "no",
		"exitonforwardfailure": "yes", "requesttty": "false", "forkafterauthentication": "no",
	}
	if f.target.SSHConfigProxy {
		want["identityfile"] = filepath.Join(f.root, "identity")
		want["certificatefile"] = filepath.Join(f.root, "certificate.pub")
		want["identityagent"] = filepath.Join(f.root, "agent.sock")
		want["hostkeyalias"] = "fixture-host-key"
		want["proxycommand"] = "/usr/bin/false fixture-route %h %p"
	}
	for key, expected := range want {
		if !slices.Contains(r.Resolved[key], expected) {
			t.Errorf("real OpenSSH -G did not preserve %s policy/routing", key)
		}
	}
	// OpenSSH 10.3 omits ControlPath entirely when its value is none.
	if values := r.Resolved["controlpath"]; len(values) != 0 && (len(values) != 1 || values[0] != "none") {
		t.Error("private route retained a multiplexing socket")
	}
	if len(r.Resolved["localforward"]) != len(r.Forwards) || slices.Contains(r.Resolved["remotecommand"], "fixture-command-must-not-run") {
		t.Error("private route inherited interactive commands/forwards or lost requested forwards")
	}
}

func (f forwardBoundaryFixture) assertAliveBoundary(t *testing.T, r forwardBoundaryRecord) {
	t.Helper()
	f.assertStartedBoundary(t, r)
	if _, err := os.Stat(r.ConfigPath); err != nil {
		t.Error("private config disappeared before attached teardown or detached readiness")
	}
}

func (f forwardBoundaryFixture) assertDetachedBoundary(t *testing.T, r forwardBoundaryRecord) {
	t.Helper()
	f.assertStartedBoundary(t, r)
	if _, err := os.Stat(filepath.Dir(r.ConfigPath)); !os.IsNotExist(err) {
		t.Error("private config directory survived successful detachment")
	}
}

func (f forwardBoundaryFixture) assertReapedBoundary(t *testing.T, r forwardBoundaryRecord, detached ...bool) {
	t.Helper()
	boundaryEventually(t, "ssh process reaped", func() bool { return syscall.Kill(r.PID, 0) == syscall.ESRCH })
	if _, err := os.Stat(filepath.Join(f.root, "deleted-while-alive.json")); len(detached) == 0 && !os.IsNotExist(err) {
		t.Error("private config was removed before ssh exited")
	}
	if r.ConfigPath != "" {
		boundaryEventually(t, "private config removed after reap", func() bool {
			_, err := os.Stat(filepath.Dir(r.ConfigPath))
			return os.IsNotExist(err)
		})
	}
	probes, _ := filepath.Glob(filepath.Join(f.root, "probe-*.json"))
	for _, path := range probes {
		var probe forwardBoundaryRecord
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &probe) != nil || probe.ArgvSecret || probe.EnvSecret || !probe.ConfigPrivate {
			t.Error("route-resolution subprocess crossed the synthetic credential/private-config boundary")
		}
	}
}

func (f forwardBoundaryFixture) exit(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, "exit"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func boundaryResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("production owner did not finish/reap")
		return nil
	}
}

// Positive control: the existing private local-forward owner uses exactly the
// same real recorder and real OpenSSH config-resolution checks as the red tests.
func TestSSHForwardBoundaryPrivateSessionControl(t *testing.T) {
	f := newForwardBoundaryFixture(t, true, "ready")
	f.target.ChildEnv = map[string]string{"TEST_FORWARD_OVERRIDE": "target-value"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	port := boundaryPort(t)
	go func() { result <- runSSHLocalForward(ctx, f.target, port, "5900", io.Discard) }()
	r := f.waitRecord(t)
	f.assertAliveBoundary(t, r)
	if !r.OverrideOK {
		t.Error("SSH local forward lost target environment overrides")
	}
	cancel()
	if err := boundaryResult(t, result); err != nil {
		t.Fatal(err)
	}
	f.assertReapedBoundary(t, r)
}
