package nebius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type cliRunner struct {
	cfg NebiusConfig
	rt  Runtime
}

type cliResult struct {
	Stdout string
	Stderr string
}

var tokenLikePattern = regexp.MustCompile(`(?i)(osb_|ncp_|iam_token|oauth[_-]?token|access[_-]?token|refresh[_-]?token|private[_-]?key|BEGIN [A-Z ]*PRIVATE KEY)[A-Za-z0-9._:/+=@ -]*`)

type nebiusAPI interface {
	ListInstances(context.Context) ([]nebiusInstance, error)
	GetInstance(context.Context, string) (nebiusInstance, error)
	CreateInstance(context.Context, nebiusCreateRequest) (nebiusInstance, error)
	WaitInstance(context.Context, string) (nebiusInstance, error)
	UpdateLabels(context.Context, string, map[string]string) error
	DeleteInstance(context.Context, string) error
}

type nebiusClient struct {
	cfg NebiusConfig
	cli cliRunner
}

type nebiusCreateRequest struct {
	Name      string
	Labels    map[string]string
	UserData  string
	PublicKey string
}

type nebiusInstance struct {
	ID       string
	Name     string
	Status   string
	Labels   map[string]string
	PublicIP string
	Raw      map[string]any
}

func newCLIRunner(cfg NebiusConfig, rt Runtime) cliRunner {
	return cliRunner{cfg: cfg, rt: rt}
}

func newNebiusClient(cfg NebiusConfig, rt Runtime) *nebiusClient {
	return &nebiusClient{cfg: cfg, cli: newCLIRunner(cfg, rt)}
}

func (c cliRunner) run(ctx context.Context, args ...string) (cliResult, error) {
	commandArgs := c.withProfile(args)
	result, err := c.rt.Exec.Run(ctx, LocalCommandRequest{
		Name: c.cfg.CLI,
		Args: commandArgs,
	})
	if err != nil {
		return cliResult{Stdout: result.Stdout, Stderr: result.Stderr}, fmt.Errorf("nebius cli %s failed: %s", strings.Join(redactNebiusArgs(commandArgs), " "), redactNebiusText(firstNonBlank(result.Stderr, err.Error())))
	}
	return cliResult{Stdout: result.Stdout, Stderr: result.Stderr}, nil
}

func (c cliRunner) withProfile(args []string) []string {
	profile := strings.TrimSpace(c.cfg.Profile)
	if profile == "" {
		return append([]string(nil), args...)
	}
	return append([]string{"--profile", profile}, args...)
}

func redactNebiusArgs(args []string) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "--cloud-init-user-data" {
			out[i+1] = "[REDACTED]"
			i++
		}
	}
	return out
}

func parseJSONObject(output string) (map[string]any, error) {
	var object map[string]any
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	return object, nil
}

func parseJSONArray(output string) ([]map[string]any, error) {
	var objects []map[string]any
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.UseNumber()
	if err := decoder.Decode(&objects); err != nil {
		return nil, err
	}
	return objects, nil
}

func (c *nebiusClient) ListInstances(ctx context.Context) ([]nebiusInstance, error) {
	result, err := c.cli.run(ctx, "compute", "instance", "list", "--parent-id", c.cfg.ParentID, "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	return parseNebiusInstances(result.Stdout)
}

func (c *nebiusClient) GetInstance(ctx context.Context, id string) (nebiusInstance, error) {
	result, err := c.cli.run(ctx, "compute", "instance", "get", id, "--format", "json")
	if err != nil {
		return nebiusInstance{}, err
	}
	return parseNebiusInstance(result.Stdout)
}

func (c *nebiusClient) CreateInstance(ctx context.Context, req nebiusCreateRequest) (nebiusInstance, error) {
	args := []string{
		"compute", "instance", "create",
		"--parent-id", c.cfg.ParentID,
		"--name", req.Name,
		"--resources-platform", c.cfg.Platform,
		"--resources-preset", c.cfg.Preset,
		"--boot-disk-managed-disk-name", req.Name,
		"--boot-disk-managed-disk-source-image-family-image-family", c.cfg.ImageFamily,
		"--boot-disk-managed-disk-type", c.cfg.DiskType,
		"--boot-disk-managed-disk-size-gibibytes", strconv.Itoa(c.cfg.DiskSizeGiB),
		"--boot-disk-attach-mode", "read_write",
		"--cloud-init-user-data", req.UserData,
		"--network-interfaces", renderNetworkInterfaces(c.cfg),
		"--recovery-policy", firstNonBlank(c.cfg.RecoveryPolicy, "fail"),
	}
	if strings.TrimSpace(c.cfg.ServiceAccountID) != "" {
		args = append(args, "--service-account-id", strings.TrimSpace(c.cfg.ServiceAccountID))
	}
	if labels := renderLabels(req.Labels); labels != "" {
		args = append(args, "--labels", labels)
	}
	args = append(args, "--format", "json")
	result, err := c.cli.run(ctx, args...)
	if err != nil {
		return nebiusInstance{}, err
	}
	return parseNebiusInstance(result.Stdout)
}

func (c *nebiusClient) WaitInstance(ctx context.Context, id string) (nebiusInstance, error) {
	var last nebiusInstance
	for attempt := 0; attempt < 60; attempt++ {
		item, err := c.GetInstance(ctx, id)
		if err != nil {
			return nebiusInstance{}, err
		}
		last = item
		if item.PublicIP != "" && instanceRunning(item.Status) {
			return item, nil
		}
		select {
		case <-ctx.Done():
			return nebiusInstance{}, context.Cause(ctx)
		case <-time.After(2 * time.Second):
		}
	}
	return last, fmt.Errorf("timeout waiting for nebius instance %s public IP/readiness", id)
}

func (c *nebiusClient) UpdateLabels(ctx context.Context, id string, labels map[string]string) error {
	args := []string{"compute", "instance", "update", id, "--parent-id", c.cfg.ParentID}
	if labelsArg := renderLabels(labels); labelsArg != "" {
		args = append(args, "--labels-add", labelsArg)
	}
	args = append(args, "--format", "json")
	_, err := c.cli.run(ctx, args...)
	return err
}

func (c *nebiusClient) DeleteInstance(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return validationError("nebius instance id is required for delete")
	}
	_, err := c.cli.run(ctx, "compute", "instance", "delete", id, "--format", "json")
	return err
}

func containsIDOrName(output, want string) (bool, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return false, nil
	}
	items, err := parseJSONArray(output)
	if err != nil {
		object, objectErr := parseJSONObject(output)
		if objectErr != nil {
			return false, err
		}
		if nested, ok := arrayField(object, "items", "instances"); ok {
			items = nested
		} else {
			items = []map[string]any{object}
		}
	}
	for _, item := range items {
		if hasStringField(item, want, "id", "metadata.id", "name", "metadata.name", "family", "metadata.family", "spec.family") {
			return true, nil
		}
	}
	return false, nil
}

func hasStringField(object map[string]any, want string, paths ...string) bool {
	for _, path := range paths {
		if stringFromAny(pathValue(object, path)) == want {
			return true
		}
	}
	return false
}

func parseNebiusInstances(output string) ([]nebiusInstance, error) {
	items, err := parseJSONArray(output)
	if err != nil {
		object, objectErr := parseJSONObject(output)
		if objectErr != nil {
			return nil, err
		}
		if nested, ok := arrayField(object, "items", "instances"); ok {
			items = nested
		} else {
			items = []map[string]any{object}
		}
	}
	out := make([]nebiusInstance, 0, len(items))
	for _, item := range items {
		out = append(out, instanceFromObject(item))
	}
	return out, nil
}

func parseNebiusInstance(output string) (nebiusInstance, error) {
	object, err := parseJSONObject(output)
	if err != nil {
		items, itemsErr := parseJSONArray(output)
		if itemsErr != nil {
			return nebiusInstance{}, err
		}
		if len(items) == 0 {
			return nebiusInstance{}, errors.New("empty nebius instance response")
		}
		object = items[0]
	}
	return instanceFromObject(object), nil
}

func instanceFromObject(object map[string]any) nebiusInstance {
	labels := mapFromAny(firstPath(object, "metadata.labels", "labels", "metadataLabels"))
	return nebiusInstance{
		ID:       stringFromAny(firstPath(object, "metadata.id", "id")),
		Name:     stringFromAny(firstPath(object, "metadata.name", "name")),
		Status:   stringFromAny(firstPath(object, "status.state", "status", "state")),
		Labels:   labels,
		PublicIP: extractNebiusPublicIP(object),
		Raw:      object,
	}
}

func serverFromInstance(item nebiusInstance, cfg Config) Server {
	labels := make(map[string]string, len(item.Labels))
	for key, value := range item.Labels {
		labels[key] = value
	}
	server := Server{
		CloudID:  item.ID,
		Provider: providerName,
		Name:     firstNonBlank(item.Name, labels["slug"]),
		Status:   normalizeNebiusState(item.Status),
		Labels:   labels,
	}
	server.PublicNet.IPv4.IP = item.PublicIP
	server.ServerType.Name = nebiusServerType(cfg)
	return server
}

func extractNebiusPublicIP(object map[string]any) string {
	for _, path := range []string{
		"status.network_interfaces.0.public_ip_address.address",
		"status.networkInterfaces.0.publicIpAddress.address",
		"network_interfaces.0.public_ip_address.address",
		"networkInterfaces.0.publicIpAddress.address",
		"public_ip_address.address",
		"publicIpAddress.address",
		"public_ip",
		"publicIP",
	} {
		if ip := cleanIP(stringFromAny(pathValue(object, path))); ip != "" {
			return ip
		}
	}
	return ""
}

func cleanIP(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if ip, _, err := net.ParseCIDR(value); err == nil {
		return ip.String()
	}
	if strings.Contains(value, "/") {
		value, _, _ = strings.Cut(value, "/")
	}
	return strings.TrimSpace(value)
}

func renderNetworkInterfaces(cfg NebiusConfig) string {
	item := map[string]any{
		"name":       "eth0",
		"subnet_id":  strings.TrimSpace(cfg.SubnetID),
		"ip_address": map[string]any{},
	}
	if !strings.EqualFold(strings.TrimSpace(cfg.PublicIP), "none") {
		item["public_ip_address"] = map[string]any{}
	}
	securityGroups := make([]map[string]string, 0, len(cfg.SecurityGroupIDs))
	for _, sg := range cfg.SecurityGroupIDs {
		if sg = strings.TrimSpace(sg); sg != "" {
			securityGroups = append(securityGroups, map[string]string{"id": sg})
		}
	}
	if len(securityGroups) > 0 {
		item["security_groups"] = securityGroups
	}
	data, err := json.Marshal([]map[string]any{item})
	if err != nil {
		return "[]"
	}
	return string(data)
}

func renderLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(labels[key]) != "" {
			out = append(out, key+"="+labels[key])
		}
	}
	return strings.Join(out, ",")
}

func normalizeNebiusState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running", "ready":
		return "ready"
	case "creating", "provisioning", "starting":
		return "provisioning"
	case "stopped":
		return "stopped"
	case "deleting":
		return "deleting"
	default:
		return strings.ToLower(strings.TrimSpace(state))
	}
}

func instanceRunning(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running", "ready":
		return true
	default:
		return false
	}
}

func arrayField(object map[string]any, names ...string) ([]map[string]any, bool) {
	for _, name := range names {
		value, ok := object[name]
		if !ok {
			continue
		}
		raw, ok := value.([]any)
		if !ok {
			continue
		}
		out := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			if obj, ok := item.(map[string]any); ok {
				out = append(out, obj)
			}
		}
		return out, true
	}
	return nil, false
}

func firstPath(object map[string]any, paths ...string) any {
	for _, path := range paths {
		if value := pathValue(object, path); value != nil {
			return value
		}
	}
	return nil
}

func pathValue(object map[string]any, path string) any {
	var current any = object
	for _, part := range strings.Split(path, ".") {
		switch typed := current.(type) {
		case map[string]any:
			current = typed[part]
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(typed) {
				return nil
			}
			current = typed[index]
		default:
			return nil
		}
	}
	return current
}

func stringFromAny(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	default:
		return ""
	}
}

func mapFromAny(value any) map[string]string {
	out := map[string]string{}
	object, ok := value.(map[string]any)
	if !ok {
		return out
	}
	for key, raw := range object {
		if value := stringFromAny(raw); value != "" {
			out[key] = value
		}
	}
	return out
}

func redactNebiusText(text string) string {
	text = tokenLikePattern.ReplaceAllString(text, "[REDACTED]")
	return strings.TrimSpace(text)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func isJSON(output string) bool {
	var raw any
	return json.Unmarshal([]byte(output), &raw) == nil
}

func validationError(format string, args ...any) error {
	return exit(2, format, args...)
}
