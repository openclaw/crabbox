package lambda

import (
	"context"
	"errors"
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type doctorClient interface {
	ListRegions(context.Context) ([]Region, error)
	ListInstanceTypes(context.Context) ([]InstanceType, error)
	ListImages(context.Context) ([]Image, error)
	ListInstances(context.Context) ([]Instance, error)
	ListFilesystems(context.Context) ([]Filesystem, error)
	ListFirewallRulesets(context.Context) ([]FirewallRuleset, error)
}

func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	if tokenFromEnv() == "" {
		return core.DoctorResult{
			Provider: providerName,
			Checks: []core.DoctorCheck{{
				Status:  "failed",
				Check:   "auth",
				Message: "provider=lambda class=missing_token hint=set LAMBDA_API_KEY mutation=false",
				Details: map[string]string{"provider": providerName, "class": "missing_token", "auth": "missing", "mutation": "false"},
			}},
		}, nil
	}
	client, err := b.clientFactory(b.rt)
	if err != nil {
		return core.DoctorResult{}, err
	}
	return lambdaDoctor(ctx, b.cfg, client)
}

func lambdaDoctor(ctx context.Context, cfg core.Config, client doctorClient) (core.DoctorResult, error) {
	checks := []core.DoctorCheck{}
	add := func(status, check, class, message string, details map[string]string) {
		if details == nil {
			details = map[string]string{}
		}
		details["provider"] = providerName
		details["class"] = class
		details["mutation"] = "false"
		checks = append(checks, core.DoctorCheck{Status: status, Check: check, Message: message, Details: details})
	}

	regions, err := client.ListRegions(ctx)
	if err != nil {
		add("failed", "auth", classifyError(err), fmt.Sprintf("provider=lambda class=%s mutation=false %v", classifyError(err), err), nil)
		return core.DoctorResult{Provider: providerName, Checks: checks}, nil
	}
	add("ok", "auth", "ready", "provider=lambda auth=ready mutation=false", map[string]string{"auth": "ready"})

	region := regionForConfig(cfg)
	if !hasRegion(regions, region) {
		add("failed", "region", "missing_region", fmt.Sprintf("provider=lambda region=%s class=missing_region mutation=false", region), map[string]string{"region": region})
		return core.DoctorResult{Provider: providerName, Checks: checks}, nil
	}
	add("ok", "region", "ready", fmt.Sprintf("provider=lambda region=%s mutation=false", region), map[string]string{"region": region})

	types, err := client.ListInstanceTypes(ctx)
	if err != nil {
		add("failed", "capacity", classifyError(err), fmt.Sprintf("provider=lambda class=%s mutation=false %v", classifyError(err), err), nil)
		return core.DoctorResult{Provider: providerName, Checks: checks}, nil
	}
	instanceType := typeForConfig(cfg)
	typeItem, ok := findType(types, instanceType)
	if !ok {
		add("failed", "type", "missing_type", fmt.Sprintf("provider=lambda type=%s class=missing_type mutation=false", instanceType), map[string]string{"type": instanceType})
		return core.DoctorResult{Provider: providerName, Checks: checks}, nil
	}
	if !typeHasCapacity(typeItem, region) {
		add("failed", "capacity", "insufficient_capacity", fmt.Sprintf("provider=lambda region=%s type=%s class=insufficient_capacity mutation=false", region, instanceType), map[string]string{"region": region, "type": instanceType})
		return core.DoctorResult{Provider: providerName, Checks: checks}, nil
	}
	add("ok", "capacity", "ready", fmt.Sprintf("provider=lambda region=%s type=%s mutation=false", region, instanceType), map[string]string{"region": region, "type": instanceType})

	if imageForConfig(cfg) != "" || imageFamilyForConfig(cfg) != "" {
		images, err := client.ListImages(ctx)
		if err != nil {
			add("failed", "image", classifyError(err), fmt.Sprintf("provider=lambda class=%s mutation=false %v", classifyError(err), err), nil)
			return core.DoctorResult{Provider: providerName, Checks: checks}, nil
		}
		if !hasImage(images, imageForConfig(cfg), imageFamilyForConfig(cfg), region) {
			add("failed", "image", "missing_image", fmt.Sprintf("provider=lambda image=%s image_family=%s class=missing_image mutation=false", imageForConfig(cfg), imageFamilyForConfig(cfg)), map[string]string{"image": imageForConfig(cfg), "image_family": imageFamilyForConfig(cfg)})
			return core.DoctorResult{Provider: providerName, Checks: checks}, nil
		}
		add("ok", "image", "ready", fmt.Sprintf("provider=lambda image=%s image_family=%s mutation=false", imageForConfig(cfg), imageFamilyForConfig(cfg)), map[string]string{"image": imageForConfig(cfg), "image_family": imageFamilyForConfig(cfg)})
	}

	if len(cfg.Lambda.FilesystemNames) > 0 || len(cfg.Lambda.FilesystemMounts) > 0 {
		filesystems, err := client.ListFilesystems(ctx)
		if err != nil {
			add("failed", "filesystem", classifyError(err), fmt.Sprintf("provider=lambda class=%s mutation=false %v", classifyError(err), err), nil)
			return core.DoctorResult{Provider: providerName, Checks: checks}, nil
		}
		for _, name := range requestedFilesystemNames(cfg) {
			if !hasFilesystem(filesystems, name, region) {
				add("failed", "filesystem", "missing_filesystem", fmt.Sprintf("provider=lambda filesystem=%s region=%s class=missing_filesystem mutation=false", name, region), map[string]string{"filesystem": name, "region": region})
				return core.DoctorResult{Provider: providerName, Checks: checks}, nil
			}
		}
		add("ok", "filesystem", "ready", fmt.Sprintf("provider=lambda filesystems=%d mutation=false", len(requestedFilesystemNames(cfg))), map[string]string{"filesystems": fmt.Sprintf("%d", len(requestedFilesystemNames(cfg)))})
	}

	if strings.TrimSpace(cfg.Lambda.FirewallRuleset) != "" {
		rulesets, err := client.ListFirewallRulesets(ctx)
		if err != nil {
			add("failed", "firewall", classifyError(err), fmt.Sprintf("provider=lambda class=%s mutation=false %v", classifyError(err), err), nil)
			return core.DoctorResult{Provider: providerName, Checks: checks}, nil
		}
		if !hasFirewallRuleset(rulesets, cfg.Lambda.FirewallRuleset, region) {
			add("failed", "firewall", "missing_firewall", fmt.Sprintf("provider=lambda firewall_ruleset=%s region=%s class=missing_firewall mutation=false", cfg.Lambda.FirewallRuleset, region), map[string]string{"firewall_ruleset": cfg.Lambda.FirewallRuleset, "region": region})
			return core.DoctorResult{Provider: providerName, Checks: checks}, nil
		}
		add("ok", "firewall", "ready", fmt.Sprintf("provider=lambda firewall_ruleset=%s mutation=false", cfg.Lambda.FirewallRuleset), map[string]string{"firewall_ruleset": cfg.Lambda.FirewallRuleset})
	}

	instances, err := client.ListInstances(ctx)
	if err != nil {
		add("failed", "inventory", classifyError(err), fmt.Sprintf("provider=lambda class=%s mutation=false %v", classifyError(err), err), nil)
		return core.DoctorResult{Provider: providerName, Checks: checks}, nil
	}
	add("ok", "inventory", "ready", fmt.Sprintf("provider=lambda inventory=ready api=list mutation=false leases=%d", len(instances)), map[string]string{"leases": fmt.Sprintf("%d", len(instances))})
	return core.DoctorResult{Provider: providerName, Checks: checks}, nil
}

func classifyError(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case "global/invalid-api-key":
			return "invalid_auth"
		case "global/account-inactive":
			return "account_inactive"
		case "global/invalid-address":
			return "invalid_billing"
		case "global/quota-exceeded":
			return "quota"
		case "instance-operations/launch/insufficient-capacity":
			return "capacity"
		case "instance-operations/launch/file-system-in-wrong-region":
			return "filesystem_region"
		case "global/object-does-not-exist":
			return "missing_resource"
		case "global/invalid-parameters":
			return "invalid_config"
		default:
			if strings.HasPrefix(apiErr.Code, "provider/") {
				return "provider"
			}
		}
	}
	return "provider"
}

func hasRegion(regions []Region, name string) bool {
	for _, region := range regions {
		if region.Name == name {
			return true
		}
	}
	return false
}

func findType(types []InstanceType, name string) (InstanceType, bool) {
	for _, item := range types {
		if item.Name == name {
			return item, true
		}
	}
	return InstanceType{}, false
}

func typeHasCapacity(item InstanceType, region string) bool {
	for _, available := range item.RegionsWithCapacityAvailable {
		if available == region {
			return true
		}
	}
	return false
}

func hasImage(images []Image, id, family, region string) bool {
	for _, image := range images {
		if id != "" && image.ID != id {
			continue
		}
		if family != "" && image.Family != family {
			continue
		}
		if image.Region != "" && image.Region != region {
			continue
		}
		return true
	}
	return false
}

func requestedFilesystemNames(cfg core.Config) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, name := range cfg.Lambda.FilesystemNames {
		name = strings.TrimSpace(name)
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, mount := range cfg.Lambda.FilesystemMounts {
		name := strings.TrimSpace(mount.Name)
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func hasFilesystem(filesystems []Filesystem, name, region string) bool {
	for _, fs := range filesystems {
		if fs.Name == name && (fs.Region.Name == "" || fs.Region.Name == region) {
			return true
		}
	}
	return false
}

func hasFirewallRuleset(rulesets []FirewallRuleset, name, region string) bool {
	for _, ruleset := range rulesets {
		if ruleset.Name == name && (ruleset.Region.Name == "" || ruleset.Region.Name == region) {
			return true
		}
	}
	return false
}
