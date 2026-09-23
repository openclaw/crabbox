package nvidiabrev

import (
	"encoding/json"
	"flag"
	"io"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestNvidiaBrevRetainedTargetAcrossProcesses(t *testing.T) {
	_, home := isolateNvidiaBrevState(t)
	restoreID := stubNvidiaBrevLeaseID("cbx_123456789abc")
	defer restoreID()
	restoreWait := stubNvidiaBrevWaitForSSH(t, nil)
	defer restoreWait()
	name := brevProviderName("cbx_123456789abc", "target")
	key := filepath.Join(home, ".brev", "brev.pem")
	writeBrevSSHConfig(t, home, "Host "+name+"\n HostName 192.0.2.10\n User brev\n IdentityFile "+key+"\n IdentitiesOnly yes\nHost "+name+"-host\n HostName 192.0.2.11\n User ubuntu\n IdentityFile "+key+"\n IdentitiesOnly yes\n")
	workspace := brevWorkspace{ID: "ws-target", Name: name, Status: "RUNNING", BuildStatus: "READY", ShellStatus: "READY", HealthStatus: "HEALTHY"}
	created := false
	runner := &fakeRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if req.Name == "ssh" && len(req.Args) > 0 && req.Args[0] == "-G" {
			// Model the effective fixture config if the provider uses native OpenSSH.
			user := "brev"
			if strings.HasSuffix(req.Args[len(req.Args)-1], "-host") {
				user = "ubuntu"
			}
			return core.LocalCommandResult{Stdout: "user " + user + "\nport 22\nhostname routed.example.test\nidentitiesonly yes\nuserknownhostsfile /dev/null\n"}, nil
		}
		switch req.Args[0] {
		case "ls":
			workspaces := []brevWorkspace{}
			if created {
				workspaces = append(workspaces, workspace)
			}
			data, err := json.Marshal(map[string]any{"workspaces": workspaces})
			return core.LocalCommandResult{Stdout: string(data)}, err
		case "create":
			created = true
		case "refresh":
		case "stop":
			workspace.Status = "STOPPED"
		case "start":
			workspace.Status = "RUNNING"
		default:
			t.Fatalf("unexpected command %s %v", req.Name, req.Args)
		}
		return core.LocalCommandResult{}, nil
	}}
	backend := func(cfg core.Config) *nvidiaBrevBackend {
		return NewNvidiaBrevBackend(Provider{}.Spec(), cfg, core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}).(*nvidiaBrevBackend)
	}
	repo := core.Repo{Root: t.TempDir()}
	lease, err := backend(core.Config{NvidiaBrev: core.NvidiaBrevConfig{Target: "host", ReleaseAction: "stop"}}).Acquire(t.Context(), core.AcquireRequest{Repo: repo, RequestedSlug: "target"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.User != "ubuntu" {
		t.Fatalf("acquire selected wrong target: %#v", lease.SSH)
	}
	assertTarget := func(want string) {
		t.Helper()
		claim, ok, err := resolveLeaseClaimForProvider(lease.LeaseID)
		if err != nil || !ok || claim.Labels["brev_target"] != want {
			t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
		}
	}
	for _, req := range []core.ResolveRequest{
		{ID: lease.LeaseID, StatusOnly: true},
		{ID: lease.LeaseID, StatusOnly: true, ReadyProbe: true},
		{ID: lease.LeaseID, Repo: repo},
	} {
		got, err := backend(core.Config{}).Resolve(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got.Server.Labels["brev_target"] != "host" {
			t.Fatalf("resolve lost target: %#v", got.Server.Labels)
		}
		if got.SSH.User != "ubuntu" {
			t.Fatalf("resolved container instead of host: %#v", got.SSH)
		}
		assertTarget("host")
		lease = got
	}
	list, err := backend(core.Config{}).List(t.Context(), core.ListRequest{})
	if err != nil || len(list) != 1 || list[0].Labels["brev_target"] != "host" {
		t.Fatalf("list=%#v err=%v", list, err)
	}
	delete(lease.Server.Labels, "brev_target")
	if _, err := backend(core.Config{}).Touch(t.Context(), core.TouchRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	assertTarget("host")
	if err := backend(core.Config{}).ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	assertTarget("host")
	resumed, err := backend(core.Config{}).Resolve(t.Context(), core.ResolveRequest{ID: lease.LeaseID, Repo: repo})
	if err != nil || resumed.SSH.User != "ubuntu" {
		t.Fatalf("resumed=%#v err=%v", resumed.SSH, err)
	}
	assertTarget("host")
	cfg := core.Config{Provider: providerName}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterNvidiaBrevProviderFlags(fs, cfg)
	if err := fs.Parse([]string{"--nvidia-brev-target=container"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyNvidiaBrevProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	switched, err := backend(cfg).Resolve(t.Context(), core.ResolveRequest{ID: lease.LeaseID, Repo: repo})
	if err != nil || switched.SSH.User != "brev" {
		t.Fatalf("switched=%#v err=%v", switched.SSH, err)
	}
	assertTarget("container")
	args := (Provider{}).CommandRouting(cfg, core.CommandRoutingRequest{}).Args
	if !strings.Contains(strings.Join(args, " "), "--nvidia-brev-target container") {
		t.Fatalf("explicit default lost from follow-up command: %v", args)
	}
}
