package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
	"github.com/islo-labs/go-sdk/core"
)

// fakeIsloClient is a hand-rolled IsloClient for tests.
type fakeIsloClient struct {
	createCalls atomic.Int32
	deleteCalls atomic.Int32
	execCalls   atomic.Int32

	createFn func(*gosdk.SandboxCreate) (*gosdk.SandboxResponse, error)
	getFn    func(name string) (*gosdk.SandboxResponse, error)
	listFn   func() ([]*gosdk.SandboxResponse, error)
	deleteFn func(name string) error
	execFn   func(name string, req *gosdk.ExecRequest, stdout, stderr io.Writer) (int, error)
}

func (f *fakeIsloClient) CreateSandbox(_ context.Context, req *gosdk.SandboxCreate) (*gosdk.SandboxResponse, error) {
	f.createCalls.Add(1)
	if f.createFn != nil {
		return f.createFn(req)
	}
	name := ""
	if req != nil && req.Name != nil {
		name = *req.Name
	}
	return &gosdk.SandboxResponse{Name: name, Status: "running", Image: "ubuntu:24.04"}, nil
}

func (f *fakeIsloClient) GetSandbox(_ context.Context, name string) (*gosdk.SandboxResponse, error) {
	if f.getFn != nil {
		return f.getFn(name)
	}
	return &gosdk.SandboxResponse{Name: name, Status: "running"}, nil
}

func (f *fakeIsloClient) ListSandboxes(_ context.Context) ([]*gosdk.SandboxResponse, error) {
	if f.listFn != nil {
		return f.listFn()
	}
	return nil, nil
}

func (f *fakeIsloClient) DeleteSandbox(_ context.Context, name string) error {
	f.deleteCalls.Add(1)
	if f.deleteFn != nil {
		return f.deleteFn(name)
	}
	return nil
}

func (f *fakeIsloClient) ExecStream(_ context.Context, name string, req *gosdk.ExecRequest, stdout, stderr io.Writer) (int, error) {
	f.execCalls.Add(1)
	if f.execFn != nil {
		return f.execFn(name, req, stdout, stderr)
	}
	return 0, nil
}

func withFakeIslo(t *testing.T, fake *fakeIsloClient) {
	t.Helper()
	original := isloClientFactory
	isloClientFactory = func(_ Config) (IsloClient, error) { return fake, nil }
	t.Cleanup(func() { isloClientFactory = original })
}

func setupIsloHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
}

func TestIsIsloProvider(t *testing.T) {
	if !isIsloProvider("islo") {
		t.Fatal("islo should match")
	}
	if isIsloProvider("blacksmith") {
		t.Fatal("blacksmith should not match islo")
	}
}

func TestIsloWarmupCreatesSandboxAndClaim(t *testing.T) {
	setupIsloHome(t)
	fake := &fakeIsloClient{}
	withFakeIslo(t, fake)
	cfg := baseConfig()
	cfg.Provider = "islo"
	cfg.Islo.Image = "docker.io/library/ubuntu:24.04"
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	if err := app.isloWarmup(context.Background(), cfg, Repo{Root: "/repo", Name: "demo"}, true, false, false); err != nil {
		t.Fatal(err)
	}
	if fake.createCalls.Load() != 1 {
		t.Fatalf("createCalls=%d want 1", fake.createCalls.Load())
	}
	if fake.deleteCalls.Load() != 0 {
		t.Fatalf("deleteCalls=%d want 0 (warmup keeps sandbox)", fake.deleteCalls.Load())
	}
}

func TestIsloRunStreamsExecOutput(t *testing.T) {
	setupIsloHome(t)
	fake := &fakeIsloClient{
		execFn: func(_ string, _ *gosdk.ExecRequest, stdout, stderr io.Writer) (int, error) {
			_, _ = stdout.Write([]byte("hello\n"))
			_, _ = stderr.Write([]byte("warn\n"))
			return 0, nil
		},
	}
	withFakeIslo(t, fake)
	cfg := baseConfig()
	cfg.Provider = "islo"
	cfg.Islo.Image = "docker.io/library/ubuntu:24.04"
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.isloRun(context.Background(), cfg, Repo{Root: "/repo", Name: "demo"}, isloRunOptions{Command: []string{"echo", "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "hello") {
		t.Fatalf("stdout missing hello: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "warn") {
		t.Fatalf("stderr missing warn: %q", stderr.String())
	}
	if fake.createCalls.Load() != 1 || fake.execCalls.Load() != 1 || fake.deleteCalls.Load() != 1 {
		t.Fatalf("calls create=%d exec=%d delete=%d (want 1/1/1)", fake.createCalls.Load(), fake.execCalls.Load(), fake.deleteCalls.Load())
	}
}

func TestIsloRunPropagatesExitCode(t *testing.T) {
	setupIsloHome(t)
	fake := &fakeIsloClient{
		execFn: func(_ string, _ *gosdk.ExecRequest, _, _ io.Writer) (int, error) {
			return 7, nil
		},
	}
	withFakeIslo(t, fake)
	cfg := baseConfig()
	cfg.Provider = "islo"
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.isloRun(context.Background(), cfg, Repo{Root: "/repo", Name: "demo"}, isloRunOptions{Command: []string{"false"}})
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("err=%v want ExitError{Code:7}", err)
	}
}

func TestIsloRunKeepsSandboxWhenKeepSet(t *testing.T) {
	setupIsloHome(t)
	fake := &fakeIsloClient{}
	withFakeIslo(t, fake)
	cfg := baseConfig()
	cfg.Provider = "islo"
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.isloRun(context.Background(), cfg, Repo{Root: "/repo", Name: "demo"}, isloRunOptions{
		Command: []string{"true"},
		Keep:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fake.deleteCalls.Load() != 0 {
		t.Fatalf("deleteCalls=%d want 0 when --keep is set", fake.deleteCalls.Load())
	}
}

func TestIsloListJSONAndText(t *testing.T) {
	setupIsloHome(t)
	created := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	fake := &fakeIsloClient{
		listFn: func() ([]*gosdk.SandboxResponse, error) {
			return []*gosdk.SandboxResponse{
				{ID: "sb_1", Name: "demo-aaaa", Status: "running", Image: "ubuntu:24.04", CreatedAt: &created},
			}, nil
		},
	}
	withFakeIslo(t, fake)
	cfg := baseConfig()
	cfg.Provider = "islo"

	var jsonBuf bytes.Buffer
	app := App{Stdout: &jsonBuf, Stderr: io.Discard}
	if err := app.isloList(context.Background(), cfg, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonBuf.String(), `"name":"demo-aaaa"`) {
		t.Fatalf("json output missing sandbox: %q", jsonBuf.String())
	}

	var textBuf bytes.Buffer
	app = App{Stdout: &textBuf, Stderr: io.Discard}
	if err := app.isloList(context.Background(), cfg, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(textBuf.String(), "demo-aaaa") || !strings.Contains(textBuf.String(), "running") {
		t.Fatalf("text output missing fields: %q", textBuf.String())
	}
}

func TestIsloStatusWaitPolls(t *testing.T) {
	setupIsloHome(t)
	calls := 0
	fake := &fakeIsloClient{
		getFn: func(name string) (*gosdk.SandboxResponse, error) {
			calls++
			status := "provisioning"
			if calls >= 2 {
				status = "running"
			}
			return &gosdk.SandboxResponse{Name: name, Status: status}, nil
		},
	}
	withFakeIslo(t, fake)
	cfg := baseConfig()
	cfg.Provider = "islo"
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.isloStatus(ctx, cfg, "isb_demo-aaaa", true, 5*time.Second, false); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("getCalls=%d want >=2 (poll until running)", calls)
	}
}

func TestIsloStatusWaitTimesOut(t *testing.T) {
	setupIsloHome(t)
	fake := &fakeIsloClient{
		getFn: func(name string) (*gosdk.SandboxResponse, error) {
			return &gosdk.SandboxResponse{Name: name, Status: "provisioning"}, nil
		},
	}
	withFakeIslo(t, fake)
	cfg := baseConfig()
	cfg.Provider = "islo"
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.isloStatus(context.Background(), cfg, "isb_demo-aaaa", true, 50*time.Millisecond, false)
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 5 {
		t.Fatalf("err=%v want timeout exit 5", err)
	}
}

func TestIsloStopRemovesClaim(t *testing.T) {
	setupIsloHome(t)
	fake := &fakeIsloClient{}
	withFakeIslo(t, fake)
	cfg := baseConfig()
	cfg.Provider = "islo"

	leaseID := "isb_demo-aaaa"
	if err := claimLeaseForRepoProvider(leaseID, "blue-crab", isloProvider, "/repo", time.Hour, false); err != nil {
		t.Fatal(err)
	}
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	if err := app.isloStop(context.Background(), cfg, leaseID); err != nil {
		t.Fatal(err)
	}
	if fake.deleteCalls.Load() != 1 {
		t.Fatalf("deleteCalls=%d want 1", fake.deleteCalls.Load())
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID != "" {
		t.Fatalf("claim leaked: %#v", claim)
	}
}

func TestIsloAuthFailureSurfacesActionable(t *testing.T) {
	setupIsloHome(t)
	apiErr := core.NewAPIError(401, errors.New("Unauthorized"))
	fake := &fakeIsloClient{
		listFn: func() ([]*gosdk.SandboxResponse, error) {
			return nil, &gosdk.UnauthorizedError{APIError: apiErr}
		},
	}
	withFakeIslo(t, fake)
	cfg := baseConfig()
	cfg.Provider = "islo"
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.isloList(context.Background(), cfg, false)
	if err == nil || !strings.Contains(err.Error(), "ISLO_API_KEY") {
		t.Fatalf("err=%v should mention ISLO_API_KEY", err)
	}
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("expected exit 2, got %v", err)
	}
}

func TestIsloFactoryRequiresAPIKey(t *testing.T) {
	t.Setenv("ISLO_API_KEY", "")
	t.Setenv("ISLO_BASE_URL", "")
	cfg := baseConfig()
	cfg.Provider = "islo"
	_, err := isloClientFactory(cfg)
	if err == nil || !strings.Contains(err.Error(), "ISLO_API_KEY") {
		t.Fatalf("err=%v should mention ISLO_API_KEY", err)
	}
}

func TestResolveIsloLeaseID(t *testing.T) {
	setupIsloHome(t)
	if err := claimLeaseForRepoProvider("isb_owned", "blue-crab", isloProvider, "/repo", time.Hour, false); err != nil {
		t.Fatal(err)
	}
	t.Run("prefixed lease", func(t *testing.T) {
		lease, name, err := resolveIsloLeaseID("isb_owned", "/repo", false)
		if err != nil {
			t.Fatal(err)
		}
		if lease != "isb_owned" || name != "owned" {
			t.Fatalf("lease=%s name=%s", lease, name)
		}
	})
	t.Run("by slug", func(t *testing.T) {
		lease, name, err := resolveIsloLeaseID("blue-crab", "/repo", false)
		if err != nil {
			t.Fatal(err)
		}
		if lease != "isb_owned" || name != "owned" {
			t.Fatalf("lease=%s name=%s", lease, name)
		}
	})
	t.Run("rejects other-provider claim", func(t *testing.T) {
		if err := claimLeaseForRepoProvider("tbx_other", "red-prawn", "blacksmith-testbox", "/repo", time.Hour, false); err != nil {
			t.Fatal(err)
		}
		if _, _, err := resolveIsloLeaseID("red-prawn", "/repo", false); err == nil {
			t.Fatal("expected provider mismatch error")
		}
	})
}

func TestParseIsloSSE(t *testing.T) {
	body := strings.Join([]string{
		`: keepalive`,
		`event: stdout`,
		`data: hello-from-islo`,
		`data: `,
		``,
		`event: stderr`,
		`data: warn-line`,
		``,
		`event: exit`,
		`data: 3`,
		``,
	}, "\n")
	var stdout, stderr bytes.Buffer
	code, err := parseIsloSSE(strings.NewReader(body), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 {
		t.Fatalf("code=%d want 3", code)
	}
	if stdout.String() != "hello-from-islo\n" {
		t.Fatalf("stdout=%q want \"hello-from-islo\\n\"", stdout.String())
	}
	if stderr.String() != "warn-line" {
		t.Fatalf("stderr=%q want \"warn-line\"", stderr.String())
	}
}
