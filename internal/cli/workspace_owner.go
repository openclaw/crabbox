package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	workspaceOwnerTTL           = 45 * time.Second
	workspaceOwnerRenewInterval = 10 * time.Second
	workspaceOwnerWaitTimeout   = 2 * time.Minute
	workspaceOwnerPollInterval  = time.Second
	workspaceOwnerProgressEvery = 10 * time.Second
)

type workspaceOwnerAction string

const (
	workspaceOwnerAcquire workspaceOwnerAction = "acquire"
	workspaceOwnerRenew   workspaceOwnerAction = "renew"
	workspaceOwnerInspect workspaceOwnerAction = "inspect"
	workspaceOwnerRelease workspaceOwnerAction = "release"
)

type workspaceOwnerRemoteRequest struct {
	Action workspaceOwnerAction
	Key    string
	Token  string
	TTL    time.Duration
}

type workspaceOwnerTransport interface {
	Do(context.Context, workspaceOwnerRemoteRequest) (string, error)
}

type sshWorkspaceOwnerTransport struct {
	target SSHTarget
}

func (t sshWorkspaceOwnerTransport) Do(ctx context.Context, req workspaceOwnerRemoteRequest) (string, error) {
	remote := remoteWorkspaceOwnerCommand(t.target, req)
	out, err := runSSHCombinedOutput(contextWithoutWorkspaceOwner(ctx), t.target, remote)
	return strings.TrimSpace(out), err
}

type workspaceOwner struct {
	target    SSHTarget
	transport workspaceOwnerTransport
	key       string
	token     string
	ttl       time.Duration
	ctx       context.Context
	cancel    context.CancelFunc
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	mu       sync.Mutex
	renewErr error
}

type workspaceOwnerContextKey struct{}

func contextWithWorkspaceOwner(ctx context.Context, owner *workspaceOwner) context.Context {
	return context.WithValue(ctx, workspaceOwnerContextKey{}, owner)
}

func contextWithoutWorkspaceOwner(ctx context.Context) context.Context {
	return context.WithValue(ctx, workspaceOwnerContextKey{}, (*workspaceOwner)(nil))
}

func workspaceOwnerFromContext(ctx context.Context) *workspaceOwner {
	owner, _ := ctx.Value(workspaceOwnerContextKey{}).(*workspaceOwner)
	return owner
}

func wrapWorkspaceOwnerRemote(ctx context.Context, remote string, preserveInput bool) string {
	owner := workspaceOwnerFromContext(ctx)
	if owner == nil {
		return remote
	}
	if preserveInput {
		return owner.WrapInputCommand(remote)
	}
	return owner.WrapCommand(remote)
}

func workspaceOwnerKey(leaseID string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte("crabbox-workspace-owner-v1\x00"+leaseID)))
}

func shouldAcquireWorkspaceOwner(acquired bool, backend SSHLeaseBackend) bool {
	if !acquired {
		return true
	}
	exclusive, ok := backend.(ExclusiveOneShotAcquireBackend)
	return !ok || !exclusive.AcquireIsExclusiveOneShot()
}

func acquireWorkspaceOwner(ctx context.Context, target SSHTarget, leaseID string, stderr io.Writer) (*workspaceOwner, error) {
	return acquireWorkspaceOwnerWithTransport(ctx, target, leaseID, stderr, sshWorkspaceOwnerTransport{target: target}, workspaceOwnerWaitTimeout, workspaceOwnerTTL, workspaceOwnerRenewInterval)
}

func acquireWorkspaceOwnerWithTransport(ctx context.Context, target SSHTarget, leaseID string, stderr io.Writer, transport workspaceOwnerTransport, waitTimeout, ttl, renewInterval time.Duration) (*workspaceOwner, error) {
	if strings.TrimSpace(leaseID) == "" {
		return nil, exit(7, "workspace owner requires a lease identity")
	}
	if waitTimeout <= 0 {
		waitTimeout = workspaceOwnerWaitTimeout
	}
	if ttl <= 0 {
		ttl = workspaceOwnerTTL
	}
	if renewInterval <= 0 || renewInterval >= ttl {
		renewInterval = ttl / 3
	}
	token, err := randomHex(32)
	if err != nil {
		return nil, exit(7, "create workspace owner fencing token: %v", err)
	}
	owner := &workspaceOwner{
		target:    target,
		transport: transport,
		key:       workspaceOwnerKey(leaseID),
		token:     token,
		ttl:       ttl,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	started := time.Now()
	nextProgress := workspaceOwnerProgressEvery
	for {
		response, callErr := transport.Do(waitCtx, workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: owner.key, Token: owner.token, TTL: ttl})
		if callErr != nil {
			return nil, exit(7, "acquire remote workspace owner: ambiguous remote state: %v", callErr)
		}
		switch response {
		case "ACQUIRED", "RECOVERED":
			owner.ctx, owner.cancel = context.WithCancel(ctx)
			go owner.renewLoop(renewInterval)
			fmt.Fprintf(stderr, "workspace owner acquired wait=%s recovered=%t\n", time.Since(started).Round(time.Millisecond), response == "RECOVERED")
			return owner, nil
		case "BUSY", "CHILD":
			elapsed := time.Since(started)
			if elapsed >= nextProgress {
				fmt.Fprintf(stderr, "waiting for reusable workspace owner elapsed=%s state=%s\n", elapsed.Round(time.Second), strings.ToLower(response))
				nextProgress += workspaceOwnerProgressEvery
			}
		case "AMBIGUOUS":
			return nil, exit(7, "acquire remote workspace owner: ambiguous protocol state")
		default:
			return nil, exit(7, "acquire remote workspace owner: unexpected protocol response %q", response)
		}
		timer := time.NewTimer(workspaceOwnerPollInterval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return nil, exit(7, "timed out after %s waiting for reusable workspace owner", waitTimeout)
			}
			return nil, waitCtx.Err()
		case <-timer.C:
		}
	}
}

func (o *workspaceOwner) Context() context.Context {
	if o == nil || o.ctx == nil {
		return context.Background()
	}
	return o.ctx
}

func (o *workspaceOwner) renewLoop(interval time.Duration) {
	defer close(o.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-o.stop:
			return
		case <-o.ctx.Done():
			return
		case <-ticker.C:
			callCtx, cancel := context.WithTimeout(context.WithoutCancel(o.ctx), interval)
			response, err := o.transport.Do(callCtx, workspaceOwnerRemoteRequest{Action: workspaceOwnerRenew, Key: o.key, Token: o.token, TTL: o.ttl})
			cancel()
			if err == nil && response == "RENEWED" {
				continue
			}
			if err == nil {
				err = fmt.Errorf("unexpected protocol response %q", response)
			}
			o.mu.Lock()
			o.renewErr = exit(7, "remote workspace owner renewal failed closed: %v", err)
			o.mu.Unlock()
			o.cancel()
			return
		}
	}
}

func (o *workspaceOwner) Err() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.renewErr
}

func (o *workspaceOwner) ConfirmNoChild(ctx context.Context) error {
	if o == nil {
		return nil
	}
	if err := o.Err(); err != nil {
		return err
	}
	response, err := o.transport.Do(ctx, workspaceOwnerRemoteRequest{Action: workspaceOwnerInspect, Key: o.key, Token: o.token, TTL: o.ttl})
	if err != nil {
		return exit(7, "confirm remote workspace owner child state: ambiguous remote state: %v", err)
	}
	if response != "OWNED" {
		return exit(7, "confirm remote workspace owner child state failed closed: %s", strings.ToLower(firstNonBlank(response, "ambiguous")))
	}
	return nil
}

func (o *workspaceOwner) WaitForChild(ctx context.Context, timeout time.Duration) error {
	if o == nil {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		response, err := o.transport.Do(ctx, workspaceOwnerRemoteRequest{Action: workspaceOwnerInspect, Key: o.key, Token: o.token, TTL: o.ttl})
		if err != nil {
			return exit(7, "confirm remote workspace phase witness: ambiguous remote state: %v", err)
		}
		switch response {
		case "CHILD":
			return nil
		case "OWNED":
			if time.Now().After(deadline) {
				return exit(7, "timed out waiting for remote workspace phase witness")
			}
		case "MISMATCH", "AMBIGUOUS":
			return exit(7, "confirm remote workspace phase witness failed closed: %s", strings.ToLower(response))
		default:
			return exit(7, "confirm remote workspace phase witness: unexpected protocol response %q", response)
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (o *workspaceOwner) rsyncGuardPayload(destination string) string {
	stop := ".crabbox/workspace-owners/" + o.key + ".rsync-stop." + o.token
	state := ".crabbox/workspace-owners/" + o.key + ".owner"
	return `stop="$HOME/` + stop + `"
state="$HOME/` + state + `"
token=` + shellQuote(o.token) + `
destination=` + shellQuote(destination) + `
while :; do
	state_token=$(sed -n '2p' "$state" 2>/dev/null || true)
	state_expiry=$(sed -n '3p' "$state" 2>/dev/null || true)
	case "$state_expiry" in ''|*[!0-9]*) state_expiry=0 ;; esac
	phase_live=
	if ps -ax -o comm=,command= 2>/dev/null | awk -v dst="$destination" '$1 ~ /(^|\/)rsync$/ && index($0, dst) { found=1 } END { exit !found }'; then phase_live=1; fi
	if [ -f "$stop" ] && [ -z "$phase_live" ]; then exit 0; fi
	if { [ "$state_token" != "$token" ] || [ "$(date +%s)" -ge "$state_expiry" ]; } && [ -z "$phase_live" ]; then exit 0; fi
	sleep 0.2
done`
}

func (o *workspaceOwner) rsyncStopCommand() string {
	path := ".crabbox/workspace-owners/" + o.key + ".rsync-stop." + o.token
	return `touch "$HOME/` + path + `"`
}

func (o *workspaceOwner) rsyncPrepareCommand() string {
	path := ".crabbox/workspace-owners/" + o.key + ".rsync-stop." + o.token
	return `rm -f "$HOME/` + path + `"`
}

func (o *workspaceOwner) Close(ctx context.Context) error {
	if o == nil {
		return nil
	}
	o.closeOnce.Do(func() {
		close(o.stop)
		<-o.done
	})
	renewErr := o.Err()
	response, releaseErr := o.transport.Do(ctx, workspaceOwnerRemoteRequest{Action: workspaceOwnerRelease, Key: o.key, Token: o.token, TTL: o.ttl})
	if releaseErr != nil {
		releaseErr = exit(7, "release remote workspace owner: ambiguous remote state: %v", releaseErr)
	} else if response != "RELEASED" {
		releaseErr = exit(7, "release remote workspace owner failed closed: %s", strings.ToLower(firstNonBlank(response, "ambiguous")))
	}
	return errors.Join(renewErr, releaseErr)
}

func (o *workspaceOwner) CloseAfterLeaseRelease() error {
	if o == nil {
		return nil
	}
	o.closeOnce.Do(func() {
		close(o.stop)
		<-o.done
	})
	return o.Err()
}

func (o *workspaceOwner) WrapCommand(remote string) string {
	return o.wrapCommand(remote, false)
}

func (o *workspaceOwner) WrapInputCommand(remote string) string {
	return o.wrapCommand(remote, true)
}

func (o *workspaceOwner) wrapCommand(remote string, preserveInput bool) string {
	if o == nil {
		return remote
	}
	if isWindowsNativeTarget(o.target) {
		return remoteWorkspaceOwnerWindowsWitness(o.key, o.token, remote, preserveInput)
	}
	return remoteWorkspaceOwnerPOSIXWitness(o.key, o.token, remote, preserveInput)
}

func (o *workspaceOwner) WrapBackgroundCommand(remote string) string {
	if o == nil {
		return remote
	}
	wrapped := o.WrapCommand(remote)
	if isWindowsNativeTarget(o.target) {
		encoded := base64.StdEncoding.EncodeToString(utf16LE([]byte(wrapped)))
		return powershellCommand(`$p = Start-Process -FilePath "powershell.exe" -ArgumentList @("-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", ` + psQuote(encoded) + `) -WindowStyle Hidden -PassThru
Write-Output $p.Id
exit 0
`)
	}
	background := "nohup /bin/sh -c " + shellQuote(wrapped) + " >/dev/null 2>&1 < /dev/null & printf '%s\\n' \"$!\""
	return remoteWorkspaceOwnerPOSIXLauncher(o.key, o.token, background)
}

func remoteWorkspaceOwnerCommand(target SSHTarget, req workspaceOwnerRemoteRequest) string {
	if isWindowsNativeTarget(target) {
		return remoteWorkspaceOwnerWindows(req)
	}
	return remoteWorkspaceOwnerPOSIXLauncher(req.Key, req.Token, remoteWorkspaceOwnerPOSIX(req))
}

func workspaceOwnerTTLSeconds(ttl time.Duration) int64 {
	seconds := int64((ttl + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

func remoteWorkspaceOwnerPOSIX(req workspaceOwnerRemoteRequest) string {
	body := `set -eu
root="$HOME/.crabbox/workspace-owners"
key=` + shellQuote(req.Key) + `
token=` + shellQuote(req.Token) + `
action=` + shellQuote(string(req.Action)) + `
ttl=` + strconv.FormatInt(workspaceOwnerTTLSeconds(req.TTL), 10) + `
state="$root/$key.owner"
child="$root/$key.child"
mkdir -p "$root"
chmod 700 "$HOME/.crabbox" "$root" 2>/dev/null || true
read_state() {
  [ -f "$state" ] || return 1
  state_version=$(sed -n '1p' "$state" 2>/dev/null || true)
  state_token=$(sed -n '2p' "$state" 2>/dev/null || true)
  state_expiry=$(sed -n '3p' "$state" 2>/dev/null || true)
  [ "$state_version" = v1 ] || return 2
  case "$state_token" in ''|*[!0-9a-f]*) return 2 ;; esac
  [ "${#state_token}" -eq 64 ] || return 2
  case "$state_expiry" in ''|*[!0-9]*) return 2 ;; esac
  return 0
}
write_state() {
  next_expiry=$(($(date +%s) + ttl))
  tmp="$state.tmp.$$"
  (umask 077; printf 'v1\n%s\n%s\n' "$token" "$next_expiry" >"$tmp")
  mv "$tmp" "$state"
}
child_status() {
  [ -f "$child" ] || return 1
  child_pid=$(sed -n '1p' "$child" 2>/dev/null || true)
  child_identity=$(sed -n '2p' "$child" 2>/dev/null || true)
  case "$child_pid" in ''|*[!0-9]*) return 2 ;; esac
  [ -n "$child_identity" ] && [ "${#child_identity}" -le 96 ] || return 2
  if kill -0 "$child_pid" 2>/dev/null; then
    live_identity=$(ps -o lstart= -p "$child_pid" 2>/dev/null | tr -s ' ' | sed 's/^ //;s/ $//' | cut -c1-96)
    [ -n "$live_identity" ] || return 2
    [ "$live_identity" = "$child_identity" ] && return 0
  fi
  return 1
}
case "$action" in
  acquire)
    if read_state; then
      now=$(date +%s)
      if [ "$state_expiry" -gt "$now" ]; then printf BUSY; exit 0; fi
      if child_status; then printf CHILD; exit 0; else child_rc=$?; fi
      if [ "$child_rc" -eq 2 ]; then printf AMBIGUOUS; exit 74; fi
      rm -f "$child"
      write_state
      printf RECOVERED
      exit 0
    else state_rc=$?; fi
    if [ "$state_rc" -eq 2 ]; then printf AMBIGUOUS; exit 74; fi
    [ ! -e "$state" ] || { printf AMBIGUOUS; exit 74; }
    write_state
    printf ACQUIRED
    ;;
  renew)
    read_state || { printf AMBIGUOUS; exit 74; }
    [ "$state_token" = "$token" ] || { printf MISMATCH; exit 75; }
    [ "$state_expiry" -gt "$(date +%s)" ] || { printf EXPIRED; exit 75; }
    write_state
    printf RENEWED
    ;;
  inspect)
    read_state || { printf AMBIGUOUS; exit 74; }
    [ "$state_token" = "$token" ] || { printf MISMATCH; exit 75; }
    [ "$state_expiry" -gt "$(date +%s)" ] || { printf EXPIRED; exit 75; }
    if child_status; then printf CHILD; exit 0; else child_rc=$?; fi
    [ "$child_rc" -ne 2 ] || { printf AMBIGUOUS; exit 74; }
    rm -f "$child"
    printf OWNED
    ;;
  release)
    read_state || { printf AMBIGUOUS; exit 74; }
    [ "$state_token" = "$token" ] || { printf MISMATCH; exit 75; }
    if child_status; then printf CHILD; exit 75; else child_rc=$?; fi
    [ "$child_rc" -ne 2 ] || { printf AMBIGUOUS; exit 74; }
    rm -f "$child" "$state"
    [ ! -e "$state" ] || { printf AMBIGUOUS; exit 74; }
    printf RELEASED
    ;;
  *) printf AMBIGUOUS; exit 74 ;;
esac
`
	timeout := "5"
	if req.Action == workspaceOwnerAcquire {
		timeout = "0"
	}
	return `set -u
root="$HOME/.crabbox/workspace-owners"
mkdir -p "$root"
chmod 700 "$HOME/.crabbox" "$root" 2>/dev/null || true
protocol_action=` + shellQuote(string(req.Action)) + `
gate="$root/` + req.Key + `.gate"
body=` + shellQuote(body) + `
run_locked() {
	if command -v flock >/dev/null 2>&1; then
		flock -x -w ` + timeout + ` "$gate" /bin/sh -c "$body"
	elif command -v lockf >/dev/null 2>&1; then
		lockf -t ` + timeout + ` "$gate" /bin/sh -c "$body"
	else
		return 73
	fi
}
set +e
output=$(run_locked)
lock_status=$?
set -e
if [ "$lock_status" -ne 0 ] && [ -z "$output" ]; then
	if [ ` + shellQuote(string(req.Action)) + ` = acquire ]; then printf BUSY; exit 0; fi
	printf AMBIGUOUS; exit 74
fi
printf %s "$output"
exit "$lock_status"
`
}

func remoteWorkspaceOwnerWindows(req workspaceOwnerRemoteRequest) string {
	return powershellCommand(`$ErrorActionPreference = "Stop"
$root = Join-Path $HOME ".crabbox\workspace-owners"
$key = ` + psQuote(req.Key) + `
$token = ` + psQuote(req.Token) + `
$action = ` + psQuote(string(req.Action)) + `
$ttl = ` + strconv.FormatInt(workspaceOwnerTTLSeconds(req.TTL), 10) + `
$state = Join-Path $root ($key + ".owner")
$child = Join-Path $root ($key + ".child")
$gate = Join-Path $root ($key + ".gate")
New-Item -ItemType Directory -Force -Path $root | Out-Null
function Enter-Gate {
	for ($i = 0; $i -lt 50; $i++) {
		try {
			$stream = [IO.File]::Open($gate, [IO.FileMode]::CreateNew, [IO.FileAccess]::ReadWrite, [IO.FileShare]::None)
			$lines = "v1" + [Environment]::NewLine + $PID + [Environment]::NewLine + $token + [Environment]::NewLine
			$bytes = [Text.Encoding]::UTF8.GetBytes($lines)
			$stream.Write($bytes, 0, $bytes.Length)
			$stream.Flush()
			return $stream
		} catch {}
		try {
			$stale = [IO.File]::Open($gate, [IO.FileMode]::Open, [IO.FileAccess]::ReadWrite, [IO.FileShare]::None)
			$stale.Dispose()
			Remove-Item -LiteralPath $gate -Force -ErrorAction Stop
			continue
		} catch {
			if ($action -eq "acquire") { return $null }
			Start-Sleep -Milliseconds 100
		}
	}
	throw "workspace owner gate is ambiguous"
}
function Read-State {
  if (-not (Test-Path -LiteralPath $state -PathType Leaf)) { return $null }
  $lines = @(Get-Content -LiteralPath $state -ErrorAction Stop)
  if ($lines.Count -ne 3 -or $lines[0] -ne "v1" -or $lines[1] -notmatch "^[0-9a-f]{64}$" -or $lines[2] -notmatch "^[0-9]+$") { throw "workspace owner state is ambiguous" }
  return @{ Token = [string]$lines[1]; Expiry = [Int64]$lines[2] }
}
function Write-State {
  $tmp = $state + ".tmp." + [Guid]::NewGuid().ToString("N")
  $expiry = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() + $ttl
  [IO.File]::WriteAllLines($tmp, @("v1", $token, [string]$expiry), [Text.UTF8Encoding]::new($false))
  Move-Item -LiteralPath $tmp -Destination $state -Force
}
function Get-ChildStatus {
  if (-not (Test-Path -LiteralPath $child -PathType Leaf)) { return "dead" }
  $lines = @(Get-Content -LiteralPath $child -ErrorAction Stop)
  if ($lines.Count -ne 2 -or $lines[0] -notmatch "^[0-9]+$" -or $lines[1] -notmatch "^[0-9]+$") { return "ambiguous" }
	try {
		$process = [Diagnostics.Process]::GetProcessById([int]$lines[0])
	} catch [ArgumentException] {
		return "dead"
	} catch {
		return "ambiguous"
	}
	try {
		if ([string]$process.StartTime.ToUniversalTime().Ticks -eq [string]$lines[1]) { return "live" }
		return "dead"
	} catch {
		try { $null = [Diagnostics.Process]::GetProcessById([int]$lines[0]); return "ambiguous" }
		catch [ArgumentException] { return "dead" }
		catch { return "ambiguous" }
	}
}
$gateStream = Enter-Gate
if ($null -eq $gateStream) { Write-Output "BUSY"; exit 0 }
try {
  $current = Read-State
  switch ($action) {
    "acquire" {
      if ($null -eq $current) { Write-State; Write-Output "ACQUIRED"; break }
      if ($current.Expiry -gt [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()) { Write-Output "BUSY"; break }
      $childStatus = Get-ChildStatus
      if ($childStatus -eq "live") { Write-Output "CHILD"; break }
      if ($childStatus -eq "ambiguous") { throw "workspace owner child state is ambiguous" }
      Remove-Item -LiteralPath $child -Force -ErrorAction SilentlyContinue
      Write-State
      Write-Output "RECOVERED"
      break
    }
    "renew" {
      if ($null -eq $current -or $current.Token -ne $token) { Write-Output "MISMATCH"; exit 75 }
      if ($current.Expiry -le [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()) { Write-Output "EXPIRED"; exit 75 }
      Write-State
      Write-Output "RENEWED"
      break
    }
    "inspect" {
      if ($null -eq $current -or $current.Token -ne $token) { Write-Output "MISMATCH"; exit 75 }
      if ($current.Expiry -le [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()) { Write-Output "EXPIRED"; exit 75 }
      $childStatus = Get-ChildStatus
      if ($childStatus -eq "live") { Write-Output "CHILD"; break }
      if ($childStatus -eq "ambiguous") { throw "workspace owner child state is ambiguous" }
      Remove-Item -LiteralPath $child -Force -ErrorAction SilentlyContinue
      Write-Output "OWNED"
      break
    }
    "release" {
      if ($null -eq $current -or $current.Token -ne $token) { Write-Output "MISMATCH"; exit 75 }
      $childStatus = Get-ChildStatus
      if ($childStatus -eq "live") { Write-Output "CHILD"; exit 75 }
      if ($childStatus -eq "ambiguous") { throw "workspace owner child state is ambiguous" }
      Remove-Item -LiteralPath $child -Force -ErrorAction SilentlyContinue
      Remove-Item -LiteralPath $state -Force -ErrorAction Stop
      if (Test-Path -LiteralPath $state) { throw "workspace owner release is ambiguous" }
      Write-Output "RELEASED"
      break
    }
    default { throw "workspace owner action is ambiguous" }
  }
} catch {
	Write-Output "AMBIGUOUS"
	exit 74
} finally {
	$gateStream.Dispose()
	Remove-Item -LiteralPath $gate -Force -ErrorAction SilentlyContinue
}
`)
}

func remoteWorkspaceOwnerPOSIXLauncher(key, token, script string) string {
	return remoteWorkspaceOwnerPOSIXEncodedLauncher(key, token, base64.StdEncoding.EncodeToString([]byte(script)), len(script))
}

func remoteWorkspaceOwnerPOSIXEncodedLauncher(key, token, encoded string, decodedSize int) string {
	launcher := `set -u; umask 077; root="$HOME/.crabbox/workspace-owners"; run_dir="$root/` + key + `.launcher.` + token + `.$$"; script="$run_dir/script"; cleanup_launcher() { rm -f "$script"; rmdir "$run_dir" 2>/dev/null || true; }; decoded_size_ok() { set -- $(wc -c <"$script"); [ "$#" -eq 1 ] && [ "$1" = ` + strconv.Itoa(decodedSize) + ` ]; }; mkdir -p "$root" || exit 74; chmod 700 "$HOME/.crabbox" "$root" 2>/dev/null || true; mkdir -m 700 "$run_dir" || exit 74; payload_b64="` + encoded + `"; decoded=; if command -v base64 >/dev/null 2>&1; then if printf %s "$payload_b64" | base64 --decode >"$script" 2>/dev/null && decoded_size_ok; then decoded=1; elif printf %s "$payload_b64" | base64 -d >"$script" 2>/dev/null && decoded_size_ok; then decoded=1; elif printf %s "$payload_b64" | base64 -D >"$script" 2>/dev/null && decoded_size_ok; then decoded=1; fi; fi; if [ -z "$decoded" ] && command -v openssl >/dev/null 2>&1; then if printf %s "$payload_b64" | openssl base64 -d -A >"$script" 2>/dev/null && decoded_size_ok; then decoded=1; fi; fi; if [ -z "$decoded" ]; then cleanup_launcher; exit 74; fi; /bin/sh "$script"; code=$?; cleanup_launcher; exit "$code"`
	return "exec /bin/sh -c " + shellQuote(launcher)
}

func remoteWorkspaceOwnerPOSIXWitness(key, token, remote string, preserveInput ...bool) string {
	return remoteWorkspaceOwnerPOSIXLauncher(key, token, remoteWorkspaceOwnerPOSIXWitnessScript(key, token, remote, preserveInput...))
}

func remoteWorkspaceOwnerPOSIXWitnessScript(key, token, remote string, preserveInput ...bool) string {
	inputSetup := ""
	inputRedirect := ""
	if len(preserveInput) > 0 && preserveInput[0] {
		inputSetup = `(umask 077; cat >"$run_dir/input") || { rm -rf "$run_dir"; exit 74; }
`
		inputRedirect = ` <"$run_dir/input"`
	}
	installBody := `set -eu
[ "$(sed -n '2p' "$state" 2>/dev/null || true)" = "$token" ] || exit 75
owner_expiry=$(sed -n '3p' "$state" 2>/dev/null || true)
case "$owner_expiry" in ''|*[!0-9]*) exit 74 ;; esac
[ "$owner_expiry" -gt "$(date +%s)" ] || exit 75
if [ -f "$child" ]; then
	existing_pid=$(sed -n '1p' "$child" 2>/dev/null || true)
	existing_identity=$(sed -n '2p' "$child" 2>/dev/null || true)
	case "$existing_pid" in ''|*[!0-9]*) exit 74 ;; esac
	[ -n "$existing_identity" ] && [ "${#existing_identity}" -le 96 ] || exit 74
	if kill -0 "$existing_pid" 2>/dev/null; then
		live_existing_identity=$(ps -o lstart= -p "$existing_pid" 2>/dev/null | tr -s ' ' | sed 's/^ //;s/ $//' | cut -c1-96)
		[ -n "$live_existing_identity" ] || exit 74
		[ "$live_existing_identity" != "$existing_identity" ] || exit 75
	fi
	rm -f "$child"
fi
child_tmp="$child.tmp.$$"
(umask 077; printf '%s\n%s\n' "$child_pid" "$child_identity" >"$child_tmp")
mv "$child_tmp" "$child"`
	clearBody := `set -eu
[ "$(sed -n '2p' "$state" 2>/dev/null || true)" = "$token" ] || exit 75
recorded_pid=$(sed -n '1p' "$child" 2>/dev/null || true)
recorded_identity=$(sed -n '2p' "$child" 2>/dev/null || true)
[ "$recorded_pid" = "$child_pid" ] && [ "$recorded_identity" = "$child_identity" ] || exit 74
rm -f "$child"`
	return `set -u
root="$HOME/.crabbox/workspace-owners"
key=` + shellQuote(key) + `
token=` + shellQuote(token) + `
payload=` + shellQuote(remote) + `
state="$root/$key.owner"
child="$root/$key.child"
gate="$root/$key.gate"
run_dir="$root/$key.run.$token"
start="$run_dir/start"
run_owner_gate() {
	if command -v flock >/dev/null 2>&1; then
		flock -x -w 5 "$gate" /bin/sh -c "$1"
	elif command -v lockf >/dev/null 2>&1; then
		lockf -t 5 "$gate" /bin/sh -c "$1"
	else
		return 74
	fi
}
rm -rf "$run_dir"
mkdir -m 700 "$run_dir" || exit 74
` + inputSetup + `(trap '' HUP; while [ ! -f "$start" ]; do sleep 0.05; done; exec sh -c "$payload"` + inputRedirect + `) &
child_pid=$!
child_identity=$(ps -o lstart= -p "$child_pid" 2>/dev/null | tr -s ' ' | sed 's/^ //;s/ $//' | cut -c1-96)
if [ -z "$child_identity" ] || ! kill -0 "$child_pid" 2>/dev/null; then kill "$child_pid" 2>/dev/null || true; rm -rf "$run_dir"; exit 74; fi
export state child token child_pid child_identity
set +e
run_owner_gate ` + shellQuote(installBody) + `
gate_status=$?
set -e
if [ "$gate_status" -ne 0 ]; then kill "$child_pid" 2>/dev/null || true; rm -rf "$run_dir"; exit "$gate_status"; fi
touch "$start"
set +e
wait "$child_pid"
code=$?
set -e
set +e
run_owner_gate ` + shellQuote(clearBody) + `
clear_status=$?
set -e
rm -rf "$run_dir"
[ "$clear_status" -eq 0 ] || exit "$clear_status"
exit "$code"
`
}

func remoteWorkspaceOwnerWindowsWitness(key, token, remote string, preserveInput ...bool) string {
	payload := base64.StdEncoding.EncodeToString([]byte(remote))
	inputSetup := ""
	inputArgument := ""
	if len(preserveInput) > 0 && preserveInput[0] {
		inputSetup = `$inputPath = Join-Path $runDir "input"
$inputFile = [IO.File]::Open($inputPath, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
try { [Console]::OpenStandardInput().CopyTo($inputFile); $inputFile.Flush() } finally { $inputFile.Dispose() }
`
		inputArgument = ` -RedirectStandardInput $inputPath`
	}
	return powershellCommand(`$ErrorActionPreference = "Stop"
$root = Join-Path $HOME ".crabbox\workspace-owners"
$key = ` + psQuote(key) + `
$token = ` + psQuote(token) + `
$state = Join-Path $root ($key + ".owner")
$child = Join-Path $root ($key + ".child")
$gate = Join-Path $root ($key + ".gate")
$runDir = Join-Path $root ($key + ".run." + $token)
$start = Join-Path $runDir "start"
function Enter-Gate {
	for ($i = 0; $i -lt 50; $i++) {
		try {
			$stream = [IO.File]::Open($gate, [IO.FileMode]::CreateNew, [IO.FileAccess]::ReadWrite, [IO.FileShare]::None)
			$lines = "v1" + [Environment]::NewLine + $PID + [Environment]::NewLine + $token + [Environment]::NewLine
			$bytes = [Text.Encoding]::UTF8.GetBytes($lines)
			$stream.Write($bytes, 0, $bytes.Length)
			$stream.Flush()
			return $stream
		} catch {}
		try {
			$stale = [IO.File]::Open($gate, [IO.FileMode]::Open, [IO.FileAccess]::ReadWrite, [IO.FileShare]::None)
			$stale.Dispose()
			Remove-Item -LiteralPath $gate -Force -ErrorAction Stop
			continue
		} catch { Start-Sleep -Milliseconds 100 }
	}
	return $null
}
function Read-Token {
	$lines = @(Get-Content -LiteralPath $state -ErrorAction Stop)
	if ($lines.Count -ne 3 -or $lines[0] -ne "v1") { throw "ambiguous owner state" }
	return [string]$lines[1]
}
function Read-Expiry {
	$lines = @(Get-Content -LiteralPath $state -ErrorAction Stop)
	if ($lines.Count -ne 3 -or $lines[0] -ne "v1" -or $lines[2] -notmatch "^[0-9]+$") { throw "ambiguous owner state" }
	return [Int64]$lines[2]
}
function Get-ExistingChildStatus {
	if (-not (Test-Path -LiteralPath $child -PathType Leaf)) { return "dead" }
	$lines = @(Get-Content -LiteralPath $child -ErrorAction Stop)
	if ($lines.Count -ne 2 -or $lines[0] -notmatch "^[0-9]+$" -or $lines[1] -notmatch "^[0-9]+$") { return "ambiguous" }
	try { $existing = [Diagnostics.Process]::GetProcessById([int]$lines[0]) }
	catch [ArgumentException] { return "dead" }
	catch { return "ambiguous" }
	try {
		if ([string]$existing.StartTime.ToUniversalTime().Ticks -eq [string]$lines[1]) { return "live" }
		return "dead"
	} catch {
		try { $null = [Diagnostics.Process]::GetProcessById([int]$lines[0]); return "ambiguous" }
		catch [ArgumentException] { return "dead" }
		catch { return "ambiguous" }
	}
}
Remove-Item -LiteralPath $runDir -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Path $runDir -ErrorAction Stop | Out-Null
` + inputSetup + `
$childSource = @'
$ErrorActionPreference = "Stop"
while (-not (Test-Path -LiteralPath '__START__')) { Start-Sleep -Milliseconds 50 }
$payload = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('__PAYLOAD__'))
& ([ScriptBlock]::Create($payload))
exit $LASTEXITCODE
'@
$childSource = $childSource.Replace('__START__', $start.Replace("'", "''")).Replace('__PAYLOAD__', ` + psQuote(payload) + `)
$childEncoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($childSource))
$process = Start-Process -FilePath "powershell.exe" -ArgumentList @("-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", $childEncoded) -NoNewWindow -PassThru` + inputArgument + `
$identity = [string]$process.StartTime.ToUniversalTime().Ticks
$gateStream = Enter-Gate
if ($null -eq $gateStream) { Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue; throw "ambiguous owner gate" }
try {
	if ((Read-Token) -ne $token) { Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue; exit 75 }
	if ((Read-Expiry) -le [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()) { Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue; exit 75 }
	$existingStatus = Get-ExistingChildStatus
	if ($existingStatus -eq "live") { Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue; exit 75 }
	if ($existingStatus -eq "ambiguous") { Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue; exit 74 }
	Remove-Item -LiteralPath $child -Force -ErrorAction SilentlyContinue
	$tmp = $child + ".tmp." + [Guid]::NewGuid().ToString("N")
	[IO.File]::WriteAllLines($tmp, @([string]$process.Id, $identity), [Text.UTF8Encoding]::new($false))
	Move-Item -LiteralPath $tmp -Destination $child -Force
} finally { $gateStream.Dispose(); Remove-Item -LiteralPath $gate -Force -ErrorAction SilentlyContinue }
New-Item -ItemType File -Path $start -Force | Out-Null
$process.WaitForExit()
$code = $process.ExitCode
$clear = $false
$gateStream = Enter-Gate
if ($null -ne $gateStream) {
	try {
		$recorded = @(Get-Content -LiteralPath $child -ErrorAction Stop)
    if ((Read-Token) -eq $token -and $recorded.Count -eq 2 -and $recorded[0] -eq [string]$process.Id -and $recorded[1] -eq $identity) {
      Remove-Item -LiteralPath $child -Force -ErrorAction Stop
      $clear = $true
    }
	} finally { $gateStream.Dispose(); Remove-Item -LiteralPath $gate -Force -ErrorAction SilentlyContinue }
}
Remove-Item -LiteralPath $runDir -Recurse -Force -ErrorAction SilentlyContinue
if (-not $clear) { exit 74 }
exit $code
`)
}
