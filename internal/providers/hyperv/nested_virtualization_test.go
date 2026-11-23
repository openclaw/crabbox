package hyperv

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

const (
	enableNesting = "Set-VMProcessor -VMName "
	verifyNesting = "Get-VMProcessor -VMName "
)

func TestCreateVMNestedVirtualization(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        []string
		wantNesting bool
	}{
		{name: "omitted"},
		{name: "disabled", args: []string{"--hyperv-nested-virtualization=false"}},
		{name: "enabled", args: []string{"--hyperv-nested-virtualization"}, wantNesting: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			vmName := core.PowerShellQuote("crabbox-nested-test")
			readback := verifyNesting + vmName + " -ErrorAction Stop | Select-Object -ExpandProperty ExposeVirtualizationExtensions -ErrorAction Stop | ConvertTo-Json -Compress"
			runner := &recordingRunner{respond: func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				if commandScript(req) == readback {
					return core.LocalCommandResult{Stdout: "true\r\n"}, nil, true
				}
				return core.LocalCommandResult{}, nil, false
			}}
			b := testBackend(runner)
			cfg := b.configForRun()
			// Start with a prior opt-in to exercise an explicit false override.
			cfg.HyperV.NestedVirtualization = tc.name == "disabled"
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			values := registerFlags(fs, cfg)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			if err := applyFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if err := b.createVM(t.Context(), cfg, "crabbox-nested-test"); err != nil {
				t.Fatal(err)
			}
			want := []string{"if (-not (Get-VMSwitch", "New-VHD ", "New-VM -Name " + vmName, "Set-VM -Name " + vmName + " -ProcessorCount "}
			if tc.wantNesting {
				want = append(want,
					enableNesting+vmName+" -ExposeVirtualizationExtensions $true -ErrorAction Stop",
					readback)
			}
			want = append(want, "Start-VM -Name "+vmName)
			if len(runner.calls) != len(want) {
				t.Fatalf("createVM made %d calls, want %d: %v", len(runner.calls), len(want), runner.calls)
			}
			for i, prefix := range want {
				if script := commandScript(runner.calls[i]); !strings.HasPrefix(script, prefix) {
					t.Errorf("command %d = %q, want prefix %q", i, script, prefix)
				}
			}
		})
	}
}

func TestAcquireNestedVirtualizationFailureRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name, output, wantError string
		setFails, readFails     bool
	}{
		{name: "set fails", setFails: true, wantError: "enable Hyper-V nested virtualization (Set-VMProcessor)"},
		{name: "read fails", readFails: true, wantError: "verify Hyper-V nested virtualization (Get-VMProcessor)"},
		{name: "read returns false", output: "false", wantError: "ExposeVirtualizationExtensions was not confirmed true"},
		{name: "read returns nothing", wantError: "ExposeVirtualizationExtensions was not confirmed true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			oldOS := hypervHostOS
			hypervHostOS = "windows"
			t.Cleanup(func() { hypervHostOS = oldOS })

			const otherVM = "crabbox-unrelated"
			otherDisk := filepath.Join(hypervVHDDir(), otherVM+".vhdx")
			template := filepath.Join(t.TempDir(), "template.vhdx")
			if err := os.MkdirAll(hypervVHDDir(), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{template, otherDisk} {
				if err := os.WriteFile(path, []byte("unrelated"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var claim core.LeaseClaim
			var name, disk string
			var lifecycle []string
			runner := &recordingRunner{respond: func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				script := commandScript(req)
				switch {
				case strings.Contains(script, "$_.Name -like 'crabbox-*'"):
					return jsonResult(t, hypervVM{Name: otherVM, State: 3}), nil, true
				case strings.HasPrefix(script, "New-VHD "):
					claims, err := core.ListLeaseClaims()
					if err != nil || len(claims) != 1 {
						t.Fatalf("expected one provisional claim: %v, %v", claims, err)
					}
					claim = claims[0]
					name = instanceNameFromClaim(claim)
					disk = filepath.Join(hypervVHDDir(), name+".vhdx")
					if name == "" || !strings.Contains(script, "-Path "+core.PowerShellQuote(disk)+" -ParentPath "+core.PowerShellQuote(template)) {
						t.Fatalf("unexpected differencing disk command: %s", script)
					}
					if err := os.WriteFile(disk, []byte("child"), 0o600); err != nil {
						t.Fatal(err)
					}
				case strings.HasPrefix(script, enableNesting):
					lifecycle = append(lifecycle, "set")
					if tc.setFails {
						return core.LocalCommandResult{}, errors.New("simulated processor configuration failure"), true
					}
				case strings.HasPrefix(script, verifyNesting):
					lifecycle = append(lifecycle, "verify")
					if tc.readFails {
						return core.LocalCommandResult{}, errors.New("simulated processor read failure"), true
					}
					return core.LocalCommandResult{Stdout: tc.output}, nil, true
				case strings.HasPrefix(script, "Start-VM "):
					t.Fatalf("started a VM after nesting failure: %s", script)
				case strings.Contains(script, "Select-Object Name, State"):
					if !strings.Contains(script, "$_.Name -eq "+core.PowerShellQuote(name)) {
						t.Fatalf("cleanup queried a different VM: %s", script)
					}
					return jsonResult(t, hypervVM{Name: name, State: 3}), nil, true
				case strings.HasPrefix(script, "Stop-VM "), strings.HasPrefix(script, "Remove-VM "):
					verb, _, _ := strings.Cut(script, " ")
					if !strings.HasPrefix(script, verb+" -Name "+core.PowerShellQuote(name)+" ") {
						t.Fatalf("cleanup targeted a different VM: %s", script)
					}
					lifecycle = append(lifecycle, verb)
				}
				return core.LocalCommandResult{}, nil, false
			}}
			b := testBackend(runner)
			b.cfg.HyperV.Image = template
			b.cfg.HyperV.NestedVirtualization = true
			_, err := b.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "nesting-failure", Keep: true})
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("Acquire error = %v, want %q", err, tc.wantError)
			}
			want := []string{"set", "verify", "Stop-VM", "Remove-VM"}
			if tc.setFails {
				want = []string{"set", "Stop-VM", "Remove-VM"}
			}
			if !reflect.DeepEqual(lifecycle, want) {
				t.Fatalf("nesting and rollback commands = %v, want %v", lifecycle, want)
			}
			key, err := core.TestboxKeyPath(claim.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{disk, key} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("rollback left %s: %v", path, err)
				}
			}
			for _, path := range []string{template, otherDisk} {
				if data, err := os.ReadFile(path); err != nil || string(data) != "unrelated" {
					t.Errorf("rollback modified unrelated disk %s: %v", path, err)
				}
			}
			if claims, err := core.ListLeaseClaims(); err != nil || len(claims) != 0 {
				t.Fatalf("rollback left claims: %v, %v", claims, err)
			}
		})
	}
}
