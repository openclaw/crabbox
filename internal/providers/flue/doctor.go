package flue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const flueDoctorOutputLimitBytes = 64 * 1024

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	result := DoctorResult{Provider: providerName}
	checks := []DoctorCheck{
		b.doctorRootCheck(),
		b.doctorOptionalFileCheck("config", b.cfg.Flue.Config),
		b.doctorOptionalFileCheck("env_file", b.cfg.Flue.EnvFile),
		b.doctorOutputCheck(),
		b.doctorTargetCheck(),
		b.doctorWorkflowCheck(),
	}
	help, helpErr := b.doctorRun(ctx, []string{"--help"})
	checks = append([]DoctorCheck{doctorCLIHelpCheck(b.cfg, help, helpErr)}, checks...)
	version, versionErr := b.doctorRun(ctx, []string{"--version"})
	checks = append(checks, doctorCLIVersionCheck(version, versionErr))
	result.Checks = checks
	if doctorChecksFailed(checks) {
		result.Status = "error"
		result.Message = "cli=blocked target=" + flueDoctorTarget(b.cfg) + " workflow=" + flueDoctorWorkflow(b.cfg) + " mutation=false"
		return result, nil
	}
	result.Status = "ok"
	result.Message = "cli=ready target=node workflow=" + flueDoctorWorkflow(b.cfg) + " workflow_discovery=unchecked mutation=false"
	return result, nil
}

func (b *backend) doctorRun(ctx context.Context, args []string) (LocalCommandResult, error) {
	if b.rt.Exec == nil {
		return LocalCommandResult{}, exit(2, "provider=%s requires Runtime.Exec to invoke the Flue CLI", providerName)
	}
	return b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:                   flueDoctorCLIPath(b.cfg),
		Args:                   args,
		Dir:                    flueDoctorCommandDir(b.cfg),
		MaxCapturedOutputBytes: flueDoctorOutputLimitBytes,
		CancelGracePeriod:      2 * time.Second,
	})
}

func (b *backend) doctorRootCheck() DoctorCheck {
	root := strings.TrimSpace(b.cfg.Flue.Root)
	details := map[string]string{"mutation": "false"}
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return DoctorCheck{Status: "warning", Check: "flue_root", Message: "root unset; current working directory could not be resolved", Details: details}
		}
		details["mode"] = "cwd"
		details["path"] = cwd
		return DoctorCheck{Status: "ok", Check: "flue_root", Message: "root unset; Flue will use the current working directory", Details: details}
	}
	path := flueDoctorResolvePath("", root)
	details["path"] = path
	info, err := os.Stat(path)
	if err != nil {
		return DoctorCheck{Status: "failed", Check: "flue_root", Message: "configured Flue root is not readable: " + err.Error(), Details: details}
	}
	if !info.IsDir() {
		return DoctorCheck{Status: "failed", Check: "flue_root", Message: "configured Flue root is not a directory", Details: details}
	}
	return DoctorCheck{Status: "ok", Check: "flue_root", Message: "ready", Details: details}
}

func (b *backend) doctorOptionalFileCheck(name, value string) DoctorCheck {
	details := map[string]string{"mutation": "false"}
	value = strings.TrimSpace(value)
	if value == "" {
		details["configured"] = "false"
		return DoctorCheck{Status: "ok", Check: name, Message: "not configured", Details: details}
	}
	path := flueDoctorResolvePath(b.cfg.Flue.Root, value)
	details["configured"] = "true"
	details["path"] = path
	info, err := os.Stat(path)
	if err != nil {
		return DoctorCheck{Status: "failed", Check: name, Message: "configured path is not readable: " + err.Error(), Details: details}
	}
	if info.IsDir() {
		return DoctorCheck{Status: "failed", Check: name, Message: "configured path is a directory; expected a file", Details: details}
	}
	file, err := os.Open(path)
	if err != nil {
		return DoctorCheck{Status: "failed", Check: name, Message: "configured path is not readable: " + err.Error(), Details: details}
	}
	_ = file.Close()
	return DoctorCheck{Status: "ok", Check: name, Message: "ready", Details: details}
}

func (b *backend) doctorOutputCheck() DoctorCheck {
	output := strings.TrimSpace(b.cfg.Flue.Output)
	details := map[string]string{"mutation": "false"}
	if output == "" {
		details["configured"] = "false"
		return DoctorCheck{Status: "ok", Check: "output", Message: "using Flue default output mode", Details: details}
	}
	details["configured"] = "true"
	details["value"] = output
	if strings.ContainsRune(output, '\x00') {
		return DoctorCheck{Status: "failed", Check: "output", Message: "output mode contains a NUL byte", Details: details}
	}
	return DoctorCheck{Status: "ok", Check: "output", Message: "configured", Details: details}
}

func (b *backend) doctorTargetCheck() DoctorCheck {
	target := flueDoctorTarget(b.cfg)
	details := map[string]string{"target": target, "mutation": "false"}
	if target != defaultTarget {
		return DoctorCheck{
			Status:  "failed",
			Check:   "target",
			Message: fmt.Sprintf("provider=%s supports flue target=node only in v1; upload/HTTP staging is required before %q can be used", providerName, target),
			Details: details,
		}
	}
	return DoctorCheck{Status: "ok", Check: "target", Message: "node", Details: details}
}

func (b *backend) doctorWorkflowCheck() DoctorCheck {
	workflow := flueDoctorWorkflow(b.cfg)
	details := map[string]string{
		"workflow":        workflow,
		"discoverability": "unchecked",
		"reason":          "safe_flue_discovery_unavailable",
		"mutation":        "false",
	}
	if workflow == "" {
		return DoctorCheck{Status: "failed", Check: "workflow", Message: "flue workflow must not be empty", Details: details}
	}
	return DoctorCheck{
		Status:  "warning",
		Check:   "workflow",
		Message: "workflow existence is unchecked because running Flue workflows may execute user code",
		Details: details,
	}
}

func doctorCLIHelpCheck(cfg Config, result LocalCommandResult, err error) DoctorCheck {
	details := map[string]string{
		"cli":      flueDoctorCLIPath(cfg),
		"mutation": "false",
	}
	if root := strings.TrimSpace(cfg.Flue.Root); root != "" {
		details["root"] = flueDoctorResolvePath("", root)
	}
	if err != nil || result.ExitCode != 0 {
		return DoctorCheck{Status: "failed", Check: "flue_help", Message: flueDoctorFailure(result, err), Details: details}
	}
	if line := firstNonEmptyLine(result.Stdout); line != "" {
		details["help"] = line
	}
	return DoctorCheck{Status: "ok", Check: "flue_help", Message: "ready", Details: details}
}

func doctorCLIVersionCheck(result LocalCommandResult, err error) DoctorCheck {
	details := map[string]string{"authoritative": "false", "optional": "true", "mutation": "false"}
	if err != nil || result.ExitCode != 0 {
		return DoctorCheck{Status: "warning", Check: "flue_version", Message: flueDoctorFailure(result, err), Details: details}
	}
	version := firstNonEmptyLine(result.Stdout)
	if version == "" {
		version = strings.TrimSpace(result.Stderr)
	}
	if version == "" {
		version = "reported"
	}
	details["version"] = version
	return DoctorCheck{Status: "ok", Check: "flue_version", Message: "version reported; compatibility is based on command-surface checks", Details: details}
}

func flueDoctorFailure(result LocalCommandResult, err error) string {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail == "" && err != nil {
		detail = err.Error()
	}
	if detail == "" {
		detail = "command failed"
	}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(detail, err.Error()) {
		detail += ": " + err.Error()
	}
	return tailString(detail, 4096)
}

func flueDoctorResolvePath(root, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if filepath.IsAbs(value) {
		if abs, err := filepath.Abs(value); err == nil {
			return abs
		}
		return value
	}
	if strings.TrimSpace(root) != "" {
		value = filepath.Join(root, value)
	}
	if abs, err := filepath.Abs(value); err == nil {
		return abs
	}
	return value
}

func flueDoctorCLIPath(cfg Config) string {
	return blank(strings.TrimSpace(cfg.Flue.CLIPath), defaultCLIPath)
}

func flueDoctorCommandDir(cfg Config) string {
	root := strings.TrimSpace(cfg.Flue.Root)
	if root == "" {
		return ""
	}
	path := flueDoctorResolvePath("", root)
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return ""
	}
	return path
}

func flueDoctorTarget(cfg Config) string {
	return strings.ToLower(blank(strings.TrimSpace(cfg.Flue.Target), defaultTarget))
}

func flueDoctorWorkflow(cfg Config) string {
	return strings.TrimSpace(cfg.Flue.Workflow)
}

func doctorChecksFailed(checks []DoctorCheck) bool {
	for _, check := range checks {
		switch strings.ToLower(strings.TrimSpace(check.Status)) {
		case "failed", "missing", "error":
			return true
		}
	}
	return false
}

func firstNonEmptyLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}
