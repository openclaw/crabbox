package codesandbox

import (
	"flag"
	"strings"
)

type codeSandboxFlagValues struct {
	TemplateID               *string
	Workdir                  *string
	VMTier                   *string
	Privacy                  *string
	HibernationTimeoutSecs   *int
	AutomaticWakeupHTTP      *bool
	AutomaticWakeupWebSocket *bool
	BridgeCommand            *string
	SDKPackage               *string
	DoctorListLimit          *int
	OperationTimeoutSecs     *int
}

func RegisterCodeSandboxProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return codeSandboxFlagValues{
		TemplateID:               fs.String("codesandbox-template-id", defaults.CodeSandbox.TemplateID, "CodeSandbox template ID used by later lifecycle operations"),
		Workdir:                  fs.String("codesandbox-workdir", defaults.CodeSandbox.Workdir, "Absolute working directory inside the sandbox; must be under /project/workspace"),
		VMTier:                   fs.String("codesandbox-vm-tier", defaults.CodeSandbox.VMTier, "CodeSandbox VM tier for later create operations (empty = workspace default)"),
		Privacy:                  fs.String("codesandbox-privacy", defaults.CodeSandbox.Privacy, "CodeSandbox sandbox privacy for later create operations"),
		HibernationTimeoutSecs:   fs.Int("codesandbox-hibernation-timeout-secs", defaults.CodeSandbox.HibernationTimeoutSecs, "CodeSandbox hibernation timeout in seconds (0 = service default)"),
		AutomaticWakeupHTTP:      fs.Bool("codesandbox-automatic-wakeup-http", defaults.CodeSandbox.AutomaticWakeupHTTP, "allow automatic wakeup on HTTP requests"),
		AutomaticWakeupWebSocket: fs.Bool("codesandbox-automatic-wakeup-websocket", defaults.CodeSandbox.AutomaticWakeupWebSocket, "allow automatic wakeup on WebSocket connections"),
		BridgeCommand:            fs.String("codesandbox-bridge-command", defaults.CodeSandbox.BridgeCommand, "local Node-compatible command used for the CodeSandbox SDK bridge"),
		SDKPackage:               fs.String("codesandbox-sdk-package", defaults.CodeSandbox.SDKPackage, "Node package spec imported by the CodeSandbox SDK bridge"),
		DoctorListLimit:          fs.Int("codesandbox-doctor-list-limit", defaults.CodeSandbox.DoctorListLimit, "maximum sandboxes read by non-mutating doctor readiness"),
		OperationTimeoutSecs:     fs.Int("codesandbox-operation-timeout-secs", defaults.CodeSandbox.OperationTimeoutSecs, "SDK bridge operation timeout in seconds"),
	}
}

func ApplyCodeSandboxProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(codeSandboxFlagValues)
	if !ok {
		return nil
	}
	if codeSandboxProviderSelected(cfg.Provider) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=codesandbox; use --codesandbox-vm-tier")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=codesandbox; use --codesandbox-vm-tier")
		}
	}
	if flagWasSet(fs, "codesandbox-template-id") {
		cfg.CodeSandbox.TemplateID = *v.TemplateID
	}
	if flagWasSet(fs, "codesandbox-workdir") {
		cfg.CodeSandbox.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "codesandbox-vm-tier") {
		cfg.CodeSandbox.VMTier = *v.VMTier
	}
	if flagWasSet(fs, "codesandbox-privacy") {
		cfg.CodeSandbox.Privacy = *v.Privacy
	}
	if flagWasSet(fs, "codesandbox-hibernation-timeout-secs") {
		cfg.CodeSandbox.HibernationTimeoutSecs = *v.HibernationTimeoutSecs
	}
	if flagWasSet(fs, "codesandbox-automatic-wakeup-http") {
		cfg.CodeSandbox.AutomaticWakeupHTTP = *v.AutomaticWakeupHTTP
	}
	if flagWasSet(fs, "codesandbox-automatic-wakeup-websocket") {
		cfg.CodeSandbox.AutomaticWakeupWebSocket = *v.AutomaticWakeupWebSocket
	}
	if flagWasSet(fs, "codesandbox-bridge-command") {
		cfg.CodeSandbox.BridgeCommand = *v.BridgeCommand
	}
	if flagWasSet(fs, "codesandbox-sdk-package") {
		cfg.CodeSandbox.SDKPackage = *v.SDKPackage
	}
	if flagWasSet(fs, "codesandbox-doctor-list-limit") {
		cfg.CodeSandbox.DoctorListLimit = *v.DoctorListLimit
	}
	if flagWasSet(fs, "codesandbox-operation-timeout-secs") {
		cfg.CodeSandbox.OperationTimeoutSecs = *v.OperationTimeoutSecs
	}
	return validateCodeSandboxConfig(*cfg)
}

func codeSandboxProviderSelected(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "csb", "code-sandbox":
		return true
	default:
		return false
	}
}
