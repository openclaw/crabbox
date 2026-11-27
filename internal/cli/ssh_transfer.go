package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
)

// OpenRsync stats each listed leaf when deriving implied directories, which
// fails for a dangling symlink. Send the same parents explicitly on macOS.
// Directory names without a trailing slash do not select their contents.
func rsyncFileList(args []string, files []byte, hasSymlinks bool) ([]string, io.Reader) {
	args = append(args, "--files-from=-", "--from0")
	if runtime.GOOS != "darwin" || !hasSymlinks {
		return args, bytes.NewReader(files)
	}
	var parents bytes.Buffer
	seen := make(map[string]bool)
	for name := range bytes.SplitSeq(files, []byte{0}) {
		for {
			end := bytes.LastIndexByte(name, '/')
			if end <= 0 || seen[string(name[:end])] {
				break
			}
			name = name[:end]
			seen[string(name)] = true
			parents.Write(name)
			parents.WriteByte(0)
		}
	}
	return append(args, "--no-implied-dirs"), io.MultiReader(&parents, bytes.NewReader(files))
}

var errOpenRsyncTimeout = errors.New("openrsync poll timeout")

// OpenRsync reports its own protocol timeout before waiting for the SSH child.
// Cancel the owned transfer group on that exact local diagnostic. Keep enough
// stderr across writes to recognize a split line without buffering the log.
type openRsyncTimeoutWriter struct {
	cmd    *exec.Cmd
	cancel context.CancelCauseFunc
	tail   string
}

func (w *openRsyncTimeoutWriter) Write(p []byte) (int, error) {
	line := fmt.Sprintf("\nrsync(%d): error: poll: timeout\n", w.cmd.Process.Pid)
	text := w.tail + string(p)
	if strings.Contains(text, line) {
		w.cancel(errOpenRsyncTimeout)
	}
	w.tail = text[max(0, len(text)-len(line)+1):]
	return len(p), nil
}

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
	handle := pondMeshExecCommand(ctx, target, name, commandArgs...)
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
