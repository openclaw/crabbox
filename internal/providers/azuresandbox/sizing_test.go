package azuresandbox

import (
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Keep expected wire quantities independent of the production catalog.
var machineSizingCases = []struct {
	typ, tier, cpu, memory, disk string
}{
	{"0.25cpu-0.5gib", "XS", "250m", "512Mi", "5Gi"},
	{"0.5cpu-1gib", "S", "500m", "1024Mi", "10Gi"},
	{"1cpu-2gib", "M", "1000m", "2048Mi", "20Gi"},
	{"2cpu-4gib", "L", "2000m", "4096Mi", "40Gi"},
	{"4cpu-8gib", "XL", "4000m", "8192Mi", "80Gi"},
	{"8cpu-16gib", "2XL", "8000m", "16384Mi", "160Gi"},
	{"16cpu-32gib", "3XL", "16000m", "32768Mi", "320Gi"},
}

func TestMachineSizingCLI(t *testing.T) {
	type testCase struct {
		name, typ, selector, cpu, memory, disk string
	}
	cases := []testCase{
		{name: "default", cpu: "2000m", memory: "4096Mi"},
		{name: "small", selector: "small", cpu: "2000m", memory: "4096Mi"},
	}
	for _, machine := range machineSizingCases {
		for _, typ := range []string{machine.typ, strings.ToLower(machine.tier)} {
			for _, selector := range []string{"type", "config", "env"} {
				cases = append(cases, testCase{name: typ + "/" + selector, typ: typ, selector: selector, cpu: machine.cpu, memory: machine.memory, disk: machine.disk})
			}
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fixture{}
			b, req := fixtureBackend(t, f)
			t.Chdir(req.Repo.Root)
			t.Setenv("CRABBOX_SERVER_TYPE", "")
			if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
				t.Fatalf("git init: %s %v", out, err)
			}
			old := newAPI
			newAPI = func(core.Config, core.Runtime) (*client, error) { return b.api, nil }
			t.Cleanup(func() { newAPI = old })
			args := []string{"warmup", "--provider", providerName, "--network", "public", "--tailscale=false", "--keep", "--lease-id", req.RequestedLeaseID, "--ttl", "4h", "--idle-timeout", "15m"}
			switch tc.selector {
			case "small":
				args = append(args, "--class", "small")
			case "type":
				args = append(args, "--type", tc.typ)
			case "config":
				if err := os.WriteFile("crabbox.yaml", []byte("serverType: "+tc.typ+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "env":
				t.Setenv("CRABBOX_SERVER_TYPE", tc.typ)
			}
			app := core.App{Stdout: io.Discard, Stderr: io.Discard}
			if err := app.Run(t.Context(), args); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"cpu": tc.cpu, "memory": tc.memory}
			if tc.disk != "" {
				want["disk"] = tc.disk
			}
			if f.creates != 1 || !reflect.DeepEqual(f.created.Resources, want) {
				t.Fatalf("CLI create = %+v, count %d; want %v", f.created.Resources, f.creates, want)
			}
		})
	}
}

func TestMachineSizingCreateReplayAndCleanup(t *testing.T) {
	cases := append([]struct{ typ, tier, cpu, memory, disk string }{
		{"", "L", "2000m", "4096Mi", ""},
	}, machineSizingCases...)
	for _, machine := range cases {
		for _, source := range []string{"public", "private"} {
			t.Run(machine.typ+"/"+source, func(t *testing.T) {
				f := &fixture{loseCreateResponse: true}
				b, req := fixtureBackend(t, f)
				if source == "private" {
					b.cfg.AzureSandbox.DiskID = "prepared-disk-id"
				}
				b.cfg.ServerType, b.cfg.ServerTypeExplicit = machine.typ, machine.typ != ""
				if _, err := b.acquire(t.Context(), req); err == nil {
					t.Fatal("lost response accepted")
				}
				want := map[string]string{"cpu": machine.cpu, "memory": machine.memory}
				if machine.disk != "" {
					want["disk"] = machine.disk
				}
				if !reflect.DeepEqual(f.created.Resources, want) {
					t.Fatalf("create resources = %v, want %v", f.created.Resources, want)
				}
				// Explicit tier aliases must recover the same intent, not allocate
				// again. An implicit legacy default continues omitting disk size.
				if machine.typ != "" {
					b.cfg.ServerType = strings.ToLower(machine.tier)
				}
				if _, err := b.acquire(t.Context(), req); err != nil || f.creates != 1 {
					t.Fatalf("alias replay: %v, creates %d", err, f.creates)
				}
				claim, err := b.claim(req.RequestedLeaseID)
				if err != nil {
					t.Fatal(err)
				}
				intent := struct {
					Config    core.AzureSandboxConfig
					TTL, Idle time.Duration
					Slug      string
				}{b.cfg.AzureSandbox, b.cfg.TTL, b.cfg.IdleTimeout, req.RequestedSlug}
				if machine.typ == "" {
					// The pre-sizing schema must remain byte-for-byte compatible.
					legacy, err := core.FixedIntentFingerprint("aca-sandbox-v1", intent)
					if err != nil || claim.FixedCreateIntent.Fingerprint != legacy {
						t.Fatalf("legacy fingerprint changed: %v", err)
					}
				} else {
					// Every explicit CPU/memory/disk capacity belongs to the intent.
					previous, err := core.FixedIntentFingerprint("aca-sandbox-v2", struct {
						Intent    any
						Resources map[string]string
					}{intent, want})
					if err != nil || claim.FixedCreateIntent.Fingerprint != previous {
						t.Fatalf("explicit tier fingerprint changed: %v", err)
					}
				}
				b.cfg.ServerType, b.cfg.ServerTypeExplicit = "2cpu-4gib", true
				if machine.cpu == "2000m" {
					b.cfg.ServerType = "4cpu-8gib"
				}
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

func TestMachineSizingRejectsLegacyDiskIntentChange(t *testing.T) {
	f := &fixture{loseCreateResponse: true}
	b, req := fixtureBackend(t, f)
	if _, err := b.acquire(t.Context(), req); err == nil {
		t.Fatal("lost response accepted")
	}
	b.cfg.ServerType, b.cfg.ServerTypeExplicit = "l", true
	if _, err := b.acquire(t.Context(), req); err == nil || f.creates != 1 {
		t.Fatal("explicit L disk sizing changed an existing implicit-default intent")
	}
	b.cfg.ServerType, b.cfg.ServerTypeExplicit = "", false
	if _, err := b.acquire(t.Context(), req); err != nil || f.creates != 1 {
		t.Fatalf("legacy replay: %v, creates %d", err, f.creates)
	}
	if f.created.Resources["disk"] != "" {
		t.Fatal("legacy default gained a disk override")
	}
	if err := b.Stop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err != nil || f.deletes != 1 {
		t.Fatalf("legacy cleanup: %v, deletes %d", err, f.deletes)
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
	for _, machine := range machineSizingCases {
		for _, typ := range []string{machine.typ, machine.tier, " " + strings.ToUpper(machine.typ) + " "} {
			cfg := core.BaseConfig()
			cfg.ServerType, cfg.ServerTypeExplicit = typ, true
			if _, err := p.Configure(cfg, core.Runtime{}); err != nil {
				t.Fatalf("type %s: %v", typ, err)
			}
			if got := p.ServerTypeForConfig(cfg); got != machine.typ {
				t.Fatalf("type %s resolved to %s, want %s", typ, got, machine.typ)
			}
		}
	}
}

func TestMachineSizingRejectsUnsupportedTypesBeforeAllocation(t *testing.T) {
	for _, typ := range []string{"unsupported", "0.25cpu-1gib", "32cpu-64gib"} {
		t.Run(typ, func(t *testing.T) {
			f := &fixture{}
			b, req := fixtureBackend(t, f)
			b.cfg.ServerType, b.cfg.ServerTypeExplicit = typ, true
			_, err := (Provider{}).Configure(b.cfg, b.rt)
			if err == nil || !strings.Contains(err.Error(), "0.25cpu-0.5gib (XS)") || !strings.Contains(err.Error(), "16cpu-32gib (3XL)") {
				t.Fatalf("unsupported type diagnostic: %v", err)
			}
			if _, err := b.acquire(t.Context(), req); err == nil || f.creates != 0 {
				t.Fatalf("unsupported type allocated: %v, creates %d", err, f.creates)
			}
		})
	}
}

func TestSnapshotSizingCannotResize(t *testing.T) {
	for _, machine := range machineSizingCases {
		for _, typ := range []string{machine.typ, strings.ToLower(machine.tier)} {
			t.Run(typ, func(t *testing.T) {
				f := &fixture{}
				b, req := fixtureBackend(t, f)
				binding := pinSnapshotFixture(t, b, nil)
				snapshotTransport(t, b, f, binding, nil)
				b.cfg.ServerType, b.cfg.ServerTypeExplicit = typ, true
				if machine.typ != "2cpu-4gib" {
					if _, err := (Provider{}).Configure(b.cfg, b.rt); err == nil {
						t.Fatal("snapshot resize configured")
					}
					if _, err := b.acquire(t.Context(), req); err == nil || f.creates != 0 {
						t.Fatal("snapshot resize allocated")
					}
					return
				}
				if _, err := b.acquire(t.Context(), req); err != nil {
					t.Fatal(err)
				}
				if f.created.Resources != nil || f.created.Lifecycle != nil || f.created.SourcesRef.Snapshot.ID != binding.SnapshotID {
					t.Fatal("snapshot restore contract changed")
				}
			})
		}
	}
}
