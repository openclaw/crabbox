package blaxel

import (
	"context"
	"errors"
	"io"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeClient struct {
	probeCalls int
	listCalls  int
	listResult ListSandboxesResult
	probeErr   error
	listErr    error
}

func (f *fakeClient) BaseURL() string { return defaultAPIURL }
func (f *fakeClient) Probe(context.Context) error {
	f.probeCalls++
	return f.probeErr
}
func (f *fakeClient) CreateSandbox(context.Context, CreateSandboxRequest) (Sandbox, error) {
	return Sandbox{}, errors.New("unexpected mutation")
}
func (f *fakeClient) GetSandbox(context.Context, string) (Sandbox, error) {
	return Sandbox{}, nil
}
func (f *fakeClient) ListSandboxes(context.Context, ListSandboxesRequest) (ListSandboxesResult, error) {
	f.listCalls++
	return f.listResult, f.listErr
}
func (f *fakeClient) UpdateSandboxLabels(context.Context, string, map[string]string) (Sandbox, error) {
	return Sandbox{}, errors.New("unexpected mutation")
}
func (f *fakeClient) DeleteSandbox(context.Context, string) error {
	return errors.New("unexpected mutation")
}
func (f *fakeClient) ExecuteProcess(context.Context, string, ExecuteProcessRequest) (Process, error) {
	return Process{}, errors.New("unexpected mutation")
}
func (f *fakeClient) GetProcess(context.Context, string, string) (Process, error) {
	return Process{}, nil
}
func (f *fakeClient) GetProcessLogs(context.Context, string, string) (ProcessLogs, error) {
	return ProcessLogs{}, nil
}
func (f *fakeClient) StopProcess(context.Context, string, string) error {
	return errors.New("unexpected mutation")
}
func (f *fakeClient) WriteFile(context.Context, string, WriteFileRequest) error {
	return errors.New("unexpected mutation")
}
func (f *fakeClient) UploadFile(context.Context, string, string, io.Reader) error {
	return errors.New("unexpected mutation")
}
func (f *fakeClient) GetDirectoryTree(context.Context, string, string) (DirectoryTree, error) {
	return DirectoryTree{}, nil
}

func TestDoctorMissingCredentialsIsRedactedAndNonMutating(t *testing.T) {
	fake := &fakeClient{}
	backend := &backend{
		spec: Provider{}.Spec(),
		cfg:  core.Config{Blaxel: core.BlaxelConfig{APIURL: defaultAPIURL}},
		clientFactory: func(Config, Runtime) (Client, error) {
			return fake, nil
		},
	}
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err == nil {
		t.Fatal("Doctor succeeded without credentials")
	}
	if result.Provider != providerName || result.Status != "failed" {
		t.Fatalf("result=%#v", result)
	}
	if fake.probeCalls != 0 || fake.listCalls != 0 {
		t.Fatalf("doctor called client without credentials: probe=%d list=%d", fake.probeCalls, fake.listCalls)
	}
}

func TestDoctorUsesOnlyProbeAndList(t *testing.T) {
	fake := &fakeClient{listResult: ListSandboxesResult{Sandboxes: []Sandbox{{ID: "sbx_1"}}}}
	backend := &backend{
		spec: Provider{}.Spec(),
		cfg: core.Config{Blaxel: core.BlaxelConfig{
			APIURL:    defaultAPIURL,
			APIKey:    "test-key",
			Workspace: "workspace-test",
		}},
		clientFactory: func(Config, Runtime) (Client, error) {
			return fake, nil
		},
	}
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if fake.probeCalls != 1 || fake.listCalls != 1 {
		t.Fatalf("doctor calls probe=%d list=%d", fake.probeCalls, fake.listCalls)
	}
	if result.Provider != providerName || result.Message == "" || len(result.Checks) == 0 {
		t.Fatalf("result=%#v", result)
	}
}
