package machine0

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type recordingRunner struct {
	calls     []core.LocalCommandRequest
	responses map[string]core.LocalCommandResult
	errors    map[string]error
	sequence  []runnerResponse
}

type runnerResponse struct {
	result core.LocalCommandResult
	err    error
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if len(r.sequence) > 0 {
		response := r.sequence[0]
		r.sequence = r.sequence[1:]
		return response.result, response.err
	}
	key := strings.Join(req.Args, "\x00")
	return r.responses[key], r.errors[key]
}

func testClient(runner *recordingRunner) *client {
	return &client{cfg: Machine0Config{CLIPath: "/opt/bin/machine0"}, rt: Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard}}
}

func TestClientCreateCommandConstruction(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}, errors: map[string]error{}}
	c := testClient(runner)
	err := c.Create(context.Background(), createMachineRequest{Name: "crabbox-blue", Size: "gpu-h100-1", Region: "us-east", Image: "desktop-v2", ImageVersion: 3, Key: "ci-key"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"new", "crabbox-blue", "--size", "gpu-h100-1", "--region", "us-east", "--image", "desktop-v2", "--image-version", "3", "--key", "ci-key"}
	if len(runner.calls) != 1 || runner.calls[0].Name != "/opt/bin/machine0" || !reflect.DeepEqual(runner.calls[0].Args, want) {
		t.Fatalf("call=%#v want args=%#v", runner.calls, want)
	}
}

func TestClientSelectedKeyReadsExplicitOrDefaultKey(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selected string
		command  string
		output   string
		wantName string
	}{
		{name: "explicit key", selected: "remote-key", command: "keys\x00get\x00remote-key\x00--json", output: `{"name":"remote-key","type":"PUBLIC","fileName":"id_ed25519"}`, wantName: "remote-key"},
		{name: "account default", command: "keys\x00ls\x00--json", output: `[{"name":"other","type":"MANAGED"},{"name":"default-key","type":"PUBLIC","fileName":"id_rsa","isDefault":true}]`, wantName: "default-key"},
		{name: "no default preserves CLI behavior", command: "keys\x00ls\x00--json", output: `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{tc.command: {Stdout: tc.output}}, errors: map[string]error{}}
			key, err := testClient(runner).SelectedKey(context.Background(), tc.selected)
			if err != nil || len(runner.calls) != 1 {
				t.Fatalf("key=%#v err=%v calls=%#v", key, err, runner.calls)
			}
			if tc.wantName == "" {
				if key != nil {
					t.Fatalf("unexpected key=%#v", key)
				}
			} else if key == nil || key.Name != tc.wantName {
				t.Fatalf("key=%#v want name=%q", key, tc.wantName)
			}
		})
	}
}

func TestClientStopCommandConstruction(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}, errors: map[string]error{}}
	c := testClient(runner)
	if err := c.Stop(context.Background(), "crabbox-blue"); err != nil {
		t.Fatal(err)
	}
	want := []string{"stop", "crabbox-blue"}
	if len(runner.calls) != 1 || runner.calls[0].Name != "/opt/bin/machine0" || !reflect.DeepEqual(runner.calls[0].Args, want) {
		t.Fatalf("call=%#v want args=%#v", runner.calls, want)
	}
}

func TestClientReadRetriesUnavailableThenSucceeds(t *testing.T) {
	for _, tc := range []struct {
		name         string
		message      string
		pollInterval time.Duration
	}{
		{name: "rate limited canonical default", message: "Rate limited. Please wait a moment and try again.", pollInterval: core.BaseConfig().Machine0.PollInterval},
		{name: "rate limited explicit override", message: "Rate limited. Please wait a moment and try again.", pollInterval: 3 * time.Second},
		{name: "cloud unavailable canonical default", message: "The cloud provider is temporarily unavailable. Please try again shortly.", pollInterval: core.BaseConfig().Machine0.PollInterval},
		{name: "cloud unavailable explicit override", message: "The cloud provider is temporarily unavailable. Please try again shortly.", pollInterval: 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unavailable := runnerResponse{
				result: core.LocalCommandResult{Stderr: "Warning: using cached credentials\n" + tc.message},
				err:    errors.New("exit status 1"),
			}
			runner := &recordingRunner{sequence: []runnerResponse{
				unavailable,
				unavailable,
				{result: core.LocalCommandResult{Stdout: `{"id":"vm-1","name":"box","status":"RUNNING","ip":"203.0.113.10"}`}},
			}}
			var stderr bytes.Buffer
			c := &client{cfg: Machine0Config{CLIPath: "/opt/bin/machine0", PollInterval: tc.pollInterval}, rt: Runtime{Exec: runner, Stdout: io.Discard, Stderr: &stderr}}
			var sleeps []time.Duration
			c.sleep = func(_ context.Context, delay time.Duration) error {
				sleeps = append(sleeps, delay)
				return nil
			}

			item, err := c.Get(context.Background(), "box")
			if err != nil {
				t.Fatal(err)
			}
			if item.ID != "vm-1" || len(runner.calls) != 3 {
				t.Fatalf("item=%#v calls=%d", item, len(runner.calls))
			}
			if len(sleeps) != 2 || sleeps[0] != tc.pollInterval || sleeps[1] != tc.pollInterval {
				t.Fatalf("sleeps=%v want=%s", sleeps, tc.pollInterval)
			}
			if strings.Count(stderr.String(), "machine0 read unavailable; retrying every") != 1 || strings.Contains(stderr.String(), "cached credentials") {
				t.Fatalf("stderr=%q", stderr.String())
			}
		})
	}
}

func TestClientReadUnavailableCancellationReturnsContextCause(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
	}{
		{name: "rate limited", message: "Rate limited. Please wait a moment and try again."},
		{name: "cloud unavailable", message: "The cloud provider is temporarily unavailable. Please try again shortly."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{sequence: []runnerResponse{{
				result: core.LocalCommandResult{Stderr: tc.message},
				err:    errors.New("exit status 1"),
			}}}
			c := testClient(runner)
			c.cfg.PollInterval = 5 * time.Second
			ctx, cancel := context.WithCancelCause(context.Background())
			wantErr := errors.New("operator canceled unavailable read")
			c.sleep = func(sleepCtx context.Context, delay time.Duration) error {
				if delay != 5*time.Second {
					t.Fatalf("delay=%s", delay)
				}
				cancel(wantErr)
				return context.Cause(sleepCtx)
			}

			_, err := c.Get(ctx, "box")
			if !errors.Is(err, wantErr) || len(runner.calls) != 1 {
				t.Fatalf("err=%v calls=%d", err, len(runner.calls))
			}
		})
	}
}

func TestClientReadDoesNotRetryUnrelatedFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
	}{
		{name: "proxy rate limit", message: "request was rate limited by an unrelated proxy"},
		{name: "proxy unavailable", message: "proxy temporarily unavailable. Please try again shortly."},
		{name: "incomplete cloud failure", message: "The cloud provider is temporarily unavailable."},
		{name: "other upstream failure", message: "unexpected upstream HTTP 500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{sequence: []runnerResponse{{
				result: core.LocalCommandResult{Stderr: tc.message},
				err:    errors.New("exit status 1"),
			}}}
			c := testClient(runner)
			c.sleep = func(context.Context, time.Duration) error {
				t.Fatal("unrelated failure must not sleep or retry")
				return nil
			}

			_, err := c.Get(context.Background(), "box")
			if err == nil || len(runner.calls) != 1 || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("err=%v calls=%d", err, len(runner.calls))
			}
		})
	}
}

func TestClientMutationDoesNotRetryUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
	}{
		{name: "rate limited", message: "Rate limited. Please wait a moment and try again."},
		{name: "cloud unavailable", message: "The cloud provider is temporarily unavailable. Please try again shortly."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{sequence: []runnerResponse{{
				result: core.LocalCommandResult{Stderr: tc.message},
				err:    errors.New("exit status 1"),
			}}}
			c := testClient(runner)
			c.sleep = func(context.Context, time.Duration) error {
				t.Fatal("mutation must not sleep or retry")
				return nil
			}

			err := c.Start(context.Background(), "box")
			if err == nil || len(runner.calls) != 1 || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("err=%v calls=%d", err, len(runner.calls))
			}
		})
	}
}

func TestMachine0ReadRetryDelayFallbackAndMinimum(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "fallback", in: 0, want: 5 * time.Second},
		{name: "minimum", in: time.Millisecond, want: time.Second},
		{name: "configured", in: 7 * time.Second, want: 7 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := machine0ReadRetryDelay(tc.in); got != tc.want {
				t.Fatalf("delay=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestClientParsesCompleteSizeCatalogAndExactCost(t *testing.T) {
	json := `[{"size":"gpu-h100-1","vcpu":20,"ramGb":240,"diskGb":720,"gpu":{"label":"1x H100","vramGb":80,"scratchDiskGb":5000},"regions":["us-east","eu"],"pricePerHourMicro":4851000,"transferGibPerMonth":9313,"estimatedSnapshotGb":200,"defaultImage":"gpu-h100x1-base","futureField":"preserved"}]`
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{"sizes\x00--all\x00--json": {Stdout: json}}, errors: map[string]error{}}
	sizes, err := testClient(runner).Sizes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 1 {
		t.Fatalf("sizes=%#v", sizes)
	}
	got := sizes[0]
	if got.Size != "gpu-h100-1" || got.VCPU != 20 || got.RAMGB != 240 || got.DiskGB != 720 || got.PricePerHourMicro != 4_851_000 || got.TransferGiBPerMonth != 9313 || got.EstimatedSnapshotGB != 200 || got.DefaultImage != "gpu-h100x1-base" {
		t.Fatalf("size=%#v", got)
	}
	if got.GPU == nil || got.GPU.Label != "1x H100" || got.GPU.VRAMGB != 80 || got.GPU.ScratchDiskGB != 5000 {
		t.Fatalf("gpu=%#v", got.GPU)
	}
	if got.ProviderMetadata["futureField"] != "preserved" {
		t.Fatalf("provider metadata=%#v", got.ProviderMetadata)
	}
}

func TestClientRejectsMalformedMachineSchemaAndTrailingJSON(t *testing.T) {
	for name, output := range map[string]string{
		"missing status": `{"id":"vm-1","name":"box"}`,
		"trailing value": `{"id":"vm-1","name":"box","status":"RUNNING"} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{"get\x00box\x00--json": {Stdout: output}}, errors: map[string]error{}}
			if _, err := testClient(runner).Get(context.Background(), "box"); err == nil {
				t.Fatal("expected schema error")
			}
		})
	}
}

func TestClientImageCommandsAndVersionParsing(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		"images\x00get\x00nixos-25-11-loaded\x00--json": {Stdout: `{"image":{"id":"img-1","name":"nixos-25-11-loaded","status":"READY"},"versions":[{"id":"iv-2","version":2,"status":"DRAFT","displayStatus":"DRAFT","snapshotStatus":"READY","pricePerHour":797.04,"totalCost":null,"versionSnapshotStorageCost":12.345}]}`},
	}, errors: map[string]error{}}
	c := testClient(runner)
	detail, err := c.GetImage(context.Background(), "nixos-25-11-loaded")
	if err != nil || len(detail.Versions) != 1 || detail.Versions[0].Version != 2 {
		t.Fatalf("detail=%#v err=%v", detail, err)
	}
	version := detail.Versions[0]
	if version.SnapshotStatus != "READY" || version.PricePerHour == nil || version.PricePerHour.String() != "797.04" || version.TotalCost != nil || version.VersionSnapshotStorageCost == nil || version.VersionSnapshotStorageCost.String() != "12.345" {
		t.Fatalf("fractional/null image version fields were not preserved: %#v", version)
	}
	costs := machine0ImageCostMetadata(version)
	if costs["machine0_price_per_hour"] != "797.04" || costs["machine0_version_snapshot_storage_cost"] != "12.345" {
		t.Fatalf("fractional cost metadata=%#v", costs)
	}
	if _, ok := costs["machine0_total_cost"]; ok {
		t.Fatalf("null total cost should stay absent: %#v", costs)
	}
	if err := c.SaveImage(context.Background(), "vm-a", "baseline", map[string]string{"crabbox_checkpoint": "chk_1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveImage(context.Background(), "baseline"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveImageVersion(context.Background(), "baseline", 2); err != nil {
		t.Fatal(err)
	}
	wants := [][]string{{"images", "save", "vm-a", "baseline", "--metadata", `{"crabbox_checkpoint":"chk_1"}`}, {"images", "rm", "baseline", "--yes"}, {"images", "versions", "rm", "baseline", "2", "--yes"}}
	for _, want := range wants {
		found := false
		for _, call := range runner.calls {
			if reflect.DeepEqual(call.Args, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing command %#v in %#v", want, runner.calls)
		}
	}
}

func TestClientActionableInstallAndAuthErrors(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{"--version": {}}, errors: map[string]error{"--version": exec.ErrNotFound}}
	if _, err := testClient(runner).Version(context.Background()); err == nil || !strings.Contains(err.Error(), "npm install -g @machine0/cli") {
		t.Fatalf("install err=%v", err)
	}
	runner = &recordingRunner{responses: map[string]core.LocalCommandResult{"ls\x00--json": {Stderr: "Not logged in."}}, errors: map[string]error{"ls\x00--json": errors.New("exit status 1")}}
	if _, err := testClient(runner).List(context.Background()); err == nil || !strings.Contains(err.Error(), "machine0 login") || !strings.Contains(err.Error(), "MACHINE0_API_TOKEN") {
		t.Fatalf("auth err=%v", err)
	}
}

func TestClientCommandFailurePreservesDeadlineAndSignalCause(t *testing.T) {
	for _, tc := range []struct {
		name      string
		context   func() (context.Context, context.CancelFunc)
		err       error
		wantCause string
	}{
		{
			name: "deadline with printed version",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				return ctx, cancel
			},
			err:       errors.New("signal: killed"),
			wantCause: "context deadline exceeded",
		},
		{
			name: "cancellation with printed version",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			err:       errors.New("signal: killed"),
			wantCause: "context canceled",
		},
		{
			name: "signal with printed version",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			err:       errors.New("signal: killed"),
			wantCause: "signal: killed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.context()
			defer cancel()
			runner := &recordingRunner{sequence: []runnerResponse{{
				result: core.LocalCommandResult{ExitCode: -1, Stdout: "1.0.164\n"},
				err:    tc.err,
			}}}
			_, err := testClient(runner).Version(ctx)
			if err == nil || !strings.Contains(err.Error(), tc.wantCause) || !strings.Contains(err.Error(), "partial output: 1.0.164") {
				t.Fatalf("err=%v, want cause %q and partial version output", err, tc.wantCause)
			}
		})
	}
}
