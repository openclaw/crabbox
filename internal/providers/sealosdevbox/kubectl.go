package sealosdevbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	devboxGroupVersion = "devbox.sealos.io/v1alpha2"
	devboxResource     = "devboxes.devbox.sealos.io"
	devboxCRD          = "devboxes.devbox.sealos.io"
)

func (b *backend) kubectl(ctx context.Context, stdout io.Writer, namespace bool, args ...string) (string, error) {
	return b.kubectlWithInput(ctx, stdout, nil, namespace, args...)
}

func (b *backend) kubectlWithInput(ctx context.Context, stdout io.Writer, stdin io.Reader, namespace bool, args ...string) (string, error) {
	commandArgs := b.kubeArgs(namespace)
	commandArgs = append(commandArgs, args...)
	runner := b.rt.Exec
	if runner == nil {
		return "", core.Exit(5, "kubectl runner unavailable")
	}
	result, err := runner.Run(ctx, core.LocalCommandRequest{
		Name:   strings.TrimSpace(b.cfg.SealosDevbox.Kubectl),
		Args:   commandArgs,
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: b.rt.Stderr,
	})
	if err != nil {
		message := strings.TrimSpace(result.Stderr)
		if message == "" {
			message = strings.TrimSpace(result.Stdout)
		}
		return "", core.Exit(result.ExitCode, "kubectl failed: %v: %s", err, redactSensitive(message))
	}
	return result.Stdout, nil
}

func (b *backend) kubeArgs(namespace bool) []string {
	cfg := b.cfg.SealosDevbox
	args := []string{}
	if strings.TrimSpace(cfg.Kubeconfig) != "" {
		args = append(args, "--kubeconfig", cfg.Kubeconfig)
	}
	if strings.TrimSpace(cfg.Context) != "" {
		args = append(args, "--context", cfg.Context)
	}
	if namespace && strings.TrimSpace(cfg.Namespace) != "" {
		args = append(args, "--namespace", cfg.Namespace)
	}
	return args
}

func (b *backend) canI(ctx context.Context, verb, resource string) core.DoctorCheck {
	_, err := b.kubectl(ctx, nil, true, "auth", "can-i", verb, resource)
	check := "rbac." + verb + "." + strings.TrimSuffix(resource, ".devbox.sealos.io")
	if err != nil {
		return doctorCheck("failed", check, err.Error(), nil)
	}
	return doctorCheck("ok", check, "allowed", map[string]string{"mutation": "false", "dry_permission_check": "true"})
}

func (b *backend) applyDevbox(ctx context.Context, manifest []byte) error {
	_, err := b.kubectlWithInput(ctx, b.rt.Stdout, strings.NewReader(string(manifest)), true, "apply", "-f", "-")
	return err
}

func (b *backend) listDevboxes(ctx context.Context) ([]devboxItem, error) {
	out, err := b.kubectl(ctx, nil, true, "get", devboxResource, "-l", managedByLabel+"=crabbox", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list devboxList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, core.Exit(5, "sealos-devbox inventory returned invalid JSON: %v", err)
	}
	return list.Items, nil
}

func (b *backend) getDevbox(ctx context.Context, name string) (devboxItem, error) {
	out, err := b.kubectl(ctx, nil, true, "get", devboxResource+"/"+name, "-o", "json")
	if err != nil {
		return devboxItem{}, err
	}
	var item devboxItem
	if err := json.Unmarshal([]byte(out), &item); err != nil {
		return devboxItem{}, core.Exit(5, "sealos-devbox Devbox lookup returned invalid JSON: %v", err)
	}
	return item, nil
}

func (b *backend) getSecret(ctx context.Context, name string) (devboxSecret, error) {
	out, err := b.kubectl(ctx, nil, true, "get", "secret/"+name, "-o", "json")
	if err != nil {
		return devboxSecret{}, err
	}
	var secret devboxSecret
	if err := json.Unmarshal([]byte(out), &secret); err != nil {
		return devboxSecret{}, core.Exit(5, "sealos-devbox Secret lookup returned invalid JSON: %v", err)
	}
	return secret, nil
}

func (b *backend) listEvents(ctx context.Context, name string) ([]devboxEvent, error) {
	out, err := b.kubectl(ctx, nil, true, "get", "events", "--field-selector", "involvedObject.name="+name, "-o", "json")
	if err != nil {
		return nil, err
	}
	var list devboxEventList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, core.Exit(5, "sealos-devbox Events lookup returned invalid JSON: %v", err)
	}
	return list.Items, nil
}

func kubectlNotFound(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "notfound") || strings.Contains(text, "not found") || strings.Contains(text, "notfound")
}

func commandString(req core.LocalCommandRequest) string {
	return strings.TrimSpace(req.Name + " " + strings.Join(req.Args, " "))
}

func redactSensitive(message string) string {
	if strings.TrimSpace(message) == "" {
		return ""
	}
	redacted := regexp.MustCompile(`(?is)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`).ReplaceAllString(message, "[redacted]")
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)(token|password|secret|private[_ -]?key|authorization|bearer)(\s*[=:]\s*)\S+`),
		regexp.MustCompile(`(?i)(client-certificate-data|client-key-data|certificate-authority-data)(\s*[=:]\s*)\S+`),
	}
	for _, pattern := range patterns {
		redacted = pattern.ReplaceAllString(redacted, `${1}${2}[redacted]`)
	}
	return redacted
}

func doctorCheck(status, check, message string, details map[string]string) core.DoctorCheck {
	if details == nil {
		details = map[string]string{}
	}
	if _, ok := details["mutation"]; !ok {
		details["mutation"] = "false"
	}
	return core.DoctorCheck{
		Status:  status,
		Check:   check,
		Message: redactSensitive(message),
		Details: details,
	}
}

func unsupportedLifecycleError(operation string) error {
	return core.Exit(2, "sealos-devbox %s is deferred to the CRD lifecycle plan; run `crabbox doctor --provider sealos-devbox` to verify prerequisites first", operation)
}

func formatDoctorSummary(checks []core.DoctorCheck) string {
	status := "ready"
	for _, check := range checks {
		if strings.EqualFold(check.Status, "failed") || strings.EqualFold(check.Status, "missing") {
			status = "blocked"
			break
		}
	}
	return fmt.Sprintf("automation_surface=%s control_plane=%s mutation=false", AutomationSurfaceDecision, status)
}
