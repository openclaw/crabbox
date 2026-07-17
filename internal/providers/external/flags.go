package external

import (
	"encoding/json"
	"flag"
	"fmt"
	"path"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	Command            *string
	Args               *stringListFlag
	ConfigJSON         *string
	WorkRoot           *string
	RoutingFile        *string
	RoutingDigest      *string
	DesktopUsername    *string
	DesktopPasswordEnv *string
	IdempotentLeaseID  *bool
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	args := &stringListFlag{}
	fs.Var(args, "external-arg", "external provider argument; repeatable")
	return flagValues{
		Command:    fs.String("external-command", defaults.External.Command, "external provider executable"),
		Args:       args,
		ConfigJSON: fs.String("external-config-json", "{}", "external provider config as a JSON object"),
		WorkRoot:   fs.String("external-work-root", defaults.External.WorkRoot, "external provider Crabbox work root"),
		RoutingFile: fs.String(
			"external-routing-file",
			defaults.External.RoutingFile,
			"private external provider routing state file",
		),
		RoutingDigest: fs.String(
			"external-routing-digest",
			core.ExternalRoutingDigest(defaults.External),
			"expected SHA-256 digest for an internal external-provider routing handoff",
		),
		DesktopUsername:    fs.String("external-desktop-username", defaults.External.Connection.Desktop.Username, "external macOS Screen Sharing account; defaults to resolved SSH user"),
		DesktopPasswordEnv: fs.String("external-desktop-password-env", defaults.External.Connection.Desktop.PasswordEnv, "environment variable name containing the external macOS Screen Sharing account password"),
		IdempotentLeaseID:  fs.Bool("external-idempotent-lease-id", defaults.External.Capabilities.IdempotentLeaseID, "adapter guarantees idempotent acquisition for caller-supplied lease IDs"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	digestWasSet := core.FlagWasSet(fs, "external-routing-digest")
	if digestWasSet && !core.FlagWasSet(fs, "external-routing-file") {
		return core.Exit(2, "--external-routing-digest requires --external-routing-file")
	}
	if core.FlagWasSet(fs, "external-routing-file") {
		core.MarkExternalRoutingFileExplicit(cfg)
		cfg.External.RoutingFile = *v.RoutingFile
		routing, err := loadRoutingFile(cfg.External.RoutingFile, *v.RoutingDigest, digestWasSet)
		if err != nil {
			return core.Exit(2, "%v", err)
		}
		core.PreserveExternalDesktopChildEnvironmentBoundary(cfg)
		cfg.External = routing
		restoreRoutingTarget(cfg, fs)
		core.MarkExternalRoutingCredentialSources(cfg)
		markRestoredRoutingTargetSources(cfg, fs)
		core.ApplyExternalDesktopEnvironmentOverrides(cfg)
		cfg.WorkRoot = externalWorkRoot(*cfg)
	} else if path := strings.TrimSpace(cfg.External.RoutingFile); path != "" && !core.ExternalRoutingLoaded(cfg.External) {
		digest := core.ExternalRoutingDigest(cfg.External)
		routing, err := loadRoutingFile(path, digest, digest != "")
		if err != nil {
			return core.Exit(2, "%v", err)
		}
		core.PreserveExternalDesktopChildEnvironmentBoundary(cfg)
		cfg.External = routing
		restoreRoutingTarget(cfg, fs)
		core.MarkExternalRoutingCredentialSources(cfg)
		markRestoredRoutingTargetSources(cfg, fs)
		core.ApplyExternalDesktopEnvironmentOverrides(cfg)
		cfg.WorkRoot = externalWorkRoot(*cfg)
	}
	if core.FlagWasSet(fs, "external-command") {
		cfg.External.Command = *v.Command
	}
	if core.FlagWasSet(fs, "external-arg") {
		cfg.External.Args = append([]string(nil), v.Args.values...)
	}
	if core.FlagWasSet(fs, "external-config-json") {
		config := map[string]any{}
		if err := json.Unmarshal([]byte(*v.ConfigJSON), &config); err != nil {
			return core.Exit(2, "external config JSON must be an object: %v", err)
		}
		cfg.External.Config = config
	}
	if core.FlagWasSet(fs, "external-work-root") {
		cfg.External.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if core.FlagWasSet(fs, "external-idempotent-lease-id") {
		cfg.External.Capabilities.IdempotentLeaseID = *v.IdempotentLeaseID
	}
	if core.FlagWasSet(fs, "external-command") || core.FlagWasSet(fs, "external-arg") || core.FlagWasSet(fs, "external-config-json") {
		core.MarkExternalProviderOutputFlagExplicit(cfg)
	}
	if core.FlagWasSet(fs, "external-desktop-username") {
		cfg.External.Connection.Desktop.Username = *v.DesktopUsername
		core.MarkExternalDesktopUsernameExplicit(cfg)
	}
	if core.FlagWasSet(fs, "external-desktop-password-env") {
		core.PreserveExternalDesktopChildEnvironmentBoundary(cfg)
		cfg.External.Connection.Desktop.PasswordEnv = *v.DesktopPasswordEnv
		core.MarkExternalDesktopPasswordEnvExplicit(cfg)
	}
	return validateConfig(*cfg)
}

func loadRoutingFile(path, digest string, bound bool) (core.ExternalConfig, error) {
	if bound {
		return core.LoadExternalRoutingWithDigest(path, digest)
	}
	return core.LoadExternalRouting(path)
}

type stringListFlag struct {
	values []string
	set    bool
}

func (f *stringListFlag) String() string {
	return strings.Join(f.values, ",")
}

func (f *stringListFlag) Set(value string) error {
	if !f.set {
		f.values = nil
		f.set = true
	}
	f.values = append(f.values, value)
	return nil
}

func validateConfig(cfg core.Config) error {
	hasCommand := strings.TrimSpace(cfg.External.Command) != ""
	hasLifecycle := lifecycleConfigured(cfg.External)
	if hasCommand == hasLifecycle {
		return core.Exit(2, "configure exactly one of external.command or external.lifecycle.acquire")
	}
	if hasCommand && strings.ContainsRune(cfg.External.Command, '\x00') {
		return core.Exit(2, "external.command contains a NUL byte")
	}
	for _, arg := range cfg.External.Args {
		if strings.ContainsRune(arg, '\x00') {
			return core.Exit(2, "external.args contains a NUL byte")
		}
	}
	if hasLifecycle {
		if !lifecycleOperationConfigured(cfg.External.Lifecycle.Release) {
			return core.Exit(2, "external.lifecycle.release.argv or steps is required")
		}
		if !lifecycleOperationConfigured(cfg.External.Lifecycle.List) {
			return core.Exit(2, "external.lifecycle.list.argv or steps is required")
		}
		for name, operation := range map[string]core.ExternalLifecycleOperation{
			"doctor":  cfg.External.Lifecycle.Doctor,
			"acquire": cfg.External.Lifecycle.Acquire,
			"resolve": cfg.External.Lifecycle.Resolve,
			"list":    cfg.External.Lifecycle.List,
			"release": cfg.External.Lifecycle.Release,
			"touch":   cfg.External.Lifecycle.Touch,
			"cleanup": cfg.External.Lifecycle.Cleanup,
		} {
			if err := validateLifecycleOperation(name, operation); err != nil {
				return err
			}
		}
		if strings.TrimSpace(cfg.External.Connection.SSH.User) == "" {
			return core.Exit(2, "external.connection.ssh.user is required")
		}
		if cfg.External.Lifecycle.List.Output != lifecycleOutputJSONNameArray && cfg.External.Lifecycle.List.Output != lifecycleOutputJSONLeaseArray {
			return core.Exit(2, "external.lifecycle.list.output must be %q or %q", lifecycleOutputJSONNameArray, lifecycleOutputJSONLeaseArray)
		}
	}
	if desktopPasswordEnv := strings.TrimSpace(cfg.External.Connection.Desktop.PasswordEnv); desktopPasswordEnv != "" && !lifecycleEnvNamePattern.MatchString(desktopPasswordEnv) {
		return core.Exit(2, "external.connection.desktop.passwordEnv must be an environment variable name")
	}
	if err := core.ValidateExternalDesktopPasswordEnvironmentName(cfg.External.Connection.Desktop.PasswordEnv); err != nil {
		return core.Exit(2, "%v", err)
	}
	workRoot := externalWorkRoot(cfg)
	if cfg.TargetOS == core.TargetWindows && cfg.WindowsMode == core.WindowsModeNormal {
		return validateExternalWindowsWorkRoot(workRoot)
	}
	clean := path.Clean(workRoot)
	if !strings.HasPrefix(clean, "/") {
		return core.Exit(2, "external.workRoot %q must resolve to an absolute path", cfg.External.WorkRoot)
	}
	safetyPath := clean
	if cfg.TargetOS == core.TargetMacOS {
		safetyPath = externalMacOSDataVolumePath(clean)
	}
	if cfg.TargetOS == core.TargetWindows && cfg.WindowsMode == core.WindowsModeWSL2 {
		if err := validateExternalWSLMountedDriveWorkRoot(clean); err != nil {
			return err
		}
	}
	if isExternalGenericBroadWorkRoot(safetyPath, cfg.TargetOS == core.TargetMacOS) {
		return core.Exit(2, "external.workRoot %q is too broad; choose a dedicated subdirectory", clean)
	}
	usesPOSIXWorkRoot := cfg.TargetOS == core.TargetLinux || cfg.TargetOS == core.TargetMacOS ||
		(cfg.TargetOS == core.TargetWindows && cfg.WindowsMode == core.WindowsModeWSL2)
	if usesPOSIXWorkRoot && isExternalTargetSpecificBroadWorkRoot(safetyPath, cfg.TargetOS, cfg.WindowsMode) {
		return core.Exit(2, "external.workRoot %q is too broad; choose a dedicated subdirectory", clean)
	}
	if usesPOSIXWorkRoot && isExternalPOSIXHomeWorkRoot(safetyPath, cfg.TargetOS, cfg.WindowsMode) {
		return core.Exit(2, "external.workRoot %q is a home directory; choose a dedicated subdirectory", clean)
	}
	return nil
}

func externalMacOSDataVolumePath(clean string) string {
	const prefix = "/System/Volumes/Data"
	if strings.EqualFold(clean, prefix) {
		return "/"
	}
	if len(clean) > len(prefix) && clean[len(prefix)] == '/' && strings.EqualFold(clean[:len(prefix)], prefix) {
		return clean[len(prefix):]
	}
	return clean
}

func isExternalGenericBroadWorkRoot(clean string, caseInsensitive bool) bool {
	for _, root := range []string{"/", "/Users", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/mnt", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var"} {
		if clean == root || (caseInsensitive && strings.EqualFold(clean, root)) {
			return true
		}
	}
	return false
}

func isExternalTargetSpecificBroadWorkRoot(clean, targetOS, windowsMode string) bool {
	if targetOS == core.TargetMacOS {
		if strings.EqualFold(clean, "/System") || (len(clean) > len("/System") && clean[len("/System")] == '/' && strings.EqualFold(clean[:len("/System")], "/System")) {
			return true
		}
		parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
		if len(parts) == 2 && strings.EqualFold(parts[0], "Volumes") && parts[1] != "" {
			return true
		}
		for _, root := range []string{"/Applications", "/Library", "/Network", "/System", "/Users", "/Volumes", "/cores", "/private", "/private/etc", "/private/tmp", "/private/var"} {
			if strings.EqualFold(clean, root) {
				return true
			}
		}
	}
	if targetOS != core.TargetWindows || windowsMode != core.WindowsModeWSL2 {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) == 1 && strings.EqualFold(parts[0], "mnt") {
		return true
	}
	if len(parts) < 2 || !strings.EqualFold(parts[0], "mnt") || len(parts[1]) != 1 ||
		!((parts[1][0] >= 'a' && parts[1][0] <= 'z') || (parts[1][0] >= 'A' && parts[1][0] <= 'Z')) {
		return false
	}
	return len(parts) == 2 || (len(parts) >= 3 && isExternalWindowsProtectedRoot(parts[2])) ||
		(len(parts) == 3 && strings.EqualFold(parts[2], "Users"))
}

func isExternalPOSIXHomeWorkRoot(clean, targetOS, windowsMode string) bool {
	caseInsensitive := targetOS == core.TargetMacOS || (targetOS == core.TargetWindows && windowsMode == core.WindowsModeWSL2)
	if targetOS == core.TargetMacOS && (strings.EqualFold(clean, "/var/root") || strings.EqualFold(clean, "/private/var/root")) {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) == 2 && parts[1] != "" {
		root := parts[0]
		if root == "Users" || root == "home" ||
			(caseInsensitive && (strings.EqualFold(root, "Users") || strings.EqualFold(root, "home"))) {
			return true
		}
	}
	return targetOS == core.TargetWindows && windowsMode == core.WindowsModeWSL2 && len(parts) == 4 &&
		strings.EqualFold(parts[0], "mnt") && len(parts[1]) == 1 &&
		((parts[1][0] >= 'a' && parts[1][0] <= 'z') || (parts[1][0] >= 'A' && parts[1][0] <= 'Z')) &&
		strings.EqualFold(parts[2], "Users") && parts[3] != ""
}

func validateLifecycleOperation(name string, operation core.ExternalLifecycleOperation) error {
	if len(operation.Argv) > 0 && len(operation.Steps) > 0 {
		return core.Exit(2, "external.lifecycle.%s configures both argv and steps", name)
	}
	switch name {
	case "acquire", "resolve":
		if operation.Output != lifecycleOutputNone && operation.Output != lifecycleOutputJSONLease {
			return core.Exit(2, "external.lifecycle.%s.output must be %q when configured", name, lifecycleOutputJSONLease)
		}
	case "list":
		if operation.Output != lifecycleOutputNone && operation.Output != lifecycleOutputJSONNameArray && operation.Output != lifecycleOutputJSONLeaseArray {
			return core.Exit(2, "external.lifecycle.list.output must be %q or %q", lifecycleOutputJSONNameArray, lifecycleOutputJSONLeaseArray)
		}
	default:
		if operation.Output != lifecycleOutputNone {
			return core.Exit(2, "external.lifecycle.%s.output is unsupported", name)
		}
	}
	if name != "list" && operation.NamePrefix != "" {
		return core.Exit(2, "external.lifecycle.%s.namePrefix is only supported for list", name)
	}
	if operation.NamePrefix != "" && operation.Output != lifecycleOutputJSONNameArray {
		return core.Exit(2, "external.lifecycle.list.namePrefix requires output %q", lifecycleOutputJSONNameArray)
	}
	switch operation.Output {
	case lifecycleOutputNone, lifecycleOutputJSONLease, lifecycleOutputJSONNameArray, lifecycleOutputJSONLeaseArray:
	default:
		return core.Exit(2, "external.lifecycle.%s.output %q is unsupported", name, operation.Output)
	}
	if operation.RollbackOnFailure && name != "acquire" {
		return core.Exit(2, "external.lifecycle.%s.rollbackOnFailure is only supported for acquire", name)
	}
	commands := lifecycleOperationCommands(operation)
	if operation.Output != lifecycleOutputNone && len(commands) == 0 {
		return core.Exit(2, "external.lifecycle.%s.output requires argv or steps", name)
	}
	if operation.RollbackOnFailure && len(commands) < 2 {
		return core.Exit(2, "external.lifecycle.acquire.rollbackOnFailure requires at least two steps")
	}
	for commandIndex, command := range commands {
		label := "argv"
		if len(operation.Steps) > 0 {
			label = fmt.Sprintf("steps[%d]", commandIndex)
		}
		if len(command) == 0 {
			return core.Exit(2, "external.lifecycle.%s.%s is empty", name, label)
		}
		for index, arg := range command {
			if strings.ContainsRune(arg, '\x00') {
				return core.Exit(2, "external.lifecycle.%s.%s[%d] contains a NUL byte", name, label, index)
			}
		}
		if strings.TrimSpace(command[0]) == "" {
			return core.Exit(2, "external.lifecycle.%s.%s executable is empty", name, label)
		}
	}
	return nil
}

func externalWorkRoot(cfg core.Config) string {
	providerRoot := strings.TrimSpace(cfg.External.WorkRoot)
	if providerRoot == "" || (providerRoot == core.BaseConfig().External.WorkRoot && !core.ExternalRoutingLoaded(cfg.External)) {
		if workRoot := strings.TrimSpace(cfg.WorkRoot); workRoot != "" {
			return workRoot
		}
	}
	return core.Blank(providerRoot, "/workspaces/crabbox")
}

func restoreRoutingTarget(cfg *core.Config, fs *flag.FlagSet) {
	targetOS, windowsMode := core.ExternalRoutingTarget(cfg.External)
	if !core.FlagWasSet(fs, "target") {
		cfg.TargetOS = targetOS
	}
	if !core.FlagWasSet(fs, "windows-mode") {
		cfg.WindowsMode = core.WindowsModeNormal
		if cfg.TargetOS == core.TargetWindows {
			cfg.WindowsMode = windowsMode
		}
	}
}

func markRestoredRoutingTargetSources(cfg *core.Config, fs *flag.FlagSet) {
	targetFlag := core.FlagWasSet(fs, "target")
	windowsModeRestored := !core.FlagWasSet(fs, "windows-mode")
	if targetFlag {
		target := *cfg
		if value := fs.Lookup("target"); value != nil {
			target.TargetOS = value.Value.String()
			core.NormalizeTargetConfig(&target)
			if target.TargetOS != core.TargetWindows {
				windowsModeRestored = false
			}
		}
	}
	core.MarkExternalRoutingTargetRestored(cfg, !targetFlag, windowsModeRestored)
}

func validateExternalWindowsWorkRoot(value string) error {
	value = strings.ReplaceAll(strings.TrimSpace(value), "/", `\`)
	if len(value) < 3 || value[1] != ':' || value[2] != '\\' {
		return core.Exit(2, "external.workRoot %q must resolve to an absolute Windows path like C:\\crabbox", value)
	}
	drive := value[0]
	if !((drive >= 'A' && drive <= 'Z') || (drive >= 'a' && drive <= 'z')) {
		return core.Exit(2, "external.workRoot %q must start with a Windows drive letter like C:\\crabbox", value)
	}
	parts := make([]string, 0)
	for _, part := range strings.Split(value[3:], `\`) {
		switch part {
		case "":
			continue
		case ".", "..":
			return core.Exit(2, "external.workRoot %q must not contain relative Windows path components", value)
		}
		if err := validateWindowsWorkRootComponent(value, part); err != nil {
			return err
		}
		parts = append(parts, part)
	}
	clean := strings.ToUpper(value[:1]) + `:\` + strings.Join(parts, `\`)
	if len(parts) == 0 {
		return core.Exit(2, "external.workRoot %q is too broad; choose a dedicated subdirectory", clean)
	}
	if isExternalWindowsProtectedRoot(parts[0]) {
		return core.Exit(2, "external.workRoot %q is inside a protected Windows directory; choose a dedicated workspace", clean)
	}
	if len(parts) <= 2 && strings.EqualFold(parts[0], "Users") {
		return core.Exit(2, "external.workRoot %q is too broad; choose a dedicated user workspace", clean)
	}
	switch strings.ToUpper(clean) {
	case `C:\WINDOWS`, `C:\PROGRAM FILES`, `C:\PROGRAM FILES (X86)`:
		return core.Exit(2, "external.workRoot %q is too broad; choose a dedicated subdirectory", clean)
	}
	return nil
}

func isExternalWindowsProtectedRoot(component string) bool {
	switch strings.ToUpper(component) {
	case "WINDOWS", "WINNT", "PROGRAM FILES", "PROGRAM FILES (X86)", "PROGRAMDATA", "$RECYCLE.BIN", "SYSTEM VOLUME INFORMATION", "DOCUMENTS AND SETTINGS":
		return true
	default:
		return false
	}
}

func validateExternalWSLMountedDriveWorkRoot(clean string) error {
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) < 2 || !strings.EqualFold(parts[0], "mnt") || len(parts[1]) != 1 ||
		!((parts[1][0] >= 'a' && parts[1][0] <= 'z') || (parts[1][0] >= 'A' && parts[1][0] <= 'Z')) {
		return nil
	}
	for _, part := range parts[2:] {
		if err := validateWindowsWorkRootComponent(clean, part); err != nil {
			return err
		}
	}
	return nil
}

func validateWindowsWorkRootComponent(value, part string) error {
	if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
		return core.Exit(2, "external.workRoot %q contains a Windows-ambiguous path component", value)
	}
	if windowsShortNameComponent(part) {
		return core.Exit(2, "external.workRoot %q must not use Windows short-name aliases", value)
	}
	if strings.ContainsAny(part, `<>:"|?*`) {
		return core.Exit(2, "external.workRoot %q contains an invalid Windows path component", value)
	}
	for _, character := range part {
		if character < 0x20 {
			return core.Exit(2, "external.workRoot %q contains an invalid Windows path component", value)
		}
	}
	base := strings.ToUpper(part)
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	if windowsReservedDeviceBase(base) {
		return core.Exit(2, "external.workRoot %q contains a reserved Windows device name", value)
	}
	return nil
}

func windowsReservedDeviceBase(base string) bool {
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || base == "CONIN$" || base == "CONOUT$" {
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return true
	}
	for _, digit := range []string{"¹", "²", "³"} {
		if base == "COM"+digit || base == "LPT"+digit {
			return true
		}
	}
	return false
}

func windowsShortNameComponent(part string) bool {
	tilde := strings.LastIndex(part, "~")
	if tilde < 1 || tilde == len(part)-1 {
		return false
	}
	suffix := part[tilde+1:]
	if dot := strings.IndexByte(suffix, '.'); dot >= 0 {
		suffix = suffix[:dot]
	}
	if suffix == "" {
		return false
	}
	for _, character := range suffix {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
