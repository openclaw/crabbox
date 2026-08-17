package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestDescribeProviderCoversRegisteredRunnableKinds(t *testing.T) {
	for _, provider := range registeredProviders() {
		provider := provider
		t.Run(provider.Name(), func(t *testing.T) {
			description, err := describeProvider(provider.Name())
			switch provider.Spec().Kind {
			case ProviderKindSSHLease, ProviderKindDelegatedRun:
				if err != nil {
					t.Fatalf("describeProvider(%q): %v", provider.Name(), err)
				}
				if !description.Runnable || description.Provider.Canonical != normalizeProviderName(provider.Name()) {
					t.Fatalf("description=%#v", description)
				}
			case ProviderKindServiceControl:
				assertDescribeExitCode(t, err, 2)
				if !strings.Contains(err.Error(), "not runnable") {
					t.Fatalf("service-control error=%v", err)
				}
			default:
				assertDescribeExitCode(t, err, 2)
			}
		})
	}

	_, err := describeProvider("does-not-exist")
	assertDescribeExitCode(t, err, 2)
}

func TestDescribeProviderAliasSchemaCapabilitiesAndFlags(t *testing.T) {
	description, err := describeProvider(" Docker ")
	if err != nil {
		t.Fatal(err)
	}
	if description.SchemaVersion != 1 || description.Provider.Requested != "docker" || description.Provider.Canonical != "local-container" || description.Provider.InputAlias != "docker" {
		t.Fatalf("identity=%#v schema=%d", description.Provider, description.SchemaVersion)
	}
	if description.Provider.Deprecated || description.Provider.Replacement != "" {
		t.Fatalf("provider deprecation=%#v", description.Provider)
	}
	if !description.Runnable || description.Kind != ProviderKindSSHLease || description.Family != "local-container" {
		t.Fatalf("runnable metadata=%#v", description)
	}
	assertSortedStrings(t, "aliases", description.Provider.Aliases)
	assertSortedStrings(t, "targets", description.Targets)
	for name, values := range map[string][]string{
		"features": description.Capabilities.Features, "runtime": description.Capabilities.Runtime,
		"reachability": description.Capabilities.Reachability, "workspace": description.Capabilities.Workspace,
		"evidence": description.Capabilities.Evidence, "lifecycle": description.Capabilities.Lifecycle,
	} {
		if values == nil {
			t.Fatalf("%s is nil", name)
		}
		assertSortedStrings(t, name, values)
	}
	if description.SharedFlags == nil || description.ProviderFlags == nil {
		t.Fatalf("flag arrays must be present: shared=%v provider=%v", description.SharedFlags, description.ProviderFlags)
	}
	assertSortedFlags(t, "shared", description.SharedFlags)
	assertSortedFlags(t, "provider", description.ProviderFlags)

	providerFlags := descriptionFlagMap(description.ProviderFlags)
	for _, name := range []string{
		"local-container-image", "local-container-cpus", "local-container-memory", "local-container-network",
		"local-container-runtime", "local-container-user", "local-container-volume", "local-container-work-root",
		"local-container-docker-socket",
	} {
		if _, ok := providerFlags[name]; !ok {
			t.Errorf("missing provider flag --%s", name)
		}
	}
	for _, unrelated := range []string{"aws-lambda-microvm-image", "azure-backend", "daytona-api-url", "tart-image"} {
		if _, ok := providerFlags[unrelated]; ok {
			t.Errorf("included unrelated provider flag --%s", unrelated)
		}
	}
	volume := providerFlags["local-container-volume"]
	if volume.Type != "string" || volume.ValueShape != "string-list" || !volume.Repeatable || !volume.CreationOnly {
		t.Fatalf("volume metadata=%#v", volume)
	}
	if defaults, ok := volume.Default.([]string); !ok || defaults == nil || len(defaults) != 0 {
		t.Fatalf("volume default=%#v", volume.Default)
	}
	shared := descriptionFlagMap(description.SharedFlags)
	if shared["download"].ValueShape != "string-list" || !shared["download"].Repeatable {
		t.Fatalf("download metadata=%#v", shared["download"])
	}
	if shared["ttl"].Type != "duration" || shared["ttl"].Default != (90*time.Minute).String() {
		t.Fatalf("ttl metadata=%#v", shared["ttl"])
	}
	if _, ok := shared["provider"]; !ok {
		t.Fatal("shared flags omitted --provider")
	}
}

func TestDescribeProviderCanonicalIdentityAndAppleDeprecations(t *testing.T) {
	canonical, err := describeProvider("local-container")
	if err != nil {
		t.Fatal(err)
	}
	if canonical.Provider.InputAlias != "" || canonical.Provider.Requested != "local-container" {
		t.Fatalf("canonical identity=%#v", canonical.Provider)
	}
	empty, err := describeProvider("aws-lambda-microvm")
	if err != nil {
		t.Fatal(err)
	}
	if empty.ProviderFlags == nil || len(empty.ProviderFlags) != 0 {
		t.Fatalf("empty provider flags=%#v", empty.ProviderFlags)
	}

	apple, err := describeProvider("apple-vm")
	if err != nil {
		t.Fatal(err)
	}
	flags := descriptionFlagMap(apple.ProviderFlags)
	for legacy, replacement := range map[string]string{
		"apple-vz-helper": "apple-vm-helper", "apple-vz-image": "apple-vm-image",
		"apple-vz-image-sha256": "apple-vm-image-sha256", "apple-vz-user": "apple-vm-user",
		"apple-vz-work-root": "apple-vm-work-root", "apple-vz-cpus": "apple-vm-cpus",
		"apple-vz-memory": "apple-vm-memory", "apple-vz-disk": "apple-vm-disk",
	} {
		item, ok := flags[legacy]
		if !ok || !item.Deprecated || item.Replacement != replacement {
			t.Errorf("deprecated flag --%s=%#v", legacy, item)
		}
		if _, ok := flags[replacement]; !ok {
			t.Errorf("replacement --%s missing", replacement)
		}
	}
}

func TestDescribeProviderRegistersOnlyAndUsesBaseDefaults(t *testing.T) {
	counters := &describeProviderCounters{}
	provider := countingDescribeProvider{counters: counters}
	providerRegistry[provider.Name()] = provider
	t.Cleanup(func() { delete(providerRegistry, provider.Name()) })
	t.Setenv("CRABBOX_PROVIDER", "ENV_SECRET_MARKER")
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))

	description, err := describeProvider(provider.Name())
	if err != nil {
		t.Fatal(err)
	}
	if counters.register != 1 || counters.apply != 0 || counters.configure != 0 {
		t.Fatalf("provider calls=%#v", counters)
	}
	flag := descriptionFlagMap(description.ProviderFlags)["counting-describe-default"]
	if flag.Default != "default" {
		t.Fatalf("flag default=%#v, want compiled base default", flag.Default)
	}
	encoded, err := json.Marshal(description)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("SECRET_MARKER")) {
		t.Fatalf("description leaked environment/config marker: %s", encoded)
	}
}

func TestProvidersDescribeHumanAndJSONArgumentOrder(t *testing.T) {
	for _, args := range [][]string{{"describe", "docker", "--json"}, {"describe", "--json", "docker"}} {
		var stdout, stderr bytes.Buffer
		err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), args)
		if err != nil {
			t.Fatalf("providers %v: %v stderr=%q", args, err, stderr.String())
		}
		var description providerDescription
		if err := json.Unmarshal(stdout.Bytes(), &description); err != nil {
			t.Fatalf("providers %v JSON: %v\n%s", args, err, stdout.String())
		}
		if description.Provider.Canonical != "local-container" {
			t.Fatalf("providers %v identity=%#v", args, description.Provider)
		}
	}

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).providers(context.Background(), []string{"describe", "docker"})
	if err != nil {
		t.Fatal(err)
	}
	human := stdout.String()
	for _, want := range []string{"docker -> local-container", "kind: ssh-lease", "runnable: true", "Shared run flags:", "local-container flags:", "--local-container-volume", "creation-only: true"} {
		if !strings.Contains(human, want) {
			t.Errorf("human output missing %q\n%s", want, human)
		}
	}
}

func TestDescribeRegisteredFlagSupportedTypesAndFailClosed(t *testing.T) {
	fs := flag.NewFlagSet("metadata", flag.ContinueOnError)
	fs.String("string", "value", "string usage")
	fs.Bool("bool", true, "bool usage")
	fs.Int("int", 3, "int usage")
	fs.Int64("int64", 4, "int64 usage")
	fs.Float64("float64", 1.5, "float usage")
	fs.Duration("duration", 2*time.Minute, "duration usage")
	list := stringListFlag{"one"}
	fs.Var(&list, "list", "list usage")
	wants := map[string]string{"string": "string", "bool": "bool", "int": "int", "int64": "int64", "float64": "float64", "duration": "duration", "list": "string"}
	for name, wantType := range wants {
		record, err := describeRegisteredFlag(fs.Lookup(name))
		if err != nil {
			t.Fatalf("--%s: %v", name, err)
		}
		if record.Type != wantType {
			t.Errorf("--%s type=%q want %q", name, record.Type, wantType)
		}
	}
	got := fs.Lookup("list").Value.(flag.Getter).Get().([]string)
	got[0] = "changed"
	if list[0] != "one" {
		t.Fatal("repeatable getter did not return a defensive copy")
	}

	unsupported := flag.NewFlagSet("unsupported", flag.ContinueOnError)
	unsupported.Func("future", "future usage", func(string) error { return nil })
	if _, err := describeRegisteredFlag(unsupported.Lookup("future")); err == nil || !strings.Contains(err.Error(), "unsupported value type") {
		t.Fatalf("unsupported flag error=%v", err)
	}
}

func TestProviderFlagAnnotationsFailOnDrift(t *testing.T) {
	fs := flag.NewFlagSet("annotations", flag.ContinueOnError)
	fs.String("legacy", "", "legacy")
	assertPanicsContaining(t, "invalid replacement", func() { MarkFlagDeprecated(fs, "legacy", "missing") })
	assertPanicsContaining(t, "unregistered flag", func() { MarkFlagDeprecated(fs, "missing", "legacy") })

	provider := driftContractProvider{}
	if _, err := providerFlagContractNames(provider, map[string]bool{"actual": true}, "routing"); err == nil || !strings.Contains(err.Error(), "unregistered provider flag") {
		t.Fatalf("routing drift error=%v", err)
	}
}

type driftContractProvider struct{}

func (driftContractProvider) Name() string                                 { return "drift" }
func (driftContractProvider) Aliases() []string                            { return nil }
func (driftContractProvider) Spec() ProviderSpec                           { return ProviderSpec{} }
func (driftContractProvider) RegisterFlags(*flag.FlagSet, Config) any      { return NoProviderFlags() }
func (driftContractProvider) ApplyFlags(*Config, *flag.FlagSet, any) error { return nil }
func (driftContractProvider) Configure(Config, Runtime) (Backend, error)   { return nil, nil }
func (driftContractProvider) RoutingFlagNames() []string                   { return []string{"missing"} }

type describeProviderCounters struct {
	register  int
	apply     int
	configure int
}

type countingDescribeProvider struct {
	counters *describeProviderCounters
}

func (p countingDescribeProvider) Name() string      { return "counting-describe" }
func (p countingDescribeProvider) Aliases() []string { return nil }
func (p countingDescribeProvider) Spec() ProviderSpec {
	return ProviderSpec{Name: p.Name(), Kind: ProviderKindSSHLease, Targets: []TargetSpec{{OS: targetLinux}}}
}
func (p countingDescribeProvider) RegisterFlags(fs *flag.FlagSet, defaults Config) any {
	p.counters.register++
	return fs.String("counting-describe-default", defaults.Profile, "compiled default probe")
}
func (p countingDescribeProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	p.counters.apply++
	return nil
}
func (p countingDescribeProvider) Configure(Config, Runtime) (Backend, error) {
	p.counters.configure++
	return nil, nil
}

func descriptionFlagMap(values []providerDescriptionFlag) map[string]providerDescriptionFlag {
	out := make(map[string]providerDescriptionFlag, len(values))
	for _, value := range values {
		out[value.Name] = value
	}
	return out
}

func assertSortedStrings(t *testing.T, name string, values []string) {
	t.Helper()
	want := append([]string(nil), values...)
	sort.Strings(want)
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("%s not sorted: %v", name, values)
	}
}

func assertSortedFlags(t *testing.T, name string, values []providerDescriptionFlag) {
	t.Helper()
	for i := 1; i < len(values); i++ {
		if values[i-1].Name >= values[i].Name {
			t.Fatalf("%s flags not sorted at %q, %q", name, values[i-1].Name, values[i].Name)
		}
	}
}

func assertDescribeExitCode(t *testing.T, err error, code int) {
	t.Helper()
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != code {
		t.Fatalf("error=%v, want exit %d", err, code)
	}
}

func assertPanicsContaining(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		got := recover()
		if got == nil || !strings.Contains(got.(string), want) {
			t.Fatalf("panic=%v, want substring %q", got, want)
		}
	}()
	fn()
}
