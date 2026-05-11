package cli

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"
	"strings"
	"time"

	gcpcompute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"
)

const (
	defaultGCPLinuxImage  = "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64"
	gcpFirewallName       = "crabbox-ssh"
	gcpCloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
)

type GCPClient struct {
	Project        string
	Zone           string
	Zones          []string
	Image          string
	Network        string
	Subnet         string
	Tags           []string
	SSHCIDRs       []string
	RootGB         int64
	ServiceAccount string
	SSHPort        string
	FallbackPorts  []string

	instances *gcpcompute.InstancesClient
	firewalls *gcpcompute.FirewallsClient
}

func NewGCPClient(ctx context.Context, cfg Config) (*GCPClient, error) {
	return newGCPClientWithOptions(ctx, cfg)
}

func newGCPClientWithOptions(ctx context.Context, cfg Config, opts ...option.ClientOption) (*GCPClient, error) {
	if cfg.GCPProject == "" {
		return nil, exit(3, "gcp project is required (set gcp.project, CRABBOX_GCP_PROJECT, GOOGLE_CLOUD_PROJECT, or GCP_PROJECT_ID)")
	}
	if cfg.GCPZone == "" {
		return nil, exit(3, "gcp zone is required (set gcp.zone or CRABBOX_GCP_ZONE)")
	}
	instances, err := gcpcompute.NewInstancesRESTClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("gcp instances client: %w", err)
	}
	firewalls, err := gcpcompute.NewFirewallsRESTClient(ctx, opts...)
	if err != nil {
		_ = instances.Close()
		return nil, fmt.Errorf("gcp firewalls client: %w", err)
	}
	cidrs := cfg.GCPSSHCIDRs
	if len(cidrs) == 0 {
		cidrs = []string{"0.0.0.0/0"}
	}
	tags := uniqueStrings(cfg.GCPTags)
	if len(tags) == 0 {
		tags = []string{"crabbox-ssh"}
	}
	return &GCPClient{
		Project:        cfg.GCPProject,
		Zone:           cfg.GCPZone,
		Zones:          uniqueStrings(append([]string{cfg.GCPZone}, cfg.Capacity.AvailabilityZones...)),
		Image:          blank(cfg.GCPImage, defaultGCPLinuxImage),
		Network:        blank(cfg.GCPNetwork, "default"),
		Subnet:         cfg.GCPSubnet,
		Tags:           tags,
		SSHCIDRs:       cidrs,
		RootGB:         cfg.GCPRootGB,
		ServiceAccount: cfg.GCPServiceAccount,
		SSHPort:        cfg.SSHPort,
		FallbackPorts:  cfg.SSHFallbackPorts,
		instances:      instances,
		firewalls:      firewalls,
	}, nil
}

func gcpMachineTypeCandidatesForClass(class string) []string {
	switch class {
	case "standard":
		return []string{"c4-standard-32", "c3-standard-22", "n2-standard-32", "n2d-standard-32"}
	case "fast":
		return []string{"c4-standard-64", "c3-standard-44", "n2-standard-64", "n2d-standard-64", "c4-standard-32"}
	case "large":
		return []string{"c4-standard-96", "c3-standard-88", "n2-standard-80", "n2d-standard-96", "c4-standard-64"}
	case "beast":
		return []string{"c4-standard-192", "c4-standard-96", "c3-standard-176", "c3-standard-88", "n2d-standard-224", "n2-standard-128"}
	default:
		return []string{class}
	}
}

func (c *GCPClient) CreateServerWithFallback(ctx context.Context, cfg Config, publicKey, leaseID, slug string, keep bool, logf func(string, ...any)) (Server, Config, error) {
	var candidates []string
	if cfg.ServerTypeExplicit && cfg.ServerType != "" {
		candidates = []string{cfg.ServerType}
	} else {
		candidates = gcpMachineTypeCandidatesForClass(cfg.Class)
		if cfg.ServerType != "" && cfg.ServerType != candidates[0] {
			candidates = append([]string{cfg.ServerType}, candidates...)
		}
	}
	zones := uniqueStrings(append([]string{cfg.GCPZone}, cfg.Capacity.AvailabilityZones...))
	var errs []error
	for _, zone := range zones {
		for i, machineType := range candidates {
			next := cfg
			next.GCPZone = zone
			next.ServerType = machineType
			if (i > 0 || zone != cfg.GCPZone) && logf != nil {
				logf("fallback provisioning zone=%s type=%s after fallback-eligible provisioning error\n", zone, machineType)
			}
			server, err := c.withZone(zone).createServer(ctx, next, publicKey, leaseID, slug, keep)
			if err == nil {
				return server, next, nil
			}
			errs = append(errs, fmt.Errorf("%s/%s: %w", zone, machineType, err))
			if !isGCPFallbackProvisioningError(err) {
				return Server{}, next, joinErrors(errs)
			}
		}
	}
	if strings.EqualFold(cfg.Capacity.Market, "spot") && strings.HasPrefix(cfg.Capacity.Fallback, "on-demand") {
		for _, zone := range zones {
			for _, machineType := range candidates {
				next := cfg
				next.GCPZone = zone
				next.ServerType = machineType
				next.Capacity.Market = "on-demand"
				if logf != nil {
					logf("fallback provisioning zone=%s type=%s market=on-demand after spot rejection\n", zone, machineType)
				}
				server, err := c.withZone(zone).createServer(ctx, next, publicKey, leaseID, slug, keep)
				if err == nil {
					return server, next, nil
				}
				errs = append(errs, fmt.Errorf("on-demand %s/%s: %w", zone, machineType, err))
				if !isGCPFallbackProvisioningError(err) {
					return Server{}, next, joinErrors(errs)
				}
			}
		}
	}
	return Server{}, cfg, joinErrors(errs)
}

func (c *GCPClient) withZone(zone string) *GCPClient {
	next := *c
	next.Zone = zone
	return &next
}

func (c *GCPClient) createServer(ctx context.Context, cfg Config, publicKey, leaseID, slug string, keep bool) (server Server, err error) {
	if cfg.TargetOS != targetLinux {
		return Server{}, exit(2, "gcp provider currently supports target=linux only")
	}
	if err := c.EnsureFirewall(ctx); err != nil {
		return Server{}, err
	}
	name := leaseProviderName(leaseID, slug)
	defer func() {
		if err != nil {
			_ = c.DeleteServer(context.Background(), name)
		}
	}()
	labels := directLeaseLabels(cfg, leaseID, slug, "gcp", mapMarket(strings.EqualFold(cfg.Capacity.Market, "spot")), keep, time.Now().UTC())
	metadata := &computepb.Metadata{Items: []*computepb.Items{
		gcpMetadataItem("enable-oslogin", "FALSE"),
		gcpMetadataItem("ssh-keys", fmt.Sprintf("%s:%s", cfg.SSHUser, publicKey)),
		gcpMetadataItem("user-data", cloudInit(cfg, publicKey)),
	}}
	instance := &computepb.Instance{
		Name:        proto.String(name),
		Labels:      gcpLabels(labels),
		MachineType: proto.String(fmt.Sprintf("zones/%s/machineTypes/%s", c.Zone, cfg.ServerType)),
		Tags:        &computepb.Tags{Items: c.Tags},
		Metadata:    metadata,
		Disks: []*computepb.AttachedDisk{{
			Boot:       proto.Bool(true),
			AutoDelete: proto.Bool(true),
			Type:       proto.String("PERSISTENT"),
			InitializeParams: &computepb.AttachedDiskInitializeParams{
				SourceImage: proto.String(c.Image),
				DiskSizeGb:  proto.Int64(c.RootGB),
				DiskType:    proto.String(fmt.Sprintf("zones/%s/diskTypes/pd-balanced", c.Zone)),
			},
		}},
		NetworkInterfaces: []*computepb.NetworkInterface{{
			Network:       proto.String(c.networkSelfLink()),
			Subnetwork:    optionalString(c.subnetworkSelfLink()),
			AccessConfigs: []*computepb.AccessConfig{{Name: proto.String("External NAT"), Type: proto.String("ONE_TO_ONE_NAT")}},
		}},
	}
	if c.ServiceAccount != "" {
		instance.ServiceAccounts = []*computepb.ServiceAccount{{
			Email:  proto.String(c.ServiceAccount),
			Scopes: []string{gcpCloudPlatformScope},
		}}
	}
	if strings.EqualFold(cfg.Capacity.Market, "spot") {
		instance.Scheduling = &computepb.Scheduling{
			ProvisioningModel:         proto.String("SPOT"),
			InstanceTerminationAction: proto.String("DELETE"),
			AutomaticRestart:          proto.Bool(false),
			OnHostMaintenance:         proto.String("TERMINATE"),
		}
	}
	op, err := c.instances.Insert(ctx, &computepb.InsertInstanceRequest{
		Project:          c.Project,
		Zone:             c.Zone,
		InstanceResource: instance,
	})
	if err != nil {
		return Server{}, fmt.Errorf("create gcp instance: %w", err)
	}
	if err := waitGCPOperation(ctx, op); err != nil {
		return Server{}, err
	}
	return c.GetServer(ctx, name)
}

func gcpMetadataItem(key, value string) *computepb.Items {
	return &computepb.Items{Key: proto.String(key), Value: proto.String(value)}
}

func (c *GCPClient) EnsureFirewall(ctx context.Context) error {
	ports := sshPortCandidates(c.SSHPort, c.FallbackPorts)
	name := gcpFirewallNameForPolicy(c.Network, c.SSHCIDRs, c.Tags, ports)
	firewall := &computepb.Firewall{
		Name:         proto.String(name),
		Description:  proto.String("Crabbox-managed SSH ingress"),
		Network:      proto.String(c.networkSelfLink()),
		Direction:    proto.String("INGRESS"),
		SourceRanges: c.SSHCIDRs,
		TargetTags:   c.Tags,
		Allowed:      []*computepb.Allowed{{IPProtocol: proto.String("tcp"), Ports: ports}},
	}
	existing, err := c.firewalls.Get(ctx, &computepb.GetFirewallRequest{Project: c.Project, Firewall: name})
	if err == nil {
		if !strings.Contains(existing.GetDescription(), "Crabbox-managed") {
			return fmt.Errorf("gcp firewall %q exists but is not Crabbox-managed; use a different CRABBOX_GCP_TAGS value or update the firewall manually", name)
		}
		op, err := c.firewalls.Update(ctx, &computepb.UpdateFirewallRequest{
			Project:          c.Project,
			Firewall:         name,
			FirewallResource: firewall,
		})
		if err != nil {
			return fmt.Errorf("update gcp firewall: %w", err)
		}
		return waitGCPOperation(ctx, op)
	}
	if !isGCPNotFound(err) {
		return fmt.Errorf("get gcp firewall: %w", err)
	}
	op, err := c.firewalls.Insert(ctx, &computepb.InsertFirewallRequest{
		Project:          c.Project,
		FirewallResource: firewall,
	})
	if err != nil {
		return fmt.Errorf("create gcp firewall: %w", err)
	}
	return waitGCPOperation(ctx, op)
}

func (c *GCPClient) GetServer(ctx context.Context, name string) (Server, error) {
	instance, err := c.instances.Get(ctx, &computepb.GetInstanceRequest{
		Project:  c.Project,
		Zone:     c.Zone,
		Instance: name,
	})
	if err != nil {
		return Server{}, err
	}
	return gcpInstanceToServer(c.Zone, instance), nil
}

func (c *GCPClient) WaitForServerIP(ctx context.Context, name string) (Server, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		server, err := c.GetServer(ctx, name)
		if err != nil {
			return Server{}, err
		}
		if server.PublicNet.IPv4.IP != "" {
			return server, nil
		}
		if time.Now().After(deadline) {
			return Server{}, fmt.Errorf("timeout waiting for gcp public ip on %s", name)
		}
		select {
		case <-ctx.Done():
			return Server{}, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *GCPClient) ListCrabboxServers(ctx context.Context) ([]Server, error) {
	var servers []Server
	it := c.instances.AggregatedList(ctx, &computepb.AggregatedListInstancesRequest{
		Project:              c.Project,
		Filter:               proto.String("labels.crabbox = true"),
		ReturnPartialSuccess: proto.Bool(true),
	})
	for {
		item, err := it.Next()
		if err == iterator.Done {
			break
		}
		if isGCPNotFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		zone := gcpResourceName(item.Key)
		for _, instance := range item.Value.GetInstances() {
			if instance == nil {
				continue
			}
			instanceZone := zone
			if instance.GetZone() != "" {
				instanceZone = gcpResourceName(instance.GetZone())
			}
			if instanceZone == "" {
				instanceZone = c.Zone
			}
			servers = append(servers, gcpInstanceToServer(instanceZone, instance))
		}
	}
	sort.Slice(servers, func(i, j int) bool {
		if servers[i].Labels["zone"] != servers[j].Labels["zone"] {
			return servers[i].Labels["zone"] < servers[j].Labels["zone"]
		}
		return servers[i].Name < servers[j].Name
	})
	return servers, nil
}

func (c *GCPClient) DeleteServer(ctx context.Context, name string) error {
	op, err := c.instances.Delete(ctx, &computepb.DeleteInstanceRequest{
		Project:  c.Project,
		Zone:     c.Zone,
		Instance: name,
	})
	if isGCPNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete gcp instance: %w", err)
	}
	return waitGCPOperation(ctx, op)
}

func (c *GCPClient) SetLabels(ctx context.Context, name string, labels map[string]string) error {
	instance, err := c.instances.Get(ctx, &computepb.GetInstanceRequest{
		Project:  c.Project,
		Zone:     c.Zone,
		Instance: name,
	})
	if err != nil {
		return err
	}
	op, err := c.instances.SetLabels(ctx, &computepb.SetLabelsInstanceRequest{
		Project:  c.Project,
		Zone:     c.Zone,
		Instance: name,
		InstancesSetLabelsRequestResource: &computepb.InstancesSetLabelsRequest{
			LabelFingerprint: proto.String(instance.GetLabelFingerprint()),
			Labels:           gcpLabels(labels),
		},
	})
	if err != nil {
		return fmt.Errorf("set gcp labels: %w", err)
	}
	return waitGCPOperation(ctx, op)
}

func waitGCPOperation(ctx context.Context, op *gcpcompute.Operation) error {
	if op == nil {
		return nil
	}
	return op.Wait(ctx)
}

func (c *GCPClient) networkSelfLink() string {
	if strings.Contains(c.Network, "/") {
		return c.Network
	}
	return fmt.Sprintf("projects/%s/global/networks/%s", c.Project, c.Network)
}

func (c *GCPClient) subnetworkSelfLink() string {
	if c.Subnet == "" {
		return ""
	}
	if strings.Contains(c.Subnet, "/") {
		return c.Subnet
	}
	return fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", c.Project, gcpRegionFromZone(c.Zone), c.Subnet)
}

func gcpFirewallNameForNetwork(network string) string {
	name := network
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" || name == "default" {
		return gcpFirewallName
	}
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	suffix := strings.Trim(b.String(), "-")
	for strings.Contains(suffix, "--") {
		suffix = strings.ReplaceAll(suffix, "--", "-")
	}
	if suffix == "" {
		suffix = "custom"
	} else if suffix[0] < 'a' || suffix[0] > 'z' {
		suffix = "net-" + suffix
	}
	prefix := gcpFirewallName + "-"
	if len(prefix)+len(suffix) > 63 {
		suffix = strings.TrimRight(suffix[:63-len(prefix)], "-")
	}
	if suffix == "" {
		suffix = "custom"
	}
	return prefix + suffix
}

func gcpFirewallNameForPolicy(network string, sourceRanges, targetTags, ports []string) string {
	base := gcpFirewallNameForNetwork(network)
	if canonicalGCPFirewallPolicyPart(sourceRanges) == "0.0.0.0/0" &&
		canonicalGCPFirewallPolicyPart(targetTags) == "crabbox-ssh" &&
		canonicalGCPFirewallPolicyPart(ports) == "22,2222" {
		return base
	}
	return gcpFirewallNameWithSuffix(base, fnv32Hex(strings.Join([]string{
		canonicalGCPFirewallPolicyPart(sourceRanges),
		canonicalGCPFirewallPolicyPart(targetTags),
		canonicalGCPFirewallPolicyPart(ports),
	}, "|")))
}

func gcpFirewallNameWithSuffix(base, suffix string) string {
	maxBaseLen := 63 - len(suffix) - 1
	if len(base) > maxBaseLen {
		base = strings.TrimRight(base[:maxBaseLen], "-")
	}
	if base == "" {
		base = gcpFirewallName
	}
	return base + "-" + suffix
}

func canonicalGCPFirewallPolicyPart(values []string) string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return strings.Join(out, ",")
}

func fnv32Hex(value string) string {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(value))
	return fmt.Sprintf("%08x", hash.Sum32())
}

func gcpRegionFromZone(zone string) string {
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return zone
}

func gcpResourceName(value string) string {
	value = strings.TrimSpace(strings.TrimSuffix(value, "/"))
	if i := strings.LastIndex(value, "/"); i >= 0 {
		return value[i+1:]
	}
	return value
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return proto.String(value)
}

func gcpInstanceToServer(zone string, instance *computepb.Instance) Server {
	s := Server{Provider: "gcp", Labels: map[string]string{}}
	if instance == nil {
		return s
	}
	s.CloudID = instance.GetName()
	s.Name = instance.GetName()
	s.Status = instance.GetStatus()
	if instance.GetId() != 0 {
		s.ID = int64(instance.GetId())
	}
	if instance.Labels != nil {
		s.Labels = instance.Labels
	}
	s.Labels["zone"] = zone
	if instance.GetMachineType() != "" {
		s.ServerType.Name = gcpResourceName(instance.GetMachineType())
	}
	for _, ni := range instance.GetNetworkInterfaces() {
		for _, ac := range ni.GetAccessConfigs() {
			if ac.GetNatIP() != "" {
				s.PublicNet.IPv4.IP = ac.GetNatIP()
				return s
			}
		}
	}
	return s
}

func gcpLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		out[gcpLabelKey(key)] = gcpLabelValue(value)
	}
	return out
}

func gcpLabelKey(value string) string {
	out := gcpLabelValue(value)
	if out[0] < 'a' || out[0] > 'z' {
		out = "x" + out
		if len(out) > 63 {
			out = out[:63]
		}
	}
	return out
}

func gcpLabelValue(value string) string {
	value = strings.ToLower(sanitizeProviderLabelValue(value))
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
		if b.Len() >= 63 {
			break
		}
	}
	out := strings.Trim(b.String(), "_-")
	if out == "" {
		return "unknown"
	}
	return out
}

func isGCPNotFound(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound
}

func IsGCPNotFound(err error) bool {
	return isGCPNotFound(err)
}

func isGCPFallbackProvisioningError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		if apiErr.Code == http.StatusForbidden {
			return isGCPQuotaOrCapacityError(apiErr)
		}
		if apiErr.Code == http.StatusTooManyRequests || apiErr.Code == http.StatusConflict || apiErr.Code >= 500 {
			return true
		}
		if (apiErr.Code == http.StatusBadRequest || apiErr.Code == http.StatusNotFound) && isGCPUnavailableMachineTypeMessage(apiErr.Message) {
			return true
		}
	}
	s := strings.ToLower(err.Error())
	return isGCPQuotaOrCapacityMessage(s) ||
		isGCPUnavailableMachineTypeMessage(s) ||
		strings.Contains(s, "try again")
}

func isGCPQuotaOrCapacityError(apiErr *googleapi.Error) bool {
	if apiErr == nil {
		return false
	}
	var b strings.Builder
	b.WriteString(apiErr.Message)
	b.WriteByte(' ')
	b.WriteString(apiErr.Body)
	for _, item := range apiErr.Errors {
		b.WriteByte(' ')
		b.WriteString(item.Reason)
		b.WriteByte(' ')
		b.WriteString(item.Message)
	}
	return isGCPQuotaOrCapacityMessage(b.String())
}

func isGCPQuotaOrCapacityMessage(message string) bool {
	s := strings.ToLower(message)
	return strings.Contains(s, "quotaexceeded") ||
		strings.Contains(s, "quota exceeded") ||
		strings.Contains(s, "exceeded quota") ||
		strings.Contains(s, "quota '") ||
		strings.Contains(s, "rate limit") ||
		strings.Contains(s, "ratelimit") ||
		strings.Contains(s, "resource_pool_exhausted") ||
		strings.Contains(s, "does not have enough resources") ||
		strings.Contains(s, "capacity")
}

func isGCPUnavailableMachineTypeMessage(message string) bool {
	s := strings.ToLower(message)
	return strings.Contains(s, "/machinetypes/") ||
		strings.Contains(s, "resource.machinetype") ||
		(strings.Contains(s, "machine type") &&
			(strings.Contains(s, "does not exist") ||
				strings.Contains(s, "not found") ||
				strings.Contains(s, "invalid value")))
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
