package sealosdevbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
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
	// kubectl and kubeconfig exec plugins may print credentials to stderr.
	// Keep diagnostics private until the error path can redact them.
	var stderr bytes.Buffer
	result, err := runner.Run(ctx, core.LocalCommandRequest{
		Name:   strings.TrimSpace(b.cfg.SealosDevbox.Kubectl),
		Args:   commandArgs,
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: &stderr,
	})
	if err != nil {
		message := strings.TrimSpace(result.Stderr)
		if message == "" {
			message = strings.TrimSpace(stderr.String())
		}
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

type resourceRule struct {
	Verbs         []string `json:"verbs"`
	APIGroups     []string `json:"apiGroups"`
	Resources     []string `json:"resources"`
	ResourceNames []string `json:"resourceNames"`
}

func (b *backend) permissionRules(ctx context.Context) ([]resourceRule, error) {
	request, err := json.Marshal(map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectRulesReview",
		"spec": map[string]string{
			"namespace": b.cfg.SealosDevbox.Namespace,
		},
	})
	if err != nil {
		return nil, err
	}
	out, err := b.kubectlWithInput(ctx, nil, bytes.NewReader(request), false,
		"create", "--raw", "/apis/authorization.k8s.io/v1/selfsubjectrulesreviews", "-f", "-")
	if err != nil {
		return nil, err
	}
	var review struct {
		Status struct {
			ResourceRules   []resourceRule `json:"resourceRules"`
			Incomplete      bool           `json:"incomplete"`
			EvaluationError string         `json:"evaluationError"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &review); err != nil {
		return nil, core.Exit(5, "sealos-devbox permission review returned invalid JSON: %v", err)
	}
	if review.Status.Incomplete || strings.TrimSpace(review.Status.EvaluationError) != "" {
		return nil, core.Exit(5, "sealos-devbox permission review incomplete")
	}
	return review.Status.ResourceRules, nil
}

func canIWithRules(rules []resourceRule, verb, resource string) core.DoctorCheck {
	check := "rbac." + verb + "." + strings.TrimSuffix(resource, ".devbox.sealos.io")
	resourceName, apiGroup := splitAPIResource(resource)
	if !rulesAllow(rules, verb, apiGroup, resourceName) {
		return doctorCheck("failed", check, "denied", map[string]string{"allowed": "false"})
	}
	return doctorCheck("ok", check, "allowed", map[string]string{"mutation": "false", "dry_permission_check": "true"})
}

func splitAPIResource(resource string) (string, string) {
	name, group, found := strings.Cut(resource, ".")
	if !found {
		return resource, ""
	}
	return name, group
}

func rulesAllow(rules []resourceRule, verb, apiGroup, resource string) bool {
	for _, rule := range rules {
		if len(rule.ResourceNames) != 0 {
			continue
		}
		if stringSetContains(rule.Verbs, verb) && stringSetContains(rule.APIGroups, apiGroup) && stringSetContains(rule.Resources, resource) {
			return true
		}
	}
	return false
}

func stringSetContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == "*" || candidate == value {
			return true
		}
	}
	return false
}

func (b *backend) createDevbox(ctx context.Context, manifest []byte) error {
	_, err := b.kubectlWithInput(ctx, b.rt.Stdout, strings.NewReader(string(manifest)), true, "create", "-f", "-")
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
		if kubectlAPIObjectNotFound(err) {
			return devboxItem{}, kubernetesObjectNotFoundError{err: err}
		}
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

func (b *backend) patchDevboxState(ctx context.Context, name, resourceVersion, state string, annotations map[string]any) error {
	return b.patchDevbox(ctx, name, resourceVersion, state, annotations)
}

func (b *backend) patchDevboxAnnotations(ctx context.Context, name, resourceVersion string, annotations map[string]any) error {
	return b.patchDevbox(ctx, name, resourceVersion, "", annotations)
}

func (b *backend) patchDevbox(ctx context.Context, name, resourceVersion, state string, annotations map[string]any) error {
	resourceVersion = strings.TrimSpace(resourceVersion)
	if resourceVersion == "" {
		return core.Exit(4, "refusing to patch Sealos DevBox %q without its Kubernetes resourceVersion", name)
	}
	patch := map[string]any{
		"metadata": map[string]any{"resourceVersion": resourceVersion},
	}
	if strings.TrimSpace(state) != "" {
		patch["spec"] = map[string]any{"state": state}
	}
	if len(annotations) > 0 {
		patch["metadata"].(map[string]any)["annotations"] = annotations
	}
	payload, err := json.Marshal(patch)
	if err != nil {
		return core.Exit(5, "encode Sealos DevBox patch: %v", err)
	}
	_, err = b.kubectl(ctx, b.rt.Stdout, true, "patch", devboxResource+"/"+name, "--type", "merge", "-p", string(payload))
	return err
}

func (b *backend) deleteDevbox(ctx context.Context, item devboxItem) error {
	name := strings.TrimSpace(item.Metadata.Name)
	namespace := strings.TrimSpace(item.Metadata.Namespace)
	if namespace == "" {
		namespace = strings.TrimSpace(b.cfg.SealosDevbox.Namespace)
	}
	uid := strings.TrimSpace(item.Metadata.UID)
	resourceVersion := strings.TrimSpace(item.Metadata.ResourceVersion)
	if name == "" || namespace == "" || uid == "" || resourceVersion == "" {
		return core.Exit(4, "refusing to delete Sealos DevBox without exact name, namespace, UID, and resourceVersion")
	}
	payload, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "DeleteOptions",
		"preconditions": map[string]string{
			"uid":             uid,
			"resourceVersion": resourceVersion,
		},
		"propagationPolicy": "Background",
	})
	if err != nil {
		return core.Exit(5, "encode Sealos DevBox delete preconditions: %v", err)
	}
	rawPath := "/apis/devbox.sealos.io/v1alpha2/namespaces/" + url.PathEscape(namespace) + "/devboxes/" + url.PathEscape(name)
	// kubectl raw delete sends -f - as the DELETE request body, preserving these preconditions.
	_, err = b.kubectlWithInput(ctx, nil, strings.NewReader(string(payload)), false, "delete", "--raw", rawPath, "-f", "-")
	if err != nil && kubectlAPIObjectNotFound(err) {
		return nil
	}
	return err
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

type kubernetesObjectNotFoundError struct {
	err error
}

func (e kubernetesObjectNotFoundError) Error() string {
	return e.err.Error()
}

func (e kubernetesObjectNotFoundError) Unwrap() error {
	return e.err
}

func kubernetesObjectNotFound(err error) bool {
	var notFound kubernetesObjectNotFoundError
	return errors.As(err, &notFound)
}

func kubectlNotFound(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "notfound") || strings.Contains(text, "not found") || strings.Contains(text, "notfound")
}

func kubectlAPIObjectNotFound(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "error from server (notfound):") && strings.Contains(text, " not found")
}

func kubectlAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "alreadyexists") || strings.Contains(text, "already exists")
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
