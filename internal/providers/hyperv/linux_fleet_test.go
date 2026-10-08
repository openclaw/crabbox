package hyperv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
	"gopkg.in/yaml.v3"
)

func assertPublicClaimTarget(t *testing.T, id, target, mode, stage string) {
	t.Helper()
	var stdout bytes.Buffer
	app := core.App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.Run(context.Background(), []string{"claims", "list", "--json"}); err != nil {
		t.Fatalf("%s: public claims command failed: %v", stage, err)
	}
	var snapshot struct {
		Version int `json:"version"`
		Claims  []struct {
			LeaseID     string `json:"leaseId"`
			TargetOS    string `json:"targetOS"`
			WindowsMode string `json:"windowsMode"`
		} `json:"claims"`
		Problems []json.RawMessage `json:"problems"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != 1 || len(snapshot.Problems) != 0 {
		t.Fatalf("%s: invalid public claim envelope", stage)
	}
	for _, claim := range snapshot.Claims {
		if claim.LeaseID == id {
			if claim.TargetOS != target || claim.WindowsMode != mode {
				t.Fatalf("%s: public target=%q mode=%q, want %q/%q", stage, claim.TargetOS, claim.WindowsMode, target, mode)
			}
			return
		}
	}
	t.Fatalf("%s: public claim is missing", stage)
}

type linuxFixture struct {
	b       *backend
	repo    core.Repo
	vm      hypervVM
	exists  bool
	removed int
}

func newLinuxFixture(t *testing.T) *linuxFixture {
	t.Helper()
	testutil.IsolateUserDirs(t)
	oldOS := hypervHostOS
	hypervHostOS = "windows"
	t.Cleanup(func() { hypervHostOS = oldOS })
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = targetLinux
	core.MarkTargetExplicit(&cfg)
	cfg.HyperV = core.HyperVConfig{
		Image: filepath.Join(t.TempDir(), "template.vhdx"),
	}
	flags := flag.NewFlagSet("linux-fixture", flag.ContinueOnError)
	values := registerFlags(flags, cfg)
	if err := flags.Parse([]string{"--hyperv-user=alice", "--hyperv-work-root=/home/alice/bench-fleet/attempt", "--hyperv-cpu=2", "--hyperv-memory=2048"}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, flags, values); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.HyperV.Image, []byte("fixture parent: never a real VHDX"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &linuxFixture{repo: core.Repo{Root: t.TempDir()}}
	f.b = newBackend(Provider{}.Spec(), cfg, core.Runtime{Exec: &recordingRunner{}, Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	f.b.linuxHost = &linuxHostOperations{
		seed: func(ctx context.Context, seed, userData, metadata string) error {
			claims, err := core.ListLeaseClaims()
			if err != nil || len(claims) != 1 {
				t.Fatalf("ownership not persisted before seed: claims=%d err=%v", len(claims), err)
			}
			if claims[0].Labels["target"] != targetLinux || claims[0].Labels["state"] != "provisioning" {
				t.Fatalf("incomplete provisional identity: %+v", claims[0].Labels)
			}
			assertPublicClaimTarget(t, claims[0].LeaseID, targetLinux, "", "provisioning-before-seed")
			var cloudConfig struct {
				Users []struct {
					Name string   `yaml:"name"`
					Keys []string `yaml:"ssh_authorized_keys"`
				} `yaml:"users"`
			}
			if err := yaml.Unmarshal([]byte(userData), &cloudConfig); err != nil {
				t.Fatal(err)
			}
			keyPath, err := core.TestboxKeyPath(claims[0].LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			publicKey, err := core.PublicKeyFor(keyPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(cloudConfig.Users) != 1 || cloudConfig.Users[0].Name != "alice" ||
				len(cloudConfig.Users[0].Keys) != 1 || cloudConfig.Users[0].Keys[0] != publicKey {
				t.Fatal("--hyperv-user and its per-lease authorized key did not reach NoCloud user-data")
			}
			if !strings.HasPrefix(userData, "#cloud-config\n") || metadata != cloudInitMetaData(instanceNameFromClaim(claims[0])) {
				t.Fatal("invalid generated NoCloud documents")
			}
			return os.WriteFile(seed, []byte(metadata), 0o600)
		},
		create: func(ctx context.Context, cfg core.Config, name string) (hypervVM, error) {
			f.vm = hypervVM{Name: name, State: 3, ID: "f72621ca-fb8c-4d75-a7af-18fac5046fda"}
			f.exists = true
			return f.vm, os.WriteFile(linuxDiskPath(name), []byte("owned child"), 0o600)
		},
		start: func(ctx context.Context, name, id string) error {
			claims, err := core.ListLeaseClaims()
			if err != nil || len(claims) != 1 || claims[0].CloudImmutableID != id || claims[0].Labels["state"] != "provisioning" {
				t.Fatalf("VM identity not persisted before start: count=%d err=%v", len(claims), err)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("unbounded acquisition")
			}
			assertPublicClaimTarget(t, claims[0].LeaseID, targetLinux, "", "provisioning-before-start")
			f.vm.State = 2
			return nil
		},
		ip: func(context.Context, string, time.Duration) (string, error) { return "192.0.2.45", nil },
		detach: func(ctx context.Context, name, id string) error {
			if !f.exists || id != f.vm.ID {
				return errors.New("changed VM")
			}
			return os.Remove(ownedCloudInitSeedPath(name))
		},
		remove: func(ctx context.Context, name, id string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if f.exists {
				if name != f.vm.Name || id == "" || id != f.vm.ID {
					return errors.New("missing or conflicting immutable identity")
				}
				f.exists = false
				f.removed++
			}
			return nil
		},
		query: func(ctx context.Context, name string) (hypervVM, error) {
			if !f.exists {
				return hypervVM{Name: name, State: hypervMissingState}, nil
			}
			return f.vm, nil
		},
	}
	f.b.sshReady = func(ctx context.Context, target *core.SSHTarget, out io.Writer, phase string, timeout time.Duration) error {
		if target.TargetOS != targetLinux || target.User != "alice" || target.ReadyCheck == "" || target.DisableHostKeyChecking {
			t.Fatalf("incorrect Linux SSH readiness target: target=%s user=%s", target.TargetOS, target.User)
		}
		return nil
	}
	return f
}

func (f *linuxFixture) acquire(keep bool) (core.LeaseTarget, error) {
	return f.b.Acquire(context.Background(), core.AcquireRequest{Repo: f.repo, RequestedSlug: "linux-fixture", Keep: keep})
}

func TestLinuxKeptBootstrapFailureScrubsTailscaleSeed(t *testing.T) {
	f := newLinuxFixture(t)
	f.b.cfg.Tailscale.Enabled = true
	f.b.cfg.Tailscale.AuthKey = "fixture-only-invalid-value"
	loggedOut := false
	f.b.logoutTailscale = func(context.Context, core.SSHTarget) (string, error) {
		loggedOut = true
		return "", nil
	}
	f.b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return errors.New("SSH bootstrap failed")
	}
	if _, err := f.acquire(true); err == nil || !strings.Contains(err.Error(), "SSH bootstrap failed") {
		t.Fatalf("Acquire error=%v", err)
	}
	claims, err := core.ListLeaseClaims()
	if err != nil || len(claims) != 1 || !f.exists || f.removed != 0 {
		t.Fatalf("kept failure lost recovery state: claims=%d exists=%v removed=%d err=%v", len(claims), f.exists, f.removed, err)
	}
	if !loggedOut {
		t.Fatal("failed Tailscale bootstrap did not attempt logout")
	}
	if _, err := os.Stat(ownedCloudInitSeedPath(f.vm.Name)); !os.IsNotExist(err) {
		t.Fatalf("credential-bearing seed survived failed bootstrap: %v", err)
	}
}

func TestLinuxLeaseAcquisitionReuseAndOwnedRelease(t *testing.T) {
	f := newLinuxFixture(t)
	lease, err := f.acquire(true)
	if err != nil {
		t.Fatal(err)
	}
	assertPublicClaimTarget(t, lease.LeaseID, targetLinux, "", "ready")
	if lease.SSH.TargetOS != targetLinux || lease.Server.Status != "ready" || lease.Server.Labels["work_root"] != f.b.cfg.HyperV.WorkRoot {
		t.Fatalf("lease not ready for Linux: %+v", lease.Server)
	}
	if _, err := os.Stat(ownedCloudInitSeedPath(f.vm.Name)); !os.IsNotExist(err) {
		t.Fatalf("seed not removed after readiness: %v", err)
	}
	f.b.cfg.TargetOS = targetWindows
	f.b.cfg.HyperV.User, f.b.cfg.HyperV.WorkRoot = "wrong-default", `C:\wrong`
	reused, err := f.b.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID, Repo: f.repo})
	if err != nil {
		t.Fatal(err)
	}
	if reused.SSH.TargetOS != targetLinux || reused.SSH.User != "alice" || reused.Server.Labels["work_root"] != "/home/alice/bench-fleet/attempt" {
		t.Fatalf("stored target was lost: OS=%s user=%s", reused.SSH.TargetOS, reused.SSH.User)
	}
	assertPublicClaimTarget(t, lease.LeaseID, targetLinux, "", "recovered-with-Windows-defaults")
	if err := f.b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: reused}); err != nil {
		t.Fatal(err)
	}
	claims, err := core.ListLeaseClaims()
	if err != nil || len(claims) != 0 || f.exists || f.removed != 1 {
		t.Fatalf("release not complete: claims=%d exists=%v removed=%d err=%v", len(claims), f.exists, f.removed, err)
	}
	if data, err := os.ReadFile(f.b.cfg.HyperV.Image); err != nil || string(data) != "fixture parent: never a real VHDX" {
		t.Fatal("parent template changed")
	}
	if _, err := os.Stat(linuxStoragePath(f.vm.Name)); !os.IsNotExist(err) {
		t.Fatalf("owned storage remains: %v", err)
	}
}

func TestLinuxBootstrapFailureKeepAndCleanup(t *testing.T) {
	for _, stage := range []string{"seed", "ip", "ready", "cancel"} {
		for _, keep := range []bool{false, true} {
			t.Run(stage+fmtBool(keep), func(t *testing.T) {
				f := newLinuxFixture(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				want := errors.New("injected " + stage)
				switch stage {
				case "seed":
					seed := f.b.linuxHost.seed
					f.b.linuxHost.seed = func(ctx context.Context, name, data, meta string) error {
						if err := seed(ctx, name, data, meta); err != nil {
							return err
						}
						return want
					}
				case "ip":
					f.b.linuxHost.ip = func(context.Context, string, time.Duration) (string, error) { return "", want }
				default:
					if stage == "cancel" {
						want = context.Canceled
					}
					f.b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return want }
					if stage == "cancel" {
						f.b.sshReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
							cancel()
							return ctx.Err()
						}
					}
				}
				_, err := f.b.Acquire(ctx, core.AcquireRequest{Repo: f.repo, RequestedSlug: "linux-fixture", Keep: keep})
				if !errors.Is(err, want) {
					t.Fatalf("failure not propagated: %v", err)
				}
				claims, err := core.ListLeaseClaims()
				if err != nil {
					t.Fatal(err)
				}
				if keep {
					if len(claims) != 1 || claims[0].Labels["target"] != targetLinux {
						t.Fatal("kept failure lost ownership identity")
					}
					assertPublicClaimTarget(t, claims[0].LeaseID, targetLinux, "", "failed-kept-"+stage)
					name := instanceNameFromClaim(claims[0])
					if _, err := os.Stat(ownedCloudInitSeedPath(name)); err != nil {
						t.Fatalf("kept pre-ready guest lost seed: %v", err)
					}
					if err := f.b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: claims[0].LeaseID, Server: core.Server{CloudID: name}}}); err != nil {
						t.Fatal(err)
					}
				} else if len(claims) != 0 || f.exists {
					t.Fatalf("failed unkept lease not removed: claims=%d VM=%v", len(claims), f.exists)
				}
			})
		}
	}
}

func fmtBool(value bool) string {
	if value {
		return "-keep"
	}
	return "-discard"
}

func TestLinuxAmbiguousCreationRetainsClaimAndVM(t *testing.T) {
	f := newLinuxFixture(t)
	create := f.b.linuxHost.create
	f.b.linuxHost.create = func(ctx context.Context, cfg core.Config, name string) (hypervVM, error) {
		_, err := create(ctx, cfg, name)
		return hypervVM{}, errors.Join(err, errors.New("lost creation response"))
	}
	if _, err := f.acquire(false); err == nil {
		t.Fatal("ambiguous creation succeeded")
	}
	claims, err := core.ListLeaseClaims()
	if err != nil || len(claims) != 1 || !f.exists || f.removed != 0 {
		t.Fatal("ambiguous VM was removed or ownership lost")
	}
}

func TestLinuxOwnershipConflictAndPartialCleanupAreRecoverable(t *testing.T) {
	f := newLinuxFixture(t)
	lease, err := f.acquire(true)
	if err != nil {
		t.Fatal(err)
	}
	id := f.vm.ID
	runner := f.b.rt.Exec.(*recordingRunner)
	callsBeforeRelease := len(runner.calls)
	f.vm.ID = "c3f9b306-42bf-4edc-b889-eb1d38a827f8"
	if _, err := f.b.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID}); err == nil {
		t.Fatal("resolved a replacement VM")
	}
	if err := f.b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || !f.exists {
		t.Fatal("replacement VM was not protected")
	}
	if len(runner.calls) != callsBeforeRelease {
		t.Fatal("release reached host operations before rejecting a replacement VM")
	}
	f.vm.ID = id
	foreign := filepath.Join(linuxStoragePath(f.vm.Name), "unrecognized-disk.vhdx")
	if err := os.WriteFile(foreign, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("partial cleanup reported success")
	}
	claims, err := core.ListLeaseClaims()
	if err != nil || len(claims) != 1 {
		t.Fatal("partial cleanup lost claim")
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "untouched" {
		t.Fatal("unrecognized disk was modified")
	}
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	if err := f.b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatalf("exact retry could not finish: %v", err)
	}
}

func TestLinuxValidationAndInheritedRoots(t *testing.T) {
	for _, user := range []string{"Alice", "user name", "root;cmd", "-root", "user$", strings.Repeat("a", 33)} {
		if validLinuxSSHUser(user) {
			t.Fatalf("accepted invalid user %q", user)
		}
	}
	for _, user := range []string{"alice", "_service", "build-42"} {
		if !validLinuxSSHUser(user) {
			t.Fatalf("rejected valid user %q", user)
		}
	}
	cfg := core.BaseConfig()
	cfg.TargetOS = targetLinux
	applyDefaults(&cfg)
	if cfg.WorkRoot != "/work/crabbox" || cfg.WindowsMode != core.WindowsModeNormal {
		t.Fatalf("Linux defaults: root=%q mode=%q", cfg.WorkRoot, cfg.WindowsMode)
	}
	cfg.HyperV.WorkRoot = "/work"
	applyDefaults(&cfg)
	if cfg.WorkRoot != "/work" {
		t.Fatal("explicit work root overwritten")
	}
	cfg.HyperV.Image = filepath.Join(t.TempDir(), "cloud.vhdx")
	if err := os.WriteFile(cfg.HyperV.Image, []byte("fixture image"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"/", "relative", `C:\work`, "/bad\nroot"} {
		cfg.HyperV.WorkRoot = root
		if err := validateLinuxConfig(cfg); err == nil {
			t.Fatalf("accepted unsafe root %q", root)
		}
	}
}

func TestLinuxIPDeadlineUsesInjectedClock(t *testing.T) {
	b := testBackend(&recordingRunner{})
	now := time.Unix(1, 0)
	b.now = func() time.Time { return now }
	b.sleep = func(ctx context.Context, delay time.Duration) error {
		now = now.Add(delay)
		return ctx.Err()
	}
	_, err := b.waitForIP(context.Background(), "crabbox-fixture", 6*time.Second)
	if err == nil || now != time.Unix(7, 0) {
		t.Fatalf("IP polling escaped deterministic budget: now=%s err=%v", now, err)
	}
	if b.acquireBudget+b.cleanupBudget >= 20*time.Minute || b.cleanupBudget >= 3*time.Minute {
		t.Fatal("provider budgets leave no fleet cleanup margin")
	}
}
