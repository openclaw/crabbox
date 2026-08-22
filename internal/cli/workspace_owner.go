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
	workspaceOwnerReconcileWait = 250 * time.Millisecond
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
	ctx = contextWithoutWorkspaceOwner(ctx)
	remote := remoteWorkspaceOwnerCommand(t.target, req)
	var input *string
	if isWindowsWSL2Target(t.target) {
		script := remote
		input = &script
		remote = wsl2StdinScriptCommandWithWaitTimeout(len(script), 0)
	} else if isWindowsNativeTarget(t.target) {
		script := remoteWorkspaceOwnerWindows(req)
		input = &script
		remote = windowsPowerShellStdinScriptCommand(len([]byte(script)))
	}
	return runWorkspaceOwnerSSHProtocol(ctx, t.target, remote, input)
}

func runWorkspaceOwnerSSHProtocol(ctx context.Context, target SSHTarget, remote string, input *string) (string, error) {
	var lastOutput string
	var lastErr error
	for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
		probe := target
		probe.Port = port
		probe.FallbackPorts = []string{}
		args := sshArgsNoInput(probe, remote)
		if input != nil {
			args = sshArgs(probe, remote)
		}
		cmd := sshCommandContext(ctx, probe, args...)
		if input != nil {
			cmd.Stdin = strings.NewReader(*input)
		}
		var stdout, stderr synchronizedBuffer
		err := runSSHCommand(cmd, &stdout, &stderr)
		output := strings.TrimSpace(stdout.String())
		if err == nil {
			return output, nil
		}
		detail := trimFailureDetail(RedactDiagnosticSecrets(stderr.String()))
		if detail != "" {
			err = fmt.Errorf("%w: %s", err, detail)
		}
		lastOutput, lastErr = output, err
		if !shouldRetrySSHPort(err) {
			return output, err
		}
	}
	return lastOutput, lastErr
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

	mu             sync.Mutex
	renewErr       error
	confirmedUntil time.Time
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

type workspaceOwnerRemotePreparation struct {
	command string
	cleanup string
	name    string
}

const (
	workspaceOwnerCleanupTimeout         = 30 * time.Second
	workspaceOwnerCanceledCleanupTimeout = 5 * time.Second
)

type workspaceOwnerCleanupRunner func(context.Context, SSHTarget, string) error

func prepareWorkspaceOwnerRemote(ctx context.Context, target SSHTarget, remote string, inputSize *int64) (workspaceOwnerRemotePreparation, error) {
	owner := workspaceOwnerFromContext(ctx)
	if owner == nil {
		return workspaceOwnerRemotePreparation{command: remote}, nil
	}
	if !isWindowsNativeTarget(target) {
		return workspaceOwnerRemotePreparation{command: owner.wrapPOSIXCommand(remote, inputSize != nil)}, nil
	}
	return stageWorkspaceOwnerWindowsWitness(contextWithoutWorkspaceOwner(ctx), target, owner, remote, inputSize, false)
}

func (p workspaceOwnerRemotePreparation) close(ctx context.Context, target SSHTarget) error {
	return p.closeWithRunner(ctx, target, runSSHQuiet)
}

func (p workspaceOwnerRemotePreparation) closeWithRunner(ctx context.Context, target SSHTarget, run workspaceOwnerCleanupRunner) error {
	timeout := workspaceOwnerCleanupTimeout
	if ctx.Err() != nil {
		timeout = workspaceOwnerCanceledCleanupTimeout
	}
	return p.closeWithin(ctx, target, timeout, run)
}

func (p workspaceOwnerRemotePreparation) closeWithin(ctx context.Context, target SSHTarget, timeout time.Duration, run workspaceOwnerCleanupRunner) error {
	if p.cleanup == "" {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	return run(contextWithoutWorkspaceOwner(cleanupCtx), target, p.cleanup)
}

func stageWorkspaceOwnerWindowsWitness(ctx context.Context, target SSHTarget, owner *workspaceOwner, remote string, inputSize *int64, selfCleaning bool) (workspaceOwnerRemotePreparation, error) {
	nonce, err := randomHex(16)
	if err != nil {
		return workspaceOwnerRemotePreparation{}, fmt.Errorf("create native Windows workspace witness name: %w", err)
	}
	name := owner.key + ".witness." + owner.token + "." + nonce + ".ps1"
	script := remoteWorkspaceOwnerWindowsWitness(owner.key, owner.token, remote, inputSize)
	if selfCleaning {
		script = remoteWorkspaceOwnerWindowsSelfCleaningWitness(name, script)
	}
	prepared := workspaceOwnerRemotePreparation{
		command: remoteWorkspaceOwnerWindowsRunWitnessCommand(name),
		cleanup: remoteWorkspaceOwnerWindowsCleanupWitnessCommand(name),
		name:    name,
	}
	var output synchronizedBuffer
	if err := runSSHInput(ctx, target, remoteWorkspaceOwnerWindowsStageWitnessCommand(owner.key, owner.token, name, int64(len([]byte(script)))), strings.NewReader(script), &output, &output); err != nil {
		detail := trimFailureDetail(strings.TrimSpace(output.String()))
		// The remote write may have succeeded even when its SSH result was lost.
		_ = prepared.closeWithin(ctx, target, workspaceOwnerCanceledCleanupTimeout, runSSHQuiet)
		if detail != "" {
			return workspaceOwnerRemotePreparation{}, fmt.Errorf("stage native Windows workspace witness: %w: %s", err, detail)
		}
		return workspaceOwnerRemotePreparation{}, fmt.Errorf("stage native Windows workspace witness: %w", err)
	}
	return prepared, nil
}

func runWorkspaceOwnerBackgroundOutput(ctx context.Context, target SSHTarget, owner *workspaceOwner, remote string) (string, error) {
	ctx = contextWithoutWorkspaceOwner(ctx)
	if owner == nil {
		return runSSHOutput(ctx, target, remote)
	}
	if !isWindowsNativeTarget(target) {
		return runSSHOutput(ctx, target, owner.wrapPOSIXBackgroundCommand(remote))
	}
	prepared, err := stageWorkspaceOwnerWindowsWitness(ctx, target, owner, remote, nil, true)
	if err != nil {
		return "", err
	}
	out, runErr := runSSHOutput(ctx, target, remoteWorkspaceOwnerWindowsStartBackgroundWitnessCommand(prepared.name))
	if runErr != nil {
		runErr = errors.Join(runErr, prepared.close(ctx, target))
	}
	return out, runErr
}

func workspaceOwnerKey(leaseID string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte("crabbox-workspace-owner-v1\x00"+leaseID)))
}

func shouldAcquireWorkspaceOwner(acquired, mayRetain bool, backend SSHLeaseBackend) bool {
	if !acquired || mayRetain {
		return true
	}
	exclusive, ok := backend.(ExclusiveOneShotAcquireBackend)
	return !ok || !exclusive.AcquireIsExclusiveOneShot()
}

func acquiredRunMayRetainLease(keep, keepOnFailure bool, stopAfter string) bool {
	if keep || keepOnFailure {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(stopAfter)) {
	case "", "always":
		return false
	default:
		return true
	}
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
	var lastCallErr error
	for {
		requestStarted := time.Now()
		response, callErr := transport.Do(waitCtx, workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: owner.key, Token: owner.token, TTL: ttl})
		retryDelay := workspaceOwnerPollInterval
		if callErr != nil {
			lastCallErr = callErr
			retryDelay = workspaceOwnerReconcileWait
		} else {
			lastCallErr = nil
			switch response {
			case "ACQUIRED", "RECOVERED":
				owner.ctx, owner.cancel = context.WithCancel(ctx)
				owner.confirmedAt(requestStarted)
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
				lastCallErr = errors.New("ambiguous protocol state")
				retryDelay = workspaceOwnerReconcileWait
			case "LEGACY":
				return nil, exit(7, "legacy workspace owner blocks lease %s: child=~/.crabbox/workspace-owners/%s.child key=%s; quiesce or reboot the remote host, verify the PID/start identity is no longer authoritative, then remove only that child file and retry", leaseID, owner.key, owner.key)
			default:
				return nil, exit(7, "acquire remote workspace owner: unexpected protocol response %q", response)
			}
		}
		timer := time.NewTimer(retryDelay)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				if lastCallErr != nil {
					return nil, exit(7, "acquire remote workspace owner: ambiguous remote state after reconciliation: %v", lastCallErr)
				}
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
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	o.renewLoopWithTicks(ticker.C, interval)
}

func (o *workspaceOwner) renewLoopWithTicks(ticks <-chan time.Time, callTimeout time.Duration) {
	defer close(o.done)
	for {
		select {
		case <-o.stop:
			return
		case <-o.ctx.Done():
			return
		case <-ticks:
			err := o.reconcileRenewal(callTimeout)
			if err == nil {
				continue
			}
			o.mu.Lock()
			o.renewErr = exit(7, "remote workspace owner renewal failed closed: %v", err)
			o.mu.Unlock()
			o.cancel()
			return
		}
	}
}

func (o *workspaceOwner) confirmedAt(requestStarted time.Time) {
	o.mu.Lock()
	o.confirmedUntil = requestStarted.Add(o.ttl)
	o.mu.Unlock()
}

func (o *workspaceOwner) reconcileRenewal(callTimeout time.Duration) error {
	for {
		o.mu.Lock()
		deadline := o.confirmedUntil
		o.mu.Unlock()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return errors.New("confirmed renewal deadline exhausted")
		}
		requestStarted := time.Now()
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(o.ctx), min(callTimeout, remaining))
		response, err := o.transport.Do(callCtx, workspaceOwnerRemoteRequest{Action: workspaceOwnerRenew, Key: o.key, Token: o.token, TTL: o.ttl})
		cancel()
		switch response {
		case "RENEWED":
			if err == nil {
				o.confirmedAt(requestStarted)
				return nil
			}
		case "MISMATCH", "EXPIRED":
			return fmt.Errorf("owner %s", strings.ToLower(response))
		default:
			if err == nil {
				return fmt.Errorf("unexpected protocol response %q", response)
			}
		}
		remaining = time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("confirmed renewal deadline exhausted: %w", err)
		}
		timer := time.NewTimer(min(workspaceOwnerReconcileWait, remaining))
		select {
		case <-o.stop:
			timer.Stop()
			return nil
		case <-o.ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
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
	o.stopRenewal()
	renewErr := o.Err()
	response, releaseErr := o.transport.Do(ctx, workspaceOwnerRemoteRequest{Action: workspaceOwnerRelease, Key: o.key, Token: o.token, TTL: o.ttl})
	if releaseErr != nil {
		releaseErr = exit(7, "release remote workspace owner: ambiguous remote state: %v", releaseErr)
	} else if response != "RELEASED" {
		releaseErr = exit(7, "release remote workspace owner failed closed: %s", strings.ToLower(firstNonBlank(response, "ambiguous")))
	}
	return errors.Join(renewErr, releaseErr)
}

func (o *workspaceOwner) QuiesceForLeaseRelease(ctx context.Context) error {
	if o == nil {
		return nil
	}
	if err := o.ConfirmNoChild(ctx); err != nil {
		return err
	}
	o.stopRenewal()
	return o.Err()
}

func (o *workspaceOwner) stopRenewal() {
	if o == nil {
		return
	}
	o.closeOnce.Do(func() {
		close(o.stop)
		<-o.done
	})
}

func (o *workspaceOwner) wrapPOSIXCommand(remote string, preserveInput bool) string {
	if o == nil {
		return remote
	}
	return remoteWorkspaceOwnerPOSIXWitness(o.key, o.token, remote, preserveInput)
}

func (o *workspaceOwner) wrapPOSIXBackgroundCommand(remote string) string {
	if o == nil {
		return remote
	}
	wrapped := o.wrapPOSIXCommand(remote, false)
	background := "nohup /bin/sh -c " + shellQuote(wrapped) + " >/dev/null 2>&1 < /dev/null & printf '%s\\n' \"$!\""
	return remoteWorkspaceOwnerPOSIXLauncher(o.key, o.token, background)
}

func remoteWorkspaceOwnerCommand(target SSHTarget, req workspaceOwnerRemoteRequest) string {
	if isWindowsNativeTarget(target) {
		return windowsPowerShellStdinScriptCommand(len([]byte(remoteWorkspaceOwnerWindows(req))))
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
  state_version=$(sed -n '1p' "$state" 2>/dev/null || true); state_token=$(sed -n '2p' "$state" 2>/dev/null || true); state_expiry=$(sed -n '3p' "$state" 2>/dev/null || true)
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
  child_lines=$(wc -l <"$child" 2>/dev/null | tr -d ' ')
  if [ "$child_lines" = 2 ]; then legacy_pid=$(sed -n '1p' "$child" 2>/dev/null || true); legacy_identity=$(sed -n '2p' "$child" 2>/dev/null || true); case "$legacy_pid" in ''|*[!0-9]*) return 2 ;; esac; [ "$legacy_pid" -gt 1 ] && [ -n "$legacy_identity" ] && [ "${#legacy_identity}" -le 96 ] || return 2; return 3; fi
  [ "$child_lines" = 4 ] || return 2
  [ "$(sed -n '1p' "$child" 2>/dev/null || true)" = v2 ] || return 2
  child_pgid=$(sed -n '2p' "$child" 2>/dev/null || true); sentinel_pid=$(sed -n '3p' "$child" 2>/dev/null || true); sentinel_identity=$(sed -n '4p' "$child" 2>/dev/null || true)
  case "$child_pgid:$sentinel_pid" in *[!0-9:]*|:*|*:) return 2 ;; esac
  [ "$child_pgid" -gt 1 ] && [ "$sentinel_pid" -gt 1 ] && [ -n "$sentinel_identity" ] && [ "${#sentinel_identity}" -le 96 ] || return 2
  live_pgid=$(ps -o pgid= -p "$sentinel_pid" 2>/dev/null | tr -d ' ' || true)
  if [ -n "$live_pgid" ]; then
    live_stat=$(ps -o stat= -p "$sentinel_pid" 2>/dev/null | tr -d ' ') || return 2; live_identity=$(ps -o lstart= -p "$sentinel_pid" 2>/dev/null | tr -s ' ' | sed 's/^ //;s/ $//' | cut -c1-96) || return 2
    [ -n "$live_stat" ] && [ -n "$live_identity" ] || return 2
    case "$live_stat" in *Z*) return 2 ;; esac; [ "$live_pgid" = "$child_pgid" ] && [ "$live_identity" = "$sentinel_identity" ] && return 0; return 2
  fi
  group_pgids=$(ps -eo pgid= 2>/dev/null) || return 2
  printf '%s\n' "$group_pgids" | awk -v pgid="$child_pgid" '$1 == pgid { found=1 } END { exit !found }' && return 2
  return 1
}
case "$action" in
  acquire)
    if read_state; then
      now=$(date +%s)
      if [ "$state_expiry" -gt "$now" ]; then
        if [ "$state_token" = "$token" ]; then write_state; printf ACQUIRED; else printf BUSY; fi
        exit 0
      fi
      if child_status; then printf CHILD; exit 0; else child_rc=$?; [ "$child_rc" -ne 3 ] || { printf LEGACY; exit 0; }; fi
      if [ "$child_rc" -eq 2 ]; then printf AMBIGUOUS; exit 74; fi
      rm -rf "$root/$key.run.$state_token".* "$root/$key.launcher.$state_token".*
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
    if child_status; then printf CHILD; exit 0; else child_rc=$?; [ "$child_rc" -ne 3 ] || { printf LEGACY; exit 0; }; fi
    [ "$child_rc" -ne 2 ] || { printf AMBIGUOUS; exit 74; }
    rm -f "$child"
    printf OWNED
    ;;
  release)
    read_state || { printf AMBIGUOUS; exit 74; }
    [ "$state_token" = "$token" ] || { printf MISMATCH; exit 75; }
    if child_status; then printf CHILD; exit 75; else child_rc=$?; [ "$child_rc" -ne 3 ] || { printf LEGACY; exit 0; }; fi
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
	return `$ErrorActionPreference = "Stop"
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
      if ($current.Expiry -gt [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()) {
        if ($current.Token -eq $token) { Write-State; Write-Output "ACQUIRED" } else { Write-Output "BUSY" }
        break
      }
      $childStatus = Get-ChildStatus
      if ($childStatus -eq "live") { Write-Output "CHILD"; break }
      if ($childStatus -eq "ambiguous") { throw "workspace owner child state is ambiguous" }
      Get-ChildItem -Path (Join-Path $root ($key + ".run." + $current.Token + ".*")) -ErrorAction SilentlyContinue | Remove-Item -Recurse -Force -ErrorAction SilentlyContinue
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
`
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
	childBody := `/bin/sh -c 'trap "" HUP TERM; exec 3<>"$1" || exit 74; : >"$2" || exit 74; while IFS= read -r command <&3; do case "$command" in expire) kill -TERM 0 2>/dev/null || true; sleep 0.2; kill -KILL 0 2>/dev/null || true ;; done) exit 0 ;; *) exit 74 ;; esac; done' crabbox-sentinel "$4" "$6" & sentinel_pid=$!; printf "%s\n" "$sentinel_pid" >"$5" || exit 74; while [ ! -f "$1" ]; do kill -0 "$sentinel_pid" 2>/dev/null || exit 74; sleep 0.05; done; sh -c "$2"; code=$?; touch "$3" || exit 74; wait "$sentinel_pid"; exit "$code"`
	installBody := `set -eu
[ "$(sed -n '2p' "$state" 2>/dev/null || true)" = "$token" ] || exit 75
owner_expiry=$(sed -n '3p' "$state" 2>/dev/null || true)
case "$owner_expiry" in ''|*[!0-9]*) exit 74 ;; esac
[ "$owner_expiry" -gt "$(date +%s)" ] || exit 75
[ ! -e "$child" ] && [ ! -e "$start" ] && [ -p "$control" ] || exit 74
[ "$(sed -n '1p' "$sentinel_pid_path" 2>/dev/null || true)" = "$sentinel_pid" ] && [ -f "$sentinel_ready" ] || exit 74
live_pgid=$(ps -o pgid= -p "$sentinel_pid" 2>/dev/null | tr -d ' '); live_stat=$(ps -o stat= -p "$sentinel_pid" 2>/dev/null | tr -d ' ')
live_identity=$(ps -o lstart= -p "$sentinel_pid" 2>/dev/null | tr -s ' ' | sed 's/^ //;s/ $//' | cut -c1-96)
[ "$live_pgid" = "$child_pgid" ] && [ "$live_identity" = "$sentinel_identity" ] || exit 74
case "$live_stat" in ''|*Z*) exit 74 ;; esac
child_tmp="$child.tmp.$$"
(umask 077; printf 'v2\n%s\n%s\n%s\n' "$child_pgid" "$sentinel_pid" "$sentinel_identity" >"$child_tmp")
mv "$child_tmp" "$child"`
	clearBody := `set -eu
[ "$(sed -n '2p' "$state" 2>/dev/null || true)" = "$token" ] || exit 75
[ "$(sed -n '1p' "$child" 2>/dev/null || true)" = v2 ] || exit 74
[ "$(sed -n '2p' "$child" 2>/dev/null || true)" = "$child_pgid" ] && [ "$(sed -n '3p' "$child" 2>/dev/null || true)" = "$sentinel_pid" ] && [ "$(sed -n '4p' "$child" 2>/dev/null || true)" = "$sentinel_identity" ] || exit 74
group_members=$(ps -eo pid=,pgid=,stat= 2>/dev/null) || exit 74
printf '%s\n' "$group_members" | awk -v pgid="$child_pgid" -v owner="$owner_pid" -v launcher="${launcher_pid:-0}" '$2 == pgid && $1 != owner && $1 != launcher && $3 !~ /Z/ { found=1 } END { exit !found }' && exit 74
rm -f "$child"`
	supervisorBody := `set -u
exec 3>"$control" || exit 0
if command -v flock >/dev/null 2>&1; then flock -x -w 5 "$gate" /bin/sh -c "$install_body"
elif command -v lockf >/dev/null 2>&1; then lockf -t 5 "$gate" /bin/sh -c "$install_body"
else exit 74
fi
gate_status=$?; if [ "$gate_status" -ne 0 ]; then printf 'expire\n' >&3; exit "$gate_status"; fi
: >"$run_dir/supervisor-ready" || { printf 'expire\n' >&3; exit 74; }
expire() { printf 'expire\n' >&3; exit 0; }
while :; do
	[ "$(sed -n '1p' "$child" 2>/dev/null || true)" = v2 ] && [ "$(sed -n '2p' "$child" 2>/dev/null || true)" = "$child_pgid" ] || exit 0
	[ "$(sed -n '3p' "$child" 2>/dev/null || true)" = "$sentinel_pid" ] && [ "$(sed -n '4p' "$child" 2>/dev/null || true)" = "$sentinel_identity" ] || exit 0
	kill -0 "$sentinel_pid" 2>/dev/null || expire
	live_pgid=$(ps -o pgid= -p "$sentinel_pid" 2>/dev/null | tr -d ' ') || expire
	live_stat=$(ps -o stat= -p "$sentinel_pid" 2>/dev/null | tr -d ' ') || expire
	live_identity=$(ps -o lstart= -p "$sentinel_pid" 2>/dev/null | tr -s ' ' | sed 's/^ //;s/ $//' | cut -c1-96) || expire
	[ "$live_pgid" = "$child_pgid" ] && [ "$live_identity" = "$sentinel_identity" ] || expire
	case "$live_stat" in ''|*Z*) expire ;; esac
	if [ -f "$run_dir/payload-done" ]; then
		members=$(ps -eo pid=,pgid=,stat= 2>/dev/null) || expire
		if ! printf '%s\n' "$members" | awk -v pgid="$child_pgid" -v leader="$child_pid" -v sentinel="$sentinel_pid" -v owner="$owner_pid" -v launcher="${launcher_pid:-0}" -v supervisor="$$" '$2 == pgid && $1 != leader && $1 != sentinel && $1 != owner && $1 != launcher && $1 != supervisor && $3 !~ /Z/ { found=1 } END { exit !found }'; then printf 'done\n' >&3; exit 0; fi
	fi
	state_token=$(sed -n '2p' "$state" 2>/dev/null || true)
	state_expiry=$(sed -n '3p' "$state" 2>/dev/null || true)
	case "$state_expiry" in ''|*[!0-9]*) state_expiry=0 ;; esac
	if [ "$state_token" != "$token" ] || [ "$(date +%s)" -ge "$state_expiry" ]; then
		printf 'expire\n' >&3
		exit 0
	fi
	sleep 0.2
done`
	return `set -u
root="$HOME/.crabbox/workspace-owners"
key=` + shellQuote(key) + `
token=` + shellQuote(token) + `
payload=` + shellQuote(remote) + `
state="$root/$key.owner"
child="$root/$key.child"
gate="$root/$key.gate"
run_dir="$root/$key.run.$token.$$"
	start="$run_dir/start"; control="$run_dir/control"; sentinel_pid_path="$run_dir/sentinel-pid"; sentinel_ready="$run_dir/sentinel-ready"; owner_pid=$$; launcher_pid=
run_owner_gate() {
	if command -v flock >/dev/null 2>&1; then
		flock -x -w 5 "$gate" /bin/sh -c "$1"
	elif command -v lockf >/dev/null 2>&1; then
		lockf -t 5 "$gate" /bin/sh -c "$1"
	else
		return 74
	fi
}
mkdir -m 700 "$run_dir" || exit 74
mkfifo "$control" || exit 74; child_body=` + shellQuote(childBody) + `
` + inputSetup + `if command -v setsid >/dev/null 2>&1; then
	(trap '' HUP; exec setsid /bin/sh -c "$child_body" "crabbox-workspace-$token" "$start" "$payload" "$run_dir/payload-done" "$control" "$sentinel_pid_path" "$sentinel_ready"` + inputRedirect + `) &
elif command -v perl >/dev/null 2>&1; then
	(trap '' HUP; exec perl -MPOSIX -e 'POSIX::setsid() >= 0 or exit 74; exec @ARGV' /bin/sh -c "$child_body" "crabbox-workspace-$token" "$start" "$payload" "$run_dir/payload-done" "$control" "$sentinel_pid_path" "$sentinel_ready"` + inputRedirect + `) &
else
	launcher_pid=$(ps -o ppid= -p "$owner_pid" 2>/dev/null | tr -d ' '); launcher_pgid=$(ps -o pgid= -p "$launcher_pid" 2>/dev/null | tr -d ' '); [ "$launcher_pid" -gt 1 ] && [ "$launcher_pgid" = "$launcher_pid" ] || exit 74; (trap '' HUP; exec /bin/sh -c "$child_body" "crabbox-workspace-$token" "$start" "$payload" "$run_dir/payload-done" "$control" "$sentinel_pid_path" "$sentinel_ready"` + inputRedirect + `) &
fi
child_pid=$!; expected_pgid=${launcher_pid:-$child_pid}
child_pgid=$(ps -o pgid= -p "$child_pid" 2>/dev/null | tr -d ' ')
attempt=0
while [ "$child_pgid" != "$expected_pgid" ] && kill -0 "$child_pid" 2>/dev/null && [ "$attempt" -lt 20 ]; do sleep 0.01; attempt=$((attempt + 1)); child_pgid=$(ps -o pgid= -p "$child_pid" 2>/dev/null | tr -d ' '); done
attempt=0
while { [ ! -s "$sentinel_pid_path" ] || [ ! -f "$sentinel_ready" ]; } && kill -0 "$child_pid" 2>/dev/null && [ "$attempt" -lt 100 ]; do sleep 0.01; attempt=$((attempt + 1)); done
sentinel_pid=$(sed -n '1p' "$sentinel_pid_path" 2>/dev/null || true)
case "$sentinel_pid" in ''|*[!0-9]*) sentinel_pid=0 ;; esac
sentinel_identity=$(ps -o lstart= -p "$sentinel_pid" 2>/dev/null | tr -s ' ' | sed 's/^ //;s/ $//' | cut -c1-96)
if [ "$child_pgid" != "$expected_pgid" ] || [ "$sentinel_pid" -le 1 ] || [ -z "$sentinel_identity" ]; then kill "$child_pid" 2>/dev/null || true; if [ "$sentinel_pid" -gt 1 ]; then kill "$sentinel_pid" 2>/dev/null || true; kill -KILL "$sentinel_pid" 2>/dev/null || true; fi; rm -rf "$run_dir"; exit 74; fi
install_body=` + shellQuote(installBody) + `
export state child gate token child_pid child_pgid sentinel_pid sentinel_identity owner_pid launcher_pid run_dir start control sentinel_pid_path sentinel_ready install_body
# Reuse only a private transport PGID; the validated sentinel remains its sole group signaler.
if command -v setsid >/dev/null 2>&1; then supervisor_setsid=setsid; else supervisor_setsid=; fi; nohup $supervisor_setsid /bin/sh -c ` + shellQuote(supervisorBody) + ` >/dev/null 2>&1 </dev/null &
supervisor_pid=$!
attempt=0; while [ ! -f "$run_dir/supervisor-ready" ] && kill -0 "$supervisor_pid" 2>/dev/null && [ "$attempt" -lt 600 ]; do sleep 0.01; attempt=$((attempt + 1)); done
if [ ! -f "$run_dir/supervisor-ready" ]; then
	kill "$supervisor_pid" "$child_pid" 2>/dev/null || true
	live_pgid=$(ps -o pgid= -p "$sentinel_pid" 2>/dev/null | tr -d ' '); live_identity=$(ps -o lstart= -p "$sentinel_pid" 2>/dev/null | tr -s ' ' | sed 's/^ //;s/ $//' | cut -c1-96); if [ "$live_pgid" = "$child_pgid" ] && [ "$live_identity" = "$sentinel_identity" ]; then kill -KILL "$sentinel_pid" 2>/dev/null || true; fi
	wait "$child_pid" 2>/dev/null || true
	set +e; wait "$supervisor_pid"; gate_status=$?; set -e
	set +e; run_owner_gate ` + shellQuote(clearBody) + `; set -e
	rm -rf "$run_dir"
	[ "$gate_status" -ne 0 ] || gate_status=74
	exit "$gate_status"
fi
touch "$start"
set +e
wait "$child_pid"
code=$?
wait "$supervisor_pid"
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

func remoteWorkspaceOwnerWindowsStageWitnessCommand(key, token, name string, scriptSize int64) string {
	return powershellCommand(`$ErrorActionPreference = "Stop"
$root = Join-Path $HOME ".crabbox\workspace-owners"
$state = Join-Path $root (` + psQuote(key) + ` + ".owner")
$path = Join-Path $root ` + psQuote(name) + `
$stream = $null
$created = $false
try {
	$rootItem = Get-Item -LiteralPath $root -ErrorAction Stop
	if (-not $rootItem.PSIsContainer -or ($rootItem.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw "ambiguous workspace owner root" }
	$lines = @(Get-Content -LiteralPath $state -ErrorAction Stop)
	if ($lines.Count -ne 3 -or $lines[0] -ne "v1" -or $lines[1] -ne ` + psQuote(token) + ` -or $lines[2] -notmatch "^[0-9]+$") { throw "workspace owner state changed before witness staging" }
	if ([Int64]$lines[2] -le [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()) { throw "workspace owner expired before witness staging" }
	$stream = [IO.File]::Open($path, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
	$created = $true
	$bom = [byte[]](0xEF, 0xBB, 0xBF)
	$stream.Write($bom, 0, $bom.Length)
` + windowsPowerShellCopyExactInput("$stream", scriptSize) + `	if ($stream.Length -ne ` + strconv.FormatInt(scriptSize+3, 10) + `) { throw "staged workspace witness length is ambiguous" }
	$stream.Flush($true)
} catch {
	if ($null -ne $stream) { $stream.Dispose(); $stream = $null }
	if ($created) { Remove-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue }
	throw
} finally {
	if ($null -ne $stream) { $stream.Dispose() }
}
`)
}

func remoteWorkspaceOwnerWindowsRunWitnessCommand(name string) string {
	return powershellCommand(`$ErrorActionPreference = "Stop"
$path = Join-Path (Join-Path $HOME ".crabbox\workspace-owners") ` + psQuote(name) + `
try {
	if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "staged workspace witness is missing" }
	& powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $path
	$code = $LASTEXITCODE
} finally {
	Remove-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
}
exit $code
`)
}

func remoteWorkspaceOwnerWindowsCleanupWitnessCommand(name string) string {
	return powershellCommand(`$ErrorActionPreference = "Stop"
$path = Join-Path (Join-Path $HOME ".crabbox\workspace-owners") ` + psQuote(name) + `
Remove-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
`)
}

func remoteWorkspaceOwnerWindowsSelfCleaningWitness(name, script string) string {
	return `$selfPath = Join-Path (Join-Path $HOME ".crabbox\workspace-owners") ` + psQuote(name) + `
try {
` + script + `
} finally {
	Remove-Item -LiteralPath $selfPath -Force -ErrorAction SilentlyContinue
}
`
}

func remoteWorkspaceOwnerWindowsStartBackgroundWitnessCommand(name string) string {
	return powershellCommand(`$ErrorActionPreference = "Stop"
$path = Join-Path (Join-Path $HOME ".crabbox\workspace-owners") ` + psQuote(name) + `
try {
	if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "staged workspace witness is missing" }
	$fileArg = '"' + $path + '"'
	$process = Start-Process -FilePath "powershell.exe" -ArgumentList @("-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", $fileArg) -WindowStyle Hidden -PassThru
	Write-Output $process.Id
} catch {
	Remove-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
	throw
}
`)
}

func remoteWorkspaceOwnerWindowsWitness(key, token, remote string, inputSize *int64) string {
	payload := base64.StdEncoding.EncodeToString([]byte(remote))
	inputSetup := ""
	inputArgument := ""
	if inputSize != nil {
		inputSetup = `$inputPath = Join-Path $runDir "input"
$inputFile = [IO.File]::Open($inputPath, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
try {
` + windowsPowerShellCopyExactInput("$inputFile", *inputSize) + `	$inputFile.Flush($true)
} finally { $inputFile.Dispose() }
`
		inputArgument = ` -RedirectStandardInput $inputPath`
	}
	return `$ErrorActionPreference = "Stop"
$root = Join-Path $HOME ".crabbox\workspace-owners"
$key = ` + psQuote(key) + `
$token = ` + psQuote(token) + `
$state = Join-Path $root ($key + ".owner")
$child = Join-Path $root ($key + ".child")
$gate = Join-Path $root ($key + ".gate")
$runDir = Join-Path $root ($key + ".run." + $token + "." + [Guid]::NewGuid().ToString("N"))
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
New-Item -ItemType Directory -Path $runDir -ErrorAction Stop | Out-Null
` + inputSetup + `
$childSource = @'
$ErrorActionPreference = "Stop"
while (-not (Test-Path -LiteralPath '__START__')) { Start-Sleep -Milliseconds 50 }
$payload = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('__PAYLOAD__'))
$global:LASTEXITCODE = $null
& ([ScriptBlock]::Create($payload))
$payloadSucceeded = $?
$payloadExitCode = $global:LASTEXITCODE
if ($null -ne $payloadExitCode) { exit [int]$payloadExitCode }
if (-not $payloadSucceeded) { exit 1 }
exit 0
'@
$childSource = $childSource.Replace('__START__', $start.Replace("'", "''")).Replace('__PAYLOAD__', ` + psQuote(payload) + `)
$childScript = Join-Path $runDir "child.ps1"
[IO.File]::WriteAllText($childScript, $childSource, [Text.UTF8Encoding]::new($true))
$childFileArg = '"' + $childScript + '"'
$process = Start-Process -FilePath "powershell.exe" -ArgumentList @("-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", $childFileArg) -NoNewWindow -PassThru` + inputArgument + `
$null = $process.Handle
$identity = [string]$process.StartTime.ToUniversalTime().Ticks
$supervisorSource = @'
$ErrorActionPreference = "Stop"
$state = '__STATE__'
$ready = '__READY__'; $token = '__TOKEN__'
$pidValue = [int]'__PID__'
$identity = '__IDENTITY__'
$jobType = 'using System; using System.Runtime.InteropServices; [StructLayout(LayoutKind.Sequential)] public struct CrabboxJobBasic { public long PerProcessUserTimeLimit, PerJobUserTimeLimit; public uint LimitFlags; public UIntPtr MinimumWorkingSetSize, MaximumWorkingSetSize; public uint ActiveProcessLimit; public UIntPtr Affinity; public uint PriorityClass, SchedulingClass; } [StructLayout(LayoutKind.Sequential)] public struct CrabboxJobIO { public ulong ReadOperationCount, WriteOperationCount, OtherOperationCount, ReadTransferCount, WriteTransferCount, OtherTransferCount; } [StructLayout(LayoutKind.Sequential)] public struct CrabboxJobLimits { public CrabboxJobBasic BasicLimitInformation; public CrabboxJobIO IoInfo; public UIntPtr ProcessMemoryLimit, JobMemoryLimit, PeakProcessMemoryUsed, PeakJobMemoryUsed; } [StructLayout(LayoutKind.Sequential)] public struct CrabboxJobTime { public uint Low, High; } [StructLayout(LayoutKind.Sequential)] public struct CrabboxJobAccounting { public long TotalUserTime, TotalKernelTime, PeriodUserTime, PeriodKernelTime; public uint TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses; } public static class CrabboxJob { [DllImport("kernel32.dll", SetLastError=true)] static extern IntPtr CreateJobObject(IntPtr a, string n); [DllImport("kernel32.dll", SetLastError=true)] static extern bool SetInformationJobObject(IntPtr j, int c, ref CrabboxJobLimits i, uint l); [DllImport("kernel32.dll", SetLastError=true)] static extern bool QueryInformationJobObject(IntPtr j, int c, out CrabboxJobAccounting i, uint l, IntPtr r); [DllImport("kernel32.dll", SetLastError=true)] static extern IntPtr OpenProcess(uint a, bool i, int p); [DllImport("kernel32.dll", SetLastError=true)] static extern bool GetProcessTimes(IntPtr p, out CrabboxJobTime c, out CrabboxJobTime e, out CrabboxJobTime k, out CrabboxJobTime u); [DllImport("kernel32.dll", SetLastError=true)] static extern bool AssignProcessToJobObject(IntPtr j, IntPtr p); [DllImport("kernel32.dll")] public static extern bool CloseHandle(IntPtr h); public static IntPtr Attach(int pid,long identity) { IntPtr j=CreateJobObject(IntPtr.Zero,null); if(j==IntPtr.Zero) throw new System.ComponentModel.Win32Exception(); CrabboxJobLimits l=new CrabboxJobLimits(); l.BasicLimitInformation.LimitFlags=0x2000; if(!SetInformationJobObject(j,9,ref l,(uint)Marshal.SizeOf(l))) { CloseHandle(j); throw new System.ComponentModel.Win32Exception(); } IntPtr p=OpenProcess(0x1101,false,pid); if(p==IntPtr.Zero) { CloseHandle(j); throw new System.ComponentModel.Win32Exception(); } try { CrabboxJobTime c,e,k,u; if(!GetProcessTimes(p,out c,out e,out k,out u)) throw new System.ComponentModel.Win32Exception(); long created=((long)c.High<<32)|c.Low; if(created+504911232000000000L!=identity) throw new InvalidOperationException("process identity changed"); if(!AssignProcessToJobObject(j,p)) throw new System.ComponentModel.Win32Exception(); } catch { CloseHandle(j); throw; } finally { CloseHandle(p); } return j; } public static uint Active(IntPtr j) { CrabboxJobAccounting i; if(!QueryInformationJobObject(j,1,out i,(uint)Marshal.SizeOf(typeof(CrabboxJobAccounting)),IntPtr.Zero)) throw new System.ComponentModel.Win32Exception(); return i.ActiveProcesses; } }'
Add-Type -TypeDefinition $jobType
$job = [CrabboxJob]::Attach($pidValue, [Int64]$identity); if ($job -eq [IntPtr]::Zero) { throw "job attachment failed" }
New-Item -ItemType File -Path $ready -Force | Out-Null; $ErrorActionPreference = "SilentlyContinue"
try { while ($true) {
	$leaderLive = $false
	try {
		$process = [Diagnostics.Process]::GetProcessById($pidValue)
		$leaderLive = [string]$process.StartTime.ToUniversalTime().Ticks -eq $identity
	} catch {}
	$current = @(Get-Content -LiteralPath $state -ErrorAction SilentlyContinue)
	$expired = $current.Count -ne 3 -or $current[0] -ne "v1" -or $current[1] -ne $token -or $current[2] -notmatch "^[0-9]+$"
	if (-not $expired) { $expired = [Int64]$current[2] -le [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() }
	if ($expired) {
		if ($job -ne [IntPtr]::Zero) { $null = [CrabboxJob]::CloseHandle($job); $job = [IntPtr]::Zero }
		exit 0
	}
	if (-not $leaderLive -and [CrabboxJob]::Active($job) -eq 0) { exit 0 }
	Start-Sleep -Milliseconds 200
} } finally { if ($job -ne [IntPtr]::Zero) { $null = [CrabboxJob]::CloseHandle($job) } }
'@
$ready = Join-Path $runDir "supervisor.ready"
$supervisorSource = $supervisorSource.Replace('__STATE__', $state.Replace("'", "''")).Replace('__CHILD__', $child.Replace("'", "''")).Replace('__RUN_DIR__', $runDir.Replace("'", "''")).Replace('__READY__', $ready.Replace("'", "''")).Replace('__TOKEN__', $token).Replace('__PID__', [string]$process.Id).Replace('__IDENTITY__', $identity)
$supervisorScript = Join-Path $runDir "supervisor.ps1"
[IO.File]::WriteAllText($supervisorScript, $supervisorSource, [Text.UTF8Encoding]::new($true))
# The detached supervisor owns the exact process handle before publication.
$supervisorFileArg = '"' + $supervisorScript + '"'; $supervisor = $null
function Stop-NewWitness([int]$code) { if ($null -ne $supervisor -and -not $supervisor.HasExited) { $supervisor.Kill() }; if ($null -ne $supervisor) { $supervisor.WaitForExit() }; if (-not $process.HasExited) { $process.Kill() }; $process.WaitForExit(); Remove-Item -LiteralPath $runDir -Recurse -Force -ErrorAction SilentlyContinue; exit $code }
try { $supervisor = Start-Process -FilePath "powershell.exe" -ArgumentList @("-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", $supervisorFileArg) -WindowStyle Hidden -PassThru; $null = $supervisor.Handle; $supervisorIdentity = [string]$supervisor.StartTime.ToUniversalTime().Ticks } catch { Stop-NewWitness 74 }
$supervisorDeadline = [DateTime]::UtcNow.AddSeconds(10)
while (-not (Test-Path -LiteralPath $ready) -and -not $supervisor.HasExited -and [DateTime]::UtcNow -lt $supervisorDeadline) { Start-Sleep -Milliseconds 20 }
if (-not (Test-Path -LiteralPath $ready)) { Stop-NewWitness 74 }
$gateStream = Enter-Gate
if ($null -eq $gateStream) { Stop-NewWitness 74 }
try {
	if ((Read-Token) -ne $token) { Stop-NewWitness 75 }
	if ((Read-Expiry) -le [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()) { Stop-NewWitness 75 }
	$existingStatus = Get-ExistingChildStatus
	if ($existingStatus -eq "live") { Stop-NewWitness 75 }
	if ($existingStatus -eq "ambiguous") { Stop-NewWitness 74 }
	Remove-Item -LiteralPath $child -Force -ErrorAction SilentlyContinue
	$tmp = $child + ".tmp." + [Guid]::NewGuid().ToString("N")
	[IO.File]::WriteAllLines($tmp, @([string]$supervisor.Id, $supervisorIdentity), [Text.UTF8Encoding]::new($false))
	Move-Item -LiteralPath $tmp -Destination $child -Force
} catch { Stop-NewWitness 74 } finally { $gateStream.Dispose(); Remove-Item -LiteralPath $gate -Force -ErrorAction SilentlyContinue }
New-Item -ItemType File -Path $start -Force | Out-Null
$process.WaitForExit()
$code = $process.ExitCode
$supervisor.WaitForExit()
$clear = $false
$gateStream = Enter-Gate
if ($null -ne $gateStream) {
	try {
		$recorded = @(Get-Content -LiteralPath $child -ErrorAction Stop)
    if ((Read-Token) -eq $token -and $recorded.Count -eq 2 -and $recorded[0] -eq [string]$supervisor.Id -and $recorded[1] -eq $supervisorIdentity) {
      Remove-Item -LiteralPath $child -Force -ErrorAction Stop
      $clear = $true
    }
	} finally { $gateStream.Dispose(); Remove-Item -LiteralPath $gate -Force -ErrorAction SilentlyContinue }
}
Remove-Item -LiteralPath $runDir -Recurse -Force -ErrorAction SilentlyContinue
if (-not $clear) { exit 74 }
exit $code
`
}
