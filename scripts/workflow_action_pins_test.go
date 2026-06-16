package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCheckWorkflowNodeRejectsMutableUsesSyntaxes(t *testing.T) {
	tests := map[string]string{
		"step shorthand":    "jobs:\n  test:\n    steps:\n      - uses: actions/checkout@v6\n",
		"quoted block key":  "jobs:\n  test:\n    \"uses\": actions/example/.github/workflows/test.yml@main\n",
		"quoted flow key":   "jobs: { test: { steps: [ { \"uses\": \"actions/checkout@v6\" } ] } }\n",
		"aliased uses key":  "usesKey: &usesKey uses\njobs:\n  test:\n    *usesKey: actions/checkout@v6\n",
		"mutable container": "jobs:\n  test:\n    steps:\n      - uses: docker://alpine:3.20\n",
	}
	for name, workflow := range tests {
		t.Run(name, func(t *testing.T) {
			var document yaml.Node
			if err := yaml.Unmarshal([]byte(workflow), &document); err != nil {
				t.Fatal(err)
			}
			findings := checkWorkflowNode("test.yml", &document)
			if len(findings) == 0 {
				t.Fatal("expected mutable action reference to be rejected")
			}
		})
	}
}

func TestCheckWorkflowNodeAcceptsImmutableUsesSyntaxes(t *testing.T) {
	sha := strings.Repeat("a", 40)
	digest := strings.Repeat("b", 64)
	workflow := "jobs:\n  local:\n    uses: ./.github/workflows/local.yml\n  remote:\n    uses: example/repo/.github/workflows/test.yml@" + sha +
		"\n  test:\n    steps:\n      - uses: actions/checkout@" + sha +
		"\n      - uses: docker://example.invalid/action@sha256:" + digest + "\n"
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(workflow), &document); err != nil {
		t.Fatal(err)
	}
	if findings := checkWorkflowNode("test.yml", &document); len(findings) != 0 {
		t.Fatalf("unexpected findings: %v", findings)
	}
}

func TestCheckWorkflowNodeIgnoresUnrelatedUsesKeys(t *testing.T) {
	workflow := "env:\n  uses: not-an-action\njobs:\n  test:\n    steps:\n      - run: echo ok\n"
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(workflow), &document); err != nil {
		t.Fatal(err)
	}
	if findings := checkWorkflowNode("test.yml", &document); len(findings) != 0 {
		t.Fatalf("unexpected findings: %v", findings)
	}
}

func TestCheckWorkflowDirRejectsMutableCompositeActionDependencies(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".github/workflows/test.yml", "jobs:\n  test:\n    steps:\n      - uses: ./.github/actions/example\n")
	writeTestFile(t, root, ".github/actions/example/action.yml", "runs:\n  using: composite\n  steps:\n    - uses: example/external@main\n")
	if err := checkWorkflowDir(filepath.Join(root, ".github", "workflows")); err == nil {
		t.Fatal("expected mutable composite action dependency to be rejected")
	}
}

func TestCheckWorkflowDirFollowsNestedLocalCompositeActions(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".github/workflows/test.yml", "jobs:\n  test:\n    steps:\n      - uses: ./.github/actions/outer\n")
	writeTestFile(t, root, ".github/actions/outer/action.yml", "runs:\n  using: composite\n  steps:\n    - uses: ./.github/actions/inner\n")
	writeTestFile(t, root, ".github/actions/inner/action.yaml", "runs:\n  using: composite\n  steps:\n    - uses: example/external@main\n")
	if err := checkWorkflowDir(filepath.Join(root, ".github", "workflows")); err == nil {
		t.Fatal("expected nested mutable composite action dependency to be rejected")
	}
}

func TestCheckWorkflowDirAcceptsPinnedCompositeActionDependencies(t *testing.T) {
	root := t.TempDir()
	sha := strings.Repeat("a", 40)
	writeTestFile(t, root, ".github/workflows/test.yml", "jobs:\n  test:\n    steps:\n      - uses: ./.github/actions/example\n")
	writeTestFile(t, root, ".github/actions/example/action.yml", "runs:\n  using: composite\n  steps:\n    - uses: example/external@"+sha+"\n")
	if err := checkWorkflowDir(filepath.Join(root, ".github", "workflows")); err != nil {
		t.Fatal(err)
	}
}

func writeTestFile(t *testing.T, root, name, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
