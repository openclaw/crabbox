package azuresandbox

import (
	"io"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestMachineSizingCLI(t *testing.T) {
	for _, selector := range []string{"default", "small", "type", "config", "env"} {
		t.Run(selector, func(t *testing.T) {
			f := &fixture{}
			b, req := fixtureBackend(t, f)
			t.Chdir(req.Repo.Root)
			if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
				t.Fatalf("git init: %s %v", out, err)
			}
			old := newAPI
			newAPI = func(core.Config, core.Runtime) (*client, error) { return b.api, nil }
			t.Cleanup(func() { newAPI = old })
			args := []string{"warmup", "--provider", providerName, "--network", "public", "--tailscale=false", "--keep", "--lease-id", req.RequestedLeaseID, "--ttl", "4h", "--idle-timeout", "15m"}
			switch selector {
			case "small":
				args = append(args, "--class", "small")
			case "type":
				args = append(args, "--type", "4cpu-8gib")
			case "config":
				if err := os.WriteFile("crabbox.yaml", []byte("serverType: 4cpu-8gib\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "env":
				t.Setenv("CRABBOX_SERVER_TYPE", "4cpu-8gib")
			}
			app := core.App{Stdout: io.Discard, Stderr: io.Discard}
			if err := app.Run(t.Context(), args); err != nil {
				t.Fatal(err)
			}
			cpu, memory := "4000m", "8192Mi"
			if selector == "default" || selector == "small" {
				cpu, memory = "2000m", "4096Mi"
			}
			if f.creates != 1 || f.created.Resources["cpu"] != cpu || f.created.Resources["memory"] != memory {
				t.Fatalf("CLI create = %+v, count %d", f.created.Resources, f.creates)
			}
		})
	}
}

func TestMachineSizingCreateReplayAndCleanup(t *testing.T) {
	for _, typ := range []string{"", "2cpu-4gib", "4cpu-8gib"} {
		for _, source := range []string{"public", "private"} {
			t.Run(typ+"/"+source, func(t *testing.T) {
				f := &fixture{loseCreateResponse: true}
				b, req := fixtureBackend(t, f)
				if source == "private" {
					b.cfg.AzureSandbox.DiskID = "prepared-disk-id"
				}
				b.cfg.ServerType, b.cfg.ServerTypeExplicit = typ, typ != ""
				if _, err := b.acquire(t.Context(), req); err == nil {
					t.Fatal("lost response accepted")
				}
				want := map[string]string{"cpu": "2000m", "memory": "4096Mi"}
				if typ == "4cpu-8gib" {
					want = map[string]string{"cpu": "4000m", "memory": "8192Mi"}
				}
				if !reflect.DeepEqual(f.created.Resources, want) {
					t.Fatalf("create resources = %v, want %v", f.created.Resources, want)
				}
				if _, err := b.acquire(t.Context(), req); err != nil {
					t.Fatal(err)
				}
				claim, err := b.claim(req.RequestedLeaseID)
				if err != nil {
					t.Fatal(err)
				}
				if typ != "4cpu-8gib" {
					// The pre-sizing schema must remain byte-for-byte compatible.
					legacy, err := core.FixedIntentFingerprint("aca-sandbox-v1", struct {
						Config    core.AzureSandboxConfig
						TTL, Idle time.Duration
						Slug      string
					}{b.cfg.AzureSandbox, b.cfg.TTL, b.cfg.IdleTimeout, req.RequestedSlug})
					if err != nil || claim.FixedCreateIntent.Fingerprint != legacy {
						t.Fatalf("legacy fingerprint changed: %v", err)
					}
					b.cfg.ServerType = "4cpu-8gib"
				} else {
					b.cfg.ServerType = "2cpu-4gib"
				}
				b.cfg.ServerTypeExplicit = true
				if _, err := b.acquire(t.Context(), req); err == nil || f.creates != 1 {
					t.Fatal("replay changed machine size or allocated twice")
				}
				if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err != nil {
					t.Fatal(err)
				}
				if f.deletes != 1 {
					t.Fatalf("deletes = %d", f.deletes)
				}
			})
		}
	}
}

func TestMachineSizingCatalogAndDefaults(t *testing.T) {
	p := Provider{}
	if got := p.ServerTypeForConfig(core.BaseConfig()); got != "2cpu-4gib" {
		t.Fatalf("default = %s", got)
	}
	for _, profile := range p.ClassProfiles() {
		cfg := core.BaseConfig()
		cfg.Class = profile.Class
		if got := p.ServerTypeForConfig(cfg); got != profile.Primary.Type || *profile.Primary.VCPU != 2 || profile.Primary.Memory.Value != 4 || profile.Primary.Memory.Unit != core.ProviderMemoryUnitGiB {
			t.Fatalf("untruthful class profile: %+v, resolved %s", profile, got)
		}
	}
	for _, typ := range []string{"2cpu-4gib", "4cpu-8gib", "unsupported"} {
		cfg := core.BaseConfig()
		cfg.ServerType, cfg.ServerTypeExplicit = typ, true
		_, err := p.Configure(cfg, core.Runtime{})
		if (err != nil) != (typ == "unsupported") {
			t.Fatalf("type %s: %v", typ, err)
		}
	}
}

func TestSnapshotSizingCannotResize(t *testing.T) {
	f := &fixture{}
	b, req := fixtureBackend(t, f)
	binding := pinSnapshotFixture(t, b, nil)
	snapshotTransport(t, b, f, binding, nil)
	b.cfg.ServerType, b.cfg.ServerTypeExplicit = "4cpu-8gib", true
	if _, err := (Provider{}).Configure(b.cfg, b.rt); err == nil {
		t.Fatal("snapshot resize configured")
	}
	if _, err := b.acquire(t.Context(), req); err == nil || f.creates != 0 {
		t.Fatal("snapshot resize allocated")
	}
	b.cfg.ServerType = "2cpu-4gib"
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if f.created.Resources != nil || f.created.Lifecycle != nil || f.created.SourcesRef.Snapshot.ID != binding.SnapshotID {
		t.Fatal("snapshot restore contract changed")
	}
}
