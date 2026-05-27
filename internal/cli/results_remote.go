package cli

import (
	"context"
	"fmt"
	"strings"
)

const (
	resultFileMarker    = "__CRABBOX_RESULT_FILE__:"
	remoteResultsMarker = ".crabbox/results-start"
	autoJUnitMaxFiles   = 50
	autoJUnitMaxBytes   = 1 << 20
	autoJUnitSniffBytes = 4 << 10
)

func collectRemoteJUnitResults(ctx context.Context, target SSHTarget, workdir string, cfg ResultsConfig, autoMarker string) (*TestResultSummary, error) {
	paths := appendUniqueStrings(nil, cfg.JUnit...)
	files := map[string]string{}
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
		autoFiles, err := collectRemoteJUnitResultFilesAuto(ctx, target, workdir, autoMarker)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for name := range files {
			seen[normalizeResultPath(workdir, name)] = true
		}
		for name, data := range filterAutoJUnitFiles(autoFiles) {
			key := normalizeResultPath(workdir, name)
			if !seen[key] {
				files[name] = data
				seen[key] = true
			}
		}
	}
	if len(files) == 0 {
		return nil, nil
	}
	return parseJUnitResults(files)
}

func collectRemoteJUnitResultFilesAuto(ctx context.Context, target SSHTarget, workdir, marker string) (map[string]string, error) {
	remote := remoteFindJUnitResultFiles(workdir, marker)
	if isWindowsNativeTarget(target) {
		remote = windowsRemoteFindJUnitResultFiles(workdir, marker)
	}
	out, err := runSSHOutput(ctx, target, remote)
	if err != nil {
		return nil, err
	}
	return parseMarkedFiles(out), nil
}

func remoteReadResultFiles(workdir string, paths []string) string {
	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(shellQuote(workdir))
	b.WriteString(" && ")
	b.WriteString("for f in")
	for _, path := range paths {
		b.WriteByte(' ')
		b.WriteString(shellQuote(path))
	}
	b.WriteString("; do if [ -f \"$f\" ]; then printf '\\n")
	b.WriteString(resultFileMarker)
	b.WriteString("%s\\n' \"$f\"; cat \"$f\"; fi; done")
	return b.String()
}

func remoteTouchResultsMarker(workdir string) string {
	return "cd " + shellQuote(workdir) + " && mkdir -p .crabbox && : > " + shellQuote(remoteResultsMarker)
}

func remoteFindJUnitResultFiles(workdir, marker string) string {
	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(shellQuote(workdir))
	b.WriteString(" && { ")
	b.WriteString("count=0; ")
	b.WriteString(`find . \( -path './node_modules' -o -path '*/node_modules' -o -path './.git' -o -path '*/.git' \) -prune -o -type f \( -name 'junit*.xml' -o -name 'TEST-*.xml' -o -name 'results.xml' \)`)
	b.WriteString(` -print 2>/dev/null | sort | while IFS= read -r f; do `)
	if strings.TrimSpace(marker) != "" {
		b.WriteString("if [ ")
		b.WriteString(shellQuote(marker))
		b.WriteString(` -nt "$f" ]; then continue; fi; `)
	}
	b.WriteString(fmt.Sprintf(`dd if="$f" bs=%d count=1 2>/dev/null | grep -Eq '<testsuites?' || continue; count=$((count + 1)); if [ "$count" -gt %d ]; then break; fi; `, autoJUnitSniffBytes, autoJUnitMaxFiles))
	b.WriteString(`printf '\n`)
	b.WriteString(resultFileMarker)
	b.WriteString(fmt.Sprintf(`%%s\n' "$f"; dd if="$f" bs=%d count=1 2>/dev/null; done; }`, autoJUnitMaxBytes))
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
	files := map[string]string{}
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
		if current != "" {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	flush()
	return files
}

func resultSummaryLine(results *TestResultSummary) string {
	if results == nil {
		return ""
	}
	return fmt.Sprintf("test results files=%d tests=%d failures=%d errors=%d skipped=%d", len(results.Files), results.Tests, results.Failures, results.Errors, results.Skipped)
}
