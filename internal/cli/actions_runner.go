package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ActionsRunnerBinding records ownership, not active status. Keep it after
// unregister so a consumed runner's guest configuration can be reconciled.
// GitHub is authoritative for whether the exact registration still exists.
type ActionsRunnerBinding struct {
	Scope            string `json:"scope"`
	RunnerID         int64  `json:"runnerID"`
	Name             string `json:"name"`
	Group            string `json:"group,omitempty"`
	Ephemeral        bool   `json:"ephemeral"`
	LeaseFingerprint string `json:"leaseFingerprint"`
}

type actionsRunnerSettings struct {
	ID        int64  `json:"agentId"`
	Name      string `json:"agentName"`
	URL       string `json:"gitHubUrl"`
	Group     string `json:"poolName"`
	Ephemeral bool   `json:"ephemeral"`
}

type actionsRunnerGuest struct {
	Settings *actionsRunnerSettings `json:"settings"`
	Active   *bool                  `json:"active"`
	Version  string                 `json:"version"`
}

var actionsRunnerScopePattern = regexp.MustCompile(`^(repos/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+|orgs/[A-Za-z0-9-]+)$`)

func actionsRunnerScope(repo Repo, repository, org, group string, explicitRepo bool) (string, error) {
	if org != "" {
		if explicitRepo || !actionsRunnerScopePattern.MatchString("orgs/"+org) {
			return "", Exit(2, "actions register requires either --repo owner/name or --org organization")
		}
		return "orgs/" + org, nil
	}
	if group != "" {
		return "", Exit(2, "--runner-group requires --org")
	}
	selected, err := resolveGitHubRepo(repo, repository)
	if err != nil {
		return "", err
	}
	scope := "repos/" + selected.Slug()
	if !actionsRunnerScopePattern.MatchString(scope) {
		return "", Exit(2, "invalid GitHub runner repository")
	}
	return scope, nil
}

func actionsRunnerURL(scope string) string {
	_, name, _ := strings.Cut(scope, "/")
	return "https://github.com/" + name
}

func actionsRunnerLeaseFingerprint(claim leaseClaim) string {
	// Match the existing claim's stable workspace owner, excluding observations,
	// timestamps and revisions. Endpoint refresh must not erase cleanup custody.
	data, _ := json.Marshal([]any{claim.LeaseID, claim.RepoRoot, claim.Provider, claim.ProviderScope,
		claim.CloudID, claim.CloudNumericID, claim.CloudImmutableID, claim.StaticHost, claim.StaticUser,
		claim.StaticPort, claim.StaticWorkRoot, claim.TargetOS, claim.WindowsMode})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (a App) registerActionsRunner(ctx context.Context, cfg Config, target SSHTarget, leaseID, slug, scope, group, name string, extraLabels []string, expected leaseClaim, strictSnapshot bool, receiptPath string) (leaseClaim, error) {
	if !supportsGitHubActionsRunnerTarget(target) {
		return leaseClaim{}, Exit(2, "actions runner registration currently supports Linux and Windows targets only")
	}
	if !actionsRunnerScopePattern.MatchString(scope) {
		return leaseClaim{}, Exit(2, "invalid GitHub runner scope")
	}
	if name == "" {
		name = LeaseProviderName(leaseID, slug)
	}
	labels := githubActionsRunnerLabels(cfg, leaseID, slug, extraLabels)
	checkVersion := cfg.Actions.RunnerVersion != "" && cfg.Actions.RunnerVersion != "latest"
	// Reserve part of the caller's deadline so a stalled setup still reports
	// guest diagnostics instead of a bare timeout.
	bootstrapCtx, cancelBootstrap := actionsRunnerBootstrapContext(ctx)
	defer cancelBootstrap()
	var updated leaseClaim
	// Registrations serialize on their own per-lease lock. The claim lock is held
	// only for claim checks, the receipt write and listener start, never across
	// the guest install, so lease renewals keep committing while setup runs.
	err := withActionsRunnerRegistrationLock(ctx, leaseID, func() error {
		var reserved *leaseClaim
		var fingerprint string
		err := WithDurableLeaseClaimLockContext(ctx, leaseID, func(claim *leaseClaim, exists bool, persist func() error) error {
			// A lease heartbeat may advance lifecycle labels and last use between the
			// caller's snapshot and this lock; any other change fails the snapshot.
			if !exists || !sameActionsWorkspaceClaimOwner(*claim, expected) || strictSnapshot && !reflect.DeepEqual(*claim, expected) && !actionsClaimLifecycleOnlyChange(expected, *claim) {
				return Exit(2, "Actions runner lease claim changed or is missing")
			}
			// Both activation paths below start a listener; a retiring lease must not gain one.
			if actionsRunnerClaimRetiring(*claim) {
				return Exit(2, "Actions runner lease is %s; refusing to activate a runner", strings.TrimSpace(claim.Labels["state"]))
			}
			fingerprint = actionsRunnerLeaseFingerprint(*claim)
			binding := claim.ActionsRunner
			guest, err := readActionsRunnerGuest(ctx, target, checkVersion)
			if err != nil {
				return err
			}
			if guest.Settings != nil {
				settings := guest.Settings
				if binding == nil {
					// Adopt shipped claims only through the exact remote configuration and
					// the canonical lease label returned by GitHub, never a name sweep.
					if settings.URL != actionsRunnerURL(scope) || settings.Name != name || settings.Ephemeral != cfg.Actions.Ephemeral || group != "" && settings.Group != group {
						return Exit(2, "existing guest runner does not match the requested registration")
					}
					binding = &ActionsRunnerBinding{Scope: scope, RunnerID: settings.ID, Name: settings.Name, Group: settings.Group, Ephemeral: settings.Ephemeral, LeaseFingerprint: fingerprint}
				} else if !actionsRunnerSettingsMatch(settings, binding) {
					return Exit(2, "guest runner does not match the recorded registration; refusing replacement")
				}
			} else if *guest.Active {
				return Exit(2, "runner listener is active without an identifiable registration; reconcile it before registering")
			}
			if binding != nil {
				present, err := githubActionsRunnerPresent(ctx, *binding, leaseID, labels, target.ChildEnvDenylist)
				if err != nil {
					return err
				}
				if present {
					if checkVersion && guest.Version != cfg.Actions.RunnerVersion {
						return Exit(2, "existing runner does not have requested version %s; unregister it before changing runner version", cfg.Actions.RunnerVersion)
					}
					if binding.LeaseFingerprint != fingerprint || guest.Settings == nil || binding.Scope != scope || binding.Name != name || binding.Ephemeral != cfg.Actions.Ephemeral || group != "" && binding.Group != group {
						return Exit(2, "owned runner registration is still present; use actions unregister before changing its configuration")
					}
					claim.ActionsRunner = binding
					if err := persist(); err != nil {
						return err
					}
					updated = cloneLeaseClaim(*claim)
					if err := writeActionsRunnerReceipt(receiptPath, leaseID, *binding); err != nil {
						return err
					}
					if !*guest.Active {
						fmt.Fprintln(a.Stderr, "actions runner setup stage=service-start")
						_, err := actionsRunnerControl(ctx, target, githubActionsRunnerStartScript(target))
						return err
					}
					return nil
				}
				if *guest.Active {
					return Exit(2, "owned runner listener is still active after registration removal; wait for it to stop before registering again")
				}
			}
			previous := ActionsRunnerBinding{Name: name}
			if binding != nil {
				previous = *binding
			}
			if _, err := actionsRunnerControl(ctx, target, remoteClearActionsRunnerSettings(target, previous)); err != nil {
				return err
			}
			snapshot := cloneLeaseClaim(*claim)
			reserved = &snapshot
			return nil
		})
		if err != nil || reserved == nil {
			return err
		}
		fmt.Fprintln(a.Stderr, "actions runner setup stage=registration-token")
		token, err := githubActionsRegistrationToken(bootstrapCtx, scope, target.ChildEnvDenylist)
		if err != nil {
			if bootstrapCtx.Err() == context.DeadlineExceeded {
				return Exit(5, "GitHub Actions runner setup timed out before registration token acquisition completed")
			}
			return err
		}
		script := githubActionsRunnerInstallScriptForTarget(cfg.Actions.RunnerVersion, cfg.Actions.Ephemeral, target)
		if isWindowsNativeTarget(target) {
			script = "$env:RUNNER_GROUP = " + psQuote(group) + "\n" + script
		} else {
			script = "export RUNNER_GROUP=" + shellQuote(group) + "\n" + script
		}
		_, destination, _ := strings.Cut(scope, "/")
		remote := githubActionsRunnerInstallRemoteCommand(target)
		input := githubActionsRunnerInstallInput(destination, name, strings.Join(labels, ","), token, script)
		// Windows OpenSSH intermittently wedges its per-connection event loop while
		// forwarding a large stdin payload, stranding setup before its first stage.
		// Copy the script instead; only the short credential lines use stdin.
		cleanupScript := func() {}
		if isWindowsNativeTarget(target) && windowsScriptNeedsFileTransport(script) {
			remotePath, cleanup, err := uploadWindowsActionsRunnerScript(bootstrapCtx, target, script)
			if err != nil {
				return err
			}
			cleanupScript = cleanup
			remote = githubActionsRunnerInstallRemoteCommandForUploadedScript(remotePath)
			input = githubActionsRunnerCredentialInput(destination, name, strings.Join(labels, ","), token)
		}
		setup, installErr := runActionsRunnerSetup(bootstrapCtx, a.Stderr, func(stdout, stderr io.Writer) error {
			return runSSHInput(bootstrapCtx, target, remote, strings.NewReader(input), stdout, stderr)
		})
		cleanupScript()
		if installErr != nil {
			diagnostics := redactActionsRunnerText(setup.Diagnostics, token)
			if diagnostics == "" {
				diagnostics = readGitHubActionsRunnerDiagnosticsBestEffort(ctx, target, token)
			}
			if bootstrapCtx.Err() == context.DeadlineExceeded {
				installErr = Exit(5, "GitHub Actions runner setup timed out on %s during stage=%s: %s", target.Host, blank(setup.LastStage, "bootstrap"), blank(diagnostics, "no diagnostics available"))
			} else {
				installErr = Exit(7, "register GitHub Actions runner on %s failed during stage=%s: %v: %s", target.Host, blank(setup.LastStage, "bootstrap"), installErr, blank(diagnostics, "no diagnostics available"))
			}
		}
		// A lost SSH acknowledgement is not permission to register again. Recover
		// the official local identity and verify its exact API object first. This
		// runs even after cancellation (for example by the hydration heartbeat after
		// a reassignment): configuration may already have created the GitHub runner,
		// and cleanup needs its exact identity. It never starts the listener.
		reconcileCtx, cancelReconcile := context.WithTimeout(context.WithoutCancel(ctx), actionsRunnerReconcileTimeout)
		defer cancelReconcile()
		var canceled error
		if ctx.Err() != nil {
			canceled = context.Cause(ctx)
		}
		// A Windows guest that has just installed Git and configured the runner
		// can miss one control call's deadline; keep asking within a budget that
		// still leaves time for the GitHub check and the claim update below.
		readCtx, cancelRead := context.WithTimeout(reconcileCtx, actionsRunnerReconcileReadBudget)
		guest, err := readActionsRunnerGuestUntil(readCtx, actionsRunnerReconcileRetryDelay, func(queryCtx context.Context) (string, error) {
			return actionsRunnerControl(queryCtx, target, remoteReadActionsRunnerGuest(target, checkVersion))
		})
		cancelRead()
		if ctx.Err() != nil {
			canceled = context.Cause(ctx)
		}
		if err != nil {
			return errors.Join(installErr, canceled, Exit(ExitCodeForError(err, 7), "read runner identity after setup; listener was not started; a runner named %s may remain registered at %s: %v", name, actionsRunnerURL(scope), err))
		}
		if guest.Settings == nil || *guest.Active || guest.Settings.URL != actionsRunnerURL(scope) || guest.Settings.Name != name || guest.Settings.Ephemeral != cfg.Actions.Ephemeral || group != "" && guest.Settings.Group != group {
			return errors.Join(installErr, canceled, Exit(7, "runner configuration identity is unconfirmed; listener was not started; reconcile this lease before retrying"))
		}
		if checkVersion && guest.Version != cfg.Actions.RunnerVersion {
			return errors.Join(installErr, canceled, Exit(7, "configured runner version is unconfirmed; listener was not started"))
		}
		binding := &ActionsRunnerBinding{Scope: scope, RunnerID: guest.Settings.ID, Name: name, Group: guest.Settings.Group, Ephemeral: cfg.Actions.Ephemeral, LeaseFingerprint: fingerprint}
		present, err := githubActionsRunnerPresent(reconcileCtx, *binding, leaseID, labels, target.ChildEnvDenylist)
		if ctx.Err() != nil {
			canceled = context.Cause(ctx)
		}
		if err != nil {
			return errors.Join(installErr, canceled, err)
		}
		if !present {
			return errors.Join(installErr, canceled, Exit(7, "configured runner %d is absent from GitHub; listener was not started", binding.RunnerID))
		}
		// Recheck the reservation before recording the runner. If the lease still
		// authorizes it, record the receipt so cleanup keeps custody, even when the
		// operation was canceled; start the listener only when it was not. Anything
		// beyond a lease renewal while setup ran unlocked means this lease no longer
		// owns the runner, so delete the exact configured ID instead.
		persisted := false
		err = WithDurableLeaseClaimLockContext(reconcileCtx, leaseID, func(claim *leaseClaim, exists bool, persist func() error) error {
			if !actionsRunnerReservationHolds(*reserved, *claim, exists, strictSnapshot) {
				return Exit(2, "Actions runner lease claim changed during runner setup")
			}
			claim.ActionsRunner = binding
			if err := persist(); err != nil {
				return err
			}
			persisted = true
			updated = cloneLeaseClaim(*claim)
			if err := writeActionsRunnerReceipt(receiptPath, leaseID, *binding); err != nil {
				return err
			}
			if ctx.Err() != nil {
				return errors.Join(installErr, context.Cause(ctx))
			}
			fmt.Fprintln(a.Stderr, "actions runner setup stage=service-start")
			_, err := actionsRunnerControl(ctx, target, githubActionsRunnerStartScript(target))
			return err
		})
		if ctx.Err() != nil {
			canceled = context.Cause(ctx)
		}
		if err != nil && !persisted {
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			rollbackErr := deleteGitHubActionsRunner(rollbackCtx, *binding, leaseID, target.ChildEnvDenylist)
			return errors.Join(canceled, Exit(ExitCodeForError(err, 7), "persist runner %s/%d; listener was not started: %v", binding.Scope, binding.RunnerID, err), rollbackErr)
		}
		return err
	})
	if err != nil {
		return updated, err
	}
	fmt.Fprintf(a.Stdout, "actions runner registered scope=%s id=%d name=%s labels=%s ephemeral=%t\n", scope, updated.ActionsRunner.RunnerID, name, strings.Join(labels, ","), cfg.Actions.Ephemeral)
	// A configured runner whose listener never connects (for example one started
	// under an SSH session that ended) would leave the dispatched job queued.
	binding := *updated.ActionsRunner
	onlineCtx, cancelOnline := boundedActionsRunnerOnlineContext(bootstrapCtx)
	defer cancelOnline()
	if err := waitForGitHubActionsRunnerOnline(onlineCtx, scope, binding.Name, 5*time.Second, func(lookupCtx context.Context) (githubActionsRunnerState, error) {
		return githubActionsRunnerStateForBinding(lookupCtx, binding, target.ChildEnvDenylist)
	}, a.Stderr); err != nil {
		diagnostics := readGitHubActionsRunnerDiagnosticsBestEffort(ctx, target)
		return updated, Exit(ExitCodeForError(err, 5), "%v; guest diagnostics: %s", err, blank(diagnostics, "no diagnostics available"))
	}
	return updated, nil
}

func actionsRunnerSettingsMatch(settings *actionsRunnerSettings, binding *ActionsRunnerBinding) bool {
	return settings.ID == binding.RunnerID && settings.Name == binding.Name && settings.URL == actionsRunnerURL(binding.Scope) && settings.Group == binding.Group && settings.Ephemeral == binding.Ephemeral
}

// withActionsRunnerRegistrationLock serializes registrations for one lease
// without holding the claim lock that lease renewals need.
func withActionsRunnerRegistrationLock(ctx context.Context, leaseID string, action func() error) error {
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		return err
	}
	return withLeaseClaimLockContext(ctx, path+".actions-runner", false, action)
}

// actionsRunnerReservationHolds reports whether the claim still authorizes the
// registration reserved before the unlocked install: same workspace owner, no
// competing runner receipt, not retiring and, for strict callers, only
// lease-renewal changes.
func actionsRunnerReservationHolds(reserved, current leaseClaim, exists, strictSnapshot bool) bool {
	if !exists || !sameActionsWorkspaceClaimOwner(current, reserved) || !reflect.DeepEqual(current.ActionsRunner, reserved.ActionsRunner) || actionsRunnerClaimRetiring(current) {
		return false
	}
	return !strictSnapshot || reflect.DeepEqual(current, reserved) || actionsClaimLifecycleOnlyChange(reserved, current)
}

// actionsRunnerClaimRetiring reports a lifecycle state in which the lease must
// not gain an active runner. Cleanup writers change only the state label, which
// a heartbeat may also change, so check it explicitly.
func actionsRunnerClaimRetiring(claim leaseClaim) bool {
	state := strings.TrimSpace(claim.Labels["state"])
	return strings.EqualFold(state, "cleanup") || claimEndpointInactiveState(state)
}

func githubActionsRunnerPresent(ctx context.Context, binding ActionsRunnerBinding, leaseID string, labels, denylist []string) (bool, error) {
	if binding.RunnerID <= 0 || !actionsRunnerScopePattern.MatchString(binding.Scope) {
		return false, Exit(2, "invalid Actions runner receipt")
	}
	endpoint := fmt.Sprintf("%s/actions/runners/%d", binding.Scope, binding.RunnerID)
	out, err := ghOutputWithChildEnvironment(ctx, "", denylist, "api", endpoint)
	if err != nil {
		if !strings.Contains(err.Error(), "HTTP 404") {
			return false, err
		}
		// GitHub masks denied scopes as 404. Require current runner-read access
		// before treating an exact missing ID as an already-consumed registration.
		count, accessErr := ghOutputWithChildEnvironment(ctx, "", denylist, "api", binding.Scope+"/actions/runners?per_page=1", "--jq", ".total_count")
		if accessErr != nil {
			return false, accessErr
		}
		if n, parseErr := strconv.ParseInt(strings.TrimSpace(count), 10, 64); parseErr != nil || n < 0 {
			return false, Exit(3, "invalid GitHub runner scope response")
		}
		return false, nil
	}
	var runner struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Labels []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"labels"`
	}
	if err := json.Unmarshal([]byte(out), &runner); err != nil {
		return false, Exit(3, "invalid GitHub runner response")
	}
	owned := false
	for _, label := range runner.Labels {
		if label.Name == "crabbox-"+sanitizeGitHubRunnerLabel(leaseID) {
			owned = true
		}
	}
	if runner.ID != binding.RunnerID || runner.Name != binding.Name || !owned {
		return false, Exit(2, "GitHub runner does not match the owned lease registration")
	}
	if labels != nil {
		custom := make(map[string]bool)
		for _, label := range runner.Labels {
			if label.Type == "custom" {
				custom[strings.ToLower(label.Name)] = true
			}
		}
		wanted := make(map[string]bool, len(labels))
		for _, label := range labels {
			wanted[strings.ToLower(label)] = true
		}
		if !reflect.DeepEqual(custom, wanted) {
			return false, Exit(2, "owned runner labels differ; unregister it before changing registration labels")
		}
	}
	return true, nil
}

func deleteGitHubActionsRunner(ctx context.Context, binding ActionsRunnerBinding, leaseID string, denylist []string) error {
	present, err := githubActionsRunnerPresent(ctx, binding, leaseID, nil, denylist)
	if err != nil || !present {
		return err
	}
	_, err = ghOutputWithChildEnvironment(ctx, "", denylist, "api", "-X", "DELETE", fmt.Sprintf("%s/actions/runners/%d", binding.Scope, binding.RunnerID))
	return err
}

func (a App) actionsUnregister(ctx context.Context, args []string) error {
	fs := newFlagSet("actions unregister", a.Stderr)
	id := fs.String("id", "", "lease id or slug with a recorded runner registration")
	receiptPath := fs.String("receipt", "", "portable cleanup receipt written by actions register")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if (*id == "") == (*receiptPath == "") {
		return Exit(2, "actions unregister requires either --id or --receipt")
	}
	if *receiptPath != "" {
		receipt, err := readActionsRunnerReceipt(*receiptPath)
		if err != nil {
			return err
		}
		return WithDurableLeaseClaimLockContext(ctx, receipt.LeaseID, func(claim *leaseClaim, exists bool, _ func() error) error {
			if exists && (claim.ActionsRunner == nil || receipt.Scope != claim.ActionsRunner.Scope || receipt.RunnerID != claim.ActionsRunner.RunnerID || receipt.Name != claim.ActionsRunner.Name) {
				return Exit(2, "portable runner receipt conflicts with local cleanup ownership")
			}
			binding := ActionsRunnerBinding{Scope: receipt.Scope, RunnerID: receipt.RunnerID, Name: receipt.Name}
			if err := deleteGitHubActionsRunner(ctx, binding, receipt.LeaseID, nil); err != nil {
				return err
			}
			fmt.Fprintf(a.Stdout, "actions runner unregistered scope=%s id=%d\n", binding.Scope, binding.RunnerID)
			return nil
		})
	}
	claim, exists, err := ResolveLeaseClaimContext(ctx, *id)
	if err != nil {
		return err
	}
	if !exists || claim.ActionsRunner == nil {
		return Exit(2, "lease has no recorded Actions runner; register the existing runner to establish ownership before unregistering")
	}
	return WithDurableLeaseClaimLockContext(ctx, claim.LeaseID, func(current *leaseClaim, exists bool, _ func() error) error {
		if !exists || !reflect.DeepEqual(current.ActionsRunner, claim.ActionsRunner) {
			return Exit(2, "Actions runner claim changed")
		}
		if err := deleteGitHubActionsRunner(ctx, *current.ActionsRunner, current.LeaseID, nil); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "actions runner unregistered scope=%s id=%d\n", current.ActionsRunner.Scope, current.ActionsRunner.RunnerID)
		return nil
	})
}

func actionsRunnerControl(ctx context.Context, target SSHTarget, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return runSSHOutputBoundedWithOptions(ctx, target, command, 64*1024, sshCommandLimit{execution: 15 * time.Second, control: target.TargetOS == targetLinux}, "10", "1")
}

func readActionsRunnerGuest(ctx context.Context, target SSHTarget, checkVersion bool) (actionsRunnerGuest, error) {
	out, err := actionsRunnerControl(ctx, target, remoteReadActionsRunnerGuest(target, checkVersion))
	if err != nil {
		return actionsRunnerGuest{}, Exit(7, "inspect existing runner registration: %v", err)
	}
	return parseActionsRunnerGuest(out)
}

// readActionsRunnerGuestUntil repeats the read-only guest query until it
// answers or ctx ends. Only transport failures are retried; an answer that is
// not a valid identity is returned as is.
func readActionsRunnerGuestUntil(ctx context.Context, delay time.Duration, query func(context.Context) (string, error)) (actionsRunnerGuest, error) {
	for {
		out, err := query(ctx)
		if err == nil {
			return parseActionsRunnerGuest(out)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return actionsRunnerGuest{}, Exit(7, "inspect existing runner registration: %v", err)
		case <-timer.C:
		}
	}
}

func parseActionsRunnerGuest(out string) (actionsRunnerGuest, error) {
	var guest actionsRunnerGuest
	if err := json.Unmarshal([]byte(out), &guest); err != nil {
		return guest, Exit(7, "invalid guest runner identity")
	}
	if guest.Active == nil {
		return guest, Exit(7, "guest runner listener state is unknown")
	}
	if guest.Settings != nil && (guest.Settings.ID <= 0 || guest.Settings.Name == "" || guest.Settings.URL == "") {
		return guest, Exit(7, "incomplete guest runner identity")
	}
	return guest, nil
}

func remoteReadActionsRunnerGuest(target SSHTarget, checkVersion bool) string {
	versionRead := ""
	if isWindowsNativeTarget(target) {
		if checkVersion {
			versionRead = `try {
  $version = (& $listener --version | Out-String).Trim()
  if ($LASTEXITCODE -ne 0) { $version = "" }
} catch { $version = "" }`
		}

		return PowershellCommand(`$ErrorActionPreference = "Stop"
$dir = Join-Path $HOME "actions-runner"
$settings = $null
$version = ""
if (Test-Path -LiteralPath (Join-Path $dir ".runner")) { $settings = Get-Content -Raw -LiteralPath (Join-Path $dir ".runner") | ConvertFrom-Json }
$script = Join-Path $dir "run-crabbox.ps1"
$listener = Join-Path $dir "bin\Runner.Listener.exe"
$active = @(Get-CimInstance Win32_Process -Filter "Name = 'powershell.exe' OR Name = 'Runner.Listener.exe'" | Where-Object { $_.ExecutablePath -eq $listener -or ($_.CommandLine -and $_.CommandLine.Contains($script)) }).Count -gt 0
if ($settings) {
  $taskName = "crabbox-actions-runner-" + ($settings.agentName -replace "[^A-Za-z0-9_.-]", "-")
  $task = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
  if ($task -and $task.State -eq "Running") { $active = $true }
  ` + versionRead + `
}
@{settings=$settings;active=$active;version=$version} | ConvertTo-Json -Depth 5 -Compress
`)
	}
	if checkVersion {
		versionRead = `version="$("$runner_dir/bin/Runner.Listener" --version 2>/dev/null)" || version=""`
	}
	return `set -eu
runner_dir="$HOME/actions-runner"
settings=null
version=""
if [ -e "$runner_dir/.runner" ]; then
  settings="$(jq -c . "$runner_dir/.runner")"
  ` + versionRead + `
fi
state="$(systemctl show crabbox-actions-runner.service --property=LoadState --property=ActiveState)"
case "$state" in *'LoadState=loaded'*|*'LoadState=not-found'*) ;; *) echo 'runner service state is unknown' >&2; exit 2;; esac
active=""
while IFS= read -r property; do
  case "$property" in
    ActiveState=active|ActiveState=activating|ActiveState=deactivating) active=true ;;
    ActiveState=inactive|ActiveState=failed) active=false ;;
  esac
done <<STATE
$state
STATE
[ -n "$active" ] || { echo 'runner service state is unknown' >&2; exit 2; }
printf '%s' "$settings" | jq -c --argjson active "$active" --arg version "$version" '{settings:., active:$active, version:$version}'`

}

func remoteClearActionsRunnerSettings(target SSHTarget, binding ActionsRunnerBinding) string {
	if isWindowsNativeTarget(target) {
		return PowershellCommand(fmt.Sprintf(`$ErrorActionPreference = "Stop"
$dir = Join-Path $HOME "actions-runner"
$path = Join-Path $dir ".runner"
if (Test-Path -LiteralPath $path) {
  $settings = Get-Content -Raw -LiteralPath $path | ConvertFrom-Json
  if ($settings.agentId -ne %d) { throw "runner registration changed" }
}
$taskName = "crabbox-actions-runner-" + (%s -replace "[^A-Za-z0-9_.-]", "-")
$task = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
if ($task) {
  if ($task.State -eq "Running") { throw "runner task is still active" }
  Unregister-ScheduledTask -InputObject $task -Confirm:$false
}
foreach ($name in @(".runner", ".credentials", ".credentials_rsaparams")) {
  $file = Join-Path $dir $name
  if (Test-Path -LiteralPath $file) { Remove-Item -LiteralPath $file -Force }
}
`, binding.RunnerID, psQuote(binding.Name)))
	}
	return fmt.Sprintf(`set -eu
runner_dir="$HOME/actions-runner"
if [ -e "$runner_dir/.runner" ]; then
  [ "$(jq -er '.agentId' "$runner_dir/.runner")" = %s ] || { echo 'runner registration changed' >&2; exit 2; }
fi
# Remove the old boot trigger before publishing a new guest registration.
state="$(systemctl show crabbox-actions-runner.service --property=LoadState)"
case "$state" in
  *'LoadState=loaded'*) sudo systemctl disable crabbox-actions-runner.service ;;
  *'LoadState=not-found'*) ;;
  *) echo 'runner service state is unknown' >&2; exit 2 ;;
esac
rm -f -- "$runner_dir/.runner" "$runner_dir/.credentials" "$runner_dir/.credentials_rsaparams"
`, shellQuote(fmt.Sprint(binding.RunnerID)))
}

const (
	defaultActionsWaitTimeout    = 20 * time.Minute
	actionsRunnerStagePrefix     = "crabbox-runner-stage="
	actionsRunnerDiagnosticLimit = 16 * 1024
	maxActionsDiagnosticReserve  = 30 * time.Second
	// actionsRunnerReconcileTimeout bounds the post-install identity read and
	// GitHub check, which outlive cancellation of the registering operation.
	actionsRunnerReconcileTimeout    = 2 * time.Minute
	actionsRunnerReconcileReadBudget = 75 * time.Second
	actionsRunnerReconcileRetryDelay = 3 * time.Second
)

type actionsRunnerSetupResult struct {
	LastStage   string
	Diagnostics string
}

type githubRunnerHydrationHooks struct {
	Register     func(context.Context) error
	ClearState   func(context.Context) error
	Inputs       func(context.Context) (map[string]bool, bool, error)
	Dispatch     func(context.Context, []string) error
	OnDispatched func()
	Wait         func(context.Context, string) (actionsHydrationState, error)
	Now          func() time.Time
}

// runGitHubRunnerHydration bounds runner setup (registration through workflow
// dispatch) by waitTimeout, then gives the marker wait its own waitTimeout from
// dispatch, so a slow setup never shortens the marker-wait contract.
func runGitHubRunnerHydration(
	ctx context.Context,
	workflow, ref, configuredJob string,
	fields []string,
	waitTimeout time.Duration,
	stderr io.Writer,
	hooks githubRunnerHydrationHooks,
) (actionsHydrationState, error) {
	now := hooks.Now
	if now == nil {
		now = time.Now
	}
	parent := ctx
	ctx, cancelSetup := context.WithDeadline(parent, now().Add(waitTimeout))
	defer cancelSetup()
	if err := hooks.Register(ctx); err != nil {
		return actionsHydrationState{}, err
	}
	if err := ctx.Err(); err != nil {
		return actionsHydrationState{}, err
	}
	if err := hooks.ClearState(ctx); err != nil {
		return actionsHydrationState{}, err
	}
	if err := ctx.Err(); err != nil {
		return actionsHydrationState{}, err
	}
	if inputs, ok, err := hooks.Inputs(ctx); err != nil {
		fmt.Fprintf(stderr, "warning: inspect workflow inputs failed: %v\n", err)
	} else if ok {
		filtered, dropped := filterWorkflowInputs(fields, inputs)
		for _, field := range dropped {
			fmt.Fprintf(stderr, "warning: workflow %s does not declare input %s; omitting it\n", workflow, fieldName(field))
		}
		fields = filtered
		for _, required := range []string{"crabbox_id", "crabbox_runner_label", "crabbox_keep_alive_minutes"} {
			if !inputs[required] {
				return actionsHydrationState{}, Exit(2, "workflow %s at %s does not declare required hydrate input %s", workflow, ref, required)
			}
		}
	}
	expectedJob := configuredJob
	if !workflowFieldsContain(fields, "crabbox_job") {
		expectedJob = ""
	}
	if err := ctx.Err(); err != nil {
		return actionsHydrationState{}, err
	}
	if err := hooks.Dispatch(ctx, fields); err != nil {
		if expectedJob != "" && strings.Contains(err.Error(), "Unexpected input") {
			if err := ctx.Err(); err != nil {
				return actionsHydrationState{}, err
			}
			fields = dropWorkflowField(fields, "crabbox_job")
			expectedJob = ""
			fmt.Fprintln(stderr, "warning: retrying workflow dispatch without crabbox_job for compatibility")
			if retryErr := hooks.Dispatch(ctx, fields); retryErr != nil {
				return actionsHydrationState{}, retryErr
			}
		} else {
			return actionsHydrationState{}, err
		}
	}
	if hooks.OnDispatched != nil {
		hooks.OnDispatched()
	}
	if err := parent.Err(); err != nil {
		return actionsHydrationState{}, err
	}
	cancelSetup()
	waitCtx, cancelWait := context.WithDeadline(parent, now().Add(waitTimeout))
	defer cancelWait()
	return hooks.Wait(waitCtx, expectedJob)
}

type actionsRunnerSetupProgress struct {
	mu          sync.Mutex
	output      io.Writer
	partial     string
	lastStage   string
	diagnostics string
}

func newActionsRunnerSetupProgress(output io.Writer) *actionsRunnerSetupProgress {
	return &actionsRunnerSetupProgress{
		output:    output,
		lastStage: "bootstrap",
	}
}

func (p *actionsRunnerSetupProgress) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.partial += string(data)
	for {
		index := strings.IndexByte(p.partial, '\n')
		if index < 0 {
			break
		}
		p.consumeLineLocked(p.partial[:index])
		p.partial = p.partial[index+1:]
	}
	if len(p.partial) > actionsRunnerDiagnosticLimit {
		p.appendDiagnosticLocked(p.partial)
		p.partial = ""
	}
	return len(data), nil
}

func (p *actionsRunnerSetupProgress) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.partial != "" {
		p.consumeLineLocked(p.partial)
		p.partial = ""
	}
}

func (p *actionsRunnerSetupProgress) Result() actionsRunnerSetupResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return actionsRunnerSetupResult{
		LastStage:   p.lastStage,
		Diagnostics: strings.TrimSpace(p.diagnostics),
	}
}

func (p *actionsRunnerSetupProgress) consumeLineLocked(line string) {
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if line == "" {
		return
	}
	if stage, ok := strings.CutPrefix(line, actionsRunnerStagePrefix); ok {
		stage = sanitizeActionsRunnerStage(stage)
		if stage == "" {
			stage = "unknown"
		}
		p.lastStage = stage
		fmt.Fprintf(p.output, "actions runner setup stage=%s\n", stage)
		return
	}
	p.appendDiagnosticLocked(line + "\n")
}

func (p *actionsRunnerSetupProgress) appendDiagnosticLocked(value string) {
	p.diagnostics += value
	if len(p.diagnostics) > actionsRunnerDiagnosticLimit {
		p.diagnostics = p.diagnostics[len(p.diagnostics)-actionsRunnerDiagnosticLimit:]
	}
}

func sanitizeActionsRunnerStage(value string) string {
	var stage strings.Builder
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z':
			stage.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			stage.WriteRune(r)
		case r >= '0' && r <= '9':
			stage.WriteRune(r)
		case r == '-', r == '_', r == '.':
			stage.WriteRune(r)
		default:
			stage.WriteByte('-')
		}
	}
	return strings.Trim(stage.String(), "-")
}

func runActionsRunnerSetup(ctx context.Context, output io.Writer, run func(stdout, stderr io.Writer) error) (actionsRunnerSetupResult, error) {
	progress := newActionsRunnerSetupProgress(output)
	fmt.Fprintln(output, "actions runner setup stage=bootstrap")
	err := run(progress, progress)
	progress.Flush()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return progress.Result(), err
}

type githubActionsRunnerState struct {
	Found  bool
	Status string
	Busy   bool
}

type githubActionsRunnerLookup func(context.Context) (githubActionsRunnerState, error)

// githubActionsRunnerStateForBinding reads the exact recorded registration, so a
// same-named runner in the scope cannot satisfy the online check.
func githubActionsRunnerStateForBinding(ctx context.Context, binding ActionsRunnerBinding, childEnvDenylist []string) (githubActionsRunnerState, error) {
	if binding.RunnerID <= 0 || !actionsRunnerScopePattern.MatchString(binding.Scope) {
		return githubActionsRunnerState{}, Exit(2, "invalid Actions runner receipt")
	}
	out, err := ghOutputWithChildEnvironment(ctx, "", childEnvDenylist, "api", fmt.Sprintf("%s/actions/runners/%d", binding.Scope, binding.RunnerID))
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return githubActionsRunnerState{}, nil
		}
		return githubActionsRunnerState{}, err
	}
	var runner struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Busy   bool   `json:"busy"`
	}
	if err := json.Unmarshal([]byte(out), &runner); err != nil {
		return githubActionsRunnerState{}, Exit(3, "invalid GitHub runner response")
	}
	if runner.ID != binding.RunnerID || runner.Name != binding.Name {
		return githubActionsRunnerState{}, nil
	}
	return githubActionsRunnerState{Found: true, Status: runner.Status, Busy: runner.Busy}, nil
}

func waitForGitHubActionsRunnerOnline(
	ctx context.Context,
	scope string,
	name string,
	interval time.Duration,
	lookup githubActionsRunnerLookup,
	stderr io.Writer,
) error {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return waitForGitHubActionsRunnerOnlineWithPoll(ctx, scope, name, lookup, stderr, func(ctx context.Context) error {
		return waitForActionsPoll(ctx, interval)
	})
}

func waitForActionsPoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitForGitHubActionsRunnerOnlineWithPoll(
	ctx context.Context,
	scope string,
	name string,
	lookup githubActionsRunnerLookup,
	stderr io.Writer,
	poll func(context.Context) error,
) error {
	var lastState githubActionsRunnerState
	var lastErr error
	for {
		state, err := lookup(ctx)
		if err == nil {
			lastState = state
			lastErr = nil
			if ctx.Err() == nil && state.Found && strings.EqualFold(state.Status, "online") {
				fmt.Fprintf(stderr, "actions runner online scope=%s name=%s busy=%t\n", scope, name, state.Busy)
				return nil
			}
		} else {
			lastErr = err
		}

		status := "missing"
		if lastState.Found {
			status = blank(lastState.Status, "unknown")
		}
		if lastErr != nil {
			fmt.Fprintf(stderr, "waiting for actions runner online scope=%s name=%s error=%q\n", scope, name, lastErr.Error())
		} else {
			fmt.Fprintf(stderr, "waiting for actions runner online scope=%s name=%s status=%s busy=%t\n", scope, name, status, lastState.Busy)
		}

		pollErr := ctx.Err()
		if pollErr == nil {
			pollErr = poll(ctx)
		}
		if pollErr != nil {
			if errors.Is(pollErr, context.DeadlineExceeded) {
				if lastErr != nil {
					return Exit(5, "timed out waiting for GitHub Actions runner %s in %s to become online: last lookup error: %v", name, scope, lastErr)
				}
				return Exit(5, "timed out waiting for GitHub Actions runner %s in %s to become online: last status=%s busy=%t", name, scope, status, lastState.Busy)
			}
			return pollErr
		}
	}
}

func actionsRunnerBootstrapContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.WithCancel(ctx)
	}
	reserve := remaining / 20
	if reserve > maxActionsDiagnosticReserve {
		reserve = maxActionsDiagnosticReserve
	}
	if reserve <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, deadline.Add(-reserve))
}

func readGitHubActionsRunnerDiagnosticsBestEffort(ctx context.Context, target SSHTarget, secrets ...string) string {
	if err := ctx.Err(); err != nil {
		return fmt.Sprintf("diagnostics skipped: timeout budget exhausted: %v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := runSSHOutputWithRemoteWaitTimeout(ctx, target, remoteGitHubActionsRunnerDiagnostics(target), 15*time.Second, "2", "1")
	if ctx.Err() != nil {
		return fmt.Sprintf("diagnostics unavailable: %v", ctx.Err())
	}
	if err != nil {
		return fmt.Sprintf("diagnostics unavailable: %v", err)
	}
	out = redactActionsRunnerText(out, secrets...)
	if len(out) > actionsRunnerDiagnosticLimit {
		out = out[len(out)-actionsRunnerDiagnosticLimit:]
	}
	return strings.TrimSpace(out)
}

func redactActionsRunnerText(value string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

func remoteGitHubActionsRunnerDiagnostics(target SSHTarget) string {
	if isWindowsNativeTarget(target) {
		return PowershellCommand(`$ErrorActionPreference = "Continue"
$runnerDir = Join-Path $HOME "actions-runner"
Get-ScheduledTask | Where-Object { $_.TaskName -like "crabbox-actions-runner-*" } | ForEach-Object {
  Write-Output ("task=" + $_.TaskName + " state=" + $_.State)
}
Get-CimInstance Win32_Process | Where-Object { $_.Name -in @("Runner.Listener.exe", "Runner.Worker.exe") } | ForEach-Object {
  Write-Output ("process=" + $_.Name + " pid=" + $_.ProcessId)
}
foreach ($path in @((Join-Path $runnerDir "crabbox-runner.err.log"), (Join-Path $runnerDir "crabbox-runner.log"), (Join-Path $runnerDir "crabbox-config.log"))) {
  if (Test-Path -LiteralPath $path) {
    Write-Output ("log=" + $path)
    Get-Content -LiteralPath $path -Tail 40
  }
}
Get-ChildItem -LiteralPath (Join-Path $runnerDir "_diag") -Filter "Runner_*.log" -ErrorAction SilentlyContinue |
  Sort-Object LastWriteTime -Descending |
  Select-Object -First 1 |
  ForEach-Object {
    Write-Output ("log=" + $_.FullName)
    Get-Content -LiteralPath $_.FullName -Tail 40
  }
exit 0
`)
	}
	return `set +e
runner_dir="$HOME/actions-runner"
printf 'service='
systemctl is-active crabbox-actions-runner.service 2>&1
systemctl status crabbox-actions-runner.service --no-pager --lines=20 2>&1
for path in "$runner_dir/crabbox-runner.err.log" "$runner_dir/crabbox-runner.log" "$runner_dir/crabbox-config.log"; do
  if [ -f "$path" ]; then
    echo "log=$path"
    tail -n 40 "$path"
  fi
done
latest="$(find "$runner_dir/_diag" -maxdepth 1 -type f -name 'Runner_*.log' -printf '%T@ %p\n' 2>/dev/null | sort -nr | head -n 1 | cut -d' ' -f2-)"
if [ -n "$latest" ]; then
  echo "log=$latest"
  tail -n 40 "$latest"
fi
exit 0`
}

// windowsRunnerGitBashPowerShell gives a native Windows runner the Git Bash that
// GitHub-hosted Windows runners provide, so workflow steps with `shell: bash`
// work. A guest without it (MinGit has no bash.exe) gets the pinned, verified
// Git for Windows installer without a machine PATH change; the runner's own
// PATH gains Git\bin before config.cmd records it.
func windowsRunnerGitBashPowerShell() string {
	return `$crabboxGitBin = Join-Path $env:ProgramFiles "Git\bin"
if (-not (Test-Path -LiteralPath (Join-Path $crabboxGitBin "bash.exe"))) {
  Write-CrabboxRunnerStage "git-bash"
  $crabboxGitInstaller = Join-Path ([IO.Path]::GetTempPath()) ("crabbox-git-for-windows-" + [Guid]::NewGuid().ToString("N") + ".exe")
  try {
    Invoke-WebRequest -Uri ` + psQuote(gitForWindowsSetupURL) + ` -OutFile $crabboxGitInstaller -UseBasicParsing -TimeoutSec 600
    $crabboxGitSha = (Get-FileHash -LiteralPath $crabboxGitInstaller -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($crabboxGitSha -ne ` + psQuote(gitForWindowsSetupSHA256) + `) {
      throw "Git for Windows installer checksum mismatch: actual=$crabboxGitSha"
    }
    $crabboxGitExit = Invoke-CrabboxSetupProgram -FilePath $crabboxGitInstaller -ArgumentList @("/VERYSILENT", "/NORESTART", "/SUPPRESSMSGBOXES", "/NOCANCEL", "/SP-", "/o:PathOption=BashOnly")
    if ($crabboxGitExit -ne 0) {
      throw ("Git for Windows installer exited " + $crabboxGitExit)
    }
  } finally {
    Remove-Item -LiteralPath $crabboxGitInstaller -Force -ErrorAction SilentlyContinue
  }
  if (-not (Test-Path -LiteralPath (Join-Path $crabboxGitBin "bash.exe"))) {
    throw "Git for Windows did not provide bash.exe"
  }
}
$env:Path = $crabboxGitBin + ";" + $env:Path
`
}

// actionsLeaseLifetimeWarning reports a lease whose recorded expiry falls before
// the GitHub-runner hydration budget ends. Heartbeats renew idle time but not
// the lease TTL, so such a lease can expire during or right after hydration.
func actionsLeaseLifetimeWarning(leaseID string, server Server, now time.Time, wait time.Duration) string {
	// Heartbeats keep moving expires_at with the idle deadline; only the TTL
	// from creation is a hard limit.
	created, ok := parseLeaseLabelTime(server.Labels["created_at"])
	if !ok {
		return ""
	}
	ttl, ok := LeaseLabelDuration(server.Labels["ttl_secs"])
	if !ok {
		return ""
	}
	remaining := created.Add(ttl).Sub(now)
	if remaining >= wait {
		return ""
	}
	return fmt.Sprintf("warning: lease %s reaches its TTL in %s, before the --wait-timeout of %s; it can expire during or right after GitHub-runner hydration. Warm runner-hydration leases with a longer --ttl.", leaseID, max(remaining, 0).Round(time.Second), wait)
}
