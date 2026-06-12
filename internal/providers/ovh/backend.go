package ovh

import (
	"context"
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type Backend struct {
	shared.DirectSSHBackend
	clientFactory func(core.Config, core.Runtime) (API, error)
}

func NewBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) *Backend {
	cfg.Provider = providerName
	b := &Backend{
		DirectSSHBackend: shared.DirectSSHBackend{SpecValue: spec, Cfg: cfg, RT: rt},
	}
	b.clientFactory = func(cfg core.Config, rt core.Runtime) (API, error) {
		return newClient(cfg, rt)
	}
	return b
}

func (b *Backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	cfg := b.Cfg
	if strings.TrimSpace(cfg.OVH.ProjectID) == "" {
		return doctorConfigFailure("CRABBOX_OVH_PROJECT_ID or ovh.projectId is required"), nil
	}
	client, err := b.clientFactory(cfg, b.RT)
	if err != nil {
		return core.DoctorResult{}, err
	}
	if _, err := client.AuthTime(ctx); err != nil {
		return core.DoctorResult{}, err
	}
	regions, err := client.ListRegions(ctx, cfg.OVH.ProjectID)
	if err != nil {
		return core.DoctorResult{}, err
	}
	if cfg.OVH.Region != "" && !regionExists(regions, cfg.OVH.Region) {
		return doctorCheckFailure("region", fmt.Sprintf("OVH region %q was not returned by the project", cfg.OVH.Region)), nil
	}
	flavors, err := client.ListFlavors(ctx, cfg.OVH.ProjectID, cfg.OVH.Region)
	if err != nil {
		return core.DoctorResult{}, err
	}
	if cfg.OVH.Flavor != "" && !flavorExists(flavors, cfg.OVH.Flavor) {
		return doctorCheckFailure("flavor", fmt.Sprintf("OVH flavor %q was not returned by the project", cfg.OVH.Flavor)), nil
	}
	images, err := client.ListImages(ctx, cfg.OVH.ProjectID, cfg.OVH.Region)
	if err != nil {
		return core.DoctorResult{}, err
	}
	if cfg.OVH.Image != "" && !imageExists(images, cfg.OVH.Image) {
		return doctorCheckFailure("image", fmt.Sprintf("OVH image %q was not returned by the project", cfg.OVH.Image)), nil
	}
	instances, err := client.ListInstances(ctx, cfg.OVH.ProjectID)
	if err != nil {
		return core.DoctorResult{}, err
	}
	leases := 0
	for _, instance := range instances {
		if isCrabboxInstance(instance) {
			leases++
		}
	}
	result := core.InventoryDoctorResult(providerName, leases)
	result.Message += fmt.Sprintf(" endpoint=%s region=%s image=%s flavor=%s", redactedEndpoint(cfg.OVH.Endpoint), blank(cfg.OVH.Region), blank(cfg.OVH.Image), blank(cfg.OVH.Flavor))
	result.Checks = []core.DoctorCheck{
		{Status: "passed", Check: "auth", Message: "OVH credentials accepted", Details: map[string]string{"mutation": "false"}},
		{Status: "passed", Check: "inventory", Message: fmt.Sprintf("found %d Crabbox-owned OVH instances", leases), Details: map[string]string{"mutation": "false"}},
	}
	return result, nil
}

func (b *Backend) Acquire(context.Context, core.AcquireRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, lifecycleUnavailable("acquire")
}

func (b *Backend) Resolve(context.Context, core.ResolveRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, lifecycleUnavailable("resolve")
}

func (b *Backend) List(context.Context, core.ListRequest) ([]core.LeaseView, error) {
	return nil, lifecycleUnavailable("list")
}

func (b *Backend) ReleaseLease(context.Context, core.ReleaseLeaseRequest) error {
	return lifecycleUnavailable("release")
}

func (b *Backend) Touch(context.Context, core.TouchRequest) (core.Server, error) {
	return core.Server{}, lifecycleUnavailable("touch")
}

func (b *Backend) Cleanup(context.Context, core.CleanupRequest) error {
	return lifecycleUnavailable("cleanup")
}

func lifecycleUnavailable(operation string) error {
	return core.Exit(2, "ovh %s lifecycle is not implemented in this build; run doctor --provider ovh for non-mutating readiness checks", operation)
}

func doctorConfigFailure(message string) core.DoctorResult {
	return doctorCheckFailure("configuration", message)
}

func doctorCheckFailure(check, message string) core.DoctorResult {
	return core.DoctorResult{
		Provider: providerName,
		Message:  "auth=configuration-incomplete control_plane=unchecked inventory=unchecked mutation=false runtime=unchecked",
		Status:   "failed",
		Checks: []core.DoctorCheck{{
			Status:  "failed",
			Check:   check,
			Message: message,
			Details: map[string]string{"mutation": "false"},
		}},
	}
}

func regionExists(regions []Region, name string) bool {
	for _, region := range regions {
		if region.Name == name {
			return true
		}
	}
	return false
}

func flavorExists(flavors []Flavor, value string) bool {
	for _, flavor := range flavors {
		if flavor.Matches(value) {
			return true
		}
	}
	return false
}

func imageExists(images []Image, value string) bool {
	for _, image := range images {
		if image.Matches(value) {
			return true
		}
	}
	return false
}

func isCrabboxInstance(instance Instance) bool {
	if strings.HasPrefix(instance.Name, "crabbox-") || strings.HasPrefix(instance.Name, "cbx_") {
		return true
	}
	if instance.Labels["managed_by"] == "crabbox" || instance.Labels["crabbox"] == "true" {
		return true
	}
	for _, tag := range instance.Tags {
		if tag == "crabbox" || strings.HasPrefix(tag, "crabbox:") {
			return true
		}
	}
	return false
}

func blank(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
