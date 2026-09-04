package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pkg/sftp"
)

const wslStageBlindingOffset, wslStageBlindingSize = 48, 32
const wslStageHeaderSize, wslStageRoot, wslStageTimeout, wslStageIdleTimeout = wslStageBlindingOffset + wslStageBlindingSize, ".crabbox/wsl-stage", 2 * time.Minute, 15 * time.Second
const sshTransportPreparationTimeout = 47 * time.Second
const wslStageCompletionMargin = time.Second
const wsl2LauncherGrace, wsl2SignalGrace, wsl2FallbackCleanupTimeout = 5 * time.Second, 5 * time.Second, 15 * time.Second
const wslStageMaxHelper = 32768
const wslStageMaxCommand = 64 << 20
const wslStageMaxSize = 1 << 40

//go:embed scripts/wsl-stage.ps1
var wslStageVerifier string

//go:embed scripts/wsl-owner.ps1
var wslWindowsOwner string

//go:embed scripts/wsl-supervisor.sh
var wslLinuxHelper string

// Framework's stdin StreamWriter may emit a UTF-8 preamble. Consume only the
// declared preamble, then the exact helper; never read ahead into command/input.
// The sentinel retains trailing newlines and wc counts bytes in every locale.
const wslHelperBootstrap = `case $2 in
0) ;;
3) p=$(dd bs=1 count=3 2>/dev/null); [ "$p" = "$(printf '\357\273\277')" ] || exit 74;;
*) exit 74;;
esac
h=$(dd bs=1 count="$1" 2>/dev/null; printf .)
h=${h%.}
[ "$(printf %s "$h" | wc -c)" -eq "$1" ] || exit 74
export CBX_HELPER="$h"
shift 2
exec bash -c "$h" sh "$@"`

const wslStageCleanupTimeout, wslStageCanceledCleanupTimeout = 15 * time.Second, 5 * time.Second
const wslSFTPDiagnosticLimit = 4 << 10

// Keep the complete encoded launcher strictly below cmd.exe's 8191-byte limit.
const wslStageLauncherCommandLimit = 8191
const wslStagePreparationMarker = "CBX_WSL_STAGE_READY_V1"

var errWSLSFTPUnavailable = errors.New("WSL2 staged transport requires the target OpenSSH SFTP subsystem")
var wslSFTPRejected = regexp.MustCompile(`(?m)^subsystem request failed on channel [0-9]+\r?$`)
var stageWSLSpool = (*wslStageSpool).stage
var uploadWSLSpool = (*wslStageSpool).upload
var probeWSLStageRoute = probeWSLStageTransport
var prepareWSLStageRoute = prepareWSLStageRoot
var cleanupPublishedWSLStage = cleanupWSLStageReady
var buildWSLStageLauncher = wslStageLauncherCommand
var publishWSLStage = (*sftp.Client).Rename
var startWSLSFTPSubsystem = startOwnedSSHTransportSubsystem
var startWSLStageUploadSubsystem = startOwnedSSHTransportSubsystem
var startWSLStageCleanupSubsystem = startOwnedSSHTransportSubsystem
var discardWSLStageFile = discardWSLStageFileNative
var wslStageEntropy io.Reader = rand.Reader

func IsWSLSFTPUnavailable(err error) bool { return errors.Is(err, errWSLSFTPUnavailable) }

type wslStageTiming struct{ stage, prepare, cleanup, operation, execute, idle, reserve time.Duration }

type wslStageBudgets struct {
	ports                                        []string
	prepare, transfer, cleanup, candidate, total time.Duration
}

// A route owns preparation, size-scaled upload, process exit and exact
// cleanup. No candidate may borrow the next candidate's complete allocation.
func wslStageRouteBudgets(target SSHTarget, timing wslStageTiming, size int64) wslStageBudgets {
	ports := sshPortCandidates(target.Port, target.FallbackPorts)
	if len(ports) == 0 {
		ports = []string{"22"}
	}
	prepare, cleanup := timing.prepare, timing.cleanup
	if prepare <= 0 {
		prepare = sshTransportPreparationTimeout
	}
	if cleanup <= 0 {
		cleanup = wslStageCleanupTimeout
	}
	transfer := wslStageBudget(timing.stage, timing.idle, size)
	candidate := addWSLStageDurations(prepare, transfer, cleanup, 2*sshCommandWaitDelay)
	total := time.Duration(0)
	for range ports {
		total = addWSLStageDurations(total, candidate)
	}
	return wslStageBudgets{ports: ports, prepare: prepare, transfer: transfer, cleanup: cleanup, candidate: candidate, total: addWSLStageDurations(total, wslStageCompletionMargin)}
}

func addWSLStageDurations(values ...time.Duration) time.Duration {
	const maximum = time.Duration(1<<63 - 1)
	var total time.Duration
	for _, value := range values {
		if value > maximum-total {
			return maximum
		}
		total += value
	}
	return total
}

// Finite control calls use a fixed metadata allowance; ordinary unlimited
// execution never grants a control call an unlimited deadline.
const sshControlMetadataLimit = 1 << 20
const sshControlExecutionLimit = 12 * time.Second

func sshTransportTiming(limit sshCommandLimit) wslStageTiming {
	timing := wslStageTiming{stage: wslStageTimeout, idle: wslStageIdleTimeout, operation: limit.execution}
	if limit.control && limit.execution > 0 {
		// Fixed owner scripts share one finite native clock for startup, work,
		// TERM grace, post-KILL absence polling, and completion. Never reset it.
		timing.operation = addWSLStageDurations(wslStageIdleTimeout, limit.execution, 2*wsl2SignalGrace, wslStageCompletionMargin)
	}
	if limit.execution > 0 {
		timing.execute = addWSLStageDurations(sshTransportPreparationTimeout, timing.operation, 2*wsl2LauncherGrace, wsl2FallbackCleanupTimeout)
	}
	if limit.control {
		// Upload still uses the raw work allowance, not the native guard.
		timing.stage = addWSLStageDurations(sshTransportPreparationTimeout, limit.execution)
		timing.idle = 2 * time.Second
		timing.reserve = addWSLStageDurations(timing.execute, wslStageCleanupTimeout, sshCommandWaitDelay, wslStageCompletionMargin)
	}
	return timing
}

func sshTransportCallBudget(target SSHTarget, size int64, limit sshCommandLimit) time.Duration {
	if limit.execution <= 0 {
		return 0
	}
	if !isWindowsWSL2Target(target) {
		routes := len(sshPortCandidates(target.Port, target.FallbackPorts))
		attempts := 1
		if routes > 1 {
			attempts += routes
		}
		total := limit.execution
		for range attempts {
			total = addWSLStageDurations(total, sshTransportPreparationTimeout)
		}
		return total
	}
	timing := sshTransportTiming(limit)
	completion := addWSLStageDurations(timing.execute, wslStageCleanupTimeout, sshCommandWaitDelay, wslStageCompletionMargin)
	return addWSLStageDurations(wslStageRouteBudgets(target, timing, size).total, completion)
}

type wslStageCleanupContextKey struct{}
type wslStageCleanupPhase struct {
	ctx     context.Context
	timeout time.Duration
	active  context.Context
	cancel  context.CancelFunc
}

func (p *wslStageCleanupPhase) start() context.Context {
	if p.active == nil {
		p.active, p.cancel = wslStageCleanupContext(p.ctx, p.timeout, wslStageCanceledCleanupTimeout)
	}
	return p.active
}

type wslStageSpool struct {
	setupMarker string
	input       *replayableSSHInput
	size        int64
	expected    [sha256.Size]byte
	timing      wslStageTiming
	routeProof  string
	shell       wslStageShell
}
type wslStageRouteProofKey struct{}
type retryableWSLStageError struct{ error }
type nonRetryableWSLStageError struct{ error }
type wslStageOwnership uint8

const (
	wslStageUnowned wslStageOwnership = iota
	wslStagePartial
	wslStageCreationUnknown
	wslStagePublished
	wslStagePublicationUnknown
)

type boundedWSLDiagnostics struct {
	buffer    bytes.Buffer
	remaining int
	redactor  *redactedWSLDiagnostics
}
type redactedWSLDiagnostics struct {
	target  SSHTarget
	writer  io.Writer
	pending []byte
}

func (e retryableWSLStageError) Unwrap() error { return e.error }
func (e nonRetryableWSLStageError) Unwrap() error {
	return e.error
}
func (w *boundedWSLDiagnostics) Write(p []byte) (int, error) {
	total, size := len(p), min(len(p), w.remaining)
	if size > 0 {
		_, _ = w.buffer.Write(p[:size])
		w.remaining -= size
	}
	return total, nil
}
func (w *redactedWSLDiagnostics) Write(p []byte) (int, error) {
	total := len(p)
	if !w.target.AuthSecret || w.target.User == "" {
		return w.write(p, total)
	}
	data := append(w.pending, p...)
	safe := len(data) - len(w.target.User) + 1
	if safe <= 0 {
		w.pending = data
		return total, nil
	}
	for offset := 0; offset < safe; {
		match := strings.Index(string(data[offset:]), w.target.User)
		if match < 0 {
			break
		}
		match += offset
		if match >= safe {
			break
		}
		if match+len(w.target.User) > safe {
			safe = match
			break
		}
		offset = match + len(w.target.User)
	}
	if _, err := w.write(data[:safe], safe); err != nil {
		return 0, err
	}
	w.pending = append(w.pending[:0], data[safe:]...)
	return total, nil
}

func (w *redactedWSLDiagnostics) write(p []byte, consumed int) (int, error) {
	_, err := io.WriteString(w.writer, redactSSHTransportDiagnostic(w.target, string(p)))
	if err != nil {
		return 0, err
	}
	return consumed, nil
}

func (w *boundedWSLDiagnostics) flush() error {
	if w.redactor == nil || len(w.redactor.pending) == 0 {
		return nil
	}
	_, err := w.redactor.write(w.redactor.pending, len(w.redactor.pending))
	w.redactor.pending = nil
	return err
}
func captureWSLSFTPDiagnostics(target SSHTarget, stderr io.Writer) (io.Writer, *boundedWSLDiagnostics) {
	if stderr == nil {
		stderr = io.Discard
	}
	redactor := &redactedWSLDiagnostics{target: target, writer: stderr}
	capture := &boundedWSLDiagnostics{remaining: wslSFTPDiagnosticLimit, redactor: redactor}
	return io.MultiWriter(redactor, capture), capture
}

func wslStageBudget(floor, idle time.Duration, frameBytes int64) time.Duration {
	const (
		bytesPerStep = int64(1 << 20)
		maxDuration  = time.Duration(1<<63 - 1)
	)
	if floor <= 0 || idle <= 0 || frameBytes <= bytesPerStep {
		return floor
	}
	steps := frameBytes / bytesPerStep
	if frameBytes%bytesPerStep != 0 {
		steps++
	}
	extraSteps := steps - 1
	if floor >= maxDuration || extraSteps > int64((maxDuration-floor)/idle) {
		return maxDuration
	}
	return floor + time.Duration(extraSteps)*idle
}

func newWSLStageSpool(command string, payload []byte, source io.ReadSeeker, payloadSize int64, limit sshCommandLimit) (*wslStageSpool, error) {
	owner := strings.NewReplacer("@BOOTSTRAP@", psQuote(wslHelperBootstrap),
		"@STARTUP@", fmt.Sprint(wslStageIdleTimeout.Milliseconds())).Replace(wslWindowsOwner)
	if len(owner) == 0 || len(wslLinuxHelper) == 0 || len(owner) > wslStageMaxHelper || len(wslLinuxHelper) > wslStageMaxHelper ||
		!utf8.ValidString(wslLinuxHelper) || strings.ContainsRune(wslLinuxHelper, 0) ||
		len(command) > wslStageMaxCommand || payloadSize < 0 || payloadSize > wslStageMaxSize || limit.execution < 0 {
		return nil, errors.New("WSL2 envelope exceeds bounded descriptor limits")
	}
	if source == nil {
		source = bytes.NewReader(payload)
	}
	size, err := source.Seek(0, io.SeekEnd)
	if err != nil || size != payloadSize {
		return nil, errors.Join(err, errors.New("WSL2 stage input size changed"))
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	timing := sshTransportTiming(limit)
	var descriptor [wslStageHeaderSize]byte
	copy(descriptor[:], "CBXFLAT2")
	binary.LittleEndian.PutUint32(descriptor[8:], uint32(len(owner)))
	binary.LittleEndian.PutUint32(descriptor[12:], uint32(len(wslLinuxHelper)))
	binary.LittleEndian.PutUint64(descriptor[16:], uint64(len(command)))
	binary.LittleEndian.PutUint64(descriptor[24:], uint64(payloadSize))
	binary.LittleEndian.PutUint64(descriptor[32:], uint64(timing.operation.Milliseconds()))
	binary.LittleEndian.PutUint32(descriptor[40:], uint32(timing.idle.Milliseconds()))
	binary.LittleEndian.PutUint32(descriptor[44:], uint32(wsl2SignalGrace.Milliseconds()))
	// Keep hidden entropy before all program/user bytes, including in partial
	// digests. Generate once, before any spool file, and retain it across retries.
	if _, err := io.ReadFull(wslStageEntropy, descriptor[wslStageBlindingOffset:]); err != nil {
		return nil, errors.New("generate private WSL2 envelope blinding failed")
	}
	prefix := append(descriptor[:], []byte(owner+wslLinuxHelper+command)...)
	total := int64(len(prefix)) + payloadSize
	if total > wslStageMaxSize {
		return nil, errors.New("WSL2 envelope is too large")
	}
	input, err := newReplayableSSHInputStream(prefix, source, payloadSize)
	if err != nil {
		return nil, err
	}
	spool := &wslStageSpool{input: input, size: total, timing: timing}
	spool.expected, err = spool.prefixDigest(context.Background(), total)
	if err != nil {
		_ = spool.close()
		return nil, err
	}
	return spool, nil
}
func (s *wslStageSpool) close() error              { return s.input.close() }
func (s *wslStageSpool) digest() [sha256.Size]byte { return s.expected }
func (s *wslStageSpool) prefixDigest(ctx context.Context, size int64) (digest [sha256.Size]byte, err error) {
	if size < 0 || size > s.size {
		return digest, errors.New("invalid WSL2 stage prefix length")
	}
	if cause := context.Cause(ctx); cause != nil {
		return digest, cause
	}
	reader, err := s.input.reset()
	if err != nil {
		return digest, err
	}
	hash := sha256.New()
	buffer := make([]byte, 32<<10)
	for remaining := size; remaining > 0; {
		if cause := context.Cause(ctx); cause != nil {
			return digest, cause
		}
		count := min(int64(len(buffer)), remaining)
		if _, err := io.ReadFull(reader, buffer[:count]); err != nil {
			return digest, err
		}
		_, _ = hash.Write(buffer[:count])
		remaining -= count
	}
	// A deadline during the final read must not grant deletion authority.
	if cause := context.Cause(ctx); cause != nil {
		return digest, cause
	}
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

func (s *wslStageSpool) run(ctx context.Context, target *SSHTarget, connectTimeout, attempts string, stdout, stderr io.Writer) (err error) {
	if err := requireWSLStageExecutionReserve(ctx, s.timing.reserve); err != nil {
		return err
	}
	nonce, err := stageWSLSpool(s, ctx, target, s.timing, connectTimeout, attempts, stderr)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if cleanupErr := cleanupPublishedWSLStage(ctx, *target, nonce, s, wslStageCleanupBudget(ctx), wslStageCanceledCleanupTimeout, connectTimeout); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("owned WSL2 ready stage cleanup failed: %w", cleanupErr))
			}
		}
	}()
	if err := requireWSLStageExecutionReserve(ctx, s.timing.reserve); err != nil {
		return err
	}
	command := buildWSLStageLauncher(nonce, s.size, s.digest(), s.shell)
	if command == "" || len(command) >= wslStageLauncherCommandLimit {
		return errors.New("WSL2 stage launcher exceeds its command budget")
	}
	execCtx, cancel := ctx, func() {}
	if s.timing.execute > 0 {
		execCtx, cancel = context.WithTimeout(ctx, s.timing.execute)
	}
	defer cancel()
	stdout, stderr, finish := workspaceOwnerSetupStreams(s.setupMarker, stdout, stderr)
	transport := sshTransportPreparation{command: command}
	_, err = transport.runOnce(execCtx, *target, connectTimeout, attempts, stdout, stderr, false)
	if err == nil {
		err = context.Cause(execCtx)
	}
	if err != nil && (shouldRetrySSHPort(err) || errors.Is(context.Cause(execCtx), context.DeadlineExceeded)) {
		err = exit(7, "WSL2 staged command result is ambiguous: %v", err)
	}
	return finish(err)
}

func requireWSLStageExecutionReserve(ctx context.Context, reserve time.Duration) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if reserve <= 0 {
		return nil
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= reserve {
		return exit(7, "WSL2 staged command cannot preserve its execution and cleanup deadline")
	}
	return nil
}

func (s *wslStageSpool) stage(ctx context.Context, target *SSHTarget, timing wslStageTiming, connectTimeout, attempts string, stderr io.Writer) (string, error) {
	budgets := wslStageRouteBudgets(*target, timing, s.size)
	budget := budgets.total
	if timing.reserve > 0 {
		if err := requireWSLStageExecutionReserve(ctx, timing.reserve); err != nil {
			return "", err
		}
		deadline, _ := ctx.Deadline()
		budget = min(budget, time.Until(deadline)-timing.reserve)
	}
	stageCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	candidate := *target
	candidate.NoControlMaster = true
	candidate.FallbackPorts = []string{}
	var err error
	for _, port := range budgets.ports {
		if cause := context.Cause(stageCtx); cause != nil {
			return "", cause
		}
		candidate.Port = port
		nonce, nonceErr := randomHex(16)
		if nonceErr != nil {
			return "", nonceErr
		}
		candidateCtx, cancelCandidate := context.WithTimeout(stageCtx, budgets.candidate)
		timeoutErr := retryableWSLStageError{fmt.Errorf("WSL2 stage candidate timed out: %w", context.DeadlineExceeded)}
		prepareCtx, cancelPrepare := context.WithTimeoutCause(candidateCtx, budgets.prepare, timeoutErr)
		// Only this mutation-free phase may fail over without exact cleanup.
		// Its time comes out of the same preparation budget as the nonce proof.
		if len(budgets.ports) > 1 {
			err = probeWSLStageRoute(prepareCtx, candidate, connectTimeout, attempts)
			if cause := context.Cause(prepareCtx); cause != nil {
				err = cause
			}
			if err != nil {
				cancelPrepare()
				cancelCandidate()
				if cause := context.Cause(stageCtx); cause != nil {
					return "", errors.Join(cause, err)
				}
				var retryable retryableWSLStageError
				if !errors.As(err, &retryable) {
					return "", err
				}
				continue
			}
		}
		proofCtx := context.WithValue(prepareCtx, wslStageRouteProofKey{}, nonce)
		s.shell, err = prepareWSLStageRoute(proofCtx, candidate, connectTimeout, attempts)
		cancelPrepare()
		cleanupPhase := &wslStageCleanupPhase{ctx: candidateCtx, timeout: budgets.cleanup}
		if err == nil {
			operationCtx, cancelOperation := context.WithTimeoutCause(candidateCtx, budgets.transfer, timeoutErr)
			operationCtx = context.WithValue(operationCtx, wslStageCleanupContextKey{}, cleanupPhase)
			s.routeProof = nonce
			err = uploadWSLSpool(s, operationCtx, candidate, nonce, timing.idle, connectTimeout, attempts, stderr)
			cancelOperation()
		}
		proofErr := discardWSLStageFile(cleanupPhase.start(), candidate, nonce, "."+nonce+".proof", int64(len(nonce)), sha256.Sum256([]byte(nonce)), connectTimeout)
		if proofErr != nil {
			if err == nil {
				err = cleanupPublishedWSLStage(cleanupPhase.active, candidate, nonce, s, budgets.cleanup, wslStageCanceledCleanupTimeout, connectTimeout)
			}
			err = nonRetryableWSLStageError{errors.Join(err, fmt.Errorf("WSL2 route proof cleanup failed: %w", proofErr))}
		}
		cleanupPhase.cancel()
		cancelCandidate()
		if err == nil {
			target.Port, target.FallbackPorts, target.NoControlMaster = port, []string{}, true
			return nonce, nil
		}
		if cause := context.Cause(stageCtx); cause != nil {
			return "", errors.Join(cause, err)
		}
		var nonRetryable nonRetryableWSLStageError
		if errors.As(err, &nonRetryable) {
			return "", err
		}
		var retryable retryableWSLStageError
		if !errors.As(err, &retryable) {
			return "", err
		}
	}
	return "", err
}

func probeWSLStageTransport(ctx context.Context, target SSHTarget, connectTimeout, attempts string) error {
	transport := sshTransportPreparation{command: sshTransportProbeCommand(target)}
	_, err := transport.runOnce(ctx, target, connectTimeout, attempts, io.Discard, io.Discard, false)
	if shouldRetrySSHPort(err) {
		return retryableWSLStageError{fmt.Errorf("probe WSL2 stage route failed: %w", err)}
	}
	return err
}

func prepareWSLStageRoot(ctx context.Context, target SSHTarget, connectTimeout, attempts string) (wslStageShell, error) {
	return prepareWSLStageRootWithin(ctx, target, connectTimeout, attempts, sshTransportPreparationTimeout)
}

func prepareWSLStageRootWithin(ctx context.Context, target SSHTarget, connectTimeout, attempts string, timeout time.Duration) (wslStageShell, error) {
	prepareCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var output bytes.Buffer
	proof, _ := ctx.Value(wslStageRouteProofKey{}).(string)
	transport := sshTransportPreparation{command: wslStageRootPreparationCommand(proof)}
	if _, err := transport.runOnce(prepareCtx, target, connectTimeout, attempts, &output, io.Discard, false); err != nil {
		if cause := context.Cause(prepareCtx); cause != nil {
			if errors.Is(cause, context.DeadlineExceeded) && context.Cause(ctx) == nil {
				return "", retryableWSLStageError{fmt.Errorf("prepare private WSL2 stage route timed out: %w", cause)}
			}
			return "", cause
		}
		if shouldRetrySSHPort(err) {
			return "", retryableWSLStageError{fmt.Errorf("prepare private WSL2 stage transport failed: %w", err)}
		}
		return "", nonRetryableWSLStageError{errors.New("prepare private WSL2 stage failed")}
	}
	fields := strings.Fields(output.String())
	if len(fields) != 3 || fields[0] != wslStagePreparationMarker || fields[1] != proof || !wslStageShell(fields[2]).valid() {
		return "", nonRetryableWSLStageError{errors.New("prepare private WSL2 stage returned an invalid readiness marker")}
	}
	return wslStageShell(fields[2]), nil
}

func wslStageRootPreparationCommand(proofs ...string) string {
	proof := ""
	if len(proofs) > 0 && proofs[0] != "" {
		proof = `  $proof = [IO.File]::Open((Join-Path $root '.` + proofs[0] + `.proof'), 'CreateNew')
  try { $proof.Write([Text.Encoding]::ASCII.GetBytes('` + proofs[0] + `'),0,32) } finally { $proof.Dispose() }
`
	}
	// Preflight all existing directories before mutation: ACL tightening cannot revoke open handles.
	// Reuse the full validator to leave canonical directories untouched: SetAccessControl's
	// Persist path can change inheritance metadata on an explicit CreateDirectory descriptor.
	// Persist also clears modified flags, so each directory needing normalization gets a fresh descriptor.
	return wslStagePowerShellCommand(`$ErrorActionPreference = "Stop"
try {
  $parentPID = (Get-CimInstance Win32_Process -Filter "ProcessId=$PID").ParentProcessId
  $shell = (Get-Process -Id $parentPID).ProcessName.ToLowerInvariant()
  if ($shell -eq "pwsh") { $shell = "powershell" }
  if ($shell -notin "cmd", "powershell") { throw "unsupported SSH shell" }
  $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User
  $allowed = @($sid.Value, "S-1-5-18", "S-1-5-32-544" | Select-Object -Unique)
  function Test-StageDirectory($path, $protected = $false, $owners = @($sid.Value)) {
    $item = Get-Item -LiteralPath $path -Force
    if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw "invalid directory" }
    $actual = [IO.Directory]::GetAccessControl($path)
    if ($actual.GetOwner($sid.GetType()).Value -notin $owners) { throw "invalid owner" }
    $raw = [Security.AccessControl.RawSecurityDescriptor]::new($actual.GetSecurityDescriptorBinaryForm(), 0)
    if ($null -eq $raw.DiscretionaryAcl) { throw "null DACL" }
    $rules = @($actual.GetAccessRules($true, $true, $sid.GetType()))
    foreach ($rule in $rules) {
      if ($rule.AccessControlType -eq "Allow" -and $rule.IdentityReference.Value -notin $allowed) { throw "untrusted grant" }
    }
    if ($protected) {
      if (-not $actual.AreAccessRulesProtected -or -not $actual.AreAccessRulesCanonical -or $rules.Count -ne $allowed.Count) { throw "invalid DACL" }
      foreach ($value in $allowed) {
        $grant = @($rules | Where-Object { $_.IdentityReference.Value -eq $value })
        if ($grant.Count -ne 1 -or $grant[0].IsInherited -or $grant[0].AccessControlType -ne "Allow" -or
            $grant[0].FileSystemRights -ne "FullControl" -or $grant[0].InheritanceFlags -ne "ContainerInherit,ObjectInherit" -or
            $grant[0].PropagationFlags -ne "None") { throw "invalid grant" }
      }
    }
  }
  $parent = Join-Path $HOME ".crabbox"
  $root = Join-Path $parent "wsl-stage"
  Test-StageDirectory $HOME $false $allowed
  foreach ($path in @($parent, $root)) {
    if (Test-Path -LiteralPath $path) { Test-StageDirectory $path }
  }
  foreach ($path in @($parent, $root)) {
    $acl = [Security.AccessControl.DirectorySecurity]::new()
    $acl.SetOwner($sid)
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($value in $allowed) {
      $principal = [Security.Principal.SecurityIdentifier]::new($value)
      $rule = [Security.AccessControl.FileSystemAccessRule]::new($principal, "FullControl", "ContainerInherit,ObjectInherit", "None", "Allow")
      $acl.AddAccessRule($rule)
    }
    if (Test-Path -LiteralPath $path) {
      Test-StageDirectory $path
      try { Test-StageDirectory $path $true; continue } catch {}
      [IO.Directory]::SetAccessControl($path, $acl)
    } else {
      [void][IO.Directory]::CreateDirectory($path, $acl)
    }
    Test-StageDirectory $path $true
  }
`+proof+`
  [Console]::Out.WriteLine("`+wslStagePreparationMarker+` `+firstNonBlank(proofs...)+` $shell")
} catch {
  [Console]::Error.WriteLine("WSL2 private stage preparation failed")
  exit 74
}`, wslStageCMD)
}

func (s *wslStageSpool) upload(ctx context.Context, target SSHTarget, nonce string, idle time.Duration, connectTimeout, attempts string, stderr io.Writer) (err error) {
	attemptCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	tee, diagnostics := captureWSLSFTPDiagnostics(target, stderr)
	defer diagnostics.flush()
	output, input, wait, err := startWSLStageUploadSubsystem(attemptCtx, target, connectTimeout, attempts, "sftp", tee)
	if err != nil {
		return retryWSLStageTransport(err)
	}
	client, err := sftp.NewClientPipe(output, input)
	if err != nil {
		_ = input.Close()
		waitErr := wait()
		if classified := classifyWSLSFTPInit(err, waitErr, diagnostics.buffer.String()); IsWSLSFTPUnavailable(classified) {
			return finishWSLStageAttempt(attemptCtx, classified, nil, nil)
		}
		return finishWSLStageAttempt(attemptCtx, wslStageOperationError("upload WSL2 stage failed", err), nil, waitErr)
	}
	ownership, operationErr := uploadToSFTP(client, s, nonce, idle, cancel)
	result := finishWSLStageAttempt(attemptCtx, operationErr, client.Close(), wait())
	cleanupBudget := wslStageCleanupBudget(ctx)
	if phase, ok := ctx.Value(wslStageCleanupContextKey{}).(*wslStageCleanupPhase); ok {
		ctx = phase.start()
		cleanupBudget = phase.timeout
	}
	if ownership == wslStageCreationUnknown {
		return nonRetryableWSLStageError{errors.Join(result, errors.New("WSL2 stage exclusive creation was not acknowledged; cleanup ownership unknown"))}
	}
	if result == nil || ownership == wslStageUnowned {
		return result
	}
	if ownership == wslStagePublicationUnknown {
		partErr := cleanupWSLStagePart(ctx, target, nonce, s, cleanupBudget, wslStageCanceledCleanupTimeout, connectTimeout)
		readyErr := cleanupPublishedWSLStage(ctx, target, nonce, s, min(cleanupBudget, wslStageCleanupBudget(ctx)), wslStageCanceledCleanupTimeout, connectTimeout)
		if cleanupErr := errors.Join(partErr, readyErr); cleanupErr != nil {
			result = errors.Join(result, fmt.Errorf("ambiguous WSL2 stage publication cleanup failed: %w", cleanupErr))
		}
		return nonRetryableWSLStageError{result}
	}
	if ownership == wslStagePublished {
		if cleanupErr := cleanupPublishedWSLStage(ctx, target, nonce, s, cleanupBudget, wslStageCanceledCleanupTimeout, connectTimeout); cleanupErr != nil {
			result = errors.Join(result, fmt.Errorf("owned WSL2 ready stage cleanup failed: %w", cleanupErr))
		}
		return nonRetryableWSLStageError{result}
	}
	if cleanupErr := cleanupWSLStagePart(ctx, target, nonce, s, cleanupBudget, wslStageCanceledCleanupTimeout, connectTimeout); cleanupErr != nil {
		if stderr == nil {
			stderr = io.Discard
		}
		detail := trimFailureDetail(cleanupErr.Error())
		_, _ = fmt.Fprintf(stderr, "warning: WSL2 stage partial cleanup failed: %s\n", detail)
		return nonRetryableWSLStageError{errors.Join(result, fmt.Errorf("owned WSL2 stage partial cleanup failed: %w", cleanupErr))}
	}
	return result
}

var probeWSLSFTPSubsystem = func(ctx context.Context, target SSHTarget, connectTimeout, attempts string, stderr io.Writer) error {
	tee, diagnostics := captureWSLSFTPDiagnostics(target, stderr)
	defer diagnostics.flush()
	output, input, wait, err := startWSLSFTPSubsystem(ctx, target, connectTimeout, attempts, "sftp", tee)
	if err != nil {
		return err
	}
	client, probeErr := sftp.NewClientPipe(output, input)
	var closeErr error
	if probeErr == nil {
		closeErr = client.Close()
	} else {
		_ = input.Close()
	}
	waitErr := wait()
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if probeErr != nil {
		return classifyWSLSFTPInit(probeErr, waitErr, diagnostics.buffer.String())
	}
	return errors.Join(closeErr, waitErr)
}

func classifyWSLSFTPInit(protocolErr, waitErr error, diagnostic string) error {
	if protocolErr != nil && exitCode(waitErr) == 255 && wslSFTPRejected.MatchString(diagnostic) {
		return errors.Join(errWSLSFTPUnavailable, protocolErr)
	}
	return errors.Join(protocolErr, waitErr)
}

func finishWSLStageAttempt(ctx context.Context, operationErr, closeErr, waitErr error) error {
	err := errors.Join(operationErr, closeErr, waitErr)
	var retryable retryableWSLStageError
	cause := context.Cause(ctx)
	if errors.As(cause, &retryable) {
		return retryableWSLStageError{errors.Join(cause, err)}
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		if operationErr != nil {
			return fmt.Errorf("%w: %v", cause, operationErr)
		}
		return cause
	}
	if operationErr != nil {
		var nonRetryable nonRetryableWSLStageError
		if errors.As(operationErr, &nonRetryable) {
			return errors.Join(operationErr, closeErr)
		}
		if errors.As(operationErr, &retryable) {
			return retryableWSLStageError{err}
		}
		if isWSLStageConnectionLoss(operationErr) {
			return retryableWSLStageError{err}
		}
		return errors.Join(operationErr, closeErr)
	}
	if waitErr != nil {
		return retryWSLStageTransport(err)
	}
	return err
}
func isWSLStageConnectionLoss(err error) bool {
	if IsWSLSFTPUnavailable(err) {
		return false
	}
	var status *sftp.StatusError
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, sftp.ErrSSHFxConnectionLost) || errors.Is(err, sftp.ErrSSHFxNoConnection) ||
		errors.As(err, &status) && (status.FxCode() == sftp.ErrSSHFxConnectionLost || status.FxCode() == sftp.ErrSSHFxNoConnection)
}
func wslStageOperationError(message string, err error) error {
	if err == nil {
		return nonRetryableWSLStageError{errors.New(message)}
	}
	wrapped := fmt.Errorf("%s: %w", message, err)
	if isWSLStageConnectionLoss(err) {
		return retryableWSLStageError{wrapped}
	}
	return nonRetryableWSLStageError{wrapped}
}
func wslStageCleanupBudget(ctx context.Context) time.Duration {
	if context.Cause(ctx) != nil {
		return wslStageCanceledCleanupTimeout
	}
	return wslStageCleanupTimeout
}
func uploadToSFTP(client *sftp.Client, spool *wslStageSpool, nonce string, idle time.Duration, cancel context.CancelCauseFunc) (wslStageOwnership, error) {
	info, err := client.Lstat(wslStageRoot)
	if err != nil {
		return wslStageUnowned, wslStageOperationError("upload WSL2 stage failed: private stage root unavailable", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return wslStageUnowned, wslStageOperationError("upload WSL2 stage failed: invalid stage root", nil)
	}
	if spool.routeProof != "" {
		if err := verifyWSLStageRouteProof(client, nonce, spool.routeProof); err != nil {
			return wslStageUnowned, wslStageOperationError("upload WSL2 stage failed: private stage root binding unavailable", err)
		}
	}
	part, ready := wslStageRoot+"/"+nonce+".part", wslStageRoot+"/"+nonce+".ready"
	remote, err := client.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		if isWSLStageConnectionLoss(err) {
			return wslStageCreationUnknown, wslStageOperationError("upload WSL2 stage failed", err)
		}
		return wslStageUnowned, wslStageOperationError("upload WSL2 stage failed", err)
	}
	fail := func(cause error) error { return errors.Join(cause, remote.Close()) }
	source, err := spool.input.reset()
	if err != nil {
		return wslStagePartial, fail(wslStageOperationError("upload WSL2 stage failed", err))
	}
	if err := copyWSLStage(remote, source, spool.size, idle, cancel); err != nil {
		return wslStagePartial, fail(wslStageOperationError("upload WSL2 stage failed", err))
	}
	if err := remote.Close(); err != nil {
		return wslStagePartial, wslStageOperationError("upload WSL2 stage failed", err)
	}
	if err := verifyWSLStageMetadata(client, part, spool.size); err != nil {
		return wslStagePartial, err
	}
	if _, err := client.Lstat(ready); err == nil {
		return wslStagePartial, wslStageOperationError("publish WSL2 stage failed: nonce collision", nil)
	} else if !errors.Is(err, os.ErrNotExist) {
		return wslStagePartial, wslStageOperationError("publish WSL2 stage failed", err)
	}
	if err := publishWSLStage(client, part, ready); err != nil {
		return wslStagePublicationUnknown, wslStageOperationError("publish WSL2 stage failed", err)
	}
	return wslStagePublished, nil
}

func verifyWSLStageRouteProof(client *sftp.Client, nonce, expected string) error {
	if nonce != expected {
		return errors.New("invalid private stage route binding")
	}
	path := wslStageRoot + "/." + nonce + ".proof"
	info, err := client.Lstat(path)
	if err != nil {
		return errors.New("private stage route binding unavailable")
	}
	if !info.Mode().IsRegular() || info.Size() != int64(len(expected)) {
		return errors.New("invalid private stage route binding")
	}
	file, err := client.Open(path)
	if err != nil {
		return errors.New("private stage route binding unavailable")
	}
	proof, readErr := io.ReadAll(io.LimitReader(file, int64(len(expected)+1)))
	if err := errors.Join(readErr, file.Close()); err != nil || subtle.ConstantTimeCompare(proof, []byte(expected)) != 1 {
		return errors.New("invalid private stage route binding")
	}
	return nil
}

func cleanupWSLStagePart(ctx context.Context, target SSHTarget, nonce string, spool *wslStageSpool, timeout, canceledTimeout time.Duration, connectTimeout string) error {
	return cleanupWSLStage(ctx, target, nonce, spool, true, timeout, canceledTimeout, connectTimeout)
}

func cleanupWSLStageReady(ctx context.Context, target SSHTarget, nonce string, spool *wslStageSpool, timeout, canceledTimeout time.Duration, connectTimeout string) error {
	return cleanupWSLStage(ctx, target, nonce, spool, false, timeout, canceledTimeout, connectTimeout)
}

func wslStageCleanupContext(ctx context.Context, timeout, canceledTimeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx.Err() != nil {
		timeout = min(timeout, canceledTimeout)
	} else if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, max(0, time.Until(deadline)))
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			timer := time.NewTimer(canceledTimeout)
			defer timer.Stop()
			select {
			case <-timer.C:
				cancel()
			case <-stop:
			}
		case <-stop:
		}
	}()
	return cleanupCtx, func() { close(stop); cancel() }
}

func cleanupWSLStage(ctx context.Context, target SSHTarget, nonce string, spool *wslStageSpool, partial bool, timeout, canceledTimeout time.Duration, connectTimeout string) error {
	if len(nonce) != 32 || strings.Trim(nonce, "0123456789abcdef") != "" || spool == nil {
		return errors.New("invalid WSL2 stage cleanup identity")
	}
	cleanupCtx, cancel := wslStageCleanupContext(ctx, timeout, canceledTimeout)
	defer cancel()
	name, size, digest := nonce+".ready", spool.size, spool.digest()
	if partial {
		name = nonce + ".part"
		observed, err := observeWSLStagePart(cleanupCtx, target, name, connectTimeout)
		if err != nil {
			return err
		}
		if observed < 0 {
			return nil
		}
		size = observed
		digest, err = spool.prefixDigest(cleanupCtx, size)
		if err != nil {
			return err
		}
	}
	return discardWSLStageFile(cleanupCtx, target, nonce, name, size, digest, connectTimeout)
}

// This read grants no deletion authority. The native verifier subsequently
// compares this exact length and expected local prefix through its pinned handle.
func observeWSLStagePart(ctx context.Context, target SSHTarget, name, connectTimeout string) (int64, error) {
	target.NoControlMaster, target.FallbackPorts = true, []string{}
	output, input, wait, err := startWSLStageCleanupSubsystem(ctx, target, connectTimeout, "1", "sftp", io.Discard)
	if err != nil {
		return 0, err
	}
	stop := context.AfterFunc(ctx, func() {
		_ = input.Close()
		if closer, ok := output.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	defer stop()
	client, err := sftp.NewClientPipe(output, input)
	if err != nil {
		_ = input.Close()
		return 0, errors.Join(err, wait())
	}
	info, statErr := client.Lstat(wslStageRoot + "/" + name)
	closeErr, waitErr := client.Close(), wait()
	if cause := context.Cause(ctx); cause != nil {
		return 0, cause
	}
	if errors.Is(statErr, os.ErrNotExist) {
		return -1, errors.Join(closeErr, waitErr)
	}
	if err := errors.Join(statErr, closeErr, waitErr); err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > wslStageMaxSize {
		return 0, errors.New("refuse non-regular or unbounded WSL2 partial")
	}
	return info.Size(), nil
}

func discardWSLStageFileNative(ctx context.Context, target SSHTarget, nonce, name string, size int64, digest [sha256.Size]byte, connectTimeout string) error {
	command := wslStageFileCommand(nonce, name, size, digest, true, wslStageCMD)
	if command == "" || len(command) >= wslStageLauncherCommandLimit {
		return errors.New("invalid WSL2 discard command")
	}
	target.NoControlMaster, target.FallbackPorts = true, []string{}
	transport := sshTransportPreparation{command: command}
	_, err := transport.runOnce(ctx, target, connectTimeout, "1", io.Discard, io.Discard, false)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("verified WSL2 stage discard failed: %w", err)
	}
	return nil
}

// Metadata gates publication; only the mandatory native exclusive-handle
// verifier grants execution/deletion authority from the complete digest.
func verifyWSLStageMetadata(client *sftp.Client, path string, size int64) error {
	info, err := client.Lstat(path)
	if err != nil {
		return wslStageOperationError("verify WSL2 stage metadata failed", err)
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return wslStageOperationError("verify WSL2 stage metadata failed", nil)
	}
	return nil
}

func copyWSLStage(dst io.Writer, src io.Reader, remaining int64, idle time.Duration, cancel context.CancelCauseFunc) error {
	timer := time.AfterFunc(idle, func() { cancel(retryableWSLStageError{errors.New("upload WSL2 stage made no progress")}) })
	defer timer.Stop()
	buffer := make([]byte, 32<<10)
	for remaining > 0 {
		chunk := min(int64(len(buffer)), remaining)
		if _, err := io.ReadFull(src, buffer[:chunk]); err != nil {
			return err
		}
		written, err := dst.Write(buffer[:chunk])
		if err != nil {
			return err
		}
		if written != int(chunk) {
			return io.ErrShortWrite
		}
		remaining -= int64(written)
		timer.Reset(idle)
	}
	return nil
}
func retryWSLStageTransport(err error) error {
	if err == nil || !shouldRetrySSHPort(err) {
		return err
	}
	return retryableWSLStageError{err}
}
func wslStageLauncherCommand(nonce string, size int64, expected [sha256.Size]byte, shell wslStageShell) string {
	return wslStageFileCommand(nonce, nonce+".ready", size, expected, false, shell)
}

func wslStageFileCommand(nonce, name string, size int64, expected [sha256.Size]byte, discard bool, shell wslStageShell) string {
	if len(nonce) != 32 || strings.Trim(nonce, "0123456789abcdef") != "" || size < 0 || size > wslStageMaxSize {
		return ""
	}
	if name != nonce+".ready" && name != nonce+".part" && name != "."+nonce+".proof" {
		return ""
	}
	flag := "$false"
	if discard {
		flag = "$true"
	}
	return wslStagePowerShellCommand(strings.NewReplacer("@NONCE@", nonce, "@NAME@", name,
		"@SIZE@", fmt.Sprint(size), "@DIGEST@", base64.StdEncoding.EncodeToString(expected[:]),
		"@DISCARD@", flag).Replace(wslStageVerifier), shell)
}

// Preparation observes its actual parent shell and binds the result to the
// fresh route nonce before upload. An outer PowerShell must execute the script
// directly: a nested native invocation transforms stderr and collapses exit codes.
type wslStageShell string

const (
	wslStageCMD        wslStageShell = "cmd"
	wslStagePowerShell wslStageShell = "powershell"
)

func (s wslStageShell) valid() bool { return s == wslStageCMD || s == wslStagePowerShell }

func wslStagePowerShellCommand(script string, shell wslStageShell) string {
	if !shell.valid() {
		return ""
	}
	command := "& ([ScriptBlock]::Create([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('" + base64.StdEncoding.EncodeToString([]byte(script)) + "'))))"
	if shell == wslStageCMD {
		command = `powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "` + command + `"`
	}
	return command
}
