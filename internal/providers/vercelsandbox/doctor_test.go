package vercelsandbox

import (
	"context"
	"errors"
	"testing"
)

type fakeClient struct {
	sdkErr     error
	cliPath    string
	cliErr     error
	authErr    error
	projectErr error
	listErr    error
	list       []sandboxSummary
	calls      []string
}

func (f *fakeClient) CheckSDK(context.Context) error {
	f.calls = append(f.calls, "sdk")
	return f.sdkErr
}

func (f *fakeClient) CheckCLI(context.Context) (string, error) {
	f.calls = append(f.calls, "cli")
	return f.cliPath, f.cliErr
}

func (f *fakeClient) CheckAuth(context.Context) error {
	f.calls = append(f.calls, "auth")
	return f.authErr
}

func (f *fakeClient) CheckProject(context.Context) error {
	f.calls = append(f.calls, "project")
	return f.projectErr
}

func (f *fakeClient) ListSandboxes(context.Context) ([]sandboxSummary, error) {
	f.calls = append(f.calls, "list")
	return f.list, f.listErr
}

func TestDoctorReadyIsNonMutating(t *testing.T) {
	fake := &fakeClient{cliPath: "/opt/bin/sandbox", list: []sandboxSummary{{ID: "vsbx_1"}}}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg:  Config{},
		newClient: func(Config, Runtime) (vercelSandboxClient, error) {
			return fake, nil
		},
	}
	result, err := b.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != providerName || result.Status != "ok" {
		t.Fatalf("result=%#v", result)
	}
	wantCalls := []string{"sdk", "cli", "auth", "project", "list"}
	if len(fake.calls) != len(wantCalls) {
		t.Fatalf("calls=%v", fake.calls)
	}
	for i := range wantCalls {
		if fake.calls[i] != wantCalls[i] {
			t.Fatalf("calls=%v want %v", fake.calls, wantCalls)
		}
	}
	for _, check := range result.Checks {
		if check.Details["mutation"] == "" {
			t.Fatalf("check missing mutation=false detail: %#v", check)
		}
	}
}

func TestDoctorReportsEnvironmentBlockersWithoutMutating(t *testing.T) {
	fake := &fakeClient{
		cliErr:     errors.New("sandbox missing"),
		authErr:    errors.New("auth missing"),
		projectErr: errors.New("project missing"),
		listErr:    errors.New("list unavailable"),
	}
	b := &backend{
		spec: Provider{}.Spec(),
		newClient: func(Config, Runtime) (vercelSandboxClient, error) {
			return fake, nil
		},
	}
	result, err := b.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" {
		t.Fatalf("status=%q checks=%#v", result.Status, result.Checks)
	}
	seen := map[string]string{}
	for _, check := range result.Checks {
		seen[check.Check] = check.Status
		if check.Check != "sdk" && check.Details["class"] != "environment_blocked" {
			t.Fatalf("check missing environment classification: %#v", check)
		}
	}
	for _, name := range []string{"cli", "auth", "project", "inventory"} {
		if seen[name] == "" {
			t.Fatalf("missing check %q in %#v", name, result.Checks)
		}
	}
}
