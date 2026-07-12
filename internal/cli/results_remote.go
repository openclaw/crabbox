package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	resultFileMarker           = "__CRABBOX_RESULT_FILE__:"
	resultWarningMarker        = "__CRABBOX_RESULT_WARNING__:"
	remoteResultsMarker        = "crabbox/results-start"
	autoJUnitMaxFiles          = 50
	autoJUnitMaxBytes          = 16 << 20
	autoJUnitMaxTotalBytes     = 64 << 20
	autoJUnitSniffBytes        = 4 << 10
	autoJUnitFailureSniffBytes = 1 << 20
)

func collectRemoteJUnitResults(ctx context.Context, target SSHTarget, workdir string, cfg ResultsConfig, autoMarker string) (*TestResultSummary, error) {
	paths := appendUniqueStrings(nil, cfg.JUnit...)
	files := map[string]string{}
	var warnings []error
	if len(paths) > 0 {
		remote := remoteReadResultFiles(workdir, paths)
		if isWindowsNativeTarget(target) {
			remote = windowsRemoteReadResultFiles(workdir, paths)
		}
		out, err := runSSHOutput(ctx, target, remote)
		if err != nil {
			return nil, err
		}
		files = parseMarkedFiles(out)
	}
	if cfg.Auto {
		autoFiles, autoWarnings, err := collectRemoteJUnitResultFilesAuto(ctx, target, workdir, autoMarker)
		if err != nil {
			return nil, err
		}
		warnings = append(warnings, autoWarnings...)
		seen := map[string]bool{}
		for name := range files {
			seen[normalizeResultPath(workdir, name)] = true
		}
		for name, data := range autoFiles {
			key := normalizeResultPath(workdir, name)
			if !seen[key] {
				files[name] = data
				seen[key] = true
			}
		}
	}
	if len(files) == 0 {
		return nil, errors.Join(warnings...)
	}
	summary, parseErr := parseJUnitResults(files)
	warnings = append(warnings, parseErr)
	return summary, errors.Join(warnings...)
}

func collectRemoteJUnitResultFilesAuto(ctx context.Context, target SSHTarget, workdir, marker string) (map[string]string, []error, error) {
	remote := remoteFindJUnitResultFiles(workdir, marker)
	if isWindowsNativeTarget(target) {
		remote = windowsRemoteFindJUnitResultFiles(workdir, marker)
	}
	out, err := runSSHOutput(ctx, target, remote)
	if err != nil {
		return nil, nil, err
	}
	files, warnings := parseMarkedResultOutput(out)
	return files, warnings, nil
}

func remoteReadResultFiles(workdir string, paths []string) string {
	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(shellQuote(workdir))
	b.WriteString(" && ")
	b.WriteString(`root=$(pwd -P) || exit 0; crabbox_resolve_file() { p=$1; hops=0; while [ "$hops" -lt 40 ]; do dir=${p%/*}; base=${p##*/}; [ -n "$dir" ] || dir=/; dir=$(cd -P "$dir" 2>/dev/null && pwd -P) || return 1; p=$dir/$base; if [ -L "$p" ]; then target=$(readlink "$p") || return 1; case "$target" in /*) p=$target;; *) p=$dir/$target;; esac; hops=$((hops + 1)); continue; fi; [ -f "$p" ] || return 1; printf '%s\n' "$p"; return 0; done; return 1; }; `)
	b.WriteString(`crabbox_read_file() { f=$1; case "$f" in /*) candidate=$f;; *) candidate=$root/$f;; esac; [ -f "$candidate" ] || return; exec 3<"$candidate" 2>/dev/null || return; resolved=$(crabbox_resolve_file "$candidate") || return; if [ "$root" != / ]; then case "$resolved" in "$root"/*) ;; *) return;; esac; fi; if [ -e /proc/self/fd/3 ]; then opened=$(readlink /proc/self/fd/3); elif command -v lsof >/dev/null 2>&1; then pidfile=$(mktemp) || return; sh -c 'echo $PPID > "$1"' sh "$pidfile" || { rm -f "$pidfile"; return; }; pid=$(cat "$pidfile"); rm -f "$pidfile"; opened=$(lsof -a -p "$pid" -d 3 -Fn 2>/dev/null | sed -n 's/^n//p'); else return; fi; [ "$opened" = "$resolved" ] || return; printf '\n`)
	b.WriteString(resultFileMarker)
	b.WriteString(`%s\n' "$f"; cat <&3; }; `)
	b.WriteString("for f in")
	for _, path := range paths {
		b.WriteByte(' ')
		b.WriteString(shellQuote(path))
	}
	b.WriteString(`; do crabbox_read_file "$f" & reader=$!; ( sleep 5; kill "$reader" 2>/dev/null ) >/dev/null 2>&1 & guard=$!; wait "$reader" 2>/dev/null || :; kill "$guard" 2>/dev/null || :; wait "$guard" 2>/dev/null || :; done`)
	return b.String()
}

func remoteTouchResultsMarker(workdir string) string {
	return "cd " + shellQuote(workdir) + " && marker=.crabbox/results-start; if git_marker=$(git rev-parse --git-path " + shellQuote(remoteResultsMarker) + " 2>/dev/null); then marker=$git_marker; fi; mkdir -p \"$(dirname \"$marker\")\" && : > \"$marker\""
}

func remoteFindJUnitResultFiles(workdir, marker string) string {
	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(shellQuote(workdir))
	b.WriteString(" && { ")
	b.WriteString("tmp=$(mktemp) || exit 0; trap 'rm -f \"$tmp\"' EXIT; count=0; total=0; ")
	if strings.TrimSpace(marker) != "" {
		b.WriteString("marker=.crabbox/results-start; if git_marker=$(git rev-parse --git-path ")
		b.WriteString(shellQuote(marker))
		b.WriteString(" 2>/dev/null); then marker=$git_marker; fi; [ -f \"$marker\" ] || exit 0; ")
	}
	b.WriteString(`find . \( -path './node_modules' -o -path '*/node_modules' -o -path './.git' -o -path '*/.git' \) -prune -o -type f \( -name 'junit*.xml' -o -name 'TEST-*.xml' -o -name 'results.xml' \)`)
	b.WriteString(` -print 2>/dev/null | sort > "$tmp"; for want_failed in 1 0; do while IFS= read -r f; do `)
	if strings.TrimSpace(marker) != "" {
		b.WriteString(`if [ "$marker" -nt "$f" ]; then continue; fi; `)
	}
	b.WriteString(fmt.Sprintf(`dd if="$f" bs=%d count=1 2>/dev/null | grep -Eq '<testsuites?' || continue; has_failed=0; dd if="$f" bs=%d count=1 2>/dev/null | grep -Eq '<(failure|error)([[:space:]>])' && has_failed=1; if [ "$want_failed" != "$has_failed" ]; then continue; fi; count=$((count + 1)); if [ "$count" -gt %d ]; then break 2; fi; size=$(wc -c < "$f" 2>/dev/null | tr -d '[:space:]') || continue; case "$size" in ''|*[!0-9]*) continue;; esac; if [ "$size" -gt %d ]; then printf '\n%s%%s\treport exceeds %d-byte per-file limit\n' "$f"; continue; fi; if [ $((total + size)) -gt %d ]; then printf '\n%s%%s\treport exceeds remaining %d-byte aggregate limit\n' "$f"; continue; fi; total=$((total + size)); `, autoJUnitSniffBytes, autoJUnitFailureSniffBytes, autoJUnitMaxFiles, autoJUnitMaxBytes, resultWarningMarker, autoJUnitMaxBytes, autoJUnitMaxTotalBytes, resultWarningMarker, autoJUnitMaxTotalBytes))
	b.WriteString(`printf '\n`)
	b.WriteString(resultFileMarker)
	b.WriteString(`%s\n' "$f"; cat "$f"; done < "$tmp"; done; }`)
	return b.String()
}

func parseAutoJUnitResults(files map[string]string) (*TestResultSummary, error) {
	return parseJUnitResults(filterAutoJUnitFiles(files))
}

func filterAutoJUnitFiles(files map[string]string) map[string]string {
	valid := map[string]string{}
	for name, data := range files {
		parsed, err := parseJUnitResults(map[string]string{name: data})
		if err == nil && parsed != nil {
			valid[name] = data
		}
	}
	return valid
}

func normalizeResultPath(workdir, name string) string {
	path := strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	path = strings.TrimPrefix(path, "./")
	root := strings.TrimRight(strings.TrimSpace(strings.ReplaceAll(workdir, "\\", "/")), "/")
	if root != "" {
		prefix := root + "/"
		if strings.HasPrefix(path, prefix) {
			path = strings.TrimPrefix(path, prefix)
		} else if len(path) >= len(prefix) && strings.EqualFold(path[:len(prefix)], prefix) {
			path = path[len(prefix):]
		}
	}
	return strings.TrimPrefix(path, "./")
}

func parseMarkedFiles(output string) map[string]string {
	files, _ := parseMarkedResultOutput(output)
	return files
}

func parseMarkedResultOutput(output string) (map[string]string, []error) {
	files := map[string]string{}
	var warnings []error
	current := ""
	var b strings.Builder
	flush := func() {
		if current != "" {
			files[current] = strings.TrimSpace(b.String())
			b.Reset()
		}
	}
	for _, line := range strings.Split(output, "\n") {
		if name, ok := strings.CutPrefix(line, resultFileMarker); ok {
			flush()
			current = strings.TrimSpace(name)
			continue
		}
		if warning, ok := strings.CutPrefix(line, resultWarningMarker); ok {
			flush()
			current = ""
			name, reason, _ := strings.Cut(strings.TrimSpace(warning), "\t")
			warnings = append(warnings, fmt.Errorf("skip junit %s: %s", name, reason))
			continue
		}
		if current != "" {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	flush()
	return files, warnings
}

func resultSummaryLine(results *TestResultSummary) string {
	if results == nil {
		return ""
	}
	return fmt.Sprintf("test results files=%d tests=%d failures=%d errors=%d skipped=%d", len(results.Files), results.Tests, results.Failures, results.Errors, results.Skipped)
}

func failRunForTestResults(commandExitCode int, cfg ResultsConfig, results *TestResultSummary) bool {
	return commandExitCode == 0 && cfg.FailOnFailures && results != nil && (results.Failures > 0 || results.Errors > 0 || len(results.Failed) > 0)
}
