package flue

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProtocolInputAndRequestRoundTrip(t *testing.T) {
	input, err := ParseCLIInput([]byte(`{"requestFile":"/tmp/crabbox-flue-request.json"}`))
	if err != nil {
		t.Fatal(err)
	}
	if input.RequestFile != "/tmp/crabbox-flue-request.json" {
		t.Fatalf("requestFile=%q", input.RequestFile)
	}

	req := validRequest()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRequest(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ProtocolVersion != protocolVersion || parsed.Operation != operationRun || parsed.Command[0] != "go" {
		t.Fatalf("parsed request=%#v", parsed)
	}
	limits := parsed.EffectiveOutputLimits()
	if limits.StdoutBytes != defaultStdoutLimitBytes || limits.StderrBytes != defaultStderrLimitBytes {
		t.Fatalf("limits=%#v", limits)
	}
}

func TestProtocolValidationRejectsMalformedRequests(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Request)
		want string
	}{
		{name: "version", edit: func(req *Request) { req.ProtocolVersion = 2 }, want: "protocolVersion"},
		{name: "operation", edit: func(req *Request) { req.Operation = "doctor" }, want: "operation"},
		{name: "workflow", edit: func(req *Request) { req.Workflow = "" }, want: "workflow"},
		{name: "target", edit: func(req *Request) { req.Target = "cloudflare" }, want: "unsupported"},
		{name: "archive", edit: func(req *Request) { req.WorkspaceArchive = "" }, want: "workspaceArchive"},
		{name: "workspace", edit: func(req *Request) { req.Workspace = "" }, want: "workspace"},
		{name: "command", edit: func(req *Request) { req.Command = nil }, want: "command"},
		{name: "timeout", edit: func(req *Request) { req.TimeoutMs = -1 }, want: "timeoutMs"},
		{name: "limits", edit: func(req *Request) { req.OutputLimits.StdoutBytes = -1 }, want: "output limits"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := validRequest()
			tc.edit(&req)
			err := req.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate err=%v want %q", err, tc.want)
			}
		})
	}
}

func TestProtocolPreservesEmptyCommandArguments(t *testing.T) {
	req := validRequest()
	req.Command = []string{"printf", ""}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate rejected empty argv argument: %v", err)
	}
	req.Command = []string{""}
	err := req.Validate()
	if err == nil || !strings.Contains(err.Error(), "command[0]") {
		t.Fatalf("Validate err=%v want command[0] rejection", err)
	}
}

func TestProtocolErrorsDoNotEchoSecretEnvValues(t *testing.T) {
	req := validRequest()
	req.Env = map[string]string{"TOKEN": "super-secret-token"}
	req.OutputLimits.StdoutBytes = -1
	err := req.Validate()
	if err == nil {
		t.Fatal("Validate err=<nil>")
	}
	if strings.Contains(err.Error(), "super-secret-token") || strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("validation error leaked env detail: %v", err)
	}

	_, err = ParseRequest([]byte(`{"protocolVersion":1,"operation":"run","env":{"TOKEN":"super-secret-token"}`))
	if err == nil {
		t.Fatal("ParseRequest err=<nil>")
	}
	if strings.Contains(err.Error(), "super-secret-token") || strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("parse error leaked JSON detail: %v", err)
	}
}

func TestProtocolResponseValidation(t *testing.T) {
	resp := Response{
		ProtocolVersion: protocolVersion,
		Operation:       operationRun,
		ExitCode:        7,
		Stdout:          "ok",
		Stderr:          "failure",
		Timing:          ResponseTiming{TotalMs: 100, RunMs: 90},
		Artifacts:       []ResponseArtifact{{Path: "logs/output.txt", SizeBytes: 12}},
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseResponse(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ExitCode != 7 || parsed.Stdout != "ok" || len(parsed.Artifacts) != 1 {
		t.Fatalf("parsed response=%#v", parsed)
	}

	resp.Artifacts[0].Path = ""
	err = resp.Validate()
	if err == nil || !strings.Contains(err.Error(), "artifact[0] path") {
		t.Fatalf("Validate response err=%v", err)
	}
}

func validRequest() Request {
	return Request{
		ProtocolVersion:  protocolVersion,
		Operation:        operationRun,
		LeaseID:          "flue_123",
		Slug:             "sample",
		Workflow:         defaultWorkflow,
		Target:           defaultTarget,
		WorkspaceArchive: "/tmp/workspace.tar.gz",
		Workspace:        defaultWorkdir,
		Command:          []string{"go", "test", "./..."},
		Env:              map[string]string{"CI": "1"},
		TimeoutMs:        60000,
		Metadata:         map[string]string{"repo": "my-app"},
	}
}
