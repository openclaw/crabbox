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
	resources   map[string]map[string]bool
	objects     map[string]*kubernetesObject
	rbac        map[string]bool
	pods        map[string][]podState
	gets        []string
	execs       []podExecRequest
	execInput   [][]byte
	execErrs    []error
	execStarted chan struct{}
	execRelease chan struct{}
	deleteErrs  []error
	creates     int
	deletes     int
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

func (f *fakeKubernetesClient) Get(_ context.Context, ref resourceRef, namespace, name string) (*kubernetesObject, error) {
	key := ref.Resource + "/" + namespace + "/" + name
	f.gets = append(f.gets, key)
	obj := f.objects[key]
	if obj == nil {
		return nil, errKubernetesNotFound
	}
	return cloneKubernetesObject(obj), nil
}

func (f *fakeKubernetesClient) Create(_ context.Context, ref resourceRef, namespace string, obj *kubernetesObject) (*kubernetesObject, error) {
	key := ref.Resource + "/" + namespace + "/" + obj.Metadata.Name
	if f.objects == nil {
		f.objects = map[string]*kubernetesObject{}
	}
	if f.objects[key] != nil {
		return nil, errors.New("already exists " + key)
	}
	f.creates++
	created := cloneKubernetesObject(obj)
	f.objects[key] = created
	if ref.Resource == sandboxClaimResource {
		sandboxName := obj.Metadata.Name + "-sandbox"
		podName := obj.Metadata.Name + "-pod"
		created.Status.Sandbox.Name = sandboxName
		sandbox := &kubernetesObject{
			Metadata: objectMeta{Name: sandboxName},
			Status: objectStatus{
				Selector:   "claim=" + obj.Metadata.Name,
				Conditions: []conditionState{{Type: "Ready", Status: "True"}},
			},
		}
		f.objects[sandboxResource+"/"+namespace+"/"+sandboxName] = sandbox
		if f.pods == nil {
			f.pods = map[string][]podState{}
		}
		f.pods[namespace+"/claim="+obj.Metadata.Name] = []podState{{Name: podName, Phase: "Running", PodIP: "10.0.0.11", Ready: true}}
	}
	return cloneKubernetesObject(created), nil
}

func (f *fakeKubernetesClient) Delete(_ context.Context, ref resourceRef, namespace, name string) error {
	if len(f.deleteErrs) > 0 {
		err := f.deleteErrs[0]
		f.deleteErrs = f.deleteErrs[1:]
		return err
	}
	key := ref.Resource + "/" + namespace + "/" + name
	if f.objects[key] == nil {
		return errKubernetesNotFound
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
	claim := LeaseClaim{LeaseID: "asbx_missing"}
	if err := retainMissingClaim(cfg, claim); err == nil {
		t.Fatal("missing claim was forgotten without explicit setting")
	}
	cfg.AgentSandbox.ForgetMissing = true
	temp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", temp)
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
	_, err := waitForSandboxReadiness(ctx, fake, "sandboxes", "claim-a", time.Millisecond)
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
		if got := strings.Join(req.Args[:4], " "); got != "--kubeconfig /tmp/cluster.yaml --context agent-context" {
			t.Fatalf("global args=%q", got)
		}
	}
	if got := strings.Join(runner.requests[0].Args[4:], " "); got != "get --raw /apis/extensions.agents.x-k8s.io/v1beta1" {
		t.Fatalf("discovery args=%q", got)
	}
	if got := strings.Join(runner.requests[1].Args[4:], " "); got != "create --namespace sandboxes -f - -o json" {
		t.Fatalf("create args=%q", got)
	}
	if len(runner.inputs) != 1 || !bytes.Contains(runner.inputs[0], []byte(`"warmPoolRef":{"name":"linux-pool"}`)) {
		t.Fatalf("create stdin=%q", runner.inputs)
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
	if args := strings.Join(runner.requests[0].Args, " "); strings.Contains(args, secret) || args != "exec --namespace sandboxes -i sandbox-a -- sh -s" {
		t.Fatalf("exec args=%q", args)
	}
	if string(runner.inputs[0]) != secret {
		t.Fatalf("stdin=%q", runner.inputs[0])
	}
}

func TestKubectlClientUsesVersionedResourcesAndAsyncDelete(t *testing.T) {
	runner := &recordingCommandRunner{}
	client := &kubectlKubernetesClient{runner: runner, kubectl: "kubectl"}
	if err := client.Delete(context.Background(), sandboxClaimGVR(), "sandboxes", "claim-a"); err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("requests=%d", len(runner.requests))
	}
	got := strings.Join(runner.requests[0].Args, " ")
	want := "delete sandboxclaims.v1beta1.extensions.agents.x-k8s.io claim-a --namespace sandboxes --ignore-not-found=true --wait=false"
	if got != want {
		t.Fatalf("delete args=%q want=%q", got, want)
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
	claim := &kubernetesObject{Metadata: objectMeta{Name: "claim-a"}}
	claim.Status.Sandbox.Name = "sandbox-a"
	sandbox := &kubernetesObject{
		Metadata: objectMeta{Name: "sandbox-a"},
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
