package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
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

func (a App) registerActionsRunner(ctx context.Context, cfg Config, target SSHTarget, leaseID, slug, scope, group, name string, extraLabels []string, expected leaseClaim, strictSnapshot bool) (leaseClaim, error) {
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
	var updated leaseClaim
	err := WithDurableLeaseClaimLockContext(ctx, leaseID, func(claim *leaseClaim, exists bool, persist func() error) error {
		if !exists || !sameActionsWorkspaceClaimOwner(*claim, expected) || strictSnapshot && !reflect.DeepEqual(*claim, expected) {
			return Exit(2, "Actions runner lease claim changed or is missing")
		}
		fingerprint := actionsRunnerLeaseFingerprint(*claim)
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
				if !*guest.Active {
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
		token, err := githubActionsRegistrationToken(ctx, scope, target.ChildEnvDenylist)
		if err != nil {
			return err
		}
		script := githubActionsRunnerInstallScriptForTarget(cfg.Actions.RunnerVersion, cfg.Actions.Ephemeral, target)
		if isWindowsNativeTarget(target) {
			script = "$env:RUNNER_GROUP = " + psQuote(group) + "\n" + script
		} else {
			script = "export RUNNER_GROUP=" + shellQuote(group) + "\n" + script
		}
		_, destination, _ := strings.Cut(scope, "/")
		input := githubActionsRunnerInstallInput(destination, name, strings.Join(labels, ","), token, script)
		installErr := runSSHInputQuiet(ctx, target, githubActionsRunnerInstallRemoteCommand(target), input)
		// A lost SSH acknowledgement is not permission to register again. Recover
		// the official local identity and verify its exact API object first.
		guest, err = readActionsRunnerGuest(ctx, target, checkVersion)
		if err != nil {
			return errors.Join(installErr, err)
		}
		if guest.Settings == nil || *guest.Active || guest.Settings.URL != actionsRunnerURL(scope) || guest.Settings.Name != name || guest.Settings.Ephemeral != cfg.Actions.Ephemeral || group != "" && guest.Settings.Group != group {
			return errors.Join(installErr, Exit(7, "runner configuration identity is unconfirmed; listener was not started; reconcile this lease before retrying"))
		}
		if checkVersion && guest.Version != cfg.Actions.RunnerVersion {
			return errors.Join(installErr, Exit(7, "configured runner version is unconfirmed; listener was not started"))
		}
		binding = &ActionsRunnerBinding{Scope: scope, RunnerID: guest.Settings.ID, Name: name, Group: guest.Settings.Group, Ephemeral: cfg.Actions.Ephemeral, LeaseFingerprint: fingerprint}
		present, err := githubActionsRunnerPresent(ctx, *binding, leaseID, labels, target.ChildEnvDenylist)
		if err != nil {
			return errors.Join(installErr, err)
		}
		if !present {
			return errors.Join(installErr, Exit(7, "configured runner %d is absent from GitHub; listener was not started", binding.RunnerID))
		}
		claim.ActionsRunner = binding
		if err := persist(); err != nil {
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			rollbackErr := deleteGitHubActionsRunner(rollbackCtx, *binding, leaseID, target.ChildEnvDenylist)
			return errors.Join(Exit(7, "persist runner %s/%d; listener was not started: %v", binding.Scope, binding.RunnerID, err), rollbackErr)
		}
		updated = cloneLeaseClaim(*claim)
		_, err = actionsRunnerControl(ctx, target, githubActionsRunnerStartScript(target))
		return err
	})
	if err != nil {
		return updated, err
	}
	fmt.Fprintf(a.Stdout, "actions runner registered scope=%s id=%d name=%s labels=%s ephemeral=%t\n", scope, updated.ActionsRunner.RunnerID, name, strings.Join(labels, ","), cfg.Actions.Ephemeral)
	return updated, nil
}

func actionsRunnerSettingsMatch(settings *actionsRunnerSettings, binding *ActionsRunnerBinding) bool {
	return settings.ID == binding.RunnerID && settings.Name == binding.Name && settings.URL == actionsRunnerURL(binding.Scope) && settings.Group == binding.Group && settings.Ephemeral == binding.Ephemeral
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
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" {
		return Exit(2, "actions unregister requires --id")
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
