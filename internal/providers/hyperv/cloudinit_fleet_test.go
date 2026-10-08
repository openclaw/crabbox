package hyperv

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"gopkg.in/yaml.v3"
)

func TestLinuxCloudInitKeyOnlyUserAndWorkRoot(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.TargetOS, cfg.SSHUser, cfg.WorkRoot = targetLinux, "alice", "/home/alice/space 'quoted'"
	var data struct {
		PasswordAuth bool `yaml:"ssh_pwauth"`
		DisableRoot  bool `yaml:"disable_root"`
		Users        []struct {
			Name string   `yaml:"name"`
			Keys []string `yaml:"ssh_authorized_keys"`
		} `yaml:"users"`
		RunCommands []string `yaml:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(linuxUserData(cfg, "fixture-public-key")), &data); err != nil {
		t.Fatal(err)
	}
	if data.PasswordAuth || !data.DisableRoot || len(data.Users) != 1 ||
		data.Users[0].Name != "alice" || len(data.Users[0].Keys) != 1 || data.Users[0].Keys[0] != "fixture-public-key" || len(data.RunCommands) == 0 {
		t.Fatal("cloud-init did not preserve the key-only guest contract")
	}
}

func TestLinuxVMCreationPowerShellBehavior(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell behavioral lane")
	}
	f := newLinuxFixture(t)
	const name = "crabbox-vm-script"
	if err := reserveLinuxStorage(name, "cbx_script"); err != nil {
		t.Fatal(err)
	}
	// The real flow writes the NoCloud seed before this script runs, and the script now repairs
	// that file's ACL before attaching it. A fixture without a seed is not the production shape.
	if err := os.MkdirAll(filepath.Dir(ownedCloudInitSeedPath(name)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownedCloudInitSeedPath(name), []byte("fake seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(t.TempDir(), "result.json")
	script := `Import-Module Microsoft.PowerShell.Management
	Import-Module Microsoft.PowerShell.Utility
	$PSModuleAutoloadingPreference='None'
	function Get-VM { param($Name,$ErrorAction) }
	function Get-VMSwitch { param($Name,$ErrorAction) [pscustomobject]@{Name=$Name} }
	function New-VHD {
	 param($Path,$ParentPath,[switch]$Differencing,$ErrorAction)
	 if (-not $Differencing -or -not [IO.File]::Exists($ParentPath)) { throw 'invalid parent contract' }
	 [IO.File]::WriteAllText($Path,'owned fake child')
	}
	function New-VM {
	 param($Name,$MemoryStartupBytes,$Generation,$VHDPath,$Path,$ErrorAction)
	 if ($Generation -ne 2) { throw 'not Generation 2' }
	 $script:VM=[pscustomobject]@{Name=$Name;State=3;Id='f72621ca-fb8c-4d75-a7af-18fac5046fda';CPU=0;Memory=[int64]$MemoryStartupBytes;DynamicMemory=$true;Checkpoints=$true;Firmware='';Seed='';Switch='';Storage=$Path}
	 $script:VM
	}
	function Set-VM {
	 param([Parameter(ValueFromPipeline=$true)]$InputObject,$ProcessorCount,$AutomaticCheckpointsEnabled)
	 process { $InputObject.CPU=$ProcessorCount; $InputObject.Checkpoints=$AutomaticCheckpointsEnabled }
	}
	function Set-VMMemory { param($VM,$DynamicMemoryEnabled,$ErrorAction) $VM.DynamicMemory=$DynamicMemoryEnabled }
	function Set-VMFirmware {
	 param($VM,$EnableSecureBoot,$SecureBootTemplate,$ErrorAction)
	 if ($EnableSecureBoot -ne 'On') { throw 'Secure Boot must remain on' }
	 $VM.Firmware=$SecureBootTemplate
	}
	function Add-VMHardDiskDrive { param($VM,$ControllerType,$ControllerNumber,$ControllerLocation,$Path,$ErrorAction) $VM.Seed=$Path }
	function Connect-VMNetworkAdapter { param($VMName,$SwitchName,$ErrorAction) $script:VM.Switch=$SwitchName }
	function Start-VM { throw 'VM started before durable identity publication' }
	` + linuxVMCreateScript(f.b.configForRun(), name) + `
	[IO.File]::WriteAllText($env:CBX_TEST_RECEIPT, ($script:VM | ConvertTo-Json -Compress))
	`
	scriptPath := filepath.Join(t.TempDir(), "vm-fixture.ps1")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath)
	cmd.Env = append(os.Environ(), "CBX_TEST_RECEIPT="+receipt)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated VM script: %v\n%s", err, output)
	}
	var vm hypervVM
	if err := json.Unmarshal(bytes.TrimSpace(output), &vm); err != nil || vm.Name != name || vm.State != 3 || !validHyperVGUID(vm.ID) {
		t.Fatalf("creation identity invalid: vm=%+v err=%v", vm, err)
	}
	data, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		CPU                        int
		Memory                     int64
		DynamicMemory, Checkpoints bool
		Firmware, Seed, Switch     string
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.CPU != 2 || state.Memory != 2048*1024*1024 || state.DynamicMemory || state.Checkpoints ||
		state.Firmware != "MicrosoftUEFICertificateAuthority" || state.Seed != ownedCloudInitSeedPath(name) ||
		state.Switch != "Default Switch" {
		t.Fatalf("wrong bounded Linux VM configuration: %+v", state)
	}
}
func TestNoCloudSeedPowerShellBehavior(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell behavioral lane")
	}
	executable, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatal("Windows PowerShell is required for the seed behavioral lane")
	}
	for _, failFormat := range []bool{false, true} {
		t.Run(fmtBool(failFormat), func(t *testing.T) {
			root := t.TempDir()
			volume := filepath.Join(root, "volume")
			if err := os.Mkdir(volume, 0o700); err != nil {
				t.Fatal(err)
			}
			userData, metaData := "#cloud-config\nhostname: fixture-\u2603\n", cloudInitMetaData("crabbox-fixture")
			userPath := filepath.Join(root, "user-input")
			metaPath := filepath.Join(root, "meta-input")
			for name, data := range map[string]string{userPath: userData, metaPath: metaData} {
				if err := os.WriteFile(name, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			seed := filepath.Join(root, "seed.vhdx")
			script := `Import-Module Microsoft.PowerShell.Management
Import-Module Microsoft.PowerShell.Utility
$PSModuleAutoloadingPreference = 'None'
$script:Mounted = $false
$script:Dismounted = $false
function New-VHD { param($Path,[switch]$Dynamic,$SizeBytes,$ErrorAction) [IO.File]::WriteAllText($Path,'fake disk') }
function Mount-VHD { param($Path,[switch]$Passthru,$ErrorAction) $script:Mounted=$true; [pscustomobject]@{Path=$Path} }
function Get-Disk { param([Parameter(ValueFromPipeline=$true)]$InputObject) process { [pscustomobject]@{Number=1;IsOffline=$false;IsReadOnly=$false} } }
function Initialize-Disk { param($Number,$PartitionStyle,$ErrorAction) }
function New-Partition { param($DiskNumber,[switch]$UseMaximumSize,[switch]$AssignDriveLetter,$ErrorAction) [pscustomobject]@{Number=1} }
function Format-Volume {
 param([Parameter(ValueFromPipeline=$true)]$InputObject,$FileSystem,$NewFileSystemLabel,[switch]$Confirm,[switch]$Force)
 process {
  if ($FileSystem -ne 'FAT' -or $NewFileSystemLabel -ne 'cidata') { throw 'invalid NoCloud format' }
  if ($env:CBX_TEST_FAIL -eq 'true') { throw 'injected format failure' }
  [pscustomobject]@{Path=$env:CBX_TEST_VOLUME}
 }
}
function Dismount-VHD { param($Path,$ErrorAction) $script:Mounted=$false; $script:Dismounted=$true }
$failed=$false
$detail=''
try {
` + noCloudSeedScript(seed) + `
} catch { $failed=$true; $detail=$_.Exception.Message }
[pscustomobject]@{Failed=$failed;Mounted=$script:Mounted;Dismounted=$script:Dismounted;Detail=$detail} | ConvertTo-Json -Compress
`
			scriptPath := filepath.Join(root, "seed-fixture.ps1")
			if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath)
			fail := "false"
			if failFormat {
				fail = "true"
			}
			cmd.Env = append(os.Environ(), "_CRABBOX_USER_DATA_PATH="+userPath, "_CRABBOX_META_DATA_PATH="+metaPath, "CBX_TEST_VOLUME="+volume, "CBX_TEST_FAIL="+fail)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("isolated seed script: %v\n%s", err, output)
			}
			var state struct {
				Failed, Mounted, Dismounted bool
				Detail                      string
			}
			if err := json.Unmarshal(bytes.TrimSpace(output), &state); err != nil {
				t.Fatalf("invalid seed fixture result: %v\n%s", err, output)
			}
			if state.Failed != failFormat || state.Mounted || !state.Dismounted {
				t.Fatalf("seed mount/failure state=%+v", state)
			}
			if !failFormat {
				for name, want := range map[string]string{"user-data": userData, "meta-data": metaData} {
					got, err := os.ReadFile(filepath.Join(volume, name))
					if err != nil || string(got) != want {
						t.Fatalf("seed %s bytes differ: %v", name, err)
					}
				}
			}
		})
	}
}

// The NoCloud seed must be openable by the Hyper-V management service, which runs as SYSTEM.
// Mount-VHD leaves the seed with a protected DACL and no SYSTEM entry, and something between
// writing the seed and attaching it re-protects the file a second time, so the grant has to be
// made here -- immediately before Add-VMHardDiskDrive -- or the attach fails with
// "Account does not have permission to open attachment ... 0x80070005" and no Linux lease can
// ever be acquired.
//
// This drives the REAL generated script over a REAL file and then reads the access control
// entries actually left behind, rather than asserting that some function was called. The fixture
// reproduces the hostile starting state (inheritance stripped, only the caller granted) and the
// stubbed Add-VMHardDiskDrive records the DACL AS THE ATTACH WOULD SEE IT, so a grant made too
// late would not satisfy it.
func TestLinuxVMCreateGrantsSystemAccessBeforeAttachingSeed(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell behavioral lane")
	}
	executable, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatal("Windows PowerShell is required for the seed behavioral lane")
	}
	f := newLinuxFixture(t)
	const name = "crabbox-vm-acl"
	if err := reserveLinuxStorage(name, "cbx_acl"); err != nil {
		t.Fatal(err)
	}
	seed := ownedCloudInitSeedPath(name)
	if err := os.MkdirAll(filepath.Dir(seed), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seed, []byte("fake seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(t.TempDir(), "acl.json")
	script := `Import-Module Microsoft.PowerShell.Management
Import-Module Microsoft.PowerShell.Utility
$PSModuleAutoloadingPreference='None'
# The state a real Mount-VHD leaves behind: protected, caller only, no SYSTEM.
icacls.exe '` + seed + `' /inheritance:r /grant:r ($env:USERNAME + ':F') | Out-Null
function Get-VM { param($Name,$ErrorAction) }
function Get-VMSwitch { param($Name,$ErrorAction) [pscustomobject]@{Name=$Name} }
function New-VHD { param($Path,$ParentPath,[switch]$Differencing,$ErrorAction) [IO.File]::WriteAllText($Path,'child') }
function New-VM {
 param($Name,$MemoryStartupBytes,$Generation,$VHDPath,$Path,$ErrorAction)
 [pscustomobject]@{Name=$Name;State=3;Id='f72621ca-fb8c-4d75-a7af-18fac5046fda'}
}
function Set-VM { param([Parameter(ValueFromPipeline=$true)]$InputObject,$ProcessorCount,$AutomaticCheckpointsEnabled) process { $InputObject } }
function Set-VMMemory { param($VM,$DynamicMemoryEnabled,$ErrorAction) }
function Set-VMFirmware { param($VM,$EnableSecureBoot,$SecureBootTemplate,$ErrorAction) }
function Add-VMHardDiskDrive {
 param($VM,$ControllerType,$ControllerNumber,$ControllerLocation,$Path,$ErrorAction)
 # Record the DACL exactly as the attach would see it: a grant made after this point is too late.
 $rules = [IO.File]::GetAccessControl($Path).GetAccessRules($true,$true,[Security.Principal.SecurityIdentifier])
 $seen = [pscustomobject]@{
  System=@($rules | Where-Object { $_.IdentityReference.Value -eq 'S-1-5-18' }).Count
  Admins=@($rules | Where-Object { $_.IdentityReference.Value -eq 'S-1-5-32-544' }).Count
 }
 $seen | ConvertTo-Json -Compress | Set-Content -LiteralPath $env:CBX_ACL_RECEIPT -Encoding ASCII
}
function Connect-VMNetworkAdapter { param($VMName,$SwitchName,$ErrorAction) }
` + linuxVMCreateScript(f.b.configForRun(), name) + `
`
	scriptPath := filepath.Join(t.TempDir(), "vm-acl-fixture.ps1")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath)
	cmd.Env = append(os.Environ(), "CBX_ACL_RECEIPT="+receipt)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated VM create script: %v\n%s", err, output)
	}
	raw, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatalf("attach never happened, so the seed ACL was never observed: %v\n%s", err, output)
	}
	var seen struct{ System, Admins int }
	if err := json.Unmarshal(bytes.TrimSpace(raw), &seen); err != nil {
		t.Fatalf("invalid ACL receipt: %v\n%s", err, raw)
	}
	if seen.System == 0 {
		t.Fatal("seed carried no SYSTEM entry when the attach read it; Hyper-V VMMS cannot open the attachment and every Linux lease fails at Add-VMHardDiskDrive")
	}
	if seen.Admins == 0 {
		t.Fatal("seed carried no Administrators entry when the attach read it")
	}
}
