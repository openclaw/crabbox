package agentsandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeKubernetesClient struct {
	resources      map[string]map[string]bool
	objects        map[string]*kubernetesObject
	rbac           map[string]bool
	pods           map[string][]podState
	gets           []string
	execs          []podExecRequest
	execInput      [][]byte
	execErrs       []error
	execStarted    chan struct{}
	execRelease    chan struct{}
	execDelays     []time.Duration
	getStarted     chan struct{}
	getRelease     chan struct{}
	getErrs        []error
	podListErrs    []error
	deleteErrs     []error
	createErrs     []error
	emptyCreateUID bool
	createPending  bool
	createStarted  chan struct{}
	createRelease  chan struct{}
	creates        int
	deletes        int
}

type recordingCommandRunner struct {
	requests []LocalCommandRequest
	inputs   [][]byte
	results  []LocalCommandResult
	errors   []error
}

func (r *recordingCommandRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.requests = append(r.requests, req)
	if req.Stdin != nil {
		input, _ := io.ReadAll(req.Stdin)
		r.inputs = append(r.inputs, input)
	}
	var result LocalCommandResult
	if len(r.results) > 0 {
		result = r.results[0]
		r.results = r.results[1:]
	}
	if req.Stdout != nil {
		_, _ = io.WriteString(req.Stdout, result.Stdout)
	}
	if req.Stderr != nil {
		_, _ = io.WriteString(req.Stderr, result.Stderr)
	}
	var err error
	if len(r.errors) > 0 {
		err = r.errors[0]
		r.errors = r.errors[1:]
	}
	return result, err
}

func (f *fakeKubernetesClient) CheckResource(_ context.Context, groupVersion, resource string) error {
	if f.resources[groupVersion][resource] {
		return nil
	}
	return errors.New("missing resource " + groupVersion + "/" + resource)
}

func (f *fakeKubernetesClient) Get(ctx context.Context, ref resourceRef, namespace, name string) (*kubernetesObject, error) {
	if f.getStarted != nil {
		select {
		case f.getStarted <- struct{}{}:
		default:
		}
		if f.getRelease != nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-f.getRelease:
			}
		}
	}
	if len(f.getErrs) > 0 {
		err := f.getErrs[0]
		f.getErrs = f.getErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	key := ref.Resource + "/" + namespace + "/" + name
	f.gets = append(f.gets, key)
	obj := f.objects[key]
	if obj == nil {
		return nil, errKubernetesNotFound
	}
	return cloneKubernetesObject(obj), nil
}

func (f *fakeKubernetesClient) Create(_ context.Context, ref resourceRef, namespace string, obj *kubernetesObject) (*kubernetesObject, error) {
	if f.createStarted != nil {
		f.createStarted <- struct{}{}
		if f.createRelease != nil {
			<-f.createRelease
		}
	}
	key := ref.Resource + "/" + namespace + "/" + obj.Metadata.Name
	if f.objects == nil {
		f.objects = map[string]*kubernetesObject{}
	}
	if f.objects[key] != nil {
		return nil, errors.New("already exists " + key)
	}
	f.creates++
	created := cloneKubernetesObject(obj)
	if created.Metadata.UID == "" {
		created.Metadata.UID = "uid-" + created.Metadata.Name
	}
	f.objects[key] = created
	if ref.Resource == sandboxClaimResource && !f.createPending {
		sandboxName := obj.Metadata.Name + "-sandbox"
		sandboxUID := "uid-" + sandboxName
		podName := obj.Metadata.Name + "-pod"
		created.Status.Sandbox.Name = sandboxName
		sandbox := &kubernetesObject{
			Metadata: objectMeta{
				Name:   sandboxName,
				UID:    sandboxUID,
				Labels: map[string]string{agentSandboxClaimUIDLabel: created.Metadata.UID},
				OwnerReferences: []ownerReference{{
					APIVersion: agentSandboxExtensionsGroupVersion,
					Kind:       "SandboxClaim",
					Name:       created.Metadata.Name,
					UID:        created.Metadata.UID,
					Controller: true,
				}},
			},
			Status: objectStatus{
				Selector:   "claim=" + obj.Metadata.Name,
				Conditions: []conditionState{{Type: "Ready", Status: "True"}},
			},
		}
		f.objects[sandboxResource+"/"+namespace+"/"+sandboxName] = sandbox
		if f.pods == nil {
			f.pods = map[string][]podState{}
		}
		f.pods[namespace+"/claim="+obj.Metadata.Name] = []podState{{
			Name:       podName,
			UID:        "uid-" + podName,
			Labels:     map[string]string{agentSandboxClaimUIDLabel: created.Metadata.UID},
			Containers: []string{testPodContainer(obj.Metadata.Annotations[annotationContainer])},
			OwnerReferences: []ownerReference{{
				APIVersion: agentSandboxCoreGroupVersion,
				Kind:       "Sandbox",
				Name:       sandboxName,
				UID:        sandboxUID,
				Controller: true,
			}},
			Phase: "Running",
			PodIP: "10.0.0.11",
			Ready: true,
		}}
	}
	if len(f.createErrs) > 0 {
		err := f.createErrs[0]
		f.createErrs = f.createErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	response := cloneKubernetesObject(created)
	if f.emptyCreateUID {
		response.Metadata.UID = ""
	}
	return response, nil
}

func (f *fakeKubernetesClient) Delete(_ context.Context, ref resourceRef, namespace, name, uid string) error {
	if len(f.deleteErrs) > 0 {
		err := f.deleteErrs[0]
		f.deleteErrs = f.deleteErrs[1:]
		return err
	}
	key := ref.Resource + "/" + namespace + "/" + name
	if f.objects[key] == nil {
		return errKubernetesNotFound
	}
	if f.objects[key].Metadata.UID != uid {
		return errors.New("Kubernetes UID precondition failed")
	}
	f.deletes++
	delete(f.objects, key)
	return nil
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

func cloneKubernetesObject(object *kubernetesObject) *kubernetesObject {
	data, _ := json.Marshal(object)
	var clone kubernetesObject
	_ = json.Unmarshal(data, &clone)
	return &clone
}

func (f *fakeKubernetesClient) ListPods(_ context.Context, namespace, selector string) ([]podState, error) {
	if len(f.podListErrs) > 0 {
		err := f.podListErrs[0]
		f.podListErrs = f.podListErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	return f.pods[namespace+"/"+selector], nil
}

func (f *fakeKubernetesClient) Exec(_ context.Context, req podExecRequest) error {
	f.execs = append(f.execs, req)
	if req.Stdin != nil {
		data, _ := io.ReadAll(req.Stdin)
		f.execInput = append(f.execInput, data)
	}
	if len(f.execDelays) > 0 {
		delay := f.execDelays[0]
		f.execDelays = f.execDelays[1:]
		time.Sleep(delay)
	}
	if len(f.execErrs) > 0 {
		err := f.execErrs[0]
		f.execErrs = f.execErrs[1:]
		return err
	}
	if len(req.Command) >= 2 && req.Command[0] == "sh" && req.Command[1] == "-s" && f.execStarted != nil {
		select {
		case f.execStarted <- struct{}{}:
		default:
		}
		if f.execRelease != nil {
			<-f.execRelease
		}
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
	for _, want := range []string{"kubeconfig:/cluster-a", "context:agent-context", "namespace:sandboxes", "warmPool:linux-pool", "containerMode:explicit", "container:worker"} {
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

func TestClaimAnnotationsStoreScopeFingerprint(t *testing.T) {
	t.Setenv("KUBECONFIG", "/Users/alice/.kube/private-cluster")
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"

	annotations := claimAnnotations(cfg)
	if got, want := annotations[annotationScope], scopeFingerprint(claimScope(cfg)); got != want {
		t.Fatalf("scope annotation=%q want=%q", got, want)
	}
	if strings.Contains(annotations[annotationScope], "/Users/alice") {
		t.Fatalf("scope annotation leaked local kubeconfig path: %q", annotations[annotationScope])
	}
}

func TestClaimScopeDistinguishesImplicitAndExplicitDefaultContainer(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	implicit := claimScope(cfg)
	cfg.AgentSandbox.Container = "default"
	explicitDefault := claimScope(cfg)
	if implicit == explicitDefault {
		t.Fatalf("implicit and explicit default container scopes collapsed: %q", implicit)
	}
	if !strings.Contains(implicit, "containerMode:implicit|container:") {
		t.Fatalf("implicit scope=%q", implicit)
	}
	if !strings.Contains(explicitDefault, "containerMode:explicit|container:default") {
		t.Fatalf("explicit scope=%q", explicitDefault)
	}
}

func TestClaimIdentityMigratesLegacyImplicitContainerSentinel(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	claim := LeaseClaim{
		LeaseID:       "asbx_legacy",
		ProviderScope: claimScope(cfg),
		Labels: map[string]string{
			claimLabelClaimUID:  "uid-legacy",
			claimLabelWarmPool:  "linux-pool",
			claimLabelContainer: "default",
		},
	}
	identity, err := claimIdentityFromLocalClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Container != "" {
		t.Fatalf("legacy implicit container=%q", identity.Container)
	}
	claim.Labels[claimLabelContainerPinned] = "true"
	identity, err = claimIdentityFromLocalClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Container != "default" {
		t.Fatalf("pinned container=%q", identity.Container)
	}
	cfg.AgentSandbox.Container = "pending"
	claim.ProviderScope = claimScope(cfg)
	delete(claim.Labels, claimLabelContainerPinned)
	claim.Labels[claimLabelContainer] = "pending"
	identity, err = claimIdentityFromLocalClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Container != "pending" {
		t.Fatalf("explicit pending container=%q", identity.Container)
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
	ready, err := sandboxReadinessOnce(context.Background(), fake, "sandboxes", "claim-a", fakeClaimIdentity(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if ready.SandboxName != "sandbox-a" || ready.PodName != "pod-a" || ready.PodIP != "10.0.0.10" || ready.Container != "default" {
		t.Fatalf("ready=%#v", ready)
	}
}

func TestPodStateCapturesContainerSelectionInputs(t *testing.T) {
	object := kubernetesObject{
		Metadata: objectMeta{
			Name:        "pod-a",
			Annotations: map[string]string{"kubectl.kubernetes.io/default-container": "worker"},
		},
		Spec: map[string]any{
			"containers": []any{
				map[string]any{"name": "sidecar"},
			},
			"initContainers":      []any{map[string]any{"name": "worker"}},
			"ephemeralContainers": []any{map[string]any{"name": "debugger"}},
		},
	}
	pod := podStateFromObject(object)
	if got := pod.Annotations["kubectl.kubernetes.io/default-container"]; got != "worker" {
		t.Fatalf("default container annotation=%q", got)
	}
	if got := strings.Join(pod.Containers, ","); got != "sidecar,worker,debugger" {
		t.Fatalf("containers=%q", got)
	}
	container, err := resolvePodContainer(pod, "")
	if err != nil || container != "worker" {
		t.Fatalf("container=%q err=%v", container, err)
	}
}

func TestSandboxReadinessPreservesDiagnostics(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	fake := readyFakeClient(cfg)
	pod := fake.pods["sandboxes/app=agent-sandbox"][0]
	pod.Ready = false
	pod.Conditions = []conditionState{{Type: "Ready", Status: "False", Reason: "ContainersNotReady"}}
	fake.pods["sandboxes/app=agent-sandbox"] = []podState{pod}
	_, err := sandboxReadinessOnce(context.Background(), fake, "sandboxes", "claim-a", fakeClaimIdentity(cfg))
	if err == nil || !strings.Contains(err.Error(), "ContainersNotReady") {
		t.Fatalf("err=%v", err)
	}
}

func TestWaitForSandboxReadinessRetriesTransientKubernetesErrors(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	fake := readyFakeClient(cfg)
	fake.getErrs = []error{errors.New("temporary API read failure")}
	fake.podListErrs = []error{errors.New("temporary pod list failure")}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ready, err := waitForSandboxReadiness(ctx, fake, "sandboxes", "claim-a", fakeClaimIdentity(cfg), time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if ready.SandboxName != "sandbox-a" || ready.PodName != "pod-a" {
		t.Fatalf("ready=%#v", ready)
	}
}

func TestWaitForSandboxReadinessRejectsTerminalStates(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*fakeKubernetesClient)
		wantError string
	}{
		{
			name: "finished sandbox",
			mutate: func(fake *fakeKubernetesClient) {
				fake.objects[sandboxResource+"/sandboxes/sandbox-a"].Status.Conditions = []conditionState{{
					Type: "Finished", Status: "True", Reason: "PodFailed", Message: "exit 1",
				}}
			},
			wantError: "Sandbox sandbox-a finished reason=PodFailed",
		},
		{
			name: "expired sandbox",
			mutate: func(fake *fakeKubernetesClient) {
				fake.objects[sandboxResource+"/sandboxes/sandbox-a"].Status.Conditions = []conditionState{
					{Type: "Finished", Status: "True", Reason: "Completed", Message: "command exited"},
					{Type: "Ready", Status: "False", Reason: "SandboxExpired", Message: "lifetime elapsed"},
				}
			},
			wantError: "Sandbox sandbox-a expired reason=SandboxExpired",
		},
		{
			name: "expired claim",
			mutate: func(fake *fakeKubernetesClient) {
				fake.objects[sandboxClaimResource+"/sandboxes/claim-a"].Status.Conditions = []conditionState{{
					Type: "Ready", Status: "False", Reason: "ClaimExpired", Message: "shutdown time elapsed",
				}}
			},
			wantError: "SandboxClaim claim-a expired reason=ClaimExpired",
		},
		{
			name: "succeeded pod",
			mutate: func(fake *fakeKubernetesClient) {
				pod := fake.pods["sandboxes/app=agent-sandbox"][0]
				pod.Phase = "Succeeded"
				pod.Ready = true
				fake.pods["sandboxes/app=agent-sandbox"] = []podState{pod}
			},
			wantError: "terminal phase=Succeeded",
		},
		{
			name: "failed pod",
			mutate: func(fake *fakeKubernetesClient) {
				pod := fake.pods["sandboxes/app=agent-sandbox"][0]
				pod.Phase = "Failed"
				pod.Ready = false
				fake.pods["sandboxes/app=agent-sandbox"] = []podState{pod}
			},
			wantError: "terminal phase=Failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.AgentSandbox.Context = "agent-context"
			cfg.AgentSandbox.Namespace = "sandboxes"
			cfg.AgentSandbox.WarmPool = "linux-pool"
			fake := readyFakeClient(cfg)
			tt.mutate(fake)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := waitForSandboxReadiness(ctx, fake, "sandboxes", "claim-a", fakeClaimIdentity(cfg), time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("err=%v want substring %q", err, tt.wantError)
			}
			if strings.Contains(err.Error(), "timed out") {
				t.Fatalf("terminal state was retried: %v", err)
			}
		})
	}
}

func TestWaitForSandboxPodReadinessRefreshesSandboxTerminalState(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	fake := readyFakeClient(cfg)
	sandbox := cloneKubernetesObject(fake.objects[sandboxResource+"/sandboxes/sandbox-a"])
	fake.objects[sandboxResource+"/sandboxes/sandbox-a"].Status.Conditions = []conditionState{{
		Type: "Finished", Status: "True", Reason: "PodFailed", Message: "exit 1",
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := waitForSandboxPodReadiness(ctx, fake, "sandboxes", "claim-a", sandbox, fakeClaimIdentity(cfg), time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "Sandbox sandbox-a finished reason=PodFailed") {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Fatalf("terminal state was retried: %v", err)
	}
}

func TestSandboxReadinessRejectsDownstreamIdentityMismatch(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*fakeKubernetesClient)
		wantError string
	}{
		{
			name: "claim warm pool",
			mutate: func(fake *fakeKubernetesClient) {
				fake.objects[sandboxClaimResource+"/sandboxes/claim-a"].Spec["warmPoolRef"].(map[string]any)["name"] = "other-pool"
			},
			wantError: "warm pool changed",
		},
		{
			name: "sandbox claim UID label",
			mutate: func(fake *fakeKubernetesClient) {
				fake.objects[sandboxResource+"/sandboxes/sandbox-a"].Metadata.Labels[agentSandboxClaimUIDLabel] = "uid-other"
			},
			wantError: "claim UID label changed",
		},
		{
			name: "sandbox owner UID",
			mutate: func(fake *fakeKubernetesClient) {
				fake.objects[sandboxResource+"/sandboxes/sandbox-a"].Metadata.OwnerReferences[0].UID = "uid-other"
			},
			wantError: "not controller-owned by SandboxClaim",
		},
		{
			name: "pod owner UID",
			mutate: func(fake *fakeKubernetesClient) {
				pod := fake.pods["sandboxes/app=agent-sandbox"][0]
				pod.OwnerReferences[0].UID = "uid-other"
				pod.Phase = "Pending"
				pod.Ready = false
				fake.pods["sandboxes/app=agent-sandbox"] = []podState{pod}
			},
			wantError: "not controller-owned by Sandbox",
		},
		{
			name: "pod claim UID label",
			mutate: func(fake *fakeKubernetesClient) {
				pod := fake.pods["sandboxes/app=agent-sandbox"][0]
				pod.Labels[agentSandboxClaimUIDLabel] = "uid-other"
				fake.pods["sandboxes/app=agent-sandbox"] = []podState{pod}
			},
			wantError: "claim UID label changed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.AgentSandbox.Context = "agent-context"
			cfg.AgentSandbox.Namespace = "sandboxes"
			cfg.AgentSandbox.WarmPool = "linux-pool"
			fake := readyFakeClient(cfg)
			tt.mutate(fake)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := waitForSandboxReadiness(ctx, fake, "sandboxes", "claim-a", fakeClaimIdentity(cfg), time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("err=%v want substring %q", err, tt.wantError)
			}
			if strings.Contains(err.Error(), "timed out") {
				t.Fatalf("identity mismatch was retried: %v", err)
			}
		})
	}
}

func TestValidateClaimIdentityRejectsLifecycleChange(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.WarmPool = "linux-pool"
	fake := readyFakeClient(cfg)
	claim := fake.objects[sandboxClaimResource+"/sandboxes/claim-a"]
	expiresAt := "2026-06-13T13:30:00Z"
	claim.Spec["lifecycle"] = map[string]any{"shutdownTime": expiresAt, "shutdownPolicy": "Retain"}
	identity := fakeClaimIdentity(cfg)
	identity.ExpiresAt = expiresAt
	if err := validateClaimIdentity(claim, identity); err != nil {
		t.Fatal(err)
	}
	claim.Spec["lifecycle"].(map[string]any)["shutdownTime"] = "2026-06-13T14:30:00Z"
	if err := validateClaimIdentity(claim, identity); err == nil || !strings.Contains(err.Error(), "lifecycle changed") {
		t.Fatalf("err=%v", err)
	}
}

func TestExecPodRevalidatesPinnedDownstreamUIDs(t *testing.T) {
	tests := []struct {
		name      string
		prepare   func(*fakeKubernetesClient)
		mutate    func(*fakeKubernetesClient)
		wantError string
	}{
		{
			name: "claim warm pool redirection",
			mutate: func(fake *fakeKubernetesClient) {
				fake.objects[sandboxClaimResource+"/sandboxes/claim-a"].Spec["warmPoolRef"].(map[string]any)["name"] = "other-pool"
			},
			wantError: "warm pool changed",
		},
		{
			name: "sandbox replacement",
			mutate: func(fake *fakeKubernetesClient) {
				fake.objects[sandboxResource+"/sandboxes/sandbox-a"].Metadata.UID = "uid-replacement"
			},
			wantError: "not controller-owned by Sandbox",
		},
		{
			name: "pod replacement",
			mutate: func(fake *fakeKubernetesClient) {
				pod := fake.pods["sandboxes/app=agent-sandbox"][0]
				pod.UID = "uid-replacement"
				fake.pods["sandboxes/app=agent-sandbox"] = []podState{pod}
			},
			wantError: "pod identity changed",
		},
		{
			name: "implicit container redirection",
			prepare: func(fake *fakeKubernetesClient) {
				pod := fake.pods["sandboxes/app=agent-sandbox"][0]
				pod.Containers = []string{"default", "sidecar"}
				pod.Annotations = map[string]string{"kubectl.kubernetes.io/default-container": "default"}
				fake.pods["sandboxes/app=agent-sandbox"] = []podState{pod}
			},
			mutate: func(fake *fakeKubernetesClient) {
				pod := fake.pods["sandboxes/app=agent-sandbox"][0]
				pod.Annotations["kubectl.kubernetes.io/default-container"] = "sidecar"
				fake.pods["sandboxes/app=agent-sandbox"] = []podState{pod}
			},
			wantError: "container changed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.AgentSandbox.Context = "agent-context"
			cfg.AgentSandbox.Namespace = "sandboxes"
			cfg.AgentSandbox.WarmPool = "linux-pool"
			fake := readyFakeClient(cfg)
			if tt.prepare != nil {
				tt.prepare(fake)
			}
			ready, err := sandboxReadinessOnce(context.Background(), fake, "sandboxes", "claim-a", fakeClaimIdentity(cfg))
			if err != nil {
				t.Fatal(err)
			}
			backend := &backend{cfg: cfg}
			if err := backend.execPod(context.Background(), fake, ready, podExecRequest{Command: []string{"true"}}); err != nil {
				t.Fatal(err)
			}
			if fake.execs[0].Container != ready.Container || ready.Container == "" {
				t.Fatalf("exec container=%q ready=%#v", fake.execs[0].Container, ready)
			}
			tt.mutate(fake)
			err = backend.execPod(context.Background(), fake, ready, podExecRequest{Command: []string{"true"}})
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("err=%v want substring %q", err, tt.wantError)
			}
			if len(fake.execs) != 1 {
				t.Fatalf("replacement reached exec: %#v", fake.execs)
			}
		})
	}
}

func TestExplicitContainerIgnoresDefaultContainerAnnotation(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	cfg.AgentSandbox.Container = "worker"
	fake := readyFakeClient(cfg)
	pod := fake.pods["sandboxes/app=agent-sandbox"][0]
	pod.Containers = []string{"worker", "sidecar"}
	pod.Annotations = map[string]string{"kubectl.kubernetes.io/default-container": "sidecar"}
	fake.pods["sandboxes/app=agent-sandbox"] = []podState{pod}
	ready, err := sandboxReadinessOnce(context.Background(), fake, "sandboxes", "claim-a", fakeClaimIdentity(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if ready.Container != "worker" {
		t.Fatalf("ready=%#v", ready)
	}
	backend := &backend{cfg: cfg}
	if err := backend.execPod(context.Background(), fake, ready, podExecRequest{Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if got := fake.execs[0].Container; got != "worker" {
		t.Fatalf("exec container=%q", got)
	}
}

func TestResolveSandboxPodRequiresControllerIdentity(t *testing.T) {
	fake := &fakeKubernetesClient{
		pods: map[string][]podState{
			"sandboxes/name=sandbox-a": {{Name: "sandbox-a", Ready: true}},
		},
	}
	_, err := resolveSandboxPod(context.Background(), fake, "sandboxes", &kubernetesObject{
		Metadata: objectMeta{Name: "sandbox-a"},
	})
	if err == nil || !strings.Contains(err.Error(), "no pod annotation or selector") {
		t.Fatalf("same-name pod fallback was accepted: %v", err)
	}
}

func TestRetainMissingClaimRequiresExplicitForget(t *testing.T) {
	cfg := core.BaseConfig()
	temp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", temp)
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	claim := LeaseClaim{LeaseID: "asbx_missing"}
	if err := retainMissingClaim(cfg, claim); err == nil {
		t.Fatal("missing claim was forgotten without explicit setting")
	}
	if err := claimLeaseForRepo(cfg, claim.LeaseID, "missing", Repo{Root: t.TempDir()}, false); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AgentSandbox.ForgetMissing = true
	if err := retainMissingClaim(cfg, claim); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorRBACRulesSplitPodExecSubresource(t *testing.T) {
	rules := doctorRBACRules("sandboxes")
	var execRule rbacRule
	for _, rule := range rules {
		if rule.Resource == podResource && rule.Subresource == "exec" {
			execRule = rule
			break
		}
	}
	if execRule.Resource == "" {
		t.Fatalf("doctor rules missing pod exec subresource rule: %#v", rules)
	}
	if execRule.Group != "" || execRule.Namespace != "sandboxes" || strings.Join(execRule.Verbs, ",") != "create" {
		t.Fatalf("execRule=%#v", execRule)
	}
	if execRule.String() != "create core/pods/exec namespace=sandboxes" {
		t.Fatalf("execRule string=%q", execRule.String())
	}
	for _, rule := range rules {
		if rule.Resource == "pods/exec" {
			t.Fatalf("pods/exec must be represented as resource pods plus subresource exec: %#v", rule)
		}
	}
}

func TestDoctorRBACRulesMatchRuntimeOperations(t *testing.T) {
	rules := doctorRBACRules("sandboxes")
	got := make([]string, 0, len(rules))
	for _, rule := range rules {
		got = append(got, rule.String())
	}
	want := []string{
		"get,create,delete extensions.agents.x-k8s.io/sandboxclaims namespace=sandboxes",
		"get extensions.agents.x-k8s.io/sandboxwarmpools namespace=sandboxes",
		"get agents.x-k8s.io/sandboxes namespace=sandboxes",
		"get,list core/pods namespace=sandboxes",
		"create core/pods/exec namespace=sandboxes",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
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
	_, err := waitForSandboxReadiness(ctx, fake, "sandboxes", "claim-a", fakeClaimIdentity(cfg), time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "claim-a") {
		t.Fatalf("err=%v", err)
	}
}

func TestKubectlClientUsesConfiguredBinaryContextAndStdinManifest(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Kubectl = "/opt/kubectl"
	cfg.AgentSandbox.Kubeconfig = "/tmp/cluster.yaml"
	cfg.AgentSandbox.Context = "agent-context"
	runner := &recordingCommandRunner{
		results: []LocalCommandResult{
			{Stdout: `{"resources":[{"name":"sandboxclaims"}]}`},
			{Stdout: `{"apiVersion":"extensions.agents.x-k8s.io/v1beta1","kind":"SandboxClaim","metadata":{"name":"claim-a","namespace":"sandboxes"}}`},
		},
	}
	clientRaw, err := newKubernetesClient(context.Background(), cfg, Runtime{Exec: runner})
	if err != nil {
		t.Fatal(err)
	}
	client := clientRaw.(*kubectlKubernetesClient)
	if err := client.CheckResource(context.Background(), agentSandboxExtensionsGroupVersion, sandboxClaimResource); err != nil {
		t.Fatal(err)
	}
	object := &kubernetesObject{
		APIVersion: agentSandboxExtensionsGroupVersion,
		Kind:       "SandboxClaim",
		Metadata:   objectMeta{Name: "claim-a", Namespace: "sandboxes"},
		Spec:       map[string]any{"warmPoolRef": map[string]any{"name": "linux-pool"}},
	}
	if _, err := client.Create(context.Background(), sandboxClaimGVR(), "sandboxes", object); err != nil {
		t.Fatal(err)
	}

	if len(runner.requests) != 2 {
		t.Fatalf("requests=%d", len(runner.requests))
	}
	for _, req := range runner.requests {
		if req.Name != "/opt/kubectl" {
			t.Fatalf("binary=%q", req.Name)
		}
		if req.MaxCapturedOutputBytes != kubectlCaptureLimitBytes {
			t.Fatalf("capture limit=%d", req.MaxCapturedOutputBytes)
		}
		if got := strings.Join(req.Args[:2], " "); got != "--kubeconfig=/tmp/cluster.yaml --context=agent-context" {
			t.Fatalf("global args=%q", got)
		}
	}
	if got := strings.Join(runner.requests[0].Args[2:], " "); got != "get --raw /apis/extensions.agents.x-k8s.io/v1beta1" {
		t.Fatalf("discovery args=%q", got)
	}
	if got := strings.Join(runner.requests[1].Args[2:], " "); got != "create --namespace=sandboxes -f - -o json" {
		t.Fatalf("create args=%q", got)
	}
	if len(runner.inputs) != 1 || !bytes.Contains(runner.inputs[0], []byte(`"warmPoolRef":{"name":"linux-pool"}`)) {
		t.Fatalf("create stdin=%q", runner.inputs)
	}
}

func TestKubectlCreateFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		result    LocalCommandResult
		err       error
		ambiguous bool
	}{
		{
			name:      "forbidden",
			result:    LocalCommandResult{Stderr: `Error from server (Forbidden): sandboxclaims is forbidden`},
			err:       errors.New("exit status 1"),
			ambiguous: false,
		},
		{
			name:      "missing kubectl",
			err:       errors.New(`exec: "kubectl": executable file not found in $PATH`),
			ambiguous: false,
		},
		{
			name:      "already exists",
			result:    LocalCommandResult{Stderr: `Error from server (AlreadyExists): sandboxclaims already exists`},
			err:       errors.New("exit status 1"),
			ambiguous: true,
		},
		{
			name:      "transport interruption",
			result:    LocalCommandResult{Stderr: "unexpected EOF"},
			err:       errors.New("exit status 1"),
			ambiguous: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := kubectlCreateMayHaveSucceeded(tc.result, tc.err); got != tc.ambiguous {
				t.Fatalf("ambiguous=%v want=%v", got, tc.ambiguous)
			}
		})
	}
}

func TestKubectlExecStreamsStdinWithoutPuttingItOnArgv(t *testing.T) {
	secret := "stdin-only-secret"
	runner := &recordingCommandRunner{results: []LocalCommandResult{{ExitCode: 0}}}
	client := &kubectlKubernetesClient{runner: runner, kubectl: "kubectl"}
	if err := client.Exec(context.Background(), podExecRequest{
		Namespace: "sandboxes",
		Pod:       "sandbox-a",
		Command:   []string{"sh", "-s"},
		Stdin:     strings.NewReader(secret),
	}); err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 1 || len(runner.inputs) != 1 {
		t.Fatalf("requests=%d inputs=%d", len(runner.requests), len(runner.inputs))
	}
	if args := strings.Join(runner.requests[0].Args, " "); strings.Contains(args, secret) || args != "exec --namespace=sandboxes -i pod/sandbox-a -- sh -s" {
		t.Fatalf("exec args=%q", args)
	}
	if string(runner.inputs[0]) != secret {
		t.Fatalf("stdin=%q", runner.inputs[0])
	}
}

func TestKubectlClientRejectsOptionLikeClusterNamesBeforeExec(t *testing.T) {
	runner := &recordingCommandRunner{}
	client := &kubectlKubernetesClient{runner: runner, kubectl: "kubectl"}
	if _, err := client.Get(context.Background(), sandboxGVR(), "sandboxes", "--server=attacker"); err == nil {
		t.Fatal("option-like Sandbox name was accepted")
	}
	if err := client.Exec(context.Background(), podExecRequest{
		Namespace: "sandboxes",
		Pod:       "--kubeconfig=attacker",
		Command:   []string{"true"},
	}); err == nil {
		t.Fatal("option-like pod name was accepted")
	}
	if len(runner.requests) != 0 {
		t.Fatalf("invalid names reached kubectl: %#v", runner.requests)
	}
}

func TestKubectlClientUsesUIDPreconditionForAsyncDelete(t *testing.T) {
	runner := &recordingCommandRunner{results: []LocalCommandResult{{Stdout: `{"kind":"Status","status":"Success"}`}}}
	client := &kubectlKubernetesClient{runner: runner, kubectl: "kubectl"}
	if err := client.Delete(context.Background(), sandboxClaimGVR(), "sandboxes", "claim-a", "uid-claim-a"); err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("requests=%d", len(runner.requests))
	}
	got := strings.Join(runner.requests[0].Args, " ")
	want := "delete --raw /apis/extensions.agents.x-k8s.io/v1beta1/namespaces/sandboxes/sandboxclaims/claim-a -f -"
	if got != want {
		t.Fatalf("delete args=%q want=%q", got, want)
	}
	if len(runner.inputs) != 1 || !bytes.Contains(runner.inputs[0], []byte(`"uid":"uid-claim-a"`)) {
		t.Fatalf("delete options=%q", runner.inputs)
	}
}

func TestSandboxReadinessRejectsReplacedClaimUID(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	fake := readyFakeClient(cfg)
	identity := fakeClaimIdentity(cfg)
	fake.objects[sandboxClaimResource+"/sandboxes/claim-a"].Metadata.UID = "uid-replacement"

	_, err := sandboxReadinessOnce(context.Background(), fake, "sandboxes", "claim-a", identity)
	if err == nil || !strings.Contains(err.Error(), "UID changed") {
		t.Fatalf("replacement claim accepted: %v", err)
	}
}

func TestKubectlCanIDenialUsesExitOneContract(t *testing.T) {
	tests := []struct {
		name        string
		result      LocalCommandResult
		err         error
		wantAllowed bool
		wantErr     bool
	}{
		{
			name:        "allowed",
			result:      LocalCommandResult{Stdout: "yes\n"},
			wantAllowed: true,
		},
		{
			name:   "denied",
			result: LocalCommandResult{ExitCode: 1, Stdout: "no - RBAC: access denied\n"},
			err:    errors.New("exit status 1"),
		},
		{
			name:    "transport failure",
			result:  LocalCommandResult{ExitCode: 2, Stderr: "connection refused\n"},
			err:     errors.New("exit status 2"),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &recordingCommandRunner{
				results: []LocalCommandResult{tt.result},
				errors:  []error{tt.err},
			}
			client := &kubectlKubernetesClient{runner: runner, kubectl: "kubectl"}
			allowed, err := client.CanI(context.Background(), rbacRule{
				Resource:  "pods",
				Namespace: "sandboxes",
				Verbs:     []string{"get"},
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if allowed != tt.wantAllowed {
				t.Fatalf("allowed=%v want=%v", allowed, tt.wantAllowed)
			}
		})
	}
}

func TestKubectlExecOnlyMapsProvenRemoteExitStatus(t *testing.T) {
	tests := []struct {
		name       string
		result     LocalCommandResult
		wantRemote bool
	}{
		{
			name:       "remote command",
			result:     LocalCommandResult{ExitCode: 42, Stderr: "command terminated with exit code 42\n"},
			wantRemote: true,
		},
		{
			name:       "remote stderr without newline",
			result:     LocalCommandResult{ExitCode: 42, Stderr: "failurecommand terminated with exit code 42\n"},
			wantRemote: true,
		},
		{
			name:   "transport failure",
			result: LocalCommandResult{ExitCode: 1, Stderr: "Unable to connect to the server: dial tcp: connection refused\n"},
		},
		{
			name:   "mismatched diagnostic",
			result: LocalCommandResult{ExitCode: 1, Stderr: "command terminated with exit code 42\n"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &recordingCommandRunner{
				results: []LocalCommandResult{tt.result},
				errors:  []error{errors.New("kubectl failed")},
			}
			client := &kubectlKubernetesClient{runner: runner, kubectl: "kubectl"}
			err := client.Exec(context.Background(), podExecRequest{
				Namespace: "sandboxes",
				Pod:       "sandbox-a",
				Command:   []string{"false"},
			})
			if err == nil {
				t.Fatal("exec unexpectedly succeeded")
			}
			_, remote := remoteExitStatus(err)
			if remote != tt.wantRemote {
				t.Fatalf("remote=%v want=%v err=%v", remote, tt.wantRemote, err)
			}
		})
	}
}

func readyFakeClient(cfg Config) *fakeKubernetesClient {
	identity := fakeClaimIdentity(cfg)
	claim := &kubernetesObject{Metadata: objectMeta{
		Name:        "claim-a",
		UID:         identity.UID,
		Labels:      claimLabels(identity.LeaseID, "test"),
		Annotations: claimAnnotations(cfg),
	}, Spec: map[string]any{"warmPoolRef": map[string]any{"name": cfg.AgentSandbox.WarmPool}}}
	claim.Status.Sandbox.Name = "sandbox-a"
	sandboxUID := "uid-sandbox-a"
	sandbox := &kubernetesObject{
		Metadata: objectMeta{
			Name:   "sandbox-a",
			UID:    sandboxUID,
			Labels: map[string]string{agentSandboxClaimUIDLabel: identity.UID},
			OwnerReferences: []ownerReference{{
				APIVersion: agentSandboxExtensionsGroupVersion,
				Kind:       "SandboxClaim",
				Name:       claim.Metadata.Name,
				UID:        identity.UID,
				Controller: true,
			}},
		},
		Status: objectStatus{
			Selector:   "app=agent-sandbox",
			Conditions: []conditionState{{Type: "Ready", Status: "True"}},
		},
	}
	warmPool := &kubernetesObject{Metadata: objectMeta{Name: cfg.AgentSandbox.WarmPool}}
	fake := &fakeKubernetesClient{
		resources: map[string]map[string]bool{
			agentSandboxCoreGroupVersion:       {sandboxResource: true},
			agentSandboxExtensionsGroupVersion: {sandboxClaimResource: true, warmPoolResource: true},
		},
		objects: map[string]*kubernetesObject{
			sandboxClaimResource + "/sandboxes/claim-a":                  claim,
			sandboxResource + "/sandboxes/sandbox-a":                     sandbox,
			warmPoolResource + "/sandboxes/" + cfg.AgentSandbox.WarmPool: warmPool,
		},
		rbac: map[string]bool{},
		pods: map[string][]podState{
			"sandboxes/app=agent-sandbox": {{
				Name:       "pod-a",
				UID:        "uid-pod-a",
				Labels:     map[string]string{agentSandboxClaimUIDLabel: identity.UID},
				Containers: []string{testPodContainer(cfg.AgentSandbox.Container)},
				OwnerReferences: []ownerReference{{
					APIVersion: agentSandboxCoreGroupVersion,
					Kind:       "Sandbox",
					Name:       sandbox.Metadata.Name,
					UID:        sandboxUID,
					Controller: true,
				}},
				Phase: "Running",
				PodIP: "10.0.0.10",
				Ready: true,
			}},
		},
	}
	for _, rule := range doctorRBACRules(cfg.AgentSandbox.Namespace) {
		fake.rbac[rule.String()] = true
	}
	return fake
}

func fakeClaimIdentity(cfg Config) claimIdentity {
	return claimIdentity{LeaseID: "asbx_test", ProviderScope: claimScope(cfg), UID: "uid-claim-a", WarmPool: cfg.AgentSandbox.WarmPool, Container: strings.TrimSpace(cfg.AgentSandbox.Container)}
}

func testPodContainer(configured string) string {
	if configured = strings.TrimSpace(configured); configured != "" && configured != "default" {
		return configured
	}
	return "default"
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
