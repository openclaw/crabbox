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
	"sort"
	"strconv"
	"strings"
	"sync"
)

// crewExposedPortsLabelKey is the reserved provider-label key that carries the
// comma-separated list of TCP ports a lease wants reachable over the SSH-mesh
// plane. The key lives next to crewLabelKey in the existing provider label
// index so `crabbox crew connect` can discover ports without growing a new
// store.
const crewExposedPortsLabelKey = "crabbox_exposed_ports"

// crewMaxExposedPort is the inclusive ceiling on TCP port numbers accepted by
// --expose. Anything above this is malformed input and rejected at flag-parse
// time so we never write garbage into provider labels.
const crewMaxExposedPort = 65535

// crewMaxExposedPortsPerLease bounds the per-lease --expose list so the
// resulting comma-separated label fits inside the 63-character provider label
// ceiling enforced by sanitizeProviderLabelValue (six characters per port plus
// a separator leaves headroom for up to ten ports).
const crewMaxExposedPortsPerLease = 10

// crewMeshLocalPortStart is the first port the operator-side allocator hands
// out for local -L forwards. Picked above the IANA registered range so it
// rarely collides with developer-local services.
const crewMeshLocalPortStart = 51820

// crewMeshLocalPortEnd bounds the operator-side allocator. The window is
// generous (a few thousand ports) so a single operator can connect to many
// large crews simultaneously without exhausting it.
const crewMeshLocalPortEnd = 52819

// crewMeshHostsRoot is the per-user state directory under HOME where
// `crew connect` writes the rendered hosts and env files. The structure
// mirrors the existing ~/.crabbox layout other commands already use.
const crewMeshHostsRoot = ".crabbox/crew"

// crewMeshHostsFileName is the rendered file mapping <peer>.cbx to the local
// loopback port the operator can use to reach that peer's exposed port.
const crewMeshHostsFileName = "hosts"

// crewMeshEnvFileName is the rendered shell-export snippet so an operator can
// `eval $(crabbox crew connect <name> --export)` and use peer names directly.
const crewMeshEnvFileName = "env"

// crewMeshRunner abstracts os/exec.CommandContext so the connect orchestration
// is testable without spawning real ssh processes. The production runner
// returns a real *exec.Cmd; tests inject a recorder that captures arguments.
type crewMeshRunner interface {
	Command(ctx context.Context, name string, args ...string) crewMeshHandle
}

// crewMeshHandle is the minimal surface area the connect loop needs from a
// spawned process: start it, wait for it to exit, and tear it down on context
// cancellation. The real implementation wraps *exec.Cmd; tests substitute a
// stub that records the invocation and exits when signaled.
type crewMeshHandle interface {
	Start() error
	Wait() error
	Process() processSignaler
	String() string
}

// processSignaler is the subset of *os.Process that the connect loop touches
// when the operator presses Ctrl-C and the orchestrator tears down each
// underlying ssh process in turn.
type processSignaler interface {
	Signal(os.Signal) error
	Kill() error
}

// crewMeshExecRunner is the production crewMeshRunner. It wraps
// exec.CommandContext directly so behaviour under ctx cancellation matches
// every other Crabbox SSH invocation.
type crewMeshExecRunner struct{}

func (crewMeshExecRunner) Command(ctx context.Context, name string, args ...string) crewMeshHandle {
	return &crewMeshExecHandle{cmd: exec.CommandContext(ctx, name, args...)}
}

type crewMeshExecHandle struct {
	cmd *exec.Cmd
}

func (h *crewMeshExecHandle) Start() error   { return h.cmd.Start() }
func (h *crewMeshExecHandle) Wait() error    { return h.cmd.Wait() }
func (h *crewMeshExecHandle) String() string { return h.cmd.String() }
func (h *crewMeshExecHandle) Process() processSignaler {
	if h.cmd.Process == nil {
		return nil
	}
	return h.cmd.Process
}

// crewMeshDefaultRunner is overridden in tests via the package-level pointer
// so the production crewMeshExecRunner never appears in unit tests. Reads are
// guarded by the tests running serially per package.
var crewMeshDefaultRunner crewMeshRunner = crewMeshExecRunner{}

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
			if err != nil || port <= 0 || port > crewMaxExposedPort {
				return nil, exit(2, "--expose %q must be a TCP port in 1..%d", part, crewMaxExposedPort)
			}
			if seen[port] {
				continue
			}
			seen[port] = true
			out = append(out, port)
		}
	}
	if len(out) > crewMaxExposedPortsPerLease {
		return nil, exit(2, "--expose accepts at most %d distinct ports per lease", crewMaxExposedPortsPerLease)
	}
	sort.Ints(out)
	rendered := make([]string, len(out))
	for i, port := range out {
		rendered[i] = strconv.Itoa(port)
	}
	return rendered, nil
}

// crewExposedPortsLabelSeparator joins port numbers inside the provider
// label. We use `-` rather than `,` because sanitizeProviderLabelValue
// rewrites any character outside [A-Za-z0-9_.-] to `_`, which would corrupt a
// comma-separated list at storage time.
const crewExposedPortsLabelSeparator = "-"

// renderExposedPortsLabel turns a normalized port list into the
// label-safe form written into the provider label. Returns "" for an empty
// list so callers can use the helper unconditionally and skip emission when
// no ports are exposed.
func renderExposedPortsLabel(ports []string) string {
	if len(ports) == 0 {
		return ""
	}
	return strings.Join(ports, crewExposedPortsLabelSeparator)
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
	for _, part := range strings.Split(value, crewExposedPortsLabelSeparator) {
		port, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || port <= 0 || port > crewMaxExposedPort {
			continue
		}
		out = append(out, port)
	}
	sort.Ints(out)
	return out
}

// crewMember is the projection of a Server crew connect consumes. The
// connect orchestration only needs the name shown in rendered hosts, the
// SSHTarget used to launch ssh, and the declared exposed ports.
type crewMember struct {
	Name  string
	SSH   SSHTarget
	Ports []int
	Lease string
}

// crewMeshForward is one (peer, port) pair plus the loopback port the
// operator-side allocator assigned to it. The doctor sub-check counts these
// to report the SSH-mesh plane status without re-running the orchestration.
type crewMeshForward struct {
	Peer       string
	RemotePort int
	LocalPort  int
	LeaseID    string
}

// crewMeshSummary captures the operator-visible result of preparing a
// connect: the forwards, the path of the rendered hosts file, and the env
// export lines so the same object can be returned from tests or rendered to
// stdout in production.
type crewMeshSummary struct {
	HostsPath string
	EnvPath   string
	Exports   []string
	Forwards  []crewMeshForward
}

// crewConnectOptions bundles the dependencies the orchestration needs.
// Production wires App.Stdout/Stderr + the real runner; tests substitute a
// recorder so the suite never spawns processes or touches HOME.
type crewConnectOptions struct {
	Stdout    io.Writer
	Stderr    io.Writer
	HomeDir   string
	Runner    crewMeshRunner
	PortAlloc func(used map[int]bool) (int, error)
}

// (a App) crewConnect is the Kong-dispatched entry point. It reads crew
// members, computes the forward table, writes hosts + env, prints the
// operator-visible exports, then holds the connections open until the
// context is cancelled (Ctrl-C). Errors during teardown are best-effort:
// the operator already knows the connect is over by the time we get there.
func (a App) crewConnect(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("crew connect", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpAll())
	jsonOut := fs.Bool("json", false, "print the forward table as JSON and exit")
	exportOnly := fs.Bool("export", false, "print shell exports for the rendered hosts and exit")
	providerFlags := registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return exit(2, "usage: crabbox crew connect <name>")
	}
	crew, err := requestedCrewName(fs.Arg(0))
	if err != nil {
		return err
	}
	if crew == "" {
		return exit(2, "usage: crabbox crew connect <name>")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	sshBackend, ok := backend.(SSHLeaseBackend)
	if !ok {
		return exit(2, "provider=%s does not expose SSH leases; crew connect requires an SSH-mesh-capable provider", backend.Spec().Name)
	}
	servers, err := sshBackend.List(ctx, ListRequest{Options: leaseOptionsFromConfig(cfg)})
	if err != nil {
		return err
	}
	members, err := collectCrewMembers(ctx, sshBackend, cfg, servers, crew)
	if err != nil {
		return err
	}
	opts := crewConnectOptions{Stdout: a.Stdout, Stderr: a.Stderr, HomeDir: os.Getenv("HOME"), Runner: crewMeshDefaultRunner}
	summary, err := prepareCrewMeshSummary(crew, members, opts)
	if err != nil {
		return err
	}
	if len(summary.Forwards) == 0 {
		fmt.Fprintf(a.Stderr, "crew %q has no members declaring --expose; nothing to forward\n", crew)
		return nil
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(summary)
	}
	if *exportOnly {
		for _, line := range summary.Exports {
			fmt.Fprintln(a.Stdout, line)
		}
		return nil
	}
	fmt.Fprintf(a.Stdout, "crew %q SSH-mesh ready (%d forwards)\n", crew, len(summary.Forwards))
	for _, line := range summary.Exports {
		fmt.Fprintln(a.Stdout, line)
	}
	fmt.Fprintf(a.Stdout, "wrote %s\nwrote %s\n", summary.HostsPath, summary.EnvPath)
	return runCrewMeshForwards(ctx, opts, members, summary)
}

// collectCrewMembers narrows a backend's list output to the crew of interest
// and resolves each member's SSHTarget. Servers without an exposed-ports
// label are kept in the projection (Ports is empty) so the no-op case is
// observable to callers and to doctor.
func collectCrewMembers(ctx context.Context, backend SSHLeaseBackend, cfg Config, servers []Server, crew string) ([]crewMember, error) {
	servers = filterServersByCrew(servers, crew)
	out := make([]crewMember, 0, len(servers))
	for _, server := range servers {
		lease, err := backend.Resolve(ctx, ResolveRequest{Options: leaseOptionsFromConfig(cfg), ID: serverSlug(server)})
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", server.Name, err)
		}
		name := strings.TrimSpace(serverSlug(server))
		if name == "" {
			name = lease.LeaseID
		}
		out = append(out, crewMember{
			Name:  name,
			SSH:   lease.SSH,
			Ports: parseExposedPortsLabel(server.Labels[crewExposedPortsLabelKey]),
			Lease: lease.LeaseID,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// prepareCrewMeshSummary builds the forward table and renders hosts + env
// files under HOME. It does not spawn processes; orchestration is split out
// so the rendering is unit-testable in isolation from any ssh exec.
func prepareCrewMeshSummary(crew string, members []crewMember, opts crewConnectOptions) (crewMeshSummary, error) {
	used := map[int]bool{}
	alloc := opts.PortAlloc
	if alloc == nil {
		alloc = allocateLocalForwardPort
	}
	forwards := []crewMeshForward{}
	for _, member := range members {
		for _, port := range member.Ports {
			localPort, err := alloc(used)
			if err != nil {
				return crewMeshSummary{}, err
			}
			used[localPort] = true
			forwards = append(forwards, crewMeshForward{
				Peer:       member.Name,
				RemotePort: port,
				LocalPort:  localPort,
				LeaseID:    member.Lease,
			})
		}
	}
	if len(forwards) == 0 {
		return crewMeshSummary{}, nil
	}
	hostsPath, envPath, err := crewMeshHostsAndEnvPaths(opts.HomeDir, crew)
	if err != nil {
		return crewMeshSummary{}, err
	}
	hostsBody := renderCrewMeshHostsFile(forwards)
	envBody, exports := renderCrewMeshEnvFile(forwards)
	if err := writeCrewMeshStateFile(hostsPath, hostsBody); err != nil {
		return crewMeshSummary{}, err
	}
	if err := writeCrewMeshStateFile(envPath, envBody); err != nil {
		return crewMeshSummary{}, err
	}
	return crewMeshSummary{HostsPath: hostsPath, EnvPath: envPath, Exports: exports, Forwards: forwards}, nil
}

// allocateLocalForwardPort walks the operator-side window looking for a free
// loopback port not already in the in-flight allocation set. It probes the
// kernel with a listen(0) bind so we never collide with an unrelated service
// the operator is already running on the same address.
func allocateLocalForwardPort(used map[int]bool) (int, error) {
	for port := crewMeshLocalPortStart; port <= crewMeshLocalPortEnd; port++ {
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
	return 0, exit(7, "no free loopback ports between %d and %d for SSH-mesh forwards", crewMeshLocalPortStart, crewMeshLocalPortEnd)
}

// crewMeshHostsAndEnvPaths returns the absolute paths to the per-crew state
// files under HOME. The parent directory is created with 0700 so the layout
// matches the rest of ~/.crabbox.
func crewMeshHostsAndEnvPaths(home, crew string) (string, string, error) {
	if home == "" {
		return "", "", exit(2, "HOME is unset; cannot write crew SSH-mesh state files")
	}
	dir := filepath.Join(home, crewMeshHostsRoot, crew)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	return filepath.Join(dir, crewMeshHostsFileName), filepath.Join(dir, crewMeshEnvFileName), nil
}

// renderCrewMeshHostsFile renders the operator-visible hosts table that maps
// `<peer>.cbx` (with `<peer>:<port>.cbx` for multi-port peers) to its assigned
// loopback port. The format mirrors /etc/hosts so an operator can paste the
// content into their resolver if they choose.
func renderCrewMeshHostsFile(forwards []crewMeshForward) string {
	var b strings.Builder
	b.WriteString("# crabbox crew SSH-mesh — operator-side forwards\n")
	b.WriteString("# Format: <local-port> <peer>.cbx\n")
	for _, fwd := range forwards {
		fmt.Fprintf(&b, "127.0.0.1:%d  %s.cbx (remote :%d)\n", fwd.LocalPort, fwd.Peer, fwd.RemotePort)
	}
	return b.String()
}

// renderCrewMeshEnvFile renders the shell-export snippet and the list of
// individual export lines used by `crew connect --export`. The snippet is
// stable across re-runs with the same forward set so `eval $(crabbox crew
// connect --export)` is safe to re-run.
func renderCrewMeshEnvFile(forwards []crewMeshForward) (string, []string) {
	exports := make([]string, 0, len(forwards))
	var b strings.Builder
	b.WriteString("# crabbox crew SSH-mesh — eval $(crabbox crew connect <name> --export)\n")
	for _, fwd := range forwards {
		line := fmt.Sprintf("export CRABBOX_CREW_%s_%d=127.0.0.1:%d", strings.ToUpper(envSafeName(fwd.Peer)), fwd.RemotePort, fwd.LocalPort)
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
	return strings.Trim(b.String(), "_")
}

// writeCrewMeshStateFile persists rendered content with 0600 permissions so
// the hosts and env files sit alongside other Crabbox per-user secrets in
// terms of disk-level posture.
func writeCrewMeshStateFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}

// crewMeshSSHArgsForForward builds the `ssh -L ...` argument vector for one
// forward. It re-uses sshBaseArgs so ControlMaster + key + port options stay
// identical to the rest of the CLI's SSH plumbing — no new transport, no new
// surface area.
func crewMeshSSHArgsForForward(target SSHTarget, fwd crewMeshForward) []string {
	args := append([]string{}, sshBaseArgs(target)...)
	forward := fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", fwd.LocalPort, fwd.RemotePort)
	args = append(args,
		"-N",
		"-L", forward,
		target.User+"@"+target.Host,
	)
	return args
}

// runCrewMeshForwards spawns one ssh -L per forward in the summary, waits for
// ctx cancellation or any process exit, and tears the rest down. The
// orchestration is deliberately simple: ControlMaster + ControlPersist (from
// sshBaseArgs) reuses a single underlying TCP connection per peer so the
// fan-out is cheap.
func runCrewMeshForwards(ctx context.Context, opts crewConnectOptions, members []crewMember, summary crewMeshSummary) error {
	runner := opts.Runner
	if runner == nil {
		runner = crewMeshDefaultRunner
	}
	peerTarget := map[string]SSHTarget{}
	for _, member := range members {
		peerTarget[member.Name] = member.SSH
	}
	type runningForward struct {
		fwd    crewMeshForward
		handle crewMeshHandle
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	running := []runningForward{}
	for _, fwd := range summary.Forwards {
		target, ok := peerTarget[fwd.Peer]
		if !ok {
			cancel()
			return exit(7, "no SSH target resolved for crew peer %q", fwd.Peer)
		}
		args := crewMeshSSHArgsForForward(target, fwd)
		handle := runner.Command(ctx, "ssh", args...)
		if err := handle.Start(); err != nil {
			cancel()
			return fmt.Errorf("start ssh -L %d:%d for %s: %w", fwd.LocalPort, fwd.RemotePort, fwd.Peer, err)
		}
		running = append(running, runningForward{fwd: fwd, handle: handle})
		fmt.Fprintf(opts.Stderr, "  -L 127.0.0.1:%d -> %s:%d\n", fwd.LocalPort, fwd.Peer, fwd.RemotePort)
	}
	var wg sync.WaitGroup
	var firstErr error
	var firstErrOnce sync.Once
	for _, rf := range running {
		wg.Add(1)
		go func(rf runningForward) {
			defer wg.Done()
			err := rf.handle.Wait()
			if err != nil && !errors.Is(err, context.Canceled) {
				firstErrOnce.Do(func() { firstErr = err })
			}
			cancel()
		}(rf)
	}
	<-ctx.Done()
	for _, rf := range running {
		if proc := rf.handle.Process(); proc != nil {
			_ = proc.Signal(os.Interrupt)
		}
	}
	wg.Wait()
	if firstErr != nil && ctx.Err() == nil {
		return firstErr
	}
	return nil
}

// crewMeshDoctorCounts inspects a slice of servers (already filtered by
// crew) and returns the per-plane summary doctor surfaces. The function is
// intentionally pure: no network, no SSH, no provider calls. doctor invokes
// it from doctorCrewMeshSummary so the test suite exercises every branch
// without spawning subprocesses.
func crewMeshDoctorCounts(servers []Server) (memberCount, exposedCount, totalPorts int) {
	for _, server := range servers {
		memberCount++
		ports := parseExposedPortsLabel(server.Labels[crewExposedPortsLabelKey])
		if len(ports) > 0 {
			exposedCount++
			totalPorts += len(ports)
		}
	}
	return memberCount, exposedCount, totalPorts
}
