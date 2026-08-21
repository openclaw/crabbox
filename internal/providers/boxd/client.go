package boxd

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName        = "boxd"
	machineNamePrefix   = "crabbox-"
	defaultBoxdWorkRoot = "/home/boxd/crabbox"
	defaultBoxdZone     = "boxd.sh"
	boxdSSHUser         = "boxd"
	// The boxd image (Ubuntu 24.04, unminimized) ships bash, git, rsync and
	// tar; the check guards against a customized account default image.
	boxdReadyCheck = "command -v bash >/dev/null && command -v git >/dev/null && command -v rsync >/dev/null && command -v tar >/dev/null"
)

// machineRow is one row of `boxd machine list --json`.
type machineRow struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	URL     string `json:"url"`
	Source  string `json:"source"`
	Sharing string `json:"sharing"`
}

// machineSummary is the `boxd machine get --json` detail view.
type machineSummary struct {
	Name          string `json:"name"`
	ID            string `json:"id"`
	Status        string `json:"status"`
	URL           string `json:"url"`
	SSH           string `json:"ssh"`
	PublicIP      string `json:"public_ip"`
	Domains       string `json:"domains"`
	Source        string `json:"source"`
	AutoSuspend   string `json:"auto_suspend"`
	AutoHibernate string `json:"auto_hibernate"`
	Sharing       string `json:"sharing"`
	Networks      string `json:"networks"`
	Isolated      string `json:"isolated"`
}

// createdSummary is the `boxd machine new --json` response.
type createdSummary struct {
	Name   string `json:"name"`
	ID     string `json:"id"`
	URL    string `json:"url"`
	Source string `json:"source"`
	Boot   string `json:"boot"`
}

// boxd runs the external boxd CLI through the runtime's command runner. The
// token is never placed on argv: the CLI reads BOXD_TOKEN or its stored
// credentials itself. --api-url/--org are appended only when explicitly
// configured, so the CLI's own defaults (prod API, active org) apply otherwise.
func (b *backend) boxd(ctx context.Context, cfg core.Config, args []string, stderr io.Writer) (core.LocalCommandResult, error) {
	full := append([]string(nil), args...)
	if url := strings.TrimSpace(cfg.Boxd.APIURL); url != "" {
		full = append(full, "--api-url", url)
	}
	if org := strings.TrimSpace(cfg.Boxd.Org); org != "" {
		full = append(full, "--org", org)
	}
	return b.rt.Exec.Run(ctx, core.LocalCommandRequest{
		Name:   cfg.Boxd.CLIPath,
		Args:   full,
		Stderr: stderr,
	})
}

func commandError(action string, result core.LocalCommandResult, err error) error {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	code := result.ExitCode
	if code == 0 {
		code = 1
	}
	if detail != "" {
		return core.Exit(code, "%s failed: %v: %s", action, err, detail)
	}
	return core.Exit(code, "%s failed: %v", action, err)
}

// machineNameForLease derives the boxd machine name that encodes a crabbox
// lease id: `crabbox-cbx-<hex>`. boxd machine names are lowercase DNS labels,
// so the lease id's underscore is dashed; leaseIDFromMachineName reverses it.
func machineNameForLease(leaseID string) string {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return ""
	}
	return machineNamePrefix + strings.ReplaceAll(leaseID, "_", "-")
}

func leaseIDFromMachineName(name string) string {
	name = strings.TrimSpace(name)
	if !strings.HasPrefix(name, machineNamePrefix) {
		return ""
	}
	suffix := strings.TrimPrefix(name, machineNamePrefix)
	if suffix == "" {
		return ""
	}
	// crabbox lease ids are cbx_<hex>; the machine name dashed the underscore.
	leaseID := strings.Replace(suffix, "-", "_", 1)
	if !core.IsCanonicalLeaseID(leaseID) {
		return ""
	}
	return leaseID
}

// boxdState normalizes a boxd machine status into crabbox lease-state
// vocabulary. `standby` (idle auto-suspend) and `hibernated` machines wake on
// SSH ingress, so they count as ready; only `stopped` needs an explicit start.
func boxdState(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "unknown":
		return "provisioning"
	case "running", "ready":
		return "ready"
	case "standby", "suspended", "hibernated", "hibernating", "suspending":
		return "ready"
	case "pending", "starting", "creating", "waking", "resuming", "restoring":
		return "provisioning"
	case "stopped", "stopping":
		return "stopped"
	default:
		return strings.ToLower(strings.TrimSpace(status))
	}
}

func machineReachable(status string) bool {
	return boxdState(status) == "ready"
}

func machineTerminal(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "destroyed", "destroying":
		return true
	default:
		return false
	}
}

// missingMachineResponse reports whether the CLI's output unambiguously says
// THIS machine does not exist ("VM 'name' not found", from the CLI's list-based
// resolver). Anchored on the quoted name so an unrelated error mentioning "not
// found" (e.g. a config or binary lookup failure) is never treated as gone.
func missingMachineResponse(name, stdout, stderr string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	detail := strings.ToLower(stdout + "\n" + stderr)
	return strings.Contains(detail, strings.ToLower("'"+name+"' not found"))
}

// extractJSON trims CLI noise around the JSON payload: the first '[' or '{'
// starts the document.
func extractJSON(output string) string {
	for i := 0; i < len(output); i++ {
		if output[i] == '[' || output[i] == '{' {
			return strings.TrimSpace(output[i:])
		}
	}
	return ""
}

func (b *backend) listMachines(ctx context.Context, cfg core.Config) ([]machineRow, error) {
	result, err := b.boxd(ctx, cfg, []string{"machine", "list", "--json"}, nil)
	if err != nil {
		return nil, commandError("boxd machine list", result, err)
	}
	payload := extractJSON(result.Stdout)
	if payload == "" {
		return nil, core.Exit(5, "boxd machine list produced no JSON output")
	}
	var rows []machineRow
	if err := json.Unmarshal([]byte(payload), &rows); err != nil {
		return nil, core.Exit(5, "parse boxd machine list output: %v", err)
	}
	return rows, nil
}

// getMachine returns (summary, found, error). A definitive not-found is
// (zero, false, nil); any other failure is an error.
func (b *backend) getMachine(ctx context.Context, cfg core.Config, name string) (machineSummary, bool, error) {
	result, err := b.boxd(ctx, cfg, []string{"machine", "get", name, "--json"}, nil)
	if err != nil {
		if missingMachineResponse(name, result.Stdout, result.Stderr) {
			return machineSummary{}, false, nil
		}
		return machineSummary{}, false, commandError("boxd machine get "+name, result, err)
	}
	payload := extractJSON(result.Stdout)
	if payload == "" {
		return machineSummary{}, false, core.Exit(5, "boxd machine get %s produced no JSON output", name)
	}
	var summary machineSummary
	if err := json.Unmarshal([]byte(payload), &summary); err != nil {
		return machineSummary{}, false, core.Exit(5, "parse boxd machine get %s output: %v", name, err)
	}
	return summary, true, nil
}

func (b *backend) createMachine(ctx context.Context, cfg core.Config, name string) (createdSummary, error) {
	// Every crabbox machine is created ISOLATED: boxd's sandbox mode (no
	// east-west reachability to the account's other machines, no in-VM boxd
	// CLI, no inherited integrations or agent credentials). Fixed at creation
	// and deliberately not configurable here.
	result, err := b.boxd(ctx, cfg, []string{"machine", "new", name, "--isolated", "--json"}, b.rt.Stderr)
	if err != nil {
		return createdSummary{}, commandError("boxd machine new "+name, result, err)
	}
	payload := extractJSON(result.Stdout)
	if payload == "" {
		return createdSummary{}, core.Exit(5, "boxd machine new %s produced no JSON output", name)
	}
	var created createdSummary
	if err := json.Unmarshal([]byte(payload), &created); err != nil {
		return createdSummary{}, core.Exit(5, "parse boxd machine new %s output: %v", name, err)
	}
	if strings.TrimSpace(created.Name) == "" {
		return createdSummary{}, core.Exit(5, "boxd machine new %s response did not include a machine name", name)
	}
	return created, nil
}

// destroyMachine removes a machine. A definitive not-found counts as success
// (the machine is gone either way); any other failure propagates so the caller
// retains the claim and can retry.
func (b *backend) destroyMachine(ctx context.Context, cfg core.Config, name string) error {
	result, err := b.boxd(ctx, cfg, []string{"machine", "remove", name, "--confirm", "--json"}, b.rt.Stderr)
	if err != nil {
		if missingMachineResponse(name, result.Stdout, result.Stderr) {
			return nil
		}
		return commandError("boxd machine remove "+name, result, err)
	}
	return nil
}

func (b *backend) startMachine(ctx context.Context, cfg core.Config, name string) error {
	result, err := b.boxd(ctx, cfg, []string{"machine", "start", name}, b.rt.Stderr)
	if err != nil {
		return commandError("boxd machine start "+name, result, err)
	}
	return nil
}

func (b *backend) stopMachine(ctx context.Context, cfg core.Config, name string) error {
	result, err := b.boxd(ctx, cfg, []string{"machine", "stop", name}, b.rt.Stderr)
	if err != nil {
		return commandError("boxd machine stop "+name, result, err)
	}
	return nil
}

// zoneFromHost recovers the cluster zone from a `<name>.<zone>` host the CLI
// reported for this machine (e.g. crabbox-cbx-1234.boxd.sh -> boxd.sh).
func zoneFromHost(host, name string) string {
	host = strings.TrimSpace(host)
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	if i := strings.IndexAny(host, "/:"); i >= 0 {
		host = host[:i]
	}
	prefix := name + "."
	if strings.HasPrefix(host, prefix) {
		return strings.TrimPrefix(host, prefix)
	}
	return ""
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now().UTC()
	}
	return time.Now().UTC()
}
