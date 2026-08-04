package runcloud

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestAcquireReturnsProxySSHTargetAndReleaseDeletesSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg: Config{
			Provider: providerName, TargetOS: targetLinux, Network: networkPublic,
			WorkRoot: "/home/runcloud/crabbox",
			RunCloud: RunCloudConfig{CLIPath: "/opt/run cloud", Image: "runcloud/agent-base", Workdir: "/home/runcloud/crabbox"},
		},
		rt:     Runtime{Stdout: io.Discard, Stderr: io.Discard},
		client: fake,
	}
	originalKey := ensureTestboxKeyFunc
	originalWait := waitForSSHReadyFunc
	ensureTestboxKeyFunc = func(string) (string, string, error) {
		return "/tmp/crabbox key", "ssh-ed25519 AAAATEST crabbox", nil
	}
	waitForSSHReadyFunc = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error { return nil }
	t.Cleanup(func() {
		ensureTestboxKeyFunc = originalKey
		waitForSSHReadyFunc = originalWait
	})

	repoRoot := t.TempDir()
	observed := LeaseTarget{}
	lease, err := b.Acquire(context.Background(), AcquireRequest{
		Repo: core.Repo{Root: repoRoot}, Keep: true,
		OnAcquired: func(target LeaseTarget) error {
			observed = target
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "sbx_1" || lease.SSH.User != "runcloud" || lease.SSH.Host != "sbx_1" || !lease.SSH.SSHConfigProxy {
		t.Fatalf("lease=%#v", lease)
	}
	if !strings.HasPrefix(observed.LeaseID, "cbx_") || observed.Server.CloudID != "sbx_1" {
		t.Fatalf("OnAcquired=%#v", observed)
	}
	if lease.SSH.ProxyCommand != "'/opt/run cloud' sandbox proxy 'sbx_1'" {
		t.Fatalf("proxy=%q", lease.SSH.ProxyCommand)
	}
	if lease.SSH.Key != "/tmp/crabbox key" || fake.installedKey == "" {
		t.Fatalf("key=%q installed=%q", lease.SSH.Key, fake.installedKey)
	}
	resolved, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, Repo: core.Repo{Root: repoRoot}})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.LeaseID != lease.LeaseID || resolved.Server.CloudID != "sbx_1" {
		t.Fatalf("resolved=%#v", resolved)
	}
	listed, err := b.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].CloudID != "sbx_1" {
		t.Fatalf("listed=%#v", listed)
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.deleted, []string{"sbx_1"}) {
		t.Fatalf("deleted=%v", fake.deleted)
	}
}
