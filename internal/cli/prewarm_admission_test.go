package cli

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type probeAdmissionObservation struct {
	cfg Config
	req RunRequest
}

type probeAdmissionProvider struct {
	spec                             ProviderSpec
	reject                           bool
	configured, warmed, ran, stopped int
	admissions                       []RunRequest
	execution                        probeAdmissionObservation
	warmupConfig                     Config
	warmupRequest                    WarmupRequest
}

func (p *probeAdmissionProvider) Name() string       { return p.spec.Name }
func (p *probeAdmissionProvider) Aliases() []string  { return nil }
func (p *probeAdmissionProvider) Spec() ProviderSpec { return p.spec }
func (p *probeAdmissionProvider) CreationOnlyFlagNames() []string {
	return []string{"admission-create"}
}
func (p *probeAdmissionProvider) RegisterFlags(fs *flag.FlagSet, cfg Config) any {
	return [2]*string{
		fs.String("admission-route", cfg.Blacksmith.Org, "test routing selector"),
		fs.String("admission-create", cfg.Blacksmith.Workflow, "test creation selector"),
	}
}
func (p *probeAdmissionProvider) ApplyFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	flags := values.([2]*string)
	if flagWasSet(fs, "admission-route") {
		cfg.Blacksmith.Org = *flags[0]
	}
	if flagWasSet(fs, "admission-create") {
		cfg.Blacksmith.Workflow = *flags[1]
	}
	return nil
}
func (p *probeAdmissionProvider) ClaimScope(cfg Config) string {
	return cfg.Blacksmith.Org + "/" + cfg.Blacksmith.Workflow
}
func (p *probeAdmissionProvider) ValidateRunOptions(req RunRequest) error {
	p.admissions = append(p.admissions, req)
	if p.reject {
		return exit(2, "probe admission: --no-sync unsupported; use a provider workflow probe")
	}
	return nil
}
func (p *probeAdmissionProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	p.configured++
	return &probeAdmissionBackend{p: p, cfg: cfg, rt: rt}, nil
}

type probeAdmissionBackend struct {
	p   *probeAdmissionProvider
	cfg Config
	rt  Runtime
}

func (b *probeAdmissionBackend) Spec() ProviderSpec             { return b.p.spec }
func (b *probeAdmissionBackend) SupportsRequestedLeaseID() bool { return true }
func (b *probeAdmissionBackend) Warmup(_ context.Context, req WarmupRequest) error {
	b.p.warmed++
	b.p.warmupRequest = req
	b.p.warmupConfig = b.cfg
	fmt.Fprintln(b.rt.Stdout, "leased cbx_0123456789ab slug=probe")
	return nil
}
func (b *probeAdmissionBackend) Run(_ context.Context, req RunRequest) (RunResult, error) {
	b.p.ran++
	b.p.execution = probeAdmissionObservation{b.cfg, req}
	return RunResult{Provider: b.p.Name(), LeaseID: req.ID}, nil
}
func (b *probeAdmissionBackend) Stop(context.Context, StopRequest) error { b.p.stopped++; return nil }
func (b *probeAdmissionBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}
func (b *probeAdmissionBackend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{}, nil
}

func setupProbeAdmission(t *testing.T, features FeatureSet) *probeAdmissionProvider {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake git executable requires /bin/sh")
	}
	clearConfigEnv(t)
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("CRABBOX_PROFILE", "")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(root, "config.yaml"))
	// No subprocess here needs a real Git executable or an ancestor checkout.
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "git"), "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(filepath.Join(bin, "git"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	p := &probeAdmissionProvider{spec: ProviderSpec{
		Name: "probe-admission-test", Kind: ProviderKindDelegatedRun,
		Targets: []TargetSpec{{OS: targetLinux}}, Features: features, Coordinator: CoordinatorNever,
	}}
	RegisterProvider(p)
	t.Cleanup(func() { delete(providerRegistry, p.Name()) })
	writeFile(t, filepath.Join(root, "config.yaml"), "provider: probe-admission-test\nblacksmith:\n  org: config-route\n  workflow: config-only\n")
	return p
}

func TestPrewarmProbeAdmissionRejectsBeforeActivity(t *testing.T) {
	for _, module := range []bool{false, true} {
		for _, dry := range []bool{false, true} {
			t.Run(fmt.Sprintf("module=%v/dry=%v", module, dry), func(t *testing.T) {
				features := FeatureSet{FeatureTailscale}
				if module {
					features = append(features, FeatureModuleRun)
				}
				p := setupProbeAdmission(t, features)
				p.reject = !module
				aclCalls := 0
				prev := pondTailnetACLClientFactory
				t.Cleanup(func() { pondTailnetACLClientFactory = prev })
				pondTailnetACLClientFactory = func(string) pondTailnetACLClient { aclCalls++; return nil }
				t.Setenv(pondACLAutoBootstrapEnvVar, "1")
				t.Setenv("CRABBOX_TAILSCALE", "1")
				t.Setenv("TS_API_KEY", "fixture-api-key")
				t.Setenv("CRABBOX_TAILSCALE_AUTH_KEY", "fixture-auth-key")
				var stdout, stderr bytes.Buffer
				args := []string{"prewarm", "--pond", "alpha", "--probe-command", "true"}
				if dry {
					args = append(args, "--dry-run")
				}
				err := (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args)
				var ee ExitError
				want := "--no-sync"
				if module {
					want = "module source"
				}
				if !AsExitError(err, &ee) || ee.Code != 2 || !strings.Contains(err.Error(), want) {
					t.Fatalf("error=%v, want exit 2 containing %q", err, want)
				}
				if p.configured != 0 || p.warmed != 0 || p.ran != 0 || p.stopped != 0 || aclCalls != 0 {
					t.Fatalf("activity configure=%d warmup=%d run=%d stop=%d ACL=%d", p.configured, p.warmed, p.ran, p.stopped, aclCalls)
				}
				if len(p.admissions) != 1 || stdout.Len() != 0 || stderr.Len() != 0 {
					t.Fatalf("admissions=%d stdout=%q stderr=%q", len(p.admissions), stdout.String(), stderr.String())
				}
			})
		}
	}
}

func TestPrewarmProbeAdmissionDefersACLUntilWarmup(t *testing.T) {
	p := setupProbeAdmission(t, FeatureSet{FeatureTailscale})
	t.Setenv("CRABBOX_TAILSCALE", "1")
	t.Setenv(pondACLAutoBootstrapEnvVar, "1")
	t.Setenv("TS_API_KEY", "fixture-api-key")
	t.Setenv("CRABBOX_TAILSCALE_AUTH_KEY", "fixture-auth-key")
	aclCalls := 0
	prev := pondTailnetACLClientFactory
	t.Cleanup(func() { pondTailnetACLClientFactory = prev })
	pondTailnetACLClientFactory = func(string) pondTailnetACLClient {
		aclCalls++
		if len(p.admissions) != 1 || p.warmed != 0 {
			t.Error("ACL bootstrap must follow admission and precede warmup")
		}
		return nil
	}
	err := (App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).Run(t.Context(), []string{
		"prewarm", "--pond", "alpha", "--probe-command", "true",
	})
	if err != nil || aclCalls != 1 || p.warmed != 1 || p.ran != 1 {
		t.Fatalf("error=%v ACL=%d warmup=%d run=%d", err, aclCalls, p.warmed, p.ran)
	}
}

func TestPrewarmProbeAdmissionPrecedesTypedPoolChecks(t *testing.T) {
	p := setupProbeAdmission(t, nil)
	p.reject = true
	err := (App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).Run(t.Context(), []string{
		"prewarm", "--probe-command", "true", "--pool", "test", "--pool-identity-file", filepath.Join(t.TempDir(), "missing.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "--no-sync") || len(p.admissions) != 1 || p.configured != 0 {
		t.Fatalf("error=%v admissions=%d configure=%d", err, len(p.admissions), p.configured)
	}
}

func TestPrewarmProbeAdmissionPreservesFollowupIntent(t *testing.T) {
	for _, archive := range []bool{false, true} {
		t.Run(fmt.Sprintf("archive=%v", archive), func(t *testing.T) {
			features := FeatureSet{}
			if archive {
				features = append(features, FeatureArchiveSync)
			}
			p := setupProbeAdmission(t, features)
			var stdout, stderr bytes.Buffer
			command := "printf '%s\\n' ready && true"
			err := (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{
				"prewarm", "--no-hydrate", "--admission-route", "flag-route", "--admission-create", "create-only",
				"--profile", "warmup-profile", "--class", "standard", "--ttl", "15m",
				"--slug", "warm-probe", "--reclaim", "--probe-command", command,
			})
			if err != nil {
				t.Fatalf("prewarm: %v\n%s", err, stderr.String())
			}
			if len(p.admissions) != 2 || p.warmed != 1 || p.ran != 1 || p.stopped != 0 {
				t.Fatalf("admissions=%d warmup=%d run=%d stop=%d", len(p.admissions), p.warmed, p.ran, p.stopped)
			}
			early, actual := p.admissions[0], p.execution
			if early.ID != "" || actual.req.ID != "cbx_0123456789ab" {
				t.Fatalf("early ID=%q actual ID=%q", early.ID, actual.req.ID)
			}
			for _, req := range []RunRequest{early, actual.req} {
				if !req.NoSync || !req.NoHydrate || !req.ShellMode || !req.ReuseLease || !reflect.DeepEqual(req.Command, []string{command}) || req.Reclaim || req.RequestedSlug != "" {
					t.Fatalf("probe intent lost: %#v", req)
				}
			}
			if early.Options.ProviderScope != "flag-route/config-only" || actual.cfg.Blacksmith.Org != "flag-route" || actual.cfg.Blacksmith.Workflow != "config-only" || p.warmupConfig.Blacksmith.Workflow != "create-only" {
				t.Fatal("routing or creation-only config projection changed")
			}
			if actual.cfg.Profile == "warmup-profile" || actual.cfg.Class == "standard" || p.warmupConfig.Profile != "warmup-profile" || p.warmupConfig.Class != "standard" {
				t.Fatal("probe inherited creation-only profile/class flags")
			}
			// These fields become available only during the concrete run's preflights.
			actual.req.ID, actual.req.Repo, actual.req.RunID, actual.req.Env = "", Repo{}, "", nil
			if !reflect.DeepEqual(early, actual.req) {
				t.Fatalf("admitted intent=%#v\nactual=%#v", early, actual.req)
			}
		})
	}
}

func TestRunProviderAdmissionBeforeConfigure(t *testing.T) {
	for _, id := range []string{"", "cbx_0123456789ab"} {
		t.Run("id="+id, func(t *testing.T) {
			p := setupProbeAdmission(t, nil)
			p.reject = true
			err := (App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).Run(t.Context(), []string{
				"run", "--id", id, "--no-sync", "--no-hydrate", "--shell", "--", "true",
			})
			var ee ExitError
			if !AsExitError(err, &ee) || ee.Code != 2 || !strings.Contains(err.Error(), "--no-sync") {
				t.Fatalf("error=%v", err)
			}
			if len(p.admissions) != 1 || p.configured != 0 || p.warmed != 0 || p.ran != 0 || p.stopped != 0 {
				t.Fatalf("unexpected activity: %#v", p)
			}
			req := p.admissions[0]
			if req.ID != id || req.ReuseLease != (id != "") || !req.NoHydrate || !req.NoSync || !req.ShellMode {
				t.Fatalf("intent=%#v", req)
			}
		})
	}
}
