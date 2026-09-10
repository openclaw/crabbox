package gcp

import (
	"flag"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

var (
	_ core.ProviderClassProfileProvider             = Provider{}
	_ core.ProviderClassSpecProvider                = Provider{}
	_ core.ProviderReadyPoolImageIdentityCapability = Provider{}
)

// Google publishes these standard machine-family ratios in the Compute Engine
// machine-family tables: https://cloud.google.com/compute/docs/general-purpose-machines
var memoryQuarterGBPerVCPU = map[string]int{
	"c4-standard":  15,
	"c3-standard":  16,
	"n2-standard":  16,
	"n2d-standard": 16,
}

var classProfiles = buildClassProfiles()

var (
	readyPoolNumericIDPattern = regexp.MustCompile(`^[0-9]+$`)
	readyPoolResourcePattern  = regexp.MustCompile(
		`^projects/([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)/global/(images|snapshots)/([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)$`,
	)
	readyPoolScopePattern = regexp.MustCompile(
		`^projects/[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?/global/(images|snapshots)$`,
	)
)

func (Provider) Name() string { return "gcp" }
func (Provider) Aliases() []string {
	return []string{"google", "google-cloud"}
}
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Name:   "gcp",
		Family: "gcp",
		Kind:   core.ProviderKindSSHLease,
		Targets: []core.TargetSpec{
			{OS: core.TargetLinux},
		},
		Features:         core.FeatureSet{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureTailscale},
		Coordinator:      core.CoordinatorSupported,
		ClassDisposition: core.ProviderClassDispositionMapped,
	}
}
func (Provider) RegisterFlags(*flag.FlagSet, core.Config) any { return core.NoProviderFlags() }
func (Provider) ApplyFlags(*core.Config, *flag.FlagSet, any) error {
	return nil
}

func (Provider) ReadyPoolImageIdentityMatchesLease(req core.ProviderReadyPoolImageIdentityRequest) bool {
	image := req.Lease.Image
	if image == nil {
		return false
	}
	scope, ok := readyPoolImageScope(image.SourceID, image.Kind)
	return req.Identity.Provider == "gcp" &&
		req.Lease.Provider == "gcp" &&
		image.Provider == "gcp" &&
		req.Lease.Project != "" &&
		strings.TrimSpace(req.Lease.Project) == req.Lease.Project &&
		ok &&
		readyPoolScopePattern.MatchString(req.Identity.Scope) &&
		readyPoolNumericIDPattern.MatchString(req.Identity.ID) &&
		image.ID == req.Identity.ID &&
		scope == req.Identity.Scope
}

func readyPoolImageScope(sourceID, kind string) (string, bool) {
	resource := sourceID
	if strings.HasPrefix(resource, "https://") {
		matched := false
		for _, prefix := range []string{
			"https://compute.googleapis.com/compute/v1/",
			"https://www.googleapis.com/compute/v1/",
		} {
			if strings.HasPrefix(resource, prefix) {
				resource = strings.TrimPrefix(resource, prefix)
				matched = true
				break
			}
		}
		if !matched {
			return "", false
		}
	}
	match := readyPoolResourcePattern.FindStringSubmatch(resource)
	if match == nil {
		return "", false
	}
	collection := match[3]
	if (kind == "gcp-image" && collection != "images") ||
		(kind == "gcp-disk-snapshot" && collection != "snapshots") ||
		(kind != "gcp-image" && kind != "gcp-disk-snapshot") {
		return "", false
	}
	return fmt.Sprintf("projects/%s/global/%s", match[1], collection), true
}

func (Provider) PrepareLeaseClaimEndpoint(existing core.LeaseClaim, provider, slug string, server core.Server, allowProviderMetadata bool) (core.Server, error) {
	_ = allowProviderMetadata
	if provider != "gcp" {
		return core.Server{}, core.Exit(2, "refusing to rewrite GCP lease=%s as provider=%s", existing.LeaseID, provider)
	}
	if slug != existing.Slug || server.Labels["lease"] != existing.LeaseID || server.Labels["slug"] != existing.Slug {
		return core.Server{}, core.Exit(2, "refusing to rewrite GCP lease=%s with mismatched label identity", existing.LeaseID)
	}
	if existing.CloudID != "" && server.CloudID != "" && existing.CloudID != server.CloudID {
		return core.Server{}, core.Exit(2, "refusing to rewrite GCP lease=%s with stale instance name", existing.LeaseID)
	}
	if existing.CloudNumericID != 0 && server.ID != 0 && existing.CloudNumericID != server.ID {
		return core.Server{}, core.Exit(2, "refusing to rewrite GCP lease=%s with stale numeric instance identity", existing.LeaseID)
	}
	if existing.CloudNumericID == 0 {
		server.ID = 0
	}
	labels := make(map[string]string, len(server.Labels))
	for key, value := range server.Labels {
		labels[key] = value
	}
	for _, key := range []string{"zone", "provider_key"} {
		existingValue := existing.Labels[key]
		if cloudValue := labels[key]; existingValue != "" && cloudValue != "" && cloudValue != existingValue {
			return core.Server{}, core.Exit(2, "refusing to rewrite GCP lease=%s with mismatched %s", existing.LeaseID, key)
		}
		if existingValue != "" {
			labels[key] = existingValue
		} else {
			delete(labels, key)
		}
	}
	server.Labels = labels
	return server, nil
}

func (Provider) ServerTypeForConfig(cfg core.Config) string {
	if candidates, matched := core.ProviderClassCandidatesForProfiles(classProfiles, cfg); matched {
		return candidates[0]
	}
	if core.IsCanonicalProviderClass(cfg.Class) {
		return ""
	}
	return gcpMachineTypeCandidatesForClass(cfg.Class)[0]
}

func (Provider) ServerTypeForClass(class string) string {
	return gcpMachineTypeCandidatesForClass(class)[0]
}

func (Provider) ClassProfiles() []core.ProviderClassProfile {
	return classProfiles
}

func (Provider) ClassSpecs() []core.ClassSpec {
	return core.ProviderClassSpecsFromProfiles(classProfiles)
}

func buildClassProfiles() []core.ProviderClassProfile {
	profiles := make([]core.ProviderClassProfile, 0, len(core.CanonicalProviderClasses()))
	for _, class := range core.CanonicalProviderClasses() {
		candidates := gcpMachineTypeCandidatesForClass(class)
		machines := make([]core.ProviderClassMachine, 0, len(candidates))
		for _, machineType := range candidates {
			machines = append(machines, gcpClassMachine(machineType))
		}
		profiles = append(profiles, core.ProviderClassProfileFromMachines(
			class, core.TargetLinux, "", core.ProviderClassArchitectureAMD64, machines,
		))
	}
	return profiles
}

func gcpClassMachine(machineType string) core.ProviderClassMachine {
	vcpus, memoryGB := machineShape(machineType)
	machine := core.ProviderClassMachine{Type: machineType, Architecture: core.ProviderClassArchitectureAMD64}
	if vcpus > 0 {
		machine.VCPU = &vcpus
	}
	if memoryGB > 0 {
		machine.Memory = &core.ProviderMemory{Value: memoryGB, Unit: core.ProviderMemoryUnitGB}
	}
	return machine
}

func machineShape(machineType string) (int, float64) {
	normalized := strings.ToLower(strings.TrimSpace(machineType))
	separator := strings.LastIndexByte(normalized, '-')
	if separator < 0 || separator == len(normalized)-1 {
		return 0, 0
	}
	vcpus, err := strconv.Atoi(normalized[separator+1:])
	if err != nil || vcpus <= 0 {
		return 0, 0
	}
	memoryQuartersPerVCPU, ok := memoryQuarterGBPerVCPU[normalized[:separator]]
	if !ok {
		return vcpus, 0
	}
	return vcpus, float64(vcpus*memoryQuartersPerVCPU) / 4
}

func gcpMachineTypeCandidatesForClass(class string) []string {
	switch class {
	case "tiny":
		return []string{"c4-standard-4", "c3-standard-4", "n2-standard-4", "n2d-standard-4"}
	case "small":
		return []string{"c4-standard-8", "c3-standard-8", "n2-standard-8", "n2d-standard-8", "c4-standard-4"}
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

func (p Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	return NewGCPLeaseBackend(p.Spec(), cfg, rt), nil
}

func (p Provider) ConfigureDoctor(cfg core.Config, rt core.Runtime) (core.DoctorBackend, error) {
	return shared.ConfigureDoctor("gcp", func() (core.Backend, error) { return p.Configure(cfg, rt) })
}

func (Provider) NativeCheckpointCapability(req core.NativeCheckpointRequest) (core.NativeCheckpointCapability, bool) {
	if req.Config.Coordinator == "" || req.Server.CloudID == "" {
		return core.NativeCheckpointCapability{}, false
	}
	if firstNonBlank(req.Target.TargetOS, req.Config.TargetOS) != core.TargetLinux {
		return core.NativeCheckpointCapability{}, false
	}
	if core.NormalizeCheckpointStrategy(req.Strategy) == core.CheckpointStrategyImage {
		return core.NativeCheckpointCapability{Kind: core.CheckpointKindGCP, RetireSource: true}, true
	}
	return core.NativeCheckpointCapability{Kind: core.CheckpointKindGCPDisk, RetireSource: true}, true
}

func firstNonBlank(values ...string) string {
	return shared.FirstNonEmpty(values...)
}

func (Provider) ApplyNativeCheckpointForkConfig(req core.NativeCheckpointForkRequest) error {
	cfg := req.Config
	switch req.Record.Kind {
	case core.CheckpointKindGCP:
		cfg.GCPMachineImage = firstNonBlank(req.Record.Resource, req.Record.ImageID)
	case core.CheckpointKindGCPDisk:
		cfg.GCPSnapshot = firstNonBlank(req.Record.Resource, req.Record.ImageID)
	default:
		return core.Exit(2, "provider=gcp does not support checkpoint kind=%s", req.Record.Kind)
	}
	if req.Record.Region != "" {
		cfg.GCPZone = req.Record.Region
	}
	if req.Record.Project != "" {
		core.SetGCPProjectExplicit(cfg, req.Record.Project)
	}
	return nil
}
