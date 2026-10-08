package hyperv

import (
	"context"
	"encoding/json"
	"fmt"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"strings"
)

const (
	secureBootTemplateWindows = "MicrosoftWindows"
	secureBootTemplateLinux   = "MicrosoftUEFICertificateAuthority"
)

type firmwareSettings struct {
	enabled  bool
	template string
}

type firmwareOutput struct {
	Enabled  bool   `json:"Enabled"`
	Template string `json:"Template"`
}

func normalizeSecureBootMode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return secureBootAuto
	}
	return value
}

func secureBootSettings(targetOS, mode string) (firmwareSettings, error) {
	mode = normalizeSecureBootMode(mode)
	switch mode {
	case secureBootOff:
		return firmwareSettings{}, nil
	case secureBootWindows:
		return firmwareSettings{enabled: true, template: secureBootTemplateWindows}, nil
	case secureBootLinux:
		return firmwareSettings{enabled: true, template: secureBootTemplateLinux}, nil
	case secureBootAuto:
		switch targetOS {
		case "", targetWindows:
			return firmwareSettings{enabled: true, template: secureBootTemplateWindows}, nil
		case targetLinux:
			return firmwareSettings{enabled: true, template: secureBootTemplateLinux}, nil
		}
	default:
		return firmwareSettings{}, core.Exit(2, "invalid --hyperv-secure-boot=%q; use auto, windows, linux, or off", mode)
	}
	return firmwareSettings{}, core.Exit(2, "provider=%s cannot select secure boot firmware for target=%s", providerName, targetOS)
}

func (b *backend) configureVMFirmware(ctx context.Context, cfg core.Config, name string) error {
	settings, err := secureBootSettings(cfg.TargetOS, cfg.HyperV.SecureBoot)
	if err != nil {
		return err
	}
	return b.configureVMFirmwareSettings(ctx, name, settings)
}

func (b *backend) configureVMFirmwareSettings(ctx context.Context, name string, settings firmwareSettings) error {
	script := fmt.Sprintf(
		`Set-VMFirmware -VMName '%s' -EnableSecureBoot Off`,
		escapePSString(name),
	)
	if settings.enabled {
		script = fmt.Sprintf(
			`Set-VMFirmware -VMName '%s' -EnableSecureBoot On -SecureBootTemplate '%s'`,
			escapePSString(name), escapePSString(settings.template),
		)
	}
	result, err := b.powershell(ctx, script)
	if err != nil {
		return shared.LocalCommandError("Set-VMFirmware", result, err)
	}
	return nil
}

func (b *backend) queryVMFirmwareSettings(ctx context.Context, name, id string) (firmwareSettings, error) {
	script := fmt.Sprintf(
		`$ErrorActionPreference='Stop'; `+
			`$vm=Get-VM -Name '%s' -ErrorAction Stop; `+
			`if ($vm.Id.Guid -ne '%s') { throw 'source VM identity changed' }; `+
			`$firmware=Get-VMFirmware -VM $vm -ErrorAction Stop; `+
			`[pscustomobject]@{Enabled=([string]$firmware.SecureBoot -eq 'On');Template=[string]$firmware.SecureBootTemplate} | ConvertTo-Json -Compress`,
		escapePSString(name),
		escapePSString(id),
	)
	result, err := b.powershell(ctx, script)
	if err != nil {
		return firmwareSettings{}, shared.LocalCommandError("Get-VMFirmware", result, err)
	}
	var output firmwareOutput
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &output); err != nil {
		return firmwareSettings{}, core.Exit(2, "parse Hyper-V firmware settings: %v", err)
	}
	if output.Enabled && strings.TrimSpace(output.Template) == "" {
		return firmwareSettings{}, core.Exit(2, "Hyper-V VM %s has secure boot enabled without a template", name)
	}
	return firmwareSettings{enabled: output.Enabled, template: strings.TrimSpace(output.Template)}, nil
}
