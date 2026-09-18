package cli

import (
	"errors"
	"path"
	"strconv"
	"time"

	"github.com/openclaw/crabbox/internal/remoteruntime"
)

// The descriptor still contains bounded UTF-8 helper source, not executable
// bytes. Its worker allowance is independent of the Windows transport clock.
func nativeWSLStageProgram(runtime *remoteNativeRuntime, purpose string, workerBudget time.Duration) (wslStageProgram, error) {
	if runtime == nil || !isWindowsWSL2Target(runtime.target) || !runtime.shell.valid() ||
		!path.IsAbs(runtime.path) || path.Clean(runtime.path) != runtime.path ||
		len(runtime.path) > 1024 || !nativeWSLMetadataLiteral(runtime.path) {
		return wslStageProgram{}, errors.New("native WSL stage requires an installed runtime on an established WSL route")
	}
	if workerBudget < 0 || purpose != "command" && purpose != "preflight" || purpose == "preflight" && workerBudget == 0 {
		return wslStageProgram{}, errors.New("invalid native WSL stage purpose or worker allowance")
	}
	milliseconds := workerBudget / time.Millisecond
	if workerBudget%time.Millisecond != 0 {
		milliseconds++
	}
	prefix := shellQuote(runtime.path) + " " + shellQuote(remoteruntime.Command)
	program := `set -eu
[ "$#" -eq 8 ] && [ "$2" = "/tmp/crabbox-command-$3" ] || exit 74
case "$1" in
  run) exec ` + prefix + " run " + shellQuote(remoteruntime.Protocol) + ` "$3" "$4" "$5" "$6" "$7" ` + strconv.FormatInt(int64(milliseconds), 10) + " " + shellQuote(purpose) + ` watched;;
  cleanup) exec ` + prefix + " control " + shellQuote(remoteruntime.Protocol) + ` "$3" cleanup "$8";;
  *) exit 74;;
esac
`
	return wslStageProgram{source: program, bootstrap: wslPOSIXHelperBootstrap, runtime: runtime}, nil
}
