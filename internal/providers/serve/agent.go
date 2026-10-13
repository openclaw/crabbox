package serve

import (
	"context"
	"crypto/rand"

	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/localcontainer"
	"golang.org/x/crypto/ssh"
)

type machine struct {
	Image          string            `json:"image"`
	MachineID      string            `json:"machineID"`
	FrontDoor      frontDoorEndpoint `json:"frontDoor"`
	Name           string            `json:"name"`
	OS             string            `json:"os"`
	Arch           string            `json:"arch"`
	CPUs           int               `json:"cpus"`
	MemoryMB       int               `json:"memoryMB"`
	MaxLeases      int               `json:"maxLeases"`
	Address        string            `json:"address"`
	Runtime        string            `json:"runtime"`
	RuntimeVersion string            `json:"runtimeVersion"`
	AgentVersion   string            `json:"agentVersion"`
	Labels         map[string]string `json:"labels"`
	Load           float64           `json:"load"`
	FreeMemoryMB   int               `json:"freeMemoryMB"`
	Draining       bool              `json:"draining"`
	Online         bool              `json:"online,omitempty"`
	Leases         []leaseView       `json:"leases"`
}
type leaseView struct {
	ID    string `json:"id"`
	State string `json:"state"`
}
type assignment struct {
	ID           string    `json:"id"`
	MachineID    string    `json:"machineID"`
	CPUs         int       `json:"cpus"`
	MemoryMB     int       `json:"memoryMB"`
	SSHPublicKey string    `json:"sshPublicKey"`
	Image        string    `json:"image"`
	ExpiresAt    time.Time `json:"expiresAt"`
	IdleDeadline int64     `json:"idleDeadline"`
	State        string    `json:"state"`
}
type relayAssignment struct {
	SessionID  string `json:"sessionID"`
	LeaseID    string `json:"leaseID"`
	AgentToken string `json:"agentToken"`
}
type heartbeat struct {
	Relays      []relayAssignment `json:"relays,omitempty"`
	Assignments []assignment      `json:"assignments"`
	Stop        []string          `json:"stop"`
	ServerTime  int64             `json:"serverTime"`
}
type localState struct {
	Machine     machine               `json:"machine"`
	Coordinator string                `json:"coordinator"`
	LastSuccess time.Time             `json:"lastSuccess"`
	Assignments map[string]assignment `json:"assignments"`
}
type options struct {
	cpus                    int
	memory                  string
	maxLeases               int
	name, runtime, stateDir string
	addresses               []string
	noLAN                   bool
	sshPort                 int
	interval                time.Duration
	noCache, drain          bool
}
type agent struct {
	relayMu               sync.Mutex
	relays                map[string]bool
	lastHeartbeatReturned time.Time
	opts                  options
	state                 localState
	coord                 *core.CoordinatorClient
	containers            containerRuntime
	rt                    core.Runtime
	volumes               []core.CacheVolumeConfig
	frontDoor             *frontDoor
	detectAddresses       func() ([]addressCandidate, error)
	startPreparation      func(context.Context)
}
type containerRuntime interface {
	List(context.Context) ([]localcontainer.ServedContainer, error)
	Create(context.Context, string, string, int, int, string, []core.CacheVolumeConfig, localcontainer.ServedContainerOptions) (localcontainer.ServedContainer, error)
	Remove(context.Context, localcontainer.ServedContainer, string) error
	Refresh(context.Context, localcontainer.ServedContainer, string, int64) error
	HostKey(context.Context, localcontainer.ServedContainer) (string, error)
	Evidence(context.Context, localcontainer.ServedContainer) (*core.ImageEvidence, error)
}

func (Provider) RunAgent(ctx context.Context, cfg core.Config, rt core.Runtime, args []string) error {
	o := options{cpus: runtime.NumCPU(), memory: os.Getenv("CRABBOX_SERVE_MEMORY"), maxLeases: 8, runtime: "docker", interval: 2 * time.Second, sshPort: cfg.Serve.SSHPort}
	if o.sshPort == 0 {
		o.sshPort = 2222
	}
	if cfg.LocalContainer.Runtime != "" {
		o.runtime = cfg.LocalContainer.Runtime
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	o.stateDir = filepath.Join(dir, "crabbox", "serve")
	o.name, _ = os.Hostname()
	command := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
		if command == "cache" && len(args) > 0 && args[0] == "prune" {
			command = "cache-prune"
			args = args[1:]
		}
	}
	fs := flag.NewFlagSet("crabbox serve", flag.ContinueOnError)
	fs.SetOutput(rt.Stderr)
	fs.IntVar(&o.cpus, "cpus", o.cpus, "CPU capacity offered to trusted leases")
	fs.StringVar(&o.memory, "memory", o.memory, "Required memory offer, e.g. 48g")
	fs.IntVar(&o.maxLeases, "max-leases", o.maxLeases, "Maximum concurrent leases (1-32)")
	fs.StringVar(&o.name, "name", o.name, "Machine name")
	o.addresses = append([]string(nil), cfg.Serve.AdvertiseAddresses...)
	// The released singular environment variable remains a supported fallback.
	if len(o.addresses) == 0 && os.Getenv("CRABBOX_SERVE_ADVERTISE_ADDRESS") != "" {
		o.addresses = []string{os.Getenv("CRABBOX_SERVE_ADVERTISE_ADDRESS")}
	}
	if value, ok := os.LookupEnv("CRABBOX_SERVE_ADVERTISE_ADDRESSES"); ok {
		o.addresses = strings.Split(value, ",")
	}
	o.noLAN = cfg.Serve.NoLANCandidates
	if value, ok := os.LookupEnv("CRABBOX_SERVE_NO_LAN_CANDIDATES"); ok {
		o.noLAN, err = strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("CRABBOX_SERVE_NO_LAN_CANDIDATES must be a boolean")
		}
	}
	explicitFlags := false
	fs.Func("advertise-address", "Client-reachable IPv4 (repeatable, max 4; replaces automatic candidates)", func(value string) error {
		if !explicitFlags {
			o.addresses = nil
			explicitFlags = true
		}
		o.addresses = append(o.addresses, value)
		return nil
	})
	fs.BoolVar(&o.noLAN, "no-lan-candidates", o.noLAN, "Disable automatic private LAN addresses")
	fs.IntVar(&o.sshPort, "ssh-port", o.sshPort, "Agent SSH front-door port (1-65535)")
	fs.StringVar(&o.runtime, "runtime", o.runtime, "docker or podman")
	fs.StringVar(&o.stateDir, "state-dir", o.stateDir, "Private agent state directory")
	fs.DurationVar(&o.interval, "poll-interval", o.interval, "Coordinator polling interval (500ms-10s)")
	fs.BoolVar(&o.noCache, "no-cache-volumes", false, "Disable package-cache named volumes")
	fs.BoolVar(&o.drain, "drain-on-exit", false, "Remove owned containers on exit")
	jsonOutput := fs.Bool("json", false, "Print JSON for machines/status")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected serve arguments")
	}
	o.stateDir, err = filepath.Abs(o.stateDir)
	if err != nil {
		return err
	}
	if o.sshPort < 1 || o.sshPort > 65535 {
		return fmt.Errorf("--ssh-port must be in 1-65535")
	}
	if command == "install" || command == "uninstall" {
		return service(ctx, command, o, rt)
	}
	if command == "status" {
		if cfg.Coordinator == "" {
			return directError()
		}
		client, err := core.NewCoordinatorClient(cfg)
		if err != nil {
			return err
		}
		if err := client.AgentRequest(ctx, "GET", "/v1/serve/machines", nil, nil); err != nil {
			return err
		}
		return showStatus(o.stateDir, *jsonOutput, rt)
	}
	if cfg.Coordinator == "" && command != "cache-prune" {
		return directError()
	}
	var coord *core.CoordinatorClient
	if cfg.Coordinator != "" {
		coord, err = core.NewCoordinatorClient(cfg)
		if err != nil {
			return err
		}
	}
	if command == "machines" {
		var result struct {
			Machines []machine `json:"machines"`
		}
		if err := coord.AgentRequest(ctx, "GET", "/v1/serve/machines", nil, &result); err != nil {
			return err
		}
		if *jsonOutput {
			return json.NewEncoder(rt.Stdout).Encode(result)
		}
		for _, m := range result.Machines {
			fmt.Fprintf(rt.Stdout, "%s %s online=%t cpus=%d memoryMB=%d\n", m.MachineID, m.Name, m.Online, m.CPUs, m.MemoryMB)
		}
		return nil
	}
	if command != "run" && command != "cache-prune" {
		return fmt.Errorf("unknown serve command %q", command)
	}
	if err := os.MkdirAll(o.stateDir, 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(o.stateDir, "agent.lock"), flock.SetPermissions(0600))
	locked, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("serve agent already owns %s", o.stateDir)
	}
	defer lock.Unlock()
	state, err := readState(o.stateDir)
	if err != nil {
		return err
	}
	if state.Machine.MachineID == "" {
		id := make([]byte, 16)
		if _, err := rand.Read(id); err != nil {
			return err
		}
		id[6] = (id[6] & 15) | 64
		id[8] = (id[8] & 63) | 128
		h := hex.EncodeToString(id)
		state.Machine.MachineID = h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	}
	cfg.LocalContainer.Runtime = o.runtime
	container := localcontainer.NewServedRuntime(cfg, rt)
	volumes := cacheVolumes(state.Machine.MachineID, cfg.Cache.Volumes)
	if command == "cache-prune" {
		return container.PruneCaches(ctx, volumes)
	}
	if o.cpus < 1 || o.cpus > 4096 || o.maxLeases < 1 || o.maxLeases > 32 || o.interval < 500*time.Millisecond || o.interval > 10*time.Second {
		return fmt.Errorf("invalid serve caps or poll interval")
	}
	memory, err := parseMemory(o.memory)
	if err != nil {
		return err
	}
	if o.runtime != "docker" && o.runtime != "podman" {
		return fmt.Errorf("serve runtime must be docker or podman")
	}
	candidates, err := detectCandidates(o.addresses, o.noLAN, nil)
	if err != nil {
		return err
	}
	front, err := startFrontDoors(o.stateDir, candidates, o.sshPort, rt.Stderr)
	if err != nil {
		return err
	}
	defer front.Close()
	prepareCtx, cancelPrepare := context.WithTimeout(ctx, 10*time.Minute)
	err = container.EnsureImage(prepareCtx)
	cancelPrepare()
	if err != nil {
		return err
	}
	version, err := container.Version(ctx)
	if err != nil {
		return err
	}
	state.Machine = machine{MachineID: state.Machine.MachineID, Name: o.name, OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: o.cpus, MemoryMB: memory, MaxLeases: o.maxLeases, Address: front.candidates[0].Address, FrontDoor: frontDoorEndpoint{Relay: true, Candidates: front.candidates, Port: o.sshPort, HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(front.signer.PublicKey())))}, Runtime: o.runtime, RuntimeVersion: version, AgentVersion: core.Version(), Image: container.Image(), Labels: map[string]string{}, FreeMemoryMB: memory}
	state.Coordinator = cfg.Coordinator
	if state.Assignments == nil {
		state.Assignments = map[string]assignment{}
	}
	if o.noCache {
		volumes = nil
	}
	a := agent{opts: o, state: state, coord: coord, containers: container, rt: rt, volumes: volumes, frontDoor: front, startPreparation: container.StartPreparation}
	if err := a.save(); err != nil {
		return err
	}
	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer func() {
		stop()
		container.WaitPreparation()
	}()
	return a.run(runCtx)
}

func (a *agent) run(ctx context.Context) error {
	startedPreparation := false
	for {
		pollStart := time.Now()
		if err := a.poll(ctx); err != nil && ctx.Err() == nil {
			var fatal *frontDoorAddressError
			if errors.As(err, &fatal) {
				return err
			}
			fmt.Fprintf(a.rt.Stderr, "serve heartbeat: %v\n", err)
		}
		if !startedPreparation && a.startPreparation != nil {
			a.startPreparation(ctx)
			startedPreparation = true
		}
		next := pollStart.Add(a.opts.interval)
		if afterHeartbeat := a.lastHeartbeatReturned.Add(a.opts.interval); afterHeartbeat.After(next) {
			next = afterHeartbeat
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return a.shutdown()
		case <-timer.C:
		}
	}
}

func parseMemory(s string) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	factor := 1
	if strings.HasSuffix(s, "g") {
		factor = 1024
		s = strings.TrimSuffix(s, "g")
	} else {
		s = strings.TrimSuffix(s, "m")
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > 16*1024*1024/factor {
		return 0, fmt.Errorf("--memory is required and must be a positive MiB or GiB amount, e.g. 48g")
	}
	return n * factor, nil
}
func cacheVolumes(id string, configured []core.CacheVolumeConfig) []core.CacheVolumeConfig {
	if len(configured) == 0 {
		configured = []core.CacheVolumeConfig{{Key: "pnpm", Path: "/home/crabbox/.local/share/pnpm/store"}, {Key: "npm", Path: "/home/crabbox/.npm"}, {Key: "go-build", Path: "/home/crabbox/.cache/go-build"}, {Key: "go-mod", Path: "/home/crabbox/go/pkg/mod"}}
	}
	result := append([]core.CacheVolumeConfig(nil), configured...)
	for i := range result {
		result[i].Key = "serve-" + id + "-" + result[i].Key
	}
	return result
}
func readState(dir string) (localState, error) {
	var s localState
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}
func (a *agent) save() error {
	b, err := json.MarshalIndent(a.state, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(a.opts.stateDir, "state.json")
	if err := os.WriteFile(path+".tmp", b, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}
func deadline(l assignment) time.Time {
	d := l.ExpiresAt
	if i := time.UnixMilli(l.IdleDeadline); i.Before(d) {
		d = i
	}
	return d
}
func owned(c localcontainer.ServedContainer, id string) bool {
	return c.Labels["crabbox.serve"] == "true" && c.Labels["crabbox.serve.machine"] == id
}
func (a *agent) ownedContainers(ctx context.Context) ([]localcontainer.ServedContainer, error) {
	all, err := a.containers.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []localcontainer.ServedContainer{}
	for _, c := range all {
		if owned(c, a.state.Machine.MachineID) {
			out = append(out, c)
		}
	}
	return out, nil
}
func (a *agent) poll(ctx context.Context) error {
	if a.frontDoor != nil {
		detect := a.detectAddresses
		if detect == nil {
			detect = func() ([]addressCandidate, error) { return detectCandidates(a.opts.addresses, a.opts.noLAN, nil) }
		}
		candidates, err := detect()
		if err != nil {
			return &frontDoorAddressError{err}
		}
		bound, err := a.frontDoor.rebind(candidates, a.opts.sshPort, a.rt.Stderr)
		if err != nil {
			return err
		}
		a.state.Machine.Address = bound[0].Address
		a.state.Machine.FrontDoor.Candidates = bound
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	containers, err := a.ownedContainers(bounded)
	if err != nil {
		return err
	}
	if a.rt.Exec != nil {
		a.sampleCapacity(bounded)
	}
	a.state.Machine.Leases = []leaseView{}
	for _, c := range containers {
		l, ok := a.state.Assignments[c.LeaseID]
		if ok && !deadline(l).After(time.Now()) {
			if err := a.containers.Remove(bounded, c, a.state.Machine.MachineID); err != nil {
				return err
			}
			continue
		}
		a.state.Machine.Leases = append(a.state.Machine.Leases, leaseView{ID: c.LeaseID, State: "running"})
	}
	containers, err = a.ownedContainers(bounded)
	if err != nil {
		return err
	}
	var response heartbeat
	err = a.coord.AgentRequest(bounded, "POST", "/v1/serve/heartbeat", a.state.Machine, &response)
	a.lastHeartbeatReturned = time.Now()
	if err != nil {
		return err
	}
	a.state.LastSuccess = time.Now()
	active := map[string]assignment{}
	var imageMismatches []string
	for _, l := range response.Assignments {
		if l.MachineID != a.state.Machine.MachineID || !core.IsCanonicalLeaseID(l.ID) || l.CPUs < 1 || l.MemoryMB < 1 || l.CPUs > a.state.Machine.CPUs || l.MemoryMB > a.state.Machine.MemoryMB {
			return fmt.Errorf("invalid serve assignment")
		}
		if l.Image != a.state.Machine.Image {
			imageMismatches = append(imageMismatches, l.ID)
			continue
		}
		active[l.ID] = l
	}
	for _, id := range response.Stop {
		delete(active, id)
	}
	a.state.Assignments = active
	a.updateFrontDoor(containers)
	a.startRelays(ctx, response.Relays)
	for _, id := range imageMismatches {
		a.failed(bounded, id, "serve_image_mismatch")
	}
	if err := a.save(); err != nil {
		return err
	}
	usedCPUs, usedMem := 0, 0
	present := map[string]localcontainer.ServedContainer{}
	for _, c := range containers {
		l, ok := active[c.LeaseID]
		if !ok || !deadline(l).After(time.Now()) {
			if err := a.containers.Remove(bounded, c, a.state.Machine.MachineID); err != nil {
				return err
			}
			continue
		}
		present[c.LeaseID] = c
		usedCPUs += l.CPUs
		usedMem += l.MemoryMB
		if err := a.containers.Refresh(bounded, c, a.state.Machine.MachineID, deadline(l).Unix()); err != nil {
			return err
		}
	}
	for id, l := range active {
		if !deadline(l).After(time.Now()) {
			continue
		}
		c, exists := present[id]
		if !exists {
			if a.state.Machine.Draining {
				continue
			}
			if len(present) >= a.opts.maxLeases || usedCPUs+l.CPUs > a.opts.cpus || usedMem+l.MemoryMB > a.state.Machine.MemoryMB {
				a.failed(bounded, id, "assignment exceeds local capacity")
				continue
			}
			c, err = a.containers.Create(bounded, id, l.SSHPublicKey, l.CPUs, l.MemoryMB, l.Image, a.volumes, localcontainer.ServedContainerOptions{Deadline: deadline(l).Unix(), Labels: map[string]string{"provider": "serve", "crabbox.serve": "true", "crabbox.serve.machine": a.state.Machine.MachineID}})
			if err != nil {
				a.failed(bounded, id, err.Error())
				continue
			}
			containers = append(containers, c)
			a.updateFrontDoor(containers)
			present[id] = c
			usedCPUs += l.CPUs
			usedMem += l.MemoryMB
		}
		if l.State != "provisioning" {
			continue
		}
		key, err := a.containers.HostKey(bounded, c)
		if err != nil {
			continue
		}
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key))
		if err != nil {
			continue
		}
		evidence, err := a.containers.Evidence(bounded, c)
		if err != nil {
			return err
		}
		report := map[string]any{"imageEvidence": evidence, "machineID": a.state.Machine.MachineID, "sshHostKey": strings.TrimSpace(key), "fingerprint": ssh.FingerprintSHA256(pub)}
		if err := a.coord.AgentRequest(bounded, "POST", "/v1/serve/leases/"+id+"/ready", report, nil); err != nil {
			return err
		}
	}
	return nil
}
func (a *agent) failed(ctx context.Context, id, reason string) {
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	_ = a.coord.AgentRequest(ctx, "POST", "/v1/serve/leases/"+id+"/failed", map[string]string{"machineID": a.state.Machine.MachineID, "reason": reason}, nil)
}
func (a *agent) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if a.frontDoor != nil {
		a.frontDoor.Close()
	}
	a.state.Machine.Draining = true
	containers, err := a.ownedContainers(ctx)
	if err != nil {
		return err
	}
	a.state.Machine.Leases = []leaseView{}
	if a.opts.drain {
		for _, c := range containers {
			if err := a.containers.Remove(ctx, c, a.state.Machine.MachineID); err != nil {
				return err
			}
		}
	} else {
		for _, c := range containers {
			a.state.Machine.Leases = append(a.state.Machine.Leases, leaseView{ID: c.LeaseID, State: "running"})
		}
	}
	var response heartbeat
	_ = a.coord.AgentRequest(ctx, "POST", "/v1/serve/heartbeat", a.state.Machine, &response)
	return a.save()
}
func showStatus(dir string, asJSON bool, rt core.Runtime) error {
	s, err := readState(dir)
	if err != nil {
		return err
	}
	s.Machine.Online = !s.LastSuccess.IsZero() && time.Since(s.LastSuccess) < 30*time.Second
	if asJSON {
		return json.NewEncoder(rt.Stdout).Encode(s)
	}
	fmt.Fprintf(rt.Stdout, "machine=%s name=%s coordinator=%s coordinator_reachable=%t last_success=%s active_leases=%d cpus=%d memoryMB=%d draining=%t\n", s.Machine.MachineID, s.Machine.Name, s.Coordinator, s.Machine.Online, s.LastSuccess.Format(time.RFC3339), len(s.Assignments), s.Machine.CPUs, s.Machine.MemoryMB, s.Machine.Draining)
	return nil
}

func (a *agent) startRelays(ctx context.Context, pending []relayAssignment) {
	if a.frontDoor == nil || len(pending) > 8 {
		return
	}
	for _, session := range pending {
		l, ok := a.state.Assignments[session.LeaseID]
		if !ok || l.MachineID != a.state.Machine.MachineID || !deadline(l).After(time.Now()) || l.State != "active" {
			continue
		}
		a.relayMu.Lock()
		if a.relays == nil {
			a.relays = map[string]bool{}
		}
		if a.relays[session.SessionID] || len(a.relays) >= 8 {
			a.relayMu.Unlock()
			continue
		}
		a.relays[session.SessionID] = true
		a.relayMu.Unlock()
		go func(session relayAssignment) {
			defer func() { a.relayMu.Lock(); delete(a.relays, session.SessionID); a.relayMu.Unlock() }()
			conn, err := core.DialServeRelay(ctx, a.coord, session.SessionID, "agent", session.AgentToken, a.state.Machine.MachineID, a.rt.Stderr)
			if err != nil {
				if ctx.Err() == nil {
					fmt.Fprintln(a.rt.Stderr, "serve relay: connection failed")
				}
				return
			}
			a.frontDoor.admit(conn, session.LeaseID)
		}(session)
	}
}
