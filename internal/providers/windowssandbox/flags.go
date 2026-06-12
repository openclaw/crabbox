package windowssandbox

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	Workdir            *string
	TempRoot           *string
	Networking         *string
	VGPU               *string
	Clipboard          *string
	ProtectedClient    *string
	AudioInput         *string
	VideoInput         *string
	PrinterRedirection *string
	MemoryMB           *int
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		Workdir:            fs.String("windows-sandbox-workdir", defaults.WindowsSandbox.Workdir, "absolute working directory inside Windows Sandbox"),
		TempRoot:           fs.String("windows-sandbox-temp-root", defaults.WindowsSandbox.TempRoot, "host directory for temporary Windows Sandbox workspaces"),
		Networking:         fs.String("windows-sandbox-networking", defaults.WindowsSandbox.Networking, "Windows Sandbox networking: enable, disable, or default"),
		VGPU:               fs.String("windows-sandbox-vgpu", defaults.WindowsSandbox.VGPU, "Windows Sandbox vGPU: enable, disable, or default"),
		Clipboard:          fs.String("windows-sandbox-clipboard", defaults.WindowsSandbox.Clipboard, "Windows Sandbox clipboard redirection: enable, disable, or default"),
		ProtectedClient:    fs.String("windows-sandbox-protected-client", defaults.WindowsSandbox.ProtectedClient, "Windows Sandbox protected client: enable, disable, or default"),
		AudioInput:         fs.String("windows-sandbox-audio-input", defaults.WindowsSandbox.AudioInput, "Windows Sandbox audio input: enable, disable, or default"),
		VideoInput:         fs.String("windows-sandbox-video-input", defaults.WindowsSandbox.VideoInput, "Windows Sandbox video input: enable, disable, or default"),
		PrinterRedirection: fs.String("windows-sandbox-printer-redirection", defaults.WindowsSandbox.PrinterRedirection, "Windows Sandbox printer redirection: enable, disable, or default"),
		MemoryMB:           fs.Int("windows-sandbox-memory-mb", defaults.WindowsSandbox.MemoryMB, "Windows Sandbox memory in MB; 0 leaves the platform default"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	selected := cfg.Provider == providerName || cfg.Provider == "wsb" || cfg.Provider == "windows-sandbox-provider"
	if selected {
		if core.FlagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s; Windows Sandbox sizing is controlled by the host", providerName)
		}
		if core.FlagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s; Windows Sandbox sizing is controlled by the host", providerName)
		}
		if !core.FlagWasSet(fs, "target") {
			cfg.TargetOS = targetWindows
		}
		if !core.FlagWasSet(fs, "windows-mode") {
			cfg.WindowsMode = windowsModeNormal
		}
	}
	if core.FlagWasSet(fs, "windows-sandbox-workdir") {
		cfg.WindowsSandbox.Workdir = *v.Workdir
	}
	if core.FlagWasSet(fs, "windows-sandbox-temp-root") {
		cfg.WindowsSandbox.TempRoot = *v.TempRoot
	}
	if core.FlagWasSet(fs, "windows-sandbox-networking") {
		normalized, err := normalizeWSBState(*v.Networking, "windows-sandbox-networking")
		if err != nil {
			return err
		}
		cfg.WindowsSandbox.Networking = normalized
	}
	if core.FlagWasSet(fs, "windows-sandbox-vgpu") {
		normalized, err := normalizeWSBState(*v.VGPU, "windows-sandbox-vgpu")
		if err != nil {
			return err
		}
		cfg.WindowsSandbox.VGPU = normalized
	}
	if core.FlagWasSet(fs, "windows-sandbox-clipboard") {
		normalized, err := normalizeWSBState(*v.Clipboard, "windows-sandbox-clipboard")
		if err != nil {
			return err
		}
		cfg.WindowsSandbox.Clipboard = normalized
	}
	if core.FlagWasSet(fs, "windows-sandbox-protected-client") {
		normalized, err := normalizeWSBState(*v.ProtectedClient, "windows-sandbox-protected-client")
		if err != nil {
			return err
		}
		cfg.WindowsSandbox.ProtectedClient = normalized
	}
	if core.FlagWasSet(fs, "windows-sandbox-audio-input") {
		normalized, err := normalizeWSBState(*v.AudioInput, "windows-sandbox-audio-input")
		if err != nil {
			return err
		}
		cfg.WindowsSandbox.AudioInput = normalized
	}
	if core.FlagWasSet(fs, "windows-sandbox-video-input") {
		normalized, err := normalizeWSBState(*v.VideoInput, "windows-sandbox-video-input")
		if err != nil {
			return err
		}
		cfg.WindowsSandbox.VideoInput = normalized
	}
	if core.FlagWasSet(fs, "windows-sandbox-printer-redirection") {
		normalized, err := normalizeWSBState(*v.PrinterRedirection, "windows-sandbox-printer-redirection")
		if err != nil {
			return err
		}
		cfg.WindowsSandbox.PrinterRedirection = normalized
	}
	if core.FlagWasSet(fs, "windows-sandbox-memory-mb") {
		if *v.MemoryMB < 0 {
			return exit(2, "--windows-sandbox-memory-mb must be non-negative")
		}
		cfg.WindowsSandbox.MemoryMB = *v.MemoryMB
	}
	if selected {
		applyDefaults(cfg)
	}
	return nil
}
