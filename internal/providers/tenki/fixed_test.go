package tenki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const testFixedTenkiID = "cbx_abcdef123456"

// A command-level fake keeps tests on the public backend boundary. Its session
// detail is independent of list output, just as it is at the provider boundary.
type fixedTenkiRunner struct {
	mu           sync.Mutex
	sessions     map[string]tenkiSession
	calls        map[string]int
	key, cert    string
	listOverride *[]tenkiSession
	hook         func(context.Context, LocalCommandRequest) (LocalCommandResult, error, bool)
	createError  error
	createOutput *string
}

func (f *fixedTenkiRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(req.Args) < 2 {
		return LocalCommandResult{}, fmt.Errorf("unexpected command %v", req.Args)
	}
	command := req.Args[1]
	f.calls[command]++
	if f.hook != nil {
		if result, err, handled := f.hook(ctx, req); handled {
			return result, err
		}
	}
	out := func(value any) (LocalCommandResult, error) {
		data, err := json.Marshal(value)
		return LocalCommandResult{Stdout: string(data)}, err
	}
	arg := func(key string) string {
		for i := 0; i+1 < len(req.Args); i++ {
			if req.Args[i] == key {
				return req.Args[i+1]
			}
		}
		return ""
	}
	switch command {
	case "create":
		id := fmt.Sprintf("session-%d", f.calls[command])
		s := tenkiSession{ID: id, Name: arg("--name"), State: "RUNNING", ProjectID: "project-owned", CPUCores: 2, MemoryMB: 4096, DiskSizeGB: 20, SourceImageRef: arg("--image"), SourceSnapshotID: arg("--snapshot"), Metadata: map[string]string{}}
		for i, value := range req.Args {
			if value == "--sticky" {
				s.Sticky = true
			}
			if value == "--metadata" && i+1 < len(req.Args) {
				k, v, _ := strings.Cut(req.Args[i+1], "=")
				s.Metadata[k] = v
			}
		}
		f.sessions[id] = s
		if f.createOutput != nil {
			return LocalCommandResult{Stdout: *f.createOutput}, f.createError
		}
		if f.createError != nil {
			return LocalCommandResult{ExitCode: 1}, f.createError
		}
		return out(tenkiSession{ID: id})
	case "list":
		if f.listOverride != nil {
			return out(*f.listOverride)
		}
		sessions := make([]tenkiSession, 0, len(f.sessions))
		for _, s := range f.sessions {
			sessions = append(sessions, s)
		}
		return out(sessions)
	case "get":
		if s, ok := f.sessions[req.Args[len(req.Args)-1]]; ok {
			return out(s)
		}
		return LocalCommandResult{ExitCode: 1}, errors.New("session not found")
	case "ssh-command":
		return out(tenkiSSHCommandOutput{SessionID: arg("--session"), User: "tenki", Host: "sandbox", Port: 22, IdentityFile: f.key, CertificateFile: f.cert, ProxyCommand: "tenki sandbox ssh-proxy"})
	case "resume", "terminate":
		id := arg("--session")
		if id == "" {
			id = req.Args[len(req.Args)-1]
		}
		s := f.sessions[id]
		s.State = "RUNNING"
		if command == "terminate" {
			s.State = "TERMINATING"
		}
		f.sessions[id] = s
		return LocalCommandResult{}, nil
	default:
		return LocalCommandResult{}, fmt.Errorf("unexpected command %v", req.Args)
	}
}

func newFixedTenkiTest(t *testing.T) (*tenkiBackend, *fixedTenkiRunner, AcquireRequest) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	f := &fixedTenkiRunner{sessions: map[string]tenkiSession{}, calls: map[string]int{}, key: filepath.Join(dir, "key"), cert: filepath.Join(dir, "cert")}
	for _, path := range []string{f.key, f.cert} {
		if err := os.WriteFile(path, []byte("fake SSH material"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	oldWait := waitForSSHReadyFunc
	waitForSSHReadyFunc = func(ctx context.Context, _ *SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		return ctx.Err()
	}
	t.Cleanup(func() { waitForSSHReadyFunc = oldWait })
	cfg := Config{Provider: tenkiProvider, TargetOS: targetLinux, Network: networkPublic, TTL: time.Hour, IdleTimeout: 30 * time.Minute, Tenki: TenkiConfig{CLIPath: "tenki", Image: "example/image:latest", CPUs: 2, MemoryMB: 4096, DiskGB: 20}}
	b := &tenkiBackend{cfg: cfg, spec: Provider{}.Spec(), rt: Runtime{Exec: f, Stdout: io.Discard, Stderr: io.Discard}, sleep: func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }, terminationAckTimeout: 10 * time.Millisecond}
	return b, f, AcquireRequest{RequestedLeaseID: testFixedTenkiID, RequestedSlug: "build-linux", Keep: true, Repo: Repo{Root: t.TempDir()}}
}

func TestTenkiFixedAcquireReplay(t *testing.T) {
	b, f, req := newFixedTenkiTest(t)
	first, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.LeaseID != req.RequestedLeaseID {
		t.Fatalf("lease ID = %s, want caller ID %s", first.LeaseID, req.RequestedLeaseID)
	}
	fresh := *b
	second, err := fresh.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if second.LeaseID != first.LeaseID || second.Server.CloudID != first.Server.CloudID || second.Server.ImmutableID != first.Server.CloudID {
		t.Fatalf("replay identity changed: first=%+v second=%+v", first, second)
	}
	if f.calls["create"] != 1 || f.calls["ssh-command"] != 2 {
		t.Fatalf("calls=%v", f.calls)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
	if err != nil || !exists || claim.FixedCreateIntent == nil || claim.CloudID != first.Server.CloudID {
		t.Fatalf("claim=%+v exists=%t err=%v", claim, exists, err)
	}
}

func TestTenkiFixedLostCreateResponseDoesNotResubmit(t *testing.T) {
	b, f, req := newFixedTenkiTest(t)
	f.createError = errors.New("connection lost after submission")
	if _, err := b.Acquire(context.Background(), req); err == nil {
		t.Fatal("expected create error")
	}
	f.createError = nil
	invisible := []tenkiSession{}
	f.listOverride = &invisible
	fresh := *b
	if _, err := fresh.Acquire(context.Background(), req); err == nil {
		t.Fatal("uncertain create was resubmitted or adopted")
	}
	if f.calls["create"] != 1 || f.calls["terminate"] != 0 {
		t.Fatalf("unsafe calls=%v", f.calls)
	}
	f.listOverride = nil
	lease, err := fresh.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != req.RequestedLeaseID || lease.Server.CloudID != "session-1" || f.calls["create"] != 1 {
		t.Fatalf("lease=%+v calls=%v", lease, f.calls)
	}
}

func TestTenkiInspectDiagnosticsResolvesSSHWithoutClaimMutation(t *testing.T) {
	for _, state := range []string{"RUNNING", "PAUSED"} {
		t.Run(state, func(t *testing.T) {
			b, f, req := newFixedTenkiTest(t)
			req.RequestedLeaseID = "" // Also exercises the existing generated-ID path.
			lease, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			s := f.sessions[lease.Server.CloudID]
			s.State = state
			f.sessions[s.ID] = s
			before, _, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			sshCalls := f.calls["ssh-command"]
			inspected, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, StatusOnly: true, NoLocalStateMutations: true, IncludeDiagnostics: true})
			if err != nil {
				t.Fatal(err)
			}
			if state == "RUNNING" {
				if inspected.SSH.Host == "" || inspected.SSH.NetworkKind != networkPublic || f.calls["ssh-command"] != sshCalls+1 {
					t.Fatalf("ready inspection has no SSH probe target: %+v calls=%v", inspected.SSH, f.calls)
				}
			} else if inspected.SSH.Host != "" || f.calls["ssh-command"] != sshCalls || f.calls["resume"] != 0 {
				t.Fatalf("paused inspection prepared SSH or resumed: %+v calls=%v", inspected, f.calls)
			}
			after, _, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("read-only inspection changed claim: err=%v", err)
			}
		})
	}
}

func cloneTenkiSession(s tenkiSession) tenkiSession { s.Metadata = maps.Clone(s.Metadata); return s }
