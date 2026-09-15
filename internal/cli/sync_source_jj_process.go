package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The caller owns binary provenance and native configuration/environment policy.
// This internal adapter never discovers a binary on PATH or inherits an implicit
// child environment. Production routing stays disabled until those owners exist.
type jjSourceProcessOptions struct {
	BinaryPath           string
	Directory            string
	Environment          []string
	ConditionEnvironment []string
	Limits               jjSourceProcessLimits
	Runner               CommandRunner
}

type jjSourceProcessLimits struct {
	ObjectBytes   uint64
	InputBytes    uint64
	ScratchBytes  uint64
	MetadataBytes int
	Timeout       time.Duration
}

type jjSourceProcess struct {
	options              jjSourceProcessOptions
	conditionEnvironment map[string]string
}

const jjConditionRequestExit = 75
const jjMaxConditionInput = 1 << 20
const jjMaxConditionDecisions = 1024

type jjEnvironmentCondition struct {
	jjProtocolHeader
	Predicate string `json:"predicate"`
}

type jjConditionAnswer struct {
	Predicate string `json:"predicate"`
	Matched   bool   `json:"matched"`
}

type jjConditionInput struct {
	Protocol      string              `json:"protocol"`
	SchemaVersion int                 `json:"schema_version"`
	Answers       []jjConditionAnswer `json:"answers"`
}

// Native JJ captures Unicode pairs in an exact-key map, even on Windows.
func jjConditionEnvironment(environment []string) map[string]string {
	result := map[string]string{}
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok && utf8.ValidString(name) && utf8.ValidString(value) {
			result[name] = value
		}
	}
	return result
}

func matchesJJEnvironmentCondition(environment map[string]string, predicate string) bool {
	name, expected, equality := strings.Cut(predicate, "=")
	value, present := environment[name]
	return present && (!equality || value == expected)
}

type jjSourceVersion struct {
	jjProtocolHeader
	Capabilities []string `json:"capabilities"`
}

func newJJSourceProcess(ctx context.Context, options jjSourceProcessOptions) (*jjSourceProcess, error) {
	if !filepath.IsAbs(options.BinaryPath) || !filepath.IsAbs(options.Directory) || options.Environment == nil || options.ConditionEnvironment == nil {
		return nil, fmt.Errorf("native source helper requires an absolute binary, directory, and explicit child/condition environments")
	}
	if options.Limits.MetadataBytes <= 0 || options.Limits.Timeout <= 0 {
		return nil, fmt.Errorf("native source helper requires finite metadata and time limits")
	}
	options.Environment = append([]string{}, options.Environment...)
	if options.Runner == nil {
		options.Runner = execCommandRunner{}
	}
	conditions := jjConditionEnvironment(options.ConditionEnvironment)
	options.ConditionEnvironment = nil
	helper := &jjSourceProcess{options: options, conditionEnvironment: conditions}
	data, err := helper.invoke(ctx, "version", "source-version")
	if err != nil {
		return nil, err
	}
	var version jjSourceVersion
	if err := decodeJJProtocolMessage(data, "version", &version); err != nil {
		return nil, err
	}
	for _, capability := range []string{"context", "live_inventory", "recorded_inventory", "recorded_export", "environment_conditions"} {
		if !slices.Contains(version.Capabilities, capability) {
			return nil, fmt.Errorf("native source helper lacks %s capability", capability)
		}
	}
	return helper, nil
}

func (h *jjSourceProcess) invoke(ctx context.Context, phase string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, h.options.Limits.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	commandArgs := []string{"--config-conditions-stdin", "--no-pager", "--color=never"}
	if phase != "version" {
		limits := h.options.Limits
		commandArgs = append(commandArgs, "--max-object-allocation-bytes", strconv.FormatUint(limits.ObjectBytes, 10),
			"--max-input-bytes", strconv.FormatUint(limits.InputBytes, 10),
			"--max-conflict-scratch-bytes", strconv.FormatUint(limits.ScratchBytes, 10), "-R", h.options.Directory)
	}
	input := jjConditionInput{Protocol: jjSourceProtocolName, SchemaVersion: jjSourceProtocolVersion, Answers: []jjConditionAnswer{}}
	answered := map[string]bool{}
	for {
		request, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		if len(request) > jjMaxConditionInput {
			return nil, fmt.Errorf("native environment condition input exceeds limit")
		}
		result, err := h.options.Runner.Run(ctx, LocalCommandRequest{
			Name: h.options.BinaryPath, Args: append(append([]string{}, commandArgs...), args...), Dir: h.options.Directory,
			Env: append([]string{}, h.options.Environment...), Stdin: bytes.NewReader(request), MaxCapturedOutputBytes: h.options.Limits.MetadataBytes,
		})
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if result.ExitCode == jjConditionRequestExit && IsPlainLocalCommandExit(result, err) {
			var condition jjEnvironmentCondition
			if err := decodeJJProtocolMessage([]byte(result.Stdout), "environment_condition", &condition); err != nil {
				return nil, err
			}
			if answered[condition.Predicate] || len(input.Answers) >= jjMaxConditionDecisions {
				return nil, fmt.Errorf("native environment condition negotiation did not converge within limits")
			}
			input.Answers = append(input.Answers, jjConditionAnswer{Predicate: condition.Predicate, Matched: matchesJJEnvironmentCondition(h.conditionEnvironment, condition.Predicate)})
			answered[condition.Predicate] = true
			continue
		}
		if err != nil || result.ExitCode != 0 {
			// Captured native diagnostics may contain user configuration. Do not replay them.
			if err == nil {
				err = fmt.Errorf("exit status %d", result.ExitCode)
			}
			return nil, fmt.Errorf("native source %s failed: %w", phase, err)
		}
		return []byte(result.Stdout), nil
	}
}

func (h *jjSourceProcess) sourceContext(ctx context.Context) ([]byte, jjSourceContext, error) {
	data, err := h.invoke(ctx, "context", "source-context")
	if err != nil {
		return nil, jjSourceContext{}, err
	}
	sourceContext, err := decodeJJSourceContext(data)
	return data, sourceContext, err
}

func (h *jjSourceProcess) readLive(ctx context.Context) (jjSourceRead, error) {
	data, sourceContext, err := h.sourceContext(ctx)
	if err != nil {
		return jjSourceRead{}, err
	}
	inventory, err := h.invoke(ctx, "live inventory", "--at-op="+sourceContext.OperationHeads[0], "source-live-inventory")
	if err != nil {
		return jjSourceRead{}, err
	}
	return decodeJJSourceRead(data, inventory)
}

func (h *jjSourceProcess) readRecorded(ctx context.Context, revision string) (jjRecordedInventory, error) {
	if strings.TrimSpace(revision) == "" {
		return jjRecordedInventory{}, fmt.Errorf("recorded native source requires an explicit revision")
	}
	_, sourceContext, err := h.sourceContext(ctx)
	if err != nil {
		return jjRecordedInventory{}, err
	}
	data, err := h.invoke(ctx, "recorded inventory", "--at-op="+sourceContext.OperationHeads[0], "source-recorded-inventory", "--revision", revision)
	if err != nil {
		return jjRecordedInventory{}, err
	}
	inventory, err := decodeJJRecordedInventory(data)
	if err != nil {
		return jjRecordedInventory{}, err
	}
	if err := validateJJContextIdentity(sourceContext, inventory.Identity); err != nil {
		return jjRecordedInventory{}, err
	}
	return inventory, nil
}

// Export uses the captured native commit/operation identity, not a re-resolved
// bookmark. The caller owns the existing empty payload/state sibling directories.
func (h *jjSourceProcess) exportRecorded(ctx context.Context, inventory jjRecordedInventory, paths []string, outputRoot, stateRoot string, entryBytes, outputBytes uint64) (result jjRecordedExport, err error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	requestData, err := encodeJJRecordedExportRequest(inventory, paths)
	if err != nil {
		return result, err
	}
	if !filepath.IsAbs(outputRoot) || !filepath.IsAbs(stateRoot) || filepath.Dir(outputRoot) != filepath.Dir(stateRoot) || outputRoot == stateRoot {
		return result, fmt.Errorf("native export requires distinct absolute owned payload/state siblings")
	}
	parent := canonicalRepositoryPath(filepath.Dir(outputRoot))
	for _, protected := range []string{inventory.Identity.WorkspaceRoot, inventory.Identity.RepositoryPath} {
		if pathWithinRoot(parent, canonicalRepositoryPath(protected)) {
			return result, fmt.Errorf("native export request must be outside source and repository administration")
		}
	}
	requestFile, err := os.CreateTemp(parent, ".crabbox-jj-selection-*")
	if err != nil {
		return result, fmt.Errorf("create native export request: %w", err)
	}
	defer func() {
		if cleanupErr := os.Remove(requestFile.Name()); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove native export request %s: %w", requestFile.Name(), cleanupErr))
		}
	}()
	_, writeErr := requestFile.Write(requestData)
	if err := errors.Join(writeErr, requestFile.Close()); err != nil {
		return result, fmt.Errorf("write native export request: %w", err)
	}
	data, err := h.invoke(ctx, "recorded export", "--at-op="+inventory.Identity.Operation, "source-recorded-export",
		"--selected", requestFile.Name(), "--output", outputRoot, "--state", stateRoot,
		"--max-entry-bytes", strconv.FormatUint(entryBytes, 10), "--max-output-bytes", strconv.FormatUint(outputBytes, 10))
	if err != nil {
		return result, err
	}
	var request jjRecordedExportRequest
	if err := json.Unmarshal(requestData, &request); err != nil {
		return result, err
	}
	return decodeJJRecordedExport(data, request, canonicalRepositoryPath(outputRoot), canonicalRepositoryPath(stateRoot))
}
