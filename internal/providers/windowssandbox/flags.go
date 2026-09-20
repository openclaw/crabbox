package windowssandbox

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterWindowsSandboxConfigFlags(fs, defaults.WindowsSandbox)
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.WindowsSandboxConfigFlagValues)
	if !ok {
		return nil
	}
	visited := core.WindowsSandboxConfigFlagPresence(fs)
	selected := core.ProviderNameMatchesExact(cfg.Provider, Provider{})
	if selected {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "Windows Sandbox sizing is controlled by the host", "Windows Sandbox sizing is controlled by the host"); err != nil {
			return err
		}
		if !core.FlagWasSet(fs, "target") {
			cfg.TargetOS = targetWindows
		}
		if !core.FlagWasSet(fs, "windows-mode") {
			cfg.WindowsMode = windowsModeNormal
		}
	}
	if visited.Workdir {
		cfg.WindowsSandbox.Workdir = *v.Workdir
		core.RecordProviderFlagInputs(cfg, true, "windows-sandbox")
	}
	if visited.TempRoot {
		cfg.WindowsSandbox.TempRoot = *v.TempRoot
		core.RecordProviderFlagInputs(cfg, true, "windows-sandbox")
	}
	if visited.Networking {
		normalized, err := normalizeWSBState(*v.Networking, "windows-sandbox-networking")
		if err != nil {
			return err
		}
		cfg.WindowsSandbox.Networking = normalized
		core.RecordProviderFlagInputs(cfg, true, "windows-sandbox")
	}
	if visited.VGPU {
		normalized, err := normalizeWSBState(*v.VGPU, "windows-sandbox-vgpu")
		if err != nil {
			return err
		}
		cfg.WindowsSandbox.VGPU = normalized
		core.RecordProviderFlagInputs(cfg, true, "windows-sandbox")
	}
	if visited.Clipboard {
		normalized, err := normalizeWSBState(*v.Clipboard, "windows-sandbox-clipboard")
		if err != nil {
			return err
		}
		cfg.WindowsSandbox.Clipboard = normalized
		core.RecordProviderFlagInputs(cfg, true, "windows-sandbox")
	}
	if visited.ProtectedClient {
		normalized, err := normalizeWSBState(*v.ProtectedClient, "windows-sandbox-protected-client")
		if err != nil {
			return err
		}
		cfg.WindowsSandbox.ProtectedClient = normalized
		core.RecordProviderFlagInputs(cfg, true, "windows-sandbox")
	}
	if visited.AudioInput {
		normalized, err := normalizeWSBState(*v.AudioInput, "windows-sandbox-audio-input")
		if err != nil {
			return err
		}
		cfg.WindowsSandbox.AudioInput = normalized
		core.RecordProviderFlagInputs(cfg, true, "windows-sandbox")
	}
	if visited.VideoInput {
		normalized, err := normalizeWSBState(*v.VideoInput, "windows-sandbox-video-input")
		if err != nil {
			return err
		}
		cfg.WindowsSandbox.VideoInput = normalized
		core.RecordProviderFlagInputs(cfg, true, "windows-sandbox")
	}
	if visited.PrinterRedirection {
		normalized, err := normalizeWSBState(*v.PrinterRedirection, "windows-sandbox-printer-redirection")
		if err != nil {
			return err
		}
		cfg.WindowsSandbox.PrinterRedirection = normalized
		core.RecordProviderFlagInputs(cfg, true, "windows-sandbox")
	}
	if visited.MemoryMB {
		if *v.MemoryMB < 0 {
			return core.Exit(2, "--windows-sandbox-memory-mb must be non-negative")
		}
		cfg.WindowsSandbox.MemoryMB = *v.MemoryMB
		core.RecordProviderFlagInputs(cfg, true, "windows-sandbox")
	}
	if selected {
		applyDefaults(cfg)
	}
	return nil
}
