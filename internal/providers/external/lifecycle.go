package external

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	lifecycleOutputNone           = ""
	lifecycleOutputJSONLease      = "json-lease"
	lifecycleOutputJSONNameArray  = "json-name-array"
	lifecycleOutputJSONLeaseArray = "json-lease-array"
	externalResourceNameLabel     = "externalResourceName"
	externalResourceNameFromEnv   = "externalResourceNameFromEnv"
)

var lifecycleRollbackTimeout = 30 * time.Second

var lifecyclePlaceholderPattern = regexp.MustCompile(`\{\{([A-Za-z_][A-Za-z0-9_.-]*)\}\}`)
var lifecycleEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type lifecycleTemplateContext struct {
	values    map[string]string
	sensitive map[string]bool
}

func (b *leaseBackend) invokeLifecycle(ctx context.Context, request protocolRequest) (protocolResponse, error) {
	operation := lifecycleOperation(b.cfg.External.Lifecycle, request.Operation)
	passwordEnvs := core.ExternalDesktopChildEnvironmentDenylist(b.cfg)
	for _, passwordEnv := range passwordEnvs {
		if err := rejectLifecycleDesktopPasswordConnectionReferences(b.cfg.External.Connection, passwordEnv); err != nil {
			return protocolResponse{}, core.Exit(2, "external lifecycle %s: %v", request.Operation, err)
		}
	}
	if request.Operation == "cleanup" && request.DryRun {
		return b.defaultLifecycleResponse(request)
	}
	commands := lifecycleOperationCommands(operation)
	if len(commands) == 0 {
		return b.defaultLifecycleResponse(request)
	}
	for _, passwordEnv := range passwordEnvs {
		if err := rejectLifecycleDesktopPasswordArgvReferences(commands, passwordEnv); err != nil {
			return protocolResponse{}, core.Exit(2, "external lifecycle %s: %v", request.Operation, err)
		}
		if err := rejectLifecycleDesktopPasswordNamePrefixReference(operation.NamePrefix, passwordEnv); err != nil {
			return protocolResponse{}, core.Exit(2, "external lifecycle %s: %v", request.Operation, err)
		}
		if err := rejectLifecycleDesktopPasswordEnvReferences(operation.Env, passwordEnv); err != nil {
			return protocolResponse{}, core.Exit(2, "external lifecycle %s env: %v", request.Operation, err)
		}
	}
	templateCtx, err := b.lifecycleContext(request, "")
	if err != nil {
		return protocolResponse{}, err
	}
	expandedCommands := make([][]string, len(commands))
	for index, command := range commands {
		argv, err := expandLifecycleArgv(command, templateCtx, operation.AllowEnvArgv)
		if err != nil {
			return protocolResponse{}, core.Exit(2, "external lifecycle %s step %d: %v", request.Operation, index+1, err)
		}
		expandedCommands[index] = argv
	}
	commandEnv, err := expandLifecycleEnv(operation.Env, templateCtx)
	if err != nil {
		return protocolResponse{}, core.Exit(2, "external lifecycle %s env: %v", request.Operation, err)
	}
	commandEnv = externalAdapterEnv(b.cfg, commandEnv)
	var defaultResponse *protocolResponse
	if operation.Output == lifecycleOutputNone && lifecycleDefaultLeaseResponseOperation(request.Operation) {
		response, err := b.defaultLifecycleResponse(request)
		if err != nil {
			return protocolResponse{}, err
		}
		defaultResponse = &response
	}
	var result core.LocalCommandResult
	for index, argv := range expandedCommands {
		result, err = b.rt.Exec.Run(ctx, core.LocalCommandRequest{
			Name:                   argv[0],
			Args:                   argv[1:],
			Env:                    commandEnv,
			Stderr:                 b.rt.Stderr,
			MaxCapturedOutputBytes: externalProviderOutputMaxBytes,
		})
		if limitErr := validateExternalCommandOutputSize(result); limitErr != nil {
			return protocolResponse{}, limitErr
		}
		if err != nil {
			var rollbackErr error
			if operation.RollbackOnFailure && index > 0 && !request.Keep {
				rollbackErr = b.rollbackLifecycleAcquire(ctx, request)
			}
			message := strings.TrimSpace(result.Stderr)
			if message == "" {
				message = strings.TrimSpace(result.Stdout)
			}
			if rollbackErr != nil {
				message = fmt.Sprintf("%s; rollback failed: %v", message, rollbackErr)
			}
			return protocolResponse{}, core.Exit(
				result.ExitCode,
				"external lifecycle %s step %d failed: %v: %s",
				request.Operation,
				index+1,
				err,
				message,
			)
		}
		if (index < len(expandedCommands)-1 || operation.Output == lifecycleOutputNone) &&
			strings.TrimSpace(result.Stdout) != "" && b.rt.Stderr != nil {
			_, _ = io.WriteString(b.rt.Stderr, result.Stdout)
			if !strings.HasSuffix(result.Stdout, "\n") {
				_, _ = io.WriteString(b.rt.Stderr, "\n")
			}
		}
	}
	switch operation.Output {
	case lifecycleOutputNone:
		if defaultResponse != nil {
			return *defaultResponse, nil
		}
		return b.defaultLifecycleResponse(request)
	case lifecycleOutputJSONLease:
		var lease protocolLease
		if err := json.Unmarshal([]byte(result.Stdout), &lease); err != nil {
			return protocolResponse{}, core.Exit(5, "external lifecycle %s returned invalid JSON lease: %v", request.Operation, err)
		}
		if err := validateRawExternalLeaseIdentity(request.Operation, lease); err != nil {
			return protocolResponse{}, err
		}
		response := protocolResponse{ProtocolVersion: protocolVersion, Lease: &lease, RawLifecycleIdentity: true}
		if err := b.validateProviderSSHOutput(request, response); err != nil {
			return protocolResponse{}, err
		}
		return response, nil
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
		if names == nil {
			return protocolResponse{}, core.Exit(5, "external lifecycle %s returned null instead of a JSON name array", request.Operation)
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
		response := protocolResponse{ProtocolVersion: protocolVersion, Leases: leases}
		if err := b.validateProviderSSHOutput(request, response); err != nil {
			return protocolResponse{}, err
		}
		return response, nil
	case lifecycleOutputJSONLeaseArray:
		var leases []protocolLease
		if err := json.Unmarshal([]byte(result.Stdout), &leases); err != nil {
			return protocolResponse{}, core.Exit(5, "external lifecycle %s returned invalid JSON lease array: %v", request.Operation, err)
		}
		if leases == nil {
			return protocolResponse{}, core.Exit(5, "external lifecycle %s returned null instead of a JSON lease array", request.Operation)
		}
		if b.cfg.External.Capabilities.IdempotentLeaseID && lifecycleControllerIdentityAttestationConfigured(b.cfg.External) {
			for index, lease := range leases {
				if err := validateRawExternalLeaseIdentity(fmt.Sprintf("%s lease %d", request.Operation, index+1), lease); err != nil {
					return protocolResponse{}, err
				}
			}
		}
		response := protocolResponse{ProtocolVersion: protocolVersion, Leases: leases}
		if err := b.validateProviderSSHOutput(request, response); err != nil {
			return protocolResponse{}, err
		}
		return response, nil
	default:
		return protocolResponse{}, core.Exit(2, "external lifecycle %s has unsupported output %q", request.Operation, operation.Output)
	}
}

func (b *leaseBackend) validateProviderSSHOutput(request protocolRequest, response protocolResponse) error {
	if request.SkipSSHOutputValidation ||
		(request.Operation != "acquire" && request.Operation != "list" &&
			(request.Operation != "resolve" || request.ReleaseOnly)) {
		return nil
	}
	if response.Lease != nil && response.Lease.SSH != nil {
		if err := core.ValidateExternalProviderSSHOutput(b.cfg); err != nil {
			return err
		}
	}
	for index := range response.Leases {
		if response.Leases[index].SSH == nil {
			continue
		}
		if err := core.ValidateExternalProviderSSHOutput(b.cfg); err != nil {
			return err
		}
	}
	return nil
}

func validateRawExternalLeaseIdentity(operation string, lease protocolLease) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"leaseId", lease.LeaseID},
		{"slug", lease.Slug},
		{"name", lease.Name},
		{"cloudId", lease.CloudID},
	} {
		if strings.TrimSpace(field.value) == "" {
			return core.Exit(5, "external lifecycle %s JSON lease is missing raw %s", operation, field.name)
		}
		if field.value != strings.TrimSpace(field.value) || !validRawLifecycleIdentityText(field.value) {
			return core.Exit(5, "external lifecycle %s JSON lease has invalid raw %s", operation, field.name)
		}
	}
	if !core.IsCanonicalLeaseID(lease.LeaseID) {
		return core.Exit(5, "external lifecycle %s JSON lease has invalid raw leaseId", operation)
	}
	if core.NormalizeLeaseSlug(lease.Slug) != lease.Slug {
		return core.Exit(5, "external lifecycle %s JSON lease has invalid raw slug", operation)
	}
	for _, key := range []string{"lease", "slug", "name", externalResourceNameLabel, externalResourceNameFromEnv} {
		if _, ok := lease.Labels[key]; ok {
			return core.Exit(5, "external lifecycle %s JSON lease sets reserved routing label %q", operation, key)
		}
	}
	return nil
}

func validRawLifecycleIdentityText(value string) bool {
	if value == "" || len(value) > 4096 {
		return false
	}
	for _, char := range value {
		if char < 32 || char == 127 {
			return false
		}
	}
	return true
}

func lifecycleOperationConfigured(operation core.ExternalLifecycleOperation) bool {
	return len(operation.Argv) > 0 || len(operation.Steps) > 0
}

func lifecycleOperationCommands(operation core.ExternalLifecycleOperation) [][]string {
	if len(operation.Steps) > 0 {
		return operation.Steps
	}
	if len(operation.Argv) > 0 {
		return [][]string{operation.Argv}
	}
	return nil
}

func lifecycleOperationConsumesRawCloudID(operation core.ExternalLifecycleOperation) bool {
	commands := lifecycleOperationCommands(operation)
	if len(commands) == 0 {
		return false
	}
	for _, command := range commands {
		consumes := false
		for _, arg := range command {
			if arg == "{{cloudId}}" {
				consumes = true
				break
			}
		}
		if !consumes {
			return false
		}
	}
	return true
}

func (b *leaseBackend) rollbackLifecycleAcquire(ctx context.Context, request protocolRequest) error {
	lease, err := b.lifecycleLease(request)
	if err != nil {
		return err
	}
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lifecycleRollbackTimeout)
	defer cancel()
	_, err = b.invokeLifecycle(rollbackCtx, protocolRequest{
		Operation: "release",
		Lease:     &lease,
		Force:     true,
	})
	return err
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
		if request.Operation == "resolve" && request.ReleaseOnly &&
			!lifecycleOperationConfigured(b.cfg.External.Lifecycle.Resolve) {
			lease := lifecycleMinimalLease(request)
			response.Lease = &lease
			response.SynthesizedIdentity = true
			break
		}
		lease, err := b.lifecycleLease(request)
		if err != nil {
			return protocolResponse{}, err
		}
		response.Lease = &lease
		response.SynthesizedIdentity = true
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
	lease := protocolLease{}
	if request.Lease != nil {
		lease = *request.Lease
	}
	lease.LeaseID = desired.LeaseID
	lease.Slug = desired.Slug
	lease.Name = desired.Name
	lease.Status = "ready"
	return lease
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
	labels := map[string]string(nil)
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return protocolLease{}, err
	}
	for _, claim := range claims {
		if !b.claimMatchesScopeOrRouting(claim, b.claimScope()) {
			continue
		}
		if strings.TrimSpace(claim.Labels["name"]) == name ||
			strings.TrimSpace(claim.Labels[externalResourceNameLabel]) == name {
			desired.LeaseID = claim.LeaseID
			desired.Slug = claim.Slug
			desired.Name = strings.TrimSpace(claim.Labels["name"])
			resourceName = name
			labels = claim.Labels
			break
		}
	}
	return b.lifecycleLeaseForRequest(protocolRequest{Desired: &desired, Lease: &protocolLease{Labels: labels}}, resourceName)
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
	if templateCtx.sensitive["resourceName"] {
		labels[externalResourceNameFromEnv] = "true"
	}
	ssh, err := lifecycleSSHForTarget(connection.SSH, templateCtx, b.cfg.TargetOS, b.cfg.WindowsMode)
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
	return lifecycleSSHForTarget(cfg, templateCtx, core.TargetLinux, core.WindowsModeNormal)
}

func lifecycleSSHForTarget(cfg core.ExternalSSHConnectionConfig, templateCtx lifecycleTemplateContext, targetOS, windowsMode string) (protocolSSH, error) {
	expand := func(label, value string) (string, error) {
		expansion, err := expandLifecycleValueTracked(value, templateCtx)
		if err != nil {
			return "", core.Exit(2, "external connection ssh.%s: %v", label, err)
		}
		// Resource-name provenance survives claims, so indirect placeholders are
		// subject to the same narrow opt-in as direct environment expansion.
		if expansion.sensitive && !cfg.AllowEnv {
			return "", core.Exit(2, "external connection ssh.%s uses an environment-derived value; set external.connection.ssh.allowEnv in trusted user config only for non-secret values", label)
		}
		return expansion.value, nil
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
	readyCheck, err := expand("readyCheck", core.Blank(cfg.ReadyCheck, externalDefaultReadyCheckForTarget(targetOS, windowsMode)))
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
	return lifecycleTemplateContext{values: values, sensitive: map[string]bool{}}, nil
}

func (b *leaseBackend) lifecycleContext(request protocolRequest, resourceName string) (lifecycleTemplateContext, error) {
	templateCtx, err := lifecycleContext(request, b.cfg.External.Config)
	if err != nil {
		return lifecycleTemplateContext{}, err
	}
	if request.Lease != nil && strings.EqualFold(strings.TrimSpace(request.Lease.Labels[externalResourceNameFromEnv]), "true") {
		templateCtx.sensitive["resourceName"] = true
	}
	if request.Lease != nil && lifecycleTemplateReferencesEnv(core.Blank(b.cfg.External.Connection.ResourceName, "{{name}}")) {
		templateCtx.sensitive["resourceName"] = true
	}
	if resourceName == "" && request.Lease != nil {
		resourceName = strings.TrimSpace(request.Lease.Labels[externalResourceNameLabel])
		if resourceName == "" {
			resourceName = strings.TrimSpace(request.Lease.Name)
		}
	}
	if resourceName == "" {
		expansion, err := expandLifecycleValueTracked(
			core.Blank(b.cfg.External.Connection.ResourceName, "{{name}}"),
			templateCtx,
		)
		if err != nil {
			return lifecycleTemplateContext{}, core.Exit(2, "external connection resourceName: %v", err)
		}
		if expansion.sensitive && !b.cfg.External.Connection.AllowEnvResourceName {
			return lifecycleTemplateContext{}, core.Exit(2, "external connection resourceName uses environment-derived value; set allowEnvResourceName only for non-secret durable identifiers")
		}
		resourceName = expansion.value
		if expansion.sensitive {
			templateCtx.sensitive["resourceName"] = true
		}
	}
	templateCtx.values["resourceName"] = resourceName
	return templateCtx, nil
}

func lifecycleTemplateReferencesEnv(value string) bool {
	matches := lifecyclePlaceholderPattern.FindAllStringSubmatch(value, -1)
	for _, match := range matches {
		if len(match) > 1 && strings.HasPrefix(match[1], "env.") {
			return true
		}
	}
	return false
}

func rejectLifecycleDesktopPasswordEnvReferences(env map[string]string, passwordEnv string) error {
	passwordEnv = strings.TrimSpace(passwordEnv)
	if passwordEnv == "" {
		return nil
	}
	keys := make([]string, 0, len(env))
	for name := range env {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		if lifecycleValueReferencesEnvironment(env[name], passwordEnv) {
			return fmt.Errorf("%s references configured desktop password environment %s", name, passwordEnv)
		}
	}
	return nil
}

func rejectLifecycleDesktopPasswordArgvReferences(commands [][]string, passwordEnv string) error {
	passwordEnv = strings.TrimSpace(passwordEnv)
	if passwordEnv == "" {
		return nil
	}
	for stepIndex, command := range commands {
		for argIndex, value := range command {
			if lifecycleValueReferencesEnvironment(value, passwordEnv) {
				return fmt.Errorf("step %d argv[%d] references configured desktop password environment %s", stepIndex+1, argIndex, passwordEnv)
			}
		}
	}
	return nil
}

func rejectLifecycleDesktopPasswordNamePrefixReference(namePrefix, passwordEnv string) error {
	passwordEnv = strings.TrimSpace(passwordEnv)
	if passwordEnv != "" && lifecycleValueReferencesEnvironment(namePrefix, passwordEnv) {
		return fmt.Errorf("namePrefix references configured desktop password environment %s", passwordEnv)
	}
	return nil
}

func rejectLifecycleDesktopPasswordConnectionReferences(connection core.ExternalConnectionConfig, passwordEnv string) error {
	passwordEnv = strings.TrimSpace(passwordEnv)
	if passwordEnv == "" {
		return nil
	}
	values := []struct {
		name  string
		value string
	}{
		{name: "resourceName", value: connection.ResourceName},
		{name: "cloudId", value: connection.CloudID},
		{name: "serverType", value: connection.ServerType},
		{name: "ssh.user", value: connection.SSH.User},
		{name: "ssh.host", value: connection.SSH.Host},
		{name: "ssh.key", value: connection.SSH.Key},
		{name: "ssh.port", value: connection.SSH.Port},
		{name: "ssh.readyCheck", value: connection.SSH.ReadyCheck},
		{name: "ssh.proxyCommand", value: connection.SSH.ProxyCommand},
	}
	labelKeys := make([]string, 0, len(connection.Labels))
	for name := range connection.Labels {
		labelKeys = append(labelKeys, name)
	}
	sort.Strings(labelKeys)
	for _, name := range labelKeys {
		values = append(values, struct {
			name  string
			value string
		}{name: "labels." + name, value: connection.Labels[name]})
	}
	for index, value := range connection.SSH.FallbackPorts {
		values = append(values, struct {
			name  string
			value string
		}{name: fmt.Sprintf("ssh.fallbackPorts[%d]", index), value: value})
	}
	for _, field := range values {
		if lifecycleValueReferencesEnvironment(field.value, passwordEnv) {
			return fmt.Errorf("external connection %s references configured desktop password environment %s", field.name, passwordEnv)
		}
	}
	return nil
}

func lifecycleValueReferencesEnvironment(value, environment string) bool {
	for _, match := range lifecyclePlaceholderPattern.FindAllStringSubmatch(value, -1) {
		if len(match) < 2 || !strings.HasPrefix(match[1], "env.") {
			continue
		}
		if strings.EqualFold(strings.TrimPrefix(match[1], "env."), environment) {
			return true
		}
	}
	return false
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

func expandLifecycleArgv(argv []string, templateCtx lifecycleTemplateContext, allowEnvArgv bool) ([]string, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("argv is empty")
	}
	expanded := make([]string, len(argv))
	for index, value := range argv {
		item, err := expandLifecycleValueTracked(value, templateCtx)
		if err != nil {
			return nil, fmt.Errorf("argv[%d]: %w", index, err)
		}
		if item.sensitive && !allowEnvArgv {
			return nil, fmt.Errorf("argv[%d] uses environment-derived value; use lifecycle env for secrets or set allowEnvArgv only for non-secret arguments", index)
		}
		if strings.ContainsRune(item.value, '\x00') {
			return nil, fmt.Errorf("argv[%d] contains a NUL byte", index)
		}
		expanded[index] = item.value
	}
	if strings.TrimSpace(expanded[0]) == "" {
		return nil, fmt.Errorf("argv executable is empty")
	}
	return expanded, nil
}

func expandLifecycleValue(value string, templateCtx lifecycleTemplateContext) (string, error) {
	expansion, err := expandLifecycleValueTracked(value, templateCtx)
	if err != nil {
		return "", err
	}
	return expansion.value, nil
}

type lifecycleExpansion struct {
	value     string
	sensitive bool
}

func expandLifecycleValueTracked(value string, templateCtx lifecycleTemplateContext) (lifecycleExpansion, error) {
	var expansionErr error
	sensitive := false
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
			sensitive = true
			return envValue
		}
		replacement, ok := templateCtx.values[key]
		if !ok {
			expansionErr = fmt.Errorf("unknown placeholder %q", key)
			return ""
		}
		if templateCtx.sensitive[key] {
			sensitive = true
		}
		return replacement
	})
	if expansionErr != nil {
		return lifecycleExpansion{}, expansionErr
	}
	if strings.Contains(expanded, "{{") || strings.Contains(expanded, "}}") {
		return lifecycleExpansion{}, fmt.Errorf("invalid placeholder syntax in %q", value)
	}
	return lifecycleExpansion{value: expanded, sensitive: sensitive}, nil
}

func expandLifecycleEnv(env map[string]string, templateCtx lifecycleTemplateContext) ([]string, error) {
	if len(env) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(env))
	for name := range env {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	expanded := append([]string(nil), os.Environ()...)
	for _, name := range keys {
		if !lifecycleEnvNamePattern.MatchString(name) {
			return nil, fmt.Errorf("invalid environment variable name %q", name)
		}
		value, err := expandLifecycleValue(env[name], templateCtx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("%s contains a NUL byte", name)
		}
		expanded = append(expanded, name+"="+value)
	}
	return expanded, nil
}
