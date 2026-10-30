package cli

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
)

// Linux streaming commands need an explicit owned cancellation channel: their
// workspace registrar ignores HUP, so closing SSH alone cannot retire children.
// Control/sync calls keep their existing transport and witness semantics.
func runOwnedLinuxSSHCommand(ctx context.Context, target SSHTarget, remote string, stdout, stderr io.Writer) (err error) {
	owner := workspaceOwnerFromContext(ctx)
	nonce, err := randomHex(16)
	if err != nil {
		return err
	}
	directory := `"$HOME/.crabbox/workspace-owners/` + owner.key + `.command.` + nonce + `"`
	// A disconnected client stops renewing ownership. The supervisor can still
	// retire only its own guarded group after expiry, never a replacement owner.
	tick := `    if [ "$(sed -n '2p' "$HOME/.crabbox/workspace-owners/` + owner.key + `.owner" 2>/dev/null)" != ` + shellQuote(owner.token) + ` ]; then : >"$directory/.lost"; fi
    expiry=$(sed -n '3p' "$HOME/.crabbox/workspace-owners/` + owner.key + `.owner" 2>/dev/null)
    case $expiry in ''|*[!0-9]*) : >"$directory/.lost";; *) [ "$expiry" -gt "$(date +%s)" ] || : >"$directory/.lost";; esac
`
	helper := composeGuardedControl(strings.NewReplacer(
		"@FUNCTIONAL_PRELUDE@", "", "@FUNCTIONAL_CLEANUP@", "", "@FUNCTIONAL_CLEANUP_POLL@", "", "@FUNCTIONAL_SCRATCH@", "",
	).Replace(wslLinuxTemplate), guardedWorkloadScript("completion_grace_ms=0\n", tick, ".1"), "bash")
	marker := workspaceOwnerSetupPrefix + nonce
	command := owner.wrapPOSIXCommand(remote, false, marker)
	bootstrap := "exec /bin/sh -c " + shellQuote(wslHelperBootstrap) + " cbx " + strconv.Itoa(len(helper)) + " 0 "
	args := directory + " " + shellQuote(nonce)
	run := bootstrap + "run-finite " + args + " " + strconv.Itoa(len(command)) + " 0 " +
		strconv.FormatInt(wslStageIdleTimeout.Milliseconds(), 10) + " " + strconv.FormatInt(wsl2SignalGrace.Milliseconds(), 10)
	input := strings.NewReader(helper + command)
	transport, err := prepareSSHTransport(contextWithoutWorkspaceOwner(ctx), target, run, input, int64(input.Len()), sshCommandLimit{})
	if err != nil {
		return sshPreparationError{err}
	}
	transport.setupMarker = marker
	err = transport.execute(contextWithoutWorkspaceOwner(ctx), &target, sshCommandLimit{}, "10", "3", stdout, stderr)
	if err == nil {
		return nil
	}
	// Cleanup has an independent finite clock; caller cancellation cannot revoke
	// its own already-admitted process-group authority. The supervisor validates
	// nonce and live guard identity and proves group absence before deleting it.
	cleanupCtx, cancel := context.WithTimeout(contextWithoutWorkspaceOwner(context.WithoutCancel(ctx)), 2*wsl2SignalGrace+10*time.Second)
	defer cancel()
	cleanup := bootstrap + "cleanup " + args + " 0 0 0 0"
	cleanupErr := executePreparedSSH(cleanupCtx, &target, cleanup, strings.NewReader(helper), int64(len(helper)), sshCommandLimit{}, "2", "1", io.Discard, stderr)
	return errors.Join(err, cleanupErr)
}
