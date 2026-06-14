package firecracker

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/containernetworking/cni/libcni"
	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecAndAliases(t *testing.T) {
	provider := Provider{}
	spec := provider.Spec()
	if provider.Name() != providerName {
		t.Fatalf("provider name=%q", provider.Name())
	}
	if len(provider.Aliases()) != 0 {
		t.Fatalf("aliases=%v want none", provider.Aliases())
	}
	if spec.Name != providerName || spec.Family != "firecracker" || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
}

func TestApplyFlagsUpdatesFirecrackerConfig(t *testing.T) {
	defaults := core.BaseConfig()
	cfg := defaults
	cfg.Provider = providerName
	fs := flag.NewFlagSet("firecracker", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	values := RegisterFirecrackerProviderFlags(fs, defaults)
	args := []string{
		"--firecracker-binary=/usr/local/bin/firecracker",
		"--firecracker-jailer=/usr/local/bin/jailer",
		"--firecracker-kernel=/srv/firecracker/vmlinux",
		"--firecracker-rootfs=/srv/firecracker/rootfs.ext4",
		"--firecracker-user=runner",
		"--firecracker-work-root=/workspace/firecracker",
		"--firecracker-cpus=8",
		"--firecracker-memory-mib=12288",
		"--firecracker-disk-mib=32768",
		"--firecracker-network=cni",
		"--firecracker-cni-network=lab-firecracker",
		"--firecracker-cni-conf-dir=/etc/cni/lab",
		"--firecracker-cni-bin-dir=/opt/cni/lab",
		"--firecracker-launch-timeout=4m",
		"--firecracker-delete-on-release=false",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := ApplyFirecrackerProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Firecracker.Binary != "/usr/local/bin/firecracker" || cfg.Firecracker.Jailer != "/usr/local/bin/jailer" || cfg.Firecracker.Kernel != "/srv/firecracker/vmlinux" || cfg.Firecracker.RootFS != "/srv/firecracker/rootfs.ext4" {
		t.Fatalf("paths=%#v", cfg.Firecracker)
	}
	if cfg.Firecracker.User != "runner" || cfg.SSHUser != "runner" || cfg.Firecracker.WorkRoot != "/workspace/firecracker" || cfg.WorkRoot != "/workspace/firecracker" {
		t.Fatalf("identity cfg=%#v sshUser=%q workRoot=%q", cfg.Firecracker, cfg.SSHUser, cfg.WorkRoot)
	}
	if cfg.Firecracker.CPUs != 8 || cfg.Firecracker.MemoryMiB != 12288 || cfg.Firecracker.DiskMiB != 32768 || cfg.Firecracker.CNINetwork != "lab-firecracker" {
		t.Fatalf("runtime=%#v", cfg.Firecracker)
	}
	if cfg.Firecracker.CNIConfDir != "/etc/cni/lab" || cfg.Firecracker.CNIBinDir != "/opt/cni/lab" || cfg.Firecracker.LaunchTimeout != 4*time.Minute || cfg.Firecracker.DeleteOnRelease {
		t.Fatalf("runtime=%#v", cfg.Firecracker)
	}
	if cfg.SSHPort != "22" || cfg.SSHFallbackPorts != nil {
		t.Fatalf("sshPort=%q fallback=%v", cfg.SSHPort, cfg.SSHFallbackPorts)
	}
	if !core.DeleteOnReleaseExplicit(cfg, providerName) {
		t.Fatal("deleteOnRelease flag should mark explicit state")
	}
}

func TestValidateConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*core.Config)
		wantErr string
	}{
		{
			name: "target must stay linux",
			mutate: func(cfg *core.Config) {
				cfg.TargetOS = core.TargetWindows
			},
			wantErr: "target=linux only",
		},
		{
			name: "tailscale not supported",
			mutate: func(cfg *core.Config) {
				cfg.Network = core.NetworkTailscale
			},
			wantErr: "does not support tailscale",
		},
		{
			name: "work root must be absolute",
			mutate: func(cfg *core.Config) {
				cfg.Firecracker.WorkRoot = "relative/path"
			},
			wantErr: "absolute POSIX path",
		},
		{
			name: "cpus must be positive",
			mutate: func(cfg *core.Config) {
				cfg.Firecracker.CPUs = 0
			},
			wantErr: "firecracker.cpus > 0",
		},
		{
			name: "memory must be positive",
			mutate: func(cfg *core.Config) {
				cfg.Firecracker.MemoryMiB = -1
			},
			wantErr: "firecracker.memoryMiB > 0",
		},
		{
			name: "disk must be positive",
			mutate: func(cfg *core.Config) {
				cfg.Firecracker.DiskMiB = 0
			},
			wantErr: "firecracker.diskMiB > 0",
		},
		{
			name: "network must be cni",
			mutate: func(cfg *core.Config) {
				cfg.Firecracker.Network = "tap"
			},
			wantErr: "supports firecracker.network=cni only",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			tc.mutate(&cfg)
			err := validateConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateConfig err=%v want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestConfigureDoctorReturnsLifecycleScaffold(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	doctor, err := Provider{}.ConfigureDoctor(cfg, core.Runtime{Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doctor.(core.SSHLeaseBackend); !ok {
		t.Fatalf("doctor backend %T does not implement SSHLeaseBackend", doctor)
	}
	if _, ok := doctor.(core.CleanupBackend); !ok {
		t.Fatalf("doctor backend %T does not implement CleanupBackend", doctor)
	}
}

func TestConfigureDoctorReportsStructuredChecksForInvalidLifecycleConfig(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Firecracker.Network = "tap"

	doctor, err := Provider{}.ConfigureDoctor(cfg, core.Runtime{Stderr: io.Discard})
	if err != nil {
		t.Fatalf("ConfigureDoctor: %v", err)
	}
	result, err := doctor.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	check := checksByName(result.Checks)["network"]
	if check.Status != "failed" || !strings.Contains(check.Message, "unsupported") {
		t.Fatalf("network check=%#v", check)
	}
}

func TestDoctorReportsUnsupportedHostAndMissingArtifacts(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Firecracker.CNINetwork = ""

	restore := stubDoctorEnvironment(
		"darwin",
		func(string) (string, error) { return "", errors.New("executable file not found in $PATH") },
		func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		func() error { return nil },
		func(string, string) (*libcni.NetworkConfigList, error) { return nil, errors.New("not reached") },
	)
	defer restore()

	result, err := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(core.DoctorBackend).Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" {
		t.Fatalf("status=%q message=%q checks=%#v", result.Status, result.Message, result.Checks)
	}
	checks := checksByName(result.Checks)
	if checks["host"].Status != "failed" || !strings.Contains(checks["host"].Message, "requires a Linux KVM host") {
		t.Fatalf("host check=%#v", checks["host"])
	}
	if checks["kvm"].Status != "skip" {
		t.Fatalf("kvm check=%#v", checks["kvm"])
	}
	if checks["binary"].Status != "failed" || checks["kernel"].Status != "failed" || checks["rootfs"].Status != "failed" {
		t.Fatalf("artifact checks=%#v", checks)
	}
	if checks["network"].Status != "failed" || !strings.Contains(checks["network"].Message, "firecracker.cniNetwork is required") {
		t.Fatalf("network check=%#v", checks["network"])
	}
}

func TestDoctorReportsReadyChecksWhenPrereqsExist(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Firecracker.Binary = "firecracker"
	cfg.Firecracker.Kernel = "/var/lib/firecracker/vmlinux"
	cfg.Firecracker.RootFS = "/var/lib/firecracker/rootfs.ext4"
	cfg.Firecracker.CNINetwork = "lab-firecracker"
	cfg.Firecracker.CNIConfDir = "/etc/cni/conf.d"
	cfg.Firecracker.CNIBinDir = "/opt/cni/bin"

	restore := stubDoctorEnvironment(
		"linux",
		func(name string) (string, error) {
			if name == "firecracker" {
				return "/usr/local/bin/firecracker", nil
			}
			return name, nil
		},
		func(path string) (os.FileInfo, error) {
			switch path {
			case "/dev/kvm", cfg.Firecracker.Kernel, cfg.Firecracker.RootFS:
				return fakeFileInfo{name: path}, nil
			case cfg.Firecracker.CNIConfDir, cfg.Firecracker.CNIBinDir:
				return fakeFileInfo{name: path, dir: true}, nil
			default:
				return nil, os.ErrNotExist
			}
		},
		func() error { return nil },
		func(string, string) (*libcni.NetworkConfigList, error) { return nil, nil },
	)
	defer restore()

	result, err := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(core.DoctorBackend).Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" {
		t.Fatalf("status=%q message=%q checks=%#v", result.Status, result.Message, result.Checks)
	}
	checks := checksByName(result.Checks)
	for _, name := range []string{"host", "kvm", "binary", "kernel", "rootfs", "network"} {
		if checks[name].Status != "ok" {
			t.Fatalf("%s check=%#v", name, checks[name])
		}
	}
	if checks["jailer"].Status != "skip" {
		t.Fatalf("jailer check=%#v", checks["jailer"])
	}
}

func TestDoctorReportsKVMAccessFailure(t *testing.T) {
	restore := stubDoctorEnvironment(
		"linux",
		exec.LookPath,
		func(path string) (os.FileInfo, error) {
			if path == "/dev/kvm" {
				return fakeFileInfo{name: path}, nil
			}
			return nil, os.ErrNotExist
		},
		func() error { return os.ErrPermission },
		func(string, string) (*libcni.NetworkConfigList, error) { return nil, nil },
	)
	defer restore()

	check := doctorKVMCheck()
	if check.Status != "failed" || !strings.Contains(check.Message, "not accessible") || check.Details["class"] != "environment_blocked" {
		t.Fatalf("kvm check=%#v", check)
	}
}

func TestDoctorReportsMissingNamedCNIConfig(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Firecracker.Network = firecrackerNetworkCNI
	cfg.Firecracker.CNINetwork = "missing-firecracker"
	cfg.Firecracker.CNIConfDir = "/etc/cni/conf.d"
	cfg.Firecracker.CNIBinDir = "/opt/cni/bin"

	restore := stubDoctorEnvironment(
		"linux",
		exec.LookPath,
		func(path string) (os.FileInfo, error) {
			switch path {
			case cfg.Firecracker.CNIConfDir, cfg.Firecracker.CNIBinDir:
				return fakeFileInfo{name: path, dir: true}, nil
			default:
				return nil, os.ErrNotExist
			}
		},
		func() error { return nil },
		func(confDir, network string) (*libcni.NetworkConfigList, error) {
			if confDir != cfg.Firecracker.CNIConfDir || network != cfg.Firecracker.CNINetwork {
				t.Fatalf("LoadConfList(%q, %q)", confDir, network)
			}
			return nil, errors.New("no net config with name missing-firecracker")
		},
	)
	defer restore()

	check := doctorNetworkCheck(cfg)
	if check.Status != "failed" || !strings.Contains(check.Message, "missing-firecracker") || check.Details["class"] != "configuration_incomplete" {
		t.Fatalf("network check=%#v", check)
	}
}

func TestDoctorRejectsConfiguredJailerUntilLifecycleSupportsIt(t *testing.T) {
	check := doctorJailerCheck("/usr/local/bin/jailer")
	if check.Status != "failed" || !strings.Contains(check.Message, "not supported yet") || check.Details["class"] != "configuration_incomplete" {
		t.Fatalf("jailer check=%#v", check)
	}
}

func checksByName(checks []core.DoctorCheck) map[string]core.DoctorCheck {
	out := make(map[string]core.DoctorCheck, len(checks))
	for _, check := range checks {
		out[check.Check] = check
	}
	return out
}

func stubDoctorEnvironment(
	goos string,
	lookPath func(string) (string, error),
	stat func(string) (os.FileInfo, error),
	openKVM func() error,
	loadCNI func(string, string) (*libcni.NetworkConfigList, error),
) func() {
	previousGOOS := firecrackerHostGOOS
	previousLookPath := firecrackerLookPath
	previousStat := firecrackerStat
	previousOpenKVM := firecrackerOpenKVM
	previousLoadCNI := firecrackerLoadCNI
	firecrackerHostGOOS = goos
	firecrackerLookPath = lookPath
	firecrackerStat = stat
	firecrackerOpenKVM = openKVM
	firecrackerLoadCNI = loadCNI
	return func() {
		firecrackerHostGOOS = previousGOOS
		firecrackerLookPath = previousLookPath
		firecrackerStat = previousStat
		firecrackerOpenKVM = previousOpenKVM
		firecrackerLoadCNI = previousLoadCNI
	}
}

type fakeFileInfo struct {
	name string
	dir  bool
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() any           { return nil }

func (f fakeFileInfo) Mode() os.FileMode {
	if f.dir {
		return os.ModeDir | 0o755
	}
	return 0o644
}
