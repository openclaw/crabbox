package utm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type vm struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Status        string        `json:"status"`
	Backend       string        `json:"backend"`
	Configuration configuration `json:"configuration"`
}
type configuration struct {
	Notes               string    `json:"notes"`
	Architecture        string    `json:"architecture"`
	Networks            []network `json:"networkInterfaces"`
	AdditionalArguments []any     `json:"qemuAdditionalArguments"`
}
type network struct {
	Index    int       `json:"index"`
	Mode     string    `json:"mode"`
	Address  string    `json:"address"`
	Forwards []forward `json:"portForwards"`
}
type forward struct {
	Protocol    string `json:"protocol"`
	HostAddress string `json:"hostAddress"`
	HostPort    int    `json:"hostPort"`
	GuestPort   int    `json:"guestPort"`
}

// Read only the published scripting dictionary. Names and status are observations;
// UUIDs and the durable allocation journal supply lifecycle identity.
const inventoryScript = `function run(argv) {
 var app = Application(argv[0]);
 return JSON.stringify(app.virtualMachines().map(function(vm) {
   return {id:vm.id(),name:vm.name(),status:String(vm.status()),backend:String(vm.backend()),configuration:vm.configuration()};
 }));
}`

func (b *backend) command(ctx context.Context, name string, args []string, input io.Reader) (core.LocalCommandResult, error) {
	if b.hostOS != "darwin" || b.hostArch != "arm64" {
		return core.LocalCommandResult{}, fmt.Errorf("provider=utm requires a local Apple Silicon macOS host with UTM application automation access")
	}
	// Bound each Apple event independently, including otherwise unbounded passive calls.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	result, err := b.rt.Exec.Run(ctx, core.LocalCommandRequest{Name: name, Args: args, Stdin: input, MaxCapturedOutputBytes: 4 << 20})
	if err != nil || result.ExitCode != 0 {
		if err == nil {
			err = fmt.Errorf("exit %d", result.ExitCode)
		}
		return result, shared.LocalCommandError("UTM control", result, err)
	}
	return result, nil
}

// control uses the same published Apple events as utmctl, through osascript.
// The signed bundled CLI can have a different Automation authorization path
// from the caller's scripting host; lifecycle must not depend on that path.
func (b *backend) control(ctx context.Context, action, id string, name ...string) error {
	if !validUUID(id) {
		return fmt.Errorf("UTM control requires an exact VM UUID")
	}
	var operation string
	switch action {
	case "clone":
		if len(name) != 1 {
			return fmt.Errorf("UTM clone requires one allocated name")
		}
		operation = "duplicate targetVM with properties {configuration:{name:" + scriptString(name[0]) + "}}"
	case "start":
		operation = "start targetVM"
	case "stop":
		operation = "stop targetVM by kill"
	case "delete":
		operation = "delete targetVM"
	case "reload":
		operation = "if status of targetVM is not stopped then error \"UTM clone must be stopped\"\n reload configuration targetVM"
	default:
		return fmt.Errorf("unsupported UTM lifecycle action %q", action)
	}
	script := "tell application " + scriptString(b.cfg.UTM.AppPath) + "\n set targetVM to first virtual machine whose id is " + scriptString(id) + "\n " + operation + "\nend tell"
	_, err := b.command(ctx, "/usr/bin/osascript", []string{"-"}, strings.NewReader(script))
	return err
}
func (b *backend) inventory(ctx context.Context) ([]vm, error) {
	out, err := b.command(ctx, "/usr/bin/osascript", []string{"-l", "JavaScript", "-e", inventoryScript, b.cfg.UTM.AppPath}, nil)
	if err != nil {
		return nil, fmt.Errorf("UTM application control unavailable (allow Automation access to UTM): %w", err)
	}
	var vms []vm
	if err := json.Unmarshal([]byte(out.Stdout), &vms); err != nil {
		return nil, fmt.Errorf("decode UTM scripting inventory: %w", err)
	}
	seen := map[string]bool{}
	for _, v := range vms {
		if !validUUID(v.ID) || seen[strings.ToLower(v.ID)] {
			return nil, fmt.Errorf("UTM inventory returned invalid or duplicate UUID %q", v.ID)
		}
		seen[strings.ToLower(v.ID)] = true
	}
	return vms, nil
}
func scriptString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`).Replace(s) + `"`
}
func findVM(vms []vm, id string) (vm, bool) {
	for _, v := range vms {
		if strings.EqualFold(v.ID, id) {
			return v, true
		}
	}
	return vm{}, false
}
func validateTemplate(v vm) error {
	if v.Status != "stopped" {
		return fmt.Errorf("UTM template %s must be stopped (status=%s)", v.ID, v.Status)
	}
	if v.Backend != "qemu" {
		return fmt.Errorf("UTM backend=%s is unsupported; use a prepared Linux QEMU template", v.Backend)
	}
	if v.Configuration.Architecture != "aarch64" {
		return fmt.Errorf("UTM architecture=%s is unsupported; initial supported QEMU architecture: aarch64 (Linux ARM64)", v.Configuration.Architecture)
	}
	if len(v.Configuration.Networks) != 1 || v.Configuration.Networks[0].Mode != "emulated" {
		return fmt.Errorf("UTM requires exactly one emulated network interface; guest-agent IP discovery, shared and bridged networking are unsupported")
	}
	if len(v.Configuration.AdditionalArguments) != 0 {
		return fmt.Errorf("UTM templates with additional QEMU arguments are unsupported: they can override lease network isolation")
	}
	return nil
}
func validateClone(v vm, r record) error {
	if r.isMacOS() {
		return validateMacOSVM(v, r)
	}
	if v.ID != r.VMID || v.ID == r.SourceID || v.Backend != "qemu" || v.Configuration.Notes != r.marker() {
		return fmt.Errorf("UTM VM %s ownership/configuration does not match lease %s", v.ID, r.LeaseID)
	}
	if v.Configuration.Architecture != r.Architecture || len(v.Configuration.Networks) != 1 || len(v.Configuration.AdditionalArguments) != 0 {
		return fmt.Errorf("UTM clone architecture/network configuration changed")
	}
	n := v.Configuration.Networks[0]
	if n.Mode != "emulated" || !strings.EqualFold(n.Address, r.MAC) || len(n.Forwards) != 1 {
		return fmt.Errorf("UTM clone network identity changed")
	}
	f := n.Forwards[0]
	if !strings.EqualFold(f.Protocol, "TCP") || f.HostAddress != "127.0.0.1" || f.HostPort != r.Port || f.GuestPort != 22 {
		return fmt.Errorf("UTM clone loopback SSH mapping changed")
	}
	return nil
}
