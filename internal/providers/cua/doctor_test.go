package cua

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestDoctorUsesBridgeClassification(t *testing.T) {
	runner := &recordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		_, _ = io.WriteString(req.Stdout, `{"ok":false,"class":"environment_blocked","doctor":{"pythonVersion":"3.11.9","importPath":"cua_sandbox","auth":"missing","checks":[{"status":"ok","check":"python","message":"python=3.11.9 required=\u003e=3.11,\u003c3.14"},{"status":"ok","check":"sdk","message":"import=cua_sandbox","details":{"import":"cua_sandbox"}},{"status":"failed","check":"auth","message":"auth=missing mutation=false","class":"environment_blocked"}]}}`)
		return LocalCommandResult{ExitCode: 0}, nil
	}}
	result, err := (backend{spec: Provider{}.Spec(), cfg: testConfig(), rt: Runtime{Exec: runner}}).Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if result.Status != "failed" || !strings.Contains(result.Message, "import=cua_sandbox") {
		t.Fatalf("result=%#v", result)
	}
	if len(result.Checks) != 5 {
		t.Fatalf("checks=%#v", result.Checks)
	}
	provisioning := result.Checks[1]
	if provisioning.Status != "warning" || provisioning.Check != "provisioning" || provisioning.Details["experimental"] != "true" || provisioning.Details["tracking_issue"] != cuaTrackingIssue {
		t.Fatalf("provisioning check=%#v", provisioning)
	}
	auth := result.Checks[len(result.Checks)-1]
	if auth.Check != "auth" || auth.Details["class"] != "environment_blocked" || auth.Details["mutation"] != "false" {
		t.Fatalf("auth check=%#v", auth)
	}
}

func TestDoctorClassifiesBridgeTransportFailureWithoutReturningError(t *testing.T) {
	runner := &recordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{ExitCode: 127}, nil
	}}
	result, err := (backend{spec: Provider{}.Spec(), cfg: testConfig(), rt: Runtime{Exec: runner}}).Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor returned transport error instead of classified result: %v", err)
	}
	if result.Status != "failed" || result.Checks[len(result.Checks)-1].Details["class"] != "environment_blocked" {
		t.Fatalf("result=%#v", result)
	}
}
