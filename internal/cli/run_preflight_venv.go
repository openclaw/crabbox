package cli

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

//go:embed scripts/functional-preflight-completion.sh
var functionalPreflightCompletionScript string

//go:embed scripts/functional-preflight-posix.sh
var functionalPreflightPOSIXTemplate string

var runFunctionalPreflightControl = functionalPreflightControl

var runOwnedFunctionalPreflight = func(ctx context.Context, target SSHTarget, workdir string, env map[string]string, envFiles []string) (functionalPreflightCompletion, error) {
	if isWindowsNativeTarget(target) {
		return functionalPreflightCompletion{}, errors.New("functional preflight is unsupported on native Windows")
	}
	if isWindowsWSL2Target(target) {
		return runWSLFunctionalPreflight(ctx, target, workdir, env, envFiles)
	}
	return runPOSIXFunctionalPreflight(ctx, target, workdir, env, envFiles)
}

func functionalPreflightDiagnostic(ctx context.Context, completion functionalPreflightCompletion, err error) string {
	cleanup := "unconfirmed"
	if completion.WorkerQuiesced && completion.ScratchRemoved && completion.StageRetired {
		cleanup = "confirmed"
	}
	state := completion.State
	if err != nil || cleanup != "confirmed" {
		state = "unavailable"
	}
	if ctx.Err() != nil {
		state = "canceled"
	} else if state == "canceled" {
		state = "unavailable"
	}
	switch state {
	case "ready", "missing-python3", "venv-unavailable", "pip-unavailable", "worker-failed", "timed-out", "canceled":
	default:
		state = "unavailable"
	}
	return pythonVenvPreflightTool + "=" + state + " cleanup=" + cleanup
}

func functionalPreflightCleanupBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	// Staging has completed. The new command's transport preparation and native
	// startup are separate from worker execution and the shared cleanup reserve.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), functionalPreflightDispatchAllowance)
	stop := context.AfterFunc(ctx, func() {
		timer := time.NewTimer(pythonVenvPreflightCleanupTime)
		defer timer.Stop()
		select {
		case <-timer.C:
			cancel()
		case <-cleanupCtx.Done():
		}
	})
	return cleanupCtx, func() { stop(); cancel() }
}

func functionalWSLPreflightHelper(budget time.Duration) string {
	workload := strings.NewReplacer(
		"@WORKLOAD_INIT@", fmt.Sprintf("deadline=$((SECONDS + %d))\n", max(1, int64((budget+time.Second-1)/time.Second))),
		"@WORKLOAD_TICK@", "    if [ \"$SECONDS\" -ge \"$deadline\" ]; then : >\"$directory/.timed-out\"; break; fi\n",
	).Replace(strings.TrimSuffix(guardedWorkload, "\n"))
	completed := "    [ -f \"$directory/.completion\" ] && [ ! -e \"$directory/scratch\" ] && [ ! -L \"$directory/scratch\" ] && exit 0\n"
	return strings.NewReplacer(
		"@GUARDED_GROUP_FUNCTIONS@", strings.TrimSuffix(guardedGroupFunctions, "\n"),
		"@GUARDED_MEMBERS@", strings.TrimSuffix(guardedMembers, "\n"),
		"@GUARDED_WORKLOAD@", workload,
		"@FUNCTIONAL_PRELUDE@", functionalPreflightCompletionScript+"\n",
		"@FUNCTIONAL_CLEANUP@", completed,
		"@FUNCTIONAL_CLEANUP_POLL@", completed,
		"@FUNCTIONAL_SCRATCH@", "mkdir -m 700 -- \"$directory/scratch\" || exit 74\n",
	).Replace(wslLinuxTemplate)
}

func functionalPOSIXPreflightHelper(budget time.Duration) string {
	workload := strings.NewReplacer(
		"@WORKLOAD_INIT@", fmt.Sprintf("deadline=$((SECONDS + %d))\n", max(1, int64((budget+time.Second-1)/time.Second))),
		"@WORKLOAD_TICK@", "    if [ \"$interrupted\" = 1 ]; then : >\"$directory/.cancel\"; break; fi\n"+
			"    if [ \"$SECONDS\" -ge \"$deadline\" ]; then : >\"$directory/.timed-out\"; break; fi\n",
	).Replace(strings.TrimSuffix(guardedWorkload, "\n"))
	return strings.NewReplacer(
		"@GUARDED_GROUP_FUNCTIONS@", strings.TrimSuffix(guardedGroupFunctions, "\n"),
		"@GUARDED_MEMBERS@", strings.TrimSuffix(guardedMembers, "\n"),
		"@GUARDED_WORKLOAD@", workload,
		"@FUNCTIONAL_PRELUDE@", strings.TrimSuffix(functionalPreflightCompletionScript, "\n"),
	).Replace(functionalPreflightPOSIXTemplate)
}

func runPOSIXFunctionalPreflight(ctx context.Context, target SSHTarget, workdir string, env map[string]string, envFiles []string) (completion functionalPreflightCompletion, err error) {
	nonce, err := randomHex(16)
	if err != nil {
		return completion, err
	}
	command := remoteShellCommandWithEnvFiles(workdir, env, envFiles,
		pythonVenvPreflightWorker("/tmp/crabbox-command-"+nonce+"/scratch"))
	if len(command) > wslStageMaxCommand {
		return completion, errors.New("functional preflight command exceeds stage limit")
	}
	helper := functionalPOSIXPreflightHelper(pythonVenvPreflightExecutionTime)
	remote := "export CBX_HELPER=" + shellQuote(helper) + "; exec bash -c \"$CBX_HELPER\" sh run " +
		shellQuote("/tmp/crabbox-command-"+nonce) + " " + shellQuote(nonce) +
		fmt.Sprintf(" %d 0 %d %d", len(command), wslStageIdleTimeout.Milliseconds(), wsl2SignalGrace.Milliseconds())
	// Resolve a usable endpoint before starting the native operation's clock.
	// The nonce-bound operation itself is dispatched only once on that route.
	target.NoControlMaster = true
	prepareCtx, cancelPrepare := context.WithTimeout(ctx, sshTransportPreparationTimeout)
	defer cancelPrepare()
	if err := resolveSSHPortNoInput(prepareCtx, &target, "2", "1", io.Discard); err != nil {
		return completion, err
	}
	size := int64(len(command))
	prepared, err := prepareWorkspaceOwnerRemote(prepareCtx, target, remote, &size)
	if err != nil {
		return completion, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.close(ctx, target))
		}
	}()
	cleanupCtx, cancelCleanup := functionalPreflightCleanupBudget(ctx)
	defer cancelCleanup()
	// The helper owns the 90s worker deadline. Its transport wrapper must remain
	// available for supervised cleanup; collection uses this same remaining clock.
	deadline, _ := cleanupCtx.Deadline()
	execCtx, cancelExec := context.WithDeadline(ctx, deadline)
	defer cancelExec()
	transport := sshTransportPreparation{command: prepared.command, direct: strings.NewReader(command), setupMarker: prepared.setupMarker}
	_, runErr := transport.runOnce(execCtx, target, "2", "1", io.Discard, io.Discard, false)
	if runErr == nil {
		runErr = context.Cause(execCtx)
	}
	return finishFunctionalPreflight(ctx, cleanupCtx, target, nonce, runErr)
}

func runWSLFunctionalPreflight(ctx context.Context, target SSHTarget, workdir string, env map[string]string, envFiles []string) (completion functionalPreflightCompletion, err error) {
	nonce, err := randomHex(16)
	if err != nil {
		return completion, err
	}
	command := remoteShellCommandWithEnvFiles(workdir, env, envFiles,
		pythonVenvPreflightWorker("/tmp/crabbox-command-"+nonce+"/scratch"))
	prepared, err := prepareWorkspaceOwnerRemote(ctx, target, command, nil)
	if err != nil {
		return completion, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.close(ctx, target))
		}
	}()
	limit := sshCommandLimit{execution: functionalPreflightSupervisorAllowance}
	spool, err := newWSLStageSpoolWithHelper(prepared.command, nil, nil, 0, limit,
		functionalWSLPreflightHelper(pythonVenvPreflightExecutionTime))
	if err != nil {
		return completion, err
	}
	defer func() {
		if closeErr := spool.close(); closeErr != nil {
			completion.State = ""
			err = errors.Join(err, closeErr)
		}
	}()
	spool.functionalNonce, spool.setupMarker = nonce, prepared.setupMarker
	runErr := spool.run(ctx, &target, "2", "1", io.Discard, io.Discard)
	if spool.functionalCleanup == nil {
		// No native dispatch occurred; do not infer a worker completion or
		// acquire Linux stage cleanup authority from a staging failure.
		return completion, runErr
	}
	return finishFunctionalPreflight(ctx, spool.functionalCleanup, target, nonce, runErr)
}

func finishFunctionalPreflight(ctx, cleanupCtx context.Context, target SSHTarget, nonce string, runErr error) (functionalPreflightCompletion, error) {
	action := "observe"
	if runErr != nil || ctx.Err() != nil {
		action = "cancel"
	}
	record, controlErr := runFunctionalPreflightControl(cleanupCtx, target, nonce, action)
	completion, parseErr := parseFunctionalPreflightCompletion(record, nonce)
	if controlErr != nil || parseErr != nil {
		return functionalPreflightCompletion{}, errors.Join(runErr, context.Cause(ctx), controlErr, parseErr)
	}
	if _, err := runFunctionalPreflightControl(cleanupCtx, target, nonce, "retire"); err != nil {
		completion.State = ""
		return completion, errors.Join(runErr, context.Cause(ctx), err)
	}
	completion.StageRetired = true
	if ctx.Err() != nil {
		completion.State = ""
		return completion, errors.Join(runErr, context.Cause(ctx))
	}
	if completion.State == "canceled" {
		completion.State = ""
		if runErr != nil {
			return completion, runErr
		}
		return completion, errors.New("functional preflight owner canceled before completion")
	}
	if runErr != nil {
		// An expected remote process exit may be diagnostic-only. A joined
		// transport or envelope-cleanup failure must retain its own outcome.
		processOutcome := runErr
		for {
			joined, ok := processOutcome.(interface{ Unwrap() []error })
			if !ok || len(joined.Unwrap()) != 1 {
				break
			}
			processOutcome = joined.Unwrap()[0]
		}
		if _, ok := processOutcome.(*exec.ExitError); !ok {
			completion.State = ""
			return completion, runErr
		}
		code := exitCode(processOutcome)
		expected := completion.State == "missing-python3" && code == pythonVenvMissingInterpreter ||
			completion.State == "venv-unavailable" && code == pythonVenvUnavailable ||
			completion.State == "pip-unavailable" && code == pythonVenvPipUnavailable ||
			completion.State == "timed-out" && code == 74 ||
			completion.State == "worker-failed" && code > 0 && code != 255
		if !expected {
			completion.State = ""
			return completion, runErr
		}
	}
	return completion, nil
}

// These controls only address the caller's nonce-bound stage. They deliberately
// use the transport's control boundary rather than register a second workload
// with the workspace owner while the original supervisor is still cleaning up.
func functionalPreflightControl(ctx context.Context, target SSHTarget, nonce, action string) ([]byte, error) {
	command, err := functionalPreflightControlCommand(nonce, action)
	if err != nil {
		return nil, err
	}
	out := newSynchronizedBuffer(functionalPreflightCompletionLimit + 1)
	err = executePreparedSSH(ctx, &target, "bash -c "+shellQuote(command), nil, 0,
		sshCommandLimit{execution: 20 * time.Second}, "2", "1", &out, io.Discard)
	if err != nil {
		return nil, errors.Join(errors.New("functional preflight cleanup unconfirmed"), err)
	}
	return []byte(out.String()), nil
}

func functionalPreflightControlCommand(nonce, action string) (string, error) {
	if len(nonce) != 32 || strings.Trim(nonce, "0123456789abcdef") != "" {
		return "", errors.New("invalid functional preflight stage identity")
	}
	if action != "observe" && action != "cancel" && action != "retire" {
		return "", errors.New("invalid functional preflight stage control")
	}
	return "nonce=" + shellQuote(nonce) + "\naction=" + shellQuote(action) + "\n" + `set -u
umask 077
directory=/tmp/crabbox-command-$nonce
[ -d "$directory" ] && [ ! -L "$directory" ] && [ -O "$directory" ] || exit 74
[ "$(cat "$directory/.nonce" 2>/dev/null)" = "$nonce" ] || exit 74
if [ "$action" = cancel ] && [ ! -e "$directory/.completion" ]; then
    : >"$directory/.cancel" || exit 74
fi
for ((i=0; i<150; i++)); do
    [ -e "$directory/.completion" ] && break
    sleep .1
done
[ -f "$directory/.completion" ] && [ ! -L "$directory/.completion" ] || exit 74
[ "$(wc -c <"$directory/.completion")" -le 256 ] || exit 74
if ! {
    IFS= read -r protocol && IFS= read -r recorded_nonce && IFS= read -r state &&
    IFS= read -r quiesced && IFS= read -r removed && IFS= read -r complete &&
    ! IFS= read -r extra && [ -z "$extra" ]
} <"$directory/.completion"; then exit 74; fi
[ "$protocol" = CBX-PREFLIGHT-1 ] && [ "$recorded_nonce" = "$nonce" ] &&
    [ "$quiesced" = worker-quiesced ] && [ "$removed" = scratch-removed ] &&
    [ "$complete" = complete ] || exit 74
case $state in
    ready|missing-python3|venv-unavailable|pip-unavailable|worker-failed|timed-out|canceled) ;;
    *) exit 74;;
esac
if [ "$action" = retire ]; then
    [ ! -e "$directory/scratch" ] && [ ! -L "$directory/scratch" ] || exit 74
    # Retirement follows the caller's successful completion validation. The
    # owner has published atomically and performs no subsequent stage writes.
    rm -rf -- "$directory" || exit 74
    [ ! -e "$directory" ] && [ ! -L "$directory" ] || exit 74
else
    cat "$directory/.completion"
fi
`, nil
}

const (
	pythonVenvPreflightTool                = "python3-venv"
	pythonVenvPreflightExecutionTime       = 90 * time.Second
	pythonVenvPreflightCleanupTime         = 30 * time.Second
	functionalPreflightSupervisorAllowance = wslStageIdleTimeout + pythonVenvPreflightExecutionTime + 2*wsl2SignalGrace + wslStageCompletionMargin
	functionalPreflightDispatchAllowance   = sshTransportPreparationTimeout + wslStageIdleTimeout + pythonVenvPreflightExecutionTime + pythonVenvPreflightCleanupTime
	pythonVenvMissingInterpreter           = 20
	pythonVenvUnavailable                  = 21
	pythonVenvPipUnavailable               = 22
	functionalPreflightCompletionLimit     = 256
)

type functionalPreflightCompletion struct {
	State          string
	WorkerQuiesced bool
	ScratchRemoved bool
	StageRetired   bool
}

// Only the stage owner may publish this record, after its children are reaped
// and scratch is removed. Worker output is never accepted as an acknowledgement.
func parseFunctionalPreflightCompletion(record []byte, nonce string) (functionalPreflightCompletion, error) {
	var empty functionalPreflightCompletion
	unconfirmed := errors.New("functional preflight cleanup unconfirmed")
	if len(nonce) != 32 || strings.Trim(nonce, "0123456789abcdef") != "" || len(record) > functionalPreflightCompletionLimit {
		return empty, unconfirmed
	}
	fields := strings.Split(string(record), "\n")
	if len(fields) != 7 || fields[0] != "CBX-PREFLIGHT-1" || fields[1] != nonce ||
		fields[3] != "worker-quiesced" || fields[4] != "scratch-removed" || fields[5] != "complete" || fields[6] != "" {
		return empty, unconfirmed
	}
	switch fields[2] {
	case "ready", "missing-python3", "venv-unavailable", "pip-unavailable", "worker-failed", "timed-out", "canceled":
		return functionalPreflightCompletion{State: fields[2], WorkerQuiesced: true, ScratchRemoved: true}, nil
	default:
		return empty, unconfirmed
	}
}

// The stage owns scratch and cleanup. The worker reports capability only;
// its exit status never establishes quiescence or successful removal.
func pythonVenvPreflightWorker(scratch string) string {
	return "if ! command -v python3 >/dev/null 2>&1; then exit 20; fi\n" +
		"TMPDIR=" + shellQuote(scratch) + " TMP=" + shellQuote(scratch) + " TEMP=" + shellQuote(scratch) +
		" python3 -I -B -c " + shellQuote(pythonVenvPreflightScript) + " " + shellQuote(scratch) + " >/dev/null 2>&1"
}

const pythonVenvPreflightScript = `import os
import subprocess
import sys

try:
    import venv
except ImportError:
    sys.exit(21)

environment = os.path.join(sys.argv[1], "venv")
try:
    venv.EnvBuilder(with_pip=False).create(environment)
except Exception:
    sys.exit(21)

python = os.path.join(environment, "bin", "python")
def invoke(arguments):
    try:
        return subprocess.run(
            [python, "-I", "-B"] + arguments,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        ).returncode == 0
    except OSError:
        return False

if not invoke(["-c", "import sys; sys.exit(sys.prefix == sys.base_prefix)"]):
    sys.exit(21)
if not invoke(["-m", "ensurepip", "--default-pip"]):
    sys.exit(22)
if not invoke(["-m", "pip", "--version"]):
    sys.exit(22)
`
