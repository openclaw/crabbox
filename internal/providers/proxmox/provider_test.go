package proxmox

import (
	"flag"
	"io"
	"reflect"
	"strconv"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestServerTypeProjection(t *testing.T) {
	for _, name := range []string{"proxmox", " Proxmox "} {
		if got := core.ServerTypeForProviderClass(name, "beast"); got != "template" {
			t.Fatalf("provider=%q type=%q, want template", name, got)
		}
	}
	for _, tc := range []struct {
		id   int
		want string
	}{{0, "template"}, {-1, "template"}, {1, "template-1"}, {9000, "template-9000"}} {
		cfg := core.Config{Class: "beast", ServerType: "prior-type", ServerTypeExplicit: true, Proxmox: core.ProxmoxConfig{TemplateID: tc.id}}
		if got := (Provider{}).ServerTypeForConfig(cfg); got != tc.want {
			t.Fatalf("template ID=%d type=%q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestProxmoxBindingFlags(t *testing.T) {
	for _, provider := range []string{"proxmox", "fixture-other"} {
		for _, raw := range []string{"", "  ", "fixture"} {
			for _, id := range []int{-2, 0, 3} {
				for _, boolean := range []bool{false, true} {
					cfg := core.BaseConfig()
					cfg.Provider, cfg.ServerType, cfg.SSHUser, cfg.WorkRoot = provider, "prior-type", "generic-user", "/generic"
					cfg.Proxmox.TemplateID = 7
					before := cfg
					fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
					fs.SetOutput(io.Discard)
					values := (Provider{}).RegisterFlags(fs, cfg)
					count := 0
					fs.VisitAll(func(*flag.Flag) { count++ })
					if count != 10 || fs.Lookup("proxmox-token-id") != nil || fs.Lookup("proxmox-token-secret") != nil {
						t.Fatal("flag surface changed")
					}
					if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil || !reflect.DeepEqual(cfg, before) {
						t.Fatal("unvisited flags changed configuration")
					}
					for _, name := range []string{"api-url", "node", "storage", "pool", "bridge", "user", "work-root"} {
						if err := fs.Set("proxmox-"+name, raw); err != nil {
							t.Fatal(err)
						}
					}
					if err := fs.Set("proxmox-template-id", strconv.Itoa(id)); err != nil {
						t.Fatal(err)
					}
					for _, name := range []string{"full-clone", "insecure-tls"} {
						if err := fs.Set("proxmox-"+name, strconv.FormatBool(boolean)); err != nil {
							t.Fatal(err)
						}
					}
					if err := (Provider{}).ApplyFlags(&cfg, fs, struct{}{}); err != nil || !reflect.DeepEqual(cfg, before) {
						t.Fatal("foreign values changed configuration")
					}
					want := before
					want.Proxmox = core.ProxmoxConfig{APIURL: raw, Node: raw, TemplateID: id, Storage: raw, Pool: raw, Bridge: raw, User: raw, WorkRoot: raw, FullClone: boolean, InsecureTLS: boolean}
					want.ServerType = "template"
					if id > 0 {
						want.ServerType = "template-" + strconv.Itoa(id)
					}
					want.SSHUser, want.WorkRoot = raw, raw
					core.RecordProviderFlagInputs(&want, true, "proxmox")
					for repeat := 0; repeat < 2; repeat++ {
						if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(cfg, want) {
							t.Fatalf("provider=%q raw=%q id=%d: flags or generic projection changed", provider, raw, id)
						}
					}
				}
			}
		}
	}
	for _, name := range []string{"proxmox-template-id", "proxmox-full-clone", "proxmox-insecure-tls"} {
		fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		(Provider{}).RegisterFlags(fs, core.BaseConfig())
		if err := fs.Parse([]string{"--" + name + "=invalid"}); err == nil {
			t.Fatalf("malformed %s accepted", name)
		}
	}
}

func TestProxmoxBindingVisitedGenericEffects(t *testing.T) {
	for _, name := range []string{"node", "template-id", "user", "work-root"} {
		cfg := core.BaseConfig()
		cfg.Provider, cfg.ServerType, cfg.SSHUser, cfg.WorkRoot = "fixture-other", "prior-type", "generic-user", "/generic"
		cfg.Proxmox.TemplateID = 7
		fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
		values := (Provider{}).RegisterFlags(fs, cfg)
		raw := ""
		if name == "template-id" {
			raw = "7"
		}
		if err := fs.Set("proxmox-"+name, raw); err != nil {
			t.Fatal(err)
		}
		if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
			t.Fatal(err)
		}
		wantType, wantUser, wantRoot := "prior-type", "generic-user", "/generic"
		if name == "template-id" {
			wantType = "template-7"
		}
		if name == "user" {
			wantUser = ""
		}
		if name == "work-root" {
			wantRoot = ""
		}
		if cfg.ServerType != wantType || cfg.SSHUser != wantUser || cfg.WorkRoot != wantRoot {
			t.Fatal("unvisited generic projection changed")
		}
	}
}
