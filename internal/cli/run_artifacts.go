package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type runArtifact struct {
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Template string `json:"template,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`
}

type runArtifactResult struct {
	Files []runArtifact `json:"files,omitempty"`
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
	remote := remoteCollectArtifactGlobsCommand(workdir, remotePath, globs)
	out, err := runSSHCombinedOutput(ctx, target, remote)
	if err != nil {
		return nil, "", exit(7, "collect artifacts: %v: %s", err, strings.TrimSpace(out))
	}
	defer func() {
		_, _ = runSSHCombinedOutput(context.Background(), target, remoteRemoveFailureCaptureCommand(workdir, remotePath))
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

func validateRunArtifactGlobs(globs []string) error {
	for _, glob := range globs {
		if !safeArtifactGlob(glob) {
			return exit(2, "--artifact-glob contains unsupported characters or non-relative path: %s", glob)
		}
	}
	return nil
}

func validateRunArtifactGlobTarget(target SSHTarget, globs []string) error {
	if len(globs) > 0 && isWindowsNativeTarget(target) {
		return exit(2, "--artifact-glob is not supported for native Windows targets")
	}
	if len(globs) > 0 && target.TargetOS == targetMacOS {
		return exit(2, "--artifact-glob is not supported for macOS targets")
	}
	return nil
}

func safeArtifactGlob(glob string) bool {
	glob = strings.TrimSpace(glob)
	if glob == "" || strings.HasPrefix(glob, "-") || strings.HasPrefix(glob, "/") || strings.Contains(glob, "..") || strings.ContainsAny(glob, "{}") {
		return false
	}
	return regexp.MustCompile(`^[A-Za-z0-9_./*?@+=:,-]+$`).MatchString(glob)
}

func remoteCollectArtifactGlobsCommand(workdir, remotePath string, globs []string) string {
	return "bash -lc " + shellQuote(runArtifactCollectScript(workdir, remotePath, globs))
}

func runArtifactCollectScript(workdir, remotePath string, globs []string) string {
	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	b.WriteString("cd " + shellQuote(workdir) + "\n")
	b.WriteString("mkdir -p .crabbox\n")
	b.WriteString("shopt -s nullglob dotglob\n")
	b.WriteString("files=()\n")
	b.WriteString("add_artifact_file() { local f=\"$1\" rel existing; { [ -f \"$f\" ] || [ -L \"$f\" ]; } || return 0; rel=${f#./}; case \"$rel\" in .git|.git/*|.crabbox|.crabbox/*|" + remotePath + ") return 0;; esac; if [ ${#files[@]} -gt 0 ]; then for existing in \"${files[@]}\"; do [ \"$existing\" = \"$rel\" ] && return 0; done; fi; files+=(\"$rel\"); }\n")
	for _, glob := range globs {
		b.WriteString("for f in " + glob + "; do add_artifact_file \"$f\"; done\n")
		if strings.Contains(glob, "**") {
			if strings.Contains(glob, "**/") {
				b.WriteString("for f in " + strings.Replace(glob, "**/", "", 1) + "; do add_artifact_file \"$f\"; done\n")
			}
			searchRoot := artifactGlobSearchRoot(glob)
			b.WriteString("artifact_regex=" + shellQuote(artifactGlobRegex(glob)) + "; artifact_root=" + shellQuote(searchRoot) + "; if [ -e \"$artifact_root\" ]; then while IFS= read -r -d '' f; do rel=${f#./}; if [[ \"$rel\" =~ $artifact_regex || \"./$rel\" =~ $artifact_regex ]]; then add_artifact_file \"$f\"; fi; done < <(find \"$artifact_root\" \\( -path './.git' -o -path './.git/*' -o -path './.crabbox' -o -path './.crabbox/*' -o -path '.git' -o -path '.git/*' -o -path '.crabbox' -o -path '.crabbox/*' \\) -prune -o \\( -type f -o -type l \\) -print0); fi\n")
		}
	}
	b.WriteString("if [ ${#files[@]} -eq 0 ]; then printf 'warning: no artifact matches\\n' >&2; tar -czf " + shellQuote(remotePath) + " --files-from /dev/null; else tar -czf " + shellQuote(remotePath) + " -- \"${files[@]}\"; fi\n")
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
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return runArtifact{}, exit(2, "create proof directory: %v", err)
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return runArtifact{}, exit(2, "write proof %s: %v", path, err)
	}
	return runArtifact{Kind: "proof", Path: path, Template: templateName, Bytes: len(content)}, nil
}

func renderRunProof(input proofRenderInput) (string, error) {
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
	b.WriteString("Exact steps or command run after this patch:\n\n" + stepsOpenFence + "\n")
	b.WriteString(steps)
	b.WriteString("\n" + stepsCloseFence + "\n\n")
	b.WriteString("Evidence after fix: Copied live console output from Crabbox")
	if input.RunID != "" {
		b.WriteString(" `" + input.RunID + "`")
	}
	logExcerpt := strings.TrimSpace(input.LogExcerpt)
	openFence, closeFence := markdownFence("text", logExcerpt)
	b.WriteString(":\n\n" + openFence + "\n")
	b.WriteString(logExcerpt)
	b.WriteString("\n" + closeFence + "\n\n")
	b.WriteString("Observed result after fix: " + observed + "\n\n")
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
