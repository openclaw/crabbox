package ssh

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestStaticSSHStopCommandCleanup(t *testing.T) {
	for _, tt := range []struct {
		name, command, target   string
		readFails, cleanupFails bool
	}{
		{name: "linux", command: "stop", target: core.TargetLinux},
		{name: "release-alias", command: "release", target: core.TargetLinux},
		{name: "state-read-failure", command: "stop", target: core.TargetLinux, readFails: true},
		{name: "cleanup-failure", command: "stop", target: core.TargetLinux, readFails: true, cleanupFails: true},
		{name: "macos", command: "stop", target: core.TargetMacOS},
		{name: "native-windows", command: "stop", target: core.TargetWindows},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newStaticCommandFixture(t, tt.target)
			initial := f.acquire(t)
			f.setFailures(t, tt.readFails, tt.cleanupFails)
			stderr := f.run(t, tt.command, "--id", f.cfg.Static.ID)
			markerClaim := f.assertCleanup(t, true, tt.cleanupFails, stderr)
			if !reflect.DeepEqual(markerClaim, initial) {
				t.Fatalf("marker did not observe the exact acquired claim: got=%#v want=%#v", markerClaim, initial)
			}
			if tt.readFails && !slices.ContainsFunc(f.commands(t), func(c staticCommandRecord) bool {
				return strings.Contains(c.Command, f.cfg.Static.ID+".env") && c.ExitCode == 255
			}) {
				t.Fatal("fixture did not fail the hydration-state read")
			}
		})
	}
}

func TestStaticSSHRunCommandReleasePolicy(t *testing.T) {
	for _, tt := range []struct {
		name               string
		keep, cleanupFails bool
	}{
		{name: "one-shot"},
		{name: "one-shot-cleanup-failure", cleanupFails: true},
		{name: "kept", keep: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newStaticCommandFixture(t, core.TargetLinux)
			f.setFailures(t, false, tt.cleanupFails)
			args := []string{"--no-sync", "--no-hydrate"}
			if tt.keep {
				args = append(args, "--keep")
			}
			args = append(args, "--", "printf", "static-command-fixture")
			stderr := f.run(t, "run", args...)
			f.assertCleanup(t, !tt.keep, tt.cleanupFails, stderr)
			commands := f.commands(t)
			if !slices.ContainsFunc(commands, func(c staticCommandRecord) bool {
				return strings.Contains(c.Command, "static-command-fixture")
			}) {
				t.Fatal("run never reached the user command")
			}
			ownerReleases := 0
			cleanupSeen := false
			for _, record := range commands {
				if strings.Contains(record.Command, f.cfg.Static.ID+".stop") {
					cleanupSeen = true
				}
				if !strings.Contains(record.Command, "protocol_action='release'") {
					continue
				}
				ownerReleases++
				var ownerClaim *core.LeaseClaim
				if err := json.Unmarshal(record.Claim, &ownerClaim); err != nil {
					t.Fatalf("decode owner-release claim snapshot: %v", err)
				}
				if !tt.keep && (!cleanupSeen || ownerClaim != nil) {
					t.Fatal("workspace owner released before connection cleanup and local unclaiming")
				}
				if tt.keep && ownerClaim == nil {
					t.Fatal("kept run removed its claim before releasing workspace ownership")
				}
			}
			if ownerReleases != 1 {
				t.Fatalf("workspace owner releases=%d, want exactly one", ownerReleases)
			}
		})
	}
}

type staticCommandFixture struct {
	cfg                    Config
	repo, logPath, keyPath string
	other                  core.LeaseClaim
}

const staticCommandKey = "synthetic configured key fixture, not a credential\n"

func newStaticCommandFixture(t *testing.T, targetOS string) staticCommandFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("recording executable wrapper requires /bin/sh; Windows targets are tested from POSIX")
	}
	dir := t.TempDir()
	// No ambient config, credentials, Git routing, or executable search paths
	// may escape this fixture, including in the recording SSH subprocess.
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "CRABBOX_CONFIG_DIR", "TMPDIR", "TMP", "TEMP"} {
		path := filepath.Join(dir, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(name, path)
	}
	t.Setenv("CRABBOX_LIVE", "0")
	t.Setenv("CRABBOX_EGRESS_DAEMON_TESTCHILD", "0")
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	f := staticCommandFixture{
		cfg:     core.BaseConfig(),
		repo:    filepath.Join(dir, "repo"),
		logPath: filepath.Join(dir, "ssh.jsonl"),
		keyPath: filepath.Join(dir, "configured-key"),
	}
	if err := os.Mkdir(f.repo, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(f.repo)
	configPath := filepath.Join(dir, "config.yaml")
	writeStaticCommandFile(t, configPath, "{}\n", 0o600)
	writeStaticCommandFile(t, f.keyPath, staticCommandKey, 0o600)
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_PROVIDER", staticProvider)
	t.Setenv("CRABBOX_SSH_KEY", f.keyPath)
	f.cfg.Provider = staticProvider
	f.cfg.TargetOS = targetOS
	f.cfg.SSHKey = f.keyPath
	f.cfg.Static.ID = "static_command_fixture"
	f.cfg.Static.Name = "command-fixture"
	f.cfg.Static.Host = "build.example.ts.net"
	f.cfg.Static.User = "runner"
	f.cfg.Static.Port = "22"
	f.cfg.Static.WorkRoot = "/work/fixture"
	if targetOS == core.TargetWindows {
		f.cfg.Static.WorkRoot = `C:\fixture`
	}
	t.Setenv("CRABBOX_STATIC_ID", f.cfg.Static.ID)
	t.Setenv("CRABBOX_STATIC_NAME", f.cfg.Static.Name)

	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("CRABBOX_STATIC_TEST_BINARY", exe)
	t.Setenv("CRABBOX_STATIC_TEST_LOG", f.logPath)
	t.Setenv("CRABBOX_STATIC_TEST_CLAIM", filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims", f.cfg.Static.ID+".json"))
	writeStaticCommandFile(t, filepath.Join(bin, "ssh"), `#!/bin/sh
CRABBOX_STATIC_TEST_HELPER=1 exec "$CRABBOX_STATIC_TEST_BINARY" -test.run='^TestStaticSSHCommandTransport$' -- "$@"
`, 0o700)
	writeStaticCommandFile(t, filepath.Join(bin, "git"), "#!/bin/sh\nexit 1\n", 0o700)
	oldWait := waitForSSH
	waitForSSH = func(_ context.Context, target *SSHTarget, _ io.Writer) error {
		// Core waits again after acquisition. Fake ssh alone does not prevent
		// its direct TCP probe; this flag routes readiness through fake ssh too.
		target.SSHConfigProxy = true
		return nil
	}
	t.Cleanup(func() { waitForSSH = oldWait })

	otherCfg := f.cfg
	otherCfg.Static.ID = "static_unrelated_fixture"
	otherCfg.Static.Name = "unrelated-fixture"
	otherCfg.Static.Host = "unrelated.invalid"
	backend := NewStaticSSHLeaseBackend(Provider{}.Spec(), otherCfg, Runtime{Stderr: io.Discard})
	if _, err := backend.(core.SSHLeaseBackend).Acquire(t.Context(), AcquireRequest{Repo: core.Repo{Root: f.repo}, Keep: true}); err != nil {
		t.Fatal(err)
	}
	f.other = readStaticCommandClaim(t, otherCfg.Static.ID)
	return f
}

func (f staticCommandFixture) acquire(t *testing.T) core.LeaseClaim {
	t.Helper()
	backend := NewStaticSSHLeaseBackend(Provider{}.Spec(), f.cfg, Runtime{Stderr: io.Discard})
	if _, err := backend.(core.SSHLeaseBackend).Acquire(t.Context(), AcquireRequest{Repo: core.Repo{Root: f.repo}, Keep: true}); err != nil {
		t.Fatal(err)
	}
	return readStaticCommandClaim(t, f.cfg.Static.ID)
}

func (f staticCommandFixture) setFailures(t *testing.T, readFails, cleanupFails bool) {
	t.Helper()
	t.Setenv("CRABBOX_STATIC_TEST_READ_FAIL", fmt.Sprint(readFails))
	t.Setenv("CRABBOX_STATIC_TEST_CLEANUP_FAIL", fmt.Sprint(cleanupFails))
}

func (f staticCommandFixture) run(t *testing.T, command string, args ...string) string {
	t.Helper()
	argv := []string{command, "--provider", "ssh", "--target", f.cfg.TargetOS,
		"--static-host", f.cfg.Static.Host, "--static-user", f.cfg.Static.User,
		"--static-port", f.cfg.Static.Port, "--static-work-root", f.cfg.Static.WorkRoot}
	var stdout, stderr bytes.Buffer
	if err := (core.App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader("")}).Run(t.Context(), append(argv, args...)); err != nil {
		t.Fatalf("%s failed: %v\nstdout=%s\nstderr=%s", command, err, &stdout, &stderr)
	}
	return stderr.String()
}

func (f staticCommandFixture) assertCleanup(t *testing.T, released, cleanupFails bool, stderr string) core.LeaseClaim {
	t.Helper()
	var markerClaim core.LeaseClaim
	markers, egress := 0, 0
	marker := ".crabbox/actions/" + f.cfg.Static.ID + ".stop"
	if f.cfg.TargetOS == core.TargetWindows {
		marker = `C:\ProgramData\crabbox\actions\` + f.cfg.Static.ID + ".stop"
	}
	for _, record := range f.commands(t) {
		if strings.Contains(record.Command, marker) {
			markers++
			writes := strings.Contains(record.Command, "mkdir -p") && strings.Contains(record.Command, "touch ")
			if f.cfg.TargetOS == core.TargetWindows {
				writes = strings.Contains(record.Command, "New-Item -ItemType Directory") && strings.Contains(record.Command, "New-Item -ItemType File")
			}
			if !writes {
				t.Fatalf("expected a stop-marker write, got %q", record.Command)
			}
			if err := json.Unmarshal(record.Claim, &markerClaim); err != nil {
				t.Fatalf("stop marker attempted without a readable claim: %v", err)
			}
			if markerClaim.LeaseID != f.cfg.Static.ID || markerClaim.Provider != staticProvider || markerClaim.StaticHost != f.cfg.Static.Host || markerClaim.RepoRoot != f.repo {
				t.Fatalf("stop marker observed the wrong fixture claim: %#v", markerClaim)
			}
		}
		// Check the cleanup category, not the current broad pkill pattern.
		if strings.Contains(record.Command, "egress client") {
			egress++
		}
		if strings.Contains(record.Command, "tailscale logout") {
			t.Fatal("ordinary static tailnet address triggered Tailscale logout")
		}
	}
	wantMarkers, wantEgress := 0, 0
	if released {
		wantMarkers = 1
		if f.cfg.TargetOS == core.TargetLinux {
			wantEgress = 1
		}
	}
	if markers != wantMarkers || egress != wantEgress {
		t.Fatalf("cleanup attempts: markers=%d egress=%d; want %d/%d", markers, egress, wantMarkers, wantEgress)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(f.cfg.Static.ID); err != nil || exists == released {
		t.Fatalf("claim after command: exists=%t err=%v released=%t", exists, err, released)
	}
	if cleanupFails {
		for _, warning := range []string{"warning: could not stop GitHub Actions hydration", "warning: egress remote client cleanup failed"} {
			if !strings.Contains(stderr, warning) {
				t.Fatalf("missing cleanup warning %q:\n%s", warning, stderr)
			}
		}
	}
	if got := readStaticCommandClaim(t, f.other.LeaseID); !reflect.DeepEqual(got, f.other) {
		t.Fatalf("unrelated fixture claim changed: got=%#v want=%#v", got, f.other)
	}
	if key, err := os.ReadFile(f.keyPath); err != nil || string(key) != staticCommandKey {
		t.Fatalf("configured key was removed or changed: %v", err)
	}
	return markerClaim
}

type staticCommandRecord struct {
	Command  string
	Claim    json.RawMessage
	ExitCode int
}

func (f staticCommandFixture) commands(t *testing.T) []staticCommandRecord {
	t.Helper()
	data, err := os.ReadFile(f.logPath)
	if err != nil {
		t.Fatal(err)
	}
	var records []staticCommandRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var record staticCommandRecord
		if err := decoder.Decode(&record); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

// The fake executable records commands and claim snapshots; it never executes
// remote payloads, reads user SSH config, or opens a network connection.
func TestStaticSSHCommandTransport(t *testing.T) {
	if os.Getenv("CRABBOX_STATIC_TEST_HELPER") != "1" {
		return
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	if slices.Contains(args, "-G") {
		fmt.Print("hostname build.example.ts.net\nproxycommand /usr/bin/false\n")
		os.Exit(0)
	}
	command := args[len(args)-1]
	for depth := 0; depth < 8; depth++ {
		_, rest, ok := strings.Cut(command, `payload_b64="`)
		if !ok {
			break
		}
		payload, _, ok := strings.Cut(rest, `"; decoded=; if command -v base64`)
		if !ok {
			t.Fatal("unrecognized POSIX command wrapper")
		}
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			t.Fatal(err)
		}
		command = string(decoded)
	}
	if _, payload, ok := strings.Cut(command, " -EncodedCommand "); ok {
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil || len(decoded)%2 != 0 {
			t.Fatalf("invalid PowerShell payload: %v", err)
		}
		units := make([]uint16, len(decoded)/2)
		for i := range units {
			units[i] = binary.LittleEndian.Uint16(decoded[i*2:])
		}
		command = string(utf16.Decode(units))
	}
	claim, err := os.ReadFile(os.Getenv("CRABBOX_STATIC_TEST_CLAIM"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	exitCode := 0
	if os.Getenv("CRABBOX_STATIC_TEST_READ_FAIL") == "true" && strings.Contains(command, "static_command_fixture.env") {
		exitCode = 255
	}
	if os.Getenv("CRABBOX_STATIC_TEST_CLEANUP_FAIL") == "true" && (strings.Contains(command, "static_command_fixture.stop") || strings.Contains(command, "egress client")) {
		exitCode = 255
	}
	log, err := os.OpenFile(os.Getenv("CRABBOX_STATIC_TEST_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(log).Encode(staticCommandRecord{Command: command, Claim: claim, ExitCode: exitCode}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
	for action, reply := range map[string]string{"acquire": "ACQUIRED", "renew": "RENEWED", "inspect": "OWNED", "release": "RELEASED"} {
		if strings.Contains(command, "protocol_action='"+action+"'") {
			fmt.Print(reply)
			os.Exit(0)
		}
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func readStaticCommandClaim(t *testing.T, id string) core.LeaseClaim {
	t.Helper()
	claim, exists, err := core.ReadLeaseClaimWithPresence(id)
	if err != nil || !exists {
		t.Fatalf("read claim %s: exists=%t err=%v", id, exists, err)
	}
	return claim
}

func writeStaticCommandFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
