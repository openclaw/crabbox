package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var detachedJobID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// Job status is an observation, not an SSH exit code. Missing completion
// evidence is unknown, even when the recorded process no longer exists.
type detachedJobStatus struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	PID      int    `json:"pid,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
}

func (a App) detachedJob(ctx context.Context, operation string, args []string) (err error) {
	defaults := defaultConfig()
	fs := newFlagSet("job "+operation, a.Stderr)
	provider := registerProviderSelectionFlag(fs, defaults, providerHelpAll())
	id := fs.String("id", "", "existing lease id or slug (required; never provisions)")
	jobID := fs.String("job-id", "", "unique job identifier; do not change it after ambiguous submission")
	lines := fs.Int("lines", 100, "maximum log lines (1..10000)")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" || *lines < 1 || *lines > 10000 {
		return Exit(2, "job %s requires --id; --lines must be between 1 and 10000", operation)
	}
	transfer := operation == "push" || operation == "pull"
	if transfer {
		if fs.NArg() != 2 || isSandboxCopyArg(fs.Arg(0)) || !safeDetachedJobPath(fs.Arg(1)) {
			return Exit(2, "usage: crabbox job %s --id <lease> <local-path> <workspace-relative-path>", operation)
		}
	} else if !detachedJobID.MatchString(*jobID) || operation == "submit" && fs.NArg() == 0 || operation != "submit" && fs.NArg() != 0 {
		return Exit(2, "job %s requires --job-id (1..64 letters, digits, underscores or hyphens); submit also requires a command after --", operation)
	}
	cfg, err := loadPortsConfig(fs, *provider, providerFlags, targetFlags, *id)
	if err != nil {
		return err
	}
	lease, err := a.resolveSSHTransportLeaseTargetForRepo(ctx, &cfg, *id, true, false)
	if err != nil {
		return err
	}
	if lease.SSH.TargetOS != targetLinux {
		return Exit(2, "detached jobs require a native Linux SSH lease")
	}
	if err := a.claimAndTouchLeaseTarget(ctx, cfg, &lease.Server, lease.SSH, lease.LeaseID, false); err != nil {
		return err
	}
	// Dispatch must never be replayed on a fallback port after an ambiguous
	// connection failure. Resolve routing before starting any mutation.
	if err := a.probeSSHTransportLeaseAfterClaim(ctx, cfg, &lease, false); err != nil {
		return err
	}
	lease.SSH.FallbackPorts = []string{}
	key := workspaceOwnerKey(lease.LeaseID)
	if operation == "status" || operation == "logs" {
		controlCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		remote := detachedJobReadScript(key, *jobID, operation, *lines)
		if operation == "logs" {
			if err := runSSHInput(controlCtx, lease.SSH, remote, nil, a.Stdout, a.Stderr); err != nil {
				return Exit(7, "job logs unavailable; this is not a workload exit: %v", err)
			}
			return nil
		}
		output, err := runSSHOutput(controlCtx, lease.SSH, remote)
		if err != nil {
			return Exit(7, "job status unavailable; workload outcome remains unknown: %v", err)
		}
		status, err := parseDetachedJobStatus(*jobID, output)
		if err != nil {
			return Exit(7, "invalid job status; workload outcome remains unknown: %v", err)
		}
		return json.NewEncoder(a.Stdout).Encode(status)
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	workdir := remoteJoin(cfg, lease.LeaseID, repo.Name)
	owner, err := acquireWorkspaceOwner(ctx, lease.SSH, lease.LeaseID, a.Stderr)
	if err != nil {
		return err
	}
	handedOff := false
	defer func() {
		if handedOff {
			owner.stopRenewal()
			if owner.closeTransport != nil {
				err = errors.Join(err, owner.closeTransport())
			}
			return
		}
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), owner.quiesceTimeout())
		defer cancel()
		err = errors.Join(err, owner.Close(closeCtx))
	}()
	if transfer {
		// Refuse symlink traversal before passing a relative operand to cp.
		// The workspace owner excludes cooperating writers during this check.
		check := "set -eu\ncd " + shellQuote(workdir) + "\n"
		prefix := ""
		for _, part := range strings.Split(path.Clean(fs.Arg(1)), "/") {
			prefix = path.Join(prefix, part)
			check += "[ ! -L " + shellQuote(prefix) + " ]\n"
		}
		if err := runSSHQuiet(owner.Context(), lease.SSH, check); err != nil {
			return Exit(7, "workspace path check failed: %v", err)
		}
		// cp can outlive a lost connection. Keep a durable reservation on any
		// failure rather than allowing expiry to authorize another writer.
		handedOff = true
		response, err := owner.transport.Do(owner.Context(), workspaceOwnerRemoteRequest{Action: workspaceOwnerDetach, Key: owner.key, Token: owner.token})
		if err != nil || response != "DETACHED" {
			return Exit(7, "transfer ownership is uncertain: %v (%s)", err, response)
		}
		remote := "SANDBOX:" + path.Join(workdir, fs.Arg(1))
		src, dst := fs.Arg(0), remote
		if operation == "pull" {
			src, dst = remote, fs.Arg(0)
		}
		if err := copyOverResolvedSSH(owner.Context(), lease.SSH, src, dst, false, a.Stdout, a.Stderr); err != nil {
			return Exit(7, "transfer failed; workspace reservation retained for inspection: %v", err)
		}
		owner.stopRenewal()
		response, err = owner.transport.Do(ctx, workspaceOwnerRemoteRequest{Action: workspaceOwnerComplete, Key: owner.key, Token: owner.token})
		if err != nil || response != "RELEASED" {
			return Exit(7, "transfer completed but workspace release is uncertain: %v (%s)", err, response)
		}
		return nil
	}
	controlCtx, cancel := context.WithTimeout(owner.Context(), time.Minute)
	defer cancel()
	// Reserve the ID before handing over ownership. mkdir refuses resubmission,
	// including a previous dispatch whose acknowledgment was lost.
	prepare := detachedJobRoot(key, *jobID) + `
mkdir -p "$root"
mkdir -m 700 "$job" || exit 73
printf 'starting\n' >"$job/state"
`
	if err := runSSHQuiet(controlCtx, lease.SSH, prepare); err != nil {
		return Exit(7, "reserve job %s failed; inspect the same ID before retrying: %v", *jobID, err)
	}
	// From this point even an unknown transport outcome must retain the barrier.
	handedOff = true
	response, err := owner.transport.Do(controlCtx, workspaceOwnerRemoteRequest{Action: workspaceOwnerDetach, Key: owner.key, Token: owner.token})
	if err != nil || response != "DETACHED" {
		return Exit(7, "job %s ownership handoff is uncertain; do not resubmit: %v (%s)", *jobID, err, response)
	}
	owner.stopRenewal()
	remote := remoteCommandWithEnvFiles(workdir, nil, nil, fs.Args())
	launch := detachedJobLaunchScript(owner.key, owner.token, *jobID, remote)
	if err := runSSHQuiet(controlCtx, lease.SSH, launch); err != nil {
		return Exit(7, "job %s dispatch is uncertain; query this ID, do not resubmit: %v", *jobID, err)
	}
	return json.NewEncoder(a.Stdout).Encode(detachedJobStatus{ID: *jobID, State: "submitted"})
}

func safeDetachedJobPath(value string) bool {
	if value == "" || path.Clean(value) == "." || path.IsAbs(value) || strings.ContainsAny(value, "\\\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == ".git" || part == ".crabbox" {
			return false
		}
	}
	return true
}

func detachedJobRoot(key, id string) string {
	return "set -eu\numask 077\nroot=\"$HOME/.crabbox/jobs/" + key + "\"\njob=\"$root/" + id + "\"\n"
}

func detachedJobLaunchScript(key, token, id, command string) string {
	workload := detachedJobRoot(key, id) + `
identity=$(ps -o lstart= -p $$ | tr -s ' ' | sed 's/^ //;s/ $//')
[ -n "$identity" ]
printf '%s\n%s\n' "$$" "$identity" >"$job/pid.tmp"
mv "$job/pid.tmp" "$job/pid"
printf 'running\n' >"$job/state"
set +e
/bin/sh -c ` + shellQuote(command) + ` >"$job/log" 2>&1
code=$?
printf '%s\n' "$code" >"$job/exit.tmp" || exit 74
mv "$job/exit.tmp" "$job/exit" || exit 74
exit 0
`
	witness := remoteWorkspaceOwnerPOSIXWitnessScript(key, token, workload, "")
	complete := remoteWorkspaceOwnerPOSIX(workspaceOwnerRemoteRequest{Action: workspaceOwnerComplete, Key: key, Token: token})
	supervisor := detachedJobRoot(key, id) + "\n/bin/sh -c " + shellQuote(witness) + "\n/bin/sh -c " + shellQuote(complete) + ` >"$job/cleanup"
printf 'complete\n' >"$job/state"
`
	return detachedJobRoot(key, id) + "\nnohup /bin/sh -c " + shellQuote(supervisor) + ` >"$job/supervisor.log" 2>&1 </dev/null &
`
}

func detachedJobReadScript(key, id, operation string, lines int) string {
	script := detachedJobRoot(key, id) + "\n[ -d \"$job\" ] || { printf 'missing\\n'; exit 0; }\n"
	if operation == "logs" {
		return script + "[ ! -f \"$job/log\" ] || tail -n " + strconv.Itoa(lines) + " \"$job/log\"\n"
	}
	return script + workspaceOwnerPOSIXProcess + `
pid=0
if [ -f "$job/pid" ]; then
  pid=$(sed -n '1p' "$job/pid")
  identity=$(sed -n '2p' "$job/pid")
  case "$pid" in ''|*[!0-9]*) exit 74 ;; esac
fi
if [ -f "$job/exit" ]; then
  printf 'exited\n%s\n' "$pid"
  cat "$job/exit"
elif [ "$pid" -gt 0 ] && owner_process_status "$pid" "$identity"; then
  printf 'running\n%s\n' "$pid"
else
  printf 'unknown\n%s\n' "$pid"
fi
`
}

func parseDetachedJobStatus(id, output string) (detachedJobStatus, error) {
	fields := strings.Fields(output)
	status := detachedJobStatus{ID: id}
	if len(fields) == 1 && fields[0] == "missing" {
		status.State = "missing"
		return status, nil
	}
	if len(fields) < 2 {
		return status, fmt.Errorf("incomplete status")
	}
	pid, err := strconv.Atoi(fields[1])
	if err != nil || pid < 0 {
		return status, fmt.Errorf("invalid PID")
	}
	status.State, status.PID = fields[0], pid
	switch {
	case status.State == "exited" && len(fields) == 3:
		code, err := strconv.Atoi(fields[2])
		if err != nil || code < 0 || code > 255 {
			return status, fmt.Errorf("invalid exit status")
		}
		status.ExitCode = &code
	case (status.State == "running" || status.State == "unknown") && len(fields) == 2:
	default:
		return status, fmt.Errorf("invalid status record")
	}
	return status, nil
}
