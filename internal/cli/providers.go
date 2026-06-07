package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type providerMatrixEntry struct {
	Provider    string       `json:"provider"`
	Family      string       `json:"family"`
	Aliases     []string     `json:"aliases,omitempty"`
	Kind        ProviderKind `json:"kind"`
	Targets     []string     `json:"targets"`
	Features    []Feature    `json:"features"`
	Coordinator string       `json:"coordinator"`
}

func (a App) providers(_ context.Context, args []string) error {
	fs := newFlagSet("providers", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return exit(2, "usage: crabbox providers [--json]")
	}
	entries := providerMatrix()
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(entries)
	}
	printProviderMatrix(a.Stdout, entries)
	return nil
}

func providerMatrix() []providerMatrixEntry {
	providers := registeredProviders()
	entries := make([]providerMatrixEntry, 0, len(providers))
	for _, provider := range providers {
		spec := provider.Spec()
		entries = append(entries, providerMatrixEntry{
			Provider:    firstNonBlank(spec.Name, provider.Name()),
			Family:      firstNonBlank(spec.Family, provider.Name()),
			Aliases:     append([]string(nil), provider.Aliases()...),
			Kind:        spec.Kind,
			Targets:     formatProviderTargets(spec.Targets),
			Features:    append(FeatureSet{}, spec.Features...),
			Coordinator: string(spec.Coordinator),
		})
	}
	return entries
}

func printProviderMatrix(out io.Writer, entries []providerMatrixEntry) {
	for _, entry := range entries {
		fmt.Fprintf(out, "%s\n", entry.Provider)
		fmt.Fprintf(out, "  family: %s\n", entry.Family)
		fmt.Fprintf(out, "  kind: %s\n", entry.Kind)
		fmt.Fprintf(out, "  targets: %s\n", commaOrDash(entry.Targets))
		fmt.Fprintf(out, "  features: %s\n", commaOrDash(featuresToStrings(entry.Features)))
		fmt.Fprintf(out, "  coordinator: %s\n", blank(entry.Coordinator, "never"))
		if len(entry.Aliases) > 0 {
			fmt.Fprintf(out, "  aliases: %s\n", strings.Join(entry.Aliases, ","))
		}
	}
}

func formatProviderTargets(targets []TargetSpec) []string {
	out := make([]string, 0, len(targets))
	for _, target := range targets {
		value := strings.TrimSpace(target.OS)
		if value == "" {
			continue
		}
		if strings.TrimSpace(target.WindowsMode) != "" {
			value += "/" + strings.TrimSpace(target.WindowsMode)
		}
		out = append(out, value)
	}
	return out
}

func featuresToStrings(features []Feature) []string {
	out := make([]string, 0, len(features))
	for _, feature := range features {
		out = append(out, string(feature))
	}
	return out
}

func commaOrDash(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ",")
}
