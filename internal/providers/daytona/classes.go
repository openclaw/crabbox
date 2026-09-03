package daytona

import (
	"context"
	"slices"
	"strings"

	api "github.com/daytonaio/daytona/libs/api-client-go"
	core "github.com/openclaw/crabbox/internal/cli"
)

type snapshotShape struct {
	name              string
	cpu, memory, disk float32
}

// Daytona fixes resources in snapshots. Adjacent classes intentionally share
// its three container tiers; creation never sends unsupported resize fields.
var classShapes = [...]snapshotShape{
	{"daytona-small", 1, 1, 3},
	{"daytona-medium", 2, 4, 8},
	{"daytona-large", 4, 8, 10},
}

var classProfiles = buildClassProfiles()

func buildClassProfiles() []core.ProviderClassProfile {
	classes := core.CanonicalProviderClasses()
	profiles := make([]core.ProviderClassProfile, 0, len(classes))
	for i, class := range classes {
		shape := classShapes[i/2]
		cpu := int(shape.cpu)
		profiles = append(profiles, core.ProviderClassProfileFromMachines(class, core.TargetLinux, "", core.ProviderClassArchitectureAMD64, []core.ProviderClassMachine{{
			Type: shape.name, Architecture: core.ProviderClassArchitectureAMD64,
			VCPU: &cpu, Memory: &core.ProviderMemory{Value: float64(shape.memory), Unit: core.ProviderMemoryUnitGiB},
		}}))
	}
	return profiles
}

func (Provider) ClassProfiles() []core.ProviderClassProfile { return classProfiles }

func (Provider) ServerTypeForClass(class string) string {
	for _, profile := range classProfiles {
		if profile.Class == class {
			return profile.Primary.Type
		}
	}
	return ""
}

func (p Provider) ServerTypeForConfig(cfg Config) string {
	if core.ShouldUseCoordinator(cfg, p.Spec()) || !core.ClassWasExplicit(cfg) {
		return "snapshot"
	}
	if snapshot := strings.TrimSpace(cfg.Daytona.Snapshot); snapshot != "" {
		return snapshot
	}
	return p.ServerTypeForClass(cfg.Class)
}

func (b *daytonaLeaseBackend) ValidateCoordinatorAcquire() error {
	if core.ClassFlagWasExplicit(b.cfg) {
		return exit(2, "provider=daytona class selection requires direct mode; the coordinator selects its configured snapshot")
	}
	return nil
}

func selectClassSnapshot(ctx context.Context, client daytonaAPI, cfg *Config) (*api.SnapshotDto, error) {
	if !core.ClassWasExplicit(*cfg) {
		return nil, nil
	}
	candidates, matched := core.ProviderClassCandidatesForProfiles(classProfiles, *cfg)
	if !matched {
		return nil, exit(2, "provider=daytona has no class profile for class=%s target=%s architecture=%s", cfg.Class, cfg.TargetOS, cfg.Architecture)
	}
	var shape snapshotShape
	for _, candidate := range classShapes {
		if candidate.name == candidates[0] {
			shape = candidate
		}
	}
	selected := blank(strings.TrimSpace(cfg.Daytona.Snapshot), shape.name)
	snapshot, err := client.GetSnapshot(ctx, selected)
	if err != nil {
		return nil, daytonaError("resolve class snapshot", err)
	}
	if snapshot == nil || snapshot.GetId() == "" || selected != snapshot.GetId() && selected != snapshot.GetName() {
		return nil, exit(4, "Daytona class snapshot identity does not match %s", selected)
	}
	if snapshot.GetState() != api.SNAPSHOTSTATE_ACTIVE || snapshot.GetSandboxClass() != "container" ||
		snapshot.GetCpu() != shape.cpu || snapshot.GetMem() != shape.memory || snapshot.GetDisk() != shape.disk || snapshot.GetGpu() != 0 {
		return nil, exit(2, "Daytona snapshot %s must be active container with %g CPU, %g GiB memory, %g GiB disk and no GPU for class=%s", selected, shape.cpu, shape.memory, shape.disk, cfg.Class)
	}
	if target := strings.TrimSpace(cfg.Daytona.Target); target != "" && !slices.Contains(snapshot.GetRegionIds(), target) {
		return nil, exit(2, "Daytona snapshot %s is unavailable in target=%s", selected, target)
	}
	// Custom and captured snapshots keep their exact identity. Class validates
	// their resources; it must never replace a prepared filesystem with a default.
	cfg.Daytona.Snapshot = snapshot.GetId()
	return snapshot, nil
}

func validateClassSandbox(sandbox *api.Sandbox, snapshot *api.SnapshotDto, cfg Config) error {
	if snapshot == nil {
		return nil
	}
	if sandbox.GetCpu() != snapshot.GetCpu() || sandbox.GetMemory() != snapshot.GetMem() ||
		sandbox.GetDisk() != snapshot.GetDisk() || sandbox.GetGpu() != snapshot.GetGpu() ||
		(sandbox.HasSandboxClass() && sandbox.GetSandboxClass() != snapshot.GetSandboxClass()) ||
		(sandbox.GetSnapshot() != snapshot.GetId() && sandbox.GetSnapshot() != snapshot.GetName()) ||
		(strings.TrimSpace(cfg.Daytona.Target) != "" && sandbox.GetTarget() != strings.TrimSpace(cfg.Daytona.Target)) {
		return exit(4, "Daytona sandbox %s does not match the selected class snapshot", sandbox.GetId())
	}
	return nil
}
