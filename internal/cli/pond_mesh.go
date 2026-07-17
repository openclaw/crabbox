package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// pondExposedPortsLabelKey is the reserved provider-label key that carries the
// comma-separated list of TCP ports a lease wants reachable over the SSH-mesh
// plane. The key lives next to pondLabelKey in the existing provider label
// index so `crabbox pond connect` can discover ports without growing a new
// store.
const pondExposedPortsLabelKey = "crabbox_exposed_ports"

// pondMaxExposedPort is the inclusive ceiling on TCP port numbers accepted by
// --expose. Anything above this is malformed input and rejected at flag-parse
// time so we never write garbage into provider labels.
const pondMaxExposedPort = 65535

// pondMaxExposedPortsPerLease bounds the per-lease --expose list so the
// resulting comma-separated label fits inside the 63-character provider label
// ceiling enforced by sanitizeProviderLabelValue (six characters per port plus
// a separator leaves headroom for up to ten ports).
const pondMaxExposedPortsPerLease = 10

// pondMeshLocalPortStart is the first port the operator-side allocator hands
// out for local -L forwards. Picked above the IANA registered range so it
// rarely collides with developer-local services.
const pondMeshLocalPortStart = 51820

// pondMeshLocalPortEnd bounds the operator-side allocator. The window is
// generous (a few thousand ports) so a single operator can connect to many
// large ponds simultaneously without exhausting it.
const pondMeshLocalPortEnd = 52819

// pondMeshCancelWaitDelay bounds the exec.CommandContext fallback when a
// platform teardown API reports an error before the SSH root has exited.
const pondMeshCancelWaitDelay = 5 * time.Second

// pondMeshHostsRoot is the per-user state directory under HOME where
// `pond connect` writes the rendered hosts and env files. The structure
// mirrors the existing ~/.crabbox layout other commands already use.
const pondMeshHostsRoot = ".crabbox/pond"

// pondMeshHostsFileName is the rendered file mapping <peer>.cbx to the local
// loopback port the operator can use to reach that peer's exposed port.
const pondMeshHostsFileName = "hosts"

// pondMeshEnvFileName is the rendered shell-export snippet so an operator can
// `eval $(crabbox pond connect <name> --export)` and use peer names directly.
const pondMeshEnvFileName = "env"

// pondMeshDaemonFileName records daemonized `pond connect --export` PIDs so
// `pond disconnect <name>` can clean up this pond without broad process scans.
const pondMeshDaemonFileName = "daemon.json"

// pondMeshRunner abstracts os/exec.CommandContext so the connect orchestration
// is testable without spawning real ssh processes. The production runner
// returns a real *exec.Cmd; tests inject a recorder that captures arguments.
type pondMeshRunner interface {
	Command(ctx context.Context, name string, args ...string) pondMeshHandle
}

type pondMeshEnvironmentRunner interface {
	CommandWithEnvironment(ctx context.Context, denied []string, name string, args ...string) pondMeshHandle
}

// pondMeshHandle is the minimal surface area the connect loop needs from a
// spawned process: start it, wait for it to exit, and tear it down on context
// cancellation. The real implementation wraps *exec.Cmd; tests substitute a
// stub that records the invocation and exits when signaled.
type pondMeshHandle interface {
	Start() error
	Wait() error
	Process() processSignaler
	PID() int
	String() string
	// WasTerminatedByOurCancel reports whether THIS process's Wait error is
	// attributable solely to our own teardown rather than a genuine failure.
	// Teardown is a hard kill of the isolated process group/tree, recorded
	// per-member process by the ctx watchdog's Cancel hook. The connect loop never
	// infers intent from the shared context,
	// which under a race can misattribute one member's genuine failure to
	// another member's cancellation. On Unix our SIGKILL is uncatchable, so our
	// kill is always ProcessState.Signaled() while a peer that reached its own
	// nonzero exit code is Exited() — unambiguous. On Windows there are no
	// signals, so the Job Object's cancellation-only exit code provides the
	// terminal provenance. A genuine failure therefore returns false even when
	// the shared context was cancelled first.
	WasTerminatedByOurCancel() bool
}

// processSignaler is the subset of *os.Process that the connect loop touches
// when the operator presses Ctrl-C and the orchestrator tears down each
// underlying ssh process in turn.
type processSignaler interface {
	Signal(os.Signal) error
	Kill() error
}

// pondMeshExecRunner is the production pondMeshRunner. It wraps
// exec.CommandContext directly so behaviour under ctx cancellation matches
// every other Crabbox SSH invocation.
type pondMeshExecRunner struct{}

func (pondMeshExecRunner) Command(ctx context.Context, name string, args ...string) pondMeshHandle {
	return pondMeshExecCommand(ctx, nil, name, args...)
}

func (pondMeshExecRunner) CommandWithEnvironment(ctx context.Context, denied []string, name string, args ...string) pondMeshHandle {
	return pondMeshExecCommand(ctx, denied, name, args...)
}

func pondMeshExecCommand(ctx context.Context, denied []string, name string, args ...string) pondMeshHandle {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(denied) > 0 {
		cmd.Env = childEnvironmentWithout(os.Environ(), denied...)
	}
	cmd.WaitDelay = pondMeshCancelWaitDelay
	h := &pondMeshExecHandle{cmd: cmd, managed: true}
	// Keep exec.CommandContext's hard-cancel semantics, but terminate the full
	// isolated process group/tree so ProxyCommand and wrapper descendants cannot
	// survive their SSH leader. We override Cancel to record provenance: mark
	// that WE initiated this process's teardown before the kill lands, so a
	// concurrent Wait observes the flag. We deliberately do NOT send a graceful,
	// catchable signal first: an ssh that trapped SIGINT and exited
	// non-zero would report ProcessState.Exited(), indistinguishable from a
	// genuine tunnel failure. SIGKILL cannot be caught, so on Unix our own
	// teardown is always Signaled() — letting WasTerminatedByOurCancel suppress
	// it without ever consulting the shared context, so a different member's
	// genuine failure is never misclassified as our cancellation.
	h.cmd.Cancel = h.cancelAndKill
	return h
}

// pondMeshDaemonRunner creates SSH tunnel processes that survive the parent
// CLI exit. It uses plain exec.Command (not CommandContext) so context
// cancellation does not kill the tunnels, and sets Setpgid so the kernel
// orphan-adopts them when crabbox exits. Used only by the --export path
// so eval $(crabbox pond connect --export) works.
type pondMeshDaemonRunner struct{}

func (pondMeshDaemonRunner) Command(_ context.Context, name string, args ...string) pondMeshHandle {
	cmd := exec.Command(name, args...)
	configureDaemonCommand(cmd)
	return &pondMeshExecHandle{cmd: cmd}
}

func (pondMeshDaemonRunner) CommandWithEnvironment(_ context.Context, denied []string, name string, args ...string) pondMeshHandle {
	cmd := exec.Command(name, args...)
	if len(denied) > 0 {
		cmd.Env = childEnvironmentWithout(os.Environ(), denied...)
	}
	configureDaemonCommand(cmd)
	return &pondMeshExecHandle{cmd: cmd}
}

func pondMeshRunnerCommand(ctx context.Context, runner pondMeshRunner, target SSHTarget, name string, args ...string) pondMeshHandle {
	if environmentRunner, ok := runner.(pondMeshEnvironmentRunner); ok {
		return environmentRunner.CommandWithEnvironment(ctx, target.ChildEnvDenylist, name, args...)
	}
	return runner.Command(ctx, name, args...)
}

type pondMeshExecHandle struct {
	cmd      *exec.Cmd
	managed  bool
	platform pondMeshPlatformState
	// cancelled records that our own teardown (the ctx watchdog's Cancel hook)
	// is terminating this process. It is set before the kill is delivered so a
	// concurrent Wait always observes it.
	cancelled atomic.Bool
	// cancelFailed makes cleanup/inventory failures observable instead of
	// suppressing them as an ordinary operator cancellation.
	cancelFailed atomic.Bool
	cancelErrMu  sync.Mutex
	cancelErr    error
}

func (h *pondMeshExecHandle) String() string { return h.cmd.String() }
func (h *pondMeshExecHandle) PID() int {
	if h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}
func (h *pondMeshExecHandle) Process() processSignaler {
	if h.cmd.Process == nil {
		return nil
	}
	return h.cmd.Process
}

// cancelAndKill is the exec.CommandContext Cancel hook. It records that OUR
// teardown initiated this process's termination, then hard-kills its complete
// isolated process group/tree.
// An os.ErrProcessDone from the kill means the process finished on its own
// before our kill landed. The cancelled flag is stored before the kill so a
// concurrent Wait observes it.
func (h *pondMeshExecHandle) cancelAndKill() error {
	h.cancelled.Store(true)
	if h.cmd.Process == nil {
		return nil
	}
	if err := terminatePondMeshForwardProcess(h); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		h.cancelFailed.Store(true)
		h.cancelErrMu.Lock()
		h.cancelErr = err
		h.cancelErrMu.Unlock()
		return err
	}
	return nil
}

func (h *pondMeshExecHandle) joinCancellationError(err error) error {
	h.cancelErrMu.Lock()
	defer h.cancelErrMu.Unlock()
	if h.cancelErr == nil || errors.Is(err, h.cancelErr) {
		return err
	}
	return errors.Join(err, h.cancelErr)
}

func (h *pondMeshExecHandle) WasTerminatedByOurCancel() bool {
	if !h.cancelled.Load() {
		// We never touched this process, so any Wait error is a genuine
		// failure regardless of whether the shared context was cancelled by a
		// sibling member or the caller.
		return false
	}
	if h.cancelFailed.Load() {
		// Cleanup itself failed. Surface that error instead of disguising a
		// possible surviving process tree as clean cancellation.
		return false
	}
	return killAttributableToCancel(h.cmd.ProcessState)
}

// pondMeshDefaultRunner is overridden in tests via the package-level pointer
// so the production pondMeshExecRunner never appears in unit tests. Reads are
// guarded by the tests running serially per package.
var pondMeshDefaultRunner pondMeshRunner = pondMeshExecRunner{}

// requestedExposedPorts validates and normalizes the values from a repeated
// `--expose` flag. Each entry must be a positive TCP port; comma-separated
// values are expanded; duplicates are dropped. The result is sorted so the
// rendered provider label is deterministic across re-runs with the same flag
// order.
func requestedExposedPorts(values []string) ([]string, error) {
	seen := map[int]bool{}
	out := []int{}
	for _, raw := range values {
		if strings.TrimSpace(raw) == "" {
			return nil, exit(2, "--expose value must not be empty")
		}
		parts := splitCommaList(raw)
		if len(parts) == 0 {
			return nil, exit(2, "--expose value must not be empty")
		}
		for _, part := range parts {
			port, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || port <= 0 || port > pondMaxExposedPort {
				return nil, exit(2, "--expose %q must be a TCP port in 1..%d", part, pondMaxExposedPort)
			}
			if seen[port] {
				continue
			}
			seen[port] = true
			out = append(out, port)
		}
	}
	if len(out) > pondMaxExposedPortsPerLease {
		return nil, exit(2, "--expose accepts at most %d distinct ports per lease", pondMaxExposedPortsPerLease)
	}
	sort.Ints(out)
	rendered := make([]string, len(out))
	for i, port := range out {
		rendered[i] = strconv.Itoa(port)
	}
	return rendered, nil
}

// pondExposedPortsLabelSeparator joins port numbers inside the provider
// label. We use `-` rather than `,` because sanitizeProviderLabelValue
// rewrites any character outside [A-Za-z0-9_.-] to `_`, which would corrupt a
// comma-separated list at storage time.
const pondExposedPortsLabelSeparator = "-"

// renderExposedPortsLabel turns a normalized port list into the
// label-safe form written into the provider label. Returns "" for an empty
// list so callers can use the helper unconditionally and skip emission when
// no ports are exposed.
func renderExposedPortsLabel(ports []string) string {
	if len(ports) == 0 {
		return ""
	}
	return strings.Join(ports, pondExposedPortsLabelSeparator)
}

// parseExposedPortsLabel inverts renderExposedPortsLabel. Unparseable tokens
// are skipped silently so a half-corrupted label never aborts the connect
// flow; the upstream writer is authoritative and any garbage there is an
// upstream bug we surface in tests rather than at runtime.
func parseExposedPortsLabel(value string) []int {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	out := []int{}
	for _, part := range strings.Split(value, pondExposedPortsLabelSeparator) {
		port, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || port <= 0 || port > pondMaxExposedPort {
			continue
		}
		out = append(out, port)
	}
	sort.Ints(out)
	return out
}

// pondMember is the projection of a Server pond connect consumes. The
// connect orchestration only needs the name shown in rendered hosts, the
// lease identity, the SSH target, and the declared exposed ports.
type pondMember struct {
	Name     string
	Provider string
	SSH      SSHTarget
	Ports    []int
	Lease    string
}

// pondMeshForward is one (peer, port) pair plus the loopback port the
// operator-side allocator assigned to it. The doctor sub-check counts these
// to report the SSH-mesh plane status without re-running the orchestration.
type pondMeshForward struct {
	Peer       string `json:"peer"`
	RemotePort int    `json:"remotePort"`
	LocalPort  int    `json:"localPort"`
	LeaseID    string `json:"leaseID"`
}

// pondMeshSummary captures the operator-visible result of preparing a
// connect: the forwards, the path of the rendered hosts file, and the env
// export lines so the same object can be returned from tests or rendered to
// stdout in production.
type pondMeshSummary struct {
	HostsPath string            `json:"hostsPath"`
	EnvPath   string            `json:"envPath"`
	Exports   []string          `json:"exports"`
	Forwards  []pondMeshForward `json:"forwards"`
}

type pondMeshDaemonState struct {
	Pond      string                  `json:"pond"`
	StartedAt string                  `json:"startedAt"`
	PIDs      []int                   `json:"pids"`
	Processes []pondMeshDaemonProcess `json:"processes,omitempty"`
	Forwards  []pondMeshForward       `json:"forwards"`
}

type pondMeshDaemonProcess struct {
	PID     int             `json:"pid"`
	Command string          `json:"command,omitempty"`
	Forward pondMeshForward `json:"forward"`
}

// pondConnectOptions bundles the dependencies the orchestration needs.
// Production wires App.Stdout/Stderr + the real runner; tests substitute a
// recorder so the suite never spawns processes or touches HOME.
type pondConnectOptions struct {
	Stdout    io.Writer
	Stderr    io.Writer
	HomeDir   string
	Runner    pondMeshRunner
	PortAlloc func(used map[int]bool) (int, error)
}

// (a App) pondConnect is the Kong-dispatched entry point. It reads pond
// members across *every* SSH-mesh-capable provider in the pond (not just
// one), computes the unified forward table, writes hosts + env, prints the
// operator-visible exports, then holds the connections open until the
// context is cancelled (Ctrl-C).
//
// A provider is SSH-mesh-eligible when it advertises FeatureSSH on its Spec.
// That includes managed-Linux providers plus SSH-lease providers; ponds can
// span both groups and still be connected with one command.
//
// `--provider X` is still accepted but is now a *filter* (single-provider
// mode), not a requirement. Errors during teardown are best-effort: the
// operator already knows the connect is over by the time we get there.
func (a App) pondConnect(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("pond connect", a.Stderr)
	providerFilter := fs.String("provider", "", "limit to a single provider (default: all SSH-mesh-capable providers in the pond)")
	jsonOut := fs.Bool("json", false, "print the forward table as JSON and exit")
	exportOnly := fs.Bool("export", false, "print shell exports for the rendered hosts and exit")
	providerFlags := registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return exit(2, "usage: crabbox pond connect <name>")
	}
	pond, err := requestedPondName(fs.Arg(0))
	if err != nil {
		return err
	}
	if pond == "" {
		return exit(2, "usage: crabbox pond connect <name>")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if *providerFilter != "" {
		cfg.Provider = *providerFilter
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	members, ineligible, err := collectPondMembersAcrossProviders(ctx, runtimeForApp(a), cfg, pond, *providerFilter)
	if err != nil {
		return err
	}
	for _, ip := range ineligible {
		fmt.Fprintf(a.Stderr, "pond %q: skipping provider %q (no SSH-mesh capability)\n", pond, ip)
	}
	if len(members) == 0 {
		fmt.Fprintf(a.Stderr, "pond %q has no SSH-mesh-capable members\n", pond)
		return nil
	}
	opts := pondConnectOptions{Stdout: a.Stdout, Stderr: a.Stderr, HomeDir: os.Getenv("HOME"), Runner: pondMeshDefaultRunner}
	summary, err := preparePondMeshSummary(pond, members, opts)
	if err != nil {
		return err
	}
	if len(summary.Forwards) == 0 {
		fmt.Fprintf(a.Stderr, "pond %q has no members declaring --expose; nothing to forward\n", pond)
		return nil
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(summary)
	}
	if *exportOnly {
		if !pondMeshDaemonSupported(runtime.GOOS) {
			return exit(2, "pond connect --export is not supported on Windows operator hosts yet; run without --export or from macOS/Linux")
		}
		// Start daemons before emitting exports so shell evals never see
		// assignments for tunnels that failed to start.
		if _, err := stopPondMeshDaemonState(opts.HomeDir, pond); err != nil {
			return err
		}
		daemonRunner := pondMeshDaemonRunner{}
		groups, err := pondMeshForwardGroups(members, summary.Forwards)
		if err != nil {
			return err
		}
		var started []pondMeshHandle
		var startedGroups []pondMeshForwardGroup
		for _, group := range groups {
			args := pondMeshSSHArgsForForwards(group.Target, group.Forwards)
			handle := pondMeshRunnerCommand(context.Background(), daemonRunner, group.Target, "ssh", args...)
			if err := handle.Start(); err != nil {
				stopDaemonHandles(started)
				return fmt.Errorf("start ssh forwards for %s: %w", pondMeshForwardGroupLabel(group.Forwards), err)
			}
			started = append(started, handle)
			startedGroups = append(startedGroups, group)
			for _, fwd := range group.Forwards {
				fmt.Fprintf(opts.Stderr, "  -L 127.0.0.1:%d -> %s:%d\n", fwd.LocalPort, fwd.Peer, fwd.RemotePort)
			}
		}
		// Give tunnels a brief window to complete authentication and forward
		// setup, then verify none exited immediately (wrong key, host unreachable, ...).
		waitCh := make(chan error, len(started))
		for i, h := range started {
			go func(group pondMeshForwardGroup, h pondMeshHandle) {
				err := h.Wait()
				select {
				case waitCh <- fmt.Errorf("ssh forwards for %s exited immediately: %w", pondMeshForwardGroupLabel(group.Forwards), err):
				default:
				}
			}(startedGroups[i], h)
		}
		select {
		case err := <-waitCh:
			stopDaemonHandles(started)
			return err
		case <-time.After(200 * time.Millisecond):
		}
		if err := writePondMeshDaemonState(opts.HomeDir, pond, summary, startedGroups, started); err != nil {
			stopDaemonHandles(started)
			return err
		}
		for _, line := range summary.Exports {
			fmt.Fprintln(a.Stdout, line)
		}
		fmt.Fprintf(a.Stderr, "pond %q SSH-mesh daemon started (%d forwards)\n", pond, len(summary.Forwards))
		return nil
	}
	fmt.Fprintf(a.Stdout, "pond %q SSH-mesh ready (%d forwards)\n", pond, len(summary.Forwards))
	for _, line := range summary.Exports {
		fmt.Fprintln(a.Stdout, line)
	}
	fmt.Fprintf(a.Stdout, "wrote %s\nwrote %s\n", summary.HostsPath, summary.EnvPath)
	return runPondMeshForwards(ctx, opts, members, summary)
}

func (a App) pondDisconnect(_ context.Context, args []string) error {
	fs := newFlagSet("pond disconnect", a.Stderr)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return exit(2, "usage: crabbox pond disconnect <name>")
	}
	pond, err := requestedPondName(fs.Arg(0))
	if err != nil {
		return err
	}
	if pond == "" {
		return exit(2, "usage: crabbox pond disconnect <name>")
	}
	stopped, err := stopPondMeshDaemonState(os.Getenv("HOME"), pond)
	if err != nil {
		return err
	}
	if stopped == 0 {
		fmt.Fprintf(a.Stdout, "pond %q has no recorded SSH-mesh daemons\n", pond)
		return nil
	}
	fmt.Fprintf(a.Stdout, "pond %q disconnected %d SSH-mesh daemon(s)\n", pond, stopped)
	return nil
}

// collectPondMembersAcrossProviders reads local claim sidecars for the pond,
// groups them by provider, and for each SSH-mesh-capable provider in the set
// loads its backend, lists leases, and collects pond members. Providers
// without SSH-mesh capability are returned in the `ineligible` list so the
// caller can warn the operator (e.g. a URL-only Modal box in the same pond
// will be skipped here but still appear in `pond peers`).
//
// providerFilter, when non-empty, restricts the search to that single
// provider — the caller passes this through from `--provider X` for users
// who want an explicit single-provider filter.
func collectPondMembersAcrossProviders(ctx context.Context, rt Runtime, cfg Config, pond, providerFilter string) ([]pondMember, []string, error) {
	claims, err := listLeaseClaims()
	if err != nil {
		return nil, nil, err
	}
	matches := filterClaimsForPond(claims, pond, providerFilter)
	if len(matches) == 0 {
		return nil, nil, nil
	}
	byProvider := make(map[string][]leaseClaim)
	order := make([]string, 0, 4)
	for _, claim := range matches {
		key := strings.TrimSpace(claim.Provider)
		if _, seen := byProvider[key]; !seen {
			order = append(order, key)
		}
		byProvider[key] = append(byProvider[key], claim)
	}
	sort.Strings(order)
	var members []pondMember
	var ineligible []string
	for _, p := range order {
		caps := providerCapabilities(p)
		if !caps.SSHMesh {
			ineligible = append(ineligible, p)
			continue
		}
		providerCfg := cfg
		providerCfg.Provider = p
		backend, berr := loadBackend(providerCfg, rt)
		if berr != nil {
			return nil, nil, fmt.Errorf("load backend for provider %s: %w", p, berr)
		}
		sshBackend, ok := backend.(SSHLeaseBackend)
		if !ok {
			// Provider declares FeatureSSH but its backend does not implement
			// SSHLeaseBackend — treat as ineligible (the operator should file
			// a provider-side bug rather than have pond connect explode).
			ineligible = append(ineligible, p)
			continue
		}
		servers, serr := sshBackend.List(ctx, ListRequest{Options: LeaseOptions{Pond: normalizePondName(pond)}})
		if serr != nil {
			return nil, nil, fmt.Errorf("list %s leases: %w", p, serr)
		}
		providerMembers, merr := collectPondMembers(ctx, sshBackend, providerCfg, servers, pond)
		if merr != nil {
			return nil, nil, fmt.Errorf("collect %s pond members: %w", p, merr)
		}
		for i := range providerMembers {
			providerMembers[i].Provider = p
		}
		members = append(members, providerMembers...)
	}
	members = disambiguatePondMemberNames(members)
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	return members, ineligible, nil
}

// collectPondMembers narrows a backend's list output to the pond of interest.
// It resolves SSH targets only for members with exposed ports, avoiding a
// provider lifecycle change when pond connect has nothing to forward.
func collectPondMembers(ctx context.Context, backend SSHLeaseBackend, cfg Config, servers []Server, pond string) ([]pondMember, error) {
	servers = filterServersByPond(servers, pond)
	out := make([]pondMember, 0, len(servers))
	for _, server := range servers {
		resolveID := pondResolveIDForServer(server)
		name := strings.TrimSpace(serverSlug(server))
		if name == "" {
			name = resolveID
		}
		ports := parseExposedPortsLabel(server.Labels[pondExposedPortsLabelKey])
		if len(ports) == 0 {
			out = append(out, pondMember{Name: name, Ports: ports, Lease: resolveID})
			continue
		}
		expectedClaim, expectedClaimed, err := resolveLeaseClaim(resolveID)
		if err != nil {
			return nil, fmt.Errorf("resolve %s claim: %w", server.Name, err)
		}
		lease, err := backend.Resolve(ctx, ResolveRequest{Options: leaseOptionsFromConfig(cfg), ID: resolveID, Prepare: true})
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", server.Name, err)
		}
		if expectedClaimed && expectedClaim.LeaseID == lease.LeaseID {
			if _, err := updateLeaseClaimEndpointIfUnchanged(lease.LeaseID, expectedClaim, lease.Server, lease.SSH); err != nil {
				current, claimed, resolveErr := resolveLeaseClaim(lease.LeaseID)
				if resolveErr != nil {
					return nil, fmt.Errorf("refresh %s claim endpoint: %w", server.Name, resolveErr)
				}
				if claimed && claimEndpointInactiveState(current.Labels["state"]) {
					return nil, fmt.Errorf("refresh %s claim endpoint: lease %s became inactive during resolve", server.Name, lease.LeaseID)
				}
				if !claimed || !leaseClaimHasEndpoint(current, lease) {
					return nil, fmt.Errorf("refresh %s claim endpoint: %w", server.Name, err)
				}
			}
		}
		if name == "" {
			name = lease.LeaseID
		}
		out = append(out, pondMember{
			Name:  name,
			SSH:   lease.SSH,
			Ports: ports,
			Lease: lease.LeaseID,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func leaseClaimHasEndpoint(claim leaseClaim, lease LeaseTarget) bool {
	if lease.Server.CloudID != "" && claim.CloudID != lease.Server.CloudID {
		return false
	}
	if lease.SSH.Host != "" && claim.SSHHost != lease.SSH.Host {
		return false
	}
	if port, err := strconv.Atoi(strings.TrimSpace(lease.SSH.Port)); err == nil && port > 0 && claim.SSHPort != port {
		return false
	}
	return true
}

func disambiguatePondMemberNames(members []pondMember) []pondMember {
	counts := map[string]int{}
	for _, member := range members {
		counts[envSafeName(member.Name)]++
	}
	used := map[string]bool{}
	for i := range members {
		key := envSafeName(members[i].Name)
		if counts[key] > 1 || used[key] {
			members[i].Name = duplicatePondMemberName(members[i])
			key = envSafeName(members[i].Name)
		}
		if used[key] {
			members[i].Name = duplicatePondMemberNameWithLease(members[i])
			key = envSafeName(members[i].Name)
		}
		used[key] = true
	}
	return members
}

func duplicatePondMemberName(member pondMember) string {
	base := normalizePondName(member.Name)
	if base == "" {
		base = "peer"
	}
	suffix := normalizePondName(member.Provider)
	if suffix == "" {
		suffix = shortLeaseID(member.Lease)
	}
	if suffix == "" {
		suffix = "peer"
	}
	return base + "-" + suffix
}

func duplicatePondMemberNameWithLease(member pondMember) string {
	name := duplicatePondMemberName(member)
	if suffix := shortLeaseID(member.Lease); suffix != "" {
		return name + "-" + suffix
	}
	return name
}

func shortLeaseID(leaseID string) string {
	leaseID = normalizePondName(leaseID)
	leaseID = strings.TrimPrefix(leaseID, "cbx-")
	leaseID = strings.TrimPrefix(leaseID, "isb-")
	if len(leaseID) > 6 {
		return leaseID[len(leaseID)-6:]
	}
	return leaseID
}

// preparePondMeshSummary builds the forward table and renders hosts + env
// files under HOME. It does not spawn processes; orchestration is split out
// so the rendering is unit-testable in isolation from any ssh exec.
func preparePondMeshSummary(pond string, members []pondMember, opts pondConnectOptions) (pondMeshSummary, error) {
	used := map[int]bool{}
	alloc := opts.PortAlloc
	if alloc == nil {
		alloc = allocateLocalForwardPort
	}
	forwards := []pondMeshForward{}
	for _, member := range members {
		for _, port := range member.Ports {
			localPort, err := alloc(used)
			if err != nil {
				return pondMeshSummary{}, err
			}
			used[localPort] = true
			forwards = append(forwards, pondMeshForward{
				Peer:       member.Name,
				RemotePort: port,
				LocalPort:  localPort,
				LeaseID:    member.Lease,
			})
		}
	}
	if len(forwards) == 0 {
		return pondMeshSummary{}, nil
	}
	hostsPath, envPath, err := pondMeshHostsAndEnvPaths(opts.HomeDir, pond)
	if err != nil {
		return pondMeshSummary{}, err
	}
	hostsBody := renderPondMeshHostsFile(forwards)
	envBody, exports := renderPondMeshEnvFile(forwards)
	if err := writePondMeshStateFile(hostsPath, hostsBody); err != nil {
		return pondMeshSummary{}, err
	}
	if err := writePondMeshStateFile(envPath, envBody); err != nil {
		return pondMeshSummary{}, err
	}
	return pondMeshSummary{HostsPath: hostsPath, EnvPath: envPath, Exports: exports, Forwards: forwards}, nil
}

// allocateLocalForwardPort walks the operator-side window looking for a free
// loopback port not already in the in-flight allocation set. It probes the
// kernel with a listen(0) bind so we never collide with an unrelated service
// the operator is already running on the same address.
func allocateLocalForwardPort(used map[int]bool) (int, error) {
	for port := pondMeshLocalPortStart; port <= pondMeshLocalPortEnd; port++ {
		if used[port] {
			continue
		}
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		_ = listener.Close()
		return port, nil
	}
	return 0, exit(7, "no free loopback ports between %d and %d for SSH-mesh forwards", pondMeshLocalPortStart, pondMeshLocalPortEnd)
}

// pondMeshHostsAndEnvPaths returns the absolute paths to the per-pond state
// files under HOME. The parent directory is created with 0700 so the layout
// matches the rest of ~/.crabbox.
func pondMeshHostsAndEnvPaths(home, pond string) (string, string, error) {
	dir, err := ensurePondMeshStateDir(home, pond)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(dir, pondMeshHostsFileName), filepath.Join(dir, pondMeshEnvFileName), nil
}

func pondMeshStateDir(home, pond string) (string, error) {
	if home == "" {
		return "", exit(2, "HOME is unset; cannot write pond SSH-mesh state files")
	}
	return filepath.Join(home, pondMeshHostsRoot, pond), nil
}

func ensurePondMeshStateDir(home, pond string) (string, error) {
	dir, err := pondMeshStateDir(home, pond)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func pondMeshDaemonStatePath(home, pond string, create bool) (string, error) {
	var (
		dir string
		err error
	)
	if create {
		dir, err = ensurePondMeshStateDir(home, pond)
	} else {
		dir, err = pondMeshStateDir(home, pond)
	}
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, pondMeshDaemonFileName), nil
}

// renderPondMeshHostsFile renders operator-visible hosts aliases. Ports stay
// in comments and CRABBOX_POND_* exports because /etc/hosts cannot encode
// host:port pairs.
func renderPondMeshHostsFile(forwards []pondMeshForward) string {
	var b strings.Builder
	b.WriteString("# crabbox pond SSH-mesh operator-side aliases\n")
	b.WriteString("# Use CRABBOX_POND_<PEER>_<PORT> for the forwarded host:port.\n")
	for _, fwd := range forwards {
		fmt.Fprintf(&b, "127.0.0.1  %s.cbx %s-%d.cbx  # local=127.0.0.1:%d remote=:%d\n", fwd.Peer, fwd.Peer, fwd.RemotePort, fwd.LocalPort, fwd.RemotePort)
	}
	return b.String()
}

// renderPondMeshEnvFile renders the shell-export snippet and the list of
// individual export lines used by `pond connect --export`. The snippet is
// stable across re-runs with the same forward set so `eval $(crabbox pond
// connect --export)` is safe to re-run.
func renderPondMeshEnvFile(forwards []pondMeshForward) (string, []string) {
	exports := make([]string, 0, len(forwards))
	var b strings.Builder
	b.WriteString("# crabbox pond SSH-mesh — eval $(crabbox pond connect <name> --export)\n")
	for _, fwd := range forwards {
		line := fmt.Sprintf("export CRABBOX_POND_%s_%d=127.0.0.1:%d", strings.ToUpper(envSafeName(fwd.Peer)), fwd.RemotePort, fwd.LocalPort)
		exports = append(exports, line)
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String(), exports
}

// envSafeName collapses anything outside [A-Z0-9_] in a peer name so the
// resulting variable name is a valid shell identifier. Empty inputs fold to
// "_" so the helper never panics on edge cases the caller has already
// validated upstream.
func envSafeName(name string) string {
	name = strings.ToUpper(strings.TrimSpace(name))
	if name == "" {
		return "_"
	}
	var b strings.Builder
	for _, r := range name {
		ok := (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "_"
	}
	return out
}

// writePondMeshStateFile persists rendered content with 0600 permissions so
// the hosts and env files sit alongside other Crabbox per-user secrets in
// terms of disk-level posture.
func writePondMeshStateFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}

// stopDaemonHandles kills every daemon process that was started before a
// partial failure. Used by the --export path to avoid orphaned tunnels when
// a later forward fails to start.
func stopDaemonHandles(handles []pondMeshHandle) {
	for _, h := range handles {
		if proc := h.Process(); proc != nil {
			if osProc, ok := proc.(*os.Process); ok {
				_ = stopDaemonProcess(osProc, h.PID())
			} else {
				_ = proc.Kill()
			}
		}
	}
}

func writePondMeshDaemonState(home, pond string, summary pondMeshSummary, groups []pondMeshForwardGroup, handles []pondMeshHandle) error {
	path, err := pondMeshDaemonStatePath(home, pond, true)
	if err != nil {
		return err
	}
	pids := make([]int, 0, len(handles))
	processes := make([]pondMeshDaemonProcess, 0, len(handles))
	for i, handle := range handles {
		if pid := handle.PID(); pid > 0 {
			pids = append(pids, pid)
			process := pondMeshDaemonProcess{PID: pid, Command: handle.String()}
			if i < len(groups) && len(groups[i].Forwards) > 0 {
				process.Forward = groups[i].Forwards[0]
			}
			processes = append(processes, process)
		}
	}
	state := pondMeshDaemonState{
		Pond:      pond,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		PIDs:      pids,
		Processes: processes,
		Forwards:  summary.Forwards,
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func stopPondMeshDaemonState(home, pond string) (int, error) {
	path, err := pondMeshDaemonStatePath(home, pond, false)
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !pondMeshDaemonSupported(runtime.GOOS) {
		return 0, exit(2, "pond disconnect is not supported on Windows operator hosts because exported SSH-mesh daemons are disabled")
	}
	var state pondMeshDaemonState
	if err := json.Unmarshal(data, &state); err != nil {
		return 0, exit(2, "parse pond daemon state %s: %v", path, err)
	}
	stopped := 0
	for _, entry := range pondMeshDaemonProcesses(state) {
		if entry.PID <= 0 {
			continue
		}
		command, alive := pondMeshDaemonProcessCommand(entry.PID)
		if !alive || !isPondMeshDaemonCommand(command, entry.Forward) {
			continue
		}
		proc, err := os.FindProcess(entry.PID)
		if err != nil {
			continue
		}
		if err := stopDaemonProcess(proc, entry.PID); err == nil {
			stopped++
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return stopped, err
	}
	return stopped, nil
}

func pondMeshDaemonProcesses(state pondMeshDaemonState) []pondMeshDaemonProcess {
	if len(state.Processes) > 0 {
		return state.Processes
	}
	out := make([]pondMeshDaemonProcess, 0, len(state.PIDs))
	for i, pid := range state.PIDs {
		process := pondMeshDaemonProcess{PID: pid}
		if i < len(state.Forwards) {
			process.Forward = state.Forwards[i]
		}
		out = append(out, process)
	}
	return out
}

func pondMeshDaemonProcessCommand(pid int) (string, bool) {
	out, err := systemInspectionCommand("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return "", false
	}
	command := strings.TrimSpace(string(out))
	return command, command != ""
}

func pondMeshDaemonSupported(goos string) bool {
	return goos != "windows"
}

func isPondMeshDaemonCommand(command string, fwd pondMeshForward) bool {
	command = strings.TrimSpace(command)
	if command == "" || fwd.LocalPort <= 0 || fwd.RemotePort <= 0 {
		return false
	}
	lower := strings.ToLower(command)
	if !strings.Contains(lower, "ssh") || !strings.Contains(command, "-L") {
		return false
	}
	forward := fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", fwd.LocalPort, fwd.RemotePort)
	return strings.Contains(command, forward)
}

func pondSSHTargetsByLease(members []pondMember) map[string]SSHTarget {
	targets := make(map[string]SSHTarget, len(members))
	for _, member := range members {
		if member.Lease == "" {
			continue
		}
		targets[member.Lease] = member.SSH
	}
	return targets
}

func pondResolveIDForServer(server Server) string {
	if server.Labels != nil {
		if leaseID := strings.TrimSpace(server.Labels["lease"]); leaseID != "" {
			return leaseID
		}
	}
	if server.CloudID != "" {
		return server.CloudID
	}
	if server.ID != 0 {
		return strconv.FormatInt(server.ID, 10)
	}
	return serverSlug(server)
}

type pondMeshForwardGroup struct {
	Target   SSHTarget
	Forwards []pondMeshForward
}

func pondMeshForwardGroups(members []pondMember, forwards []pondMeshForward) ([]pondMeshForwardGroup, error) {
	peerTarget := pondSSHTargetsByLease(members)
	groupIndex := make(map[string]int, len(peerTarget))
	groups := make([]pondMeshForwardGroup, 0, len(peerTarget))
	for _, fwd := range forwards {
		target, ok := peerTarget[fwd.LeaseID]
		if !ok {
			return nil, exit(7, "no SSH target resolved for pond peer %q", fwd.Peer)
		}
		index, ok := groupIndex[fwd.LeaseID]
		if !ok {
			index = len(groups)
			groupIndex[fwd.LeaseID] = index
			groups = append(groups, pondMeshForwardGroup{Target: target})
		}
		groups[index].Forwards = append(groups[index].Forwards, fwd)
	}
	return groups, nil
}

func pondMeshForwardGroupLabel(forwards []pondMeshForward) string {
	if len(forwards) == 0 {
		return "unknown peer"
	}
	ports := make([]string, 0, len(forwards))
	for _, fwd := range forwards {
		ports = append(ports, strconv.Itoa(fwd.RemotePort))
	}
	return fmt.Sprintf("%s:%s", forwards[0].Peer, strings.Join(ports, ","))
}

// pondMeshSSHArgsForForwards builds one owned `ssh` argument vector containing
// every `-L` for a member. One connection per member avoids both multiplexed
// background masters and concurrent per-port handshakes.
func pondMeshSSHArgsForForwards(target SSHTarget, forwards []pondMeshForward) []string {
	// A pond member tunnel must remain the process owned by its exec.Cmd. Reusing a
	// persistent master lets the short-lived mux client exit while the master
	// retains the listener, so cancellation cannot reap or classify the tunnel.
	target.NoControlMaster = true
	args := append([]string{}, sshBaseArgs(target)...)
	args = append(args,
		"-o", "ControlPath=none",
		"-o", "ControlPersist=no",
		"-N",
		"-o", "ExitOnForwardFailure=yes",
	)
	for _, fwd := range forwards {
		forward := fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", fwd.LocalPort, fwd.RemotePort)
		args = append(args, "-L", forward)
	}
	args = append(args, target.User+"@"+target.Host)
	return args
}

// runPondMeshForwards spawns one SSH process per member, with all that member's
// -L specifications, then waits for ctx cancellation or any process exit and
// tears the rest down. Each member owns one non-multiplexed SSH process so
// cancellation owns its complete lifetime and process tree.
func runPondMeshForwards(ctx context.Context, opts pondConnectOptions, members []pondMember, summary pondMeshSummary) error {
	terminationCtx, stopTerminationSignals := pondMeshTerminationContext(ctx)
	defer stopTerminationSignals()
	runner := opts.Runner
	if runner == nil {
		runner = pondMeshDefaultRunner
	}
	groups, err := pondMeshForwardGroups(members, summary.Forwards)
	if err != nil {
		return err
	}
	type runningForwardGroup struct {
		group  pondMeshForwardGroup
		handle pondMeshHandle
	}
	ctx, cancel := context.WithCancel(terminationCtx)
	defer cancel()
	running := []runningForwardGroup{}
	reapStarted := func() error {
		var wg sync.WaitGroup
		waitErrs := make([]error, len(running))
		terminatedByCancel := make([]bool, len(running))
		for i, rf := range running {
			wg.Add(1)
			go func() {
				defer wg.Done()
				waitErrs[i] = rf.handle.Wait()
				terminatedByCancel[i] = rf.handle.WasTerminatedByOurCancel()
			}()
		}
		wg.Wait()
		for i, rf := range running {
			if waitErrs[i] == nil && !terminatedByCancel[i] {
				return fmt.Errorf("ssh forwards for %s exited unexpectedly", pondMeshForwardGroupLabel(rf.group.Forwards))
			}
			if waitErrs[i] != nil && !terminatedByCancel[i] {
				return waitErrs[i]
			}
		}
		return nil
	}
	for _, group := range groups {
		args := pondMeshSSHArgsForForwards(group.Target, group.Forwards)
		handle := pondMeshRunnerCommand(ctx, runner, group.Target, "ssh", args...)
		if err := handle.Start(); err != nil {
			parentErr := terminationCtx.Err()
			cancel()
			if reapErr := reapStarted(); reapErr != nil {
				return reapErr
			}
			if parentErr != nil && errors.Is(err, parentErr) {
				return nil
			}
			return fmt.Errorf("start ssh forwards for %s: %w", pondMeshForwardGroupLabel(group.Forwards), err)
		}
		running = append(running, runningForwardGroup{group: group, handle: handle})
		for _, fwd := range group.Forwards {
			fmt.Fprintf(opts.Stderr, "  -L 127.0.0.1:%d -> %s:%d\n", fwd.LocalPort, fwd.Peer, fwd.RemotePort)
		}
	}
	var wg sync.WaitGroup
	var firstErr error
	var firstErrOnce sync.Once
	for _, rf := range running {
		wg.Add(1)
		go func(rf runningForwardGroup) {
			defer wg.Done()
			err := rf.handle.Wait()
			// Classify by PER-MEMBER-PROCESS provenance, never the shared context.
			// Reading ctx.Err() here is unsafe under a race: if a sibling
			// forward or the caller cancels first, ctx.Err() is non-nil by the
			// time THIS forward's genuine failure is classified, and its error
			// would be silently discarded. Instead we suppress a Wait error
			// only when OUR teardown terminated THIS specific process with the
			// platform's hard process-group/tree kill.
			// A genuine non-zero exit the peer reached on its own is still
			// recorded even when the shared context was already cancelled.
			terminatedByCancel := rf.handle.WasTerminatedByOurCancel()
			if err == nil && !terminatedByCancel {
				firstErrOnce.Do(func() {
					firstErr = fmt.Errorf("ssh forwards for %s exited unexpectedly", pondMeshForwardGroupLabel(rf.group.Forwards))
				})
			} else if err != nil && !terminatedByCancel {
				firstErrOnce.Do(func() { firstErr = err })
			}
			cancel()
		}(rf)
	}
	<-ctx.Done()
	// Cancelling the derived context fires each handle's Cancel hook, which
	// records provenance and hard-kills the complete platform process group/tree.
	// We intentionally send no catchable signal
	// of our own — an ssh that trapped SIGINT and exited non-zero would look
	// like a genuine failure. Just wait for every waiter to finish reaping.
	wg.Wait()
	return firstErr
}

// pondMeshDoctorCounts inspects a slice of servers (already filtered by
// pond) and returns the per-plane summary doctor surfaces. The function is
// intentionally pure: no network, no SSH, no provider calls. doctor invokes
// it from doctorPondMeshSummary so the test suite exercises every branch
// without spawning subprocesses.
func pondMeshDoctorCounts(servers []Server) (memberCount, exposedCount, totalPorts int) {
	for _, server := range servers {
		memberCount++
		ports := parseExposedPortsLabel(server.Labels[pondExposedPortsLabelKey])
		if len(ports) > 0 {
			exposedCount++
			totalPorts += len(ports)
		}
	}
	return memberCount, exposedCount, totalPorts
}
