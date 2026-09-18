package localcontainer

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestOrdinaryScalarFlagsUnselected(t *testing.T) {
	initial := core.LocalContainerConfig{
		Runtime: "prior", Image: "example:prior", User: "prior-user", WorkRoot: "prior-root",
		CPUs: 5, Memory: "5g", Network: "prior-network", DockerSocket: true, NoHostname: true,
	}
	for _, tc := range []struct {
		name                 string
		args                 []string
		want                 core.LocalContainerConfig
		runtime, image, root bool
		user                 bool
	}{
		{name: "unvisited", want: initial},
		{name: "equal", args: []string{"--local-container-runtime=prior", "--local-container-image=example:prior", "--local-container-user=prior-user", "--local-container-work-root=prior-root", "--local-container-cpus=5", "--local-container-memory=5g", "--local-container-network=prior-network", "--local-container-docker-socket=true"}, want: initial, runtime: true, image: true, root: true, user: true},
		{name: "empty zero false", args: []string{"--local-container-runtime=", "--local-container-image=", "--local-container-user=", "--local-container-work-root=", "--local-container-cpus=0", "--local-container-memory=", "--local-container-network=", "--local-container-docker-socket=false"}, want: core.LocalContainerConfig{NoHostname: true}, runtime: true, image: true, root: true, user: true},
		{name: "raw values", args: []string{"--local-container-runtime= custom ", "--local-container-image= example:new ", "--local-container-user= new-user ", "--local-container-work-root=~/literal ", "--local-container-cpus=-2", "--local-container-memory= 7g ", "--local-container-network= custom ", "--local-container-docker-socket=false"}, want: core.LocalContainerConfig{Runtime: " custom ", Image: " example:new ", User: " new-user ", WorkRoot: "~/literal ", CPUs: -2, Memory: " 7g ", Network: " custom ", NoHostname: true}, runtime: true, image: true, root: true, user: true},
		{name: "runtime event only", args: []string{"--local-container-runtime=prior"}, want: initial, runtime: true},
		{name: "image event only", args: []string{"--local-container-image=example:prior"}, want: initial, image: true},
		{name: "root event only", args: []string{"--local-container-work-root=prior-root"}, want: initial, root: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := core.Config{Provider: "unselected-config-test", SSHUser: "generic-user", WorkRoot: "generic-root", LocalContainer: initial}
			core.MarkWorkRootExplicit(&cfg)
			defaults := core.Config{LocalContainer: core.LocalContainerConfig{Runtime: "registration", Image: "example:registration", User: "registration", WorkRoot: "registration", CPUs: 9, Memory: "9g", Network: "registration"}}
			fs := flag.NewFlagSet(tc.name, flag.ContinueOnError)
			provider := Provider{}
			values := provider.RegisterFlags(fs, defaults)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			want := cfg
			want.LocalContainer = tc.want
			core.RecordProviderFlagInputs(&want, len(tc.args) > 0, providerName)
			if tc.runtime {
				core.MarkLocalContainerRuntimeExplicit(&want)
			}
			if tc.image {
				core.MarkLocalContainerImageExplicit(&want)
			}
			if tc.root {
				want.WorkRoot = tc.want.WorkRoot
				core.MarkWorkRootExplicit(&want)
				core.MarkLocalContainerWorkRootExplicit(&want)
			}
			if tc.user {
				want.SSHUser = tc.want.User
			}
			if err := provider.ApplyFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg, want) {
				t.Fatal("scalar flags changed values, generic copies, or explicit-source markers")
			}
			wantExplicit := !tc.root || tc.want.WorkRoot != ""
			if core.IsWorkRootExplicit(&cfg) != wantExplicit {
				t.Fatal("generic root marker did not snapshot the accepted string")
			}
			cfg.WorkRoot = "later-raw-root"
			if wantExplicit {
				cfg.WorkRoot = ""
			}
			if core.IsWorkRootExplicit(&cfg) != wantExplicit {
				t.Fatal("later raw root assignment changed the explicit snapshot")
			}
		})
	}
}

func TestNormalizeConfigForShowPreservesUnrelatedSettings(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.WorkRoot = "/work/generic"
	cfg.LocalContainer.User = "test-runner"
	cfg.LocalContainer.Volumes = []string{"/synthetic-private-volume:/mnt/data"}
	cfg.LocalContainer.CheckpointMetadata = map[string]string{checkpointMetadataForkName: "synthetic-private-checkpoint"}

	want := cfg
	want.LocalContainer.WorkRoot = cfg.WorkRoot
	got := (Provider{}).NormalizeConfigForShow(cfg)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("display normalization changed fields beyond container defaults and work root")
	}
	if cfg.LocalContainer.WorkRoot != "" {
		t.Fatal("display normalization changed execution input")
	}
	if got.ServerType == "synthetic-private-checkpoint" || got.SSHUser == "test-runner" {
		t.Fatal("display normalization exposed checkpoint metadata or changed SSH defaults")
	}
}

func TestLocalContainerConfigShowSection(t *testing.T) {
	projector, ok := any(Provider{}).(core.ProviderConfigShowProjector)
	if !ok {
		t.Fatal("real provider is missing passive config-show ownership")
	}
	for _, tc := range []struct {
		name string
		cfg  core.LocalContainerConfig
		want map[string]any
		text string
	}{
		{name: "zero", cfg: core.LocalContainerConfig{Runtime: "", Image: "", User: "", WorkRoot: "", CPUs: 0, Memory: "", Network: "", DockerSocket: false, NoHostname: true, Volumes: []string{"internal-volume"}, CheckpointMetadata: map[string]string{"internal-key": "internal-checkpoint"}}, want: map[string]any{"runtime": "", "image": "", "user": "", "workRoot": "", "cpus": 0, "memory": "", "network": "", "dockerSocket": false}, text: "local_container runtime= image= user= work_root=- cpus=0 memory=- network= docker_socket=false\n"},
		{name: "raw", cfg: core.LocalContainerConfig{Runtime: " raw-runtime ", Image: " raw-image ", User: " raw-user ", WorkRoot: " raw-root ", CPUs: -2, Memory: "   ", Network: " raw-network ", DockerSocket: true, NoHostname: true, Volumes: []string{"internal-volume"}, CheckpointMetadata: map[string]string{"internal-key": "internal-checkpoint"}}, want: map[string]any{"runtime": " raw-runtime ", "image": " raw-image ", "user": " raw-user ", "workRoot": " raw-root ", "cpus": -2, "memory": "   ", "network": " raw-network ", "dockerSocket": true}, text: "local_container runtime= raw-runtime  image= raw-image  user= raw-user  work_root= raw-root  cpus=-2 memory=    network= raw-network  docker_socket=true\n"},
		{name: "configured", cfg: core.LocalContainerConfig{Runtime: "docker", Image: "example:stable", User: "runner", WorkRoot: "/work/example", CPUs: 3, Memory: "6g", Network: "none", DockerSocket: true, NoHostname: true, Volumes: []string{"internal-volume"}, CheckpointMetadata: map[string]string{"internal-key": "internal-checkpoint"}}, want: map[string]any{"runtime": "docker", "image": "example:stable", "user": "runner", "workRoot": "/work/example", "cpus": 3, "memory": "6g", "network": "none", "dockerSocket": true}, text: "local_container runtime=docker image=example:stable user=runner work_root=/work/example cpus=3 memory=6g network=none docker_socket=true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := core.Config{Provider: "unselected-display-test", LocalContainer: tc.cfg}
			before, err := json.Marshal(cfg.LocalContainer)
			if err != nil {
				t.Fatal(err)
			}
			section := projector.ConfigShowSection(cfg)
			got := map[string]any{}
			var fields []string
			for _, field := range section.Fields {
				got[field.JSONName] = field.JSONValue
				fields = append(fields, field.TextName+"="+field.TextValue)
			}
			if section.JSONKey != "localContainer" || section.TextLabel != "local_container" || !reflect.DeepEqual(section.Providers, []string{"local-container"}) || len(section.Fields) != 8 {
				t.Fatalf("section metadata=%#v", section)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("public fields=%#v want %#v", got, tc.want)
			}
			if line := section.TextLabel + " " + strings.Join(fields, " ") + "\n"; line != tc.text {
				t.Fatalf("text=%q want %q", line, tc.text)
			}
			after, err := json.Marshal(cfg.LocalContainer)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("projection mutated original config or slice contents")
			}
		})
	}
}

func TestLocalContainerBindingVolumePhases(t *testing.T) {
	for _, provider := range []string{"other", providerName} {
		for _, prior := range [][]string{nil, {}, {" inherited ", "a,b"}} {
			for _, args := range [][]string{{}, {""}, {" raw ", "a,b", "raw", "raw"}} {
				for _, guard := range []string{"", "id", "pool", "both"} {
					cfg := core.Config{Provider: provider, SSHUser: "generic", WorkRoot: "/generic", LocalContainer: core.LocalContainerConfig{Volumes: []string{"cfg-prior"}, CheckpointMetadata: map[string]string{"keep": "same"}}}
					want := cfg
					fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
					fs.String("id", "", "")
					fs.String("pool", "", "")
					v := registerFlags(fs, core.Config{LocalContainer: core.LocalContainerConfig{Volumes: prior}})
					if fs.Lookup("local-container-volume").DefValue != strings.Join(prior, ",") {
						t.Fatal("default text")
					}
					getter := fs.Lookup("local-container-volume").Value.(flag.Getter)
					if getter.Get().([]string) == nil {
						t.Fatal("nil getter")
					}
					for _, raw := range args {
						if err := fs.Set("local-container-volume", raw); err != nil {
							t.Fatal(err)
						}
					}
					if err := fs.Parse([]string{"--local-container-runtime=runtime-fixture", "--local-container-image=image-fixture", "--local-container-user=user-fixture", "--local-container-work-root=/work/fixture", "--local-container-cpus=-2", "--local-container-memory=5g", "--local-container-network=network-fixture"}); err != nil {
						t.Fatal(err)
					}
					if guard == "id" || guard == "both" {
						if err := fs.Set("id", " lease "); err != nil {
							t.Fatal(err)
						}
					}
					if guard == "pool" || guard == "both" {
						if err := fs.Set("pool", " pool "); err != nil {
							t.Fatal(err)
						}
					}
					core.ApplyLocalContainerRuntime(&want, "runtime-fixture")
					core.ApplyLocalContainerImage(&want, "image-fixture")
					core.ApplyLocalContainerWorkRoot(&want, "/work/fixture")
					want.LocalContainer.User = "user-fixture"
					want.SSHUser = "user-fixture"
					want.WorkRoot = "/work/fixture"
					core.MarkWorkRootExplicit(&want)
					want.LocalContainer.CPUs = -2
					want.LocalContainer.Memory = "5g"
					want.LocalContainer.Network = "network-fixture"
					core.RecordProviderFlagInputs(&want, true, providerName)
					list := append(append([]string(nil), prior...), args...)
					bad := len(list) > 0 && guard != ""
					if len(list) > 0 && !bad {
						want.LocalContainer.Volumes = list
					}
					if !bad && provider == providerName {
						applyDefaults(&want)
					}
					err := applyFlags(&cfg, fs, v)
					if (err != nil) != bad {
						t.Fatalf("guard=%s prior=%#v args=%#v err=%v", guard, prior, args, err)
					}
					if bad {
						marker := "--pool"
						if guard == "id" || guard == "both" {
							marker = "--id"
						}
						if !strings.Contains(err.Error(), marker) {
							t.Fatal("guard order")
						}
					}
					if !reflect.DeepEqual(cfg, want) {
						t.Fatalf("partial/default state changed: %+v want %+v", cfg.LocalContainer, want.LocalContainer)
					}
					if len(list) > 0 {
						got := getter.Get().([]string)
						got[0] = "changed"
						if getter.Get().([]string)[0] != list[0] {
							t.Fatal("getter aliases")
						}
					}
				}
			}
		}
	}
}

func TestLocalContainerBindingRegistrationOrder(t *testing.T) {
	order := []string{"runtime", "image", "user", "work-root", "cpus", "memory", "network", "docker-socket", "volume"}
	for i, dup := range order {
		fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("local-container-"+dup, "", "")
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("duplicate missing")
				}
			}()
			registerFlags(fs, core.Config{})
		}()
		for j, name := range order {
			if (fs.Lookup("local-container-"+name) != nil) != (j <= i) {
				t.Fatal("registration order")
			}
		}
		if fs.Lookup("local-container-no-hostname") != nil {
			t.Fatal("new hostname flag")
		}
	}
}

func TestLocalContainerBindingPureForkFlagMapper(t *testing.T) {
	// This mapper only assigns parsed fields; no checkpoint operation is invoked.
	for _, prior := range [][]string{nil, {}, {" inherited "}} {
		for _, extra := range []string{"absent", "", " raw,a "} {
			cfg := core.Config{LocalContainer: core.LocalContainerConfig{Runtime: "keep-runtime", Image: "keep-image", User: "keep-user", WorkRoot: "keep-root", CPUs: 1, Memory: "prior", Network: "prior", Volumes: []string{"prior-cfg"}, CheckpointMetadata: map[string]string{"keep": "value"}}}
			want := cfg
			fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
			v := registerFlags(fs, core.Config{LocalContainer: core.LocalContainerConfig{Volumes: prior}})
			if err := (Provider{}).ApplyNativeCheckpointForkFlags(&cfg, fs, struct{}{}); err != nil || !reflect.DeepEqual(cfg, want) {
				t.Fatal("foreign mapper value")
			}
			if err := fs.Parse([]string{"--local-container-runtime=ignored", "--local-container-image=ignored", "--local-container-work-root=ignored", "--local-container-cpus=-2", "--local-container-memory=memory", "--local-container-network=network"}); err != nil {
				t.Fatal(err)
			}
			list := append([]string(nil), prior...)
			if extra != "absent" {
				if err := fs.Set("local-container-volume", extra); err != nil {
					t.Fatal(err)
				}
				list = append(list, extra)
			}
			want.LocalContainer.CPUs = -2
			want.LocalContainer.Memory = "memory"
			want.LocalContainer.Network = "network"
			if len(list) > 0 {
				want.LocalContainer.Volumes = list
			}
			if err := (Provider{}).ApplyNativeCheckpointForkFlags(&cfg, fs, v); err != nil || !reflect.DeepEqual(cfg, want) {
				t.Fatal("mapper subset changed")
			}
			if len(list) > 0 {
				cfg.LocalContainer.Volumes[0] = "changed"
				if fs.Lookup("local-container-volume").Value.(flag.Getter).Get().([]string)[0] != list[0] {
					t.Fatal("mapper failed to copy")
				}
			}
		}
	}
}
