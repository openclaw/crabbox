package agentsandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fakeKubernetesClient struct {
	resources map[string]map[string]bool
	objects   map[string]*unstructured.Unstructured
	rbac      map[string]bool
	pods      map[string][]podState
	gets      []string
	execs     []podExecRequest
	execInput [][]byte
	execErrs  []error
	creates   int
	deletes   int
}

func (f *fakeKubernetesClient) CheckResource(_ context.Context, groupVersion, resource string) error {
	if f.resources[groupVersion][resource] {
		return nil
	}
	return errors.New("missing resource " + groupVersion + "/" + resource)
}

func (f *fakeKubernetesClient) Get(_ context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	key := gvr.Resource + "/" + namespace + "/" + name
	f.gets = append(f.gets, key)
	obj := f.objects[key]
	if obj == nil {
		return nil, errKubernetesNotFound
	}
	return obj, nil
}

func (f *fakeKubernetesClient) Create(_ context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	key := gvr.Resource + "/" + namespace + "/" + obj.GetName()
	if f.objects == nil {
		f.objects = map[string]*unstructured.Unstructured{}
	}
	if f.objects[key] != nil {
		return nil, errors.New("already exists " + key)
	}
	f.creates++
	created := obj.DeepCopy()
	f.objects[key] = created
	if gvr.Resource == sandboxClaimResource {
		sandboxName := obj.GetName() + "-sandbox"
		podName := obj.GetName() + "-pod"
		_ = unstructured.SetNestedField(created.Object, sandboxName, "status", "sandbox", "name")
		sandbox := &unstructured.Unstructured{Object: map[string]any{
			"status": map[string]any{
				"selector": "claim=" + obj.GetName(),
				"conditions": []any{
					map[string]any{"type": "Ready", "status": "True"},
				},
			},
		}}
		sandbox.SetName(sandboxName)
		f.objects[sandboxResource+"/"+namespace+"/"+sandboxName] = sandbox
		if f.pods == nil {
			f.pods = map[string][]podState{}
		}
		f.pods[namespace+"/claim="+obj.GetName()] = []podState{{Name: podName, Phase: "Running", PodIP: "10.0.0.11", Ready: true}}
	}
	return obj.DeepCopy(), nil
}

func (f *fakeKubernetesClient) Delete(_ context.Context, gvr schema.GroupVersionResource, namespace, name string) error {
	key := gvr.Resource + "/" + namespace + "/" + name
	if f.objects[key] == nil {
		return errKubernetesNotFound
	}
	f.deletes++
	delete(f.objects, key)
	return nil
}

func (f *fakeKubernetesClient) List(_ context.Context, gvr schema.GroupVersionResource, namespace string, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	prefix := gvr.Resource + "/" + namespace + "/"
	list := &unstructured.UnstructuredList{}
	for key, obj := range f.objects {
		if strings.HasPrefix(key, prefix) {
			list.Items = append(list.Items, *obj.DeepCopy())
		}
	}
	return list, nil
}

func (f *fakeKubernetesClient) CanI(_ context.Context, rule rbacRule) (bool, error) {
	allowed, ok := f.rbac[rule.String()]
	return ok && allowed, nil
}

func (f *fakeKubernetesClient) GetPod(_ context.Context, namespace, name string) (podState, error) {
	pods := f.pods[namespace+"/name="+name]
	if len(pods) != 1 {
		return podState{}, errors.New("pod not found " + namespace + "/" + name)
	}
	return pods[0], nil
}

func (f *fakeKubernetesClient) ListPods(_ context.Context, namespace, selector string) ([]podState, error) {
	return f.pods[namespace+"/"+selector], nil
}

func (f *fakeKubernetesClient) Exec(_ context.Context, req podExecRequest) error {
	f.execs = append(f.execs, req)
	if req.Stdin != nil {
		data, _ := io.ReadAll(req.Stdin)
		f.execInput = append(f.execInput, data)
	}
	if len(f.execErrs) > 0 {
		err := f.execErrs[0]
		f.execErrs = f.execErrs[1:]
		return err
	}
	return nil
}

func TestDoctorChecksAreNonMutating(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	fake := readyFakeClient(cfg)
	backend := &backend{spec: Provider{}.Spec(), cfg: cfg, newClient: func(context.Context, Config, Runtime) (kubernetesClient, error) {
		return fake, nil
	}}
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != providerName || result.Status != "ready" || !strings.Contains(result.Message, "mutation=false") {
		t.Fatalf("doctor result=%#v", result)
	}
	if fake.creates != 0 || fake.deletes != 0 {
		t.Fatalf("doctor mutated claims: creates=%d deletes=%d", fake.creates, fake.deletes)
	}
	if len(result.Checks) == 0 {
		t.Fatal("doctor checks were empty")
	}
}

func TestDoctorReportsMissingCRDAndWarmPool(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	fake := readyFakeClient(cfg)
	fake.resources[agentSandboxExtensionsGroupVersion][warmPoolResource] = false
	backend := &backend{spec: Provider{}.Spec(), cfg: cfg, newClient: func(context.Context, Config, Runtime) (kubernetesClient, error) {
		return fake, nil
	}}
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err == nil {
		t.Fatal("missing CRD was accepted")
	}
	if result.Status != "blocked" || !strings.Contains(result.Message, warmPoolResource) {
		t.Fatalf("result=%#v err=%v", result, err)
	}

	fake = readyFakeClient(cfg)
	delete(fake.objects, warmPoolResource+"/sandboxes/linux-pool")
	backend.newClient = func(context.Context, Config, Runtime) (kubernetesClient, error) { return fake, nil }
	result, err = backend.Doctor(context.Background(), DoctorRequest{})
	if err == nil {
		t.Fatal("missing warm pool was accepted")
	}
	if result.Status != "blocked" || !strings.Contains(result.Message, "not found") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestClaimScopeIncludesClusterContextAndRuntimeFields(t *testing.T) {
	t.Setenv("KUBECONFIG", "/cluster-a")
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	cfg.AgentSandbox.Container = "worker"
	scopeA := claimScope(cfg)
	for _, want := range []string{"kubeconfig:/cluster-a", "context:agent-context", "namespace:sandboxes", "warmPool:linux-pool", "container:worker"} {
		if !strings.Contains(scopeA, want) {
			t.Fatalf("scope %q missing %q", scopeA, want)
		}
	}
	cfg.AgentSandbox.Kubeconfig = "/cluster-b"
	scopeB := claimScope(cfg)
	if scopeA == scopeB || !strings.Contains(scopeB, "kubeconfig:/cluster-b") {
		t.Fatalf("scopeA=%q scopeB=%q", scopeA, scopeB)
	}
}

func TestAuthorizeClaimScopeFailsClosed(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	claim := LeaseClaim{LeaseID: "asbx_test", Provider: providerName, ProviderScope: "kubeconfig:/other|context:agent-context|namespace:sandboxes|warmPool:linux-pool|container:default"}
	if err := authorizeClaimScope(cfg, claim); err == nil {
		t.Fatal("wrong scope was accepted")
	}
}

func TestClaimNameIsKubernetesSafeAndBounded(t *testing.T) {
	name := claimName("asbx_123", "Feature/Branch_With Very Long Name That Needs To Be Bounded For Kubernetes Labels")
	if len(name) > 63 {
		t.Fatalf("name length=%d name=%q", len(name), name)
	}
	if strings.ContainsAny(name, "_/ ") || name != strings.ToLower(name) {
		t.Fatalf("unsafe name=%q", name)
	}
}

func TestClaimNameIncludesLeaseDerivedUniqueness(t *testing.T) {
	a := claimName("asbx_111111111111", "same-slug")
	b := claimName("asbx_222222222222", "same-slug")
	if a == b {
		t.Fatalf("claim names collided: %q", a)
	}
	if !strings.HasPrefix(a, "crabbox-same-slug-") || !strings.HasPrefix(b, "crabbox-same-slug-") {
		t.Fatalf("claim names lost slug context: %q %q", a, b)
	}
}

func TestSandboxReadinessResolvesClaimSandboxAndPod(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	fake := readyFakeClient(cfg)
	ready, err := sandboxReadinessOnce(context.Background(), fake, "sandboxes", "claim-a")
	if err != nil {
		t.Fatal(err)
	}
	if ready.SandboxName != "sandbox-a" || ready.PodName != "pod-a" || ready.PodIP != "10.0.0.10" {
		t.Fatalf("ready=%#v", ready)
	}
}

func TestSandboxReadinessPreservesDiagnostics(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	fake := readyFakeClient(cfg)
	fake.pods["sandboxes/app=agent-sandbox"] = []podState{{Name: "pod-a", Phase: "Running", Ready: false, Conditions: []conditionState{{Type: "Ready", Status: "False", Reason: "ContainersNotReady"}}}}
	_, err := sandboxReadinessOnce(context.Background(), fake, "sandboxes", "claim-a")
	if err == nil || !strings.Contains(err.Error(), "ContainersNotReady") {
		t.Fatalf("err=%v", err)
	}
}

func TestRetainMissingClaimRequiresExplicitForget(t *testing.T) {
	cfg := core.BaseConfig()
	claim := LeaseClaim{LeaseID: "asbx_missing"}
	if err := retainMissingClaim(cfg, claim); err == nil {
		t.Fatal("missing claim was forgotten without explicit setting")
	}
	cfg.AgentSandbox.ForgetMissing = true
	temp := t.TempDir()
	t.Setenv("CRABBOX_STATE_DIR", temp)
	if err := retainMissingClaim(cfg, claim); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForSandboxReadinessTimesOut(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	fake := readyFakeClient(cfg)
	delete(fake.objects, sandboxClaimResource+"/sandboxes/claim-a")
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err := waitForSandboxReadiness(ctx, fake, "sandboxes", "claim-a", time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "claim-a") {
		t.Fatalf("err=%v", err)
	}
}

func readyFakeClient(cfg Config) *fakeKubernetesClient {
	claim := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"sandbox": map[string]any{"name": "sandbox-a"}},
	}}
	claim.SetName("claim-a")
	sandbox := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"selector": "app=agent-sandbox",
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "True"},
			},
		},
	}}
	sandbox.SetName("sandbox-a")
	warmPool := &unstructured.Unstructured{}
	warmPool.SetName(cfg.AgentSandbox.WarmPool)
	fake := &fakeKubernetesClient{
		resources: map[string]map[string]bool{
			agentSandboxCoreGroupVersion:       {sandboxResource: true},
			agentSandboxExtensionsGroupVersion: {sandboxClaimResource: true, warmPoolResource: true},
		},
		objects: map[string]*unstructured.Unstructured{
			sandboxClaimResource + "/sandboxes/claim-a":                  claim,
			sandboxResource + "/sandboxes/sandbox-a":                     sandbox,
			warmPoolResource + "/sandboxes/" + cfg.AgentSandbox.WarmPool: warmPool,
		},
		rbac: map[string]bool{},
		pods: map[string][]podState{
			"sandboxes/app=agent-sandbox": {{Name: "pod-a", Phase: "Running", PodIP: "10.0.0.10", Ready: true}},
		},
	}
	for _, rule := range doctorRBACRules(cfg.AgentSandbox.Namespace) {
		fake.rbac[rule.String()] = true
	}
	return fake
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
