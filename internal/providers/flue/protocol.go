package flue

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	defaultStdoutLimitBytes  = 10 * 1024 * 1024
	defaultStderrLimitBytes  = 10 * 1024 * 1024
	maxArtifactMetadataItems = 128
)

type CLIInput struct {
	RequestFile string `json:"requestFile"`
}

type Request struct {
	ProtocolVersion  int               `json:"protocolVersion"`
	Operation        string            `json:"operation"`
	LeaseID          string            `json:"leaseId,omitempty"`
	Slug             string            `json:"slug,omitempty"`
	Workflow         string            `json:"workflow"`
	Target           string            `json:"target"`
	WorkspaceArchive string            `json:"workspaceArchive"`
	Workspace        string            `json:"workspace"`
	Command          []string          `json:"command"`
	Env              map[string]string `json:"env,omitempty"`
	TimeoutMs        int64             `json:"timeoutMs,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	OutputLimits     OutputLimits      `json:"outputLimits,omitempty"`
}

type OutputLimits struct {
	StdoutBytes int64 `json:"stdoutBytes,omitempty"`
	StderrBytes int64 `json:"stderrBytes,omitempty"`
}

type Response struct {
	ProtocolVersion int                `json:"protocolVersion"`
	Operation       string             `json:"operation,omitempty"`
	LeaseID         string             `json:"leaseId,omitempty"`
	Slug            string             `json:"slug,omitempty"`
	ExitCode        int                `json:"exitCode"`
	Stdout          string             `json:"stdout,omitempty"`
	Stderr          string             `json:"stderr,omitempty"`
	Timing          ResponseTiming     `json:"timing,omitempty"`
	Artifacts       []ResponseArtifact `json:"artifacts,omitempty"`
	Error           string             `json:"error,omitempty"`
}

type ResponseTiming struct {
	TotalMs int64 `json:"totalMs,omitempty"`
	RunMs   int64 `json:"runMs,omitempty"`
}

type ResponseArtifact struct {
	Path        string            `json:"path"`
	Destination string            `json:"destination,omitempty"`
	SizeBytes   int64             `json:"sizeBytes,omitempty"`
	SHA256      string            `json:"sha256,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

func ParseCLIInput(data []byte) (CLIInput, error) {
	var input CLIInput
	if err := json.Unmarshal(data, &input); err != nil {
		return CLIInput{}, fmt.Errorf("parse flue input pointer: malformed JSON")
	}
	if err := input.Validate(); err != nil {
		return CLIInput{}, err
	}
	return input, nil
}

func (input CLIInput) Validate() error {
	if strings.TrimSpace(input.RequestFile) == "" {
		return exit(2, "flue input requestFile is required")
	}
	return nil
}

func ParseRequest(data []byte) (Request, error) {
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		return Request{}, fmt.Errorf("parse flue request: malformed JSON")
	}
	if err := req.Validate(); err != nil {
		return Request{}, err
	}
	return req, nil
}

func (req Request) Validate() error {
	if req.ProtocolVersion != protocolVersion {
		return exit(2, "unsupported flue protocolVersion %d", req.ProtocolVersion)
	}
	if strings.TrimSpace(req.Operation) != operationRun {
		return exit(2, "unsupported flue operation %q", strings.TrimSpace(req.Operation))
	}
	if strings.TrimSpace(req.Workflow) == "" {
		return exit(2, "flue request workflow is required")
	}
	target := strings.TrimSpace(req.Target)
	if target == "" {
		target = defaultTarget
	}
	if target != defaultTarget {
		return exit(2, "flue request target %q is unsupported in v1", target)
	}
	if strings.TrimSpace(req.WorkspaceArchive) == "" {
		return exit(2, "flue request workspaceArchive is required")
	}
	if strings.TrimSpace(req.Workspace) == "" {
		return exit(2, "flue request workspace is required")
	}
	if len(req.Command) == 0 {
		return exit(2, "flue request command is required")
	}
	if strings.TrimSpace(req.Command[0]) == "" {
		return exit(2, "flue request command[0] must not be empty")
	}
	if req.TimeoutMs < 0 {
		return exit(2, "flue request timeoutMs must be non-negative")
	}
	if req.OutputLimits.StdoutBytes < 0 || req.OutputLimits.StderrBytes < 0 {
		return exit(2, "flue request output limits must be non-negative")
	}
	return nil
}

func (req Request) EffectiveOutputLimits() OutputLimits {
	limits := req.OutputLimits
	if limits.StdoutBytes == 0 {
		limits.StdoutBytes = defaultStdoutLimitBytes
	}
	if limits.StderrBytes == 0 {
		limits.StderrBytes = defaultStderrLimitBytes
	}
	return limits
}

func ParseResponse(data []byte) (Response, error) {
	var resp Response
	if err := json.Unmarshal(data, &resp); err != nil {
		return Response{}, fmt.Errorf("parse flue response: malformed JSON")
	}
	if err := resp.Validate(); err != nil {
		return Response{}, err
	}
	return resp, nil
}

func (resp Response) Validate() error {
	if resp.ProtocolVersion != protocolVersion {
		return exit(2, "unsupported flue response protocolVersion %d", resp.ProtocolVersion)
	}
	if op := strings.TrimSpace(resp.Operation); op != "" && op != operationRun {
		return exit(2, "unsupported flue response operation %q", op)
	}
	if resp.Timing.TotalMs < 0 || resp.Timing.RunMs < 0 {
		return exit(2, "flue response timing must be non-negative")
	}
	if len(resp.Artifacts) > maxArtifactMetadataItems {
		return exit(2, "flue response artifacts exceed limit %d", maxArtifactMetadataItems)
	}
	for i, artifact := range resp.Artifacts {
		if strings.TrimSpace(artifact.Path) == "" {
			return exit(2, "flue response artifact[%d] path is required", i)
		}
		if artifact.SizeBytes < 0 {
			return exit(2, "flue response artifact[%d] sizeBytes must be non-negative", i)
		}
	}
	return nil
}

func ParseResponseFromStdout(stdout string) (Response, error) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return Response{}, exit(5, "flue run produced no protocol response on stdout")
	}
	if resp, err := ParseResponse([]byte(trimmed)); err == nil {
		return resp, nil
	}
	lines := bytes.Split([]byte(stdout), []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		resp, err := ParseResponse(line)
		if err == nil {
			return resp, nil
		}
		return Response{}, exit(5, "parse flue protocol response: %v", err)
	}
	return Response{}, exit(5, "flue run stdout did not contain a protocol JSON response")
}
