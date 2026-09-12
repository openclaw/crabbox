package localcontainer

import (
	"bytes"
	"encoding/json"
	"flag"
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
