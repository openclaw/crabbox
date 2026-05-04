package cli

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

type SSHTarget struct {
	User          string
	Host          string
	Key           string
	Port          string
	FallbackPorts []string
	TargetOS      string
	WindowsMode   string
	ReadyCheck    string
}

func sshTargetFromConfig(cfg Config, host string) SSHTarget {
	return sshTargetForLease(cfg, host, cfg.SSHUser, cfg.SSHPort)
}

func sshTargetForLease(cfg Config, host, user, port string) SSHTarget {
	if user == "" {
		user = cfg.SSHUser
	}
	if port == "" {
		port = cfg.SSHPort
	}
	return SSHTarget{
		User:          user,
		Host:          host,
		Key:           cfg.SSHKey,
		Port:          port,
		FallbackPorts: cfg.SSHFallbackPorts,
		TargetOS:      cfg.TargetOS,
		WindowsMode:   cfg.WindowsMode,
	}
}

func waitForSSH(ctx context.Context, target *SSHTarget, stderr io.Writer) error {
	return waitForSSHReady(ctx, target, stderr, "bootstrap", 20*time.Minute)
}

func waitForSSHReady(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	start := time.Now()
	deadline := time.Now().Add(timeout)
	for {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if time.Now().After(deadline) {
			return exit(5, "timed out waiting for SSH on %s during %s", target.Host, phase)
		}
		reachablePort := ""
		for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
			probe := *target
			probe.Port = port
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(probe.Host, probe.Port), 5*time.Second)
			if err != nil {
				continue
			}
			_ = conn.Close()
			if reachablePort == "" {
				reachablePort = probe.Port
			}
			if runSSHQuiet(ctx, probe, sshReadyCommand(probe)) == nil {
				if target.Port != probe.Port {
					fmt.Fprintf(stderr, "using ssh port %s for %s (configured %s not ready)\n", probe.Port, target.Host, target.Port)
					target.Port = probe.Port
				}
				return nil
			}
		}
		fmt.Fprintln(stderr, sshWaitProgressMessage(target, phase, reachablePort, time.Since(start), time.Until(deadline)))
		time.Sleep(10 * time.Second)
	}
}

func sshWaitProgressMessage(target *SSHTarget, phase, reachablePort string, elapsed, remaining time.Duration) string {
	if remaining < 0 {
		remaining = 0
	}
	elapsed = elapsed.Round(time.Second)
	remaining = remaining.Round(time.Second)
	if reachablePort != "" {
		return fmt.Sprintf("waiting for %s:%s %s toolchain... elapsed=%s remaining=%s", target.Host, reachablePort, phase, elapsed, remaining)
	}
	return fmt.Sprintf("waiting for %s:%s %s... elapsed=%s remaining=%s", target.Host, target.Port, phase, elapsed, remaining)
}

func probeSSHReady(ctx context.Context, target *SSHTarget, timeout time.Duration) bool {
	if target.Host == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := *target
		probe.Port = port
		dialer := net.Dialer{Timeout: minDuration(timeout, 2*time.Second)}
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(probe.Host, probe.Port))
		if err != nil {
			continue
		}
		_ = conn.Close()
		if runSSHQuietWithOptions(ctx, probe, sshReadyCommand(probe), "2", "1") == nil {
			target.Port = probe.Port
			return true
		}
	}
	return false
}

func probeSSHTransport(ctx context.Context, target *SSHTarget, timeout time.Duration) bool {
	if target.Host == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := *target
		probe.Port = port
		dialer := net.Dialer{Timeout: minDuration(timeout, 2*time.Second)}
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(probe.Host, probe.Port))
		if err != nil {
			continue
		}
		_ = conn.Close()
		if runSSHQuietWithOptions(ctx, probe, sshTransportProbeCommand(probe), "2", "1") == nil {
			target.Port = probe.Port
			return true
		}
	}
	return false
}

func sshTransportProbeCommand(SSHTarget) string {
	return "exit 0"
}

func sshReadyCommand(target SSHTarget) string {
	if target.ReadyCheck != "" {
		return target.ReadyCheck
	}
	if isWindowsNativeTarget(target) {
		return powershellCommand(`$ErrorActionPreference = "Stop"
git --version | Out-Null
tar --version | Out-Null
if (-not (Test-Path -LiteralPath ` + psQuote(targetWindowsReadyRoot(target)) + `)) { throw "work root missing" }`)
	}
	return "test -x /usr/local/bin/crabbox-ready && crabbox-ready >/tmp/crabbox-ready.log 2>&1"
}

func targetWindowsReadyRoot(target SSHTarget) string {
	_ = target
	return `C:\`
}

func sshPortCandidates(port string, fallbackPorts []string) []string {
	if fallbackPorts == nil {
		fallbackPorts = []string{"22"}
	}
	return uniqueSSHPorts(append([]string{port}, fallbackPorts...))
}

func uniqueSSHPorts(ports []string) []string {
	seen := make(map[string]bool, len(ports))
	out := make([]string, 0, len(ports))
	for _, port := range ports {
		port = strings.TrimSpace(port)
		if port == "" || seen[port] {
			continue
		}
		seen[port] = true
		out = append(out, port)
	}
	return out
}

func runSSHQuiet(ctx context.Context, target SSHTarget, remote string) error {
	return runSSHQuietWithOptions(ctx, target, remote, "10", "3")
}

func runSSHQuietWithOptions(ctx context.Context, target SSHTarget, remote, connectTimeout, connectionAttempts string) error {
	remote = wrapRemoteForTarget(target, remote)
	cmd := exec.CommandContext(ctx, "ssh", sshArgsWithOptions(target, remote, connectTimeout, connectionAttempts)...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func runSSHOutput(ctx context.Context, target SSHTarget, remote string) (string, error) {
	remote = wrapRemoteForTarget(target, remote)
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(target, remote)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func runSSHCombinedOutput(ctx context.Context, target SSHTarget, remote string) (string, error) {
	remote = wrapRemoteForTarget(target, remote)
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(target, remote)...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func runSSHInputQuiet(ctx context.Context, target SSHTarget, remote, input string) error {
	return runSSHInput(ctx, target, remote, strings.NewReader(input), io.Discard, io.Discard)
}

func runSSHInput(ctx context.Context, target SSHTarget, remote string, input io.Reader, stdout, stderr io.Writer) error {
	remote = wrapRemoteForTarget(target, remote)
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(target, remote)...)
	cmd.Stdin = input
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func runSSHStream(ctx context.Context, target SSHTarget, remote string, stdout, stderr io.Writer) int {
	remote = wrapRemoteForTarget(target, remote)
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(target, remote)...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if ok := asExitError(err, &exitErr); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return status.ExitStatus()
		}
	}
	return 7
}

func sshArgs(target SSHTarget, remote string) []string {
	return sshArgsWithOptions(target, remote, "10", "3")
}

func sshArgsWithOptions(target SSHTarget, remote, connectTimeout, connectionAttempts string) []string {
	return append(sshBaseArgsWithOptions(target, connectTimeout, connectionAttempts),
		target.User+"@"+target.Host,
		remote,
	)
}

func sshBaseArgs(target SSHTarget) []string {
	return sshBaseArgsWithOptions(target, "10", "3")
}

func sshBaseArgsWithOptions(target SSHTarget, connectTimeout, connectionAttempts string) []string {
	return []string{
		"-i", target.Key,
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + sshConfigFileValue(knownHostsFile(target)),
		"-o", "ConnectTimeout=" + connectTimeout,
		"-o", "ConnectionAttempts=" + connectionAttempts,
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=2",
		"-o", "ControlMaster=auto",
		"-o", "ControlPersist=60s",
		"-o", "ControlPath=" + sshControlPath(target),
		"-p", target.Port,
	}
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func knownHostsFile(target SSHTarget) string {
	if target.Key != "" {
		return filepath.Join(filepath.Dir(target.Key), "known_hosts")
	}
	return filepath.Join(os.Getenv("HOME"), ".ssh", "known_hosts")
}

func sshConfigFileValue(path string) string {
	if strings.ContainsAny(path, " \t\"'") {
		return strconv.Quote(path)
	}
	return path
}

func sshControlPath(target SSHTarget) string {
	scope := target.Key
	if scope == "" {
		scope = target.User
	}
	sum := sha1.Sum([]byte(scope))
	return filepath.Join("/tmp", "crabbox-ssh-"+hex.EncodeToString(sum[:4])+"-%C")
}

type rsyncOptions struct {
	Debug             bool
	Delete            bool
	Checksum          bool
	UseFilesFrom      bool
	FilesFrom         []byte
	Timeout           time.Duration
	HeartbeatInterval time.Duration
}

func rsync(ctx context.Context, target SSHTarget, src, dst string, excludes []string, stdout, stderr io.Writer, opts rsyncOptions) error {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	args := []string{
		"-az",
		"-e", strings.Join(shellWords(append([]string{"ssh"}, sshBaseArgs(target)...)), " "),
	}
	if opts.Delete && !opts.UseFilesFrom {
		args = append(args, "--delete")
	}
	if opts.Checksum {
		args = append(args, "--checksum")
	}
	if opts.UseFilesFrom {
		args = append(args, "--files-from=-", "--from0")
	}
	if isWindowsWSL2Target(target) {
		args = append(args, "--rsync-path", "wsl.exe rsync")
	}
	for _, exclude := range excludes {
		args = append(args, "--exclude", exclude)
	}
	if opts.Debug {
		args = append(args, "--stats", "--itemize-changes", "--progress")
	}
	args = append(args, ensureTrailingSlash(src), target.User+"@"+target.Host+":"+dst+"/")
	start := time.Now()
	cmd := exec.CommandContext(ctx, "rsync", args...)
	if opts.UseFilesFrom {
		cmd.Stdin = bytes.NewReader(opts.FilesFrom)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	stopHeartbeat := startSyncHeartbeat(stderr, start, opts.HeartbeatInterval)
	err := cmd.Run()
	stopHeartbeat()
	if ctx.Err() == context.DeadlineExceeded {
		return exit(6, "rsync timed out after %s", opts.Timeout)
	}
	if opts.Debug {
		fmt.Fprintf(stderr, "rsync elapsed=%s checksum=%t delete=%t\n", time.Since(start).Round(time.Millisecond), opts.Checksum, opts.Delete)
	}
	return err
}

func wrapRemoteForTarget(target SSHTarget, remote string) string {
	if isWindowsNativeTarget(target) {
		if strings.HasPrefix(remote, "powershell.exe ") || strings.HasPrefix(remote, "powershell ") {
			return remote
		}
		return powershellCommand(remote)
	}
	if isWindowsWSL2Target(target) {
		return "wsl.exe --exec bash -lc " + shellQuote(remote)
	}
	return remote
}

func startSyncHeartbeat(stderr io.Writer, start time.Time, interval time.Duration) func() {
	if interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Fprintf(stderr, "still syncing after %s...\n", time.Since(start).Round(time.Second))
			}
		}
	}()
	return func() { close(done) }
}

func ensureTrailingSlash(path string) string {
	if strings.HasSuffix(path, "/") {
		return path
	}
	return path + "/"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func powershellCommand(script string) string {
	encoded := base64.StdEncoding.EncodeToString(utf16LE([]byte(script)))
	return "powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand " + encoded
}

func utf16LE(input []byte) []byte {
	encoded := utf16.Encode([]rune(string(input)))
	out := make([]byte, 0, len(encoded)*2)
	for _, unit := range encoded {
		out = append(out, byte(unit), byte(unit>>8))
	}
	return out
}

func remoteCommand(workdir string, env map[string]string, command []string) string {
	return remoteCommandWithEnvFile(workdir, env, "", command)
}

func remoteCommandWithEnvFile(workdir string, env map[string]string, envFile string, command []string) string {
	var b strings.Builder
	writeRemoteCommandPrefix(&b, workdir, env, envFile)
	b.WriteString("bash -lc ")
	b.WriteString(shellQuote(`exec "$@"`))
	b.WriteString(" bash")
	for _, word := range command {
		b.WriteByte(' ')
		b.WriteString(shellQuote(word))
	}
	return b.String()
}

func remoteShellCommand(workdir string, env map[string]string, script string) string {
	return remoteShellCommandWithEnvFile(workdir, env, "", script)
}

func remoteShellCommandWithEnvFile(workdir string, env map[string]string, envFile, script string) string {
	var b strings.Builder
	writeRemoteCommandPrefix(&b, workdir, env, envFile)
	b.WriteString("bash -lc ")
	b.WriteString(shellQuote(script))
	return b.String()
}

func writeRemoteCommandPrefix(b *strings.Builder, workdir string, env map[string]string, envFile string) {
	b.WriteString("cd ")
	b.WriteString(shellQuote(workdir))
	b.WriteString(" && ")
	if envFile != "" {
		b.WriteString("if [ -f ")
		b.WriteString(shellQuote(envFile))
		b.WriteString(" ]; then . ")
		b.WriteString(shellQuote(envFile))
		b.WriteString("; fi && ")
	}
	for k, v := range env {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(shellQuote(v))
		b.WriteByte(' ')
	}
}

func shellWords(words []string) []string {
	out := make([]string, 0, len(words))
	for _, w := range words {
		out = append(out, shellQuote(w))
	}
	return out
}

func remoteMkdir(workdir string) string {
	return "mkdir -p " + shellQuote(workdir)
}

func remoteGitHydrate(workdir, baseRef string) string {
	if baseRef == "" {
		return "true"
	}
	refspec := "+refs/heads/" + baseRef + ":refs/remotes/origin/" + baseRef
	return "cd " + shellQuote(workdir) + " && " +
		"if git rev-parse --is-inside-work-tree >/dev/null 2>&1 && git remote get-url origin >/dev/null 2>&1; then " +
		"git fetch --quiet --unshallow origin " + shellQuote(refspec) + " || git fetch --quiet --depth=1000 origin " + shellQuote(refspec) + " || git fetch --quiet origin " + shellQuote(refspec) + " || git fetch --quiet origin " + shellQuote(baseRef) + " || true; " +
		"fi"
}

func remoteGitHydrateStatus(workdir, baseRef, expectedSHA string) string {
	if baseRef == "" || expectedSHA == "" {
		return "printf ''"
	}
	script := `cd ` + shellQuote(workdir) + ` && ` + remoteSyncMetaDirScript() + `
if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  exit 0
fi
marker="$meta_dir/git-hydrate-base"
remote_sha="$(git rev-parse --verify ` + shellQuote("refs/remotes/origin/"+baseRef+"^{commit}") + ` 2>/dev/null || git rev-parse --verify ` + shellQuote("origin/"+baseRef+"^{commit}") + ` 2>/dev/null || true)"
if [ "$remote_sha" = ` + shellQuote(expectedSHA) + ` ]; then
  if [ -f "$marker" ] && grep -qx ` + shellQuote(baseRef+" "+expectedSHA) + ` "$marker"; then
    printf 'marker base current'
    exit 0
  fi
  printf 'remote base current'
  exit 0
fi
if [ -n "$remote_sha" ] && git merge-base --is-ancestor ` + shellQuote(expectedSHA) + ` "$remote_sha" >/dev/null 2>&1; then
  printf 'remote base contains local'
fi`
	return "bash -lc " + shellQuote(script)
}

func remoteWriteGitHydrateMarker(workdir, baseRef, expectedSHA string) string {
	if baseRef == "" || expectedSHA == "" {
		return "true"
	}
	script := "cd " + shellQuote(workdir) + " && " + remoteSyncMetaDirScript() + "mkdir -p \"$meta_dir\" && printf %s " + shellQuote(baseRef+" "+expectedSHA+"\n") + " > \"$meta_dir/git-hydrate-base\""
	return "bash -lc " + shellQuote(script)
}

func remoteGitSeed(workdir, remoteURL, head string) string {
	remoteURL = normalizeGitRemoteURL(remoteURL)
	if remoteURL == "" || head == "" {
		return "true"
	}
	parent := filepath.ToSlash(filepath.Dir(workdir))
	return "if [ ! -d " + shellQuote(workdir+"/.git") + " ]; then " +
		"mkdir -p " + shellQuote(parent) + "; " +
		"tmp=$(mktemp -d " + shellQuote(parent+"/.seed.XXXXXX") + "); " +
		"if git clone --quiet --filter=blob:none --no-checkout " + shellQuote(remoteURL) + " \"$tmp\" >/dev/null 2>&1; then " +
		"(cd \"$tmp\" && (git fetch --quiet --depth=1 origin " + shellQuote(head) + " || true) && (git checkout --quiet " + shellQuote(head) + " || git checkout --quiet FETCH_HEAD || true)); " +
		"rm -rf " + shellQuote(workdir) + " && mv \"$tmp\" " + shellQuote(workdir) + "; " +
		"else rm -rf \"$tmp\"; fi; " +
		"fi"
}

func normalizeGitRemoteURL(remoteURL string) string {
	if strings.HasPrefix(remoteURL, "git@github.com:") {
		return "https://github.com/" + strings.TrimSuffix(strings.TrimPrefix(remoteURL, "git@github.com:"), ".git") + ".git"
	}
	return remoteURL
}

func remoteReadSyncFingerprint(workdir string) string {
	script := "cd " + shellQuote(workdir) + " && " + remoteSyncMetaDirScript() + "cat \"$meta_dir/sync-fingerprint\" 2>/dev/null || true"
	return "bash -lc " + shellQuote(script)
}

func remoteWriteSyncFingerprint(workdir, fingerprint string) string {
	script := "cd " + shellQuote(workdir) + " && " + remoteSyncMetaDirScript() + "mkdir -p \"$meta_dir\" && printf %s " + shellQuote(fingerprint) + " > \"$meta_dir/sync-fingerprint\""
	return "bash -lc " + shellQuote(script)
}

func remoteWriteSyncManifestNew(workdir string) string {
	script := "cd " + shellQuote(workdir) + " && " + remoteSyncMetaDirScript() + "mkdir -p \"$meta_dir\" && cat > \"$meta_dir/sync-manifest.new\""
	return "bash -lc " + shellQuote(script)
}

func remoteWriteSyncDeletedNew(workdir string) string {
	script := "cd " + shellQuote(workdir) + " && " + remoteSyncMetaDirScript() + "mkdir -p \"$meta_dir\" && cat > \"$meta_dir/sync-deleted.new\""
	return "bash -lc " + shellQuote(script)
}

func remotePruneSyncManifest(workdir string) string {
	script := "set -e\ncd " + shellQuote(workdir) + `
` + remoteSyncMetaDirScript() + `
old="$meta_dir/sync-manifest"
new="$meta_dir/sync-manifest.new"
deleted="$meta_dir/sync-deleted.new"
delete_paths() {
  while IFS= read -r -d '' rel; do
    case "$rel" in ''|/*|../*|*/../*) continue ;; esac
    rm -f -- "$rel"
    dir=$(dirname -- "$rel")
    while [ "$dir" != . ] && [ "$dir" != / ]; do
      rmdir -- "$dir" 2>/dev/null || break
      dir=$(dirname -- "$dir")
    done
  done
}
manifest_removed_paths() {
  python3 - "$old" "$new" <<'PY'
import pathlib
import sys

def read_manifest(path):
    try:
        data = pathlib.Path(path).read_bytes()
    except FileNotFoundError:
        return []
    return [entry for entry in data.split(b"\0") if entry]

old = read_manifest(sys.argv[1])
new = set(read_manifest(sys.argv[2]))
sys.stdout.buffer.write(b"".join(entry + b"\0" for entry in old if entry not in new))
PY
}
if [ -f "$deleted" ]; then delete_paths < "$deleted"; fi
if [ -f "$old" ] && [ -f "$new" ]; then manifest_removed_paths | delete_paths; fi
`
	return "bash -lc " + shellQuote(script)
}

func remoteApplySyncManifest(workdir string) string {
	script := "set -e; cd " + shellQuote(workdir) + "; " + remoteSyncMetaDirScript() + "mkdir -p \"$meta_dir\"; new=\"$meta_dir/sync-manifest.new\"; deleted=\"$meta_dir/sync-deleted.new\"; rm -f \"$deleted\"; mv \"$new\" \"$meta_dir/sync-manifest\""
	return "bash -lc " + shellQuote(script)
}

func remoteSyncMetaDirScript() string {
	return "meta_dir=$(if [ -d .git ]; then printf %s .git/crabbox; else printf %s .crabbox; fi); "
}

func remoteSyncSanity(workdir string, allowMassDeletions bool) string {
	allowValue := ""
	if allowMassDeletions {
		allowValue = "1"
	}
	return "cd " + shellQuote(workdir) + " && " +
		"if test -d .git && git status --short >/tmp/crabbox-git-status 2>/dev/null; then " +
		"deletions=$(awk '/^ D|^D / { n++ } END { print n+0 }' /tmp/crabbox-git-status); " +
		"if [ " + shellQuote(allowValue) + " != '1' ] && [ \"$deletions\" -ge 200 ]; then " +
		"echo \"remote sync sanity failed: $deletions tracked deletions\" >&2; " +
		"awk '/^ D|^D / { print \"  \" substr($0,4) }' /tmp/crabbox-git-status | head -20 >&2; " +
		"exit 66; " +
		"fi; " +
		"fi"
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if asExitError(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return status.ExitStatus()
		}
	}
	return 1
}

func parseServerID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil
}
