package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	harnessIndexNone  = "none"
	harnessIndexLight = "light"
)

type HarnessConfig struct {
	Path       string
	Index      string
	Version    string
	Template   string
	Job        string
	PlanFile   string
	Scope      []string
	Validate   HarnessValidateConfig
	Compliance HarnessComplianceConfig
}

type HarnessValidateConfig struct {
	Commands []string
}

type HarnessComplianceConfig struct {
	RequirePlan       bool
	RequireJUnit      bool
	RequiredArtifacts []string
}

type HarnessMetadata struct {
	Path        string `json:"path"`
	Template    string `json:"template,omitempty"`
	HarnessHash string `json:"harnessHash"`
	PlanHash    string `json:"planHash,omitempty"`
	Index       string `json:"index"`
	Status      string `json:"status"`
}

type harnessDocument struct {
	Config      HarnessConfig
	Body        string
	Content     []byte
	HarnessHash string
	PlanHash    string
}

type harnessGrounding struct {
	Version     string               `json:"version"`
	GeneratedAt string               `json:"generatedAt"`
	Repo        harnessGroundingRepo `json:"repo"`
	Harness     HarnessMetadata      `json:"harness"`
	Scope       []string             `json:"scope,omitempty"`
	Command     []string             `json:"command,omitempty"`
	Job         string               `json:"job,omitempty"`
	Label       string               `json:"label,omitempty"`
	Sync        harnessGroundingSync `json:"sync"`
}

type harnessGroundingRepo struct {
	Root         string   `json:"root,omitempty"`
	RemoteURL    string   `json:"remoteUrl,omitempty"`
	Head         string   `json:"head,omitempty"`
	Branch       string   `json:"branch,omitempty"`
	DirtySummary []string `json:"dirtySummary,omitempty"`
}

type harnessGroundingSync struct {
	NoSync     bool     `json:"noSync,omitempty"`
	SyncOnly   bool     `json:"syncOnly,omitempty"`
	Checksum   bool     `json:"checksum,omitempty"`
	ScopePaths []string `json:"scopePaths,omitempty"`
}

type harnessComplianceReport struct {
	Status        string          `json:"status"`
	ExitCode      int             `json:"exitCode"`
	GeneratedAt   string          `json:"generatedAt"`
	Harness       HarnessMetadata `json:"harness"`
	Missing       []string        `json:"missing,omitempty"`
	Evidence      []runArtifact   `json:"evidence,omitempty"`
	Required      map[string]any  `json:"required,omitempty"`
	Command       string          `json:"command,omitempty"`
	ActionsRunURL string          `json:"actionsRunUrl,omitempty"`
}

type fileHarnessConfig struct {
	Path       string                       `yaml:"path,omitempty"`
	Index      string                       `yaml:"index,omitempty"`
	Version    yamlStringValue              `yaml:"version,omitempty"`
	Template   string                       `yaml:"template,omitempty"`
	Job        string                       `yaml:"job,omitempty"`
	PlanFile   string                       `yaml:"plan_file,omitempty"`
	Scope      yamlStringList               `yaml:"scope,omitempty"`
	Validate   *fileHarnessValidateConfig   `yaml:"validate,omitempty"`
	Compliance *fileHarnessComplianceConfig `yaml:"compliance,omitempty"`
}

type fileHarnessValidateConfig struct {
	Commands yamlStringList `yaml:"commands,omitempty"`
}

type fileHarnessComplianceConfig struct {
	RequirePlan       *bool          `yaml:"require_plan,omitempty"`
	RequireJUnit      *bool          `yaml:"require_junit,omitempty"`
	RequiredArtifacts yamlStringList `yaml:"required_artifacts,omitempty"`
}

type yamlStringList []string

type yamlStringValue string

func (v *yamlStringValue) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("expected scalar")
	}
	*v = yamlStringValue(strings.TrimSpace(value.Value))
	return nil
}

func (l *yamlStringList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		item := strings.TrimSpace(value.Value)
		if item != "" {
			*l = []string{item}
		}
	case yaml.SequenceNode:
		var out []string
		for _, node := range value.Content {
			item := strings.TrimSpace(node.Value)
			if item != "" {
				out = append(out, item)
			}
		}
		*l = out
	case 0:
		return nil
	default:
		return fmt.Errorf("expected string or list")
	}
	return nil
}

func applyFileHarnessConfig(h HarnessConfig, file fileHarnessConfig) HarnessConfig {
	if file.Path != "" {
		h.Path = file.Path
	}
	if file.Index != "" {
		h.Index = file.Index
	}
	if file.Version != "" {
		h.Version = string(file.Version)
	}
	if file.Template != "" {
		h.Template = file.Template
	}
	if file.Job != "" {
		h.Job = file.Job
	}
	if file.PlanFile != "" {
		h.PlanFile = file.PlanFile
	}
	if len(file.Scope) > 0 {
		h.Scope = appendUniqueStrings(nil, file.Scope...)
	}
	if file.Validate != nil && len(file.Validate.Commands) > 0 {
		h.Validate.Commands = appendUniqueStrings(nil, file.Validate.Commands...)
	}
	if file.Compliance != nil {
		if file.Compliance.RequirePlan != nil {
			h.Compliance.RequirePlan = *file.Compliance.RequirePlan
		}
		if file.Compliance.RequireJUnit != nil {
			h.Compliance.RequireJUnit = *file.Compliance.RequireJUnit
		}
		if len(file.Compliance.RequiredArtifacts) > 0 {
			h.Compliance.RequiredArtifacts = appendUniqueStrings(nil, file.Compliance.RequiredArtifacts...)
		}
	}
	return h
}

func (a App) harness(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return exit(2, "usage: crabbox harness validate HARNESS.md [--json]")
	}
	switch args[0] {
	case "validate":
		return a.harnessValidate(ctx, args[1:])
	default:
		return exit(2, "unknown harness subcommand %q", args[0])
	}
}

func (a App) harnessValidate(_ context.Context, args []string) error {
	fs := newFlagSet("harness validate", a.Stderr)
	jsonOut := fs.Bool("json", false, "print validation result as JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox harness validate HARNESS.md [--json]")
	}
	doc, err := loadHarnessDocument(HarnessConfig{Path: fs.Arg(0)}, Repo{})
	if err != nil {
		return err
	}
	if *jsonOut {
		out := map[string]any{
			"ok":          true,
			"path":        doc.Config.Path,
			"template":    doc.Config.Template,
			"job":         doc.Config.Job,
			"harnessHash": doc.HarnessHash,
			"planHash":    doc.PlanHash,
		}
		encoder := json.NewEncoder(a.Stdout)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(out)
	}
	fmt.Fprintf(a.Stdout, "harness ok path=%s hash=%s plan_hash=%s\n", doc.Config.Path, doc.HarnessHash, blank(doc.PlanHash, "-"))
	return nil
}

func validateHarnessIndex(index string) error {
	switch strings.TrimSpace(index) {
	case "", harnessIndexNone, harnessIndexLight:
		return nil
	default:
		return exit(2, "--index must be none or light")
	}
}

func effectiveHarnessIndex(path, index string) string {
	index = strings.TrimSpace(index)
	if index != "" {
		return index
	}
	if strings.TrimSpace(path) != "" {
		return harnessIndexLight
	}
	return ""
}

func loadHarnessDocument(defaults HarnessConfig, repo Repo) (*harnessDocument, error) {
	path := strings.TrimSpace(defaults.Path)
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, exit(2, "read harness %s: %v", path, err)
	}
	body, fileConfig, err := parseHarnessMarkdown(data)
	if err != nil {
		return nil, exit(2, "parse harness %s: %v", path, err)
	}
	cfg := applyFileHarnessConfig(defaults, fileConfig)
	cfg.Path = path
	if cfg.Version == "" {
		cfg.Version = "1"
	}
	if err := validateHarnessConfig(cfg); err != nil {
		return nil, err
	}
	planHash, err := harnessPlanHash(cfg, repo, path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return &harnessDocument{
		Config:      cfg,
		Body:        body,
		Content:     data,
		HarnessHash: hex.EncodeToString(sum[:]),
		PlanHash:    planHash,
	}, nil
}

func parseHarnessMarkdown(data []byte) (string, fileHarnessConfig, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return strings.TrimSpace(text), fileHarnessConfig{}, nil
	}
	rest := strings.TrimPrefix(text, "---\n")
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return "", fileHarnessConfig{}, fmt.Errorf("frontmatter is missing closing ---")
	}
	raw := rest[:idx]
	body := strings.TrimPrefix(rest[idx+len("\n---"):], "\n")
	if err := validateHarnessFrontmatterKeys(raw); err != nil {
		return "", fileHarnessConfig{}, err
	}
	var cfg fileHarnessConfig
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		return "", fileHarnessConfig{}, err
	}
	return strings.TrimSpace(body), cfg, nil
}

func validateHarnessFrontmatterKeys(raw string) error {
	var top map[string]any
	if err := yaml.Unmarshal([]byte(raw), &top); err != nil {
		return err
	}
	allowed := map[string]bool{
		"version": true, "template": true, "job": true, "plan_file": true,
		"scope": true, "validate": true, "compliance": true,
	}
	for key := range top {
		if !allowed[key] {
			return fmt.Errorf("unknown frontmatter key %q", key)
		}
	}
	return nil
}

func validateHarnessConfig(cfg HarnessConfig) error {
	if err := validateHarnessIndex(cfg.Index); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Path) == "" {
		return exit(2, "harness path is required")
	}
	return nil
}

func harnessPlanHash(cfg HarnessConfig, repo Repo, harnessPath string) (string, error) {
	planFile := strings.TrimSpace(cfg.PlanFile)
	if planFile == "" {
		return "", nil
	}
	path := planFile
	if !filepath.IsAbs(path) {
		base := filepath.Dir(harnessPath)
		if base == "." && repo.Root != "" {
			base = repo.Root
		}
		path = filepath.Join(base, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", exit(2, "read harness plan_file %s: %v", planFile, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func buildHarnessGrounding(repo Repo, doc *harnessDocument, index string, command []string, job, label string, noSync, syncOnly, checksum bool) harnessGrounding {
	meta := HarnessMetadata{
		Path:        doc.Config.Path,
		Template:    doc.Config.Template,
		HarnessHash: doc.HarnessHash,
		PlanHash:    doc.PlanHash,
		Index:       index,
		Status:      "pending",
	}
	return harnessGrounding{
		Version:     "1",
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Repo: harnessGroundingRepo{
			Root:         repo.Root,
			RemoteURL:    repo.RemoteURL,
			Head:         repo.Head,
			Branch:       gitOutput(repo.Root, "branch", "--show-current"),
			DirtySummary: splitHarnessNonEmptyLines(gitOutput(repo.Root, "status", "--short")),
		},
		Harness: meta,
		Scope:   doc.Config.Scope,
		Command: append([]string{}, command...),
		Job:     strings.TrimSpace(firstNonBlank(job, doc.Config.Job)),
		Label:   strings.TrimSpace(label),
		Sync: harnessGroundingSync{
			NoSync:     noSync,
			SyncOnly:   syncOnly,
			Checksum:   checksum,
			ScopePaths: append([]string{}, doc.Config.Scope...),
		},
	}
}

func splitHarnessNonEmptyLines(value string) []string {
	var out []string
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func writeHarnessRemoteEvidence(ctx context.Context, target SSHTarget, workdir string, doc *harnessDocument, grounding harnessGrounding) error {
	if doc == nil {
		return nil
	}
	groundingJSON, err := json.MarshalIndent(grounding, "", "  ")
	if err != nil {
		return err
	}
	if err := uploadHarnessRemoteFile(ctx, target, workdir, ".crabbox/grounding/harness.md", doc.Content); err != nil {
		return err
	}
	return uploadHarnessRemoteFile(ctx, target, workdir, ".crabbox/grounding/grounding.json", append(groundingJSON, '\n'))
}

func uploadHarnessRemoteFile(ctx context.Context, target SSHTarget, workdir, remotePath string, data []byte) error {
	remote := remoteUploadRunScriptCommand(workdir, remotePath)
	if isWindowsNativeTarget(target) {
		remote = windowsRemoteUploadUTF8BOMFileCommand(workdir, remotePath)
	}
	if err := runSSHInput(ctx, target, remote, strings.NewReader(string(data)), nil, nil); err != nil {
		return exit(7, "upload harness evidence %s: %v", remotePath, err)
	}
	return nil
}

func writeHarnessLocalEvidence(repoRoot, runID, leaseID string, doc *harnessDocument, grounding harnessGrounding, exitCode int, commandDisplay, actionsURL string, artifacts []runArtifact, results *TestResultSummary) ([]runArtifact, *HarnessMetadata, error) {
	if doc == nil {
		return nil, nil, nil
	}
	grounding.Harness.Status = "passed"
	report := buildHarnessComplianceReport(doc, grounding.Harness, exitCode, commandDisplay, actionsURL, artifacts, results)
	if report.Status != "passed" {
		grounding.Harness.Status = report.Status
		report.Harness.Status = report.Status
	}
	groundingJSON, err := json.MarshalIndent(grounding, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	reportJSON, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	written := make([]runArtifact, 0, 4)
	write := func(kind, name string, data []byte) error {
		path := localRunArtifactPath(repoRoot, runID, leaseID, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
		written = append(written, runArtifact{Kind: kind, Path: path, Bytes: len(data)})
		return nil
	}
	if err := write("harness", "harness.md", doc.Content); err != nil {
		return nil, nil, err
	}
	if err := write("grounding", "grounding.json", append(groundingJSON, '\n')); err != nil {
		return nil, nil, err
	}
	if err := write("compliance-json", "compliance-report.json", append(reportJSON, '\n')); err != nil {
		return nil, nil, err
	}
	if err := write("compliance-report", "compliance-report.md", []byte(renderHarnessComplianceMarkdown(report))); err != nil {
		return nil, nil, err
	}
	return written, &report.Harness, nil
}

func buildHarnessComplianceReport(doc *harnessDocument, meta HarnessMetadata, exitCode int, commandDisplay, actionsURL string, artifacts []runArtifact, results *TestResultSummary) harnessComplianceReport {
	missing := []string{}
	if exitCode != 0 {
		missing = append(missing, "command exit 0")
	}
	if doc.Config.Compliance.RequirePlan && doc.PlanHash == "" {
		missing = append(missing, "plan evidence")
	}
	if doc.Config.Compliance.RequireJUnit {
		if results == nil {
			missing = append(missing, "junit evidence")
		} else if results.Failures > 0 || results.Errors > 0 {
			missing = append(missing, "passing junit evidence")
		}
	}
	if len(doc.Config.Compliance.RequiredArtifacts) > 0 && len(artifacts) == 0 {
		for _, artifact := range doc.Config.Compliance.RequiredArtifacts {
			missing = append(missing, "artifact "+artifact)
		}
	}
	status := "passed"
	if len(missing) > 0 {
		status = "failed"
	}
	meta.Status = status
	required := map[string]any{}
	if doc.Config.Compliance.RequirePlan {
		required["plan"] = true
	}
	if doc.Config.Compliance.RequireJUnit {
		required["junit"] = true
	}
	if len(doc.Config.Compliance.RequiredArtifacts) > 0 {
		required["artifacts"] = doc.Config.Compliance.RequiredArtifacts
	}
	return harnessComplianceReport{
		Status:        status,
		ExitCode:      exitCode,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Harness:       meta,
		Missing:       missing,
		Evidence:      artifacts,
		Required:      required,
		Command:       commandDisplay,
		ActionsRunURL: actionsURL,
	}
}

func renderHarnessComplianceMarkdown(report harnessComplianceReport) string {
	var b strings.Builder
	b.WriteString("# Crabbox harness compliance report\n\n")
	b.WriteString("Status: " + report.Status + "\n\n")
	b.WriteString(fmt.Sprintf("Exit code: %d\n\n", report.ExitCode))
	b.WriteString("Harness: `" + report.Harness.Path + "`\n\n")
	b.WriteString("Harness hash: `" + report.Harness.HarnessHash + "`\n\n")
	if report.Harness.PlanHash != "" {
		b.WriteString("Plan hash: `" + report.Harness.PlanHash + "`\n\n")
	}
	if report.Command != "" {
		open, close := markdownFence("sh", report.Command)
		b.WriteString("Command:\n\n" + open + "\n" + report.Command + "\n" + close + "\n\n")
	}
	if len(report.Missing) > 0 {
		b.WriteString("## Missing required evidence\n\n")
		for _, missing := range report.Missing {
			b.WriteString("- " + missing + "\n")
		}
		b.WriteString("\n")
	}
	if len(report.Evidence) > 0 {
		b.WriteString("## Evidence\n\n")
		for _, artifact := range report.Evidence {
			b.WriteString(fmt.Sprintf("- %s: `%s`", artifact.Kind, artifact.Path))
			if artifact.Bytes > 0 {
				b.WriteString(fmt.Sprintf(" (%d bytes)", artifact.Bytes))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}
