package nebius

import (
	"context"
	"fmt"
	"strings"
)

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func NewBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt}
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) Acquire(context.Context, AcquireRequest) (LeaseTarget, error) {
	return LeaseTarget{}, notImplemented("acquire")
}

func (b *backend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	return LeaseTarget{}, notImplemented("resolve")
}

func (b *backend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, notImplemented("list")
}

func (b *backend) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	return notImplemented("release")
}

func (b *backend) Touch(context.Context, TouchRequest) error {
	return notImplemented("touch")
}

func (b *backend) Cleanup(context.Context, CleanupRequest) error {
	return notImplemented("cleanup")
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	if b.rt.Exec == nil {
		return DoctorResult{}, exit(2, "provider=nebius doctor requires command runner")
	}
	client := newCLIRunner(b.cfg.Nebius, b.rt)
	checks := []DoctorCheck{
		b.checkVersion(ctx, client),
		b.checkProfile(ctx, client),
		b.checkParentID(ctx, client),
		b.checkSubnet(ctx, client),
		b.checkPlatform(ctx, client),
		b.checkImage(ctx, client),
		b.checkJSON(ctx, client),
	}
	status := "ok"
	for _, check := range checks {
		if check.Status != "ok" {
			status = "error"
			break
		}
	}
	return DoctorResult{
		Provider: providerName,
		Status:   status,
		Message:  fmt.Sprintf("cli=%s control_plane=read_only mutation=false", status),
		Checks:   checks,
	}, nil
}

func (b *backend) checkVersion(ctx context.Context, client cliRunner) DoctorCheck {
	result, err := client.run(ctx, "--version")
	if err != nil {
		return doctorCheck("cli", "error", err.Error(), nil)
	}
	return doctorCheck("cli", "ok", "nebius cli available", map[string]string{"version": redactNebiusText(firstNonBlank(result.Stdout, result.Stderr))})
}

func (b *backend) checkProfile(ctx context.Context, client cliRunner) DoctorCheck {
	result, err := client.run(ctx, "config", "profile", "list", "--format", "json")
	if err != nil {
		return doctorCheck("profile", "error", err.Error(), nil)
	}
	if !isJSON(result.Stdout) {
		return doctorCheck("profile", "error", "profile list did not return JSON", nil)
	}
	return doctorCheck("profile", "ok", "profile store readable", nil)
}

func (b *backend) checkParentID(ctx context.Context, client cliRunner) DoctorCheck {
	parentID := strings.TrimSpace(b.cfg.Nebius.ParentID)
	if parentID == "" {
		return doctorCheck("parent-id", "error", "nebius.parentId is required", nil)
	}
	result, err := client.run(ctx, "iam", "project", "get", parentID, "--format", "json")
	if err != nil {
		return doctorCheck("parent-id", "error", err.Error(), map[string]string{"parentId": parentID})
	}
	if !isJSON(result.Stdout) {
		return doctorCheck("parent-id", "error", "project lookup did not return JSON", map[string]string{"parentId": parentID})
	}
	return doctorCheck("parent-id", "ok", "project readable", map[string]string{"parentId": parentID})
}

func (b *backend) checkSubnet(ctx context.Context, client cliRunner) DoctorCheck {
	subnetID := strings.TrimSpace(b.cfg.Nebius.SubnetID)
	if subnetID == "" {
		return doctorCheck("subnet", "error", "nebius.subnetId is required", nil)
	}
	result, err := client.run(ctx, "vpc", "subnet", "list", "--parent-id", b.cfg.Nebius.ParentID, "--format", "json")
	if err != nil {
		return doctorCheck("subnet", "error", err.Error(), map[string]string{"subnetId": subnetID})
	}
	ok, err := containsIDOrName(result.Stdout, subnetID)
	if err != nil {
		return doctorCheck("subnet", "error", "subnet list did not return expected JSON", map[string]string{"subnetId": subnetID})
	}
	if !ok {
		return doctorCheck("subnet", "error", "configured subnet not found", map[string]string{"subnetId": subnetID})
	}
	return doctorCheck("subnet", "ok", "subnet readable", map[string]string{"subnetId": subnetID})
}

func (b *backend) checkPlatform(ctx context.Context, client cliRunner) DoctorCheck {
	platform := strings.TrimSpace(b.cfg.Nebius.Platform)
	result, err := client.run(ctx, "compute", "platform", "list", "--format", "json")
	if err != nil {
		return doctorCheck("platform", "error", err.Error(), map[string]string{"platform": platform})
	}
	ok, err := containsIDOrName(result.Stdout, platform)
	if err != nil {
		return doctorCheck("platform", "error", "platform list did not return expected JSON", map[string]string{"platform": platform})
	}
	if !ok {
		return doctorCheck("platform", "error", "configured platform not found", map[string]string{"platform": platform})
	}
	return doctorCheck("platform", "ok", "platform readable", map[string]string{"platform": platform})
}

func (b *backend) checkImage(ctx context.Context, client cliRunner) DoctorCheck {
	imageFamily := strings.TrimSpace(b.cfg.Nebius.ImageFamily)
	result, err := client.run(ctx, "compute", "image", "list", "--family", imageFamily, "--format", "json")
	if err != nil {
		return doctorCheck("image", "error", err.Error(), map[string]string{"imageFamily": imageFamily})
	}
	if !isJSON(result.Stdout) {
		return doctorCheck("image", "error", "image list did not return JSON", map[string]string{"imageFamily": imageFamily})
	}
	return doctorCheck("image", "ok", "image family readable", map[string]string{"imageFamily": imageFamily})
}

func (b *backend) checkJSON(ctx context.Context, client cliRunner) DoctorCheck {
	result, err := client.run(ctx, "config", "get", "parent-id", "--format", "json")
	if err != nil {
		return doctorCheck("json", "error", err.Error(), nil)
	}
	if !isJSON(result.Stdout) {
		return doctorCheck("json", "error", "json output is unavailable", nil)
	}
	return doctorCheck("json", "ok", "json output available", nil)
}

func doctorCheck(name, status, message string, details map[string]string) DoctorCheck {
	return DoctorCheck{Check: name, Status: status, Message: redactNebiusText(message), Details: details}
}
