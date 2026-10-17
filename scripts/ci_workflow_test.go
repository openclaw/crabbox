package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestCIGoContract(t *testing.T) {
	if err := checkCIGoContract(readCIGoWorkflow(t)); err != nil {
		t.Fatal(err)
	}
}

func TestCIGoNativeEventVerifier(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell is not installed")
	}
	verifier, err := filepath.Abs("verify-go-test-events.ps1")
	if err != nil {
		t.Fatal(err)
	}
	// PowerShell's cold startup is part of this fixture test, not a performance
	// contract. Use the package budget, reserving 5% for cancellation and cleanup.
	ctx := t.Context()
	if deadline, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline.Add(-time.Until(deadline)/20))
		defer cancel()
	}
	// Reuse an isolated home so each case need not rebuild PowerShell's caches.
	home := t.TempDir()
	complete := []string{
		`{"Action":"run","Test":"TestAlpha"}`,
		`{"Action":"pass","Test":"TestAlpha"}`,
		`{"Action":"run","Test":"TestBeta"}`,
		`{"Action":"pass","Test":"TestBeta"}`,
		`{"Action":"pass","Package":"example.test/synthetic"}`,
	}
	for _, tc := range []struct {
		name    string
		events  []string
		wantErr string
	}{
		{"complete", complete, ""},
		{"missing run", complete[1:], "TestAlpha did not run"},
		{"missing pass", append([]string{complete[0]}, complete[2:]...), "TestAlpha did not pass"},
		{"missing test", complete[:2], "TestBeta did not run"},
		{"empty", []string{}, "TestAlpha did not run"},
		{"required skip", append(append([]string{}, complete...), `{"Action":"skip","Test":"TestAlpha"}`), "was skipped"},
		{"unrelated skip", append(append([]string{}, complete...), `{"Action":"skip","Test":"TestOther"}`), "was skipped"},
		{"package skip", append(append([]string{}, complete...), `{"Action":"skip","Package":"example.test/other"}`), "was skipped"},
		{"malformed JSON", append(append([]string{}, complete...), `{`), "ConvertFrom-Json"},
		{"PowerShell comparison semantics", []string{
			`{"Action":"RUN","Test":"testalpha"}`,
			`{"Action":"PASS","Test":"testalpha"}`,
			`{"Action":"run","Test":"TESTBETA"}`,
			`{"Action":"pass","Test":"TESTBETA"}`,
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			input, err := json.Marshal(struct {
				Events        []string `json:"events"`
				RequiredTests []string `json:"requiredTests"`
			}{tc.events, []string{"TestAlpha", "TestBeta"}})
			if err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, dir, "events.json", string(input))
			writeTestFile(t, dir, "verify.ps1", `param([string]$Verifier, [string]$InputPath)
$ErrorActionPreference = 'Stop'
$fixture = Get-Content -Raw -LiteralPath $InputPath | ConvertFrom-Json
& $Verifier -Events $fixture.events -RequiredTests $fixture.requiredTests -Label 'synthetic native test'
`)
			cmd := exec.CommandContext(ctx, pwsh, "-NoLogo", "-NoProfile", "-NonInteractive", "-File",
				filepath.Join(dir, "verify.ps1"), verifier, filepath.Join(dir, "events.json"))
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "POWERSHELL_TELEMETRY_OPTOUT=1"}
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("verifier timed out: %v\n%s", ctx.Err(), output)
			}
			if tc.wantErr == "" {
				if err != nil || len(output) != 0 {
					t.Fatalf("successful verifier must be silent: err=%v output=%s", err, output)
				}
				return
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || !strings.Contains(string(output), tc.wantErr) {
				t.Fatalf("want failed verifier containing %q, got err=%v output=%s", tc.wantErr, err, output)
			}
		})
	}
}

func TestCIGoDocumentedRaceCommand(t *testing.T) {
	// The local command remains the unsharded equivalent of all CI race lanes.
	command := "go test -race -timeout=20m ./..."
	raceCommand := regexp.MustCompile("go test -race[^\\r\\n`]*\\./\\.\\.\\.")
	for _, path := range []string{
		"README.md", "AGENTS.md", "docs/operations.md",
		"docs/features/provider-authoring.md", "docs/providers/islo.md", "docs/providers/morph.md",
	} {
		t.Run(path, func(t *testing.T) {
			content, err := os.ReadFile("../" + path)
			if err != nil {
				t.Fatal(err)
			}
			commands := raceCommand.FindAllString(string(content), -1)
			if len(commands) == 0 {
				t.Fatal("missing documented full race-test command")
			}
			for _, documented := range commands {
				if documented != command {
					t.Errorf("documented race command %q differs from full local command %q", documented, command)
				}
			}
		})
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
	t.Log("executed all 343 dependency-result combinations; only success/success/success passed")
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
	triggers := ciGoField(document, "on")
	fields(triggers, "workflow triggers", "push", "pull_request", "workflow_dispatch")
	push := ciGoField(triggers, "push")
	fields(push, "push trigger", "branches")
	branches := ciGoField(push, "branches")
	require(branches.Kind == yaml.SequenceNode && len(branches.Content) == 1 && scalarNodeValue(branches.Content[0]) == "main", "push must cover main without path filters")
	for _, trigger := range []string{"pull_request", "workflow_dispatch"} {
		node := ciGoField(triggers, trigger)
		require(node.Kind == yaml.ScalarNode && node.Tag == "!!null", "%s must remain unfiltered", trigger)
	}

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
		{"go-test-core", "Go core"}, {"go-modules-core", "Go normal non-CLI and nested modules"}, {"go-coverage", "Go coverage"}, {"go-coverage-core", "Go non-CLI coverage"}, {"go", "Go"},
	} {
		node := ciGoJob(document, job.id)
		keys := []string{"name", "runs-on", "timeout-minutes", "steps"}
		if job.id == "go" || job.id == "go-coverage" {
			keys = append(keys, "needs", "if")
		}
		fields(node, job.id, keys...)
		require(ciGoScalar(node, "name") == job.name, "%s name changed", job.id)
		require(ciGoScalar(node, "runs-on") == "ubuntu-latest", "%s runner changed", job.id)
		require(ciGoScalar(node, "timeout-minutes") == "30", "%s must retain its 30-minute cap", job.id)
		require(ciGoField(node, "steps").Kind == yaml.SequenceNode, "%s steps must be a sequence", job.id)
	}

	testSteps := ciGoSteps(ciGoJob(document, "go-test-core"))
	moduleSteps := ciGoSteps(ciGoJob(document, "go-modules-core"))
	coverageSteps := ciGoSteps(ciGoJob(document, "go-coverage"))
	coverageCoreSteps := ciGoSteps(ciGoJob(document, "go-coverage-core"))
	coverageCLISteps := ciGoSteps(ciGoJob(document, "go-coverage-cli"))
	require(len(testSteps) == 2+len(ciGoTestCommands), "go-test must retain setup and every ordered workload step")
	require(len(moduleSteps) == 3, "go-modules-core must have setup and the normal root/non-CLI and nested workload")
	require(len(coverageSteps) == 5, "coverage gate needs producer checks before setup, download, and Coverage")
	require(len(coverageCoreSteps) == 4 && len(coverageCLISteps) == 4, "coverage producers need setup, execution, and upload")
	for _, workload := range []struct {
		id    string
		steps []*yaml.Node
	}{{"go-test-core", testSteps}, {"go-modules-core", moduleSteps}, {"go-coverage", coverageSteps}, {"go-coverage-core", coverageCoreSteps}, {"go-coverage-cli", coverageCLISteps}} {
		offset := 0
		if workload.id == "go-coverage" {
			offset = 1
		}
		for i, setup := range []struct{ name, action string }{
			{"Check out", "actions/checkout"}, {"Set up Go", "actions/setup-go"},
		} {
			if i+offset >= len(workload.steps) {
				continue
			}
			step := workload.steps[i+offset]
			label := workload.id + "/" + setup.name
			keys := []string{"name", "uses"}
			if i == 1 {
				keys = append(keys, "with")
				with := ciGoField(step, "with")
				fields(with, label+" inputs", "go-version-file", "cache", "cache-dependency-path")
				require(ciGoScalar(with, "go-version-file") == "go.mod", "%s must use go.mod", label)
				require(ciGoScalar(with, "cache") == "true", "%s must cache Go builds and modules", label)
				require(ciGoScalar(with, "cache-dependency-path") == "**/go.sum", "%s must key every module dependency", label)
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
	if len(moduleSteps) == 3 {
		checkCommand(moduleSteps[2], ciGoCommand{name: "Test non-CLI root and all nested modules", run: "scripts/test-go-modules.sh --exclude-root-cli"})
	}
	if len(coverageSteps) == 5 {
		checkCommand(coverageSteps[4], ciGoCommand{name: "Coverage", run: `scripts/check-go-coverage.sh 90.0 --profiles "${{ runner.temp }}/go-coverage"`})
		step := coverageSteps[0]
		fields(step, "coverage producer gate", "name", "shell", "env", "run")
		require(ciGoScalar(step, "name") == "Require all coverage producers" && ciGoScalar(step, "shell") == "bash", "coverage gate command changed")
		bindings := ciGoField(step, "env")
		fields(bindings, "coverage gate bindings", "CORE_RESULT", "CLI_RESULT")
		require(ciGoScalar(bindings, "CORE_RESULT") == "${{ needs['go-coverage-core'].result }}" && ciGoScalar(bindings, "CLI_RESULT") == "${{ needs['go-coverage-cli'].result }}", "coverage result bindings changed")
		require(ciGoScalar(step, "run") == ciGoCoverageGateScript, "coverage gate must propagate every unsuccessful result")
		download := coverageSteps[3]
		fields(download, "coverage download", "name", "uses", "with")
		require(ciGoScalar(download, "name") == "Download coverage profiles" && strings.HasPrefix(ciGoScalar(download, "uses"), "actions/download-artifact@"), "coverage download changed")
		with := ciGoField(download, "with")
		fields(with, "coverage download inputs", "pattern", "path", "merge-multiple")
		require(ciGoScalar(with, "pattern") == "go-coverage-*" && ciGoScalar(with, "path") == "${{ runner.temp }}/go-coverage" && ciGoScalar(with, "merge-multiple") == "true", "coverage must download this run's complete profile set")
	}
	coverageGate := ciGoJob(document, "go-coverage")
	coverageNeeds := ciGoField(coverageGate, "needs")
	require(ciGoScalar(coverageGate, "if") == "${{ !cancelled() }}", "coverage gate must evaluate unless the workflow is cancelled")
	require(coverageNeeds.Kind == yaml.SequenceNode && len(coverageNeeds.Content) == 2, "coverage needs both producers")
	if len(coverageNeeds.Content) == 2 {
		require(scalarNodeValue(coverageNeeds.Content[0]) == "go-coverage-core" && scalarNodeValue(coverageNeeds.Content[1]) == "go-coverage-cli", "coverage dependencies changed")
	}
	coverageCLI := ciGoJob(document, "go-coverage-cli")
	fields(coverageCLI, "CLI coverage shards", "name", "runs-on", "timeout-minutes", "strategy", "steps")
	require(ciGoScalar(coverageCLI, "name") == "Go CLI coverage shard ${{ matrix.shard }}" && ciGoScalar(coverageCLI, "runs-on") == "ubuntu-latest" && ciGoScalar(coverageCLI, "timeout-minutes") == "30", "CLI coverage execution changed")
	coverageStrategy := ciGoField(coverageCLI, "strategy")
	fields(coverageStrategy, "coverage strategy", "fail-fast", "matrix")
	require(ciGoScalar(coverageStrategy, "fail-fast") == "false", "all coverage shards must finish")
	coverageMatrix := ciGoField(coverageStrategy, "matrix")
	fields(coverageMatrix, "coverage matrix", "shard")
	coverageShards := ciGoField(coverageMatrix, "shard")
	require(coverageShards.Kind == yaml.SequenceNode && len(coverageShards.Content) == 3, "all three CLI coverage shards are required")
	for i, shard := range coverageShards.Content {
		require(scalarNodeValue(shard) == fmt.Sprint(i), "coverage shards must be unique and exhaustive")
	}
	if len(coverageCoreSteps) == 4 {
		checkCommand(coverageCoreSteps[2], ciGoCommand{name: "Cover other packages", shell: "bash", run: ciGoCoverageCoreScript})
	}
	if len(coverageCLISteps) == 4 {
		checkCommand(coverageCLISteps[2], ciGoCommand{name: "Cover CLI shard", run: `python3 scripts/test-go-shard.py ${{ matrix.shard }} 3 --coverprofile "${{ runner.temp }}/cli-${{ matrix.shard }}.out"`})
	}
	for _, producer := range []struct {
		steps           []*yaml.Node
		suffix, profile string
	}{{coverageCoreSteps, "core", "core"}, {coverageCLISteps, "cli-${{ matrix.shard }}", "cli-${{ matrix.shard }}"}} {
		if len(producer.steps) != 4 {
			continue
		}
		upload := producer.steps[3]
		fields(upload, "coverage upload", "name", "uses", "with")
		require(ciGoScalar(upload, "name") == "Upload coverage profile" && strings.HasPrefix(ciGoScalar(upload, "uses"), "actions/upload-artifact@"), "coverage upload changed")
		with := ciGoField(upload, "with")
		fields(with, "coverage upload inputs", "name", "path", "overwrite", "if-no-files-found", "retention-days")
		require(ciGoScalar(with, "name") == "go-coverage-"+producer.suffix && ciGoScalar(with, "path") == "${{ runner.temp }}/"+producer.profile+".out", "coverage artifact identity changed")
		require(ciGoScalar(with, "if-no-files-found") == "error" && ciGoScalar(with, "retention-days") == "1", "coverage profiles must exist and expire after one day")
		require(ciGoScalar(with, "overwrite") == "true", "a rerun must replace only its own coverage profile")
	}

	aggregate := ciGoJob(document, "go")
	needs := ciGoField(aggregate, "needs")
	require(needs.Kind == yaml.SequenceNode && len(needs.Content) == 3, "Go needs exactly all three workloads")
	if len(needs.Content) == 3 {
		require(scalarNodeValue(needs.Content[0]) == "go-test" && scalarNodeValue(needs.Content[1]) == "go-modules" && scalarNodeValue(needs.Content[2]) == "go-coverage", "Go dependencies changed")
	}
	require(ciGoScalar(aggregate, "if") == "${{ !cancelled() }}", "Go must evaluate dependency results unless the workflow is cancelled")
	steps := ciGoSteps(aggregate)
	require(len(steps) == 1, "Go must contain only the aggregate Bash step")
	if len(steps) == 1 {
		step := steps[0]
		fields(step, "Go aggregate step", "name", "shell", "env", "run")
		require(ciGoScalar(step, "name") == "Require all Go checks", "aggregate step name changed")
		require(ciGoScalar(step, "shell") == "bash", "aggregate must run Bash")
		bindings := ciGoField(step, "env")
		fields(bindings, "Go result bindings", "GO_TEST_RESULT", "GO_MODULES_RESULT", "GO_COVERAGE_RESULT")
		require(ciGoScalar(bindings, "GO_TEST_RESULT") == "${{ needs['go-test'].result }}", "Go test result binding changed")
		require(ciGoScalar(bindings, "GO_MODULES_RESULT") == "${{ needs['go-modules'].result }}", "Go modules result binding changed")
		require(ciGoScalar(bindings, "GO_COVERAGE_RESULT") == "${{ needs['go-coverage'].result }}", "Go coverage result binding changed")
		lines := strings.Split(strings.TrimSuffix(ciGoScalar(step, "run"), "\n"), "\n")
		require(len(lines) == 3, "aggregate must print diagnostics then finish with its predicate")
		if len(lines) == 3 {
			require(lines[0] == `printf 'Go test: %s\nGo modules: %s\nGo coverage: %s\n' \` &&
				lines[1] == `  "${GO_TEST_RESULT:-missing}" "${GO_MODULES_RESULT:-missing}" "${GO_COVERAGE_RESULT:-missing}"`, "aggregate must print all three results first")
			require(lines[2] == `[[ "${GO_TEST_RESULT:-}" == success && "${GO_MODULES_RESULT:-}" == success && "${GO_COVERAGE_RESULT:-}" == success ]]`, "aggregate must end with the quoted success/success/success predicate")
		}
	}
	for _, gate := range []struct {
		id, name, step, first, second, firstKey, secondKey, script, timeout string
	}{
		{"go-test", "Go test", "Require all race shards", "go-test-core", "go-test-cli", "CORE_RESULT", "CLI_RESULT", ciGoRaceGateScript, "5"},
		{"go-modules", "Go modules", "Require all normal module tests", "go-modules-core", "go-modules-cli", "CORE_RESULT", "CLI_RESULT", ciGoModulesGateScript, "30"},
		{"windows-pond-mesh-cancellation", "Windows Pond Mesh Cancellation", "Require all native Windows contracts", "windows-native-contracts", "windows-powershell-architecture", "CONTRACTS_RESULT", "ARCHITECTURE_RESULT", ciWindowsGateScript, "5"},
	} {
		node := ciGoJob(document, gate.id)
		fields(node, gate.id, "name", "needs", "if", "runs-on", "timeout-minutes", "steps")
		require(ciGoScalar(node, "name") == gate.name, "%s name changed", gate.id)
		require(ciGoScalar(node, "if") == "${{ !cancelled() }}", "%s must evaluate unless the workflow is cancelled", gate.id)
		require(ciGoScalar(node, "runs-on") == "ubuntu-latest" && ciGoScalar(node, "timeout-minutes") == gate.timeout, "%s execution changed", gate.id)
		needs := ciGoField(node, "needs")
		require(needs.Kind == yaml.SequenceNode && len(needs.Content) == 2, "%s needs both workloads", gate.id)
		if len(needs.Content) == 2 {
			require(scalarNodeValue(needs.Content[0]) == gate.first && scalarNodeValue(needs.Content[1]) == gate.second, "%s dependencies changed", gate.id)
		}
		steps := ciGoSteps(node)
		require(len(steps) == 1, "%s needs one step", gate.id)
		if len(steps) == 1 {
			step := steps[0]
			fields(step, gate.id+" step", "name", "shell", "env", "run")
			require(ciGoScalar(step, "name") == gate.step && ciGoScalar(step, "shell") == "bash", "%s step changed", gate.id)
			bindings := ciGoField(step, "env")
			fields(bindings, gate.id+" bindings", gate.firstKey, gate.secondKey)
			require(ciGoScalar(bindings, gate.firstKey) == "${{ needs['"+gate.first+"'].result }}" && ciGoScalar(bindings, gate.secondKey) == "${{ needs['"+gate.second+"'].result }}", "%s result bindings changed", gate.id)
			require(ciGoScalar(step, "run") == gate.script, "%s must propagate every unsuccessful result", gate.id)
		}
	}
	for _, lane := range []struct {
		id, name, timeout, step, command string
		keepCache                        bool
	}{
		{"go-test-cli", "Go CLI shard ${{ matrix.shard }}", "20", "Test CLI shard", "python3 scripts/test-go-shard.py ${{ matrix.shard }} 4", true},
		{"go-modules-cli", "Go normal CLI shard ${{ matrix.shard }}", "30", "Test normal CLI shard", "python3 scripts/test-go-shard.py ${{ matrix.shard }} 4 --normal", false},
	} {
		cliJob := ciGoJob(document, lane.id)
		fields(cliJob, lane.id, "name", "runs-on", "timeout-minutes", "strategy", "steps")
		require(ciGoScalar(cliJob, "name") == lane.name, "%s names changed", lane.id)
		require(ciGoScalar(cliJob, "runs-on") == "ubuntu-latest" && ciGoScalar(cliJob, "timeout-minutes") == lane.timeout, "%s execution changed", lane.id)
		strategy := ciGoField(cliJob, "strategy")
		fields(strategy, lane.id+" strategy", "fail-fast", "matrix")
		require(ciGoScalar(strategy, "fail-fast") == "false", "all %s shards must finish", lane.id)
		matrix := ciGoField(strategy, "matrix")
		fields(matrix, lane.id+" matrix", "shard")
		shards := ciGoField(matrix, "shard")
		require(shards.Kind == yaml.SequenceNode && len(shards.Content) == 4, "all four %s shards are required", lane.id)
		for i, shard := range shards.Content {
			require(scalarNodeValue(shard) == fmt.Sprint(i), "%s shards must be unique and exhaustive", lane.id)
		}
		cliSteps := ciGoSteps(cliJob)
		count := 3
		if lane.keepCache {
			count++
		}
		require(len(cliSteps) == count, "%s needs setup and every ordered workload step", lane.id)
		if len(cliSteps) == count && len(testSteps) >= 2 {
			for i := 0; i < 2; i++ {
				want, _ := yaml.Marshal(testSteps[i])
				got, _ := yaml.Marshal(cliSteps[i])
				require(string(got) == string(want), "%s setup must match core setup", lane.id)
			}
			if lane.keepCache {
				checkCommand(cliSteps[2], ciGoCommand{name: "Keep restored Go build cache", run: ciGoKeepCacheScript})
			}
			checkCommand(cliSteps[count-1], ciGoCommand{name: lane.step, run: lane.command})
		}
	}
	return errors.Join(findings...)
}

// The race build would otherwise trim restored non-race entries older than
// Go's 5-day limit, and builtCLITestBinary would recompile them in every shard.
const ciGoKeepCacheScript = `cache="$(go env GOCACHE)"
mkdir -p "$cache"
find "$cache" -type f -exec touch -c {} +
`

const ciGoRaceGateScript = `printf 'Go core: %s\nGo CLI shards: %s\n' "${CORE_RESULT:-missing}" "${CLI_RESULT:-missing}"
[[ "${CORE_RESULT:-}" == success && "${CLI_RESULT:-}" == success ]]
`

const ciGoModulesGateScript = `printf 'Go non-CLI and nested modules: %s\nGo normal CLI shards: %s\n' "${CORE_RESULT:-missing}" "${CLI_RESULT:-missing}"
[[ "${CORE_RESULT:-}" == success && "${CLI_RESULT:-}" == success ]]
`

const ciGoCoverageGateScript = `printf 'Go non-CLI coverage: %s\nGo CLI coverage shards: %s\n' "${CORE_RESULT:-missing}" "${CLI_RESULT:-missing}"
[[ "${CORE_RESULT:-}" == success && "${CLI_RESULT:-}" == success ]]
`

const ciWindowsGateScript = `printf 'Windows contracts: %s\nPowerShell architecture: %s\n' \
  "${CONTRACTS_RESULT:-missing}" "${ARCHITECTURE_RESULT:-missing}"
[[ "${CONTRACTS_RESULT:-}" == success && "${ARCHITECTURE_RESULT:-}" == success ]]
`

const ciGoCoverageCoreScript = `packages=$(go list ./...)
mapfile -t packages < <(printf '%s\n' "$packages" | sed '\|^github.com/openclaw/crabbox/internal/cli$|d')
go test -count=1 -timeout=20m -covermode=atomic -coverprofile="${{ runner.temp }}/core.out" "${packages[@]}"
`

func TestCIGoShardSelection(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 is not installed")
	}
	cmd := exec.CommandContext(t.Context(), python, "test-go-shard_test.py")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shard selection: %v\n%s", err, output)
	}
}

func TestCIGoRaceGateTruthTable(t *testing.T) {
	checkCIGoProducerTruthTable(t, "go-test", 0, "CORE_RESULT", "CLI_RESULT")
}

func TestCIGoCoverageGateTruthTable(t *testing.T) {
	checkCIGoProducerTruthTable(t, "go-coverage", 0, "CORE_RESULT", "CLI_RESULT")
}

func TestCIGoModulesGateTruthTable(t *testing.T) {
	checkCIGoProducerTruthTable(t, "go-modules", 0, "CORE_RESULT", "CLI_RESULT")
}

func TestCIWindowsGateTruthTable(t *testing.T) {
	checkCIGoProducerTruthTable(t, "windows-pond-mesh-cancellation", 0, "CONTRACTS_RESULT", "ARCHITECTURE_RESULT")
}

func checkCIGoProducerTruthTable(t *testing.T, job string, index int, firstKey, secondKey string) {
	t.Helper()
	steps := ciGoSteps(ciGoJob(readCIGoWorkflow(t), job))
	if len(steps) <= index {
		t.Fatal("missing producer gate")
	}
	states := []string{"success", "failure", "cancelled", "skipped", "", "unknown", "unset"}
	for _, first := range states {
		for _, second := range states {
			cmd := exec.CommandContext(t.Context(), "bash", "-c", ciGoScalar(steps[index], "run"))
			cmd.Env = []string{}
			if first != "unset" {
				cmd.Env = append(cmd.Env, firstKey+"="+first)
			}
			if second != "unset" {
				cmd.Env = append(cmd.Env, secondKey+"="+second)
			}
			output, err := cmd.CombinedOutput()
			if (err == nil) != (first == "success" && second == "success") {
				t.Fatalf("%s=%q %s=%q err=%v output=%s", firstKey, first, secondKey, second, err, output)
			}
		}
	}
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
		for _, moduleResult := range states {
			for _, coverageResult := range states {
				cmd := exec.CommandContext(ctx, bash, "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", script)
				// Do not inherit BASH_ENV, exported functions, or any result variable.
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
				if testResult.present {
					cmd.Env = append(cmd.Env, "GO_TEST_RESULT="+testResult.value)
				}
				if moduleResult.present {
					cmd.Env = append(cmd.Env, "GO_MODULES_RESULT="+moduleResult.value)
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
						return fmt.Errorf("aggregate execution error for %s/%s/%s: exit %d: %s", testResult.name, moduleResult.name, coverageResult.name, code, output)
					}
				}
				want := 1
				if testResult.name == "success" && moduleResult.name == "success" && coverageResult.name == "success" {
					want = 0
				}
				if code != want {
					return fmt.Errorf("%w: %s/%s/%s: exit %d, want %d: %s", errCIGoAggregateResult, testResult.name, moduleResult.name, coverageResult.name, code, want, output)
				}
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
		{"missing normal CLI shard", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoJob(d, "go-modules-cli"), "strategy"), "matrix", "{shard: [0, 1, 2]}")
		}},
		{"duplicate normal CLI shard", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoJob(d, "go-modules-cli"), "strategy"), "matrix", "{shard: [0, 1, 2, 2]}")
		}},
		{"normal shard fail fast", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoJob(d, "go-modules-cli"), "strategy"), "fail-fast", "true")
		}},
		{"normal shard uses race mode", func(d *yaml.Node) {
			ciGoSet(t, ciGoSteps(ciGoJob(d, "go-modules-cli"))[2], "run", "python3 scripts/test-go-shard.py ${{ matrix.shard }} 4")
		}},
		{"normal shard wrong count", func(d *yaml.Node) {
			ciGoSet(t, ciGoSteps(ciGoJob(d, "go-modules-cli"))[2], "run", "python3 scripts/test-go-shard.py ${{ matrix.shard }} 3 --normal")
		}},
		{"normal gate missing CLI producer", func(d *yaml.Node) {
			ciGoSet(t, ciGoJob(d, "go-modules"), "needs", "[go-modules-core]")
		}},
		{"normal gate substitutes coverage", func(d *yaml.Node) {
			ciGoSet(t, ciGoJob(d, "go-modules"), "needs", "[go-modules-core, go-coverage-cli]")
		}},
		{"normal gate masks failure", func(d *yaml.Node) {
			ciGoSet(t, ciGoSteps(ciGoJob(d, "go-modules"))[0], "run", "true")
		}},
		{"normal gate wrong result", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoSteps(ciGoJob(d, "go-modules"))[0], "env"), "CLI_RESULT", "success")
		}},
		{"missing coverage shard", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoJob(d, "go-coverage-cli"), "strategy"), "matrix", "{shard: [0, 1]}")
		}},
		{"duplicate coverage shard", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoJob(d, "go-coverage-cli"), "strategy"), "matrix", "{shard: [0, 1, 1]}")
		}},
		{"skip coverage shard", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go-coverage-cli"), "if", "false") }},
		{"ignore coverage failure", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go-coverage-cli"), "continue-on-error", "true") }},
		{"coverage wrong shard count", func(d *yaml.Node) {
			ciGoField(ciGoSteps(ciGoJob(d, "go-coverage-cli"))[2], "run").Value = strings.Replace(ciGoScalar(ciGoSteps(ciGoJob(d, "go-coverage-cli"))[2], "run"), " 3 ", " 2 ", 1)
		}},
		{"skip coverage on failure", func(d *yaml.Node) { ciGoDelete(t, ciGoJob(d, "go-coverage"), "if") }},
		{"coverage missing dependency", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go-coverage"), "needs", "[go-coverage-cli]") }},
		{"coverage masks producer failure", func(d *yaml.Node) { ciGoSet(t, ciGoSteps(ciGoJob(d, "go-coverage"))[0], "run", "true") }},
		{"coverage wrong result", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoSteps(ciGoJob(d, "go-coverage"))[0], "env"), "CLI_RESULT", "success")
		}},
		{"coverage checks producers after checkout", func(d *yaml.Node) {
			steps := ciGoSteps(ciGoJob(d, "go-coverage"))
			steps[0], steps[1] = steps[1], steps[0]
		}},
		{"coverage checks producers after setup", func(d *yaml.Node) {
			steps := ciGoSteps(ciGoJob(d, "go-coverage"))
			steps[0], steps[2] = steps[2], steps[0]
		}},
		{"coverage missing upload succeeds", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoSteps(ciGoJob(d, "go-coverage-cli"))[3], "with"), "if-no-files-found", "warn")
		}},
		{"coverage rerun profile collision", func(d *yaml.Node) {
			ciGoDelete(t, ciGoField(ciGoSteps(ciGoJob(d, "go-coverage-cli"))[3], "with"), "overwrite")
		}},
		{"coverage profile collision", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoSteps(ciGoJob(d, "go-coverage-cli"))[3], "with"), "name", "go-coverage-core")
		}},
		{"coverage foreign run", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoSteps(ciGoJob(d, "go-coverage"))[3], "with"), "run-id", "1")
		}},
		{"coverage omit non-CLI packages", func(d *yaml.Node) {
			ciGoSet(t, ciGoSteps(ciGoJob(d, "go-coverage-core"))[2], "run", "go test ./internal/cli")
		}},
		{"missing CLI shard", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoJob(d, "go-test-cli"), "strategy"), "matrix", "{shard: [0, 1, 2]}")
		}},
		{"duplicate CLI shard", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoJob(d, "go-test-cli"), "strategy"), "matrix", "{shard: [0, 1, 2, 2]}")
		}},
		{"exclude CLI shard", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoField(ciGoJob(d, "go-test-cli"), "strategy"), "matrix"), "exclude", "[{shard: 3}]")
		}},
		{"skip CLI matrix", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go-test-cli"), "if", "false") }},
		{"ignore CLI failure", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go-test-cli"), "continue-on-error", "true") }},
		{"race gate missing dependency", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go-test"), "needs", "[go-test-core]") }},
		{"race gate skip on failure", func(d *yaml.Node) { ciGoDelete(t, ciGoJob(d, "go-test"), "if") }},
		{"race gate masks failure", func(d *yaml.Node) { ciGoSet(t, ciGoSteps(ciGoJob(d, "go-test"))[0], "run", "true") }},
		{"wrong shard count", func(d *yaml.Node) {
			ciGoSet(t, ciGoSteps(ciGoJob(d, "go-test-cli"))[3], "run", "python3 scripts/test-go-shard.py ${{ matrix.shard }} 3")
		}},
		{"CLI shard trims restored cache", func(d *yaml.Node) {
			steps := ciGoField(ciGoJob(d, "go-test-cli"), "steps")
			steps.Content = append(steps.Content[:2], steps.Content[3:]...)
		}},
		{"CLI shard keeps cache after tests", func(d *yaml.Node) {
			steps := ciGoSteps(ciGoJob(d, "go-test-cli"))
			steps[2], steps[3] = steps[3], steps[2]
		}},
		{"cache ignores nested modules", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoSteps(ciGoJob(d, "go-test-core"))[1], "with"), "cache-dependency-path", "go.sum")
		}},
		{"remove modules job", func(d *yaml.Node) { ciGoDelete(t, ciGoField(d, "jobs"), "go-modules") }},
		{"remove coverage job", func(d *yaml.Node) { ciGoDelete(t, ciGoField(d, "jobs"), "go-coverage") }},
		{"remove dependencies", func(d *yaml.Node) { ciGoDelete(t, ciGoJob(d, "go"), "needs") }},
		{"remove coverage dependency", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go"), "needs", "[go-test, go-modules]") }},
		{"remove modules dependency", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go"), "needs", "[go-test, go-coverage]") }},
		{"remove test dependency", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go"), "needs", "[go-modules, go-coverage]") }},
		{"extra dependency", func(d *yaml.Node) {
			ciGoSet(t, ciGoJob(d, "go"), "needs", "[go-test, go-modules, go-coverage, worker]")
		}},
		{"remove cancellation guard", func(d *yaml.Node) { ciGoDelete(t, ciGoJob(d, "go"), "if") }},
		{"Windows gate missing dependency", func(d *yaml.Node) {
			ciGoSet(t, ciGoJob(d, "windows-pond-mesh-cancellation"), "needs", "[windows-native-contracts]")
		}},
		{"Windows gate masks failure", func(d *yaml.Node) {
			ciGoSet(t, ciGoSteps(ciGoJob(d, "windows-pond-mesh-cancellation"))[0], "run", "true")
		}},
		{"serialize coverage", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go-coverage"), "needs", "[go-test]") }},
		{"serialize tests", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go-test-core"), "needs", "[go-coverage]") }},
		{"serialize modules", func(d *yaml.Node) { ciGoSet(t, ciGoJob(d, "go-modules"), "needs", "[go-test]") }},
		{"threshold", func(d *yaml.Node) {
			ciGoSet(t, ciGoSteps(ciGoJob(d, "go-coverage"))[4], "run", `scripts/check-go-coverage.sh 89.0 --profiles "${{ runner.temp }}/go-coverage"`)
		}},
		{"coverage checkout differs", func(d *yaml.Node) {
			ciGoSet(t, ciGoSteps(ciGoJob(d, "go-coverage"))[1], "uses", "actions/checkout@"+strings.Repeat("a", 40))
		}},
		{"setup toolchain source", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoSteps(ciGoJob(d, "go-test-core"))[1], "with"), "go-version-file", "worker/go.mod")
		}},
		{"setup cache", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoSteps(ciGoJob(d, "go-coverage"))[2], "with"), "cache", "false")
		}},
		{"modules checkout differs", func(d *yaml.Node) {
			ciGoSet(t, ciGoSteps(ciGoJob(d, "go-modules-core"))[0], "uses", "actions/checkout@"+strings.Repeat("a", 40))
		}},
		{"modules setup differs", func(d *yaml.Node) {
			ciGoSet(t, ciGoSteps(ciGoJob(d, "go-modules-core"))[1], "uses", "actions/setup-go@"+strings.Repeat("a", 40))
		}},
		{"modules toolchain source", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoSteps(ciGoJob(d, "go-modules-core"))[1], "with"), "go-version-file", "worker/go.mod")
		}},
		{"modules cache", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoSteps(ciGoJob(d, "go-modules-core"))[1], "with"), "cache", "false")
		}},
		{"modules setup order", func(d *yaml.Node) {
			steps := ciGoSteps(ciGoJob(d, "go-modules-core"))
			steps[0], steps[1] = steps[1], steps[0]
		}},
		{"modules skip root", func(d *yaml.Node) {
			ciGoSet(t, ciGoSteps(ciGoJob(d, "go-modules-core"))[2], "run", "scripts/test-go-modules.sh --skip-root")
		}},
		{"modules masked failure", func(d *yaml.Node) {
			ciGoSet(t, ciGoSteps(ciGoJob(d, "go-modules-core"))[2], "run", "scripts/test-go-modules.sh || true")
		}},
		{"modules moved back to tests", func(d *yaml.Node) {
			tests := ciGoField(ciGoJob(d, "go-test-core"), "steps")
			modules := ciGoField(ciGoJob(d, "go-modules-core"), "steps")
			tests.Content = append(tests.Content, modules.Content[2])
			modules.Content = modules.Content[:2]
		}},
		{"push path filter", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoField(d, "on"), "push"), "paths", "['**.go']")
		}},
		{"PR path filter", func(d *yaml.Node) {
			ciGoSet(t, ciGoField(d, "on"), "pull_request", "{paths-ignore: ['docs/**']}")
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
			steps := ciGoSteps(ciGoJob(d, "go-test-core"))
			steps[3], steps[5] = steps[5], steps[3]
		}},
		{"comment out race command", func(d *yaml.Node) {
			ciGoField(ciGoSteps(ciGoJob(d, "go-test-core"))[5], "run").Value = "# go test -race -timeout=20m ./...\ntrue\n"
		}},
		{"implicit package timeout", func(d *yaml.Node) {
			ciGoField(ciGoSteps(ciGoJob(d, "go-test-core"))[5], "run").Value = "go test -race ./..."
		}},
		{"unbounded package timeout", func(d *yaml.Node) {
			ciGoField(ciGoSteps(ciGoJob(d, "go-test-core"))[5], "run").Value = "go test -race -timeout=0 ./..."
		}},
		{"remove Linux skip assertion", func(d *yaml.Node) {
			run := ciGoField(ciGoSteps(ciGoJob(d, "go-test-core"))[6], "run")
			run.Value = strings.Replace(run.Value, "assert not any", "# assert not any", 1)
		}},
		{"remove Linux required case", func(d *yaml.Node) {
			run := ciGoField(ciGoSteps(ciGoJob(d, "go-test-core"))[6], "run")
			run.Value = strings.Replace(run.Value, "    'TestWSL2ProductionCleanupKillsEntireStagingGroup',\n", "", 1)
		}},
		{"remove SSH skip assertion", func(d *yaml.Node) {
			run := ciGoField(ciGoSteps(ciGoJob(d, "go-test-core"))[7], "run")
			run.Value = strings.Replace(run.Value, "assert not any", "# assert not any", 1)
		}},
		{"remove SSH proxy cases", func(d *yaml.Node) {
			run := ciGoField(ciGoSteps(ciGoJob(d, "go-test-core"))[7], "run")
			run.Value = strings.Replace(run.Value, "for proxy in ('false', 'true')", "for proxy in ('false',)", 1)
		}},
		{"logging after predicate", func(d *yaml.Node) {
			ciGoField(ciGoSteps(ciGoJob(d, "go"))[0], "run").Value += "echo done\n"
		}},
	}
	for _, binding := range []string{"GO_TEST_RESULT", "GO_MODULES_RESULT", "GO_COVERAGE_RESULT"} {
		mutations = append(mutations, mutation{"alter " + binding, func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoSteps(ciGoJob(d, "go"))[0], "env"), binding, "${{ needs.worker.result }}")
		}})
	}
	for _, binding := range []string{"CONTRACTS_RESULT", "ARCHITECTURE_RESULT"} {
		mutations = append(mutations, mutation{"alter Windows " + binding, func(d *yaml.Node) {
			ciGoSet(t, ciGoField(ciGoSteps(ciGoJob(d, "windows-pond-mesh-cancellation"))[0], "env"), binding, "${{ needs.worker.result }}")
		}})
	}
	for _, id := range []string{"go-test", "go-modules", "go-coverage", "go", "windows-pond-mesh-cancellation"} {
		mutations = append(mutations, mutation{id + " runs after workflow cancellation", func(d *yaml.Node) {
			ciGoSet(t, ciGoJob(d, id), "if", "${{ always() }}")
		}})
	}
	for _, id := range []string{"go-test-core", "go-test", "go-modules", "go-modules-core", "go-modules-cli", "go-coverage", "go", "windows-pond-mesh-cancellation"} {
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
		{"weaken first AND to OR", func(script string) string { return strings.Replace(script, " && ", " || ", 1) }},
		{"weaken second AND to OR", func(script string) string {
			index := strings.LastIndex(script, " && ")
			return script[:index] + strings.Replace(script[index:], " && ", " || ", 1)
		}},
		{"ignore modules result", func(script string) string {
			return strings.Replace(script, ` && "${GO_MODULES_RESULT:-}" == success`, "", 1)
		}},
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
	{"Test", "bash", `packages=$(go list ./...)
mapfile -t packages < <(printf '%s\n' "$packages" | sed '\|^github.com/openclaw/crabbox/internal/cli$|d')
go test -race -count=1 -timeout=10m "${packages[@]}"
`},
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
	{"Require real SSH readiness diagnostics", "bash", `go test ./internal/cli -run '^TestWaitForSSHReadyRealSSHTimeoutDiagnostic$' -count=1 -json | tee "$RUNNER_TEMP/ssh-readiness-tests.jsonl"
python3 - "$RUNNER_TEMP/ssh-readiness-tests.jsonl" <<'PY'
import json, sys
records = [json.loads(line) for line in open(sys.argv[1])]
root = 'TestWaitForSSHReadyRealSSHTimeoutDiagnostic'
required = [root] + [
    f'{root}/proxy={proxy}/transport-success={success}'
    for proxy in ('false', 'true')
    for success in ('true', 'false')
]
for name in required:
    for action in ('run', 'pass'):
        assert any(r.get('Test') == name and r['Action'] == action for r in records), (name, action)
assert not any(r['Action'] == 'skip' for r in records), 'real SSH fixture skipped'
PY
`},
	{"Build", "", "go build -trimpath -o /tmp/crabbox ./cmd/crabbox"},
}
