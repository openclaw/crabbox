package cli

import (
	"context"
	"runtime"
	"strings"
)

func newWorkspaceRsyncSession(ctx context.Context, target SSHTarget) (*sshTransportSession, string, string, error) {
	wslExe := ""
	if sshTransferUsesWSL(runtime.GOOS, target) {
		wslExe, _ = windowsRsyncWSLExecutable(ctx, target)
	}
	session, mountRoot, err := newRsyncTransportSession(ctx, target, wslExe)
	return session, wslExe, mountRoot, err
}

func sshTransferUsesWSL(goos string, target SSHTarget) bool {
	return goos == "windows" && !target.SSHConfigProxy
}

func newRsyncTransportSession(ctx context.Context, target SSHTarget, wslExe string) (*sshTransportSession, string, error) {
	if wslExe == "" {
		session, err := newSSHTransportSession(ctx, target, false)
		return session, "", err
	}
	mountRoot := windowsWSLMountRoot(ctx, target, wslExe)
	session, err := newWSLSSHTransportSession(ctx, target, wslExe, mountRoot)
	return session, mountRoot, err
}

func resolvedRsyncCommand(ctx context.Context, target SSHTarget, args []string, wslExe, mountRoot string) (*pondMeshExecHandle, error) {
	return resolvedRsyncCommandForGOOS(ctx, runtime.GOOS, target, args, wslExe, mountRoot)
}

func resolvedRsyncCommandForGOOS(ctx context.Context, goos string, target SSHTarget, args []string, wslExe, mountRoot string) (*pondMeshExecHandle, error) {
	name := "rsync"
	commandArgs := args
	nativeWindows := goos == "windows" && wslExe == ""
	if goos == "windows" {
		if wslExe != "" {
			name = wslExe
			commandArgs = append([]string{"rsync"}, resolvedRsyncWSLArgs(args, mountRoot)...)
		} else {
			var err error
			name, commandArgs, err = windowsNativeRsyncCommandSpec(args)
			if err != nil {
				return nil, err
			}
		}
	}
	handle := pondMeshExecCommand(ctx, target.ChildEnvDenylist, name, commandArgs...).(*pondMeshExecHandle)
	applyTargetChildEnvironment(handle.cmd, target)
	if nativeWindows {
		applyWindowsNativeRsyncEnvironment(handle.cmd)
	}
	return handle, nil
}

func resolvedRsyncWSLArgs(args []string, mountRoot string) []string {
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

func resolvedSCPUploadArgs(session *sshTransportSession, target SSHTarget, localPath, remotePath string) []string {
	args := session.commandPrefixWithOptions("10", "3")
	if isWindowsNativeTarget(target) {
		args = append(args, "-O")
	}
	return append(args, "--", localPath, session.host()+":"+remotePath)
}
