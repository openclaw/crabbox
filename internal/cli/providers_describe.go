package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const providerDescriptionSchemaVersion = 1

type providerDescription struct {
	SchemaVersion int                         `json:"schemaVersion"`
	Provider      providerDescriptionIdentity `json:"provider"`
	Runnable      bool                        `json:"runnable"`
	Kind          ProviderKind                `json:"kind"`
	Family        string                      `json:"family"`
	Targets       []string                    `json:"targets"`
	Capabilities  providerDescriptionCaps     `json:"capabilities"`
	SharedFlags   []providerDescriptionFlag   `json:"sharedFlags"`
	ProviderFlags []providerDescriptionFlag   `json:"providerFlags"`
}

type providerDescriptionIdentity struct {
	Requested   string   `json:"requested"`
	Canonical   string   `json:"canonical"`
	InputAlias  string   `json:"inputAlias"`
	Aliases     []string `json:"aliases"`
	Deprecated  bool     `json:"deprecated"`
	Replacement string   `json:"replacement"`
}

type providerDescriptionCaps struct {
	Features     []string `json:"features"`
	Runtime      []string `json:"runtime"`
	Reachability []string `json:"reachability"`
	Workspace    []string `json:"workspace"`
	Evidence     []string `json:"evidence"`
	Lifecycle    []string `json:"lifecycle"`
	Coordinator  string   `json:"coordinator"`
}

type providerDescriptionFlag struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	ValueShape   string `json:"valueShape"`
	Default      any    `json:"default"`
	Repeatable   bool   `json:"repeatable"`
	Usage        string `json:"usage"`
	Deprecated   bool   `json:"deprecated"`
	Replacement  string `json:"replacement"`
	Routing      bool   `json:"routing"`
	CreationOnly bool   `json:"creationOnly"`
}

func (a App) providerDescribe(args []string) error {
	fs := newFlagSet("providers describe", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	flagArgs, positional := splitProviderDescribeArgs(args)
	if err := parseFlags(fs, append(flagArgs, positional...)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox providers describe <provider> [--json]")
	}
	description, err := describeProvider(fs.Arg(0))
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(description)
	}
	printProviderDescription(a.Stdout, description)
	return nil
}

func splitProviderDescribeArgs(args []string) (flagArgs, positional []string) {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
		} else {
			positional = append(positional, arg)
		}
	}
	return flagArgs, positional
}

func describeProvider(requestedName string) (providerDescription, error) {
	requested := normalizeProviderName(requestedName)
	if requested == "" {
		return providerDescription{}, exit(2, "provider name must not be empty")
	}
	provider, err := ProviderFor(requested)
	if err != nil {
		return providerDescription{}, err
	}
	canonical := normalizeProviderName(provider.Name())
	spec := provider.Spec()
	switch spec.Kind {
	case ProviderKindSSHLease, ProviderKindDelegatedRun:
	case ProviderKindServiceControl:
		return providerDescription{}, exit(2, "provider %q is not runnable (kind %s); providers describe supports ssh-lease and delegated-run providers", canonical, spec.Kind)
	default:
		return providerDescription{}, exit(2, "provider %q has unsupported kind %q; providers describe supports ssh-lease and delegated-run providers", canonical, spec.Kind)
	}

	providerOwned := map[string]map[string]bool{}
	defaults := baseConfig()
	runFlags := newFlagSet("run", io.Discard)
	defer clearFlagAnnotations(runFlags)
	registerRunFlags(runFlags, defaults, leaseCreateFlagRegistrationOptions{
		serverTypeDefault: defaults.ServerType,
		observe: func(owner Provider, added []*flag.Flag) {
			owned := map[string]bool{}
			for _, item := range added {
				owned[item.Name] = true
			}
			providerOwned[normalizeProviderName(owner.Name())] = owned
		},
	})

	routing, err := providerFlagContractNames(provider, providerOwned[canonical], "routing")
	if err != nil {
		return providerDescription{}, err
	}
	creationOnly, err := providerFlagContractNames(provider, providerOwned[canonical], "creation-only")
	if err != nil {
		return providerDescription{}, err
	}

	allProviderFlags := map[string]bool{}
	for _, owned := range providerOwned {
		for name := range owned {
			allProviderFlags[name] = true
		}
	}
	shared := make([]providerDescriptionFlag, 0)
	selected := make([]providerDescriptionFlag, 0)
	var metadataErr error
	runFlags.VisitAll(func(item *flag.Flag) {
		if metadataErr != nil {
			return
		}
		if allProviderFlags[item.Name] && !providerOwned[canonical][item.Name] {
			return
		}
		record, recordErr := describeRegisteredFlag(item)
		if recordErr != nil {
			metadataErr = recordErr
			return
		}
		record.Routing = routing[item.Name]
		record.CreationOnly = creationOnly[item.Name]
		if providerOwned[canonical][item.Name] {
			selected = append(selected, record)
		} else {
			shared = append(shared, record)
		}
	})
	if metadataErr != nil {
		return providerDescription{}, metadataErr
	}
	sort.Slice(shared, func(i, j int) bool { return shared[i].Name < shared[j].Name })
	sort.Slice(selected, func(i, j int) bool { return selected[i].Name < selected[j].Name })
	if shared == nil {
		shared = []providerDescriptionFlag{}
	}
	if selected == nil {
		selected = []providerDescriptionFlag{}
	}

	entry := providerMatrixEntryFor(provider)
	aliases := normalizedSortedStrings(provider.Aliases())
	inputAlias := ""
	if requested != canonical {
		inputAlias = requested
	}
	return providerDescription{
		SchemaVersion: providerDescriptionSchemaVersion,
		Provider: providerDescriptionIdentity{
			Requested:   requested,
			Canonical:   canonical,
			InputAlias:  inputAlias,
			Aliases:     aliases,
			Deprecated:  false,
			Replacement: "",
		},
		Runnable: true,
		Kind:     spec.Kind,
		Family:   normalizeProviderName(firstNonBlank(spec.Family, canonical)),
		Targets:  normalizedSortedStrings(entry.Targets),
		Capabilities: providerDescriptionCaps{
			Features:     normalizedSortedStrings(featuresToStrings(entry.Features)),
			Runtime:      normalizedSortedStrings(entry.Runtime),
			Reachability: normalizedSortedStrings(entry.Reachability),
			Workspace:    normalizedSortedStrings(entry.Workspace),
			Evidence:     normalizedSortedStrings(entry.Evidence),
			Lifecycle:    normalizedSortedStrings(entry.Lifecycle),
			Coordinator:  firstNonBlank(entry.Coordinator, string(CoordinatorNever)),
		},
		SharedFlags:   shared,
		ProviderFlags: selected,
	}, nil
}

func providerFlagContractNames(provider Provider, owned map[string]bool, contract string) (map[string]bool, error) {
	result := map[string]bool{}
	var names []string
	switch contract {
	case "routing":
		if source, ok := provider.(ProviderRoutingFlagProvider); ok {
			names = source.RoutingFlagNames()
		}
	case "creation-only":
		if source, ok := provider.(ProviderCreationOnlyFlagProvider); ok {
			names = source.CreationOnlyFlagNames()
		}
	}
	for _, raw := range names {
		name := strings.TrimLeft(strings.TrimSpace(raw), "-")
		if name == "" || !owned[name] {
			return nil, exit(2, "provider %q %s flag annotation references unregistered provider flag --%s", provider.Name(), contract, name)
		}
		result[name] = true
	}
	return result, nil
}

func describeRegisteredFlag(item *flag.Flag) (providerDescriptionFlag, error) {
	getter, ok := item.Value.(flag.Getter)
	if !ok {
		return providerDescriptionFlag{}, exit(2, "run flag --%s uses unsupported value type %T; implement flag.Getter with a supported typed value", item.Name, item.Value)
	}
	value := getter.Get()
	record := providerDescriptionFlag{
		Name:        item.Name,
		ValueShape:  "scalar",
		Usage:       item.Usage,
		Replacement: "",
	}
	switch typed := value.(type) {
	case string:
		record.Type = "string"
		record.Default = typed
	case bool:
		record.Type = "bool"
		record.Default = typed
	case int:
		record.Type = "int"
		record.Default = typed
	case int64:
		record.Type = "int64"
		record.Default = typed
	case float64:
		record.Type = "float64"
		record.Default = typed
	case time.Duration:
		record.Type = "duration"
		record.Default = typed.String()
	case []string:
		record.Type = "string"
		record.ValueShape = "string-list"
		record.Default = append([]string{}, typed...)
		record.Repeatable = true
	default:
		return providerDescriptionFlag{}, exit(2, "run flag --%s uses unsupported getter value type %T", item.Name, value)
	}
	annotation := annotationForFlag(item)
	if annotation.Deprecated {
		if item.Name == annotation.Replacement || item.Value == nil {
			return providerDescriptionFlag{}, exit(2, "run flag --%s has invalid deprecation metadata", item.Name)
		}
		record.Deprecated = true
		record.Replacement = annotation.Replacement
	}
	return record, nil
}

func normalizedSortedStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func printProviderDescription(out io.Writer, description providerDescription) {
	identity := description.Provider
	if identity.InputAlias != "" {
		fmt.Fprintf(out, "%s -> %s\n", identity.Requested, identity.Canonical)
	} else {
		fmt.Fprintln(out, identity.Canonical)
	}
	fmt.Fprintf(out, "  kind: %s\n", description.Kind)
	fmt.Fprintf(out, "  runnable: %t\n", description.Runnable)
	fmt.Fprintf(out, "  family: %s\n", description.Family)
	fmt.Fprintf(out, "  aliases: %s\n", commaOrDash(identity.Aliases))
	fmt.Fprintf(out, "  deprecated: %t\n", identity.Deprecated)
	fmt.Fprintf(out, "  replacement: %s\n", blank(identity.Replacement, "-"))
	fmt.Fprintf(out, "  targets: %s\n", commaOrDash(description.Targets))
	fmt.Fprintf(out, "  features: %s\n", commaOrDash(description.Capabilities.Features))
	fmt.Fprintf(out, "  runtime: %s\n", commaOrDash(description.Capabilities.Runtime))
	fmt.Fprintf(out, "  reachability: %s\n", commaOrDash(description.Capabilities.Reachability))
	fmt.Fprintf(out, "  workspace: %s\n", commaOrDash(description.Capabilities.Workspace))
	fmt.Fprintf(out, "  evidence: %s\n", commaOrDash(description.Capabilities.Evidence))
	fmt.Fprintf(out, "  lifecycle: %s\n", commaOrDash(description.Capabilities.Lifecycle))
	fmt.Fprintf(out, "  coordinator: %s\n", description.Capabilities.Coordinator)
	printProviderDescriptionFlags(out, "Shared run flags", description.SharedFlags)
	printProviderDescriptionFlags(out, description.Provider.Canonical+" flags", description.ProviderFlags)
}

func printProviderDescriptionFlags(out io.Writer, title string, flags []providerDescriptionFlag) {
	fmt.Fprintf(out, "\n%s:\n", title)
	if len(flags) == 0 {
		fmt.Fprintln(out, "  (none)")
		return
	}
	for _, item := range flags {
		encodedDefault, _ := json.Marshal(item.Default)
		fmt.Fprintf(out, "  --%s\n", item.Name)
		fmt.Fprintf(out, "    type: %s; value shape: %s; default: %s; repeatable: %t\n", item.Type, item.ValueShape, encodedDefault, item.Repeatable)
		fmt.Fprintf(out, "    deprecated: %t; replacement: %s; routing: %t; creation-only: %t\n", item.Deprecated, blank(item.Replacement, "-"), item.Routing, item.CreationOnly)
		fmt.Fprintf(out, "    %s\n", item.Usage)
	}
}
