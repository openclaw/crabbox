package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openclaw/crabbox/internal/remoteruntime"
)

const nativeWSLMetadataCommandLimit = 8000

// nativeWSLMetadataCommand renders a metadata call to an already-verified Linux
// runtime in the default WSL distribution. Artifact installation and protocol
// semantics belong to the caller, which supplies the established shell route.
// The launcher inherits stdout/stderr unchanged.
// On timeout it terminates only its wsl.exe process; guest termination is not
// established by killing that launcher.
func nativeWSLMetadataCommand(runtimePath string, args []string, timeout time.Duration, shell wslStageShell) (string, error) {
	if !shell.valid() {
		return "", fmt.Errorf("native WSL metadata requires an established shell route")
	}
	if !path.IsAbs(runtimePath) || path.Clean(runtimePath) != runtimePath || len(runtimePath) > 1024 || !nativeWSLMetadataLiteral(runtimePath) {
		return "", fmt.Errorf("native WSL metadata requires a bounded absolute runtime path")
	}
	if timeout < time.Millisecond || timeout > time.Minute {
		return "", fmt.Errorf("native WSL metadata timeout must be between 1ms and 1m")
	}
	if !(len(args) == 2 && args[0] == remoteruntime.Command && args[1] == "identity") &&
		!(len(args) == 5 && args[0] == remoteruntime.Command && args[1] == "control" && args[4] != "cleanup") &&
		!(len(args) == 6 && args[0] == remoteruntime.Command && args[1] == "control" && args[4] == "cleanup") {
		return "", fmt.Errorf("native WSL metadata accepts only identity or control arguments")
	}
	for _, arg := range args {
		if len(arg) > 256 || !nativeWSLMetadataLiteral(arg) {
			return "", fmt.Errorf("native WSL metadata argument is empty, invalid, or too long")
		}
	}
	argv := append([]string{runtimePath}, args...)
	return nativeWSLProcessCommand(argv, timeout, shell)
}

// This lower transport is only for trusted installer/control plumbing. User
// workloads continue through the command supervisor and its finite input frame.
func nativeWSLPOSIXCommand(script string, timeout time.Duration, shell wslStageShell) (string, error) {
	if len(script) > wslStageMaxHelper || !nativeWSLMetadataLiteral(script) {
		return "", errors.New("native WSL control script must be bounded UTF-8 text")
	}
	return nativeWSLProcessCommand(nativeWSLPOSIXArgs(script), timeout, shell)
}

func nativeWSLPOSIXArgs(script string) []string {
	return []string{"/usr/bin/env", "BASH_ENV=/dev/null", "ENV=/dev/null", "/bin/sh", "-c", script}
}

func nativeWSLExecArguments(argv []string) string {
	// WSL parses its own options as raw text; quoting --exec selects the
	// default shell instead. Only the Linux executable and argv use CRT quoting.
	return "--exec " + quoteWindowsCommandArgs(argv)
}

func nativeWSLProcessCommand(argv []string, timeout time.Duration, shell wslStageShell) (string, error) {
	if !shell.valid() || timeout < time.Millisecond || timeout > nativeWSLUploadMaxWait {
		return "", errors.New("native WSL process requires an established shell and bounded Windows wait")
	}
	command := wslStagePowerShellCommand(nativeRuntimeMetadataProcessScript("wsl.exe", nativeWSLExecArguments(argv), timeout), shell)
	if len(command) > nativeWSLMetadataCommandLimit {
		return "", fmt.Errorf("native WSL metadata command exceeds %d bytes", nativeWSLMetadataCommandLimit)
	}
	return command, nil
}

const nativeWSLRouteMarker = "CBX_WSL_NATIVE_ROUTE_V1"

// Discover the parent SSH shell without creating a Windows stage or depending
// on a Linux shell, SFTP, or an installed runtime. The nonce binds this response
// to this invocation, not to a cached route from a previous connection.
func discoverNativeWSLShell(ctx context.Context, target SSHTarget) (wslStageShell, error) {
	nonce, err := randomHex(16)
	if err != nil {
		return "", err
	}
	script := "$ErrorActionPreference='Stop'\ntry {\n" + wslStageShellDiscoveryScript +
		"  [Console]::Out.WriteLine(\"" + nativeWSLRouteMarker + " " + nonce + " $shell\")\n" +
		"} catch { [Console]::Error.WriteLine('WSL runtime route discovery failed'); exit 74 }"
	transport := sshTransportPreparation{command: wslStagePowerShellCommand(script, wslStageCMD)}
	output := newSynchronizedBuffer(256)
	if _, err := transport.runOnce(ctx, target, "2", "1", &output, io.Discard, false); err != nil {
		return "", fmt.Errorf("discover native WSL shell: %w", err)
	}
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	return parseNativeWSLShell(output.String(), nonce)
}

func parseNativeWSLShell(output, nonce string) (wslStageShell, error) {
	fields := strings.Fields(output)
	if len(fields) != 3 || fields[0] != nativeWSLRouteMarker || fields[1] != nonce || !wslStageShell(fields[2]).valid() {
		return "", errors.New("invalid native WSL shell response")
	}
	return wslStageShell(fields[2]), nil
}

func nativeWSLMetadataLiteral(value string) bool {
	return value != "" && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func nativeRuntimeMetadataProcessScript(executable, arguments string, timeout time.Duration) string {
	return `$ErrorActionPreference='Stop'
$p=$null
$code=125
try {
  $i=[Diagnostics.ProcessStartInfo]::new(` + psQuote(executable) + `)
  $i.UseShellExecute=$false
  $i.Arguments=` + psQuote(arguments) + `
  $p=[Diagnostics.Process]::Start($i)
  if (!$p.WaitForExit(` + strconv.FormatInt(timeout.Milliseconds(), 10) + `)) {
    [Console]::Error.WriteLine('WSL metadata launcher timed out')
    $code=124
  } else { $code=$p.ExitCode }
} catch {
  [Console]::Error.WriteLine('WSL metadata launcher failed')
  $code=125
} finally {
  if ($null -ne $p) {
    try {
      if (!$p.HasExited) {
        $p.Kill()
        if (!$p.WaitForExit(1000)) {
          [Console]::Error.WriteLine('WSL metadata launcher did not exit after termination')
          $code=125
        }
      }
    } catch {
      [Console]::Error.WriteLine('WSL metadata launcher termination failed')
      $code=125
    } finally { $p.Dispose() }
  }
}
exit $code`
}
