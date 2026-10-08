package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Larger stdin payloads can wedge Windows OpenSSH's per-connection event loop.
const windowsInlineScriptLimit = 4096

func windowsScriptNeedsFileTransport(script string) bool {
	return len(script) > windowsInlineScriptLimit
}

type windowsScriptTransport struct {
	run  func(context.Context, SSHTarget, string, io.Reader, io.Writer, io.Writer) error
	copy func(context.Context, SSHTarget, string, string) error
}

func runWindowsPowerShellScriptToWriter(ctx context.Context, target SSHTarget, script string, stdout io.Writer) error {
	return runGuestWindowsPowerShellScript(ctx, target, script, stdout, nil)
}

func runGuestWindowsPowerShellScript(ctx context.Context, target SSHTarget, script string, stdout, stderr io.Writer) error {
	return (windowsScriptTransport{run: runSSHInput, copy: copyLocalFileToTarget}).execute(ctx, target, script, stdout, stderr)
}

func (transport windowsScriptTransport) execute(ctx context.Context, target SSHTarget, script string, stdout, stderr io.Writer) (result error) {
	var buffered bytes.Buffer
	errWriter := stderr
	if errWriter == nil {
		errWriter = &buffered
	}
	defer func() {
		if result != nil {
			if detail := strings.TrimSpace(buffered.String()); detail != "" {
				result = fmt.Errorf("%w: %s", result, detail)
			}
		}
	}()
	if !windowsScriptNeedsFileTransport(script) {
		return transport.run(ctx, target, windowsPowerShellStdinScriptCommand(len(script)), strings.NewReader(script), stdout, errWriter)
	}

	localDir, err := os.MkdirTemp("", "crabbox-windows-script-")
	if err != nil {
		return Exit(2, "create temp script dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(localDir); err != nil {
			result = errors.Join(result, fmt.Errorf("remove local Windows script: %w", err))
		}
	}()
	localScript := filepath.Join(localDir, "script.ps1")
	// PowerShell 5.1 needs a BOM to decode non-ASCII -File scripts as UTF-8.
	if err := os.WriteFile(localScript, append([]byte{0xEF, 0xBB, 0xBF}, []byte(script)...), 0o600); err != nil {
		return Exit(2, "write Windows script: %v", err)
	}
	remoteDir := `C:\ProgramData\crabbox\scripts\` + filepath.Base(localDir)
	remoteScript := remoteDir + `\script.ps1`
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		command := PowershellCommand(`$ErrorActionPreference = "Stop"
if (Test-Path -LiteralPath ` + psQuote(remoteDir) + `) {
  Remove-Item -Recurse -Force -LiteralPath ` + psQuote(remoteDir) + `
}`)
		if err := transport.run(cleanupCtx, target, command, nil, io.Discard, errWriter); err != nil {
			result = errors.Join(result, fmt.Errorf("remove remote Windows script: %w", err))
		}
	}()
	prepare := PowershellCommand(`$ErrorActionPreference = "Stop"
` + windowsInteractiveScheduledTaskPowerShell() + `
Protect-CrabboxInteractiveDirectory 'C:\ProgramData\crabbox\scripts'
Protect-CrabboxInteractiveDirectory ` + psQuote(remoteDir))
	if err := transport.run(ctx, target, prepare, nil, io.Discard, errWriter); err != nil {
		return fmt.Errorf("prepare private Windows script directory: %w", err)
	}
	if err := transport.copy(ctx, target, localScript, remoteScript); err != nil {
		return fmt.Errorf("copy Windows script: %w", err)
	}
	command := PowershellCommand(`$ErrorActionPreference = "Stop"
& powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File ` + psQuote(remoteScript) + `
exit $LASTEXITCODE`)
	return transport.run(ctx, target, command, nil, stdout, errWriter)
}
