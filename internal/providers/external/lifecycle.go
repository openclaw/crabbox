package external

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	lifecycleOutputNone           = ""
	lifecycleOutputJSONNameArray  = "json-name-array"
	lifecycleOutputJSONLeaseArray = "json-lease-array"
	externalResourceNameLabel     = "externalResourceName"
)

var lifecyclePlaceholderPattern = regexp.MustCompile(`\{\{([A-Za-z_][A-Za-z0-9_.-]*)\}\}`)
var lifecycleEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type lifecycleTemplateContext struct {
	values map[string]string
}

func (b *leaseBackend) invokeLifecycle(ctx context.Context, request protocolRequest) (protocolResponse, error) {
	operation := lifecycleOperation(b.cfg.External.Lifecycle, request.Operation)
	if request.Operation == "cleanup" && request.DryRun {
		return b.defaultLifecycleResponse(request)
	}
	if len(operation.Argv) == 0 {
		return b.defaultLifecycleResponse(request)
	}
	templateCtx, err := b.lifecycleContext(request, "")
	if err != nil {
		return protocolResponse{}, err
	}
	argv, err := expandLifecycleArgv(operation.Argv, templateCtx)
	if err != nil {
		return protocolResponse{}, core.Exit(2, "external lifecycle %s: %v", request.Operation, err)
	}
	var defaultResponse *protocolResponse
	if operation.Output == lifecycleOutputNone && lifecycleDefaultLeaseResponseOperation(request.Operation) {
		response, err := b.defaultLifecycleResponse(request)
		if err != nil {
			return protocolResponse{}, err
		}
		defaultResponse = &response
	}
	result, err := b.rt.Exec.Run(ctx, core.LocalCommandRequest{
		Name:   argv[0],
		Args:   argv[1:],
		Stderr: b.rt.Stderr,
	})
	if err != nil {
		message := strings.TrimSpace(result.Stderr)
		if message == "" {
			message = strings.TrimSpace(result.Stdout)
		}
		return protocolResponse{}, core.Exit(result.ExitCode, "external lifecycle %s failed: %v: %s", request.Operation, err, message)
	}
	if operation.Output == lifecycleOutputNone && strings.TrimSpace(result.Stdout) != "" && b.rt.Stderr != nil {
		_, _ = io.WriteString(b.rt.Stderr, result.Stdout)
		if !strings.HasSuffix(result.Stdout, "\n") {
			_, _ = io.WriteString(b.rt.Stderr, "\n")
		}
	}
	switch operation.Output {
	case lifecycleOutputNone:
		if defaultResponse != nil {
			return *defaultResponse, nil
		}
		return b.defaultLifecycleResponse(request)
	case lifecycleOutputJSONNameArray:
		namePrefix := ""
		if operation.NamePrefix != "" {
			namePrefix, err = expandLifecycleValue(operation.NamePrefix, templateCtx)
			if err != nil {
				return protocolResponse{}, core.Exit(2, "external lifecycle list namePrefix: %v", err)
			}
		}
		var names []string
		if err := json.Unmarshal([]byte(result.Stdout), &names); err != nil {
			return protocolResponse{}, core.Exit(5, "external lifecycle %s returned invalid JSON name array: %v", request.Operation, err)
		}
		leases := make([]protocolLease, 0, len(names))
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if namePrefix != "" && !strings.HasPrefix(name, namePrefix) {
				continue
			}
			lease, err := b.lifecycleLeaseForName(name)
			if err != nil {
				return protocolResponse{}, err
			}
			leases = append(leases, lease)
		}
		return protocolResponse{ProtocolVersion: protocolVersion, Leases: leases}, nil
	case lifecycleOutputJSONLeaseArray:
		var leases []protocolLease
		if err := json.Unmarshal([]byte(result.Stdout), &leases); err != nil {
			return protocolResponse{}, core.Exit(5, "external lifecycle %s returned invalid JSON lease array: %v", request.Operation, err)
		}
		return protocolResponse{ProtocolVersion: protocolVersion, Leases: leases}, nil
	default:
		return protocolResponse{}, core.Exit(2, "external lifecycle %s has unsupported output %q", request.Operation, operation.Output)
	}
}

func lifecycleDefaultLeaseResponseOperation(operation string) bool {
	switch operation {
	case "acquire", "resolve", "touch":
		return true
	default:
		return false
	}
}

func (b *leaseBackend) defaultLifecycleResponse(request protocolRequest) (protocolResponse, error) {
	response := protocolResponse{ProtocolVersion: protocolVersion}
	switch request.Operation {
	case "doctor":
		response.Message = "declarative external provider ready"
	case "acquire", "resolve":
		if request.Operation == "resolve" && request.ReleaseOnly && len(b.cfg.External.Lifecycle.Resolve.Argv) == 0 {
			lease := lifecycleMinimalLease(request)
			response.Lease = &lease
			break
		}
		lease, err := b.lifecycleLease(request)
		if err != nil {
			return protocolResponse{}, err
		}
		response.Lease = &lease
	case "touch":
	case "list":
		response.Leases = []protocolLease{}
	case "release", "cleanup":
	default:
		return protocolResponse{}, core.Exit(2, "external lifecycle operation %q is unsupported", request.Operation)
	}
	return response, nil
}

func lifecycleMinimalLease(request protocolRequest) protocolLease {
	desired := request.Desired
	if desired == nil && request.Lease != nil {
		desired = &desiredLease{
			LeaseID: request.Lease.LeaseID,
			Slug:    request.Lease.Slug,
			Name:    request.Lease.Name,
		}
	}
	if desired == nil {
		name := strings.TrimSpace(request.ID)
		desired = &desiredLease{LeaseID: name, Slug: core.NormalizeLeaseSlug(name), Name: name}
	}
	return protocolLease{
		LeaseID: desired.LeaseID,
		Slug:    desired.Slug,
		Name:    desired.Name,
		Status:  "ready",
	}
}

func lifecycleOperation(cfg core.ExternalLifecycleConfig, operation string) core.ExternalLifecycleOperation {
	switch operation {
	case "doctor":
		return cfg.Doctor
	case "acquire":
		return cfg.Acquire
	case "resolve":
		return cfg.Resolve
	case "list":
		return cfg.List
	case "release":
		return cfg.Release
	case "touch":
		return cfg.Touch
	case "cleanup":
		return cfg.Cleanup
	default:
		return core.ExternalLifecycleOperation{}
	}
}

func (b *leaseBackend) lifecycleLease(request protocolRequest) (protocolLease, error) {
	resourceName := ""
	if request.Lease != nil {
		resourceName = strings.TrimSpace(request.Lease.Labels[externalResourceNameLabel])
		if resourceName == "" {
			resourceName = strings.TrimSpace(request.Lease.Name)
		}
	}
	desired := request.Desired
	if desired == nil && request.Lease != nil {
		desired = &desiredLease{
			LeaseID: request.Lease.LeaseID,
			Slug:    request.Lease.Slug,
			Name:    request.Lease.Name,
		}
	}
	if desired == nil {
		name := strings.TrimSpace(request.ID)
		desired = &desiredLease{LeaseID: name, Slug: core.NormalizeLeaseSlug(name), Name: name}
	}
	request.Desired = desired
	return b.lifecycleLeaseForRequest(request, resourceName)
}

func (b *leaseBackend) lifecycleLeaseForName(name string) (protocolLease, error) {
	desired := desiredLease{LeaseID: name, Slug: core.NormalizeLeaseSlug(name), Name: name}
	resourceName := ""
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return protocolLease{}, err
	}
	for _, claim := range claims {
		if !externalClaimMatchesScope(claim, b.claimScope()) {
			continue
		}
		if strings.TrimSpace(claim.Labels["name"]) == name ||
			strings.TrimSpace(claim.Labels[externalResourceNameLabel]) == name {
			desired.LeaseID = claim.LeaseID
			desired.Slug = claim.Slug
			desired.Name = strings.TrimSpace(claim.Labels["name"])
			resourceName = name
			break
		}
	}
	return b.lifecycleLeaseForRequest(protocolRequest{Desired: &desired}, resourceName)
}

func (b *leaseBackend) lifecycleLeaseForRequest(request protocolRequest, resourceName string) (protocolLease, error) {
	templateCtx, err := b.lifecycleContext(request, resourceName)
	if err != nil {
		return protocolLease{}, err
	}
	desired := request.Desired
	if desired == nil {
		return protocolLease{}, core.Exit(2, "external lifecycle lease requires desired identity")
	}
	connection := b.cfg.External.Connection
	cloudID, err := expandLifecycleValue(core.Blank(connection.CloudID, "{{name}}"), templateCtx)
	if err != nil {
		return protocolLease{}, core.Exit(2, "external connection cloudId: %v", err)
	}
	serverType, err := expandLifecycleValue(core.Blank(connection.ServerType, "external"), templateCtx)
	if err != nil {
		return protocolLease{}, core.Exit(2, "external connection serverType: %v", err)
	}
	labels := make(map[string]string, len(connection.Labels))
	for key, value := range connection.Labels {
		expanded, err := expandLifecycleValue(value, templateCtx)
		if err != nil {
			return protocolLease{}, core.Exit(2, "external connection label %s: %v", key, err)
		}
		labels[key] = expanded
	}
	labels[externalResourceNameLabel] = templateCtx.values["resourceName"]
	ssh, err := lifecycleSSH(connection.SSH, templateCtx)
	if err != nil {
		return protocolLease{}, err
	}
	return protocolLease{
		LeaseID:    desired.LeaseID,
		Slug:       desired.Slug,
		Name:       desired.Name,
		CloudID:    cloudID,
		Status:     "ready",
		ServerType: serverType,
		Labels:     labels,
		SSH:        &ssh,
	}, nil
}

func lifecycleSSH(cfg core.ExternalSSHConnectionConfig, templateCtx lifecycleTemplateContext) (protocolSSH, error) {
	expand := func(label, value string) (string, error) {
		expanded, err := expandLifecycleValue(value, templateCtx)
		if err != nil {
			return "", core.Exit(2, "external connection ssh.%s: %v", label, err)
		}
		return expanded, nil
	}
	user, err := expand("user", cfg.User)
	if err != nil {
		return protocolSSH{}, err
	}
	host, err := expand("host", core.Blank(cfg.Host, "{{resourceName}}"))
	if err != nil {
		return protocolSSH{}, err
	}
	key, err := expand("key", cfg.Key)
	if err != nil {
		return protocolSSH{}, err
	}
	port, err := expand("port", core.Blank(cfg.Port, "22"))
	if err != nil {
		return protocolSSH{}, err
	}
	readyCheck, err := expand("readyCheck", core.Blank(cfg.ReadyCheck, externalDefaultReadyCheck))
	if err != nil {
		return protocolSSH{}, err
	}
	proxyCommand, err := expand("proxyCommand", cfg.ProxyCommand)
	if err != nil {
		return protocolSSH{}, err
	}
	fallbackPorts := make([]string, 0, len(cfg.FallbackPorts))
	for _, fallback := range cfg.FallbackPorts {
		expanded, err := expand("fallbackPorts", fallback)
		if err != nil {
			return protocolSSH{}, err
		}
		fallbackPorts = append(fallbackPorts, expanded)
	}
	return protocolSSH{
		User:            user,
		Host:            host,
		Key:             key,
		Port:            port,
		FallbackPorts:   fallbackPorts,
		ReadyCheck:      readyCheck,
		AuthSecret:      cfg.AuthSecret,
		NoControlMaster: cfg.NoControlMaster,
		SSHConfigProxy:  cfg.SSHConfigProxy,
		ProxyCommand:    proxyCommand,
	}, nil
}

func lifecycleContext(request protocolRequest, config map[string]any) (lifecycleTemplateContext, error) {
	values := map[string]string{
		"id":          strings.TrimSpace(request.ID),
		"state":       strings.TrimSpace(request.State),
		"keep":        strconv.FormatBool(request.Keep),
		"reclaim":     strconv.FormatBool(request.Reclaim),
		"releaseOnly": strconv.FormatBool(request.ReleaseOnly),
		"force":       strconv.FormatBool(request.Force),
		"all":         strconv.FormatBool(request.All),
		"refresh":     strconv.FormatBool(request.Refresh),
		"dryRun":      strconv.FormatBool(request.DryRun),
	}
	if request.Desired != nil {
		values["leaseId"] = request.Desired.LeaseID
		values["slug"] = request.Desired.Slug
		values["name"] = request.Desired.Name
	}
	if request.Lease != nil {
		if values["leaseId"] == "" {
			values["leaseId"] = request.Lease.LeaseID
		}
		if values["slug"] == "" {
			values["slug"] = request.Lease.Slug
		}
		if values["name"] == "" {
			values["name"] = request.Lease.Name
		}
		values["cloudId"] = request.Lease.CloudID
	}
	if values["id"] == "" {
		values["id"] = values["leaseId"]
	}
	if values["name"] == "" {
		values["name"] = values["id"]
	}
	if values["slug"] == "" {
		values["slug"] = core.NormalizeLeaseSlug(values["name"])
	}
	if values["leaseId"] == "" {
		values["leaseId"] = values["id"]
	}
	values["leaseIdSlug"] = core.NormalizeLeaseSlug(values["leaseId"])
	if request.Repo != nil {
		values["repo.root"] = request.Repo.Root
		values["repo.name"] = request.Repo.Name
		values["repo.remoteUrl"] = request.Repo.RemoteURL
		values["repo.head"] = request.Repo.Head
		values["repo.baseRef"] = request.Repo.BaseRef
	}
	for key, value := range config {
		addLifecycleConfigValues(values, "config."+key, value)
	}
	return lifecycleTemplateContext{values: values}, nil
}

func (b *leaseBackend) lifecycleContext(request protocolRequest, resourceName string) (lifecycleTemplateContext, error) {
	templateCtx, err := lifecycleContext(request, b.cfg.External.Config)
	if err != nil {
		return lifecycleTemplateContext{}, err
	}
	if resourceName == "" && request.Lease != nil {
		resourceName = strings.TrimSpace(request.Lease.Labels[externalResourceNameLabel])
		if resourceName == "" {
			resourceName = strings.TrimSpace(request.Lease.Name)
		}
	}
	if resourceName == "" {
		resourceName, err = expandLifecycleValue(
			core.Blank(b.cfg.External.Connection.ResourceName, "{{name}}"),
			templateCtx,
		)
		if err != nil {
			return lifecycleTemplateContext{}, core.Exit(2, "external connection resourceName: %v", err)
		}
	}
	templateCtx.values["resourceName"] = resourceName
	return templateCtx, nil
}

func addLifecycleConfigValues(values map[string]string, key string, value any) {
	switch typed := value.(type) {
	case string:
		values[key] = typed
	case bool:
		values[key] = strconv.FormatBool(typed)
	case float64:
		values[key] = strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		values[key] = strconv.FormatFloat(float64(typed), 'f', -1, 32)
	case int:
		values[key] = strconv.Itoa(typed)
	case int64:
		values[key] = strconv.FormatInt(typed, 10)
	case nil:
		values[key] = ""
	case map[string]any:
		for nestedKey, nestedValue := range typed {
			addLifecycleConfigValues(values, key+"."+nestedKey, nestedValue)
		}
	}
}

func expandLifecycleArgv(argv []string, templateCtx lifecycleTemplateContext) ([]string, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("argv is empty")
	}
	expanded := make([]string, len(argv))
	for index, value := range argv {
		item, err := expandLifecycleValue(value, templateCtx)
		if err != nil {
			return nil, fmt.Errorf("argv[%d]: %w", index, err)
		}
		if strings.ContainsRune(item, '\x00') {
			return nil, fmt.Errorf("argv[%d] contains a NUL byte", index)
		}
		expanded[index] = item
	}
	if strings.TrimSpace(expanded[0]) == "" {
		return nil, fmt.Errorf("argv executable is empty")
	}
	return expanded, nil
}

func expandLifecycleValue(value string, templateCtx lifecycleTemplateContext) (string, error) {
	var expansionErr error
	expanded := lifecyclePlaceholderPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := lifecyclePlaceholderPattern.FindStringSubmatch(match)
		key := parts[1]
		if strings.HasPrefix(key, "env.") {
			name := strings.TrimPrefix(key, "env.")
			if !lifecycleEnvNamePattern.MatchString(name) {
				expansionErr = fmt.Errorf("invalid environment placeholder %q", key)
				return ""
			}
			envValue, ok := os.LookupEnv(name)
			if !ok {
				expansionErr = fmt.Errorf("environment variable %s is not set", name)
				return ""
			}
			return envValue
		}
		replacement, ok := templateCtx.values[key]
		if !ok {
			expansionErr = fmt.Errorf("unknown placeholder %q", key)
			return ""
		}
		return replacement
	})
	if expansionErr != nil {
		return "", expansionErr
	}
	if strings.Contains(expanded, "{{") || strings.Contains(expanded, "}}") {
		return "", fmt.Errorf("invalid placeholder syntax in %q", value)
	}
	return expanded, nil
}
