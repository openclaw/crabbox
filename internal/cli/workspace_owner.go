package cli

import (
	"bytes"
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
	workspaceOwnerRemoteTimeout                                                                                  = sshControlExecutionLimit
	workspaceOwnerRenewInterval                                                                                  = 10 * time.Second
	workspaceOwnerRenewMargin                                                                                    = time.Second
	workspaceOwnerProgressEvery                                                                                  = 10 * time.Second
	workspaceOwnerPollInterval, workspaceOwnerSSHConnectTimeoutOption, workspaceOwnerSSHConnectionAttemptsOption = time.Second, "10", "3"
)

var (
	workspaceOwnerCallTimeout         = sshTransportCallBudget(SSHTarget{}, sshControlMetadataLimit, sshCommandLimit{execution: workspaceOwnerRemoteTimeout, control: true})
	workspaceOwnerTTL                 = workspaceOwnerRenewInterval + workspaceOwnerCallTimeout + workspaceOwnerRenewMargin
	workspaceOwnerCloseQuiesceTimeout = 2*workspaceOwnerCallTimeout + workspaceOwnerRenewMargin
	workspaceOwnerWaitTimeout         = workspaceOwnerTTL + workspaceOwnerPollInterval + workspaceOwnerCallTimeout + workspaceOwnerRenewMargin
)

func workspaceOwnerTransportCallBudget(transport workspaceOwnerTransport) time.Duration {
	if budgeted, ok := transport.(interface{ CallBudget() time.Duration }); ok {
		return budgeted.CallBudget()
	}
	return workspaceOwnerCallTimeout
}

func (o *workspaceOwner) callTimeout() time.Duration {
	if o == nil {
		return workspaceOwnerCallTimeout
	}
	return workspaceOwnerTransportCallBudget(o.transport)
}

func (o *workspaceOwner) quiesceTimeout() time.Duration {
	return 2*o.callTimeout() + workspaceOwnerRenewMargin
}

type workspaceOwnerAction string

type workspaceOwnerInspectResult uint8

const (
	workspaceOwnerAcquire workspaceOwnerAction = "acquire"
	workspaceOwnerRenew   workspaceOwnerAction = "renew"
	workspaceOwnerInspect workspaceOwnerAction = "inspect"
	workspaceOwnerRelease workspaceOwnerAction = "release"
)

const workspaceOwnerQuiescent, workspaceOwnerChildActive workspaceOwnerInspectResult = 0, 1

type workspaceOwnerRemoteRequest struct {
	Action workspaceOwnerAction
	Key    string
	Token  string
	TTL    time.Duration
}

type workspaceOwnerTransport interface {
	Do(context.Context, workspaceOwnerRemoteRequest) (string, error)
}

func callWorkspaceOwnerTransport(ctx context.Context, timeout time.Duration, transport workspaceOwnerTransport, req workspaceOwnerRemoteRequest) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, min(timeout, workspaceOwnerTransportCallBudget(transport)))
	defer cancel()
	return transport.Do(callCtx, req)
}

type sshWorkspaceOwnerTransport struct {
	target SSHTarget
}

func (t sshWorkspaceOwnerTransport) CallBudget() time.Duration {
	return sshTransportCallBudget(t.target, sshControlMetadataLimit, sshCommandLimit{execution: workspaceOwnerRemoteTimeout, control: true})
}

func (t sshWorkspaceOwnerTransport) Do(ctx context.Context, req workspaceOwnerRemoteRequest) (string, error) {
	ctx = contextWithoutWorkspaceOwner(ctx)
	ctx, cancel := context.WithTimeout(ctx, workspaceOwnerTransportCallBudget(t))
	defer cancel()
	remote := remoteWorkspaceOwnerCommand(t.target, req)
	var input []byte
	if isWindowsWSL2Target(t.target) {
		t.target.NoControlMaster = true
	} else if isWindowsNativeTarget(t.target) {
		script := remoteWorkspaceOwnerWindows(req)
		input = []byte(script)
		remote = windowsPowerShellStdinScriptCommand(len([]byte(script)))
	}
	return runWorkspaceOwnerSSHProtocol(ctx, t.target, remote, input, req.Token)
}

func runWorkspaceOwnerSSHProtocol(ctx context.Context, target SSHTarget, remote string, input []byte, requestToken string) (output string, err error) {
	if len(remote)+len(input) > sshControlMetadataLimit {
		return "", errors.New("workspace owner metadata exceeds its accounted transport budget")
	}
	var source io.ReadSeeker
	if input != nil {
		source = bytes.NewReader(input)
	}
	var stdout, stderr synchronizedBuffer
	err = executePreparedSSH(ctx, &target, remote, source, int64(len(input)), sshCommandLimit{execution: workspaceOwnerRemoteTimeout, control: true},
		workspaceOwnerSSHConnectTimeoutOption, workspaceOwnerSSHConnectionAttemptsOption, &stdout, &stderr)
	output = strings.TrimSpace(stdout.String())
	if err != nil {
		detail := trimFailureDetail(RedactDiagnosticSecrets(redactSSHTransportDiagnostic(target, stderr.String()), requestToken))
		if detail != "" {
			err = fmt.Errorf("%w: %s", err, detail)
		}
	}
	return output, err
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

type workspaceOwnerRemotePreparation struct {
	command     string
	cleanup     string
	name        string
	setupMarker string
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
		nonce, err := randomHex(16)
		if err != nil {
			return workspaceOwnerRemotePreparation{}, fmt.Errorf("create workspace witness diagnostic marker: %w", err)
		}
		marker := workspaceOwnerSetupPrefix + nonce
		return workspaceOwnerRemotePreparation{
			command:     owner.wrapPOSIXCommand(remote, inputSize != nil, marker),
			setupMarker: marker,
		}, nil
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
	transport := sshWorkspaceOwnerTransport{target: target}
	call := workspaceOwnerTransportCallBudget(transport)
	ttl := workspaceOwnerRenewInterval + call + workspaceOwnerRenewMargin
	wait := ttl + workspaceOwnerPollInterval + call + workspaceOwnerRenewMargin
	return acquireWorkspaceOwnerWithTransport(ctx, target, leaseID, stderr, transport, wait, ttl, workspaceOwnerRenewInterval)
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
		response, callErr := callWorkspaceOwnerTransport(waitCtx, owner.callTimeout(), transport, workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: owner.key, Token: owner.token, TTL: ttl})
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
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	o.renewLoopWithTicks(ticker.C, o.callTimeout())
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
			response, err := callWorkspaceOwnerTransport(context.WithoutCancel(o.ctx), callTimeout, o.transport, workspaceOwnerRemoteRequest{Action: workspaceOwnerRenew, Key: o.key, Token: o.token, TTL: o.ttl})
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
	result, err := o.inspectChild(ctx)
	if err == nil && result == workspaceOwnerChildActive {
		err = exit(7, "confirm remote workspace owner child state failed closed: child")
	}
	return err
}

func (o *workspaceOwner) inspectChild(ctx context.Context) (workspaceOwnerInspectResult, error) {
	if o == nil {
		return workspaceOwnerQuiescent, nil
	}
	if err := o.Err(); err != nil {
		return workspaceOwnerQuiescent, err
	}
	response, err := callWorkspaceOwnerTransport(ctx, o.callTimeout(), o.transport, workspaceOwnerRemoteRequest{Action: workspaceOwnerInspect, Key: o.key, Token: o.token, TTL: o.ttl})
	if err != nil {
		return workspaceOwnerQuiescent, exit(7, "confirm remote workspace owner child state: ambiguous remote state: %v", err)
	}
	switch response {
	case "OWNED":
		return workspaceOwnerQuiescent, nil
	case "CHILD":
		return workspaceOwnerChildActive, nil
	default:
		return workspaceOwnerQuiescent, exit(7, "confirm remote workspace owner child state failed closed: %s", strings.ToLower(firstNonBlank(response, "ambiguous")))
	}
}

func (o *workspaceOwner) WaitForChild(ctx context.Context, timeout time.Duration) error {
	if o == nil {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		response, err := callWorkspaceOwnerTransport(ctx, min(o.callTimeout(), time.Until(deadline)), o.transport, workspaceOwnerRemoteRequest{Action: workspaceOwnerInspect, Key: o.key, Token: o.token, TTL: o.ttl})
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
	response, releaseErr := callWorkspaceOwnerTransport(ctx, o.callTimeout(), o.transport, workspaceOwnerRemoteRequest{Action: workspaceOwnerRelease, Key: o.key, Token: o.token, TTL: o.ttl})
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

func (o *workspaceOwner) wrapPOSIXCommand(remote string, preserveInput bool, setupMarker ...string) string {
	if o == nil {
		return remote
	}
	marker := ""
	if len(setupMarker) > 0 {
		marker = setupMarker[0]
	}
	return remoteWorkspaceOwnerPOSIXLauncher(o.key, o.token, remoteWorkspaceOwnerPOSIXWitnessScript(o.key, o.token, remote, marker, preserveInput), marker)
}

func (o *workspaceOwner) wrapPOSIXBackgroundCommand(remote string) string {
	if o == nil {
		return remote
	}
	wrapped := o.wrapPOSIXCommand(remote, false)
	executable := "/bin/sh"
	if isWindowsWSL2Target(o.target) {
		// Only intentional owner-witnessed helpers escape the transient stage
		// group. The witness and its existing expiry policy remain unchanged.
		executable = "setsid /bin/sh"
	}
	background := "nohup " + executable + " -c " + shellQuote(wrapped) + " >/dev/null 2>&1 < /dev/null & child=$!\n"
	if isWindowsWSL2Target(o.target) {
		background += `# Observe detachment before returning to the stage supervisor.
i=0
while [ "$i" -lt 50 ]; do
  i=$((i + 1))
  [ "$(ps -o pgid= -p "$child" | tr -d ' ')" != "$child" ] || break
  kill -0 "$child" 2>/dev/null || exit 74
  sleep .1
done
[ "$(ps -o pgid= -p "$child" | tr -d ' ')" = "$child" ] || exit 74
`
	}
	background += "printf '%s\\n' \"$child\""

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

// A denied signal probe is not proof of death. Never erase a witness until
// independent PID-only observation confirms absence (no process arguments).
const workspaceOwnerPOSIXAbsent = `owner_child_absent() {
  observed_pids=$(ps -e -o pid= 2>/dev/null) || return 1
  matching_pid=$(printf '%s\n' "$observed_pids" | awk -v pid="$1" '$1 == pid { print $1 }') || return 1
  [ -z "$matching_pid" ]
}
`

func remoteWorkspaceOwnerPOSIX(req workspaceOwnerRemoteRequest) string {
	body := `set -eu
` + workspaceOwnerPOSIXAbsent + `
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
  else
    owner_child_absent "$child_pid" || return 2
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
umask 077
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
`
}

func remoteWorkspaceOwnerPOSIXLauncher(key, token, script string, setupMarker ...string) string {
	return remoteWorkspaceOwnerPOSIXEncodedLauncher(key, token, base64.StdEncoding.EncodeToString([]byte(script)), len(script), setupMarker...)
}

func remoteWorkspaceOwnerPOSIXEncodedLauncher(key, token, encoded string, decodedSize int, setupMarker ...string) string {
	// Private staging must not impose its creation policy on the launched script.
	launcher := `set -u; command_umask=$(umask); umask 077; root="$HOME/.crabbox/workspace-owners"; run_dir="$root/` + key + `.launcher.` + token + `.$$"; script="$run_dir/script"; cleanup_launcher() { rm -f "$script" 2>/dev/null; rmdir "$run_dir" 2>/dev/null || true; }; decoded_size_ok() { set -- $(wc -c <"$script"); [ "$#" -eq 1 ] && [ "$1" = ` + strconv.Itoa(decodedSize) + ` ]; }; mkdir -p "$root" 2>/dev/null || exit 74; chmod 700 "$HOME/.crabbox" "$root" 2>/dev/null || true; mkdir -m 700 "$run_dir" 2>/dev/null || exit 74; payload_b64="` + encoded + `"; decoded=; if command -v base64 >/dev/null 2>&1; then if printf %s "$payload_b64" | base64 --decode 2>/dev/null >"$script" && decoded_size_ok; then decoded=1; elif printf %s "$payload_b64" | base64 -d 2>/dev/null >"$script" && decoded_size_ok; then decoded=1; elif printf %s "$payload_b64" | base64 -D 2>/dev/null >"$script" && decoded_size_ok; then decoded=1; fi; fi; if [ -z "$decoded" ] && command -v openssl >/dev/null 2>&1; then if printf %s "$payload_b64" | openssl base64 -d -A 2>/dev/null >"$script" && decoded_size_ok; then decoded=1; fi; fi; if [ -z "$decoded" ]; then cleanup_launcher; exit 74; fi; umask "$command_umask"; /bin/sh "$script"; code=$?; cleanup_launcher; exit "$code"`
	if len(setupMarker) > 0 && setupMarker[0] != "" {
		launcher = remoteWorkspaceOwnerPOSIXSetupDiagnostic(setupMarker[0]) + strings.ReplaceAll(launcher, "exit 74", "setup_failed staging")
	}
	return "exec /bin/sh -c " + shellQuote(launcher)
}

func remoteWorkspaceOwnerPOSIXWitness(key, token, remote string, preserveInput ...bool) string {
	return remoteWorkspaceOwnerPOSIXLauncher(key, token, remoteWorkspaceOwnerPOSIXWitnessScript(key, token, remote, "", preserveInput...))
}

func remoteWorkspaceOwnerPOSIXSetupDiagnostic(marker string) string {
	if marker == "" {
		marker = "remote workspace owner setup:"
	}
	// The marker is fixed ASCII plus locally generated hex, never remote input.
	// Double quotes keep the outer launcher portable across login shells.
	return `setup_failed() { printf "%s\n" "` + marker + ` failed $1" >&2; exit "${2:-74}"; }; `
}

func remoteWorkspaceOwnerPOSIXWitnessScript(key, token, remote, setupMarker string, preserveInput ...bool) string {
	diagnostic := remoteWorkspaceOwnerPOSIXSetupDiagnostic(setupMarker)
	started := ""
	if setupMarker != "" {
		started = "printf '%s\\n' " + shellQuote(setupMarker+" started") + " >&2 || setup_failed handoff\n"
	}
	inputSetup := ""
	inputRedirect := ""
	if len(preserveInput) > 0 && preserveInput[0] {
		inputSetup = `(umask 077; cat >"$run_dir/input") 2>/dev/null || { rm -rf "$run_dir" 2>/dev/null; setup_failed input; }
`
		inputRedirect = ` <"$run_dir/input"`
	}
	gateFunction := `run_owner_gate() {
	if command -v flock >/dev/null 2>&1; then
		flock -x -w 5 "$gate" /bin/sh -c "$1" 2>/dev/null
	elif command -v lockf >/dev/null 2>&1; then
		lockf -t 5 "$gate" /bin/sh -c "$1" 2>/dev/null
	else
		return 74
	fi
}
`
	installBody := `set -eu
` + workspaceOwnerPOSIXAbsent + `
[ "$(sed -n '2p' "$state" 2>/dev/null || true)" = "$token" ] || exit 75
owner_expiry=$(sed -n '3p' "$state" 2>/dev/null || true)
case "$owner_expiry" in ''|*[!0-9]*) exit 74 ;; esac
[ "$owner_expiry" -gt "$(date +%s)" ] || exit 75
kill -0 "$child_pid" 2>/dev/null || exit 74
live_child_identity=$(ps -o lstart= -p "$child_pid" 2>/dev/null | tr -s ' ' | sed 's/^ //;s/ $//' | cut -c1-96)
[ -n "$live_child_identity" ] && [ "$live_child_identity" = "$child_identity" ] || exit 74
if [ -f "$child" ]; then
	existing_pid=$(sed -n '1p' "$child" 2>/dev/null || true)
	existing_identity=$(sed -n '2p' "$child" 2>/dev/null || true)
	case "$existing_pid" in ''|*[!0-9]*) exit 74 ;; esac
	[ -n "$existing_identity" ] && [ "${#existing_identity}" -le 96 ] || exit 74
	if kill -0 "$existing_pid" 2>/dev/null; then
		live_existing_identity=$(ps -o lstart= -p "$existing_pid" 2>/dev/null | tr -s ' ' | sed 's/^ //;s/ $//' | cut -c1-96)
		[ -n "$live_existing_identity" ] || exit 74
		[ "$live_existing_identity" != "$existing_identity" ] || exit 75
	else
		owner_child_absent "$existing_pid" || exit 74
	fi
	rm -f "$child"
fi
child_tmp="$child.tmp.$$"
(umask 077; printf '%s\n%s\n' "$child_pid" "$child_identity" >"$child_tmp")
mv "$child_tmp" "$child"`
	clearBody := `set -eu
` + workspaceOwnerPOSIXAbsent + `
[ "$(sed -n '2p' "$state" 2>/dev/null || true)" = "$token" ] || exit 75
recorded_pid=$(sed -n '1p' "$child" 2>/dev/null || true)
recorded_identity=$(sed -n '2p' "$child" 2>/dev/null || true)
[ "$recorded_pid" = "$child_pid" ] && [ "$recorded_identity" = "$child_identity" ] || exit 74
if kill -0 "$recorded_pid" 2>/dev/null; then
	live_identity=$(ps -o lstart= -p "$recorded_pid" 2>/dev/null | tr -s ' ' | sed 's/^ //;s/ $//' | cut -c1-96)
	[ -n "$live_identity" ] && [ "$live_identity" != "$recorded_identity" ] || exit 74
else
	owner_child_absent "$recorded_pid" || exit 74
fi
rm -f "$child"`
	// An asynchronous shell list makes INT/QUIT ignored before exec. Register in
	// a foreground shell instead; close its identity pipe before user code runs.
	// The parent catches these signals while waiting so Bash reaches cleanup
	// after a signaled child; caught dispositions reset in the fresh child shell.
	// Pre-start waits use the lock's five-second deadline, never signal-based
	// cleanup. If the supervisor disappears, the identity pipe refuses handoff.
	registrar := diagnostic + `set -u
trap '' HUP
trap 'rm -rf "$run_dir" 2>/dev/null' 0
trap 'setup_failed handoff' PIPE
child_pid=$$
child_identity=$(ps -o lstart= -p "$child_pid" 2>/dev/null | tr -s ' ' | sed 's/^ //;s/ $//' | cut -c1-96)
[ -n "$child_identity" ] || setup_failed identity
export child_pid child_identity
` + gateFunction + `run_owner_gate ` + shellQuote(installBody) + ` || setup_failed registration "$?"
printf '%s\n%s\n' "$child_pid" "$child_identity" >&3 || setup_failed handoff
exec 3>&-
umask "$command_umask"
` + started + `exec sh -c ` + shellQuote(remote) + inputRedirect + `
`
	return diagnostic + `set -u
command_umask=$(umask)
umask 077
root="$HOME/.crabbox/workspace-owners"
key=` + shellQuote(key) + `
token=` + shellQuote(token) + `
state="$root/$key.owner"
child="$root/$key.child"
gate="$root/$key.gate"
run_dir="$root/$key.run.$token.$$"
` + gateFunction + `
mkdir -m 700 "$run_dir" 2>/dev/null || setup_failed staging
` + inputSetup + `export state child gate token command_umask run_dir
exec 4>&1
trap : INT QUIT
set +e
identity=$(exec /bin/sh -c ` + shellQuote(registrar) + ` 3>&1 1>&4 4>&-)
code=$?
trap - INT QUIT
exec 4>&-
if [ -z "$identity" ]; then rm -rf "$run_dir" 2>/dev/null; [ "$code" -ne 0 ] || code=74; setup_failed handoff "$code"; fi
child_pid=$(printf '%s\n' "$identity" | sed -n '1p')
child_identity=$(printf '%s\n' "$identity" | sed -n '2p')
case "$child_pid" in ''|*[!0-9]*) rm -rf "$run_dir" 2>/dev/null; exit 74 ;; esac
if [ -z "$child_identity" ] || [ "${#child_identity}" -gt 96 ]; then rm -rf "$run_dir" 2>/dev/null; exit 74; fi
export child_pid child_identity
run_owner_gate ` + shellQuote(clearBody) + `
clear_status=$?
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
`
}
