package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func (a App) copyCommand(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("cp", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpAll())
	id := fs.String("id", "", "lease id or slug")
	followLink := fs.Bool("L", false, "follow symbolic links when copying from host to sandbox")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if strings.TrimSpace(*id) == "" || fs.NArg() != 2 {
		return exit(2, "usage: crabbox cp --id <lease-id-or-slug> [-L] <src> <dst>")
	}
	if err := validateCopyArgs(fs.Arg(0), fs.Arg(1)); err != nil {
		return err
	}
	cfg, err := loadPortsConfig(fs, *provider, providerFlags, targetFlags, *id)
	if err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	copyBackend, ok := backend.(CopyBackend)
	if ok {
		return copyBackend.Copy(ctx, CopyRequest{
			Options:     leaseOptionsFromConfig(cfg),
			ID:          *id,
			Source:      fs.Arg(0),
			Destination: fs.Arg(1),
			FollowLink:  *followLink,
		})
	}
	if _, ok := backend.(SSHLeaseBackend); !ok {
		return exit(2, "provider=%s does not support cp; it has neither native copy nor an SSH lease transport", backend.Spec().Name)
	}
	lease, err := a.resolveSSHTransportLeaseTargetForRepo(ctx, &cfg, *id, true, false)
	if err != nil {
		return err
	}
	if err := a.claimAndTouchLeaseTarget(ctx, cfg, lease.Server, lease.SSH, lease.LeaseID, false); err != nil {
		return err
	}
	if err := a.probeSSHTransportLeaseAfterClaim(ctx, cfg, &lease, false); err != nil {
		return err
	}
	stopActivity := a.startInteractiveSSHLeaseActivity(ctx, cfg, lease)
	defer stopActivity()
	return copyOverResolvedSSH(ctx, lease.SSH, fs.Arg(0), fs.Arg(1), *followLink, a.Stdout, a.Stderr)
}

func validateCopyArgs(src, dst string) error {
	srcSandbox := isSandboxCopyArg(src)
	dstSandbox := isSandboxCopyArg(dst)
	if srcSandbox == dstSandbox {
		return exit(2, "usage: crabbox cp --id <lease-id-or-slug> [-L] <src> <dst> (exactly one path must use SANDBOX:PATH)")
	}
	return nil
}

func isSandboxCopyArg(value string) bool {
	prefix := value
	if idx := strings.IndexByte(value, ':'); idx >= 0 {
		prefix = value[:idx]
	}
	return strings.EqualFold(strings.TrimSpace(prefix), "SANDBOX") && strings.Contains(value, ":")
}

func copyOverResolvedSSH(ctx context.Context, target SSHTarget, src, dst string, followLink bool, stdout, stderr anyWriter) (err error) {
	if isWindowsNativeTarget(target) {
		return exit(2, "SSH cp over rsync is not available for native Windows targets; use a provider-native copy backend or a WSL2 target")
	}
	terminationCtx, stopTerminationSignals := pondMeshTerminationContext(ctx)
	defer stopTerminationSignals()
	ctx = terminationCtx
	session, wslExe, mountRoot, capabilities, err := newResolvedSSHCopySession(ctx, target)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	if ctxErr := context.Cause(ctx); ctxErr != nil {
		return ctxErr
	}
	if !capabilities.safeTransport {
		version := capabilities.version
		if version == "" {
			version = "unknown"
		}
		return exit(2, "SSH cp requires rsync 3.4.3 or newer for secure transfers; found %s", version)
	}
	// Prefer secluded arguments whenever the remote rsync supports them: the
	// paths then travel over the rsync protocol stream instead of the remote
	// shell command line. Shell-transported filename args are where rsync
	// clients apply wildcard escaping, and rsync 3.4.4's safe_arg() leaves one
	// uninitialized heap byte after a backslash-escaped wildcard (our escaped
	// "\[" and friends), intermittently corrupting the remote path. WSL2
	// targets still hard-require secluded support; other targets fall back to
	// shell-transported args so remotes without secluded support (for example
	// macOS openrsync) keep working.
	secludedArgs := isWindowsWSL2Target(target)
	if secludedArgs {
		if probeErr := probeResolvedSSHRemoteSecludedArgs(ctx, session, target, wslExe); probeErr != nil {
			if ctxErr := context.Cause(ctx); ctxErr != nil {
				return ctxErr
			}
			return exit(2, "SSH cp to WSL2 requires remote rsync support for secluded arguments")
		}
	} else if probeErr := probeResolvedSSHRemoteSecludedArgs(ctx, session, target, wslExe); probeErr == nil {
		secludedArgs = true
	} else if ctxErr := context.Cause(ctx); ctxErr != nil {
		return ctxErr
	}
	args, err := resolvedSSHCopyArgs(session, target, src, dst, followLink, secludedArgs)
	if err != nil {
		return err
	}
	name := "rsync"
	commandArgs := args
	if runtime.GOOS == "windows" {
		if wslExe != "" {
			name = wslExe
			commandArgs = append([]string{"rsync"}, resolvedSSHCopyWSLArgs(args, mountRoot)...)
		}
	}
	handle := pondMeshExecCommand(ctx, target.ChildEnvDenylist, name, commandArgs...)
	stderrTail := newSynchronizedTailBuffer(failureTailLines)
	if execHandle, ok := handle.(*pondMeshExecHandle); ok {
		execHandle.cmd.Stdout = stdout
		execHandle.cmd.Stderr = stderrTail
		if runtime.GOOS == "windows" && wslExe == "" {
			if execHandle.cmd.Env == nil {
				execHandle.cmd.Env = os.Environ()
			}
			execHandle.cmd.Env = append(execHandle.cmd.Env, "MSYS2_ARG_CONV_EXCL=*", "MSYS_NO_PATHCONV=1", "CYGWIN=nodosfilewarning")
		}
	}
	if err := handle.Start(); err != nil {
		return fmt.Errorf("start copy over resolved SSH transport: %w", err)
	}
	waitErr := handle.Wait()
	writeSSHTransportDiagnostic(stderr, target, stderrTail.String())
	if waitErr != nil {
		if ctxErr := context.Cause(ctx); ctxErr != nil {
			if handle.WasTerminatedByOurCancel() {
				return ctxErr
			}
		}
		return fmt.Errorf("copy over resolved SSH transport: %w", waitErr)
	}
	return nil
}

func resolvedSSHCopyWSLArgs(args []string, mountRoot string) []string {
	wslArgs := append([]string(nil), args...)
	operands := false
	for index, arg := range wslArgs {
		if arg == "--" {
			operands = true
			continue
		}
		if operands && !strings.HasPrefix(arg, sshTransportHostAlias+":") {
			wslArgs[index] = windowsToWSLPathWithRoot(arg, mountRoot)
		}
	}
	return wslArgs
}

func probeResolvedSSHRemoteSecludedArgs(ctx context.Context, session *sshTransportSession, target SSHTarget, wslExe string) error {
	// --protect-args is the pre-3.2.6 spelling of --secluded-args, so the
	// probe also accepts older remote rsyncs that predate the rename.
	remoteProbe := "rsync --protect-args --version"
	if isWindowsWSL2Target(target) {
		remoteProbe = "wsl.exe " + remoteProbe
	}
	args := append(session.commandPrefix(), "-n", session.host(), remoteProbe)
	name := "ssh"
	if wslExe != "" {
		name = wslExe
		args = append([]string{"ssh"}, args...)
	}
	handle := pondMeshExecCommand(ctx, target.ChildEnvDenylist, name, args...)
	if execHandle, ok := handle.(*pondMeshExecHandle); ok {
		execHandle.cmd.Stdout = io.Discard
		execHandle.cmd.Stderr = io.Discard
	}
	if err := handle.Start(); err != nil {
		return err
	}
	err := handle.Wait()
	if ctxErr := context.Cause(ctx); ctxErr != nil {
		if handle.WasTerminatedByOurCancel() {
			return ctxErr
		}
	}
	return err
}

type resolvedRsyncCapabilities struct {
	version       string
	safeTransport bool
}

func resolvedSSHCopyRsyncCapabilities(ctx context.Context, target SSHTarget, wslExe string) resolvedRsyncCapabilities {
	name := "rsync"
	prefix := []string(nil)
	if runtime.GOOS == "windows" && wslExe != "" {
		name = wslExe
		prefix = []string{"rsync"}
	}
	capabilities := resolvedRsyncCapabilities{}
	versionArgs := append(append([]string{}, prefix...), "--version")
	versionCommand := exec.CommandContext(ctx, name, versionArgs...)
	applyTargetChildEnvironment(versionCommand, target)
	if output, err := versionCommand.CombinedOutput(); err == nil {
		major, minor, patch, version, ok := parseRsyncVersion(string(output))
		if ok {
			capabilities.version = version
			capabilities.safeTransport = rsyncVersionAtLeast(major, minor, patch, 3, 4, 3)
		}
	}
	return capabilities
}

func parseRsyncVersion(output string) (int, int, int, string, bool) {
	fields := strings.Fields(output)
	for index := 0; index+2 < len(fields); index++ {
		if fields[index] != "rsync" || fields[index+1] != "version" {
			continue
		}
		version := strings.TrimSpace(fields[index+2])
		var major, minor, patch int
		var suffix string
		count, _ := fmt.Sscanf(version, "%d.%d.%d%s", &major, &minor, &patch, &suffix)
		if count < 3 {
			continue
		}
		if suffix != "" {
			lower := strings.ToLower(suffix)
			packagingSuffix := strings.HasPrefix(suffix, "-") || strings.HasPrefix(suffix, "+")
			prerelease := strings.Contains(lower, "pre") || strings.Contains(lower, "rc") ||
				strings.Contains(lower, "dev") || strings.Contains(lower, "alpha") || strings.Contains(lower, "beta")
			if !packagingSuffix || prerelease {
				return 0, 0, 0, version, false
			}
		}
		return major, minor, patch, version, true
	}
	return 0, 0, 0, "", false
}

func rsyncVersionAtLeast(major, minor, patch, wantMajor, wantMinor, wantPatch int) bool {
	if major != wantMajor {
		return major > wantMajor
	}
	if minor != wantMinor {
		return minor > wantMinor
	}
	return patch >= wantPatch
}

func writeSSHTransportDiagnostic(writer anyWriter, target SSHTarget, value string) {
	value = strings.TrimSpace(redactSSHTransportDiagnostic(target, value))
	if value != "" {
		fmt.Fprintln(writer, value)
	}
}

func redactSSHTransportDiagnostic(target SSHTarget, value string) string {
	if target.AuthSecret {
		return RedactDiagnosticSecrets(value, target.User)
	}
	return RedactDiagnosticSecrets(value)
}

func newResolvedSSHCopySession(ctx context.Context, target SSHTarget) (*sshTransportSession, string, string, resolvedRsyncCapabilities, error) {
	if !sshCopyUsesWSL(runtime.GOOS, target) {
		session, err := newSSHTransportSession(ctx, target, false)
		return session, "", "", resolvedSSHCopyRsyncCapabilities(ctx, target, ""), err
	}
	wslExe, ok := windowsRsyncWSLExecutable(ctx, target)
	if !ok {
		session, err := newSSHTransportSession(ctx, target, false)
		return session, "", "", resolvedSSHCopyRsyncCapabilities(ctx, target, ""), err
	}
	wslCapabilities := resolvedSSHCopyRsyncCapabilities(ctx, target, wslExe)
	if !wslCapabilities.safeTransport {
		nativeCapabilities := resolvedSSHCopyRsyncCapabilities(ctx, target, "")
		if preferNativeResolvedRsync(wslCapabilities, nativeCapabilities) {
			session, err := newSSHTransportSession(ctx, target, false)
			return session, "", "", nativeCapabilities, err
		}
	}
	mountRoot := windowsWSLMountRoot(ctx, target, wslExe)
	session, err := newWSLSSHTransportSession(ctx, target, wslExe, mountRoot)
	return session, wslExe, mountRoot, wslCapabilities, err
}

func preferNativeResolvedRsync(wsl, native resolvedRsyncCapabilities) bool {
	return !wsl.safeTransport && native.safeTransport
}

func sshCopyUsesWSL(goos string, target SSHTarget) bool {
	return goos == "windows" && !target.SSHConfigProxy
}

func resolvedSSHCopyArgs(session *sshTransportSession, target SSHTarget, src, dst string, followLink, secludedArgs bool) ([]string, error) {
	secludedArgs = secludedArgs || isWindowsWSL2Target(target)
	srcRemote, srcPath := sandboxCopyPath(src)
	dstRemote, dstPath := sandboxCopyPath(dst)
	if srcRemote == dstRemote {
		return nil, exit(2, "copy requires exactly one SANDBOX:PATH")
	}
	if strings.TrimSpace(srcPath) == "" || strings.TrimSpace(dstPath) == "" {
		return nil, exit(2, "copy source and destination paths must not be empty")
	}
	remotePath := dstPath
	if srcRemote {
		remotePath = srcPath
	}
	if strings.ContainsAny(remotePath, "\x00\r\n") {
		return nil, exit(2, "remote copy paths must not contain control characters")
	}
	// Tilde paths need remote-shell expansion. Keep that behavior when the
	// encoded path avoids rsync 3.4.4's safe_arg() bug, and fail closed when a
	// backslash-wildcard pair would re-enter the vulnerable transport path.
	if secludedArgs && !isWindowsWSL2Target(target) && strings.HasPrefix(remotePath, "~") {
		if srcRemote && !strings.Contains(remotePath, "/") {
			return nil, exit(2, "remote downloads from bare ~ or ~user are unsupported; use a path under ~/ or an absolute path")
		}
		if rsyncRemoteCopyPathTriggersSafeArgBug(remotePath) {
			return nil, exit(2, "remote copy paths using ~ must not require rsync wildcard escaping; use an absolute path")
		}
		secludedArgs = false
	}
	args := []string{"-az", "--no-old-args"}
	if srcRemote {
		// A lease is outside the host trust boundary. Preserve regular file and
		// directory contents plus timestamps, but never materialize sender-owned
		// links, special files, ownership, groups, or permission bits locally.
		args = []string{"-rtz", "--no-links", "--no-devices", "--no-specials", "--no-owner", "--no-group", "--no-perms", "--chmod=Du=rwx,Dgo=rx,Fu=rw,Fgo=r", "--no-old-args"}
	}
	if secludedArgs {
		args = append(args, "--secluded-args")
	} else {
		args = append(args, "--no-secluded-args")
	}
	args = append(args, "-e", session.rsyncRemoteShell())
	if isWindowsWSL2Target(target) {
		args = append(args, "--rsync-path", "wsl.exe rsync")
	}
	if followLink && !srcRemote {
		args = append(args, "--copy-links")
	}
	args = append(args, "--")
	if srcRemote {
		remoteSource := rsyncRemoteCopyPath(srcPath)
		if secludedArgs {
			remoteSource = rsyncSecludedRemoteSourcePath(srcPath)
		}
		args = append(args, session.host()+":"+remoteSource, rsyncCopyLocalPath(dstPath))
	} else {
		remoteDestination := rsyncRemoteCopyPath(dstPath)
		if secludedArgs {
			remoteDestination = rsyncSecludedRemoteDestinationPath(dstPath)
		}
		args = append(args, rsyncCopyLocalPath(srcPath), session.host()+":"+remoteDestination)
	}
	return args, nil
}

func rsyncSecludedRemoteDestinationPath(path string) string {
	if strings.HasPrefix(path, ":") {
		return "./" + path
	}
	return path
}

func rsyncSecludedRemoteSourcePath(path string) string {
	components := strings.Split(path, "/")
	for index, component := range components {
		if strings.ContainsAny(component, "*?[") {
			component = strings.ReplaceAll(component, `\`, `\\`)
		}
		components[index] = strings.NewReplacer(
			`*`, `\*`,
			`?`, `\?`,
			`[`, `\[`,
		).Replace(component)
	}
	path = strings.Join(components, "/")
	if strings.HasPrefix(path, ":") {
		return "./" + path
	}
	return path
}

func rsyncRemoteCopyPath(path string) string {
	if strings.ContainsAny(path, "*?[") {
		path = strings.ReplaceAll(path, `\`, `\\`)
	}
	path = strings.NewReplacer(
		`*`, `\*`,
		`?`, `\?`,
		`[`, `\[`,
	).Replace(path)
	if strings.HasPrefix(path, ":") {
		return "./" + path
	}
	return path
}

func rsyncRemoteCopyPathTriggersSafeArgBug(path string) bool {
	path = rsyncRemoteCopyPath(path)
	for index := 0; index+1 < len(path); index++ {
		if path[index] == '\\' && strings.ContainsRune("*?[]", rune(path[index+1])) {
			return true
		}
	}
	return false
}

func rsyncCopyLocalPath(path string) string {
	return rsyncCopyLocalPathForGOOS(runtime.GOOS, path)
}

func rsyncCopyLocalPathForGOOS(goos, path string) string {
	absolute := filepath.IsAbs(path)
	if goos == "windows" {
		normalized := strings.ReplaceAll(path, `\`, "/")
		absolute = strings.HasPrefix(normalized, "/") ||
			(len(normalized) >= 3 && normalized[1] == ':' && normalized[2] == '/')
	}
	converted := rsyncLocalPathForGOOS(goos, path)
	if converted != "" && !absolute && !strings.HasPrefix(converted, "./") {
		return "./" + converted
	}
	return converted
}

func sandboxCopyPath(value string) (bool, string) {
	if !isSandboxCopyArg(value) {
		return false, value
	}
	_, path, _ := strings.Cut(value, ":")
	return true, path
}
