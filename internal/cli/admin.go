package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func (a App) adminLeases(ctx context.Context, args []string) error {
	fs := newFlagSet("admin leases", a.Stderr)
	state := fs.String("state", "", "filter by state")
	owner := fs.String("owner", "", "filter by owner")
	org := fs.String("org", "", "filter by org")
	limit := fs.Int("limit", 100, "maximum leases")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	leases, err := coord.AdminLeases(ctx, *state, *owner, *org, *limit)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(leases)
	}
	for _, lease := range leases {
		fmt.Fprintf(a.Stdout, "%-16s %-16s %-8s %-10s %-14s %-24s owner=%s org=%s idle=%s expires=%s\n",
			lease.ID, blank(lease.Slug, "-"), lease.Provider, lease.State, lease.ServerType, lease.Host, lease.Owner, lease.Org, formatSecondsDuration(lease.IdleTimeoutSeconds), blank(lease.ExpiresAt, "-"))
	}
	return nil
}

func (a App) adminLeaseAudit(ctx context.Context, args []string) error {
	fs := newFlagSet("admin lease-audit", a.Stderr)
	state := fs.String("state", "expired", "filter by state")
	provider := fs.String("provider", "aws", "filter by provider")
	owner := fs.String("owner", "", "filter by owner")
	org := fs.String("org", "", "filter by org")
	limit := fs.Int("limit", 100, "maximum leases")
	failOnLive := fs.Bool("fail-on-live", false, "exit non-zero when expired leases still have live cloud instances or audit errors")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	audits, err := coord.AdminLeaseAudit(ctx, *state, *provider, *owner, *org, *limit)
	if err != nil {
		return err
	}
	if *jsonOut {
		if err := json.NewEncoder(a.Stdout).Encode(audits); err != nil {
			return err
		}
	} else {
		for _, audit := range audits {
			fmt.Fprintf(a.Stdout, "%-16s %-16s %-8s %-8s %-14s cloud=%-7s cloud_state=%s host=%s expires=%s cleanup=%s\n",
				audit.LeaseID, blank(audit.Slug, "-"), audit.Provider, audit.State, audit.ServerType, audit.CloudStatus, blank(audit.CloudState, "-"), blank(audit.CloudHost, "-"), blank(audit.ExpiresAt, "-"), leaseAuditCleanupSummary(audit))
		}
	}
	if *failOnLive {
		for _, audit := range audits {
			if audit.CloudStatus == "found" || audit.CloudStatus == "error" {
				return exit(1, "lease audit found unreconciled cloud instances or audit errors")
			}
		}
	}
	return nil
}

func leaseAuditCleanupSummary(audit CoordinatorLeaseCloudAudit) string {
	if audit.CleanupAttempts == 0 && audit.CleanupError == "" {
		return "-"
	}
	if audit.CleanupError == "" {
		return fmt.Sprintf("attempts=%d", audit.CleanupAttempts)
	}
	return fmt.Sprintf("attempts=%d error=%s", audit.CleanupAttempts, audit.CleanupError)
}

func (a App) adminAWSIdentity(ctx context.Context, args []string) error {
	fs := newFlagSet("admin aws-identity", a.Stderr)
	region := fs.String("region", "", "AWS region used for the STS endpoint")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	identity, err := coord.AdminAWSIdentity(ctx, *region)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(identity)
	}
	fmt.Fprintf(a.Stdout, "aws identity account=%s arn=%s user_id=%s region=%s\n",
		blank(identity.Account, "-"), blank(identity.ARN, "-"), blank(identity.UserID, "-"), blank(identity.Region, "-"))
	return nil
}

const awsProviderPolicyJSON = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeImages",
        "ec2:DescribeInstances",
        "ec2:DescribeKeyPairs",
        "ec2:DescribeSecurityGroups",
        "ec2:DescribeSnapshots",
        "ec2:DescribeSubnets",
        "ec2:DescribeVpcs",
        "ec2:DescribeHosts"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "ec2:ImportKeyPair",
        "ec2:DeleteKeyPair",
        "ec2:RunInstances",
        "ec2:TerminateInstances",
        "ec2:CreateSecurityGroup",
        "ec2:AuthorizeSecurityGroupIngress",
        "ec2:RevokeSecurityGroupIngress",
        "ec2:CreateImage",
        "ec2:RegisterImage",
        "ec2:DeregisterImage",
        "ec2:CreateSnapshot",
        "ec2:DeleteSnapshot",
        "ec2:CreateTags"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": "servicequotas:GetServiceQuota",
      "Resource": "*"
    }
  ]
}`

func (a App) adminAWSPolicy(args []string) error {
	fs := newFlagSet("admin aws-policy", a.Stderr)
	includeMacHosts := fs.Bool("mac-hosts", false, "include EC2 Mac Dedicated Host lifecycle permissions")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return exit(2, "usage: crabbox admin aws-policy [--mac-hosts]")
	}
	policy := awsProviderPolicyJSON
	if *includeMacHosts {
		combined, err := combineIAMPolicyJSON(awsProviderPolicyJSON, macHostLifecyclePolicyJSON)
		if err != nil {
			return err
		}
		policy = combined
	}
	fmt.Fprintln(a.Stdout, policy)
	return nil
}

type iamPolicyDocument struct {
	Version   string            `json:"Version"`
	Statement []json.RawMessage `json:"Statement"`
}

func combineIAMPolicyJSON(policies ...string) (string, error) {
	combined := iamPolicyDocument{Version: "2012-10-17"}
	for _, policy := range policies {
		var doc iamPolicyDocument
		if err := json.Unmarshal([]byte(policy), &doc); err != nil {
			return "", err
		}
		if doc.Version != "" {
			combined.Version = doc.Version
		}
		combined.Statement = append(combined.Statement, doc.Statement...)
	}
	out, err := json.MarshalIndent(combined, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (a App) adminMacHosts(ctx context.Context, args []string) error {
	args = stripKongCommandPath(args, "admin", "mac-hosts")
	if len(args) == 0 || isHelpArg(args[0]) {
		return exit(2, "usage: crabbox admin mac-hosts <list|offerings|quota|allocate|release|policy> [flags]")
	}
	switch args[0] {
	case "list":
		return a.adminMacHostsList(ctx, args[1:])
	case "offerings":
		return a.adminMacHostOfferings(ctx, args[1:])
	case "quota":
		return a.adminMacHostQuota(ctx, args[1:])
	case "allocate":
		return a.adminMacHostsAllocate(ctx, args[1:])
	case "release":
		return a.adminMacHostsRelease(ctx, args[1:])
	case "policy":
		return a.adminMacHostsPolicy(args[1:])
	default:
		return exit(2, "usage: crabbox admin mac-hosts <list|offerings|quota|allocate|release|policy> [flags]")
	}
}

const macHostLifecyclePolicyJSON = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeHosts",
        "ec2:DescribeInstanceTypeOfferings"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "ec2:AllocateHosts",
        "ec2:ReleaseHosts"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": "ec2:CreateTags",
      "Resource": "*",
      "Condition": {
        "StringEquals": {
          "ec2:CreateAction": "AllocateHosts"
        }
      }
    },
    {
      "Effect": "Allow",
      "Action": "servicequotas:ListServiceQuotas",
      "Resource": "*"
    }
  ]
}`

func (a App) adminMacHostsPolicy(args []string) error {
	fs := newFlagSet("admin mac-hosts policy", a.Stderr)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return exit(2, "usage: crabbox admin mac-hosts policy")
	}
	fmt.Fprintln(a.Stdout, macHostLifecyclePolicyJSON)
	return nil
}

func (a App) adminMacHostsList(ctx context.Context, args []string) error {
	fs := newFlagSet("admin mac-hosts list", a.Stderr)
	region := fs.String("region", "", "AWS region")
	serverType := fs.String("type", "", "filter by EC2 Mac instance type")
	state := fs.String("state", "", "filter by host state")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	hosts, err := coord.AdminMacHosts(ctx, *region, *serverType, *state)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(hosts)
	}
	for _, host := range hosts {
		fmt.Fprintf(a.Stdout, "%-18s %-12s %-14s %-12s %-10s auto=%s allocated=%s\n",
			host.ID, host.Region, host.AvailabilityZone, host.InstanceType, host.State,
			blank(host.AutoPlacement, "-"), blank(host.AllocationTime, "-"))
	}
	return nil
}

func (a App) adminMacHostOfferings(ctx context.Context, args []string) error {
	fs := newFlagSet("admin mac-hosts offerings", a.Stderr)
	region := fs.String("region", "", "AWS region")
	serverType := fs.String("type", "mac2.metal", "EC2 Mac instance type")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	offerings, err := coord.AdminMacHostOfferings(ctx, *region, *serverType)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(offerings)
	}
	for _, offering := range offerings {
		fmt.Fprintf(a.Stdout, "%-12s %-14s %-12s\n",
			offering.Region, offering.AvailabilityZone, offering.InstanceType)
	}
	return nil
}

func (a App) adminMacHostQuota(ctx context.Context, args []string) error {
	fs := newFlagSet("admin mac-hosts quota", a.Stderr)
	region := fs.String("region", "", "AWS region")
	serverType := fs.String("type", "mac2.metal", "EC2 Mac instance type")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	quotas, err := coord.AdminMacHostQuotas(ctx, *region, *serverType)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(quotas)
	}
	if len(quotas) == 0 {
		fmt.Fprintf(a.Stdout, "no EC2 Mac Dedicated Host quota found for type=%s region=%s\n", *serverType, blank(*region, "-"))
		return nil
	}
	for _, quota := range quotas {
		fmt.Fprintf(a.Stdout, "quota region=%s type=%s code=%s name=%q value=%g adjustable=%t global=%t unit=%s\n",
			blank(*region, "-"), *serverType, quota.QuotaCode, quota.QuotaName, quota.Value, quota.Adjustable, quota.GlobalQuota, blank(quota.Unit, "-"))
	}
	return nil
}

func (a App) adminMacHostsAllocate(ctx context.Context, args []string) error {
	args, forceAnywhere := extractBoolFlag(args, "force")
	args, jsonAnywhere := extractBoolFlag(args, "json")
	args, dryRunAnywhere := extractBoolFlag(args, "dry-run")
	fs := newFlagSet("admin mac-hosts allocate", a.Stderr)
	region := fs.String("region", "", "AWS region")
	serverType := fs.String("type", "mac2.metal", "EC2 Mac instance type")
	availabilityZone := fs.String("availability-zone", "", "AWS availability zone")
	dryRun := fs.Bool("dry-run", false, "validate allocation request without allocating a host")
	force := fs.Bool("force", false, "confirm host allocation")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if forceAnywhere {
		*force = true
	}
	if jsonAnywhere {
		*jsonOut = true
	}
	if dryRunAnywhere {
		*dryRun = true
	}
	if !*dryRun && !*force {
		return exit(2, "admin mac-hosts allocate requires --force")
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	if *dryRun {
		checks, err := coord.AdminDryRunAllocateMacHost(ctx, *region, *serverType, *availabilityZone)
		if err != nil {
			return err
		}
		checks = sanitizeMacHostDryRunChecks(checks)
		if *jsonOut {
			return json.NewEncoder(a.Stdout).Encode(checks)
		}
		for _, check := range checks {
			status := "blocked"
			if check.OK {
				status = "ok"
			}
			fmt.Fprintf(a.Stdout, "dry-run %s region=%s az=%s type=%s message=%s\n",
				status, check.Region, check.AvailabilityZone, check.InstanceType, summarizeMacHostDryRunMessage(check.Message))
		}
		return nil
	}
	hosts, err := coord.AdminAllocateMacHost(ctx, *region, *serverType, *availabilityZone)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(hosts)
	}
	for _, host := range hosts {
		fmt.Fprintf(a.Stdout, "allocated host=%s region=%s az=%s type=%s state=%s\n",
			host.ID, host.Region, host.AvailabilityZone, host.InstanceType, host.State)
	}
	return nil
}

func sanitizeMacHostDryRunChecks(checks []CoordinatorMacHostAllocationDryRun) []CoordinatorMacHostAllocationDryRun {
	sanitized := make([]CoordinatorMacHostAllocationDryRun, len(checks))
	for i, check := range checks {
		check.Message = summarizeMacHostDryRunMessage(check.Message)
		sanitized[i] = check
	}
	return sanitized
}

func summarizeMacHostDryRunMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return "-"
	}
	code := xmlTagValue(message, "Code")
	if code == "DryRunOperation" || strings.Contains(message, "DryRunOperation") {
		return "DryRunOperation: request would have succeeded"
	}
	if code == "UnauthorizedOperation" || strings.Contains(message, "UnauthorizedOperation") {
		return "UnauthorizedOperation: coordinator AWS identity needs EC2 Mac host lifecycle permissions, including ec2:AllocateHosts and ec2:CreateTags"
	}
	if code != "" {
		return code
	}
	const max = 240
	if len(message) > max {
		return strings.TrimSpace(message[:max]) + "..."
	}
	return message
}

func xmlTagValue(input, tag string) string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	start := strings.Index(input, open)
	if start < 0 {
		return ""
	}
	start += len(open)
	end := strings.Index(input[start:], close)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(input[start : start+end])
}

func (a App) adminMacHostsRelease(ctx context.Context, args []string) error {
	args, forceAnywhere := extractBoolFlag(args, "force")
	args, jsonAnywhere := extractBoolFlag(args, "json")
	fs := newFlagSet("admin mac-hosts release", a.Stderr)
	id := fs.String("id", "", "EC2 Mac Dedicated Host id")
	region := fs.String("region", "", "AWS region")
	force := fs.Bool("force", false, "confirm host release")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" && fs.NArg() > 0 {
		*id = fs.Arg(0)
	}
	if forceAnywhere {
		*force = true
	}
	if jsonAnywhere {
		*jsonOut = true
	}
	if *id == "" {
		return exit(2, "usage: crabbox admin mac-hosts release <host-id> [--region <region>] --force")
	}
	if !*force {
		return exit(2, "admin mac-hosts release requires --force")
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	released, err := coord.AdminReleaseMacHost(ctx, *region, *id)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(released)
	}
	fmt.Fprintf(a.Stdout, "released host=%s region=%s released=%s\n", *id, blank(*region, "-"), strings.Join(released, ","))
	return nil
}

func (a App) adminRelease(ctx context.Context, args []string) error {
	args, deleteAnywhere := extractBoolFlag(args, "delete")
	args, jsonAnywhere := extractBoolFlag(args, "json")
	fs := newFlagSet("admin release", a.Stderr)
	id := fs.String("id", "", "lease id or slug")
	deleteServer := fs.Bool("delete", false, "delete server while releasing")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" && fs.NArg() > 0 {
		*id = fs.Arg(0)
	}
	if *id == "" {
		return exit(2, "usage: crabbox admin release --id <lease-id-or-slug>")
	}
	if deleteAnywhere {
		*deleteServer = true
	}
	if jsonAnywhere {
		*jsonOut = true
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	lease, err := coord.AdminReleaseLease(ctx, *id, *deleteServer)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(lease)
	}
	fmt.Fprintf(a.Stdout, "released %s slug=%s state=%s delete=%t\n", lease.ID, blank(lease.Slug, "-"), lease.State, *deleteServer)
	return nil
}

func (a App) adminDelete(ctx context.Context, args []string) error {
	args, forceAnywhere := extractBoolFlag(args, "force")
	args, jsonAnywhere := extractBoolFlag(args, "json")
	fs := newFlagSet("admin delete", a.Stderr)
	id := fs.String("id", "", "lease id or slug")
	force := fs.Bool("force", false, "confirm deletion")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" && fs.NArg() > 0 {
		*id = fs.Arg(0)
	}
	if *id == "" {
		return exit(2, "usage: crabbox admin delete --id <lease-id-or-slug> --force")
	}
	if forceAnywhere {
		*force = true
	}
	if jsonAnywhere {
		*jsonOut = true
	}
	if !*force {
		return exit(2, "admin delete requires --force")
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	lease, err := coord.AdminDeleteLease(ctx, *id)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(lease)
	}
	fmt.Fprintf(a.Stdout, "deleted %s slug=%s state=%s\n", lease.ID, blank(lease.Slug, "-"), lease.State)
	return nil
}

func configuredCoordinator() (*CoordinatorClient, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	coord, ok, err := newCoordinatorClient(cfg)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, exit(2, "command requires a configured coordinator")
	}
	return coord, nil
}

func configuredAdminCoordinator() (*CoordinatorClient, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	if cfg.CoordAdminToken == "" {
		return nil, exit(2, "admin command requires broker.adminToken or CRABBOX_COORDINATOR_ADMIN_TOKEN")
	}
	cfg.CoordToken = cfg.CoordAdminToken
	coord, ok, err := newCoordinatorClient(cfg)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, exit(2, "admin command requires a configured coordinator")
	}
	return coord, nil
}
