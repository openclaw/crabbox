//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	firesdk "github.com/firecracker-microvm/firecracker-go-sdk"
	fcmodels "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"
)

type sdkMachineFactory struct {
	LogWriter io.Writer
}

func (f sdkMachineFactory) New(ctx context.Context, launch machineLaunchConfig) (machine, error) {
	binary := strings.TrimSpace(launch.BinaryPath)
	if binary == "" {
		binary = "firecracker"
	}
	sdkConfig := firesdk.Config{
		SocketPath:      launch.SocketPath,
		LogPath:         launch.LogPath,
		LogLevel:        "Info",
		KernelImagePath: launch.KernelPath,
		KernelArgs:      launch.KernelArgs,
		Drives: firesdk.NewDrivesBuilder(launch.RootFSPath).
			AddDrive(launch.CloudInitPath, true, firesdk.WithDriveID("cidata")).
			Build(),
		NetworkInterfaces: firesdk.NetworkInterfaces{{
			CNIConfiguration: &firesdk.CNIConfiguration{
				NetworkName: launch.CNINetwork,
				IfName:      firecrackerHostInterface,
				VMIfName:    firecrackerGuestInterface,
				BinPath:     []string{launch.CNIBinDir},
				ConfDir:     launch.CNIConfDir,
				CacheDir:    launch.CNICacheDir,
				Force:       true,
			},
		}},
		MachineCfg: fcmodels.MachineConfiguration{
			VcpuCount:  firesdk.Int64(int64(launch.CPUs)),
			MemSizeMib: firesdk.Int64(int64(launch.MemoryMiB)),
			Smt:        firesdk.Bool(false),
		},
		VMID:  launch.VMID,
		NetNS: launch.NetNSPath,
	}
	cmd := firesdk.VMCommandBuilder{}.
		WithBin(binary).
		WithSocketPath(sdkConfig.SocketPath).
		Build(context.WithoutCancel(ctx))

	logger := logrus.New()
	if f.LogWriter == nil {
		logger.SetOutput(io.Discard)
	} else {
		logger.SetOutput(f.LogWriter)
	}
	logger.SetLevel(logrus.WarnLevel)

	vm, err := firesdk.NewMachine(
		context.WithoutCancel(ctx),
		sdkConfig,
		firesdk.WithProcessRunner(cmd),
		firesdk.WithLogger(logrus.NewEntry(logger)),
	)
	if err != nil {
		return nil, err
	}
	return &sdkMachine{machine: vm, cmd: cmd}, nil
}

type sdkMachine struct {
	machine *firesdk.Machine
	cmd     *exec.Cmd
}

func (m *sdkMachine) Start(ctx context.Context) error {
	if m == nil || m.machine == nil {
		return fmt.Errorf("firecracker machine is unavailable")
	}
	return m.machine.Start(context.WithoutCancel(ctx))
}

func (m *sdkMachine) StopVMM() error {
	if m == nil || m.machine == nil {
		return nil
	}
	return m.machine.StopVMM()
}

func (m *sdkMachine) PID() int {
	if m == nil || m.cmd == nil || m.cmd.Process == nil {
		return 0
	}
	return m.cmd.Process.Pid
}

func (m *sdkMachine) GuestIP() string {
	if m == nil || m.machine == nil {
		return ""
	}
	for _, iface := range m.machine.Cfg.NetworkInterfaces {
		if iface.StaticConfiguration == nil || iface.StaticConfiguration.IPConfiguration == nil {
			continue
		}
		if ip := iface.StaticConfiguration.IPConfiguration.IPAddr.IP.String(); strings.TrimSpace(ip) != "" {
			return ip
		}
	}
	return ""
}
