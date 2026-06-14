package agentsandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	errNotReady             = errors.New("not ready")
	errKubernetesNotFound   = errors.New("kubernetes object not found")
	errSandboxClaimNotFound = fmt.Errorf("SandboxClaim not found: %w", errKubernetesNotFound)
)

const (
	kubectlCaptureLimitBytes     = 8 << 20
	kubectlErrorDetailLimitBytes = 16 << 10
)

var kubernetesDNSLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)

type resourceRef struct {
	GroupVersion string
	Resource     string
}

func (r resourceRef) qualifiedResource() string {
	group, version, found := strings.Cut(r.GroupVersion, "/")
	if !found || group == "" {
		return r.Resource
	}
	return r.Resource + "." + version + "." + group
}

type objectMeta struct {
	Name            string            `json:"name,omitempty"`
	Namespace       string            `json:"namespace,omitempty"`
	UID             string            `json:"uid,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	OwnerReferences []ownerReference  `json:"ownerReferences,omitempty"`
}

type ownerReference struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Name       string `json:"name,omitempty"`
	UID        string `json:"uid,omitempty"`
	Controller bool   `json:"controller,omitempty"`
}

type resourceIdentityError struct {
	err error
}

func (e resourceIdentityError) Error() string {
	return e.err.Error()
}

func (e resourceIdentityError) Unwrap() error {
	return e.err
}

type resourceTerminalError struct {
	err error
}

func (e resourceTerminalError) Error() string {
	return e.err.Error()
}

func (e resourceTerminalError) Unwrap() error {
	return e.err
}

type sandboxExpiredError struct {
	err error
}

func (e sandboxExpiredError) Error() string {
	return e.err.Error()
}

func (e sandboxExpiredError) Unwrap() error {
	return e.err
}

type conditionState struct {
	Type    string `json:"type,omitempty"`
	Status  string `json:"status,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type objectStatus struct {
	Sandbox struct {
		Name string `json:"name,omitempty"`
	} `json:"sandbox,omitempty"`
	Selector   string           `json:"selector,omitempty"`
	Phase      string           `json:"phase,omitempty"`
	PodIP      string           `json:"podIP,omitempty"`
	Conditions []conditionState `json:"conditions,omitempty"`
}

type kubernetesObject struct {
	APIVersion string         `json:"apiVersion,omitempty"`
	Kind       string         `json:"kind,omitempty"`
	Metadata   objectMeta     `json:"metadata"`
	Spec       map[string]any `json:"spec,omitempty"`
	Status     objectStatus   `json:"status,omitempty"`
}

type kubernetesObjectList struct {
	Items []kubernetesObject `json:"items"`
}

type apiResourceList struct {
	Resources []struct {
		Name string `json:"name"`
	} `json:"resources"`
}

type kubernetesClient interface {
	CheckResource(ctx context.Context, groupVersion, resource string) error
	Get(ctx context.Context, ref resourceRef, namespace, name string) (*kubernetesObject, error)
	Create(ctx context.Context, ref resourceRef, namespace string, object *kubernetesObject) (*kubernetesObject, error)
	Delete(ctx context.Context, ref resourceRef, namespace, name, uid string) error
	CanI(ctx context.Context, rule rbacRule) (bool, error)
	GetPod(ctx context.Context, namespace, name string) (podState, error)
	ListPods(ctx context.Context, namespace, selector string) ([]podState, error)
	Exec(ctx context.Context, req podExecRequest) error
}

type kubectlKubernetesClient struct {
	runner   CommandRunner
	kubectl  string
	baseArgs []string
}

type kubernetesCreateError struct {
	err       error
	ambiguous bool
}

func (e *kubernetesCreateError) Error() string {
	return e.err.Error()
}

func (e *kubernetesCreateError) Unwrap() error {
	return e.err
}

func createMayHaveSucceeded(err error) bool {
	var createErr *kubernetesCreateError
	if errors.As(err, &createErr) {
		return createErr.ambiguous
	}
	// Other client implementations do not expose transport classification.
	return true
}

func newKubernetesClient(ctx context.Context, cfg Config, rt Runtime) (kubernetesClient, error) {
	_ = ctx
	if rt.Exec == nil {
		return nil, fmt.Errorf("agent-sandbox provider requires a command runner")
	}

	values := cfg.AgentSandbox
	if err := validateKubeconfigInputs(values.Kubeconfig); err != nil {
		return nil, err
	}
	kubectl := strings.TrimSpace(values.Kubectl)
	if kubectl == "" {
		kubectl = "kubectl"
	}

	baseArgs := make([]string, 0, 4)
	if kubeconfig := expandHomePath(values.Kubeconfig); kubeconfig != "" {
		baseArgs = append(baseArgs, "--kubeconfig="+kubeconfig)
	}
	if contextName := strings.TrimSpace(values.Context); contextName != "" {
		baseArgs = append(baseArgs, "--context="+contextName)
	}

	return &kubectlKubernetesClient{
		runner:   rt.Exec,
		kubectl:  kubectl,
		baseArgs: baseArgs,
	}, nil
}

func expandHomePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func validateKubeconfigInputs(configured string) error {
	if kubeconfig := expandHomePath(configured); kubeconfig != "" {
		if !filepath.IsAbs(kubeconfig) {
			return exit(2, "agent-sandbox kubeconfig %q must be absolute after home expansion", configured)
		}
		return nil
	}
	raw, ok := os.LookupEnv("KUBECONFIG")
	if !ok || raw == "" {
		return nil
	}
	for _, kubeconfig := range filepath.SplitList(raw) {
		if kubeconfig == "" {
			continue
		}
		if !filepath.IsAbs(kubeconfig) {
			return exit(2, "agent-sandbox KUBECONFIG entry %q must be absolute", kubeconfig)
		}
	}
	return nil
}

func (c *kubectlKubernetesClient) CheckResource(ctx context.Context, groupVersion, resource string) error {
	group, version, found := strings.Cut(strings.TrimSpace(groupVersion), "/")
	if !found || group == "" || version == "" {
		return fmt.Errorf("agent-sandbox API version must be group/version, got %q", groupVersion)
	}
	result, err := c.run(ctx, LocalCommandRequest{}, "get", "--raw", "/apis/"+group+"/"+version)
	if err != nil {
		return c.commandError("discover "+groupVersion, result, err)
	}

	var resources apiResourceList
	if err := json.Unmarshal([]byte(result.Stdout), &resources); err != nil {
		return fmt.Errorf("decode Kubernetes discovery for %s: %w", groupVersion, err)
	}
	for _, candidate := range resources.Resources {
		if candidate.Name == resource {
			return nil
		}
	}
	return fmt.Errorf("Kubernetes resource %s is not served by %s", resource, groupVersion)
}

func (c *kubectlKubernetesClient) Get(
	ctx context.Context,
	ref resourceRef,
	namespace, name string,
) (*kubernetesObject, error) {
	if err := validateKubernetesObjectName(ref.qualifiedResource(), name); err != nil {
		return nil, err
	}
	result, err := c.run(
		ctx,
		LocalCommandRequest{},
		"get", ref.qualifiedResource()+"/"+name,
		"--namespace="+namespace,
		"--ignore-not-found=true",
		"-o", "json",
	)
	if err != nil {
		return nil, c.commandError("get "+ref.qualifiedResource()+"/"+name, result, err)
	}
	if len(bytes.TrimSpace([]byte(result.Stdout))) == 0 {
		return nil, fmt.Errorf("%s/%s: %w", ref.qualifiedResource(), name, errKubernetesNotFound)
	}

	var object kubernetesObject
	if err := json.Unmarshal([]byte(result.Stdout), &object); err != nil {
		return nil, fmt.Errorf("decode %s/%s: %w", ref.qualifiedResource(), name, err)
	}
	return &object, nil
}

func (c *kubectlKubernetesClient) Create(
	ctx context.Context,
	ref resourceRef,
	namespace string,
	object *kubernetesObject,
) (*kubernetesObject, error) {
	if err := validateKubernetesObjectName(ref.qualifiedResource(), object.Metadata.Name); err != nil {
		return nil, &kubernetesCreateError{err: err, ambiguous: false}
	}
	manifest, err := json.Marshal(object)
	if err != nil {
		return nil, &kubernetesCreateError{
			err:       fmt.Errorf("encode %s/%s: %w", ref.qualifiedResource(), object.Metadata.Name, err),
			ambiguous: false,
		}
	}

	result, err := c.run(
		ctx,
		LocalCommandRequest{Stdin: bytes.NewReader(manifest)},
		"create", "--namespace="+namespace, "-f", "-", "-o", "json",
	)
	if err != nil {
		return nil, &kubernetesCreateError{
			err:       c.commandError("create "+ref.qualifiedResource()+"/"+object.Metadata.Name, result, err),
			ambiguous: kubectlCreateMayHaveSucceeded(result, err),
		}
	}

	var created kubernetesObject
	if err := json.Unmarshal([]byte(result.Stdout), &created); err != nil {
		return nil, &kubernetesCreateError{
			err:       fmt.Errorf("decode created %s/%s: %w", ref.qualifiedResource(), object.Metadata.Name, err),
			ambiguous: true,
		}
	}
	return &created, nil
}

func kubectlCreateMayHaveSucceeded(result LocalCommandResult, err error) bool {
	detail := strings.ToLower(strings.Join([]string{result.Stderr, result.Stdout, err.Error()}, "\n"))
	if strings.Contains(detail, "alreadyexists") || strings.Contains(detail, "already exists") {
		return true
	}
	for _, marker := range []string{
		"error from server (forbidden)",
		"error from server (unauthorized)",
		"error from server (invalid)",
		"error from server (badrequest)",
		"error from server (notfound)",
		"error validating",
		"unable to recognize",
		"no matches for kind",
		"doesn't have a resource type",
		"executable file not found",
		"no such file or directory",
		"no such host",
		"connection refused",
		"certificate",
		"x509:",
	} {
		if strings.Contains(detail, marker) {
			return false
		}
	}
	return true
}

func (c *kubectlKubernetesClient) Delete(
	ctx context.Context,
	ref resourceRef,
	namespace, name, uid string,
) error {
	if err := validateKubernetesObjectName(ref.qualifiedResource(), name); err != nil {
		return err
	}
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return fmt.Errorf("delete %s/%s requires a Kubernetes UID precondition", ref.qualifiedResource(), name)
	}
	options, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "DeleteOptions",
		"preconditions": map[string]string{
			"uid": uid,
		},
		"propagationPolicy": "Background",
	})
	if err != nil {
		return fmt.Errorf("encode delete options for %s/%s: %w", ref.qualifiedResource(), name, err)
	}
	result, err := c.run(
		ctx,
		LocalCommandRequest{Stdin: bytes.NewReader(options)},
		"delete", "--raw", resourceURL(ref, namespace, name), "-f", "-",
	)
	if err != nil {
		if kubectlNotFound(result) {
			return fmt.Errorf("%s/%s: %w", ref.qualifiedResource(), name, errKubernetesNotFound)
		}
		return c.commandError("delete "+ref.qualifiedResource()+"/"+name, result, err)
	}
	return nil
}

func resourceURL(ref resourceRef, namespace, name string) string {
	group, version, found := strings.Cut(ref.GroupVersion, "/")
	prefix := "/api/" + url.PathEscape(ref.GroupVersion)
	if found && group != "" {
		prefix = "/apis/" + url.PathEscape(group) + "/" + url.PathEscape(version)
	}
	return prefix + "/namespaces/" + url.PathEscape(namespace) + "/" + url.PathEscape(ref.Resource) + "/" + url.PathEscape(name)
}

func kubectlNotFound(result LocalCommandResult) bool {
	detail := strings.ToLower(result.Stderr + "\n" + result.Stdout)
	return strings.Contains(detail, "(notfound)") ||
		strings.Contains(detail, "\"code\":404") ||
		strings.Contains(detail, "\"reason\":\"notfound\"")
}

func (c *kubectlKubernetesClient) CanI(ctx context.Context, rule rbacRule) (bool, error) {
	qualified := rule.Resource
	if strings.TrimSpace(rule.Group) != "" {
		qualified += "." + rule.Group
	}
	for _, verb := range rule.Verbs {
		args := []string{"auth", "can-i", verb, qualified, "--namespace=" + rule.Namespace}
		if strings.TrimSpace(rule.Subresource) != "" {
			args = append(args, "--subresource="+rule.Subresource)
		}
		result, err := c.run(ctx, LocalCommandRequest{}, args...)
		allowed, recognized := parseKubectlCanI(result.Stdout)
		if err != nil {
			if result.ExitCode == 1 && recognized && !allowed {
				return false, nil
			}
			return false, c.commandError("check Kubernetes authorization", result, err)
		}
		if recognized {
			if !allowed {
				return false, nil
			}
			continue
		}
		return false, fmt.Errorf("unexpected kubectl auth can-i response %q", strings.TrimSpace(result.Stdout))
	}
	return true, nil
}

func parseKubectlCanI(output string) (allowed, recognized bool) {
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return false, false
	}
	switch fields[0] {
	case "yes":
		return true, true
	case "no":
		return false, true
	default:
		return false, false
	}
}

func (c *kubectlKubernetesClient) GetPod(ctx context.Context, namespace, name string) (podState, error) {
	object, err := c.Get(ctx, resourceRef{GroupVersion: "v1", Resource: "pods"}, namespace, name)
	if err != nil {
		return podState{}, err
	}
	return podStateFromObject(*object), nil
}

func (c *kubectlKubernetesClient) ListPods(
	ctx context.Context,
	namespace, selector string,
) ([]podState, error) {
	result, err := c.run(
		ctx,
		LocalCommandRequest{},
		"get", "pods",
		"--namespace="+namespace,
		"--selector="+selector,
		"-o", "json",
	)
	if err != nil {
		return nil, c.commandError("list pods", result, err)
	}

	var list kubernetesObjectList
	if err := json.Unmarshal([]byte(result.Stdout), &list); err != nil {
		return nil, fmt.Errorf("decode pods: %w", err)
	}
	pods := make([]podState, 0, len(list.Items))
	for _, object := range list.Items {
		pods = append(pods, podStateFromObject(object))
	}
	return pods, nil
}

type podExecRequest struct {
	Namespace string
	Pod       string
	Container string
	Command   []string
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
}

func (c *kubectlKubernetesClient) Exec(ctx context.Context, req podExecRequest) error {
	if err := validateKubernetesObjectName("pod", req.Pod); err != nil {
		return err
	}
	if container := strings.TrimSpace(req.Container); container != "" && !isKubernetesDNSLabel(container) {
		return fmt.Errorf("agent-sandbox pod container name %q is invalid", req.Container)
	}
	args := []string{"exec", "--namespace=" + req.Namespace}
	if req.Stdin != nil {
		args = append(args, "-i")
	}
	args = append(args, "pod/"+req.Pod)
	if strings.TrimSpace(req.Container) != "" {
		args = append(args, "--container="+req.Container)
	}
	args = append(args, "--")
	args = append(args, req.Command...)

	var stderrTail tailBuffer
	stderrTail.limit = kubectlErrorDetailLimitBytes
	stderr := io.Writer(&stderrTail)
	if req.Stderr != nil {
		stderr = io.MultiWriter(req.Stderr, &stderrTail)
	}
	result, err := c.run(ctx, LocalCommandRequest{
		Stdin:                req.Stdin,
		Stdout:               req.Stdout,
		Stderr:               stderr,
		DisableOutputCapture: true,
	}, args...)
	if err == nil {
		return nil
	}
	commandErr := c.commandError("exec in pod "+req.Pod, LocalCommandResult{
		ExitCode: result.ExitCode,
		Stderr:   stderrTail.String(),
	}, err)
	if code, ok := kubectlRemoteExitStatus(stderrTail.String(), result.ExitCode); ok {
		return kubectlExitError{code: code, err: commandErr}
	}
	return commandErr
}

func (c *kubectlKubernetesClient) run(
	ctx context.Context,
	req LocalCommandRequest,
	args ...string,
) (LocalCommandResult, error) {
	req.Name = c.kubectl
	req.Args = append(append([]string(nil), c.baseArgs...), args...)
	if !req.DisableOutputCapture && req.MaxCapturedOutputBytes == 0 {
		req.MaxCapturedOutputBytes = kubectlCaptureLimitBytes
	}
	return c.runner.Run(ctx, req)
}

func (c *kubectlKubernetesClient) commandError(operation string, result LocalCommandResult, err error) error {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail == "" {
		return fmt.Errorf("%s with %s: %w", operation, c.kubectl, err)
	}
	if len(detail) > kubectlErrorDetailLimitBytes {
		detail = detail[:kubectlErrorDetailLimitBytes] + "\n[truncated]"
	}
	return fmt.Errorf("%s with %s: %w: %s", operation, c.kubectl, err, detail)
}

type kubectlExitError struct {
	code int
	err  error
}

func (e kubectlExitError) Error() string {
	return e.err.Error()
}

func (e kubectlExitError) Unwrap() error {
	return e.err
}

func (e kubectlExitError) ExitStatus() int {
	return e.code
}

func kubectlRemoteExitStatus(stderr string, processExitCode int) (int, bool) {
	const prefix = "command terminated with exit code "
	detail := strings.TrimSpace(stderr)
	index := strings.LastIndex(detail, prefix)
	if index < 0 {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(detail[index+len(prefix):]))
	if err != nil {
		return 0, false
	}
	if code <= 0 || code != processExitCode {
		return 0, false
	}
	return code, true
}

type tailBuffer struct {
	data  []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return len(p), nil
	}
	if len(p) >= b.limit {
		b.data = append(b.data[:0], p[len(p)-b.limit:]...)
		return len(p), nil
	}
	overflow := len(b.data) + len(p) - b.limit
	if overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *tailBuffer) String() string {
	return string(b.data)
}

func podStateFromObject(object kubernetesObject) podState {
	state := podState{
		Name:            object.Metadata.Name,
		UID:             object.Metadata.UID,
		Labels:          cloneStringMap(object.Metadata.Labels),
		Annotations:     cloneStringMap(object.Metadata.Annotations),
		OwnerReferences: append([]ownerReference(nil), object.Metadata.OwnerReferences...),
		Phase:           object.Status.Phase,
		PodIP:           object.Status.PodIP,
		Conditions:      append([]conditionState(nil), object.Status.Conditions...),
	}
	for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
		if containers, ok := object.Spec[field].([]any); ok {
			for _, item := range containers {
				container, _ := item.(map[string]any)
				name, _ := container["name"].(string)
				if name = strings.TrimSpace(name); name != "" {
					state.Containers = append(state.Containers, name)
				}
			}
		}
	}
	for _, condition := range state.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" {
			state.Ready = true
			break
		}
	}
	return state
}

type podState struct {
	Name            string
	UID             string
	Labels          map[string]string
	Annotations     map[string]string
	OwnerReferences []ownerReference
	Containers      []string
	Phase           string
	PodIP           string
	Ready           bool
	Conditions      []conditionState
}

func sandboxGVR() resourceRef {
	return resourceRef{GroupVersion: agentSandboxCoreGroupVersion, Resource: sandboxResource}
}

func sandboxClaimGVR() resourceRef {
	return resourceRef{GroupVersion: agentSandboxExtensionsGroupVersion, Resource: sandboxClaimResource}
}

func warmPoolGVR() resourceRef {
	return resourceRef{GroupVersion: agentSandboxExtensionsGroupVersion, Resource: warmPoolResource}
}

type sandboxReadiness struct {
	ClaimName   string
	ClaimUID    string
	SandboxName string
	SandboxUID  string
	PodName     string
	PodUID      string
	PodIP       string
	Container   string
	identity    claimIdentity
}

type sandboxResourceReadiness struct {
	ClaimName   string
	SandboxName string
	Sandbox     *kubernetesObject
}

func claimSandboxName(claim *kubernetesObject) (string, error) {
	name := strings.TrimSpace(claim.Status.Sandbox.Name)
	if name == "" {
		return "", fmt.Errorf("%w: SandboxClaim %s has no status.sandbox.name", errNotReady, claim.Metadata.Name)
	}
	return name, nil
}

func validateKubernetesObjectName(resource, name string) error {
	if name != strings.TrimSpace(name) || name == "" || len(name) > 253 {
		return resourceIdentityError{err: exit(4, "agent-sandbox %s name %q is invalid", resource, name)}
	}
	for _, label := range strings.Split(name, ".") {
		if !isKubernetesDNSLabel(label) {
			return resourceIdentityError{err: exit(4, "agent-sandbox %s name %q is invalid", resource, name)}
		}
	}
	return nil
}

func isKubernetesDNSLabel(value string) bool {
	return len(value) > 0 && len(value) <= 63 && kubernetesDNSLabelPattern.MatchString(value)
}

func sandboxReady(sandbox *kubernetesObject) error {
	for _, condition := range sandbox.Status.Conditions {
		if condition.Type == "Ready" &&
			condition.Status != "True" &&
			strings.EqualFold(strings.TrimSpace(condition.Reason), "SandboxExpired") {
			return sandboxExpiredError{err: exit(
				4,
				"agent-sandbox Sandbox %s expired reason=%s message=%s",
				sandbox.Metadata.Name,
				condition.Reason,
				blank(condition.Message, "none"),
			)}
		}
	}
	for _, condition := range sandbox.Status.Conditions {
		if condition.Type == "Finished" && condition.Status == "True" {
			return resourceTerminalError{err: exit(
				4,
				"agent-sandbox Sandbox %s finished reason=%s message=%s",
				sandbox.Metadata.Name,
				blank(condition.Reason, "unknown"),
				blank(condition.Message, "none"),
			)}
		}
	}
	for _, condition := range sandbox.Status.Conditions {
		if condition.Type != "Ready" {
			continue
		}
		if condition.Status == "True" {
			return nil
		}
		return fmt.Errorf("%w: Sandbox %s Ready=%s reason=%s message=%s", errNotReady, sandbox.Metadata.Name, condition.Status, condition.Reason, condition.Message)
	}
	return fmt.Errorf("%w: Sandbox %s has no Ready condition", errNotReady, sandbox.Metadata.Name)
}

func resolveSandboxPod(
	ctx context.Context,
	client kubernetesClient,
	namespace string,
	sandbox *kubernetesObject,
) (podState, error) {
	if podName := strings.TrimSpace(sandbox.Metadata.Annotations["agents.x-k8s.io/pod-name"]); podName != "" {
		return client.GetPod(ctx, namespace, podName)
	}
	if selector := strings.TrimSpace(sandbox.Status.Selector); selector != "" {
		pods, err := client.ListPods(ctx, namespace, selector)
		if err != nil {
			return podState{}, err
		}
		if len(pods) == 1 {
			return pods[0], nil
		}
		return podState{}, fmt.Errorf("%w: Sandbox %s selector %q matched %d pods", errNotReady, sandbox.Metadata.Name, selector, len(pods))
	}
	return podState{}, fmt.Errorf("%w: Sandbox %s has no pod annotation or selector", errNotReady, sandbox.Metadata.Name)
}

func waitForSandboxReadiness(ctx context.Context, client kubernetesClient, namespace, claimName string, identity claimIdentity, poll time.Duration) (sandboxReadiness, error) {
	resource, err := waitForSandboxResourceReadiness(ctx, client, namespace, claimName, identity, poll)
	if err != nil {
		return sandboxReadiness{}, err
	}
	pod, err := waitForSandboxPodReadiness(ctx, client, namespace, resource.ClaimName, resource.Sandbox, identity, poll)
	if err != nil {
		return sandboxReadiness{}, err
	}
	container, err := resolvePodContainer(pod, identity.Container)
	if err != nil {
		return sandboxReadiness{}, err
	}
	return newSandboxReadiness(resource, pod, identity, container), nil
}

func waitForSandboxReadinessWithTimeouts(ctx context.Context, client kubernetesClient, namespace, claimName string, identity claimIdentity, sandboxTimeout, podTimeout, poll time.Duration) (sandboxReadiness, error) {
	sandboxCtx := ctx
	sandboxCancel := func() {}
	if sandboxTimeout > 0 {
		sandboxCtx, sandboxCancel = context.WithTimeout(ctx, sandboxTimeout)
	}
	resource, err := waitForSandboxResourceReadiness(sandboxCtx, client, namespace, claimName, identity, poll)
	sandboxCancel()
	if err != nil {
		return sandboxReadiness{}, err
	}
	podCtx := ctx
	podCancel := func() {}
	if podTimeout > 0 {
		podCtx, podCancel = context.WithTimeout(ctx, podTimeout)
	}
	pod, err := waitForSandboxPodReadiness(podCtx, client, namespace, resource.ClaimName, resource.Sandbox, identity, poll)
	podCancel()
	if err != nil {
		return sandboxReadiness{}, err
	}
	container, err := resolvePodContainer(pod, identity.Container)
	if err != nil {
		return sandboxReadiness{}, err
	}
	return newSandboxReadiness(resource, pod, identity, container), nil
}

func waitForSandboxResourceReadiness(ctx context.Context, client kubernetesClient, namespace, claimName string, identity claimIdentity, poll time.Duration) (sandboxResourceReadiness, error) {
	if poll <= 0 {
		poll = time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	var lastErr error
	for {
		ready, err := sandboxResourceReadinessOnce(ctx, client, namespace, claimName, identity)
		if err == nil {
			return ready, nil
		}
		if errors.Is(err, errSandboxClaimNotFound) {
			return sandboxResourceReadiness{}, err
		}
		if isResourceIdentityError(err) || isResourceTerminalError(err) {
			return sandboxResourceReadiness{}, err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return sandboxResourceReadiness{}, fmt.Errorf("agent-sandbox readiness timed out for claim %s: %w", claimName, lastErr)
		case <-ticker.C:
		}
	}
}

func waitForSandboxPodReadiness(ctx context.Context, client kubernetesClient, namespace, claimName string, sandbox *kubernetesObject, identity claimIdentity, poll time.Duration) (podState, error) {
	if poll <= 0 {
		poll = time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	var lastErr error
	for {
		currentSandbox, err := client.Get(ctx, sandboxGVR(), namespace, sandbox.Metadata.Name)
		if err == nil {
			err = validateSandboxClaimBinding(currentSandbox, claimName, identity)
		}
		if err == nil && currentSandbox.Metadata.UID != sandbox.Metadata.UID {
			err = resourceIdentityError{err: exit(
				4,
				"agent-sandbox Sandbox identity changed from %s UID %s to %s UID %s",
				sandbox.Metadata.Name,
				sandbox.Metadata.UID,
				currentSandbox.Metadata.Name,
				currentSandbox.Metadata.UID,
			)}
		}
		if err == nil {
			err = sandboxReady(currentSandbox)
		}
		var pod podState
		if err == nil {
			pod, err = resolveSandboxPod(ctx, client, namespace, currentSandbox)
		}
		if err == nil {
			if err := validatePodSandboxBinding(pod, currentSandbox, identity); err != nil {
				return podState{}, err
			}
			err = podReady(pod)
			if err == nil {
				return pod, nil
			}
		}
		if isResourceIdentityError(err) || isResourceTerminalError(err) {
			return podState{}, err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return podState{}, fmt.Errorf("agent-sandbox pod readiness timed out for sandbox %s: %w", sandbox.Metadata.Name, lastErr)
		case <-ticker.C:
		}
	}
}

func sandboxResourceReadinessOnce(ctx context.Context, client kubernetesClient, namespace, claimName string, identity claimIdentity) (sandboxResourceReadiness, error) {
	claim, err := client.Get(ctx, sandboxClaimGVR(), namespace, claimName)
	if err != nil {
		if isNotFound(err) {
			return sandboxResourceReadiness{}, fmt.Errorf("SandboxClaim %s/%s: %w", namespace, claimName, errSandboxClaimNotFound)
		}
		return sandboxResourceReadiness{}, err
	}
	if err := validateClaimIdentity(claim, identity); err != nil {
		return sandboxResourceReadiness{}, err
	}
	if reason, expired := sandboxClaimControllerExpiry(claim); expired {
		return sandboxResourceReadiness{}, sandboxExpiredError{err: exit(
			4,
			"agent-sandbox SandboxClaim %s expired reason=%s",
			claim.Metadata.Name,
			reason,
		)}
	}
	sandboxName, err := claimSandboxName(claim)
	if err != nil {
		return sandboxResourceReadiness{}, err
	}
	sandbox, err := client.Get(ctx, sandboxGVR(), namespace, sandboxName)
	if err != nil {
		return sandboxResourceReadiness{}, err
	}
	if err := validateSandboxClaimBinding(sandbox, claimName, identity); err != nil {
		return sandboxResourceReadiness{}, err
	}
	if err := sandboxReady(sandbox); err != nil {
		return sandboxResourceReadiness{}, err
	}
	return sandboxResourceReadiness{ClaimName: claimName, SandboxName: sandboxName, Sandbox: sandbox}, nil
}

func sandboxReadinessOnce(ctx context.Context, client kubernetesClient, namespace, claimName string, identity claimIdentity) (sandboxReadiness, error) {
	resource, err := sandboxResourceReadinessOnce(ctx, client, namespace, claimName, identity)
	if err != nil {
		return sandboxReadiness{}, err
	}
	pod, err := resolveSandboxPod(ctx, client, namespace, resource.Sandbox)
	if err != nil {
		return sandboxReadiness{}, err
	}
	if err := validatePodSandboxBinding(pod, resource.Sandbox, identity); err != nil {
		return sandboxReadiness{}, err
	}
	if err := podReady(pod); err != nil {
		return sandboxReadiness{}, err
	}
	container, err := resolvePodContainer(pod, identity.Container)
	if err != nil {
		return sandboxReadiness{}, err
	}
	return newSandboxReadiness(resource, pod, identity, container), nil
}

func podReady(pod podState) error {
	switch strings.ToLower(strings.TrimSpace(pod.Phase)) {
	case "succeeded", "failed":
		return resourceTerminalError{err: exit(
			4,
			"agent-sandbox pod %s reached terminal phase=%s conditions=%s",
			pod.Name,
			pod.Phase,
			podConditionSummary(pod.Conditions),
		)}
	}
	if pod.Ready {
		return nil
	}
	return fmt.Errorf("%w: pod %s phase=%s conditions=%s", errNotReady, pod.Name, pod.Phase, podConditionSummary(pod.Conditions))
}

func resolvePodContainer(pod podState, pinned string) (string, error) {
	selected := strings.TrimSpace(pinned)
	if selected == "" {
		selected = strings.TrimSpace(pod.Annotations["kubectl.kubernetes.io/default-container"])
	}
	if selected == "" && len(pod.Containers) > 0 {
		selected = pod.Containers[0]
	}
	if selected == "" {
		return "", resourceIdentityError{err: exit(4, "agent-sandbox pod %s has no selectable container", pod.Name)}
	}
	for _, container := range pod.Containers {
		if container == selected {
			return selected, nil
		}
	}
	return "", resourceIdentityError{err: exit(4, "agent-sandbox pod %s does not contain pinned container %s", pod.Name, selected)}
}

func newSandboxReadiness(resource sandboxResourceReadiness, pod podState, identity claimIdentity, container string) sandboxReadiness {
	return sandboxReadiness{
		ClaimName:   resource.ClaimName,
		ClaimUID:    identity.UID,
		SandboxName: resource.SandboxName,
		SandboxUID:  resource.Sandbox.Metadata.UID,
		PodName:     pod.Name,
		PodUID:      pod.UID,
		PodIP:       pod.PodIP,
		Container:   container,
		identity:    identity,
	}
}

func revalidateSandboxReadiness(ctx context.Context, client kubernetesClient, namespace string, expected sandboxReadiness) error {
	current, err := sandboxReadinessOnce(ctx, client, namespace, expected.ClaimName, expected.identity)
	if err != nil {
		return err
	}
	if current.SandboxName != expected.SandboxName || current.SandboxUID != expected.SandboxUID {
		return resourceIdentityError{err: exit(
			4,
			"agent-sandbox Sandbox identity changed from %s UID %s to %s UID %s",
			expected.SandboxName,
			expected.SandboxUID,
			current.SandboxName,
			current.SandboxUID,
		)}
	}
	if current.PodName != expected.PodName || current.PodUID != expected.PodUID {
		return resourceIdentityError{err: exit(
			4,
			"agent-sandbox pod identity changed from %s UID %s to %s UID %s",
			expected.PodName,
			expected.PodUID,
			current.PodName,
			current.PodUID,
		)}
	}
	if current.Container != expected.Container {
		return resourceIdentityError{err: exit(
			4,
			"agent-sandbox pod container changed from %s to %s",
			expected.Container,
			current.Container,
		)}
	}
	return nil
}

func validateSandboxClaimBinding(sandbox *kubernetesObject, claimName string, identity claimIdentity) error {
	if sandbox == nil {
		return resourceIdentityError{err: exit(4, "agent-sandbox Sandbox identity is missing")}
	}
	if got := strings.TrimSpace(sandbox.Metadata.Labels[agentSandboxClaimUIDLabel]); got != identity.UID {
		return resourceIdentityError{err: exit(4, "agent-sandbox Sandbox %s claim UID label changed from %s to %s", sandbox.Metadata.Name, identity.UID, blank(got, "<empty>"))}
	}
	ref, ok := controllerOwnerReference(sandbox.Metadata.OwnerReferences)
	if !ok ||
		ref.APIVersion != agentSandboxExtensionsGroupVersion ||
		ref.Kind != "SandboxClaim" ||
		ref.Name != claimName ||
		ref.UID != identity.UID {
		return resourceIdentityError{err: exit(4, "agent-sandbox Sandbox %s is not controller-owned by SandboxClaim %s UID %s", sandbox.Metadata.Name, claimName, identity.UID)}
	}
	if strings.TrimSpace(sandbox.Metadata.UID) == "" {
		return resourceIdentityError{err: exit(4, "agent-sandbox Sandbox %s has no Kubernetes UID", sandbox.Metadata.Name)}
	}
	return nil
}

func validatePodSandboxBinding(pod podState, sandbox *kubernetesObject, identity claimIdentity) error {
	if sandbox == nil {
		return resourceIdentityError{err: exit(4, "agent-sandbox Sandbox identity is missing")}
	}
	if got := strings.TrimSpace(pod.Labels[agentSandboxClaimUIDLabel]); got != "" && got != identity.UID {
		return resourceIdentityError{err: exit(4, "agent-sandbox pod %s claim UID label changed from %s to %s", pod.Name, identity.UID, got)}
	}
	if strings.TrimSpace(pod.UID) == "" {
		return resourceIdentityError{err: exit(4, "agent-sandbox pod %s has no Kubernetes UID", pod.Name)}
	}
	ref, ok := controllerOwnerReference(pod.OwnerReferences)
	if !ok ||
		ref.APIVersion != agentSandboxCoreGroupVersion ||
		ref.Kind != "Sandbox" ||
		ref.Name != sandbox.Metadata.Name ||
		ref.UID != sandbox.Metadata.UID {
		return resourceIdentityError{err: exit(4, "agent-sandbox pod %s is not controller-owned by Sandbox %s UID %s", pod.Name, sandbox.Metadata.Name, sandbox.Metadata.UID)}
	}
	return nil
}

func isResourceIdentityError(err error) bool {
	var target resourceIdentityError
	return errors.As(err, &target)
}

func isResourceTerminalError(err error) bool {
	if isSandboxExpiredError(err) {
		return true
	}
	var target resourceTerminalError
	return errors.As(err, &target)
}

func isSandboxExpiredError(err error) bool {
	var target sandboxExpiredError
	return errors.As(err, &target)
}

func controllerOwnerReference(refs []ownerReference) (ownerReference, bool) {
	for _, ref := range refs {
		if ref.Controller {
			return ref, true
		}
	}
	return ownerReference{}, false
}

func podConditionSummary(conditions []conditionState) string {
	if len(conditions) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(conditions))
	for _, condition := range conditions {
		part := condition.Type + "=" + condition.Status
		if condition.Reason != "" {
			part += "/" + condition.Reason
		}
		if condition.Message != "" {
			part += ":" + condition.Message
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ",")
}

func effectiveKubeconfigIdentity(cfg AgentSandboxConfig) string {
	if path := strings.TrimSpace(cfg.Kubeconfig); path != "" {
		return expandHomePath(path)
	}
	if path := os.Getenv("KUBECONFIG"); path != "" {
		return path
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".kube", "config")
	}
	return "default"
}
