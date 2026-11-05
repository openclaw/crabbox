//go:build windows

package hyperv

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

// hyperVStubs replaces the Hyper-V cmdlets the fork scripts call with functions
// over a fake registration, so the generated scripts run in real Windows
// PowerShell against real files. Each script runs in its own process, so removal
// of the registration persists as a flag file; Get-VHD reports the parent of
// each disk from $global:parents.
const hyperVStubs = `
function Import-VM { [CmdletBinding()] param($Path, [switch]$Copy, [switch]$GenerateNewId, $VirtualMachinePath, $SnapshotFilePath, $VhdDestinationPath) [pscustomobject]@{ Id=[guid]'99999999-8888-7777-6666-555555555555'; Name='imported' } }
function Rename-VM { [CmdletBinding()] param($VM, $NewName) }
function Get-VM { [CmdletBinding()] param($Id, $Name) if (Test-Path -LiteralPath $global:removedFlag) { return }; [pscustomobject]@{ Id=[guid]'99999999-8888-7777-6666-555555555555'; Name=$global:vmName; ConfigurationLocation=$global:vmRoot; SnapshotFileLocation=$global:vmRoot } }
function Get-VMNetworkAdapter { [CmdletBinding()] param($VM) }
function Set-VMNetworkAdapter { [CmdletBinding()] param([Parameter(ValueFromPipeline)]$InputObject, [switch]$DynamicMacAddress) }
function Disconnect-VMNetworkAdapter { [CmdletBinding()] param([Parameter(ValueFromPipeline)]$InputObject) }
function Set-VM { [CmdletBinding()] param($VM, $AutomaticCheckpointsEnabled, $CheckpointType) if ($global:failSetVM) { throw 'Set-VM failed' } }
function Start-VM { [CmdletBinding()] param($VM) }
function Get-VMHardDiskDrive { [CmdletBinding()] param($VM) if (Test-Path -LiteralPath $global:removedFlag) { return }; [pscustomobject]@{ Path=$global:child } }
function Get-VHD { [CmdletBinding()] param($Path) [pscustomobject]@{ Path=$Path; ParentPath=$global:parents[$Path] } }
function Stop-VM { [CmdletBinding(SupportsShouldProcess)] param($VM, [switch]$Force) }
function Remove-VM { [CmdletBinding(SupportsShouldProcess)] param($VM, [switch]$Force) Set-Content -LiteralPath $global:removedFlag -Value removed }
`

// stubbedPowerShellRunner runs each host script in Windows PowerShell after the
// Hyper-V stubs and the given fixture variables.
type stubbedPowerShellRunner struct {
	t        *testing.T
	fixtures string
	calls    []string
}

func (r *stubbedPowerShellRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	script := commandScript(req)
	r.calls = append(r.calls, script)
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", hyperVStubs+r.fixtures+script)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return core.LocalCommandResult{Stdout: stdout.String(), Stderr: stderr.String()}, err
}

type forkDiskChainFixture struct {
	name, vmRoot, child, parent, template string
}

// newForkDiskChainFixture lays out what Import-VM -Copy leaves for a fork of a
// differencing source: the child and its copied parent inside the fork's VHD
// directory, whose parent is the template outside Crabbox fork storage.
func newForkDiskChainFixture(t *testing.T) forkDiskChainFixture {
	t.Helper()
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("powershell.exe unavailable")
	}
	setCheckpointTestState(t)
	f := forkDiskChainFixture{name: "crabbox-fork-chain-1a2b"}
	f.vmRoot = filepath.Join(hypervVMDir(), f.name)
	vhdRoot := filepath.Join(hypervVHDDir(), f.name)
	f.child = filepath.Join(vhdRoot, "source-lease.vhdx")
	f.parent = filepath.Join(vhdRoot, "windows-template.vhdx")
	f.template = filepath.Join(t.TempDir(), "windows-template.vhdx")
	for _, dir := range []string{f.vmRoot, vhdRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{f.child, f.parent, f.template} {
		if err := os.WriteFile(path, []byte("disk"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f forkDiskChainFixture) runner(t *testing.T, extra string) *stubbedPowerShellRunner {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	fixtures := "$global:vmName=" + quote(f.name) + "; $global:vmRoot=" + quote(f.vmRoot) + "; $global:child=" + quote(f.child) + "; $global:removedFlag=" + quote(filepath.Join(t.TempDir(), "vm.removed")) +
		"; $global:parents=@{ " + quote(f.child) + "=" + quote(f.parent) + "; " + quote(f.parent) + "=" + quote(f.template) + " }; " + extra + "\n"
	return &stubbedPowerShellRunner{t: t, fixtures: fixtures}
}

func assertGone(t *testing.T, label string, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s left %s behind: %v", label, path, err)
		}
	}
}

// Import-VM -Copy copies the source's whole differencing chain; releasing the
// fork must delete the copied parent too, never the template outside the fork.
func TestImportedForkReleaseDeletesTheCopiedDiskChain(t *testing.T) {
	f := newForkDiskChainFixture(t)
	runner := f.runner(t, "")
	b := testBackend(&recordingRunner{})
	b.rt.Exec = runner

	if _, err := b.importCheckpointVM(context.Background(), `C:\export\vm.vmcx`, f.name, false); err != nil {
		t.Fatalf("importCheckpointVM: %v", err)
	}
	var recorded []string
	data, err := os.ReadFile(filepath.Join(filepath.Dir(f.child), forkDisksManifest))
	if err != nil || json.Unmarshal(data, &recorded) != nil {
		t.Fatalf("fork disk manifest: %v %s", err, data)
	}
	if len(recorded) != 2 || !strings.EqualFold(recorded[0], filepath.Base(f.child)) || !strings.EqualFold(recorded[1], filepath.Base(f.parent)) {
		t.Fatalf("manifest=%v, want the copied child and parent", recorded)
	}
	if err := b.removeVMStorage(f.name, []string{f.child}); err != nil {
		t.Fatalf("removeVMStorage: %v", err)
	}
	assertGone(t, "fork release", f.child, f.parent, filepath.Dir(f.child))
	if _, err := os.Stat(f.template); err != nil {
		t.Fatalf("fork release removed the template outside fork storage: %v", err)
	}
}

// A fork rolled back after import, or an import that fails after Import-VM,
// must delete the copied parent with the child.
func TestImportedForkRollbackDeletesTheCopiedDiskChain(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, f forkDiskChainFixture, b *backend) error
		set  string
	}{
		{name: "rollback-after-import", run: func(t *testing.T, f forkDiskChainFixture, b *backend) error {
			return b.removeImportedCheckpointRegistration(context.Background(), f.name)
		}},
		{name: "import-fails-after-copy", set: "$global:failSetVM=$true;", run: func(t *testing.T, f forkDiskChainFixture, b *backend) error {
			if _, err := b.importCheckpointVM(context.Background(), `C:\export\vm.vmcx`, f.name, false); err == nil {
				t.Fatal("import succeeded although Set-VM failed")
			}
			return nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newForkDiskChainFixture(t)
			runner := f.runner(t, tc.set)
			b := testBackend(&recordingRunner{})
			b.rt.Exec = runner
			if err := tc.run(t, f, b); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			assertGone(t, tc.name, f.child, f.parent)
			if _, err := os.Stat(f.template); err != nil {
				t.Fatalf("%s removed the template outside fork storage: %v", tc.name, err)
			}
		})
	}
}
