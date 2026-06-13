package agentsandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

var (
	errNotReady           = errors.New("not ready")
	errKubernetesNotFound = errors.New("kubernetes resource not found")
)

type kubernetesClient interface {
	CheckResource(ctx context.Context, groupVersion, resource string) error
	Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error)
	Create(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
	Delete(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) error
	List(ctx context.Context, gvr schema.GroupVersionResource, namespace string, opts metav1.ListOptions) (*unstructured.UnstructuredList, error)
	CanI(ctx context.Context, rule rbacRule) (bool, error)
	GetPod(ctx context.Context, namespace, name string) (podState, error)
	ListPods(ctx context.Context, namespace, selector string) ([]podState, error)
	Exec(ctx context.Context, req podExecRequest) error
}

type dynamicKubernetesClient struct {
	discovery discovery.DiscoveryInterface
	dynamic   dynamic.Interface
	core      kubernetes.Interface
	rest      *rest.Config
}

func newKubernetesClient(_ context.Context, cfg Config, _ Runtime) (kubernetesClient, error) {
	restConfig, err := loadRESTConfig(cfg.AgentSandbox)
	if err != nil {
		return nil, err
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create discovery client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}
	coreClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create core client: %w", err)
	}
	return &dynamicKubernetesClient{discovery: discoveryClient, dynamic: dynamicClient, core: coreClient, rest: restConfig}, nil
}

func loadRESTConfig(cfg AgentSandboxConfig) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if strings.TrimSpace(cfg.Kubeconfig) != "" {
		loadingRules.ExplicitPath = cfg.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: strings.TrimSpace(cfg.Context)}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err == nil {
		return restConfig, nil
	}
	if strings.TrimSpace(cfg.Kubeconfig) != "" || strings.TrimSpace(os.Getenv("KUBECONFIG")) != "" {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	inCluster, inClusterErr := rest.InClusterConfig()
	if inClusterErr != nil {
		return nil, fmt.Errorf("load kubeconfig: %w; in-cluster config unavailable: %w", err, inClusterErr)
	}
	return inCluster, nil
}

func (c *dynamicKubernetesClient) CheckResource(ctx context.Context, groupVersion, resource string) error {
	_ = ctx
	resources, err := c.discovery.ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		return fmt.Errorf("discover %s %s: %w", groupVersion, resource, err)
	}
	for _, apiResource := range resources.APIResources {
		if apiResource.Name == resource {
			return nil
		}
	}
	return fmt.Errorf("discover %s %s: resource not found", groupVersion, resource)
}

func (c *dynamicKubernetesClient) Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	obj, err := c.dynamic.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s/%s %s/%s: %w", gvr.Group, gvr.Resource, namespace, name, err)
	}
	return obj, nil
}

func (c *dynamicKubernetesClient) Create(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	created, err := c.dynamic.Resource(gvr).Namespace(namespace).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create %s/%s %s/%s: %w", gvr.Group, gvr.Resource, namespace, obj.GetName(), err)
	}
	return created, nil
}

func (c *dynamicKubernetesClient) Delete(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) error {
	if err := c.dynamic.Resource(gvr).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("delete %s/%s %s/%s: %w", gvr.Group, gvr.Resource, namespace, name, err)
	}
	return nil
}

func (c *dynamicKubernetesClient) List(ctx context.Context, gvr schema.GroupVersionResource, namespace string, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	list, err := c.dynamic.Resource(gvr).Namespace(namespace).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("list %s/%s namespace=%s: %w", gvr.Group, gvr.Resource, namespace, err)
	}
	return list, nil
}

func (c *dynamicKubernetesClient) CanI(ctx context.Context, rule rbacRule) (bool, error) {
	for _, verb := range rule.Verbs {
		review := &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace:   rule.Namespace,
					Verb:        verb,
					Group:       rule.Group,
					Resource:    rule.Resource,
					Subresource: rule.Subresource,
				},
			},
		}
		result, err := c.core.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err != nil {
			return false, fmt.Errorf("self subject access review %s: %w", rule.String(), err)
		}
		if !result.Status.Allowed {
			return false, nil
		}
	}
	return true, nil
}

func (c *dynamicKubernetesClient) GetPod(ctx context.Context, namespace, name string) (podState, error) {
	pod, err := c.core.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return podState{}, fmt.Errorf("get pod %s/%s: %w", namespace, name, err)
	}
	return podStateFromPod(pod), nil
}

func (c *dynamicKubernetesClient) ListPods(ctx context.Context, namespace, selector string) ([]podState, error) {
	list, err := c.core.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list pods namespace=%s selector=%q: %w", namespace, selector, err)
	}
	pods := make([]podState, 0, len(list.Items))
	for _, pod := range list.Items {
		pods = append(pods, podStateFromPod(&pod))
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

func (c *dynamicKubernetesClient) Exec(ctx context.Context, req podExecRequest) error {
	execReq := c.core.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(req.Namespace).
		Name(req.Pod).
		SubResource("exec")
	execReq.VersionedParams(&corev1.PodExecOptions{
		Container: req.Container,
		Command:   req.Command,
		Stdin:     req.Stdin != nil,
		Stdout:    req.Stdout != nil,
		Stderr:    req.Stderr != nil,
		TTY:       false,
	}, runtime.NewParameterCodec(scheme.Scheme))
	executor, err := remotecommand.NewSPDYExecutor(c.rest, "POST", execReq.URL())
	if err != nil {
		return fmt.Errorf("create pod exec executor: %w", err)
	}
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  req.Stdin,
		Stdout: req.Stdout,
		Stderr: req.Stderr,
		Tty:    false,
	}); err != nil {
		return fmt.Errorf("exec pod %s/%s: %w", req.Namespace, req.Pod, err)
	}
	return nil
}

func sandboxGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: sandboxResource}
}

func sandboxClaimGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: sandboxClaimResource}
}

func warmPoolGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: warmPoolResource}
}

type sandboxReadiness struct {
	ClaimName   string
	SandboxName string
	PodName     string
	PodIP       string
}

type sandboxResourceReadiness struct {
	ClaimName   string
	SandboxName string
	Sandbox     *unstructured.Unstructured
}

type podState struct {
	Name       string
	Phase      string
	PodIP      string
	Ready      bool
	Conditions []conditionState
}

type conditionState struct {
	Type    string
	Status  string
	Reason  string
	Message string
}

func claimSandboxName(claim *unstructured.Unstructured) (string, error) {
	name, ok, err := unstructured.NestedString(claim.Object, "status", "sandbox", "name")
	if err != nil {
		return "", err
	}
	if !ok || strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("%w: SandboxClaim %s has no status.sandbox.name", errNotReady, claim.GetName())
	}
	return name, nil
}

func sandboxReady(sandbox *unstructured.Unstructured) error {
	conditions, ok, err := unstructured.NestedSlice(sandbox.Object, "status", "conditions")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: Sandbox %s has no Ready condition", errNotReady, sandbox.GetName())
	}
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok || condition["type"] != "Ready" {
			continue
		}
		if condition["status"] == "True" {
			return nil
		}
		return fmt.Errorf("%w: Sandbox %s Ready=%v reason=%v message=%v", errNotReady, sandbox.GetName(), condition["status"], condition["reason"], condition["message"])
	}
	return fmt.Errorf("%w: Sandbox %s has no Ready condition", errNotReady, sandbox.GetName())
}

func resolveSandboxPod(ctx context.Context, client kubernetesClient, namespace string, sandbox *unstructured.Unstructured) (podState, error) {
	if podName := strings.TrimSpace(sandbox.GetAnnotations()["agents.x-k8s.io/pod-name"]); podName != "" {
		return client.GetPod(ctx, namespace, podName)
	}
	if selector, ok, _ := unstructured.NestedString(sandbox.Object, "status", "selector"); ok && strings.TrimSpace(selector) != "" {
		pods, err := client.ListPods(ctx, namespace, selector)
		if err != nil {
			return podState{}, err
		}
		if len(pods) == 1 {
			return pods[0], nil
		}
		return podState{}, fmt.Errorf("%w: Sandbox %s selector %q matched %d pods", errNotReady, sandbox.GetName(), selector, len(pods))
	}
	return podState{}, fmt.Errorf("%w: Sandbox %s has no pod annotation or selector", errNotReady, sandbox.GetName())
}

func waitForSandboxReadiness(ctx context.Context, client kubernetesClient, namespace, claimName string, poll time.Duration) (sandboxReadiness, error) {
	resource, err := waitForSandboxResourceReadiness(ctx, client, namespace, claimName, poll)
	if err != nil {
		return sandboxReadiness{}, err
	}
	pod, err := waitForSandboxPodReadiness(ctx, client, namespace, resource.Sandbox, poll)
	if err != nil {
		return sandboxReadiness{}, err
	}
	return sandboxReadiness{ClaimName: resource.ClaimName, SandboxName: resource.SandboxName, PodName: pod.Name, PodIP: pod.PodIP}, nil
}

func waitForSandboxReadinessWithTimeouts(ctx context.Context, client kubernetesClient, namespace, claimName string, sandboxTimeout, podTimeout, poll time.Duration) (sandboxReadiness, error) {
	sandboxCtx := ctx
	sandboxCancel := func() {}
	if sandboxTimeout > 0 {
		sandboxCtx, sandboxCancel = context.WithTimeout(ctx, sandboxTimeout)
	}
	resource, err := waitForSandboxResourceReadiness(sandboxCtx, client, namespace, claimName, poll)
	sandboxCancel()
	if err != nil {
		return sandboxReadiness{}, err
	}
	podCtx := ctx
	podCancel := func() {}
	if podTimeout > 0 {
		podCtx, podCancel = context.WithTimeout(ctx, podTimeout)
	}
	pod, err := waitForSandboxPodReadiness(podCtx, client, namespace, resource.Sandbox, poll)
	podCancel()
	if err != nil {
		return sandboxReadiness{}, err
	}
	return sandboxReadiness{ClaimName: resource.ClaimName, SandboxName: resource.SandboxName, PodName: pod.Name, PodIP: pod.PodIP}, nil
}

func waitForSandboxResourceReadiness(ctx context.Context, client kubernetesClient, namespace, claimName string, poll time.Duration) (sandboxResourceReadiness, error) {
	if poll <= 0 {
		poll = time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	var lastErr error
	for {
		ready, err := sandboxResourceReadinessOnce(ctx, client, namespace, claimName)
		if err == nil {
			return ready, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return sandboxResourceReadiness{}, fmt.Errorf("agent-sandbox readiness timed out for claim %s: %w", claimName, lastErr)
		case <-ticker.C:
		}
	}
}

func waitForSandboxPodReadiness(ctx context.Context, client kubernetesClient, namespace string, sandbox *unstructured.Unstructured, poll time.Duration) (podState, error) {
	if poll <= 0 {
		poll = time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	var lastErr error
	for {
		pod, err := resolveSandboxPod(ctx, client, namespace, sandbox)
		if err == nil && pod.Ready {
			return pod, nil
		}
		if err == nil {
			err = fmt.Errorf("%w: pod %s phase=%s conditions=%s", errNotReady, pod.Name, pod.Phase, podConditionSummary(pod.Conditions))
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return podState{}, fmt.Errorf("agent-sandbox pod readiness timed out for sandbox %s: %w", sandbox.GetName(), lastErr)
		case <-ticker.C:
		}
	}
}

func sandboxResourceReadinessOnce(ctx context.Context, client kubernetesClient, namespace, claimName string) (sandboxResourceReadiness, error) {
	claim, err := client.Get(ctx, sandboxClaimGVR(), namespace, claimName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return sandboxResourceReadiness{}, fmt.Errorf("%w: SandboxClaim %s/%s not found", errNotReady, namespace, claimName)
		}
		return sandboxResourceReadiness{}, err
	}
	sandboxName, err := claimSandboxName(claim)
	if err != nil {
		return sandboxResourceReadiness{}, err
	}
	sandbox, err := client.Get(ctx, sandboxGVR(), namespace, sandboxName)
	if err != nil {
		return sandboxResourceReadiness{}, err
	}
	if err := sandboxReady(sandbox); err != nil {
		return sandboxResourceReadiness{}, err
	}
	return sandboxResourceReadiness{ClaimName: claimName, SandboxName: sandboxName, Sandbox: sandbox}, nil
}

func sandboxReadinessOnce(ctx context.Context, client kubernetesClient, namespace, claimName string) (sandboxReadiness, error) {
	resource, err := sandboxResourceReadinessOnce(ctx, client, namespace, claimName)
	if err != nil {
		return sandboxReadiness{}, err
	}
	pod, err := resolveSandboxPod(ctx, client, namespace, resource.Sandbox)
	if err != nil {
		return sandboxReadiness{}, err
	}
	if !pod.Ready {
		return sandboxReadiness{}, fmt.Errorf("%w: pod %s phase=%s conditions=%s", errNotReady, pod.Name, pod.Phase, podConditionSummary(pod.Conditions))
	}
	return sandboxReadiness{ClaimName: resource.ClaimName, SandboxName: resource.SandboxName, PodName: pod.Name, PodIP: pod.PodIP}, nil
}

func podStateFromPod(pod *corev1.Pod) podState {
	state := podState{Name: pod.Name, Phase: string(pod.Status.Phase), PodIP: pod.Status.PodIP}
	for _, condition := range pod.Status.Conditions {
		state.Conditions = append(state.Conditions, conditionState{
			Type:    string(condition.Type),
			Status:  string(condition.Status),
			Reason:  condition.Reason,
			Message: condition.Message,
		})
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			state.Ready = true
		}
	}
	return state
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
		return expandUserPath(path)
	}
	if path := strings.TrimSpace(os.Getenv("KUBECONFIG")); path != "" {
		return path
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".kube", "config")
	}
	return "in-cluster-or-default"
}
