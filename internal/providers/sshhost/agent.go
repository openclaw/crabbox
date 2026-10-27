package sshhost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
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
	"github.com/openclaw/crabbox/internal/providers/serve"
	"golang.org/x/crypto/ssh"
)

type hostAssignment struct {
	LeaseID          string `json:"leaseID"`
	HostID           string `json:"hostID"`
	Generation       int    `json:"generation"`
	AccessGeneration int    `json:"accessGeneration"`
	PublicKey        string `json:"publicKey"`
	Operation        string `json:"operation"`
	Deadline         int64  `json:"deadline"`
	Target           string `json:"target"`
}
type hostReport struct {
	LeaseID          string `json:"leaseID"`
	Generation       int    `json:"generation"`
	AccessGeneration int    `json:"accessGeneration"`
	State            string `json:"state"`
	SSHHostKey       string `json:"sshHostKey,omitempty"`
	SSHUser          string `json:"sshUser,omitempty"`
	WorkRoot         string `json:"workRoot,omitempty"`
}
type hostState struct {
	FrontDoorKey string          `json:"frontDoorKey"`
	ID           string          `json:"id"`
	Coordinator  string          `json:"coordinator"`
	Assignment   *hostAssignment `json:"assignment,omitempty"`
	Runtime      *runtimeState   `json:"runtime,omitempty"`
	Report       *hostReport     `json:"report,omitempty"`
}
type runtimeState struct {
	User              string `json:"user,omitempty"`
	Marker            string `json:"marker,omitempty"`
	Home              string `json:"home,omitempty"`
	UID               string `json:"uid,omitempty"`
	GID               string `json:"gid,omitempty"`
	Port              int    `json:"port,omitempty"`
	HostKey           string `json:"hostKey,omitempty"`
	Endpoint          string `json:"endpoint,omitempty"`
	SandboxID         string `json:"sandboxID,omitempty"`
	CorrelationVector string `json:"correlationVector,omitempty"`
}
type relayAssignment struct {
	SessionID  string `json:"sessionID"`
	LeaseID    string `json:"leaseID"`
	AgentToken string `json:"agentToken"`
}
type hostHeartbeat struct {
	Assignment *hostAssignment   `json:"assignment"`
	Relays     []relayAssignment `json:"relays"`
	ServerTime int64             `json:"serverTime"`
}
type hostAgent struct {
	coord  *core.CoordinatorClient
	state  hostState
	path   string
	front  *serve.HostFrontDoor
	stderr interface{ Write([]byte) (int, error) }
	mxc    string
	mu     sync.Mutex
	relays map[string]bool
}

func (Provider) RunAgent(ctx context.Context, cfg core.Config, rt core.Runtime, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: crabbox host enroll|list|enable|drain|remove|serve")
	}
	action := args[0]
	fs := flag.NewFlagSet("host "+action, flag.ContinueOnError)
	fs.SetOutput(rt.Stderr)
	defaultDir := "/var/lib/crabbox-host"
	if runtime.GOOS == "darwin" {
		defaultDir = "/Library/Application Support/crabbox-host"
	}
	if runtime.GOOS == "windows" {
		defaultDir = filepath.Join(os.Getenv("LOCALAPPDATA"), "crabbox-host")
	}
	dir := fs.String("state-dir", defaultDir, "Private operator-owned host state, outside repositories")
	id := fs.String("id", "", "Host UUID for inventory operations")
	name := fs.String("name", "", "Inventory display name")
	template := fs.String("template", "", "Operator-controlled environment revision")
	capabilities := fs.String("capabilities", "", "Comma-separated advertised capabilities")
	mxc := fs.String("mxc-executable", "", "Absolute pinned MXC 0.8.0 executor (Windows interactive desktop only)")
	endpoint := fs.String("endpoint-state", "", "Internal isolated endpoint configuration")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected host arguments")
	}
	if action == "endpoint" {
		if *endpoint == "" {
			return fmt.Errorf("endpoint state required")
		}
		return runEndpoint(ctx, *endpoint)
	}
	c, err := core.NewCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if action == "list" {
		var out json.RawMessage
		err = c.AgentRequest(ctx, "GET", "/v1/ssh-hosts", nil, &out)
		if err == nil {
			_, err = fmt.Fprintln(rt.Stdout, string(out))
		}
		return err
	}
	if action == "enable" || action == "disable" || action == "drain" || action == "resume" || action == "remove" {
		if *id == "" {
			return fmt.Errorf("--id is required")
		}
		method := "PATCH"
		body := map[string]bool{}
		switch action {
		case "enable":
			body["enabled"] = true
		case "disable":
			body["enabled"] = false
		case "drain":
			body["draining"] = true
		case "resume":
			body["draining"] = false
		case "remove":
			method = "DELETE"
		}
		var out json.RawMessage
		err = c.AgentRequest(ctx, method, "/v1/ssh-hosts/"+*id, body, &out)
		if err == nil {
			_, err = fmt.Fprintln(rt.Stdout, string(out))
		}
		return err
	}
	if action != "enroll" && action != "serve" {
		return fmt.Errorf("unknown host action %q", action)
	}
	if !filepath.IsAbs(*dir) {
		return fmt.Errorf("host state directory must be absolute")
	}
	if err = secureStateDir(*dir); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(*dir, "agent.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("host agent already owns this state")
	}
	defer lock.Close()
	a := &hostAgent{coord: c, path: filepath.Join(*dir, "state.json"), stderr: rt.Stderr, mxc: *mxc, relays: map[string]bool{}}
	data, err := os.ReadFile(a.path)
	if err == nil {
		if err = json.Unmarshal(data, &a.state); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		if action != "enroll" {
			return fmt.Errorf("enroll this host first")
		}
		var b [16]byte
		if _, err = rand.Read(b[:]); err != nil {
			return err
		}
		b[6] = (b[6] & 15) | 64
		b[8] = (b[8] & 63) | 128
		h := hex.EncodeToString(b[:])
		a.state.ID = h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
		a.state.Coordinator = c.BaseURL
		if err = a.save(); err != nil {
			return err
		}
	}
	if a.state.Coordinator != c.BaseURL {
		return fmt.Errorf("host state is bound to a different coordinator")
	}
	if action == "enroll" {
		target := runtime.GOOS
		if target == "darwin" {
			target = "macos"
		}
		kind := "native-user"
		if target == "windows" {
			kind = "mxc-isolation-session"
		}
		if *name == "" || *template == "" {
			return fmt.Errorf("--name and --template are required")
		}
		caps := []string{}
		for _, v := range strings.Split(*capabilities, ",") {
			if v = strings.TrimSpace(v); v != "" {
				caps = append(caps, v)
			}
		}
		var out json.RawMessage
		err = c.AgentRequest(ctx, "POST", "/v1/ssh-hosts", map[string]any{"id": a.state.ID, "name": *name, "target": target, "architecture": runtime.GOARCH, "runtime": kind, "template": *template, "capabilities": caps}, &out)
		if err == nil {
			_, err = fmt.Fprintln(rt.Stdout, string(out))
		}
		return err
	}
	if err = validateRuntime(a.mxc); err != nil {
		return err
	}
	releaseMachine, err := lockMachine()
	if err != nil {
		return err
	}
	defer releaseMachine()
	if a.state.FrontDoorKey == "" {
		var public string
		a.state.FrontDoorKey, public, err = generateHostKey()
		_ = public
		if err != nil {
			return err
		}
		if err = a.save(); err != nil {
			return err
		}
	}
	signer, err := ssh.ParsePrivateKey([]byte(a.state.FrontDoorKey))
	if err != nil {
		return err
	}
	a.front = serve.NewHostFrontDoor(signer)
	defer a.front.Close()
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// A restart always revokes and settles the retained generation before asking
	// for new work. Losing local custody is not interpreted as an empty host.
	if a.state.Assignment != nil {
		if err = a.cleanup(ctx); err != nil {
			return fmt.Errorf("host restart cleanup pending: %w", err)
		}
		a.state.Report = a.report("failed")
		if err = a.save(); err != nil {
			return err
		}
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err = a.tick(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintf(rt.Stderr, "managed host reconciliation failed; allocation retained: %v\n", err)
		}
		select {
		case <-ctx.Done():
			a.front.Revoke()
			cleanupCtx, stop := context.WithTimeout(context.Background(), 45*time.Second)
			err = a.cleanup(cleanupCtx)
			stop()
			return err
		case <-ticker.C:
		}
	}
}
func (a *hostAgent) save() error { return writePrivateJSON(a.path, a.state) }
func (a *hostAgent) report(state string) *hostReport {
	l := a.state.Assignment
	if l == nil {
		return nil
	}
	r := &hostReport{LeaseID: l.LeaseID, Generation: l.Generation, AccessGeneration: l.AccessGeneration, State: state}
	if state == "ready" && a.state.Runtime != nil {
		r.SSHHostKey = a.state.Runtime.HostKey
		r.SSHUser = "crabbox"
		r.WorkRoot = filepath.Join(a.state.Runtime.Home, "work")
	}
	return r
}
func (a *hostAgent) cleanup(ctx context.Context) error {
	if a.front != nil {
		a.front.Revoke()
	}
	if a.state.Runtime != nil {
		if err := cleanupRuntime(ctx, a.state.Runtime, a.mxc); err != nil {
			a.state.Report = a.report("failed")
			_ = a.save()
			return err
		}
	}
	a.state.Runtime = nil
	a.state.Report = a.report("clean")
	return a.save()
}
func (a *hostAgent) tick(ctx context.Context) error {
	if l := a.state.Assignment; l != nil && l.Deadline <= time.Now().UnixMilli() && a.state.Runtime != nil {
		if err := a.cleanup(ctx); err != nil {
			return err
		}
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var response hostHeartbeat
	if err := a.coord.AgentRequest(bounded, "POST", "/v1/ssh-hosts/"+a.state.ID+"/heartbeat", map[string]any{"hostKey": strings.TrimSpace(a.front.HostKey()), "report": a.state.Report}, &response); err != nil {
		return err
	}
	next := response.Assignment
	if next == nil {
		if a.state.Runtime != nil {
			if err := a.cleanup(ctx); err != nil {
				return err
			}
		}
		a.state.Assignment = nil
		a.state.Report = nil
		return a.save()
	}
	if next.HostID != a.state.ID || next.LeaseID == "" || next.Generation < 1 || next.Deadline <= 0 {
		return fmt.Errorf("invalid host assignment")
	}
	prior := a.state.Assignment
	changed := prior == nil || prior.LeaseID != next.LeaseID || prior.Generation != next.Generation || prior.AccessGeneration != next.AccessGeneration
	if changed && a.state.Runtime != nil {
		if err := a.cleanup(ctx); err != nil {
			return err
		}
	}
	a.state.Assignment = next
	if changed {
		a.state.Report = nil
	}
	if err := a.save(); err != nil {
		return err
	}
	if next.Operation == "clean" || next.Deadline <= time.Now().UnixMilli() {
		return a.cleanup(ctx)
	}
	if next.Operation == "idle" {
		a.front.Revoke()
		return nil
	}
	if next.Operation != "prepare" {
		return fmt.Errorf("unknown host operation")
	}
	if a.state.Runtime == nil {
		record, err := newRuntimeState(a.state.ID, *next)
		if err != nil {
			return err
		}
		a.state.Runtime = record
		if err = a.save(); err != nil {
			return err
		}
		if err = prepareRuntime(ctx, record, *next, a.mxc, a.save); err == nil {
			err = waitEndpoint(ctx, record)
		}
		if err != nil {
			a.state.Report = a.report("failed")
			_ = a.save()
			return err
		}
	}
	if err := refreshEndpoint(a.state.Runtime, next.Deadline); err != nil {
		return err
	}
	if err := a.front.Set(next.LeaseID, next.PublicKey, strconv.Itoa(a.state.Runtime.Port), time.UnixMilli(next.Deadline)); err != nil {
		return err
	}
	a.state.Report = a.report("ready")
	if err := a.save(); err != nil {
		return err
	}
	for _, relay := range response.Relays {
		if relay.LeaseID != next.LeaseID {
			continue
		}
		a.mu.Lock()
		if a.relays[relay.SessionID] || len(a.relays) >= 4 {
			a.mu.Unlock()
			continue
		}
		a.relays[relay.SessionID] = true
		a.mu.Unlock()
		go func(r relayAssignment) {
			defer func() { a.mu.Lock(); delete(a.relays, r.SessionID); a.mu.Unlock() }()
			conn, err := core.DialServeRelay(ctx, a.coord, r.SessionID, "agent", r.AgentToken, next.HostID)
			if err == nil {
				a.front.Admit(conn, r.LeaseID)
			}
		}(relay)
	}
	return nil
}
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
func refreshEndpoint(r *runtimeState, deadline int64) error {
	c, err := readEndpoint(r.Endpoint)
	if err != nil {
		return err
	}
	c.Deadline = deadline
	return writeEndpoint(r, c)
}
