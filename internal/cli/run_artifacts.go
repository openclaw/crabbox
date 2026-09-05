package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	DelegatedRunArtifactDefaultMaxFiles = 256
	DelegatedRunArtifactDefaultMaxBytes = int64(10 * 1024 * 1024)
	DelegatedRunArtifactBeginMarker     = "__CRABBOX_ARTIFACT_TAR_BEGIN__"
	DelegatedRunArtifactEndMarker       = "__CRABBOX_ARTIFACT_TAR_END__"
)

type RunArtifact struct {
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Template string `json:"template,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`
}

type runArtifact = RunArtifact

type runArtifactResult struct {
	Files []runArtifact `json:"files,omitempty"`
}

func requireRunArtifactGlobs(ctx context.Context, target SSHTarget, workdir string, globs []string) (string, error) {
	if len(globs) == 0 {
		return "", nil
	}
	if err := validateRequiredRunArtifactGlobs(globs); err != nil {
		return "", err
	}
	if err := validateRequiredRunArtifactGlobTarget(target, globs); err != nil {
		return "", err
	}
	remote := remoteRequireArtifactGlobsCommand(target, workdir, globs)
	out, err := runSSHCombinedOutput(ctx, target, remote)
	if err != nil {
		return strings.TrimSpace(out), exit(7, "require artifacts: %v: %s", err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

func collectRunArtifactGlobs(ctx context.Context, target SSHTarget, workdir, repoRoot, runID, leaseID string, globs []string) ([]runArtifact, string, error) {
	if len(globs) == 0 {
		return nil, "", nil
	}
	if err := validateRunArtifactGlobs(globs); err != nil {
		return nil, "", err
	}
	if err := validateRunArtifactGlobTarget(target, globs); err != nil {
		return nil, "", err
	}
	name := safeCaptureName(firstNonBlank(runID, leaseID, "run")) + "-artifacts.tgz"
	remotePath := ".crabbox/" + name
	script := runArtifactCollectScript(workdir, remotePath, globs)
	var output synchronizedBuffer
	err := runSSHInput(ctx, target, remoteRunArtifactShellInputCommand(target), strings.NewReader(script), &output, &output)
	out := output.String()
	if err != nil {
		return nil, "", exit(7, "collect artifacts: %v: %s", err, strings.TrimSpace(out))
	}
	defer func() {
		_, _ = runSSHCombinedOutput(context.Background(), target, remoteRemoveRunArtifactCommand(target, workdir, remotePath))
	}()
	localPath := localRunArtifactPath(repoRoot, runID, leaseID, name)
	bytes, local, err := downloadRemoteFile(ctx, target, workdir, remotePath+"="+localPath)
	if err != nil {
		return nil, strings.TrimSpace(out), err
	}
	return []runArtifact{{Kind: "artifact-glob", Path: local, Bytes: bytes}}, strings.TrimSpace(out), nil
}

func localRunArtifactPath(repoRoot, runID, leaseID, name string) string {
	root := strings.TrimSpace(repoRoot)
	if root == "" {
		root = "."
	}
	return filepath.Join(root, ".crabbox", "runs", safeCaptureName(firstNonBlank(runID, leaseID, "run")), name)
}

func LocalRunArtifactPath(repoRoot, runID, leaseID, name string) string {
	return localRunArtifactPath(repoRoot, runID, leaseID, name)
}

func validateRunArtifactGlobs(globs []string) error {
	return validateRunArtifactGlobsForFlag("--artifact-glob", globs)
}

func validateRequiredRunArtifactGlobs(globs []string) error {
	return validateRunArtifactGlobsForFlag("--require-artifact", globs)
}

func ValidateRunArtifactGlobs(globs []string) error {
	return validateRunArtifactGlobs(globs)
}

func ValidateRequiredRunArtifactGlobs(globs []string) error {
	return validateRequiredRunArtifactGlobs(globs)
}

func validateRunArtifactGlobsForFlag(flag string, globs []string) error {
	for _, glob := range globs {
		if !safeArtifactGlob(glob) {
			return exit(2, "%s contains unsupported characters or non-relative path: %s", flag, glob)
		}
		for _, component := range strings.Split(filepath.ToSlash(strings.TrimSpace(glob)), "/") {
			if component == ".git" || component == ".crabbox" {
				return exit(2, "%s excludes protected path components: %s", flag, glob)
			}
		}
	}
	return nil
}

func validateRunArtifactGlobTarget(target SSHTarget, globs []string) error {
	return validateRunArtifactGlobTargetForFlag(target, globs, "--artifact-glob")
}

func validateRequiredRunArtifactGlobTarget(target SSHTarget, globs []string) error {
	return validateRunArtifactGlobTargetForFlag(target, globs, "--require-artifact")
}

func validateRunArtifactGlobTargetForFlag(target SSHTarget, globs []string, flag string) error {
	if len(globs) > 0 && isWindowsNativeTarget(target) {
		return exit(2, "%s is not supported for native Windows targets", flag)
	}
	return nil
}

func safeArtifactGlob(glob string) bool {
	glob = strings.TrimSpace(glob)
	if glob == "" || strings.HasPrefix(glob, "-") || strings.HasPrefix(glob, "/") || strings.ContainsAny(glob, "{}") {
		return false
	}
	rel := strings.TrimPrefix(filepath.ToSlash(glob), "./")
	if strings.HasPrefix(rel, "/") {
		return false
	}
	for _, component := range strings.Split(rel, "/") {
		if component == ".." {
			return false
		}
	}
	return regexp.MustCompile(`^[A-Za-z0-9_./*?@+=:,-]+$`).MatchString(glob)
}

func remoteCollectArtifactGlobsCommand(target SSHTarget, workdir, remotePath string, globs []string) string {
	return remoteRunArtifactShellCommand(target, runArtifactCollectScript(workdir, remotePath, globs))
}

func remoteRequireArtifactGlobsCommand(target SSHTarget, workdir string, globs []string) string {
	return remoteRunArtifactShellCommand(target, runArtifactRequireScript(workdir, globs))
}

func remoteRemoveRunArtifactCommand(target SSHTarget, workdir, remotePath string) string {
	rm := "rm"
	if target.TargetOS == targetMacOS {
		rm = "/bin/rm"
	}
	script := "set -eu\ncd " + shellQuote(workdir) + "\n" + rm + " -f -- " + shellQuote(remotePath)
	return remoteRunArtifactShellCommand(target, script)
}

func remoteRunArtifactShellCommand(target SSHTarget, script string) string {
	bash := "bash"
	if target.TargetOS == targetMacOS {
		bash = "/bin/bash"
	}
	return bash + " -lc " + shellQuote(script)
}

func remoteRunArtifactShellInputCommand(target SSHTarget) string {
	bash := "bash"
	if target.TargetOS == targetMacOS {
		bash = "/bin/bash"
	}
	return remoteRunArtifactShellCommand(target, bash+" -s")
}

func writeArtifactGlobMatcher(b *strings.Builder) {
	b.WriteString("artifact_rel_path() { local rel=\"${1#./}\"; case \"$rel\" in \"\"|.|/*|..|../*|*/../*|*/..) return 1;; esac; case \"/$rel/\" in */.git/*|*/.crabbox/*) return 1;; esac; printf '%s' \"$rel\"; }\n")
	b.WriteString("artifact_safe_search_root() { local root=\"${1#./}\" component path=; [ \"$1\" = . ] && return 0; case \"$root\" in \"\"|.|/*|..|../*|*/../*|*/..) return 1;; esac; while [ -n \"$root\" ]; do component=${root%%/*}; case \"$component\" in \"\"|.|..|.git|.crabbox) return 1;; esac; if [ \"$component\" = \"$root\" ]; then root=; else root=${root#*/}; fi; if [ -n \"$path\" ]; then path=\"$path/$component\"; else path=$component; fi; [ ! -L \"$path\" ] || return 1; [ -d \"$path\" ] || return 1; done; }\n")
}

func writeArtifactGlobEnumeration(b *strings.Builder, glob, addFunction string) {
	root := artifactGlobSearchRoot(glob)
	enumerate := `find "$artifact_root" \( -name .git -o -name .crabbox \) -prune -o \( -type f -o -type l \) -print0`
	// The legacy regexp quotes bytes individually; keep non-ASCII spellings on that path.
	asciiOnly := strings.IndexFunc(glob, func(r rune) bool { return r > unicode.MaxASCII }) < 0
	if asciiOnly && !strings.ContainsAny(glob, "*?") {
		// Expand one literal leaf to retain its directory-entry case on macOS.
		// Keep outer whitespace: validation trims it, but the regex matches it literally.
		leaf := glob[strings.LastIndex(glob, "/")+1:]
		if leaf != "" {
			last := len(leaf) - 1
			enumerate = "printf '%s\\0' " + shellQuote(root+"/"+leaf[:last]) + "[" + shellQuote(leaf[last:]) + "]"
		}
	}
	b.WriteString("artifact_regex=" + shellQuote(artifactGlobRegex(glob)) + "; artifact_root=" + shellQuote(root) + "; if artifact_safe_search_root \"$artifact_root\"; then while IFS= read -r -d '' f; do rel=$(artifact_rel_path \"$f\") || continue; if [[ \"$rel\" =~ $artifact_regex || \"./$rel\" =~ $artifact_regex ]]; then " + addFunction + " \"$f\"; fi; done < <(" + enumerate + "); fi\n")
}

func runArtifactRequireScript(workdir string, globs []string) string {
	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	b.WriteString("cd " + shellQuote(workdir) + "\n")
	b.WriteString("missing=()\n")
	writeArtifactGlobMatcher(&b)
	b.WriteString("check_artifact_file() { local f=\"$1\" rel; [ -f \"$f\" ] || return 1; rel=$(artifact_rel_path \"$f\") || return 1; return 0; }\n")
	b.WriteString("add_required_artifact_match() { local f=\"$1\" rel existing; check_artifact_file \"$f\" || return 0; rel=$(artifact_rel_path \"$f\") || return 0; if [ ${#matches[@]} -gt 0 ]; then for existing in \"${matches[@]}\"; do [ \"$existing\" = \"$rel\" ] && return 0; done; fi; matches+=(\"$rel\"); }\n")
	for _, glob := range globs {
		b.WriteString("matches=()\n")
		writeArtifactGlobEnumeration(&b, glob, "add_required_artifact_match")
		b.WriteString("if [ ${#matches[@]} -eq 0 ]; then missing+=(" + shellQuote(glob) + "); else printf 'required artifact %s matched=%s\\n' " + shellQuote(glob) + " \"${#matches[@]}\"; fi\n")
	}
	b.WriteString("if [ ${#missing[@]} -gt 0 ]; then for f in \"${missing[@]}\"; do printf 'missing required artifact: %s\\n' \"$f\" >&2; done; exit 8; fi\n")
	return b.String()
}

func runArtifactCollectScript(workdir, remotePath string, globs []string) string {
	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	b.WriteString("cd " + shellQuote(workdir) + "\n")
	b.WriteString("mkdir -p .crabbox\n")
	b.WriteString("files=()\n")
	writeArtifactGlobMatcher(&b)
	b.WriteString("add_artifact_file() { local f=\"$1\" rel existing; [ -f \"$f\" ] || return 0; rel=$(artifact_rel_path \"$f\") || return 0; case \"$rel\" in " + remotePath + ") return 0;; esac; if [ ${#files[@]} -gt 0 ]; then for existing in \"${files[@]}\"; do [ \"$existing\" = \"$rel\" ] && return 0; done; fi; files+=(\"$rel\"); }\n")
	for _, glob := range globs {
		writeArtifactGlobEnumeration(&b, glob, "add_artifact_file")
	}
	b.WriteString("if [ ${#files[@]} -eq 0 ]; then printf 'warning: no artifact matches\\n' >&2; COPYFILE_DISABLE=1 tar -czf " + shellQuote(remotePath) + " --files-from /dev/null; else COPYFILE_DISABLE=1 tar -czf " + shellQuote(remotePath) + " -- \"${files[@]}\"; fi\n")
	return b.String()
}

func DelegatedRunArtifactScript(requiredGlobs, artifactGlobs []string, maxFiles int, maxBytes int64) string {
	if maxFiles <= 0 {
		maxFiles = DelegatedRunArtifactDefaultMaxFiles
	}
	if maxBytes <= 0 {
		maxBytes = DelegatedRunArtifactDefaultMaxBytes
	}
	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	writeArtifactGlobMatcher(&b)
	b.WriteString("check_artifact_file() { local f=\"$1\" rel; [ -f \"$f\" ] || return 1; rel=$(artifact_rel_path \"$f\") || return 1; return 0; }\n")
	b.WriteString("dedupe_artifact_match() { local f=\"$1\" rel existing; check_artifact_file \"$f\" || return 0; rel=$(artifact_rel_path \"$f\") || return 0; if [ ${#matches[@]} -gt 0 ]; then for existing in \"${matches[@]}\"; do [ \"$existing\" = \"$rel\" ] && return 0; done; fi; matches+=(\"$rel\"); }\n")
	appendArtifactGlobMatches := func(glob string) {
		writeArtifactGlobEnumeration(&b, glob, "dedupe_artifact_match")
	}
	if len(requiredGlobs) > 0 {
		b.WriteString("missing=()\n")
		for _, glob := range requiredGlobs {
			b.WriteString("matches=()\n")
			appendArtifactGlobMatches(glob)
			b.WriteString("if [ ${#matches[@]} -eq 0 ]; then missing+=(" + shellQuote(glob) + "); else printf 'required artifact %s matched=%s\\n' " + shellQuote(glob) + " \"${#matches[@]}\"; fi\n")
		}
		b.WriteString("if [ ${#missing[@]} -gt 0 ]; then for f in \"${missing[@]}\"; do printf 'missing required artifact: %s\\n' \"$f\" >&2; done; exit 8; fi\n")
	}
	if len(artifactGlobs) == 0 {
		return b.String()
	}
	b.WriteString("matches=()\n")
	for _, glob := range artifactGlobs {
		appendArtifactGlobMatches(glob)
	}
	b.WriteString("if [ ${#matches[@]} -gt " + fmt.Sprint(maxFiles) + " ]; then printf 'artifact-glob matched too many files: %s > %s\\n' \"${#matches[@]}\" " + shellQuote(fmt.Sprint(maxFiles)) + " >&2; exit 9; fi\n")
	b.WriteString("tmp=$(mktemp -t crabbox-artifacts.XXXXXX.tgz); trap 'rm -f \"$tmp\"' EXIT\n")
	b.WriteString("if [ ${#matches[@]} -eq 0 ]; then printf 'warning: no artifact matches\\n' >&2; COPYFILE_DISABLE=1 tar -czf \"$tmp\" --files-from /dev/null; else COPYFILE_DISABLE=1 tar -czf \"$tmp\" -- \"${matches[@]}\"; fi\n")
	b.WriteString("bytes=$(wc -c < \"$tmp\" | tr -d ' ')\n")
	b.WriteString("if [ \"$bytes\" -gt " + shellQuote(fmt.Sprint(maxBytes)) + " ]; then printf 'artifact-glob archive too large: %s > %s bytes\\n' \"$bytes\" " + shellQuote(fmt.Sprint(maxBytes)) + " >&2; exit 9; fi\n")
	b.WriteString("printf '" + DelegatedRunArtifactBeginMarker + "\\n'\n")
	b.WriteString("base64 < \"$tmp\"\n")
	b.WriteString("printf '\\n" + DelegatedRunArtifactEndMarker + "\\n'\n")
	return b.String()
}

func artifactGlobSearchRoot(glob string) string {
	glob = strings.TrimSpace(filepath.ToSlash(glob))
	glob = strings.TrimPrefix(glob, "./")
	if glob == "" {
		return "."
	}
	firstMeta := strings.IndexAny(glob, "*?")
	if firstMeta < 0 {
		dir := filepath.ToSlash(filepath.Dir(glob))
		if dir == "" {
			return "."
		}
		return dir
	}
	prefix := strings.TrimRight(glob[:firstMeta], "/")
	if prefix == "" {
		return "."
	}
	dir := filepath.ToSlash(filepath.Dir(prefix))
	if dir == "." && strings.HasSuffix(glob[:firstMeta], "/") {
		return prefix
	}
	if dir == "." && !strings.Contains(prefix, "/") {
		return "."
	}
	return dir
}

func artifactGlobRegex(glob string) string {
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(glob); {
		if strings.HasPrefix(glob[i:], "**/") {
			b.WriteString("(.*/)?")
			i += 3
			continue
		}
		if strings.HasPrefix(glob[i:], "**") {
			b.WriteString(".*")
			i += 2
			continue
		}
		switch glob[i] {
		case '*':
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
		}
		i++
	}
	b.WriteByte('$')
	return b.String()
}

type proofRenderInput struct {
	Template    ProofTemplateConfig
	Provider    string
	LeaseID     string
	Slug        string
	RunID       string
	Command     string
	LogExcerpt  string
	Captures    []streamCaptureMetadata
	ActionsURL  string
	Artifacts   []runArtifact
	Variables   map[string]string
	CommandMs   int64
	ExitCode    int
	GeneratedAt time.Time
}

func writeRunProof(path, templateName string, input proofRenderInput) (runArtifact, error) {
	content, err := renderRunProof(input)
	if err != nil {
		return runArtifact{}, err
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := createPrivateRunOutputDir(dir); err != nil {
			return runArtifact{}, exit(2, "create proof directory: %v", err)
		}
	}
	if err := writePrivateRunOutputFile(path, []byte(content)); err != nil {
		return runArtifact{}, exit(2, "write proof %s: %v", path, err)
	}
	return runArtifact{Kind: "proof", Path: path, Template: templateName, Bytes: len(content)}, nil
}

func renderRunProof(input proofRenderInput) (string, error) {
	input.LogExcerpt = proofConsoleEvidence(input.LogExcerpt, input.Captures)
	values := proofTemplateValues(input)
	tmpl := input.Template
	behavior, err := renderProofTemplateField("behaviorAddressed", tmpl.BehaviorAddressed, "Remote behavior exercised by the Crabbox command.", values)
	if err != nil {
		return "", err
	}
	environment, err := renderProofTemplateField("realEnvironmentTested", tmpl.RealEnvironmentTested, fmt.Sprintf("%s Crabbox lease %s (%s).", input.Provider, input.LeaseID, blank(input.Slug, "-")), values)
	if err != nil {
		return "", err
	}
	steps, err := renderProofTemplateField("exactSteps", tmpl.ExactSteps, input.Command, values)
	if err != nil {
		return "", err
	}
	observed, err := renderProofTemplateField("observedResult", tmpl.ObservedResult, "The command completed successfully on the remote environment.", values)
	if err != nil {
		return "", err
	}
	notTested, err := renderProofTemplateField("notTested", tmpl.NotTested, "No additional environments beyond this Crabbox run.", values)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("## Real behavior proof\n\n")
	b.WriteString("Behavior addressed: " + behavior + "\n\n")
	b.WriteString("Real environment tested: " + environment + "\n\n")
	stepsOpenFence, stepsCloseFence := markdownFence("sh", steps)
	b.WriteString("Exact steps or command run:\n\n" + stepsOpenFence + "\n")
	b.WriteString(steps)
	b.WriteString("\n" + stepsCloseFence + "\n\n")
	b.WriteString("Evidence: Copied live console output from Crabbox")
	if input.RunID != "" {
		b.WriteString(" `" + input.RunID + "`")
	}
	logExcerpt := strings.TrimSpace(input.LogExcerpt)
	openFence, closeFence := markdownFence("text", logExcerpt)
	b.WriteString(":\n\n" + openFence + "\n")
	b.WriteString(logExcerpt)
	b.WriteString("\n" + closeFence + "\n\n")
	b.WriteString("Observed result: " + observed + "\n\n")
	if len(input.Artifacts) > 0 || input.ActionsURL != "" {
		b.WriteString("Additional evidence: ")
		parts := make([]string, 0, len(input.Artifacts)+1)
		if input.ActionsURL != "" {
			parts = append(parts, input.ActionsURL)
		}
		for _, artifact := range input.Artifacts {
			parts = append(parts, artifact.Path)
		}
		b.WriteString(strings.Join(parts, "; ") + "\n\n")
	}
	b.WriteString("What was not tested: " + notTested + "\n")
	return b.String(), nil
}

func proofConsoleEvidence(logExcerpt string, captures []streamCaptureMetadata) string {
	logExcerpt = strings.TrimSpace(logExcerpt)
	if len(captures) == 0 {
		if logExcerpt == "" {
			return "(no console output captured)"
		}
		return logExcerpt
	}
	if logExcerpt == "(no console output captured)" {
		logExcerpt = ""
	}
	lines := make([]string, 0, len(captures)+1)
	if logExcerpt != "" {
		lines = append(lines, logExcerpt)
	}
	for _, capture := range captures {
		lines = append(lines, fmt.Sprintf("captured stream=%s path=%s bytes=%d", capture.Label, quoteProofCapturePath(capture.Path), capture.Bytes))
	}
	return strings.Join(lines, "\n")
}

func quoteProofCapturePath(path string) string {
	const safe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/.- "
	var b strings.Builder
	b.Grow(len(path) + 2)
	b.WriteByte('"')
	for _, value := range []byte(path) {
		if strings.IndexByte(safe, value) >= 0 {
			b.WriteByte(value)
			continue
		}
		fmt.Fprintf(&b, "\\x%02x", value)
	}
	b.WriteByte('"')
	return b.String()
}

func proofTemplateValues(input proofRenderInput) map[string]string {
	values := map[string]string{}
	for key, value := range input.Variables {
		values[key] = value
	}
	builtins := map[string]string{
		"provider":   input.Provider,
		"leaseId":    input.LeaseID,
		"slug":       input.Slug,
		"runId":      input.RunID,
		"command":    input.Command,
		"logExcerpt": input.LogExcerpt,
		"actionsUrl": input.ActionsURL,
	}
	for key, value := range builtins {
		values[key] = value
	}
	return values
}

func renderProofTemplateField(label, templateValue, fallback string, values map[string]string) (string, error) {
	if strings.TrimSpace(templateValue) == "" {
		return strings.TrimSpace(fallback), nil
	}
	if err := validateProofTemplatePlaceholders(label, templateValue, values); err != nil {
		return "", err
	}
	return expandPresetValue(templateValue, values), nil
}

func validateProofTemplatePlaceholders(label, value string, values map[string]string) error {
	matches := presetPlaceholderPattern.FindAllString(value, -1)
	if len(matches) == 0 {
		return nil
	}
	var missing []string
	for _, match := range appendUniqueStrings(nil, matches...) {
		key := strings.TrimSuffix(strings.TrimPrefix(match, "{{"), "}}")
		if _, ok := values[key]; !ok {
			missing = append(missing, match)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return exit(2, "proof template %s has unresolved preset variable(s): %s", label, strings.Join(missing, ", "))
}

func markdownFence(info, content string) (string, string) {
	size := 3
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "```") {
			continue
		}
		count := 0
		for _, r := range trimmed {
			if r != '`' {
				break
			}
			count++
		}
		if count >= size {
			size = count + 1
		}
	}
	fence := strings.Repeat("`", size)
	if strings.TrimSpace(info) == "" {
		return fence, fence
	}
	return fence + strings.TrimSpace(info), fence
}

func selectProofLogExcerpt(log string) string {
	log = strings.ReplaceAll(stripANSI(log), "\r", "\n")
	if redacted, ok := RedactKnownFailureBody(log); ok {
		return redacted
	}
	lines := strings.Split(strings.TrimSpace(log), "\n")
	out := make([]string, 0, 12)
	for i := len(lines) - 1; i >= 0 && len(out) < 12; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			out = append(out, line)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if len(out) == 0 {
		return "(no console output captured)"
	}
	excerpt := strings.Join(out, "\n")
	if len(excerpt) > 4000 {
		excerpt = excerpt[len(excerpt)-4000:]
	}
	return excerpt
}

func SelectProofLogExcerpt(log string) string {
	return selectProofLogExcerpt(log)
}

func remoteProfileDoctorCommand(profile string, doctor DoctorProfileConfig, workdir string) string {
	script := profileDoctorScript(doctor, workdir)
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	return "bash -lc " + shellQuote("tmp=$(mktemp); trap 'rm -f \"$tmp\"' EXIT; printf %s "+shellQuote(encoded)+" | base64 -d > \"$tmp\" || exit 1; bash \"$tmp\"")
}

func profileDoctorScript(doctor DoctorProfileConfig, workdir string) string {
	tools := normalizePreflightToolNames(doctor.Tools)
	if doctor.NodeMajor > 0 {
		tools = appendUniqueStrings(tools, "node")
	}
	if doctor.RequireDocker {
		tools = appendUniqueStrings(tools, "docker")
	}
	var b strings.Builder
	b.WriteString("set +e\n")
	b.WriteString("fail=0\n")
	b.WriteString("check_cmd() { name=\"$1\"; shift; if \"$@\" >/tmp/crabbox-doctor.$name 2>&1; then v=$(head -1 /tmp/crabbox-doctor.$name); printf 'ok      %-16s %s\\n' \"$name\" \"$v\"; else printf 'failed  %-16s missing or unusable\\n' \"$name\"; fail=1; fi; rm -f /tmp/crabbox-doctor.$name; }\n")
	for _, tool := range tools {
		switch tool {
		case "corepack":
			b.WriteString("check_cmd corepack corepack --version\n")
		case "docker":
			b.WriteString("check_cmd docker docker --version\n")
		case "node":
			if doctor.NodeMajor > 0 {
				b.WriteString(fmt.Sprintf("node_v=$(node --version 2>/dev/null); node_major=${node_v#v}; node_major=${node_major%%%%.*}; if [ \"$node_major\" = %s ]; then printf 'ok      %%-16s %%s\\n' node \"$node_v\"; else printf 'failed  %%-16s got=%%s want_major=%d\\n' node \"${node_v:-missing}\"; fail=1; fi\n", shellQuote(fmt.Sprint(doctor.NodeMajor)), doctor.NodeMajor))
			} else {
				b.WriteString("check_cmd node node --version\n")
			}
		case "pnpm":
			b.WriteString("check_cmd pnpm pnpm --version\n")
		case "sudo":
			b.WriteString("if command -v sudo >/tmp/crabbox-doctor.sudo 2>&1 && sudo -n true >>/tmp/crabbox-doctor.sudo 2>&1; then printf 'ok      %-16s noninteractive\\n' sudo; else printf 'failed  %-16s missing or requires password\\n' sudo; fail=1; fi; rm -f /tmp/crabbox-doctor.sudo\n")
		default:
			if spec, ok := preflightToolRegistry[tool]; ok && len(spec.Posix) > 0 {
				b.WriteString("check_cmd " + shellQuote(tool) + " " + strings.Join(readableShellWords(spec.Posix), " ") + "\n")
			}
		}
	}
	if doctor.RequireCompose {
		b.WriteString("if docker compose version >/tmp/crabbox-doctor.compose 2>&1; then printf 'ok      %-16s %s\\n' docker-compose \"$(head -1 /tmp/crabbox-doctor.compose)\"; else printf 'failed  %-16s install Docker Compose v2 so docker compose works\\n' docker-compose; fail=1; fi; rm -f /tmp/crabbox-doctor.compose\n")
	}
	if doctor.RequireDocker {
		b.WriteString("if docker version >/tmp/crabbox-doctor.docker-daemon 2>&1; then printf 'ok      %-16s %s\\n' docker-daemon \"$(head -1 /tmp/crabbox-doctor.docker-daemon)\"; else printf 'failed  %-16s Docker daemon unavailable or not usable\\n' docker-daemon; fail=1; fi; rm -f /tmp/crabbox-doctor.docker-daemon\n")
	}
	if doctor.MinDiskGB > 0 {
		diskPath := strings.TrimSpace(workdir)
		if diskPath == "" {
			diskPath = "."
		}
		b.WriteString(fmt.Sprintf("free=$(df -Pk %s | awk 'NR==2 {print int($4/1024/1024)}'); if [ \"$free\" -ge %d ]; then printf 'ok      %%-16s free_gb=%%s path=%%s\\n' disk \"$free\" %s; else printf 'failed  %%-16s free_gb=%%s want>=%d path=%%s\\n' disk \"$free\" %s; fail=1; fi\n", shellQuote(diskPath), doctor.MinDiskGB, shellQuote(diskPath), doctor.MinDiskGB, shellQuote(diskPath)))
	}
	b.WriteString("printf 'ok      %-16s cpus=%s mem_mb=%s\\n' system \"$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo unknown)\" \"$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo 2>/dev/null || echo unknown)\"\n")
	b.WriteString("exit $fail\n")
	return b.String()
}

func profileDoctorWorkdirForLease(cfg Config, leaseID string) string {
	if strings.TrimSpace(cfg.WorkRoot) != "" {
		return cfg.WorkRoot
	}
	if strings.TrimSpace(leaseID) != "" {
		return remoteJoin(cfg, leaseID)
	}
	return "."
}
