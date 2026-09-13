package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	preflightDefaultSelector = "default"
	preflightDefaultsAlias   = "defaults"
	preflightNoneSelector    = "none"
)

type preflightToolEntry struct {
	Name    string   `json:"name"`
	Default bool     `json:"default"`
	Targets []string `json:"targets"`
}

type preflightSelectorEntry struct {
	Name        string   `json:"name"`
	Aliases     []string `json:"aliases"`
	Description string   `json:"description"`
}

type preflightToolCatalog struct {
	Tools        []preflightToolEntry     `json:"tools"`
	DefaultTools []string                 `json:"defaultTools"`
	Selectors    []preflightSelectorEntry `json:"selectors"`
}

func preflightCatalog() preflightToolCatalog {
	catalog := preflightToolCatalog{
		Tools:        make([]preflightToolEntry, 0, len(preflightToolRegistry)),
		DefaultTools: append([]string{}, defaultPreflightToolNames...),
		Selectors: []preflightSelectorEntry{
			{preflightDefaultSelector, []string{preflightDefaultsAlias}, "Expand the ordered default tool list."},
			{preflightNoneSelector, []string{}, "Disable tool probes when used alone; when mixed with other names, those names remain selected."},
		},
	}
	defaults := make(map[string]bool, len(defaultPreflightToolNames))
	for _, name := range defaultPreflightToolNames {
		defaults[name] = true
	}
	targets := []struct {
		name   string
		target SSHTarget
	}{
		{"linux", SSHTarget{TargetOS: targetLinux}},
		{"macos", SSHTarget{TargetOS: targetMacOS}},
		{"windows/normal", SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}},
		{"windows/wsl2", SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}},
	}
	for name, spec := range preflightToolRegistry {
		entry := preflightToolEntry{Name: name, Default: defaults[name], Targets: []string{}}
		for _, target := range targets {
			if spec.supports(preflightOSKind(target.target)) {
				entry.Targets = append(entry.Targets, target.name)
			}
		}
		catalog.Tools = append(catalog.Tools, entry)
	}
	sort.Slice(catalog.Tools, func(i, j int) bool { return catalog.Tools[i].Name < catalog.Tools[j].Name })
	return catalog
}

func (a App) preflightTools(args []string) error {
	fs := newFlagSet("preflight-tools", a.Stderr)
	jsonOut := fs.Bool("json", false, "print the compiled tool catalog as JSON")
	fs.Usage = func() {
		fmt.Fprintln(a.Stderr, "Usage: crabbox preflight-tools [--json]\n\nList accepted preflight names, defaults, and target support offline.\nNo probes run; no configuration or provider is loaded.\n\nExamples:\n  crabbox preflight-tools\n  crabbox preflight-tools --json\n\nFlags:")
		fs.PrintDefaults()
	}
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return parseFlags(fs, []string{"--help"})
		}
	}
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Exit(2, "usage: crabbox preflight-tools [--json]")
	}
	catalog := preflightCatalog()
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(catalog)
	}
	var out strings.Builder
	fmt.Fprintln(&out, "Compiled preflight tools (no probes run):")
	fmt.Fprintf(&out, "%-18s %-7s %s\n", "NAME", "DEFAULT", "TARGETS")
	for _, tool := range catalog.Tools {
		member := "no"
		if tool.Default {
			member = "yes"
		}
		fmt.Fprintf(&out, "%-18s %-7s %s\n", tool.Name, member, strings.Join(tool.Targets, ", "))
	}
	fmt.Fprintf(&out, "\nDefault expansion: %s\n\nSelectors:\n", strings.Join(catalog.DefaultTools, ", "))
	for _, selector := range catalog.Selectors {
		name := selector.Name
		if len(selector.Aliases) > 0 {
			name += " (alias: " + strings.Join(selector.Aliases, ", ") + ")"
		}
		fmt.Fprintf(&out, "  %s: %s\n", name, selector.Description)
	}
	fmt.Fprintln(&out, "\nOmitted settings inherit lower layers; an empty-string CLI flag keeps the resolved configuration.\nNonempty whitespace/comma-only CLI input selects defaults; YAML [] disables tool probes.\nTarget support describes compiled behavior, not installed tools or readiness.")
	_, err := io.WriteString(a.Stdout, out.String())
	return err
}
