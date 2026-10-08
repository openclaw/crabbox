package hyperv

import (
	"context"
	"flag"
	"io"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestSecureBootSettings(t *testing.T) {
	tests := []struct {
		name     string
		targetOS string
		mode     string
		enabled  bool
		template string
	}{
		{name: "auto windows", targetOS: targetWindows, mode: secureBootAuto, enabled: true, template: secureBootTemplateWindows},
		{name: "auto linux", targetOS: targetLinux, mode: secureBootAuto, enabled: true, template: secureBootTemplateLinux},
		{name: "explicit windows", targetOS: targetLinux, mode: secureBootWindows, enabled: true, template: secureBootTemplateWindows},
		{name: "explicit linux", targetOS: targetWindows, mode: secureBootLinux, enabled: true, template: secureBootTemplateLinux},
		{name: "off", targetOS: targetLinux, mode: secureBootOff},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := secureBootSettings(tc.targetOS, tc.mode)
			if err != nil {
				t.Fatal(err)
			}
			if got.enabled != tc.enabled || got.template != tc.template {
				t.Fatalf("settings=%#v want enabled=%v template=%q", got, tc.enabled, tc.template)
			}
		})
	}
}

func TestConfigureVMFirmwareUsesMappedTemplate(t *testing.T) {
	tests := []struct {
		name     string
		targetOS string
		mode     string
		want     string
	}{
		{name: "windows auto", targetOS: targetWindows, mode: secureBootAuto, want: "-EnableSecureBoot On -SecureBootTemplate 'MicrosoftWindows'"},
		{name: "linux auto", targetOS: targetLinux, mode: secureBootAuto, want: "-EnableSecureBoot On -SecureBootTemplate 'MicrosoftUEFICertificateAuthority'"},
		{name: "off", targetOS: targetLinux, mode: secureBootOff, want: "-EnableSecureBoot Off"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{}
			b := testBackend(runner)
			cfg := b.configForRun()
			cfg.TargetOS = tc.targetOS
			cfg.HyperV.SecureBoot = tc.mode
			if err := b.configureVMFirmware(context.Background(), cfg, "crabbox-blue-1234"); err != nil {
				t.Fatal(err)
			}
			if len(runner.calls) != 1 {
				t.Fatalf("calls=%d want 1", len(runner.calls))
			}
			script := runner.calls[0].Args[len(runner.calls[0].Args)-1]
			if !strings.Contains(script, tc.want) {
				t.Fatalf("firmware script=%q want %q", script, tc.want)
			}
		})
	}
}

func TestConfigureRejectsInvalidSecureBoot(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.HyperV.SecureBoot = "shim"
	_, err := (Provider{}).Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: &recordingRunner{}})
	if err == nil || !strings.Contains(err.Error(), "auto, windows, linux, or off") {
		t.Fatalf("Configure error=%v", err)
	}
}

func TestHyperVFlagHelpDescribesBothTargets(t *testing.T) {
	fs := flag.NewFlagSet("hyperv", flag.ContinueOnError)
	registerFlags(fs, core.BaseConfig())
	for _, name := range []string{"hyperv-image", "hyperv-work-root"} {
		usage := strings.ToLower(fs.Lookup(name).Usage)
		if strings.Contains(usage, "windows") || strings.Contains(usage, "administrator") {
			t.Fatalf("--%s help is target-specific: %q", name, usage)
		}
		userHelp := strings.ToLower(fs.Lookup("hyperv-user").Usage)
		if !strings.Contains(userHelp, "linux") || !strings.Contains(userHelp, "administrator") {
			t.Fatalf("--hyperv-user help must describe Linux SSH and Windows administrator accounts: %q", userHelp)
		}
	}
	if got := fs.Lookup("hyperv-secure-boot"); got == nil || got.DefValue != "" {
		t.Fatalf("secure boot flag=%#v", got)
	}
}
