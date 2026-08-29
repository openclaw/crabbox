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
	const summary = `[{"name":"other","type":"MANAGED"},{"name":"default-key","type":"PUBLIC","isDefault":true}]`
	const detail = `{"name":"default-key","type":"PUBLIC","fileName":"id_ed25519","isDefault":true}`
	list := []string{"keys", "ls", "--json"}
	getDefault := []string{"keys", "get", "default-key", "--json"}
	getExplicit := []string{"keys", "get", "remote-key", "--json"}
	for _, tc := range []struct {
		name        string
		selected    string
		listOutput  string
		listError   error
		detail      string
		detailError error
		want        *machineKey
		wantError   string
		commands    [][]string
	}{
		{name: "explicit key trims lookup and validates trimmed detail name", selected: " remote-key\n", detail: `{"name":" remote-key ","type":"PUBLIC","fileName":"id_ed25519"}`, want: &machineKey{Name: " remote-key ", Type: "PUBLIC", FileName: "id_ed25519"}, commands: [][]string{getExplicit}},
		{name: "account default reads full metadata", listOutput: summary, detail: detail, want: &machineKey{Name: "default-key", Type: "PUBLIC", FileName: "id_ed25519", IsDefault: true}, commands: [][]string{list, getDefault}},
		{name: "default name is trimmed", selected: " \n", listOutput: `[{"name":" default-key ","type":"PUBLIC","isDefault":true}]`, detail: detail, want: &machineKey{Name: "default-key", Type: "PUBLIC", FileName: "id_ed25519", IsDefault: true}, commands: [][]string{list, getDefault}},
		{name: "no keys preserves CLI behavior", listOutput: `[]`, commands: [][]string{list}},
		{name: "no default does not choose another key", listOutput: `[{"name":"other","type":"MANAGED"}]`, commands: [][]string{list}},
		{name: "unnamed default fails without another selection", listOutput: `[{"name":" \t","isDefault":true},{"name":"other","isDefault":true}]`, wantError: "default SSH key has no name", commands: [][]string{list}},
		{name: "default detail name mismatch", listOutput: summary, detail: `{"name":"other","fileName":"id_ed25519"}`, wantError: "mismatched key name", commands: [][]string{list, getDefault}},
		{name: "explicit detail name mismatch", selected: "remote-key", detail: detail, wantError: "mismatched key name", commands: [][]string{getExplicit}},
		{name: "default detail omits name", listOutput: summary, detail: `{"fileName":"id_ed25519"}`, wantError: "mismatched key name", commands: [][]string{list, getDefault}},
		{name: "default detail parse failure", listOutput: summary, detail: `{`, wantError: "parse machine0 keys get", commands: [][]string{list, getDefault}},
		{name: "default detail command failure", listOutput: summary, detailError: errors.New("key detail unavailable"), wantError: "key detail unavailable", commands: [][]string{list, getDefault}},
		{name: "list command failure", listError: errors.New("key list unavailable"), wantError: "key list unavailable", commands: [][]string{list}},
		{name: "list parse failure", listOutput: `{`, wantError: "parse machine0 keys ls", commands: [][]string{list}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
				strings.Join(list, "\x00"):        {Stdout: tc.listOutput},
				strings.Join(getDefault, "\x00"):  {Stdout: tc.detail},
				strings.Join(getExplicit, "\x00"): {Stdout: tc.detail},
			}, errors: map[string]error{
				strings.Join(list, "\x00"):        tc.listError,
				strings.Join(getDefault, "\x00"):  tc.detailError,
				strings.Join(getExplicit, "\x00"): tc.detailError,
			}}
			key, err := testClient(runner).SelectedKey(context.Background(), tc.selected)
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("err=%v want=%q", err, tc.wantError)
			}
			if !reflect.DeepEqual(key, tc.want) {
				t.Fatalf("key=%#v want=%#v", key, tc.want)
			}
			var commands [][]string
			for _, call := range runner.calls {
				commands = append(commands, call.Args)
			}
			if !reflect.DeepEqual(commands, tc.commands) {
				t.Fatalf("commands=%q want=%q", commands, tc.commands)
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

func TestClientGetUUIDUsesVerifiedFullDetail(t *testing.T) {
	const id = "abcdef12-3456-7890-abcd-ef1234567890"
	for _, tc := range []struct {
		name        string
		lookup      string
		inventoryID string
		detailID    string
	}{
		{name: "lowercase", lookup: id, inventoryID: id, detailID: id},
		{name: "uppercase input", lookup: strings.ToUpper(id), inventoryID: id, detailID: id},
		{name: "uppercase inventory", lookup: id, inventoryID: strings.ToUpper(id), detailID: id},
		{name: "uppercase detail", lookup: id, inventoryID: id, detailID: strings.ToUpper(id)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
				"ls\x00--json":                  {Stdout: `[{"id":"11111111-1111-1111-1111-111111111111","name":"other","status":"RUNNING"},{"id":"` + tc.inventoryID + `","name":"current-name","status":"STARTING","ip":"203.0.113.10"}]`},
				"get\x00current-name\x00--json": {Stdout: `{"id":"` + tc.detailID + `","name":"current-name","status":"RUNNING","ip":"203.0.113.99","defaultSSHUsername":"nix","distribution":"nixos","image":"nixos-loaded","imageVersion":2,"key":{"name":"ci-key","type":"MANAGED","fileName":"machine0__ci-key"}}`},
			}, errors: map[string]error{
				"get\x00" + tc.lookup + "\x00--json": errors.New("No such procedure"),
			}}
			item, err := testClient(runner).Get(context.Background(), tc.lookup)
			if err != nil {
				t.Fatal(err)
			}
			want := machine{ID: tc.detailID, Name: "current-name", Status: "RUNNING", IP: "203.0.113.99", DefaultSSHUsername: "nix", Distribution: "nixos", Image: "nixos-loaded", ImageVersion: 2, Key: &machineKey{Name: "ci-key", Type: "MANAGED", FileName: "machine0__ci-key"}}
			if !reflect.DeepEqual(item, want) {
				t.Fatalf("detail=%#v want=%#v", item, want)
			}
			if len(runner.calls) != 2 || !reflect.DeepEqual(runner.calls[0].Args, []string{"ls", "--json"}) || !reflect.DeepEqual(runner.calls[1].Args, []string{"get", "current-name", "--json"}) {
				t.Fatalf("UUID lookup must list then read full detail by current name: %#v", runner.calls)
			}
		})
	}
}

func TestClientGetNonUUIDUsesDirectDetail(t *testing.T) {
	for _, name := range []string{
		"my-app", "cbx_abcdef123456", "abcdef1234567890abcdef1234567890ab",
		"{abcdef12-3456-7890-abcd-ef1234567890}", "urn:uuid:abcdef12-3456-7890-abcd-ef1234567890",
		"abcdef12-3456-7890-abcd-ef123456789z", "abcdef12-3456-7890-abcd-ef1234567890-extra",
	} {
		t.Run(name, func(t *testing.T) {
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
				"get\x00" + name + "\x00--json": {Stdout: `{"id":"vm-1","name":"my-app","status":"RUNNING","defaultSSHUsername":"nix"}`},
			}}
			item, err := testClient(runner).Get(context.Background(), name)
			if err != nil || item.ID != "vm-1" || item.DefaultSSHUsername != "nix" {
				t.Fatalf("item=%#v err=%v", item, err)
			}
			if len(runner.calls) != 1 || !reflect.DeepEqual(runner.calls[0].Args, []string{"get", name, "--json"}) {
				t.Fatalf("non-UUID lookup must remain a single direct detail read: %#v", runner.calls)
			}
		})
	}
}

func TestClientGetUUIDRejectsUnverifiedLookup(t *testing.T) {
	const id = "abcdef12-3456-7890-abcd-ef1234567890"
	const row = `{"id":"` + id + `","name":"my-app","status":"RUNNING"}`
	const other = `{"id":"11111111-1111-1111-1111-111111111111","name":"other","status":"RUNNING"}`
	for _, tc := range []struct {
		name      string
		inventory string
		listError string
		detail    string
		getError  string
		wantError string
		wantCode  int
		wantGet   bool
	}{
		{name: "empty inventory", inventory: `[]`, wantError: "absent from current authorized inventory", wantCode: 4},
		{name: "no exact UUID", inventory: `[` + other + `]`, wantError: "absent from current authorized inventory", wantCode: 4},
		{name: "duplicate exact UUID", inventory: `[` + row + `,` + row + `]`, wantError: "multiple", wantCode: 5},
		{name: "duplicate UUID differs only in case", inventory: `[` + row + `,` + strings.Replace(row, id, strings.ToUpper(id), 1) + `]`, wantError: "multiple", wantCode: 5},
		{name: "missing ID cannot prove absence", inventory: `[{"name":"other","status":"RUNNING"}]`, wantError: "missing or malformed UUID", wantCode: 5},
		{name: "missing ID after match", inventory: `[` + row + `,{"name":"other","status":"RUNNING"}]`, wantError: "missing or malformed UUID", wantCode: 5},
		{name: "malformed ID", inventory: `[` + strings.Replace(other, "11111111-1111-1111-1111-111111111111", "not-a-uuid", 1) + `]`, wantError: "missing or malformed UUID", wantCode: 5},
		{name: "missing name", inventory: `[{"id":"` + id + `","status":"RUNNING"}]`, wantError: "missing name", wantCode: 5},
		{name: "missing status", inventory: `[{"id":"` + id + `","name":"my-app"}]`, wantError: "missing status", wantCode: 5},
		{name: "UUID cannot be a transport name", inventory: `[` + strings.Replace(row, "my-app", id, 1) + `]`, wantError: "invalid machine name", wantCode: 5},
		{name: "null inventory", inventory: `null`, wantError: "expected an array", wantCode: 5},
		{name: "wrong inventory shape", inventory: `{"machines":[]}`, wantError: "parse machine0 ls", wantCode: 5},
		{name: "null inventory row", inventory: `[null]`, wantError: "missing name", wantCode: 5},
		{name: "invalid inventory JSON", inventory: `not-json`, wantError: "parse machine0 ls", wantCode: 5},
		{name: "truncated inventory", inventory: `[` + row, wantError: "parse machine0 ls", wantCode: 5},
		{name: "trailing inventory JSON", inventory: `[] {}`, wantError: "trailing JSON", wantCode: 5},
		{name: "inventory authentication failure with empty output", inventory: `[]`, listError: "Not logged in.", wantError: "authentication is required", wantCode: 3},
		{name: "inventory read failure with matching output", inventory: `[` + row + `]`, listError: "upstream read failed", wantError: "upstream read failed", wantCode: 5},
		{name: "name reused for another UUID", inventory: `[` + row + `]`, detail: strings.Replace(other, "other", "my-app", 1), wantError: "identity changed", wantCode: 5, wantGet: true},
		{name: "detail name changed", inventory: `[` + row + `]`, detail: strings.Replace(row, "my-app", "renamed", 1), wantError: "identity changed", wantCode: 5, wantGet: true},
		{name: "detail missing ID", inventory: `[` + row + `]`, detail: `{"name":"my-app","status":"RUNNING"}`, wantError: "missing id", wantCode: 5, wantGet: true},
		{name: "invalid detail JSON", inventory: `[` + row + `]`, detail: `not-json`, wantError: "parse machine0 get", wantCode: 5, wantGet: true},
		{name: "full detail failure", inventory: `[` + row + `]`, getError: "No such procedure", wantError: "No such procedure", wantCode: 5, wantGet: true},
		{name: "detail disappears after inventory", inventory: `[` + row + `]`, getError: "VM not found", wantError: "VM not found", wantCode: 5, wantGet: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
				"ls\x00--json":            {Stdout: tc.inventory, Stderr: tc.listError},
				"get\x00my-app\x00--json": {Stdout: tc.detail, Stderr: tc.getError},
			}, errors: map[string]error{}}
			if tc.listError != "" {
				runner.errors["ls\x00--json"] = errors.New("exit status 1")
			}
			if tc.getError != "" {
				runner.errors["get\x00my-app\x00--json"] = errors.New("exit status 1")
			}
			item, err := testClient(runner).Get(context.Background(), id)
			assertMachine0Exit(t, err, tc.wantCode, tc.wantError)
			if !reflect.DeepEqual(item, machine{}) {
				t.Fatalf("unverified lookup returned a machine: %#v", item)
			}
			if tc.wantCode != 4 && strings.Contains(err.Error(), "absent from current authorized inventory") {
				t.Fatalf("uncertain lookup reported absence: %v", err)
			}
			wantCalls := [][]string{{"ls", "--json"}}
			if tc.wantGet {
				wantCalls = append(wantCalls, []string{"get", "my-app", "--json"})
			}
			var calls [][]string
			for _, call := range runner.calls {
				calls = append(calls, call.Args)
			}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("calls=%#v want=%#v", calls, wantCalls)
			}
		})
	}
}

func TestClientReadRetriesUnavailableThenSucceeds(t *testing.T) {
	for _, tc := range []struct {
		name         string
		message      string
		pollInterval time.Duration
		uuidStage    string
	}{
		{name: "rate limited canonical default", message: "Rate limited. Please wait a moment and try again.", pollInterval: core.BaseConfig().Machine0.PollInterval},
		{name: "rate limited explicit override", message: "Rate limited. Please wait a moment and try again.", pollInterval: 3 * time.Second},
		{name: "cloud unavailable canonical default", message: "The cloud provider is temporarily unavailable. Please try again shortly.", pollInterval: core.BaseConfig().Machine0.PollInterval},
		{name: "cloud unavailable explicit override", message: "The cloud provider is temporarily unavailable. Please try again shortly.", pollInterval: 3 * time.Second},
		{name: "UUID inventory rate limited", message: "Rate limited. Please wait a moment and try again.", pollInterval: 3 * time.Second, uuidStage: "inventory"},
		{name: "UUID detail unavailable", message: "The cloud provider is temporarily unavailable. Please try again shortly.", pollInterval: 3 * time.Second, uuidStage: "detail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unavailable := runnerResponse{
				result: core.LocalCommandResult{Stderr: "Warning: using cached credentials\n" + tc.message},
				err:    errors.New("exit status 1"),
			}
			lookup, id := "box", "vm-1"
			if tc.uuidStage != "" {
				id = "abcdef12-3456-7890-abcd-ef1234567890"
				lookup = id
			}
			detail := `{"id":"` + id + `","name":"box","status":"RUNNING","ip":"203.0.113.10"}`
			inventory := runnerResponse{result: core.LocalCommandResult{Stdout: `[{"id":"` + id + `","name":"box","status":"RUNNING"}]`}}
			sequence := []runnerResponse{unavailable, unavailable}
			wantCalls := [][]string{{"get", "box", "--json"}, {"get", "box", "--json"}, {"get", "box", "--json"}}
			switch tc.uuidStage {
			case "inventory":
				sequence = append(sequence, inventory)
				wantCalls = [][]string{{"ls", "--json"}, {"ls", "--json"}, {"ls", "--json"}, {"get", "box", "--json"}}
			case "detail":
				sequence = append([]runnerResponse{inventory}, sequence...)
				wantCalls = append([][]string{{"ls", "--json"}}, wantCalls...)
			}
			sequence = append(sequence, runnerResponse{result: core.LocalCommandResult{Stdout: detail}})
			runner := &recordingRunner{sequence: sequence}
			var stderr bytes.Buffer
			c := &client{cfg: Machine0Config{CLIPath: "/opt/bin/machine0", PollInterval: tc.pollInterval}, rt: Runtime{Exec: runner, Stdout: io.Discard, Stderr: &stderr}}
			var sleeps []time.Duration
			c.sleep = func(_ context.Context, delay time.Duration) error {
				sleeps = append(sleeps, delay)
				return nil
			}

			item, err := c.Get(context.Background(), lookup)
			if err != nil {
				t.Fatal(err)
			}
			var calls [][]string
			for _, call := range runner.calls {
				calls = append(calls, call.Args)
			}
			if item.ID != id || !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("item=%#v calls=%#v want=%#v", item, calls, wantCalls)
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
