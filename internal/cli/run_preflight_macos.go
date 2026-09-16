package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	macOSPlatformPreflightTool = "macos_platform"
	macOSPreflightCommandTime  = 5 * time.Second
	macOSPreflightTotalTime    = 15 * time.Second
)

// The existing owner's watcher remains a living direct child until retirement.
// A relative builtin timeout avoids wall-clock arithmetic and a sleep child.
func macOSPreflightHelper(budget time.Duration) string {
	watch := `exec /bin/zsh -f -c '
directory=$1
: >"$directory/.timer-ready" || exit 74
while [[ ! -e "$directory/.armed" ]]; do
    read -r -t 0.01 -u 8 ignored || :
done
read -r -t "$2" -u 8 ignored || :
: >"$directory/.timed-out" || exit 74
while :; do read -r -t 1 -u 8 ignored || :; done
' sh "$directory" ` + shellQuote(fmt.Sprintf("%.6f", budget.Seconds())) + "\n"
	init := `for ((i=0; i<50; i++)); do
    [ -e "$directory/.timer-ready" ] && break
    kill -0 "$watcher" 2>/dev/null || break
    sleep .1
done
if [ ! -e "$directory/.timer-ready" ] || ! kill -0 "$watcher" 2>/dev/null; then
    : >"$directory/.cancel"
fi
`
	tick := `    if [ "$interrupted" = 1 ] || ! kill -0 "$watcher" 2>/dev/null; then : >"$directory/.cancel"; break; fi
    [ ! -e "$directory/.timed-out" ] || break
`
	// A 100ms owner poll can leave a short diagnostic running beyond its
	// deadline even after the native timer has fired.
	workload := guardedWorkloadScript(init, tick, ".01")
	return posixPreflightHelper(workload, watch)
}

type macOSPreflightProbeResult struct {
	output    string
	elapsed   time.Duration
	ready     bool
	completed bool
}

var runMacOSPreflightProbe = func(ctx context.Context, target SSHTarget, workdir string, env map[string]string, envFiles []string, script string, budget time.Duration) (macOSPreflightProbeResult, error) {
	output := newSynchronizedBuffer(32 << 10)
	result, err := runPOSIXPreflight(ctx, target, macOSPreflightHelper(budget), func(nonce string) string {
		command := remoteShellCommandWithEnvFiles(workdir, env, envFiles, script)
		return macOSPreflightWorker(nonce, "/tmp/crabbox-command-"+nonce+"/scratch", command)
	}, &output, io.Discard)
	err = functionalPreflightWithOwnerError(ctx, err)
	if err != nil {
		return macOSPreflightProbeResult{}, err
	}
	if !result.completion.WorkerQuiesced || !result.completion.ScratchRemoved || !result.completion.StageRetired {
		return macOSPreflightProbeResult{}, errors.New("macOS preflight cleanup unconfirmed")
	}
	if result.completion.State == "timed-out" {
		return macOSPreflightProbeResult{elapsed: budget}, nil
	}
	if result.completion.State != "ready" {
		return macOSPreflightProbeResult{}, errors.New("macOS preflight execution unavailable")
	}
	value, elapsed, code, err := parseMacOSPreflightOutput(output.String(), result.nonce)
	if err != nil {
		return macOSPreflightProbeResult{}, err
	}
	return macOSPreflightProbeResult{output: value, elapsed: min(elapsed, budget), ready: code == 0 && elapsed <= budget, completed: elapsed <= budget}, nil
}

func macOSPreflightWorker(nonce, scratch, command string) string {
	// Only time's report uses the C locale; restore the child's original
	// presence/value before applying its ordinary workload environment.
	child := "if [ \"$1\" = x ]; then export LC_ALL=\"$2\"; else unset LC_ALL; fi\nexec 2>/dev/null\n" + command
	timing := shellQuote(scratch + "/timing")
	probe := shellQuote(scratch + "/probe")
	// SSH stderr also carries supervisor diagnostics. Keep the fixed-size
	// native timing report in owned scratch, then frame it on worker stdout.
	// Drain excess stdout rather than SIGPIPE a successful verbose command.
	// Stage forwarded assignments privately instead of in the outer shell argv.
	return "(umask 077; builtin printf '%s' " + shellQuote(child) + " >" + probe + ") || exit 74\n" +
		"printf 'CBX-MACOS-OUTPUT-1 %s\\n' " + shellQuote(nonce) + "\n" +
		"LC_ALL=C /usr/bin/time -p /bin/bash " + probe + " \"${LC_ALL+x}\" \"${LC_ALL-}\" 2>" + timing +
		" | { /usr/bin/head -c 4096; /bin/cat >/dev/null; }\n" +
		"probe_code=${PIPESTATUS[0]}\nprintf '\\nCBX-MACOS-TIME-1 %s %s\\n' " + shellQuote(nonce) + " \"$probe_code\"\n" +
		"/bin/cat " + timing + " || exit 74\nexit 0\n"
}

func parseMacOSPreflightOutput(record, nonce string) (string, time.Duration, int, error) {
	invalid := errors.New("macOS preflight execution result unavailable")
	header := "CBX-MACOS-OUTPUT-1 " + nonce + "\n"
	if !strings.HasPrefix(record, header) {
		return "", 0, 0, invalid
	}
	content := strings.TrimPrefix(record, header)
	marker := "\nCBX-MACOS-TIME-1 " + nonce + " "
	index := strings.LastIndex(content, marker)
	if index < 0 || index > 4096 {
		return "", 0, 0, invalid
	}
	code, timing, ok := strings.Cut(content[index+len(marker):], "\n")
	if !ok {
		return "", 0, 0, invalid
	}
	elapsed, exitCode, err := parseMacOSPreflightTiming(timing+"CBX-MACOS-TIME-1 "+nonce+" "+code+"\n", nonce)
	return content[:index], elapsed, exitCode, err
}

func parseMacOSPreflightTiming(value, nonce string) (time.Duration, int, error) {
	fields := strings.Fields(value)
	invalid := errors.New("macOS preflight execution timing unavailable")
	if len(fields) != 9 || fields[0] != "real" || fields[2] != "user" || fields[4] != "sys" || fields[6] != "CBX-MACOS-TIME-1" || fields[7] != nonce {
		return 0, 0, invalid
	}
	// Apple time -p truncates at centiseconds. Round up one quantum so a
	// completed probe never gives unaccounted execution time to the next one.
	parts := strings.Split(fields[1], ".")
	if len(parts) != 2 || len(parts[1]) != 2 || strings.Trim(fields[1], "0123456789.") != "" {
		return 0, 0, invalid
	}
	elapsed, err := time.ParseDuration(fields[1] + "s")
	code, codeErr := strconv.Atoi(fields[8])
	if err != nil || elapsed < 0 || elapsed > macOSPreflightTotalTime || codeErr != nil || code < 0 || code > 255 {
		return 0, 0, invalid
	}
	return elapsed + 10*time.Millisecond, code, nil
}

func macOSPreflightValue(value string) string {
	line, _, _ := strings.Cut(value, "\n")
	line = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, line))
	runes := []rune(line)
	if len(runes) > 512 {
		line = string(runes[:512])
	}
	if line == "" {
		return "missing"
	}
	return line
}

func isMacOSPreflightTool(tool string) bool {
	switch tool {
	case macOSPlatformPreflightTool, "swift", "xcodebuild", "brew":
		return true
	}
	return false
}

func printMacOSCapabilityPreflight(ctx context.Context, w io.Writer, target SSHTarget, workdir string, env map[string]string, envFiles, tools []string) error {
	remaining := macOSPreflightTotalTime
	run := func(name, script string) (macOSPreflightProbeResult, string, error) {
		if remaining <= 0 {
			return macOSPreflightProbeResult{}, " reason=budget-exhausted", nil
		}
		probe, err := runMacOSPreflightProbe(ctx, target, workdir, env, envFiles, script, min(remaining, macOSPreflightCommandTime))
		if err != nil {
			return probe, "", fmt.Errorf("macOS preflight %s: %w", name, err)
		}
		remaining -= probe.elapsed
		return probe, "", nil
	}
	for _, tool := range tools {
		if !isMacOSPreflightTool(tool) {
			continue
		}
		if tool == macOSPlatformPreflightTool {
			if err := printMacOSPlatformPreflight(w, run); err != nil {
				return err
			}
			continue
		}
		script := strings.Join(readableShellWords(preflightToolRegistry[tool].Posix), " ")
		if tool == "brew" {
			script = "HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ANALYTICS=1 " + script
		}
		probe, reason, err := run(tool, script)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "remote preflight %s=%s%s\n", tool, probe.value(), reason)
	}
	return nil
}

func (p macOSPreflightProbeResult) value() string {
	if !p.ready {
		return "missing"
	}
	return macOSPreflightValue(p.output)
}

func printMacOSPlatformPreflight(w io.Writer, run func(string, string) (macOSPreflightProbeResult, string, error)) error {
	// Keep one execution allowance per native command, and publish completed
	// fields before attempting a later, potentially unavailable toolchain check.
	for _, query := range []struct{ name, command string }{
		{"macos_version", "/usr/bin/sw_vers -productVersion"},
		{"macos_build", "/usr/bin/sw_vers -buildVersion"},
		{"architecture", "/usr/bin/uname -m"},
		{"developer_directory", `if [ -n "${DEVELOPER_DIR:-}" ]; then printf '%s\n' "$DEVELOPER_DIR"; else /usr/bin/xcode-select -p; fi`},
	} {
		probe, reason, err := run(query.name, query.command)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "remote preflight %s=%s%s\n", query.name, probe.value(), reason)
	}
	clang, reason, err := run("developer_tools", "/usr/bin/xcrun clang --version")
	if err != nil {
		return err
	}
	tools := "unavailable"
	if clang.value() != "missing" {
		xcode, xcodeReason, err := run("developer_tools", "/usr/bin/xcodebuild -version")
		if err != nil {
			return err
		}
		reason = xcodeReason
		if xcode.value() != "missing" {
			tools = "xcode"
		} else if xcode.completed {
			tools = "clt"
		}
	}
	fmt.Fprintf(w, "remote preflight developer_tools=%s%s\n", tools, reason)
	return nil
}
