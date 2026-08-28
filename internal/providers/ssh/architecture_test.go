package ssh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func stubStaticArchitecture(t *testing.T) {
	t.Helper()
	old := runArchitectureProbe
	runArchitectureProbe = func(_ context.Context, target SSHTarget, _ string, _ int) (string, error) {
		_, source, _ := architectureProbe(target)
		if source == "ssh-uname" {
			return "v1|arm64|-|-|-", nil
		}
		return "v1|arm64|arm64|arm64|false", nil
	}
	t.Cleanup(func() { runArchitectureProbe = old })
}

func staticArchitectureFixture(t *testing.T, target, mode, assertion string) (*staticLeaseBackend, *bytes.Buffer, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := waitForSSH
	waitForSSH = func(ctx context.Context, target *SSHTarget, _ io.Writer) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		target.Port = "2207"
		return nil
	}
	t.Cleanup(func() { waitForSSH = old })
	stubStaticArchitecture(t)
	cfg := core.BaseConfig()
	cfg.Provider, cfg.TargetOS, cfg.WindowsMode = "ssh", target, mode
	cfg.Static.Host, cfg.Static.ID = "build.example.test", "static_architecture"
	cfg.Static.User, cfg.Static.Port = "builder", "2222"
	if assertion != "" {
		cfg.Architecture = assertion
		core.MarkArchitectureExplicit(&cfg)
	}
	var log bytes.Buffer
	b := NewStaticSSHLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stderr: &log, Clock: staticLifecycleClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}}).(*staticLeaseBackend)
	return b, &log, t.TempDir()
}

func TestStaticSSHArchitectureAliasesAndTargets(t *testing.T) {
	for _, cfg := range []Config{{TargetOS: "worker-runtime"}, {TargetOS: "windows", WindowsMode: "unsupported"}, {TargetOS: "macos", WindowsMode: "wsl2"}} {
		if (Provider{}).SupportsArchitecture(cfg, "arm64") {
			t.Fatalf("unsupported tuple admitted: target=%s mode=%s", cfg.TargetOS, cfg.WindowsMode)
		}
	}
	if (Provider{}).SupportsArchitecture(Config{TargetOS: "linux"}, "s390x") {
		t.Fatal("unsupported architecture admitted")
	}
	for _, alias := range []string{"ssh", "static", "static-ssh"} {
		for _, target := range []struct{ os, mode string }{{"linux", "normal"}, {"macos", "normal"}, {"windows", "normal"}, {"windows", "wsl2"}} {
			for _, arch := range []string{"amd64", "arm64"} {
				t.Run(alias+"/"+target.os+"/"+target.mode+"/"+arch, func(t *testing.T) {
					b, _, _ := staticArchitectureFixture(t, target.os, target.mode, arch)
					p, err := core.ProviderFor(alias)
					if err != nil {
						t.Fatal(err)
					}
					capability, ok := p.(core.ProviderArchitectureCapability)
					if !ok || !capability.SupportsArchitecture(b.Cfg, arch) {
						t.Fatalf("actual adapter rejected tuple: %T", p)
					}
					// The real CLI admission path must reach the adapter's readiness boundary.
					sentinel := errors.New("admitted to static adapter")
					waitForSSH = func(context.Context, *SSHTarget, io.Writer) error { return sentinel }
					t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
					t.Setenv("CRABBOX_PROVIDER", "ssh")
					var log bytes.Buffer
					err = (core.App{Stdout: &log, Stderr: &log}).Run(context.Background(), []string{"warmup", "--provider", alias, "--target", target.os, "--windows-mode", target.mode, "--arch", arch, "--static-host", "build.example.test"})
					if !errors.Is(err, sentinel) {
						t.Fatalf("real admission did not reach adapter: %v\n%s", err, &log)
					}
				})
			}
		}
	}
}

func TestStaticSSHArchitectureAssertions(t *testing.T) {
	cases := []struct {
		name, target, mode, assertion, output, want string
		probeErr                                    error
		fail                                        bool
	}{
		{name: "implicit arm replaces default", output: "v1|arm64|-|-|-", want: "arm64"},
		{name: "matching amd64", assertion: "amd64", output: "v1|amd64|-|-|-", want: "amd64"},
		{name: "matching arm64", assertion: "arm64", output: "v1|arm64|-|-|-", want: "arm64"},
		{name: "amd64 mismatch", assertion: "amd64", output: "v1|arm64|-|-|-", fail: true},
		{name: "arm64 mismatch", assertion: "arm64", output: "v1|amd64|-|-|-", fail: true},
		{name: "empty assertion", assertion: "amd64", fail: true},
		{name: "empty implicit", want: "unknown"},
		{name: "unsupported assertion", assertion: "arm64", output: "v1|s390x|-|-|-", fail: true},
		{name: "unsupported implicit", output: "v1|s390x|-|-|-", want: "unknown"},
		{name: "malformed assertion", assertion: "amd64", output: "amd64", fail: true},
		{name: "malformed implicit", output: "v1|amd64|-|-|-\nsecret junk", want: "unknown"},
		{name: "unavailable asserted", assertion: "arm64", output: "v1|unknown|-|-|-", fail: true},
		{name: "unavailable implicit", output: "v1|unknown|-|-|-", want: "unknown"},
		{name: "transport asserted", assertion: "arm64", probeErr: errors.New("SSH transport failure"), fail: true},
		{name: "transport implicit", probeErr: errors.New("SSH transport failure"), fail: true},
		{name: "timeout asserted", assertion: "arm64", probeErr: context.DeadlineExceeded, fail: true},
		{name: "timeout implicit", probeErr: context.DeadlineExceeded, fail: true},
		{name: "cancel asserted", assertion: "arm64", probeErr: context.Canceled, fail: true},
		{name: "cancel implicit", probeErr: context.Canceled, fail: true},
		{name: "overflow asserted", assertion: "arm64", probeErr: core.ErrSSHOutputLimit, fail: true},
		{name: "overflow implicit", output: "v1|arm64|-|-|-", probeErr: core.ErrSSHOutputLimit, want: "unknown"},
		{name: "overflow with transport cleanup failure", probeErr: errors.Join(core.ErrSSHOutputLimit, errors.New("SSH cleanup failure")), fail: true},
		{name: "native Mac", target: "macos", assertion: "arm64", output: "v1|arm64|arm64|arm64|false", want: "arm64"},
		{name: "translated Mac arm", target: "macos", assertion: "arm64", output: "v1|amd64|arm64|amd64|true", fail: true},
		{name: "translated Mac amd", target: "macos", assertion: "amd64", output: "v1|amd64|arm64|amd64|true", fail: true},
		{name: "translated Mac implicit", target: "macos", output: "v1|amd64|arm64|amd64|true", want: "amd64"},
		{name: "Mac contradiction", target: "macos", assertion: "amd64", output: "v1|amd64|arm64|amd64|false", fail: true},
		{name: "Mac Rosetta unknown", target: "macos", assertion: "arm64", output: "v1|arm64|arm64|arm64|unknown", fail: true},
		{name: "native Windows", target: "windows", mode: "normal", assertion: "arm64", output: "v1|arm64|arm64|arm64|false", want: "arm64"},
		{name: "Windows UNKNOWN ARM64 independently x64", target: "windows", mode: "normal", assertion: "arm64", output: "v1|amd64|arm64|amd64|true", fail: true},
		{name: "Windows x64 translation assertion", target: "windows", mode: "normal", assertion: "amd64", output: "v1|amd64|arm64|amd64|true", fail: true},
		{name: "Windows process unavailable", target: "windows", mode: "normal", assertion: "arm64", output: "v1|unknown|arm64|unknown|unknown", fail: true},
		{name: "Windows process unavailable implicit", target: "windows", mode: "normal", output: "v1|unknown|arm64|unknown|unknown", want: "unknown"},
		{name: "WSL arm inside amd host", target: "windows", mode: "wsl2", assertion: "arm64", output: "v1|arm64|-|-|-", want: "arm64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, mode := tc.target, tc.mode
			if target == "" {
				target = "linux"
			}
			if mode == "" {
				mode = "normal"
			}
			b, log, repo := staticArchitectureFixture(t, target, mode, tc.assertion)
			beforeCfg := b.Cfg
			runArchitectureProbe = func(ctx context.Context, target SSHTarget, command string, limit int) (string, error) {
				if target.Port != "2207" || target.FallbackPorts == nil || len(target.FallbackPorts) != 0 {
					t.Fatalf("probe did not pin resolved port: %#v", target)
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > architectureProbeTimeout {
					t.Fatal("probe is unbounded")
				}
				if limit != architectureProbeLimit {
					t.Fatal("unexpected output limit")
				}
				if target.TargetOS == "windows" && target.WindowsMode == "normal" {
					if !strings.Contains(command, "IsWow64Process2") || !strings.Contains(command, "::ProcessArchitecture") || strings.Contains(command, "::OSArchitecture") || strings.Contains(command, "$process = $native") {
						t.Fatal("Windows probe lacks independent process evidence")
					}
				} else if target.WindowsMode == "wsl2" && (strings.Contains(command, "IsWow64Process2") || !strings.Contains(command, "uname -m")) {
					t.Fatal("WSL measured Windows")
				}
				return tc.output, tc.probeErr
			}
			lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
			if (err != nil) != tc.fail {
				t.Fatalf("err=%v want failure=%t", err, tc.fail)
			}
			if !reflect.DeepEqual(b.Cfg, beforeCfg) {
				t.Fatal("observation changed requested config")
			}
			claim, exists, readErr := core.ReadLeaseClaimWithPresence(b.Cfg.Static.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.fail {
				if exists || b.acquired.LeaseID != "" {
					t.Fatal("failed assertion published claim/cache")
				}
				return
			}
			if !exists || lease.Server.Labels["architecture"] != tc.want || claim.Labels["architecture"] != tc.want {
				t.Fatalf("lease=%#v claim=%#v", lease.Server.Labels, claim.Labels)
			}
			wantType := tc.want
			if wantType == "unknown" {
				wantType = ""
			}
			if lease.Server.ServerType.Architecture != wantType {
				t.Fatalf("server type architecture=%q", lease.Server.ServerType.Architecture)
			}
			if !strings.Contains(log.String(), "architecture observed="+tc.want) || strings.Contains(log.String(), "secret junk") {
				t.Fatalf("report=%s", log)
			}
			if tc.want == "unknown" && !strings.Contains(log.String(), "warning:") {
				t.Fatal("unknown lacked warning")
			}
			if strings.Contains(tc.output, "|true") && !strings.Contains(log.String(), "translated=true") {
				t.Fatal("translation hidden")
			}
		})
	}
}

func TestStaticSSHArchitecturePreparedReuseAndHistoricalLookup(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "claimed", true: "cached"}[cached], func(t *testing.T) {
			b, log, repo := staticArchitectureFixture(t, "linux", "normal", "")
			lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
			if err != nil {
				t.Fatal(err)
			}
			initial, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if !cached {
				b = NewStaticSSHLeaseBackend(Provider{}.Spec(), b.Cfg, b.RT).(*staticLeaseBackend)
			}
			calls := 0
			runArchitectureProbe = func(context.Context, SSHTarget, string, int) (string, error) { calls++; return "v1|amd64|-|-|-", nil }
			offline, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID})
			if err != nil {
				t.Fatal(err)
			}
			views, err := b.List(context.Background(), ListRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 0 || offline.Server.ServerType.Architecture != "arm64" || views[0].ServerType.Architecture != "arm64" || !strings.Contains(log.String(), "historical=arm64") {
				t.Fatalf("offline lookup: calls=%d log=%s", calls, log)
			}
			b.Cfg.Architecture = "amd64"
			core.MarkArchitectureExplicit(&b.Cfg)
			fresh, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, Prepare: true})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || fresh.Server.ServerType.Architecture != "amd64" {
				t.Fatal("cached evidence accepted instead of fresh probe")
			}
			expected, exists, set := core.ServerLeaseClaimSnapshot(fresh.Server)
			if !exists || !set || !reflect.DeepEqual(expected, initial) {
				t.Fatal("Prepare changed the original claim snapshot")
			}
			afterPrepare, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if !reflect.DeepEqual(afterPrepare, initial) {
				t.Fatal("Prepare published evidence before its caller")
			}
			updated, err := core.ClaimLeaseTargetForRepoConfigIfUnchanged(lease.LeaseID, serverSlug(fresh.Server), b.Cfg, fresh.Server, fresh.SSH, repo, b.Cfg.IdleTimeout, false, expected, exists)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"created_at", "last_touched_at", "state", "idle_timeout_secs"} {
				if updated.Labels[key] != initial.Labels[key] {
					t.Fatalf("publication changed lifecycle label %s", key)
				}
			}
			core.SetServerLeaseClaimSnapshot(&fresh.Server, updated, true)
			touched, err := b.Touch(context.Background(), TouchRequest{Lease: fresh, State: "busy"})
			if err != nil {
				t.Fatal(err)
			}
			if touched.Labels["architecture_observed_at"] != fresh.Server.Labels["architecture_observed_at"] || touched.ServerType.Architecture != "amd64" {
				t.Fatal("Touch lost evidence")
			}
			after, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if after.Labels["architecture_endpoint"] != updated.Labels["architecture_endpoint"] {
				t.Fatal("Touch lost endpoint binding")
			}
		})
	}
}

func TestStaticSSHArchitectureFailedRefreshPreservesClaimsAndCache(t *testing.T) {
	for _, path := range []string{"acquire", "claimed", "cached"} {
		t.Run(path, func(t *testing.T) {
			b, _, repo := staticArchitectureFixture(t, "linux", "normal", "")
			lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
			if err != nil {
				t.Fatal(err)
			}
			initial, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			b.Cfg.Architecture = "amd64"
			core.MarkArchitectureExplicit(&b.Cfg)
			if path == "claimed" {
				b = NewStaticSSHLeaseBackend(Provider{}.Spec(), b.Cfg, b.RT).(*staticLeaseBackend)
			}
			if path == "acquire" {
				_, err = b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
			} else {
				_, err = b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, Prepare: true})
			}
			if err == nil {
				t.Fatal("accepted mismatch")
			}
			after, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if !reflect.DeepEqual(initial, after) {
				t.Fatal("failed assertion changed claim")
			}
			if b.acquired.LeaseID != "" && b.acquired.Server.ServerType.Architecture != "arm64" {
				t.Fatal("failed assertion changed cached evidence")
			}
		})
	}
}

func TestStaticSSHArchitectureClaimReplacementRaces(t *testing.T) {
	for _, path := range []string{"acquire", "claimed", "cached"} {
		for _, remove := range []bool{false, true} {
			t.Run(path+map[bool]string{false: "/replace", true: "/remove"}[remove], func(t *testing.T) {
				b, _, repo := staticArchitectureFixture(t, "linux", "normal", "")
				lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
				if err != nil {
					t.Fatal(err)
				}
				initial, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
				if path == "claimed" {
					b = NewStaticSSHLeaseBackend(Provider{}.Spec(), b.Cfg, b.RT).(*staticLeaseBackend)
				}
				var replacement core.LeaseClaim
				runArchitectureProbe = func(context.Context, SSHTarget, string, int) (string, error) {
					if remove {
						core.RemoveLeaseClaim(lease.LeaseID)
					} else {
						labels := make(map[string]string)
						for k, v := range initial.Labels {
							labels[k] = v
						}
						labels["concurrent-owner"] = "replacement"
						replacement, err = core.UpdateLeaseClaimLabelsIfUnchanged(lease.LeaseID, initial, labels)
						if err != nil {
							t.Fatal(err)
						}
					}
					return "v1|amd64|-|-|-", nil
				}
				if path == "acquire" {
					_, err = b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
				} else {
					_, err = b.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, Prepare: true})
				}
				if err == nil || !strings.Contains(err.Error(), "claim changed") {
					t.Fatalf("race err=%v", err)
				}
				after, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
				if err != nil || exists == remove || (!remove && !reflect.DeepEqual(after, replacement)) {
					t.Fatal("raced claim replaced or recreated")
				}
			})
		}
	}
}

func TestStaticSSHArchitectureLegacyAndEndpointOverrides(t *testing.T) {
	for _, change := range []string{"legacy", "host", "user", "port", "target", "mode", "key"} {
		t.Run(change, func(t *testing.T) {
			b, _, repo := staticArchitectureFixture(t, "linux", "normal", "")
			lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
			if err != nil {
				t.Fatal(err)
			}
			initial, _, _ := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			cfg := b.Cfg
			switch change {
			case "legacy":
				labels := make(map[string]string)
				for k, v := range initial.Labels {
					if !strings.HasPrefix(k, "architecture") {
						labels[k] = v
					}
				}
				if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(lease.LeaseID, initial, labels); err != nil {
					t.Fatal(err)
				}
			case "host":
				cfg.Static.Host = "another.example.test"
			case "user":
				cfg.Static.User = "other-user"
			case "port":
				cfg.Static.Port = "2208"
			case "target":
				cfg.TargetOS = "macos"
				core.MarkTargetExplicit(&cfg)
			case "mode":
				cfg.TargetOS = "windows"
				cfg.WindowsMode = "wsl2"
				core.MarkTargetExplicit(&cfg)
			case "key":
				cfg.SSHKey = "/fixture/other-key"
			}
			fresh := NewStaticSSHLeaseBackend(Provider{}.Spec(), cfg, b.RT).(*staticLeaseBackend)
			calls := 0
			runArchitectureProbe = func(context.Context, SSHTarget, string, int) (string, error) { calls++; return "v1|amd64|-|-|-", nil }
			offline, err := fresh.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 0 || offline.Server.ServerType.Architecture != "" || offline.Server.Labels["architecture_observed_at"] != "" {
				t.Fatal("attached old evidence to changed/legacy route")
			}
			if change == "legacy" {
				prepared, err := fresh.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, Prepare: true})
				if err != nil {
					t.Fatal(err)
				}
				if calls != 1 || prepared.Server.ServerType.Architecture != "amd64" {
					t.Fatal("legacy evidence not refreshed")
				}
			}
		})
	}
}

func TestStaticSSHArchitectureCancellationBeforePublication(t *testing.T) {
	b, _, repo := staticArchitectureFixture(t, "linux", "normal", "")
	ctx, cancel := context.WithCancel(context.Background())
	runArchitectureProbe = func(context.Context, SSHTarget, string, int) (string, error) { cancel(); return "v1|arm64|-|-|-", nil }
	if _, err := b.Acquire(ctx, AcquireRequest{Repo: core.Repo{Root: repo}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if _, exists, _ := core.ReadLeaseClaimWithPresence(b.Cfg.Static.ID); exists {
		t.Fatal("cancellation published claim")
	}
}

// WSL must select POSIX execution-environment queries, not Windows host queries.
func TestStaticSSHArchitectureProbeSelection(t *testing.T) {
	for _, tc := range []struct{ target, mode, source, scope string }{{"linux", "normal", "ssh-uname", "posix-environment"}, {"macos", "normal", "ssh-uname-sysctl", "posix-process"}, {"windows", "normal", "ssh-iswow64process2", "powershell-process"}, {"windows", "wsl2", "ssh-uname", "wsl-environment"}} {
		command, source, scope := architectureProbe(SSHTarget{TargetOS: tc.target, WindowsMode: tc.mode})
		if source != tc.source || scope != tc.scope || command == "" {
			t.Fatalf("selection %s/%s", tc.target, tc.mode)
		}
	}
}

func TestStaticSSHArchitectureResolvedTransportPreserved(t *testing.T) {
	b, _, repo := staticArchitectureFixture(t, "linux", "normal", "")
	var resolved SSHTarget
	waitForSSH = func(_ context.Context, target *SSHTarget, _ io.Writer) error {
		target.Port = "2207"
		target.Key = "/fixture/resolved-key"
		target.CertificateFile = "/fixture/certificate"
		target.KnownHostsFile = "/fixture/known-hosts"
		target.HostKeyAlias = "fixture-alias"
		target.SSHConfigProxy = true
		target.ProxyCommand = "fixture-proxy %h %p"
		target.ChildEnvDenylist = []string{"FIXTURE_DENIED"}
		target.ChildEnv = map[string]string{"FIXTURE_ALLOWED": "yes"}
		resolved = *target
		resolved.FallbackPorts = []string{}
		return nil
	}
	runArchitectureProbe = func(_ context.Context, target SSHTarget, _ string, _ int) (string, error) {
		if !reflect.DeepEqual(target, resolved) {
			t.Fatalf("probe changed resolved transport: %#v", target)
		}
		return "v1|arm64|-|-|-", nil
	}
	lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lease.SSH, resolved) {
		t.Fatal("published different endpoint than observed")
	}
}

func TestStaticSSHArchitectureFreshAcquireClaimRace(t *testing.T) {
	b, _, repo := staticArchitectureFixture(t, "linux", "normal", "")
	var replacement core.LeaseClaim
	runArchitectureProbe = func(context.Context, SSHTarget, string, int) (string, error) {
		if err := core.ClaimLeaseForRepoProvider(b.Cfg.Static.ID, "concurrent-claim", "ssh", repo, 0, false); err != nil {
			t.Fatal(err)
		}
		replacement, _, _ = core.ReadLeaseClaimWithPresence(b.Cfg.Static.ID)
		return "v1|arm64|-|-|-", nil
	}
	if _, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}}); err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("err=%v", err)
	}
	after, _, _ := core.ReadLeaseClaimWithPresence(b.Cfg.Static.ID)
	if !reflect.DeepEqual(after, replacement) {
		t.Fatal("replaced concurrently created claim")
	}
}

func TestStaticSSHArchitectureSuccessfulReacquirePreservesLifecycle(t *testing.T) {
	b, _, repo := staticArchitectureFixture(t, "linux", "normal", "")
	lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
	if err != nil {
		t.Fatal(err)
	}
	touched, err := b.Touch(context.Background(), TouchRequest{Lease: lease, State: "busy"})
	if err != nil {
		t.Fatal(err)
	}
	runArchitectureProbe = func(context.Context, SSHTarget, string, int) (string, error) { return "v1|amd64|-|-|-", nil }
	fresh, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: repo}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"created_at", "last_touched_at", "state", "keep", "idle_timeout_secs"} {
		if fresh.Server.Labels[key] != touched.Labels[key] {
			t.Fatalf("reacquire changed %s", key)
		}
	}
	if fresh.Server.ServerType.Architecture != "amd64" {
		t.Fatal("reacquire reused old evidence")
	}
}

func TestStaticSSHArchitectureRequestSources(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, env string
		flags           []string
	}{
		{name: "yaml amd64", yaml: "architecture: amd64\n"},
		{name: "environment amd64", env: "amd64"},
		{name: "flag overrides environment", env: "arm64", flags: []string{"--arch", "amd64"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _ = staticArchitectureFixture(t, "linux", "normal", "")
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(configPath, []byte("provider: ssh\nstatic:\n  host: build.example.test\n"+tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			t.Setenv("CRABBOX_PROVIDER", "ssh")
			t.Setenv("CRABBOX_ARCH", tc.env)
			var log bytes.Buffer
			args := append([]string{"warmup", "--provider", "ssh", "--static-host", "build.example.test"}, tc.flags...)
			err := (core.App{Stdout: &log, Stderr: &log}).Run(context.Background(), args)
			if err == nil || !strings.Contains(err.Error(), "architecture=amd64 assertion failed") {
				t.Fatalf("source did not assert: err=%v log=%s", err, &log)
			}
		})
	}
}

func TestStaticSSHArchitectureLegacyConfigMigration(t *testing.T) {
	_, _, repo := staticArchitectureFixture(t, "linux", "normal", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", "")
	t.Chdir(repo)
	repo, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	userDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	userPath := filepath.Join(userDir, "crabbox", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(userPath), 0700); err != nil {
		t.Fatal(err)
	}
	const leaseID = "static_architecture_migration"
	const baseYAML = "provider: ssh\ntarget: linux\nstatic:\n  id: " + leaseID + "\n  host: build.example.test\n  user: builder\n"
	writeConfig := func(path, contents string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	probes := 0
	runArchitectureProbe = func(_ context.Context, target SSHTarget, _ string, _ int) (string, error) {
		probes++
		if target.Host != "build.example.test" || target.User != "builder" || target.Port != "2207" || target.TargetOS != "linux" {
			t.Fatalf("migration changed the resolved SSH identity: %#v", target)
		}
		return "v1|arm64|-|-|-", nil
	}
	var out, log bytes.Buffer
	app := core.App{Stdout: &out, Stderr: &log}
	args := []string{"warmup", "--provider", "ssh", "--static-host", "build.example.test"}
	for _, step := range []struct {
		name, userYAML, repoYAML, env string
		discover                      bool
	}{
		{name: "legacy user YAML", userYAML: "architecture: amd64\n"},
		{name: "legacy repo YAML", repoYAML: "architecture: amd64\n"},
		{name: "legacy environment overrides YAML", userYAML: "architecture: arm64\n", repoYAML: "architecture: arm64\n", env: "amd64"},
		{name: "all legacy sources", userYAML: "architecture: amd64\n", repoYAML: "architecture: amd64\n", env: "amd64"},
		{name: "empty overrides retain user assertion", userYAML: "architecture: amd64\n", repoYAML: "architecture: ''\n"},
		{name: "removing user and environment retains repo assertion", repoYAML: "architecture: amd64\n"},
		{name: "remove all explicit sources", discover: true},
	} {
		t.Run(step.name, func(t *testing.T) {
			writeConfig(userPath, baseYAML+step.userYAML)
			writeConfig(filepath.Join(repo, "crabbox.yaml"), step.repoYAML)
			t.Setenv("CRABBOX_ARCH", step.env)
			if step.discover {
				if err := os.Unsetenv("CRABBOX_ARCH"); err != nil {
					t.Fatal(err)
				}
			}
			out.Reset()
			before := probes
			if err := app.Run(context.Background(), []string{"config", "show", "--json"}); err != nil {
				t.Fatal(err)
			}
			var view map[string]any
			if err := json.Unmarshal(out.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if view["architecture"] != "amd64" || view["architectureExplicit"] != !step.discover || probes != before {
				t.Fatalf("offline config did not preserve requested/default distinction: %s probes=%d", &out, probes-before)
			}
			log.Reset()
			err := app.Run(context.Background(), args)
			claim, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID)
			if readErr != nil || probes != before+1 {
				t.Fatalf("read=%v probes=%d warmup=%v log=%s", readErr, probes-before, err, &log)
			}
			if !step.discover {
				if err == nil || !strings.Contains(err.Error(), "architecture=amd64 assertion failed: observed=arm64") || exists {
					t.Fatalf("legacy assertion did not reject before claim publication: err=%v exists=%t log=%s", err, exists, &log)
				}
				return
			}
			if err != nil || !exists {
				t.Fatalf("discovery failed: err=%v exists=%t log=%s", err, exists, &log)
			}
			if claim.LeaseID != leaseID || claim.Provider != "ssh" || claim.RepoRoot != repo || claim.SSHHost != "build.example.test" || claim.SSHPort != 2207 {
				t.Fatalf("discovery changed lease identity: %#v", claim)
			}
			if claim.Labels["architecture"] != "arm64" || claim.Labels["architecture_source"] != "ssh-uname" || claim.Labels["architecture_scope"] != "posix-environment" || claim.Labels["architecture_observed_at"] == "" || claim.Labels["architecture_process"] != "" {
				t.Fatalf("discovery did not publish scoped fresh evidence: %v", claim.Labels)
			}
			t.Chdir(t.TempDir())
			err = app.Run(context.Background(), args)
			after, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID)
			if err == nil || !strings.Contains(err.Error(), "claimed by repo") || readErr != nil || !exists || !reflect.DeepEqual(after, claim) {
				t.Fatalf("migration bypassed repository ownership: err=%v read=%v exists=%t", err, readErr, exists)
			}
		})
	}
}

func TestStaticSSHArchitectureDestinationApprovalPrecedesProbe(t *testing.T) {
	_, _, repo := staticArchitectureFixture(t, "linux", "normal", "")
	t.Chdir(repo)
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, "crabbox.yaml")
	if err := os.WriteFile(path, []byte("provider: ssh\narchitecture: arm64\nstatic:\n  host: repo.example.test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", path)
	t.Setenv("CRABBOX_PROVIDER", "ssh")
	calls := 0
	waitForSSH = func(context.Context, *SSHTarget, io.Writer) error { calls++; return errors.New("unexpected readiness") }
	runArchitectureProbe = func(context.Context, SSHTarget, string, int) (string, error) {
		calls++
		return "", errors.New("unexpected probe")
	}
	var log bytes.Buffer
	err := (core.App{Stdout: &log, Stderr: &log}).Run(context.Background(), []string{"warmup", "--provider", "ssh"})
	if calls != 0 || err == nil || !strings.Contains(err.Error(), "static.host") {
		t.Fatalf("destination not checked before SSH: calls=%d err=%v log=%s", calls, err, &log)
	}
}

func TestStaticSSHArchitecturePreparedWindowsModeOverride(t *testing.T) {
	b, _, _ := staticArchitectureFixture(t, "windows", "wsl2", "")
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	lease, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: root}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("provider: ssh\nstatic:\n  host: build.example.test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", path)
	t.Setenv("CRABBOX_PROVIDER", "ssh")
	sentinel := errors.New("native Windows probe selected")
	calls := 0
	runArchitectureProbe = func(_ context.Context, target SSHTarget, command string, _ int) (string, error) {
		calls++
		if target.TargetOS != "windows" || target.WindowsMode != "normal" || !strings.Contains(command, "IsWow64Process2") {
			t.Fatalf("mode override ignored: target=%#v", target)
		}
		return "", sentinel
	}
	var log bytes.Buffer
	err = (core.App{Stdout: &log, Stderr: &log}).Run(context.Background(), []string{"run", "--provider", "ssh", "--id", lease.LeaseID, "--static-host", "build.example.test", "--windows-mode", "normal", "--arch", "arm64", "--", "true"})
	if !errors.Is(err, sentinel) || calls != 1 {
		t.Fatalf("err=%v calls=%d log=%s", err, calls, &log)
	}
}
