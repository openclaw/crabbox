package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCIGoContract(t *testing.T) {
	if err := checkCIGoContract(readCIGoWorkflow(t)); err != nil {
		t.Fatal(err)
	}
}

func TestCIGoAggregateTruthTable(t *testing.T) {
	steps := ciGoSteps(ciGoJob(readCIGoWorkflow(t), "go"))
	if len(steps) != 1 {
		t.Fatal("expected one aggregate step")
	}
	script := ciGoScalar(steps[0], "run")
	if err := checkCIGoAggregateTruthTable(t.Context(), script); err != nil {
		t.Fatal(err)
	}
}

func checkCIGoContract(document *yaml.Node) error {
	var findings []error
	require := func(ok bool, format string, args ...any) {
		if !ok {
			findings = append(findings, fmt.Errorf(format, args...))
		}
	}
	// Exact field sets reject controls even when their YAML value is false or empty.
	fields := func(node *yaml.Node, label string, keys ...string) {
		node = resolvedNode(node)
		require(node.Kind == yaml.MappingNode, "%s must be a mapping", label)
		allowed := make(map[string]bool)
		for _, key := range keys {
			allowed[key] = true
			require(len(mappingValues(node, key, nil)) == 1, "%s needs exactly one %s", label, key)
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := scalarNodeValue(node.Content[i])
			require(allowed[key], "%s has unexpected field %q", label, key)
		}
	}
	require(len(mappingValues(document, "jobs", nil)) == 1, "workflow needs one jobs mapping")
	require(len(mappingValues(document, "env", nil)) == 1, "workflow needs one env mapping")
	require(len(mappingValues(document, "defaults", nil)) == 0, "workflow must not override execution defaults")
	env := ciGoField(document, "env")
	fields(env, "workflow env", "GOFLAGS", "GOTOOLCHAIN")
	require(ciGoScalar(env, "GOFLAGS") == "-mod=readonly -trimpath", "global GOFLAGS changed")
	require(ciGoScalar(env, "GOTOOLCHAIN") == "local", "global GOTOOLCHAIN changed")

	jobs := ciGoField(document, "jobs")
	require(jobs.Kind == yaml.MappingNode, "jobs must be a mapping")
	ids, names := make(map[string]bool), make(map[string]bool)
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		id := scalarNodeValue(jobs.Content[i])
		name := ciGoScalar(jobs.Content[i+1], "name")
		require(id != "" && !ids[id], "duplicate or empty job ID %q", id)
		require(name != "" && !names[name], "duplicate or empty job name %q", name)
		ids[id], names[name] = true, true
	}

	for _, job := range []struct{ id, name string }{
		{"go-test", "Go test"}, {"go-coverage", "Go coverage"}, {"go", "Go"},
	} {
		node := ciGoJob(document, job.id)
		keys := []string{"name", "runs-on", "timeout-minutes", "steps"}
		if job.id == "go" {
			keys = append(keys, "needs", "if")
		}
		fields(node, job.id, keys...)
		require(ciGoScalar(node, "name") == job.name, "%s name changed", job.id)
		require(ciGoScalar(node, "runs-on") == "ubuntu-latest", "%s runner changed", job.id)
		require(ciGoScalar(node, "timeout-minutes") == "30", "%s must retain its 30-minute cap", job.id)
		require(ciGoField(node, "steps").Kind == yaml.SequenceNode, "%s steps must be a sequence", job.id)
	}

	testSteps := ciGoSteps(ciGoJob(document, "go-test"))
	coverageSteps := ciGoSteps(ciGoJob(document, "go-coverage"))
	require(len(testSteps) == 2+len(ciGoTestCommands), "go-test must retain setup and every ordered workload step")
	require(len(coverageSteps) == 3, "go-coverage must have setup and Coverage only")
	for _, workload := range []struct {
		id    string
		steps []*yaml.Node
	}{{"go-test", testSteps}, {"go-coverage", coverageSteps}} {
		for i, setup := range []struct{ name, action string }{
			{"Check out", "actions/checkout"}, {"Set up Go", "actions/setup-go"},
		} {
			if i >= len(workload.steps) {
				continue
			}
			step := workload.steps[i]
			label := workload.id + "/" + setup.name
			keys := []string{"name", "uses"}
			if i == 1 {
				keys = append(keys, "with")
				with := ciGoField(step, "with")
				fields(with, label+" inputs", "go-version-file", "cache")
				require(ciGoScalar(with, "go-version-file") == "go.mod", "%s must use go.mod", label)
				require(ciGoScalar(with, "cache") == "false", "%s must disable setup cache", label)
			}
			fields(step, label, keys...)
			require(ciGoScalar(step, "name") == setup.name, "%s is out of order", label)
			uses := ciGoScalar(step, "uses")
			require(strings.HasPrefix(uses, setup.action+"@"), "%s action changed", label)
			if i < len(testSteps) {
				require(uses == ciGoScalar(testSteps[i], "uses"), "%s must match go-test setup", label)
			}
		}
	}
	// The existing checker owns immutable action references; no copied SHA inventory.
	findings = append(findings, checkWorkflowNode("ci.yml", document)...)
	checkCommand := func(step *yaml.Node, want ciGoCommand) {
		keys := []string{"name", "run"}
		if want.shell != "" {
			keys = append(keys, "shell")
			require(ciGoScalar(step, "shell") == want.shell, "%s shell changed", want.name)
		}
		fields(step, want.name, keys...)
		require(ciGoScalar(step, "name") == want.name, "expected ordered step %q", want.name)
		require(ciGoScalar(step, "run") == want.run, "%s executable script changed", want.name)
	}
	for i, want := range ciGoTestCommands {
		if i+2 < len(testSteps) {
			checkCommand(testSteps[i+2], want)
		}
	}
	if len(coverageSteps) == 3 {
		checkCommand(coverageSteps[2], ciGoCommand{name: "Coverage", run: "scripts/check-go-coverage.sh 90.0"})
	}

	aggregate := ciGoJob(document, "go")
	needs := ciGoField(aggregate, "needs")
	require(needs.Kind == yaml.SequenceNode && len(needs.Content) == 2, "Go needs exactly both workloads")
	if len(needs.Content) == 2 {
		require(scalarNodeValue(needs.Content[0]) == "go-test" && scalarNodeValue(needs.Content[1]) == "go-coverage", "Go dependencies changed")
	}
	require(ciGoScalar(aggregate, "if") == "${{ always() }}", "Go must always evaluate dependency results")
	steps := ciGoSteps(aggregate)
	require(len(steps) == 1, "Go must contain only the aggregate Bash step")
	if len(steps) == 1 {
		step := steps[0]
		fields(step, "Go aggregate step", "name", "shell", "env", "run")
		require(ciGoScalar(step, "name") == "Require all Go checks", "aggregate step name changed")
		require(ciGoScalar(step, "shell") == "bash", "aggregate must run Bash")
		bindings := ciGoField(step, "env")
		fields(bindings, "Go result bindings", "GO_TEST_RESULT", "GO_COVERAGE_RESULT")
		require(ciGoScalar(bindings, "GO_TEST_RESULT") == "${{ needs['go-test'].result }}", "Go test result binding changed")
		require(ciGoScalar(bindings, "GO_COVERAGE_RESULT") == "${{ needs['go-coverage'].result }}", "Go coverage result binding changed")
		lines := strings.Split(strings.TrimSuffix(ciGoScalar(step, "run"), "\n"), "\n")
		require(len(lines) == 3, "aggregate must print diagnostics then finish with its predicate")
		if len(lines) == 3 {
			require(lines[0] == `printf 'Go test: %s\nGo coverage: %s\n' \` &&
				lines[1] == `  "${GO_TEST_RESULT:-missing}" "${GO_COVERAGE_RESULT:-missing}"`, "aggregate must print both results first")
			require(lines[2] == `[[ "${GO_TEST_RESULT:-}" == success && "${GO_COVERAGE_RESULT:-}" == success ]]`, "aggregate must end with the quoted success/success predicate")
		}
	}
	return errors.Join(findings...)
}

var errCIGoAggregateResult = errors.New("aggregate result mismatch")

func checkCIGoAggregateTruthTable(ctx context.Context, script string) error {
	bash, err := exec.LookPath("bash")
	if err != nil {
		return fmt.Errorf("find Bash: %w", err)
	}
	states := []struct {
		name, value string
		present     bool
	}{
		{"success", "success", true}, {"failure", "failure", true},
		{"cancelled", "cancelled", true}, {"skipped", "skipped", true},
		{"empty", "", true}, {"unset", "", false}, {"unexpected", "unexpected", true},
	}
	for _, testResult := range states {
		for _, coverageResult := range states {
			cmd := exec.CommandContext(ctx, bash, "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", script)
			// Do not inherit BASH_ENV, exported functions, or either result variable.
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
			if testResult.present {
				cmd.Env = append(cmd.Env, "GO_TEST_RESULT="+testResult.value)
			}
			if coverageResult.present {
				cmd.Env = append(cmd.Env, "GO_COVERAGE_RESULT="+coverageResult.value)
			}
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				return fmt.Errorf("aggregate interrupted: %w", ctx.Err())
			}
			code := 0
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					return fmt.Errorf("launch aggregate: %w", err)
				}
				code = exitErr.ExitCode()
				if code != 1 {
					return fmt.Errorf("aggregate execution error for %s/%s: exit %d: %s", testResult.name, coverageResult.name, code, output)
				}
			}
			want := 1
			if testResult.name == "success" && coverageResult.name == "success" {
				want = 0
			}
			if code != want {
				return fmt.Errorf("%w: %s/%s: exit %d, want %d: %s", errCIGoAggregateResult, testResult.name, coverageResult.name, code, want, output)
			}
		}
	}
	return nil
}

func TestCIGoContractRejectsMutations(t *testing.T) {
	if err := checkCIGoContract(readCIGoWorkflow(t)); err != nil {
		t.Fatalf("unmodified source must satisfy contract: %v", err)
	}
	type mutation struct {
		name   string
		change func(*yaml.Node)
	}
	mutations := []mutation{
		{"remove coverage job", func(d *yaml.Node) { ciGoDelete(t, ciGoField(d, "jobs"), "go-coverage") }},
		{"remove dependencies", func(d *yaml.Node) { ciGoDelete(t, ciGoJob(d, "go"), "needs") }},
		{"remove coverage dependency", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go"), "needs", "[go-test]") }},
		{"remove test dependency", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go"), "needs", "[go-coverage]") }},
		{"extra dependency", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go"), "needs", "[go-test, go-coverage, worker]") }},
		{"remove always", func(d *yaml.Node) { ciGoDelete(t, ciGoJob(d, "go"), "if") }},
		{"serialize coverage", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go-coverage"), "needs", "[go-test]") }},
		{"serialize tests", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go-test"), "needs", "[go-coverage]") }},
		{"threshold", func(d *yaml.Node) {
			ciGoSet(t, ciGoSteps(ciGoJob(d, "go-coverage"))[2], "run", "scripts/check-go-coverage.sh 89.0")
		}},
		{"coverage checkout differs", func(d *yaml.Node) {
			ciGoSet(t, ciGoSteps(ciGoJob(d, "go-coverage"))[0], "uses", "actions/checkout@"+strings.Repeat("a", 40))
		}},
		{"setup toolchain source", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoSteps(ciGoJob(d, "go-test"))[1], "with"), "go-version-file", "worker/go.mod")
		}},
		{"setup cache", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoSteps(ciGoJob(d, "go-coverage"))[1], "with"), "cache", "true")
		}},
		{"global flags", func(d *yaml.Node) { ciGoSet(t, ciGoField(d, "env"), "GOFLAGS", "-mod=mod") }},
		{"global toolchain", func(d *yaml.Node) { ciGoSet(t, ciGoField(d, "env"), "GOTOOLCHAIN", "auto") }},
		{"global defaults", func(d *yaml.Node) { ciGoSet(t, d, "defaults", "{run: {shell: 'bash {0}', working-directory: worker}}") }},
		{"duplicate name", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go-coverage"), "name", "Go") }},
		{"duplicate ID", func(d *yaml.Node) {
			jobs := ciGoField(d, "jobs")
			jobs.Content = append(jobs.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "go"}, ciGoJob(d, "go"))
		}},
		{"reorder workload", func(d *yaml.Node) {
			steps := ciGoSteps(ciGoJob(d, "go-test"))
			steps[3], steps[5] = steps[5], steps[3]
		}},
		{"comment out race command", func(d *yaml.Node) {
			ciGoField(ciGoSteps(ciGoJob(d, "go-test"))[5], "run").Value = "# go test -race -timeout=15m ./...\ntrue\n"
		}},
		{"implicit package timeout", func(d *yaml.Node) {
			ciGoField(ciGoSteps(ciGoJob(d, "go-test"))[5], "run").Value = "go test -race ./..."
		}},
		{"unbounded package timeout", func(d *yaml.Node) {
			ciGoField(ciGoSteps(ciGoJob(d, "go-test"))[5], "run").Value = "go test -race -timeout=0 ./..."
		}},
		{"remove Linux skip assertion", func(d *yaml.Node) {
			run := ciGoField(ciGoSteps(ciGoJob(d, "go-test"))[6], "run")
			run.Value = strings.Replace(run.Value, "assert not any", "# assert not any", 1)
		}},
		{"remove Linux required case", func(d *yaml.Node) {
			run := ciGoField(ciGoSteps(ciGoJob(d, "go-test"))[6], "run")
			run.Value = strings.Replace(run.Value, "    'TestWSL2ProductionCleanupKillsEntireStagingGroup',\n", "", 1)
		}},
		{"logging after predicate", func(d *yaml.Node) {
			ciGoField(ciGoSteps(ciGoJob(d, "go"))[0], "run").Value += "echo done\n"
		}},
	}
	for _, binding := range []string{"GO_TEST_RESULT", "GO_COVERAGE_RESULT"} {
		mutations = append(mutations, mutation{"alter " + binding, func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoSteps(ciGoJob(d, "go"))[0], "env"), binding, "${{ needs.worker.result }}")
		}})
	}
	for _, id := range []string{"go-test", "go-coverage", "go"} {
		mutations = append(mutations, mutation{id + " timeout", func(d *yaml.Node) {
			ciGoSet(t, ciGoJob(d, id), "timeout-minutes", "31")
		}})
		for i := range ciGoSteps(ciGoJob(readCIGoWorkflow(t), id)) {
			mutations = append(mutations, mutation{fmt.Sprintf("%s remove step %d", id, i), func(d *yaml.Node) {
				steps := ciGoField(ciGoJob(d, id), "steps")
				steps.Content = append(steps.Content[:i], steps.Content[i+1:]...)
			}})
		}
		for _, scope := range []string{"job", "step"} {
			for _, override := range []struct{ key, value string }{
				{"if", "false"}, {"if", "${{ success() }}"}, {"continue-on-error", "true"},
				{"env", "{GOFLAGS: '-run=^$'}"}, {"defaults", "{run: {shell: 'bash {0}'}}"},
				{"working-directory", "worker"},
			} {
				mutations = append(mutations, mutation{id + " " + scope + " " + override.key + "=" + override.value, func(d *yaml.Node) {
					node := ciGoJob(d, id)
					if scope == "step" {
						steps := ciGoSteps(node)
						node = steps[len(steps)-1]
					}
					ciGoSet(t, node, override.key, override.value)
				}})
			}
		}
		mutations = append(mutations, mutation{id + " shell override", func(d *yaml.Node) {
			steps := ciGoSteps(ciGoJob(d, id))
			ciGoSet(t, steps[len(steps)-1], "shell", "bash {0}")
		}})
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			document := readCIGoWorkflow(t)
			tc.change(document)
			if err := checkCIGoContract(document); err == nil {
				t.Fatal("mutated workflow satisfied the Go contract")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(string) string
	}{
		{"unconditional success", func(string) string { return "true\n" }},
		{"weaken AND to OR", func(script string) string { return strings.Replace(script, " && ", " || ", 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document := readCIGoWorkflow(t)
			run := ciGoField(ciGoSteps(ciGoJob(document, "go"))[0], "run")
			run.Value = tc.change(run.Value)
			if err := checkCIGoAggregateTruthTable(t.Context(), run.Value); !errors.Is(err, errCIGoAggregateResult) {
				t.Fatalf("expected a truth-table mismatch, got %v", err)
			}
		})
	}
}

func readCIGoWorkflow(t *testing.T) *yaml.Node {
	t.Helper()
	source, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(source, &document); err != nil {
		t.Fatal(err)
	}
	return &document
}

func ciGoField(node *yaml.Node, key string) *yaml.Node {
	values := mappingValues(node, key, nil)
	if len(values) != 1 {
		return &yaml.Node{}
	}
	return resolvedNode(values[0])
}

func ciGoScalar(node *yaml.Node, key string) string {
	return scalarNodeValue(ciGoField(node, key))
}

func ciGoJob(document *yaml.Node, id string) *yaml.Node {
	return ciGoField(ciGoField(document, "jobs"), id)
}

func ciGoSteps(job *yaml.Node) []*yaml.Node {
	node := ciGoField(job, "steps")
	if node.Kind != yaml.SequenceNode {
		return nil
	}
	return node.Content
}

func ciGoSet(t *testing.T, node *yaml.Node, key, value string) {
	t.Helper()
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(value), &document); err != nil {
		t.Fatal(err)
	}
	node = resolvedNode(node)
	for i := 0; i+1 < len(node.Content); i += 2 {
		if scalarNodeValue(node.Content[i]) == key {
			node.Content[i+1] = resolvedNode(&document)
			return
		}
	}
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, resolvedNode(&document))
}

func ciGoDelete(t *testing.T, node *yaml.Node, key string) {
	t.Helper()
	node = resolvedNode(node)
	for i := 0; i+1 < len(node.Content); i += 2 {
		if scalarNodeValue(node.Content[i]) == key {
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
			return
		}
	}
	t.Fatalf("mutation target %s is missing", key)
}

type ciGoCommand struct{ name, shell, run string }

// Bound the executable scripts themselves so comments or a weaker command cannot
// satisfy the contract by retaining a familiar substring.
var ciGoTestCommands = []ciGoCommand{
	{"Check formatting", "bash", `mapfile -t files < <(git ls-files '*.go')
if [ "${#files[@]}" -gt 0 ]; then
  diff="$(gofmt -l "${files[@]}")"
  if [ -n "$diff" ]; then
    echo "$diff"
    exit 1
  fi
fi
`},
	{"Vet", "", "go vet ./..."},
	{"Deadcode", "", `output_file=$(mktemp)
go run golang.org/x/tools/cmd/deadcode@v0.45.0 -test ./... > "$output_file"
if [ -s "$output_file" ]; then
  cat "$output_file"
  exit 1
fi
`},
	{"Test", "", "go test -race -timeout=15m ./..."},
	{"Require executed Linux supervision fixtures", "bash", `go test ./internal/cli -run '^(TestWorkspaceOwnerWSL2Watchdog.*|TestWSL2(OrdinaryShortFrameWatchdogCleansState|MarkerPublicationFailureLeavesUnarmedDiagnosticState|GuardSurvivesPublishedMarkerBeforeArm|ProductionCleanup.*))$' -count=1 -json | tee "$RUNNER_TEMP/wsl-linux-tests.jsonl"
python3 - "$RUNNER_TEMP/wsl-linux-tests.jsonl" <<'PY'
import json, sys
records = [json.loads(line) for line in open(sys.argv[1])]
required = (
    'TestWorkspaceOwnerWSL2WatchdogAllowsCompletedFrameExecution',
    'TestWSL2OrdinaryShortFrameWatchdogCleansState',
    'TestWSL2MarkerPublicationFailureLeavesUnarmedDiagnosticState',
    'TestWSL2GuardSurvivesPublishedMarkerBeforeArm',
    'TestWSL2ProductionCleanupKillsEntireStagingGroup',
    'TestWSL2ProductionCleanupFailsClosedWithoutLiveGuard',
    'TestWSL2ProductionCleanupFailsClosedWhenGuardMarkerChanges',
    'TestWorkspaceOwnerWSL2WatchdogCleansCompletedFrameAfterLauncherExit',
    'TestWorkspaceOwnerWSL2WatchdogPreservesIntentionalBackgroundWitness',
)
for name in required:
    for action in ('run', 'pass'):
        assert any(r.get('Test') == name and r['Action'] == action for r in records), (name, action)
assert not any(r['Action'] == 'skip' for r in records), 'native fixture skipped'
PY
`},
	{"Test all Go modules", "", "scripts/test-go-modules.sh"},
	{"Build", "", "go build -trimpath -o /tmp/crabbox ./cmd/crabbox"},
}
