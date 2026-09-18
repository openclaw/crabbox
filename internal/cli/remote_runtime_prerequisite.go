package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

const missingNativeRuntimePackDiagnostic = "native runtime pack is unavailable for this CLI-only installation; install the complete release using Homebrew or extract the platform archive with its crabbox-runtime directory intact"

const legacyBashMissing = "CBX_LEGACY_BASH_MISSING\n"
const legacyBashProbe = "if command -v bash >/dev/null 2>&1; then :; else printf 'CBX_LEGACY_BASH_MISSING\\n'; fi"

// Only internal Bash-dependent supervisors call this prerequisite check. Its
// result never comes from the user command or that command's exit status.
func requireLegacyBash(ctx context.Context, target SSHTarget, shell wslStageShell) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	scope, _ := ctx.Value(nativeRuntimeScopeKey{}).(*nativeRuntimeScope)
	key := nativeRuntimeInstallKey{nativeRuntimeLease(ctx), nativeRuntimeRouteKey(target)}
	if scope != nil {
		if _, err := scope.prepared(ctx, target); err != nil && !errors.Is(err, errNativeRuntimeUnprepared) {
			return err
		}
		scope.mu.Lock()
		ready := scope.legacyBashReady[key]
		scope.mu.Unlock()
		if ready {
			return context.Cause(ctx)
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := legacyBashProbe
	if isWindowsWSL2Target(target) {
		var err error
		command, err = nativeWSLPOSIXCommand(command, 5*time.Second, shell)
		if err != nil {
			return err
		}
	}
	output := newSynchronizedBuffer(128)
	transport := sshTransportPreparation{command: command}
	if _, err := transport.runOnce(probeCtx, target, "2", "1", &output, io.Discard, false); err != nil {
		return fmt.Errorf("check legacy supervisor Bash prerequisite: %w", err)
	}
	if err := context.Cause(probeCtx); err != nil {
		return err
	}
	if err := legacyBashProbeResult(output.String()); err != nil {
		return err
	}
	if scope != nil {
		if _, err := scope.prepared(ctx, target); err != nil && !errors.Is(err, errNativeRuntimeUnprepared) {
			return err
		}
		scope.mu.Lock()
		if scope.legacyBashReady == nil {
			scope.legacyBashReady = make(map[nativeRuntimeInstallKey]bool)
		}
		scope.legacyBashReady[key] = true
		scope.mu.Unlock()
	}
	return nil
}

func legacyBashProbeResult(output string) error {
	switch output {
	case "":
		return nil
	case legacyBashMissing:
		return errors.New("remote internal supervisor requires Bash, but Bash is unavailable; " + missingNativeRuntimePackDiagnostic)
	default:
		return errors.New("unexpected response checking legacy supervisor Bash prerequisite")
	}
}
