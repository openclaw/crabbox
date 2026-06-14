package firecracker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	firecrackerDefaultKernelArgs = "root=/dev/vda rw console=ttyS0 noapic reboot=k panic=1 pci=off nomodules"
	firecrackerGuestInterface    = "eth0"
	firecrackerHostInterface     = "veth0"
	firecrackerStopTimeout       = 15 * time.Second
	firecrackerKillTimeout       = 5 * time.Second
)

type machine interface {
	Start(context.Context) error
	StopVMM() error
	PID() int
	GuestIP() string
}

type machineLaunchConfig struct {
	BinaryPath    string
	SocketPath    string
	LogPath       string
	KernelPath    string
	KernelArgs    string
	RootFSPath    string
	CloudInitPath string
	VMID          string
	NetNSPath     string
	CNINetwork    string
	CNIConfDir    string
	CNIBinDir     string
	CNICacheDir   string
	CPUs          int
	MemoryMiB     int
}

type machineFactory interface {
	New(context.Context, machineLaunchConfig) (machine, error)
}

type processIdentity struct {
	PID     int
	Started string
	BootID  string
}

type processManager interface {
	Capture(pid int) (processIdentity, error)
	Matches(identity processIdentity) bool
	Signal(identity processIdentity, sig syscall.Signal) error
}

type localProcessManager struct{}

func (localProcessManager) Capture(pid int) (processIdentity, error) {
	if pid <= 0 {
		return processIdentity{}, fmt.Errorf("firecracker process pid unavailable")
	}
	started, startedErr := core.LocalProcessStartIdentity(pid)
	bootID, bootErr := core.LocalProcessBootIdentity()
	if core.LocalProcessBootIdentityRequired() {
		if bootErr != nil {
			return processIdentity{}, fmt.Errorf("identify firecracker process boot: %w", bootErr)
		}
		if startedErr != nil {
			return processIdentity{}, fmt.Errorf("identify firecracker process start: %w", startedErr)
		}
		if strings.TrimSpace(started) == "" {
			return processIdentity{}, fmt.Errorf("identify firecracker process start: empty start identity")
		}
	} else {
		if startedErr != nil {
			started = ""
		}
		if bootErr != nil {
			bootID = ""
		}
	}
	return processIdentity{PID: pid, Started: started, BootID: bootID}, nil
}

func (localProcessManager) Matches(identity processIdentity) bool {
	if identity.PID <= 0 {
		return false
	}
	if core.LocalProcessBootIdentityRequired() {
		if strings.TrimSpace(identity.BootID) == "" {
			return false
		}
		bootID, err := core.LocalProcessBootIdentity()
		if err != nil {
			return processAlive(identity.PID)
		}
		if bootID != identity.BootID {
			return false
		}
	}
	if strings.TrimSpace(identity.Started) == "" {
		if core.LocalProcessBootIdentityRequired() {
			return false
		}
		return processAlive(identity.PID)
	}
	started, err := core.LocalProcessStartIdentity(identity.PID)
	if err != nil {
		return processAlive(identity.PID)
	}
	return started == identity.Started
}

func (p localProcessManager) Signal(identity processIdentity, sig syscall.Signal) error {
	if identity.PID <= 0 || !p.Matches(identity) {
		return nil
	}
	if err := syscall.Kill(identity.PID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func waitForProcessExit(manager processManager, identity processIdentity, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for manager.Matches(identity) {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for firecracker process %d to exit", identity.PID)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}
