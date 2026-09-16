package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestPreflightCatalogMatchesRegistry(t *testing.T) {
	catalog := preflightCatalog()
	if len(catalog.Tools) != len(preflightToolRegistry) || !reflect.DeepEqual(catalog.DefaultTools, defaultPreflightToolNames) {
		t.Fatal("catalogue lost registry names or default ordering")
	}
	var names []string
	for _, entry := range catalog.Tools {
		names = append(names, entry.Name)
		if _, ok := preflightToolRegistry[entry.Name]; !ok || entry.Default != slices.Contains(defaultPreflightToolNames, entry.Name) {
			t.Fatalf("unexpected registry/default entry: %+v", entry)
		}
		want := []string{}
		for _, target := range []struct{ label, os, mode string }{
			{"linux", targetLinux, ""}, {"macos", targetMacOS, ""},
			{"windows/normal", targetWindows, windowsModeNormal}, {"windows/wsl2", targetWindows, windowsModeWSL2},
		} {
			if len(preflightToolsForTarget(SSHTarget{TargetOS: target.os, WindowsMode: target.mode}, []string{entry.Name})) != 0 {
				want = append(want, target.label)
			}
		}
		if !reflect.DeepEqual(entry.Targets, want) {
			t.Fatalf("targets for %s=%v, selection uses %v", entry.Name, entry.Targets, want)
		}
	}
	if !slices.IsSorted(names) || len(slices.Compact(slices.Clone(names))) != len(names) {
		t.Fatal("registry names must appear once, in deterministic order")
	}
	for _, name := range catalog.DefaultTools {
		if _, ok := preflightToolRegistry[name]; !ok {
			t.Fatalf("default %q is not registered", name)
		}
	}
	if !slices.Contains(names, "bubblewrap") || !slices.Contains(names, "bwrap") {
		t.Fatal("distinct accepted bubblewrap spellings were collapsed")
	}
	for _, selector := range catalog.Selectors {
		for _, name := range append([]string{selector.Name}, selector.Aliases...) {
			if _, ok := preflightToolRegistry[name]; ok {
				t.Fatalf("reserved selector %q shadows a registered probe", name)
			}
			if err := validatePreflightTools([]string{name}); err != nil {
				t.Fatalf("advertised selector %q rejected: %v", name, err)
			}
		}
	}
	if got := normalizePreflightToolNames([]string{" DeFaUlTs "}); !reflect.DeepEqual(got, defaultPreflightToolNames) {
		t.Fatalf("default alias changed: %v", got)
	}
	if got := preflightToolsForTarget(SSHTarget{TargetOS: targetLinux}, []string{"none", "git"}); !reflect.DeepEqual(got, []string{"git"}) {
		t.Fatalf("mixed none selector changed: %v", got)
	}
	if got := preflightToolsForTarget(SSHTarget{TargetOS: targetLinux}, []string{}); len(got) != 0 {
		t.Fatalf("explicit empty list enabled defaults: %v", got)
	}
	catalog.DefaultTools[0] = "changed-copy"
	if defaultPreflightToolNames[0] == "changed-copy" {
		t.Fatal("catalogue exposes mutable default storage")
	}
}

func TestPreflightCatalogReflectsRegistryChanges(t *testing.T) {
	const name = "test_catalog_metadata_only"
	if _, ok := preflightToolRegistry[name]; ok {
		t.Fatal("test key is already registered")
	}
	preflightToolRegistry[name] = preflightToolSpec{OS: map[string]bool{"linux": true}}
	t.Cleanup(func() { delete(preflightToolRegistry, name) })
	if err := validatePreflightTools([]string{name}); err != nil {
		t.Fatal(err)
	}
	for _, tool := range preflightCatalog().Tools {
		if tool.Name == name {
			if tool.Default || !reflect.DeepEqual(tool.Targets, []string{"linux", "windows/wsl2"}) {
				t.Fatalf("OS-only metadata=%+v", tool)
			}
			return
		}
	}
	t.Fatal("registry addition did not appear in discovery")
}

func TestPreflightToolsCommandOffline(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	config := filepath.Join(dir, "invalid.yaml")
	broken := []byte("broker: [invalid\n")
	if err := os.WriteFile(config, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", config)
	t.Setenv("CRABBOX_PROVIDER", "not-a-provider")
	t.Setenv("PATH", t.TempDir())
	for _, jsonOut := range []bool{false, true} {
		var out, stderr bytes.Buffer
		args := []string{"preflight-tools"}
		if jsonOut {
			args = append(args, "--json")
		}
		if err := (App{Stdout: &out, Stderr: &stderr, Stdin: strings.NewReader("")}).Run(t.Context(), args); err != nil || stderr.Len() != 0 {
			t.Fatalf("offline command: %v %s", err, stderr.String())
		}
		if jsonOut {
			var got preflightToolCatalog
			if err := json.Unmarshal(out.Bytes(), &got); err != nil || !reflect.DeepEqual(got, preflightCatalog()) {
				t.Fatalf("JSON catalogue mismatch: %s (%v)", out.String(), err)
			}
		} else {
			for _, want := range []string{"NAME", "DEFAULT", "TARGETS", "windows/normal", "windows/wsl2", "default (alias: defaults)", "none:", "YAML []", "not installed tools"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("text omitted %q: %s", want, out.String())
				}
			}
		}
	}
	for _, args := range [][]string{{"preflight-tools", "--unknown"}, {"preflight-tools", "extra"}, {"preflight-tools", "--json=invalid"}} {
		var out bytes.Buffer
		err := (App{Stdout: &out, Stderr: io.Discard}).Run(t.Context(), args)
		if ExitCodeForError(err, 1) != 2 || out.Len() != 0 {
			t.Fatalf("usage %v returned %v / %q", args, err, out.String())
		}
	}
	if got, err := os.ReadFile(config); err != nil || !bytes.Equal(got, broken) {
		t.Fatalf("discovery changed configuration: %v", err)
	}
}
