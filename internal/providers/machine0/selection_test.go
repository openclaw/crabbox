package machine0

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	_ "github.com/openclaw/crabbox/internal/providers/namespace"
)

// Resource facts are from the native sizes --all --json contract; capacity is a fixture.
const selectionCatalog = `[
 {"size":"large","vcpu":2,"ramGb":4,"diskGb":80,"regions":["eu","us-east"],"pricePerHourMicro":52000},
 {"size":"xxxl","vcpu":16,"ramGb":64,"diskGb":200,"regions":["eu"],"pricePerHourMicro":825000},
 {"size":"gpu-h100-1","vcpu":20,"ramGb":240,"diskGb":720,"gpu":{"label":"1x H100","vramGb":80,"scratchDiskGb":5000},"regions":[],"pricePerHourMicro":4851000}
]`

func selectionCLI(t *testing.T, config string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is POSIX-only")
	}
	setupState(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", path)
	for _, name := range []string{"CRABBOX_PROVIDER", "CRABBOX_SERVER_TYPE", "CRABBOX_DEFAULT_CLASS", "CRABBOX_MACHINE0_SIZE", "CRABBOX_MACHINE0_REGION"} {
		t.Setenv(name, "")
	}
	path = filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(path, []byte(selectionCatalog), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
cd "$(dirname "$0")" || exit 1
printf '%s\n' "$*" >> calls
case "$*" in
 'sizes --all --json') cat catalog.json ;;
 *) echo "unexpected fixture command: $*" >&2; exit 1 ;;
esac
`
	path = filepath.Join(dir, "machine0")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_MACHINE0_CLI", path)
	return dir
}

func runSelectionCLI(args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), args)
	return stdout.String(), err
}

func TestCLIProvidersSizesUsesMachine0LiveCatalog(t *testing.T) {
	selectionCLI(t, "provider: machine0\n")
	for _, tc := range []struct {
		name      string
		flags     []string
		wantCount int
	}{
		{"available array", []string{"--json"}, 2},
		{"all array", []string{"--all", "--refresh", "--json"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := runSelectionCLI(append([]string{"providers", "sizes", "machine0"}, tc.flags...)...)
			if err != nil {
				t.Fatal(err)
			}
			var sizes []core.ProviderSize
			if err := json.Unmarshal([]byte(output), &sizes); err != nil || len(sizes) != tc.wantCount {
				t.Fatalf("array=%s err=%v", output, err)
			}
			if sizes[0].Name != "large" || sizes[0].VCPU != 2 || sizes[0].RAMGB != 4 || sizes[0].DiskGB != 80 || sizes[0].PricePerHourMicro != 52000 {
				t.Fatalf("resources=%#v", sizes[0])
			}
		})
	}
	output, err := runSelectionCLI("providers", "sizes", "machine0")
	const want = "SIZE\tCPU\tGPU\tRAM_GB\tDISK_GB\tPRICE_PER_HOUR\tREGIONS\nlarge\t2\t-\t4\t80\t$0.052\teu,us-east\nxxxl\t16\t-\t64\t200\t$0.825\teu\n"
	if err != nil || output != want {
		t.Fatalf("human=%q err=%v", output, err)
	}
}

func TestCLIProvidersSizesSelectionContext(t *testing.T) {
	for _, tc := range []struct{ name, config, envSize, envType, class, wantType, region string }{
		{name: "native default", wantType: "large", region: "eu"},
		{name: "class overrides configured class", config: "class: tiny\n", class: "fast", wantType: "xxxl", region: "eu"},
		{name: "YAML native large wins class", config: "machine0:\n  size: large\n  region: us-east\n", class: "fast", wantType: "large", region: "us-east"},
		{name: "environment native large wins class", envSize: "large", class: "fast", wantType: "large", region: "eu"},
		{name: "generic type wins native config", envSize: "large", envType: "gpu-h100-1", class: "fast", wantType: "gpu-h100-1", region: "eu"},
		{name: "unavailable native remains selected", envSize: "gpu-h100-1", class: "fast", wantType: "gpu-h100-1", region: "eu"},
		{name: "unknown configured type is honest", envSize: "future-native", class: "fast", wantType: "future-native", region: "eu"},
		{name: "select provider before defaults", config: "provider: namespace-devbox\nnamespace:\n  size: XL\n", class: "fast", wantType: "xxxl", region: "eu"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := selectionCLI(t, tc.config)
			t.Setenv("CRABBOX_MACHINE0_SIZE", tc.envSize)
			t.Setenv("CRABBOX_SERVER_TYPE", tc.envType)
			args := []string{"providers", "sizes", "machine0", "--with-context", "--json", "--all", "--refresh"}
			if tc.class != "" {
				args = append(args, "--class", tc.class)
			}
			output, err := runSelectionCLI(args...)
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Sizes     []core.ProviderSize `json:"sizes"`
				Selection map[string]string   `json:"selection"`
			}
			if err := json.Unmarshal([]byte(output), &envelope); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"selector": "type", "effectiveType": tc.wantType, "region": tc.region}
			if !reflect.DeepEqual(envelope.Selection, want) {
				t.Fatalf("selection=%v want=%v", envelope.Selection, want)
			}
			legacy, err := runSelectionCLI("providers", "sizes", "machine0", "--all", "--json")
			if err != nil {
				t.Fatal(err)
			}
			var sizes []core.ProviderSize
			if err := json.Unmarshal([]byte(legacy), &sizes); err != nil {
				t.Fatal(err)
			}
			if len(envelope.Sizes) != 3 || !reflect.DeepEqual(envelope.Sizes, sizes) {
				t.Fatalf("context changed catalog: %s", output)
			}
			gpu := envelope.Sizes[2]
			if gpu.Name != "gpu-h100-1" || gpu.VCPU != 20 || gpu.RAMGB != 240 || gpu.DiskGB != 720 || gpu.PricePerHourMicro != 4851000 || gpu.GPU == nil || gpu.GPU.Label != "1x H100" || gpu.GPU.VRAMGB != 80 || gpu.GPU.ScratchDiskGB != 5000 || len(gpu.Regions) != 0 {
				t.Fatalf("GPU resources=%#v", gpu)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil || string(calls) != "sizes --all --json\nsizes --all --json\n" {
				t.Fatalf("unexpected provider calls=%q err=%v", calls, err)
			}
		})
	}
}

func TestCLIProvidersSizesContextFailures(t *testing.T) {
	for _, tc := range []struct {
		name, provider, catalog, wantError string
		flags                              []string
	}{
		{name: "JSON required", provider: "machine0", flags: []string{"--with-context"}, wantError: "--with-context requires --json"},
		{name: "unsupported provider", provider: "namespace-devbox", flags: []string{"--with-context", "--json"}, wantError: "does not expose native size selection"},
		{name: "catalog failure", provider: "machine0", catalog: "not JSON", flags: []string{"--with-context", "--json"}, wantError: "parse machine0 sizes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := selectionCLI(t, "")
			if tc.catalog != "" {
				if err := os.WriteFile(filepath.Join(dir, "catalog.json"), []byte(tc.catalog), 0600); err != nil {
					t.Fatal(err)
				}
			}
			output, err := runSelectionCLI(append([]string{"providers", "sizes", tc.provider}, tc.flags...)...)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) || output != "" {
				t.Fatalf("stdout=%q err=%v want=%s", output, err, tc.wantError)
			}
		})
	}
}

func TestCLIMachine0NativeSelectionTransport(t *testing.T) {
	for _, fork := range []bool{false, true} {
		for _, tc := range []struct {
			name, config, envSize, envType, want string
			flags                                []string
		}{
			{name: "YAML native default wins portable class", config: "machine0:\n  size: large\n", want: "large"},
			{name: "environment native default wins portable class", envSize: "large", want: "large"},
			{name: "explicit type wins native default", envSize: "large", flags: []string{"--type", "xxxl"}, want: "xxxl"},
			{name: "native CLI wins inherited generic type", envSize: "large", envType: "xxxl", flags: []string{"--machine0-size", "large"}, want: "large"},
			{name: "generic CLI after native CLI", flags: []string{"--machine0-size", "large", "--type", "gpu-h100-1"}, want: "gpu-h100-1"},
			{name: "generic CLI before native CLI", flags: []string{"--type", "gpu-h100-1", "--machine0-size", "large"}, want: "gpu-h100-1"},
			{name: "future catalog type needs no Crabbox enum", flags: []string{"--type", "future-native"}, want: "future-native"},
		} {
			lane := "cold/"
			if fork {
				lane = "fork/"
			}
			t.Run(lane+tc.name, func(t *testing.T) {
				dir := selectionCLI(t, "provider: machine0\n"+tc.config)
				t.Setenv("CRABBOX_MACHINE0_SIZE", tc.envSize)
				t.Setenv("CRABBOX_SERVER_TYPE", tc.envType)
				// Forward-compatible native IDs carry owner resource facts, never inferred class specs.
				catalog := strings.Replace(selectionCatalog, `"regions":[]`, `"regions":["eu"]`, 1)
				catalog = strings.TrimSuffix(catalog, "]") + `,{"size":"future-native","vcpu":2,"ramGb":4,"diskGb":80,"regions":["eu"],"pricePerHourMicro":52000}]`
				if err := os.WriteFile(filepath.Join(dir, "catalog.json"), []byte(catalog), 0600); err != nil {
					t.Fatal(err)
				}
				script := `#!/bin/sh
cd "$(dirname "$0")" || exit 1
case "$*" in
 'sizes --all --json') cat catalog.json ;;
 'keys ls --json') printf '%s\n' '[{"name":"ci","type":"MANAGED","isDefault":true}]' ;;
 'keys get ci --json') printf '%s\n' '{"name":"ci","type":"MANAGED"}' ;;
 'ls --json') printf '[]\n' ;;
 new\ *) printf '%s\n' "$@" > create-argv; echo 'fixture create boundary' >&2; exit 1 ;;
 *) echo "unexpected fixture command: $*" >&2; exit 1 ;;
esac
`
				if err := os.WriteFile(filepath.Join(dir, "machine0"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				args := []string{"warmup"}
				wantImage, wantRegion := "ubuntu-24-04-loaded", "eu"
				if fork {
					const checkpointID = "chk_abcdef1234567890"
					state, err := core.CrabboxStateDir()
					if err != nil {
						t.Fatal(err)
					}
					checkpointDir := filepath.Join(state, "checkpoints", checkpointID)
					if err := os.MkdirAll(checkpointDir, 0700); err != nil {
						t.Fatal(err)
					}
					record := `{"id":"chk_abcdef1234567890","kind":"machine0-image","provider":"machine0","targetOS":"linux","serverType":"xxxl","createdAt":"2026-08-27T00:00:00Z","native":{"provider":"machine0","direct":true,"imageId":"img-fixture","region":"eu","metadata":{"machine0_image_name":"fixture-checkpoint","machine0_image_version":"3"}}}`
					if err := os.WriteFile(filepath.Join(checkpointDir, "checkpoint.json"), []byte(record), 0600); err != nil {
						t.Fatal(err)
					}
					args = []string{"checkpoint", "fork", checkpointID}
					wantImage = "fixture-checkpoint"
					t.Setenv("CRABBOX_MACHINE0_REGION", "us-east")
				}
				args = append(args, "--provider", "machine0", "--class", "fast")
				args = append(args, tc.flags...)
				_, err := runSelectionCLI(args...)
				if err == nil || !strings.Contains(err.Error(), "fixture create boundary") {
					t.Fatalf("did not reach create: %v", err)
				}
				data, err := os.ReadFile(filepath.Join(dir, "create-argv"))
				if err != nil {
					t.Fatal(err)
				}
				argv := strings.Split(strings.TrimSpace(string(data)), "\n")
				for flag, want := range map[string]string{"--size": tc.want, "--region": wantRegion, "--image": wantImage} {
					found := ""
					for i := 0; i+1 < len(argv); i++ {
						if argv[i] == flag {
							found = argv[i+1]
							break
						}
					}
					if found != want {
						t.Errorf("%s=%q want=%q argv=%v", flag, found, want, argv)
					}
				}
				if fork && !strings.Contains(string(data), "--image-version\n3\n") {
					t.Errorf("fork omitted version: %s", data)
				}
			})
		}
	}
}
