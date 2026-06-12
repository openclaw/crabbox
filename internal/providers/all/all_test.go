package all

import (
	"io"
	"slices"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestAppleContainerRegistersWithoutAliasCollision(t *testing.T) {
	for _, alias := range []string{"apple-container", "apple", "applecontainer"} {
		provider, err := core.ProviderFor(alias)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", alias, err)
		}
		if provider.Name() != "apple-container" {
			t.Fatalf("ProviderFor(%q).Name=%q want apple-container", alias, provider.Name())
		}
	}
	// The bare "container" alias must keep pointing at local-container.
	got, err := core.ProviderFor("container")
	if err != nil {
		t.Fatalf("ProviderFor(container): %v", err)
	}
	if got.Name() != "local-container" {
		t.Fatalf("'container' alias now resolves to %q; apple-container must not steal it", got.Name())
	}
}

func TestDockerSandboxRegistersWithoutAliasCollision(t *testing.T) {
	provider, err := core.ProviderFor("docker-sandbox")
	if err != nil {
		t.Fatalf("ProviderFor(docker-sandbox): %v", err)
	}
	if provider.Name() != "docker-sandbox" {
		t.Fatalf("ProviderFor(docker-sandbox).Name=%q", provider.Name())
	}
	for _, alias := range []string{"docker", "container", "local-docker"} {
		got, err := core.ProviderFor(alias)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", alias, err)
		}
		if got.Name() != "local-container" {
			t.Fatalf("%q alias now resolves to %q; docker-sandbox must not steal local-container aliases", alias, got.Name())
		}
	}
}

func TestOpenSandboxRegistersWithoutAliasCollision(t *testing.T) {
	provider, err := core.ProviderFor("opensandbox")
	if err != nil {
		t.Fatalf("ProviderFor(opensandbox): %v", err)
	}
	if provider.Name() != "opensandbox" {
		t.Fatalf("ProviderFor(opensandbox).Name=%q", provider.Name())
	}
	for _, alias := range []string{"osb", "open-sandbox"} {
		if got, err := core.ProviderFor(alias); err == nil && got.Name() == "opensandbox" {
			t.Fatalf("%q alias unexpectedly resolves to opensandbox; v1 should reserve aliases", alias)
		}
	}
}

func TestNvidiaBrevRegistersCanonicalAndAliases(t *testing.T) {
	for _, name := range []string{"nvidia-brev", "brev", "nvidia"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
		if provider.Name() != "nvidia-brev" {
			t.Fatalf("ProviderFor(%q).Name=%q want nvidia-brev", name, provider.Name())
		}
		if _, ok := provider.(core.DoctorProvider); !ok {
			t.Fatalf("ProviderFor(%q) does not expose doctor", name)
		}
	}
}

func TestNebiusRegistersWithoutAliases(t *testing.T) {
	provider, err := core.ProviderFor("nebius")
	if err != nil {
		t.Fatalf("ProviderFor(nebius): %v", err)
	}
	if provider.Name() != "nebius" {
		t.Fatalf("ProviderFor(nebius).Name=%q", provider.Name())
	}
	if _, ok := provider.(core.DoctorProvider); !ok {
		t.Fatal("nebius provider does not expose doctor")
	}
	spec := provider.Spec()
	if spec.Family != "nebius" || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("nebius spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("nebius targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("nebius features=%v missing %s", spec.Features, feature)
		}
	}
	for _, alias := range []string{"nebius-ai", "nebius-compute", "nb"} {
		if got, err := core.ProviderFor(alias); err == nil && got.Name() == "nebius" {
			t.Fatalf("%q alias unexpectedly resolves to nebius", alias)
		}
	}
}

func TestAgentSandboxRegistersWithoutAliases(t *testing.T) {
	provider, err := core.ProviderFor("agent-sandbox")
	if err != nil {
		t.Fatalf("ProviderFor(agent-sandbox): %v", err)
	}
	if provider.Name() != "agent-sandbox" {
		t.Fatalf("ProviderFor(agent-sandbox).Name=%q", provider.Name())
	}
	spec := provider.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever || len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("agent-sandbox spec=%#v", spec)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("agent-sandbox features=%v", spec.Features)
	}
	for _, alias := range []string{"agentsandbox", "asb", "sandbox"} {
		if got, err := core.ProviderFor(alias); err == nil && got.Name() == "agent-sandbox" {
			t.Fatalf("%q alias unexpectedly resolves to agent-sandbox", alias)
		}
	}
}

func TestSuperserveRegistersWithoutAliases(t *testing.T) {
	provider, err := core.ProviderFor("superserve")
	if err != nil {
		t.Fatalf("ProviderFor(superserve): %v", err)
	}
	if provider.Name() != "superserve" {
		t.Fatalf("ProviderFor(superserve).Name=%q", provider.Name())
	}
	spec := provider.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever || len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("superserve spec=%#v", spec)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("superserve features=%v", spec.Features)
	}
	for _, alias := range []string{"ss", "sup", "super-serve"} {
		if got, err := core.ProviderFor(alias); err == nil && got.Name() == "superserve" {
			t.Fatalf("%q alias unexpectedly resolves to superserve", alias)
		}
	}
}

func TestScalewayRegistersWithoutAliases(t *testing.T) {
	provider, err := core.ProviderFor("scaleway")
	if err != nil {
		t.Fatalf("ProviderFor(scaleway): %v", err)
	}
	if provider.Name() != "scaleway" {
		t.Fatalf("ProviderFor(scaleway).Name=%q", provider.Name())
	}
	if provider.Aliases() != nil {
		t.Fatalf("scaleway aliases=%v, want none", provider.Aliases())
	}
	spec := provider.Spec()
	if spec.Kind != core.ProviderKindSSHLease || spec.Family != "scaleway" || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("scaleway spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("scaleway targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureTailscale} {
		if !spec.Features.Has(feature) {
			t.Fatalf("scaleway features=%v missing %s", spec.Features, feature)
		}
	}
	if _, ok := provider.(core.DoctorProvider); !ok {
		t.Fatal("scaleway does not expose doctor")
	}
}

func TestNamespaceInstanceRegistersWithoutAliasCollision(t *testing.T) {
	for _, name := range []string{"namespace-instance", "namespace-compute"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
		if provider.Name() != "namespace-instance" {
			t.Fatalf("ProviderFor(%q).Name=%q", name, provider.Name())
		}
	}
	provider, err := core.ProviderFor("namespace")
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != "namespace-devbox" {
		t.Fatalf("namespace alias resolves to %q; want namespace-devbox", provider.Name())
	}
}

func TestVercelSandboxRegistersWithoutAliases(t *testing.T) {
	provider, err := core.ProviderFor("vercel-sandbox")
	if err != nil {
		t.Fatalf("ProviderFor(vercel-sandbox): %v", err)
	}
	if provider.Name() != "vercel-sandbox" {
		t.Fatalf("ProviderFor(vercel-sandbox).Name=%q", provider.Name())
	}
	spec := provider.Spec()
	if spec.Family != "vercel" || spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("vercel-sandbox spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("vercel-sandbox targets=%#v", spec.Targets)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("vercel-sandbox features=%v", spec.Features)
	}
	for _, alias := range []string{"vercel", "vsb", "sandbox"} {
		if got, err := core.ProviderFor(alias); err == nil && got.Name() == "vercel-sandbox" {
			t.Fatalf("%q alias unexpectedly resolves to vercel-sandbox", alias)
		}
	}
}

func TestBlaxelRegistersCanonicalWithoutAliases(t *testing.T) {
	provider, err := core.ProviderFor("blaxel")
	if err != nil {
		t.Fatalf("ProviderFor(blaxel): %v", err)
	}
	if provider.Name() != "blaxel" {
		t.Fatalf("ProviderFor(blaxel).Name=%q", provider.Name())
	}
	for _, alias := range []string{"blx", "sandbox"} {
		if got, err := core.ProviderFor(alias); err == nil && got.Name() == "blaxel" {
			t.Fatalf("%q alias unexpectedly resolves to blaxel", alias)
		}
	}
	spec := provider.Spec()
	if spec.Family != "blaxel" || spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("blaxel spec=%#v", spec)
	}
	if !spec.Features.Has(core.FeatureArchiveSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("blaxel features=%#v", spec.Features)
	}
}

func TestAnthropicSandboxRuntimeRegistersCanonicalAndAlias(t *testing.T) {
	for _, name := range []string{"anthropic-sandbox-runtime", "srt"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
		if provider.Name() != "anthropic-sandbox-runtime" {
			t.Fatalf("ProviderFor(%q).Name=%q", name, provider.Name())
		}
	}
	spec := mustProvider(t, "anthropic-sandbox-runtime").Spec()
	if spec.Family != "anthropic-sandbox-runtime" || spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever || len(spec.Features) != 0 {
		t.Fatalf("anthropic-sandbox-runtime spec=%#v", spec)
	}
}

func TestCloudflareDynamicWorkersRegistersCanonicalAndAliases(t *testing.T) {
	for _, name := range []string{"cloudflare-dynamic-workers", "cf-dynamic", "cfdw"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
		if provider.Name() != "cloudflare-dynamic-workers" {
			t.Fatalf("ProviderFor(%q).Name=%q", name, provider.Name())
		}
	}
	if provider := mustProvider(t, "cloudflare"); provider.Name() != "cloudflare" {
		t.Fatalf("cloudflare resolved to %q; dynamic workers must not replace Cloudflare Containers", provider.Name())
	}
	if provider := mustProvider(t, "cf"); provider.Name() != "cloudflare" {
		t.Fatalf("cf alias resolved to %q; dynamic workers must not steal it", provider.Name())
	}
	spec := mustProvider(t, "cloudflare-dynamic-workers").Spec()
	if spec.Family != "cloudflare" || spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("cloudflare-dynamic-workers spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetWorkerRuntime {
		t.Fatalf("cloudflare-dynamic-workers targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureCleanup, core.FeatureModuleRun, core.FeatureRunSession} {
		if !spec.Features.Has(feature) {
			t.Fatalf("cloudflare-dynamic-workers features=%v missing %s", spec.Features, feature)
		}
	}
}

func TestCloudflareSandboxRegistersWithoutAliasCollision(t *testing.T) {
	provider, err := core.ProviderFor("cloudflare-sandbox")
	if err != nil {
		t.Fatalf("ProviderFor(cloudflare-sandbox): %v", err)
	}
	if provider.Name() != "cloudflare-sandbox" {
		t.Fatalf("ProviderFor(cloudflare-sandbox).Name=%q", provider.Name())
	}
	if _, ok := provider.(core.DoctorProvider); !ok {
		t.Fatalf("ProviderFor(cloudflare-sandbox) does not expose doctor")
	}
	spec := provider.Spec()
	if spec.Family != "cloudflare" || spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("cloudflare-sandbox spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("cloudflare-sandbox targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureArchiveSync, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("cloudflare-sandbox features=%v missing %s", spec.Features, feature)
		}
	}
	for _, alias := range []string{"cloudflare", "cf", "cloudflare-dynamic-workers", "cf-dynamic", "cfdw", "sandbox"} {
		got, err := core.ProviderFor(alias)
		if err == nil && got.Name() == "cloudflare-sandbox" {
			t.Fatalf("%q alias unexpectedly resolves to cloudflare-sandbox", alias)
		}
	}
	if provider := mustProvider(t, "cloudflare"); provider.Name() != "cloudflare" {
		t.Fatalf("cloudflare resolved to %q; sandbox must not replace Cloudflare Containers", provider.Name())
	}
	if provider := mustProvider(t, "cf"); provider.Name() != "cloudflare" {
		t.Fatalf("cf alias resolved to %q; sandbox must not steal it", provider.Name())
	}
	if provider := mustProvider(t, "cloudflare-dynamic-workers"); provider.Name() != "cloudflare-dynamic-workers" {
		t.Fatalf("cloudflare-dynamic-workers resolved to %q; sandbox must not replace Dynamic Workers", provider.Name())
	}
}

func TestCodeSandboxRegistersCanonicalAndAliases(t *testing.T) {
	for _, name := range []string{"codesandbox", "csb", "code-sandbox"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
		if provider.Name() != "codesandbox" {
			t.Fatalf("ProviderFor(%q).Name=%q want codesandbox", name, provider.Name())
		}
		if _, ok := provider.(core.DoctorProvider); !ok {
			t.Fatalf("ProviderFor(%q) does not expose doctor", name)
		}
	}
	spec := mustProvider(t, "codesandbox").Spec()
	if spec.Family != "codesandbox" || spec.Kind != core.ProviderKindDelegatedRun || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("codesandbox spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("codesandbox targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureArchiveSync, core.FeatureCleanup, core.FeaturePauseResume} {
		if !spec.Features.Has(feature) {
			t.Fatalf("codesandbox features=%v missing %s", spec.Features, feature)
		}
	}
	if spec.Features.Has(core.FeatureURLBridge) {
		t.Fatalf("codesandbox features=%v must not advertise URL bridge without BridgeProvider support", spec.Features)
	}
}

func TestIncusRegistersAsBuiltInProvider(t *testing.T) {
	provider, err := core.ProviderFor("incus")
	if err != nil {
		t.Fatalf("ProviderFor(incus): %v", err)
	}
	if provider.Name() != "incus" {
		t.Fatalf("ProviderFor(incus).Name=%q", provider.Name())
	}
}

func TestVultrRegistersAsBuiltInProvider(t *testing.T) {
	provider, err := core.ProviderFor("vultr")
	if err != nil {
		t.Fatalf("ProviderFor(vultr): %v", err)
	}
	if provider.Name() != "vultr" {
		t.Fatalf("ProviderFor(vultr).Name=%q", provider.Name())
	}
	spec := provider.Spec()
	if spec.Family != "vultr" || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("vultr spec=%#v", spec)
	}
	if !spec.Features.Has(core.FeatureSSH) || !spec.Features.Has(core.FeatureCrabboxSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("vultr features=%#v", spec.Features)
	}
	if spec.Features.Has(core.FeatureTailscale) {
		t.Fatalf("vultr must not advertise tailscale before lifecycle proof: %#v", spec.Features)
	}
}

func TestAppleVZRegistersAsBuiltInProvider(t *testing.T) {
	for _, name := range []string{"apple-vz", "applevz"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
		if provider.Name() != "apple-vz" {
			t.Fatalf("ProviderFor(%q).Name=%q want apple-vz", name, provider.Name())
		}
	}
}

func TestAllBuiltInProvidersExposeDoctor(t *testing.T) {
	for _, name := range allBuiltInProviderNames() {
		t.Run(name, func(t *testing.T) {
			provider, err := core.ProviderFor(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := provider.(core.DoctorProvider); !ok {
				t.Fatalf("%s does not implement DoctorProvider", name)
			}
		})
	}
}

func TestAllBuiltInProviderNamesCoverRegistry(t *testing.T) {
	want := core.RegisteredProviderNames()
	got := append([]string(nil), allBuiltInProviderNames()...)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("allBuiltInProviderNames()=%v, registered providers=%v", got, want)
	}
}

func TestProviderKindFeatureContracts(t *testing.T) {
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		spec := provider.Spec()
		if spec.Kind == core.ProviderKindSSHLease && !spec.Features.Has(core.FeatureSSH) {
			t.Fatalf("%s kind=%s must advertise %s", name, spec.Kind, core.FeatureSSH)
		}
		if spec.Features.Has(core.FeatureCrabboxSync) && (spec.Kind != core.ProviderKindSSHLease || !spec.Features.Has(core.FeatureSSH)) {
			t.Fatalf("%s advertises %s but kind=%s features=%v", name, core.FeatureCrabboxSync, spec.Kind, spec.Features)
		}
		for _, feature := range []core.Feature{core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode} {
			if spec.Features.Has(feature) && (spec.Kind != core.ProviderKindSSHLease || !spec.Features.Has(core.FeatureSSH)) {
				t.Fatalf("%s advertises %s but kind=%s features=%v", name, feature, spec.Kind, spec.Features)
			}
		}
		for _, feature := range []core.Feature{
			core.FeatureArchiveSync,
			core.FeatureModuleRun,
			core.FeatureRunProof,
			core.FeatureRunSession,
			core.FeatureRunArtifacts,
			core.FeatureRunDownloads,
			core.FeaturePauseResume,
			core.FeatureMCP,
		} {
			if spec.Features.Has(feature) && spec.Kind != core.ProviderKindDelegatedRun {
				t.Fatalf("%s advertises %s but kind=%s", name, feature, spec.Kind)
			}
		}
	}
}

func TestArchiveSyncFeatureGatesDelegatedSyncOptions(t *testing.T) {
	checked := 0
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		spec := provider.Spec()
		if spec.Kind != core.ProviderKindDelegatedRun {
			continue
		}
		err := core.RejectDelegatedSyncOptionsForSpec(spec, core.RunRequest{SyncOnly: true})
		if spec.Features.Has(core.FeatureArchiveSync) {
			if err != nil {
				t.Fatalf("%s advertises %s but rejects --sync-only: %v", name, core.FeatureArchiveSync, err)
			}
			if err := core.RejectDelegatedSyncOptionsForSpec(spec, core.RunRequest{ForceSyncLarge: true}); err != nil {
				t.Fatalf("%s advertises %s but rejects --force-sync-large: %v", name, core.FeatureArchiveSync, err)
			}
			if err := core.RejectDelegatedSyncOptionsForSpec(spec, core.RunRequest{ChecksumSync: true}); err == nil {
				t.Fatalf("%s advertises %s but accepts --checksum", name, core.FeatureArchiveSync)
			}
			checked++
			continue
		}
		if err == nil {
			t.Fatalf("%s accepts --sync-only without %s", name, core.FeatureArchiveSync)
		}
		if err := core.RejectDelegatedSyncOptionsForSpec(spec, core.RunRequest{ForceSyncLarge: true}); err == nil {
			t.Fatalf("%s accepts --force-sync-large without %s", name, core.FeatureArchiveSync)
		}
	}
	if checked == 0 {
		t.Fatalf("no providers advertised %s; conformance test is stale", core.FeatureArchiveSync)
	}
}

func TestModuleRunFeatureGatesScriptMode(t *testing.T) {
	checked := 0
	script := &core.RunScriptSpec{Source: "worker.mjs", Data: []byte("export default {}")}
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		spec := provider.Spec()
		if spec.Kind != core.ProviderKindDelegatedRun {
			continue
		}
		err := core.RejectDelegatedSyncOptionsForSpec(spec, core.RunRequest{
			ScriptRequested: true,
			Script:          script,
		})
		if spec.Features.Has(core.FeatureModuleRun) {
			if err != nil {
				t.Fatalf("%s advertises %s but rejects script mode: %v", name, core.FeatureModuleRun, err)
			}
			if err := core.RejectDelegatedSyncOptionsForSpec(spec, core.RunRequest{Command: []string{"node", "worker.mjs"}}); err == nil {
				t.Fatalf("%s advertises %s but accepts trailing command argv", name, core.FeatureModuleRun)
			}
			if err := core.RejectDelegatedSyncOptionsForSpec(spec, core.RunRequest{
				ScriptRequested: true,
				ShellMode:       true,
			}); err == nil {
				t.Fatalf("%s advertises %s but accepts --shell", name, core.FeatureModuleRun)
			}
			checked++
			continue
		}
		if err == nil {
			t.Fatalf("%s accepts script mode without %s", name, core.FeatureModuleRun)
		}
	}
	if checked == 0 {
		t.Fatalf("no providers advertised %s; conformance test is stale", core.FeatureModuleRun)
	}
}

func TestRunProofFeatureGatesEmitProof(t *testing.T) {
	checked := 0
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		spec := provider.Spec()
		if spec.Kind != core.ProviderKindDelegatedRun {
			continue
		}
		err := core.RejectDelegatedSyncOptionsForSpec(spec, core.RunRequest{EmitProof: "/tmp/proof.md"})
		if spec.Features.Has(core.FeatureRunProof) {
			if err != nil {
				t.Fatalf("%s advertises %s but rejects --emit-proof: %v", name, core.FeatureRunProof, err)
			}
			checked++
			continue
		}
		if err == nil {
			t.Fatalf("%s accepts --emit-proof without %s", name, core.FeatureRunProof)
		}
	}
	if checked == 0 {
		t.Fatalf("no providers advertised %s; conformance test is stale", core.FeatureRunProof)
	}
}

func TestProviderKindRequiresBackendInterface(t *testing.T) {
	sshLeaseProviders := 0
	delegatedRunProviders := 0
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		spec := provider.Spec()
		cfg, ok := offlineConformanceConfig(name)
		if !ok {
			t.Fatalf("%s kind=%s; add an offline conformance config for it", name, spec.Kind)
		}
		backend, err := provider.Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
		if err != nil {
			t.Fatalf("%s configure error: %v", name, err)
		}
		switch spec.Kind {
		case core.ProviderKindSSHLease:
			if _, ok := backend.(core.SSHLeaseBackend); !ok {
				t.Fatalf("%s kind=%s but backend %T is not SSHLeaseBackend", name, spec.Kind, backend)
			}
			sshLeaseProviders++
		case core.ProviderKindDelegatedRun:
			if _, ok := backend.(core.DelegatedRunBackend); !ok {
				t.Fatalf("%s kind=%s but backend %T is not DelegatedRunBackend", name, spec.Kind, backend)
			}
			delegatedRunProviders++
		case core.ProviderKindServiceControl:
			// Service-control providers expose command-specific interfaces instead of
			// the two lease/run backend shapes.
		default:
			t.Fatalf("%s has unknown provider kind %q", name, spec.Kind)
		}
	}
	if sshLeaseProviders == 0 {
		t.Fatalf("no providers advertised kind=%s; conformance test is stale", core.ProviderKindSSHLease)
	}
	if delegatedRunProviders == 0 {
		t.Fatalf("no providers advertised kind=%s; conformance test is stale", core.ProviderKindDelegatedRun)
	}
}

func TestRunSessionFeatureRequiresDelegatedBackendAndValidHandle(t *testing.T) {
	checked := 0
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		spec := provider.Spec()
		if !spec.Features.Has(core.FeatureRunSession) {
			if err := core.ValidateRunSessionForSpec(spec, core.RunResult{Session: &core.RunSessionHandle{
				Provider:       name,
				LeaseID:        "cbx_test",
				CleanupCommand: "crabbox stop --provider " + name + " cbx_test",
			}}); err == nil {
				t.Fatalf("%s accepts a run session without %s", name, core.FeatureRunSession)
			}
			continue
		}
		if spec.Kind != core.ProviderKindDelegatedRun {
			t.Fatalf("%s advertises %s but kind=%s", name, core.FeatureRunSession, spec.Kind)
		}
		cfg, ok := offlineConformanceConfig(name)
		if !ok {
			t.Fatalf("%s advertises %s; add an offline conformance config for it", name, core.FeatureRunSession)
		}
		backend, err := provider.Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
		if err != nil {
			t.Fatalf("%s configure error: %v", name, err)
		}
		if _, ok := backend.(core.DelegatedRunBackend); !ok {
			t.Fatalf("%s advertises %s but backend %T is not DelegatedRunBackend", name, core.FeatureRunSession, backend)
		}
		if err := core.ValidateRunSessionForSpec(spec, core.RunResult{Session: &core.RunSessionHandle{
			Provider:       name,
			LeaseID:        "cbx_test",
			CleanupCommand: "crabbox stop --provider " + name + " cbx_test",
		}}); err != nil {
			t.Fatalf("%s rejects a valid %s handle: %v", name, core.FeatureRunSession, err)
		}
		checked++
	}
	if checked == 0 {
		t.Fatalf("no providers advertised %s; conformance test is stale", core.FeatureRunSession)
	}
}

func TestInteractiveLeaseFeaturesRequireSSHLeaseBackends(t *testing.T) {
	counts := map[core.Feature]int{
		core.FeatureDesktop: 0,
		core.FeatureBrowser: 0,
		core.FeatureCode:    0,
	}
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		spec := provider.Spec()
		features := []core.Feature{core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode}
		if !hasAnyFeature(spec.Features, features...) {
			continue
		}
		if spec.Kind != core.ProviderKindSSHLease || !spec.Features.Has(core.FeatureSSH) {
			t.Fatalf("%s advertises interactive lease features %v but kind=%s features=%v", name, features, spec.Kind, spec.Features)
		}
		cfg, ok := offlineConformanceConfig(name)
		if !ok {
			t.Fatalf("%s advertises interactive lease features %v; add an offline conformance config for it", name, spec.Features)
		}
		backend, err := provider.Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
		if err != nil {
			t.Fatalf("%s configure error: %v", name, err)
		}
		if _, ok := backend.(core.SSHLeaseBackend); !ok {
			t.Fatalf("%s advertises interactive lease features %v but backend %T is not SSHLeaseBackend", name, spec.Features, backend)
		}
		for _, feature := range features {
			if spec.Features.Has(feature) {
				counts[feature]++
			}
		}
	}
	for _, feature := range []core.Feature{core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode} {
		if counts[feature] == 0 {
			t.Fatalf("no providers advertised %s; conformance test is stale", feature)
		}
	}
}

func TestCleanupFeatureRequiresCleanupBackend(t *testing.T) {
	checked := 0
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		if !provider.Spec().Features.Has(core.FeatureCleanup) {
			continue
		}
		cfg, ok := offlineConformanceConfig(name)
		if !ok {
			t.Fatalf("%s advertises %s; add an offline conformance config for it", name, core.FeatureCleanup)
		}
		backend, err := provider.Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
		if err != nil {
			t.Fatalf("%s configure error: %v", name, err)
		}
		if _, ok := backend.(core.CleanupBackend); !ok {
			t.Fatalf("%s advertises %s but backend %T is not CleanupBackend", name, core.FeatureCleanup, backend)
		}
		checked++
	}
	if checked == 0 {
		t.Fatalf("no providers advertised %s; conformance test is stale", core.FeatureCleanup)
	}
}

func TestCacheVolumeFeatureGatesRequiredCacheVolumes(t *testing.T) {
	checked := 0
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		cfg := core.BaseConfig()
		cfg.Provider = name
		cfg.Cache.Volumes = []core.CacheVolumeConfig{{
			Name:     "pnpm",
			Key:      "repo-linux-node24-lock",
			Path:     "/var/cache/crabbox/pnpm",
			Required: true,
		}}
		err := core.ValidateCacheVolumesForProvider(cfg)
		if provider.Spec().Features.Has(core.FeatureCacheVolume) {
			if err != nil {
				t.Fatalf("%s advertises %s but rejects required cache volume: %v", name, core.FeatureCacheVolume, err)
			}
			checked++
			continue
		}
		if err == nil {
			t.Fatalf("%s accepts required cache volume without %s", name, core.FeatureCacheVolume)
		}
	}
	if checked == 0 {
		t.Fatalf("no providers advertised %s; conformance test is stale", core.FeatureCacheVolume)
	}
}

func TestDirectTailscaleFeatureRequiresMetadataBackend(t *testing.T) {
	checked := 0
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		spec := provider.Spec()
		if !spec.Features.Has(core.FeatureTailscale) || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
			continue
		}
		cfg, ok := offlineConformanceConfig(name)
		if !ok {
			t.Fatalf("%s advertises direct %s; add an offline conformance config for it", name, core.FeatureTailscale)
		}
		backend, err := provider.Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
		if err != nil {
			t.Fatalf("%s configure error: %v", name, err)
		}
		if _, ok := backend.(core.TailscaleMetadataBackend); !ok {
			t.Fatalf("%s advertises direct %s but backend %T is not TailscaleMetadataBackend", name, core.FeatureTailscale, backend)
		}
		checked++
	}
	if checked == 0 {
		t.Fatalf("no direct SSH-lease providers advertised %s; conformance test is stale", core.FeatureTailscale)
	}
}

func TestSSHFeatureRequiresSSHLoginBackend(t *testing.T) {
	checked := 0
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		spec := provider.Spec()
		if !spec.Features.Has(core.FeatureSSH) {
			continue
		}
		cfg, ok := offlineConformanceConfig(name)
		if !ok {
			t.Fatalf("%s advertises %s; add an offline conformance config for it", name, core.FeatureSSH)
		}
		backend, err := provider.Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
		if err != nil {
			t.Fatalf("%s configure error: %v", name, err)
		}
		if _, ok := backend.(core.SSHLoginBackend); !ok {
			t.Fatalf("%s advertises %s but backend %T is not SSHLoginBackend", name, core.FeatureSSH, backend)
		}
		checked++
	}
	if checked == 0 {
		t.Fatalf("no providers advertised %s; conformance test is stale", core.FeatureSSH)
	}
}

func TestCrabboxSyncFeatureRequiresSSHLeaseBackend(t *testing.T) {
	checked := 0
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		spec := provider.Spec()
		if !spec.Features.Has(core.FeatureCrabboxSync) {
			continue
		}
		cfg, ok := offlineConformanceConfig(name)
		if !ok {
			t.Fatalf("%s advertises %s; add an offline conformance config for it", name, core.FeatureCrabboxSync)
		}
		backend, err := provider.Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
		if err != nil {
			t.Fatalf("%s configure error: %v", name, err)
		}
		if _, ok := backend.(core.SSHLeaseBackend); !ok {
			t.Fatalf("%s advertises %s but backend %T is not SSHLeaseBackend", name, core.FeatureCrabboxSync, backend)
		}
		checked++
	}
	if checked == 0 {
		t.Fatalf("no providers advertised %s; conformance test is stale", core.FeatureCrabboxSync)
	}
}

func TestPauseResumeFeatureRequiresPausableBackend(t *testing.T) {
	checked := 0
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		if !provider.Spec().Features.Has(core.FeaturePauseResume) {
			continue
		}
		cfg, ok := offlineConformanceConfig(name)
		if !ok {
			t.Fatalf("%s advertises %s; add an offline conformance config for it", name, core.FeaturePauseResume)
		}
		backend, err := provider.Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
		if err != nil {
			t.Fatalf("%s configure error: %v", name, err)
		}
		if _, ok := backend.(core.PausableBackend); !ok {
			t.Fatalf("%s advertises %s but backend %T is not PausableBackend", name, core.FeaturePauseResume, backend)
		}
		checked++
	}
	if checked == 0 {
		t.Fatalf("no providers advertised %s; conformance test is stale", core.FeaturePauseResume)
	}
}

func TestWorkspaceFeaturesRequireNativeCheckpointProvider(t *testing.T) {
	workspaceFeatures := []core.Feature{
		core.FeatureCheckpoint,
		core.FeatureFork,
		core.FeatureRestore,
		core.FeatureSnapshot,
	}
	counts := make(map[core.Feature]int, len(workspaceFeatures))
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		spec := provider.Spec()
		if !hasAnyFeature(spec.Features, workspaceFeatures...) {
			continue
		}
		if _, ok := provider.(core.NativeCheckpointProvider); !ok {
			t.Fatalf("%s advertises workspace features %v but does not implement NativeCheckpointProvider", name, spec.Features)
		}
		if spec.Features.Has(core.FeatureFork) {
			if _, ok := provider.(core.NativeCheckpointForkProvider); !ok {
				t.Fatalf("%s advertises %s but does not implement NativeCheckpointForkProvider", name, core.FeatureFork)
			}
		}
		for _, feature := range workspaceFeatures {
			if spec.Features.Has(feature) {
				counts[feature]++
			}
		}
	}
	for _, feature := range workspaceFeatures {
		if counts[feature] == 0 {
			t.Fatalf("no providers advertised %s; conformance test is stale", feature)
		}
	}
}

func TestRunEvidenceFeaturesRequireDelegatedBackends(t *testing.T) {
	artifactProviders := 0
	downloadProviders := 0
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		spec := provider.Spec()
		if !hasAnyFeature(spec.Features, core.FeatureRunArtifacts, core.FeatureRunDownloads) {
			continue
		}
		if spec.Kind != core.ProviderKindDelegatedRun {
			t.Fatalf("%s advertises delegated run evidence features %v but has kind=%s", name, spec.Features, spec.Kind)
		}
		cfg, ok := offlineConformanceConfig(name)
		if !ok {
			t.Fatalf("%s advertises delegated run evidence features %v; add an offline conformance config for it", name, spec.Features)
		}
		backend, err := provider.Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
		if err != nil {
			t.Fatalf("%s configure error: %v", name, err)
		}
		if spec.Features.Has(core.FeatureRunArtifacts) {
			if _, ok := backend.(core.DelegatedRunArtifactBackend); !ok {
				t.Fatalf("%s advertises %s but backend %T is not DelegatedRunArtifactBackend", name, core.FeatureRunArtifacts, backend)
			}
			artifactProviders++
		}
		if spec.Features.Has(core.FeatureRunDownloads) {
			if _, ok := backend.(core.DelegatedRunDownloadBackend); !ok {
				t.Fatalf("%s advertises %s but backend %T is not DelegatedRunDownloadBackend", name, core.FeatureRunDownloads, backend)
			}
			downloadProviders++
		}
	}
	if artifactProviders == 0 {
		t.Fatalf("no providers advertised %s; conformance test is stale", core.FeatureRunArtifacts)
	}
	if downloadProviders == 0 {
		t.Fatalf("no providers advertised %s; conformance test is stale", core.FeatureRunDownloads)
	}
}

func TestRunEvidenceFeaturesGateDelegatedOptions(t *testing.T) {
	artifactProviders := 0
	downloadProviders := 0
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		spec := provider.Spec()
		if spec.Kind != core.ProviderKindDelegatedRun {
			continue
		}
		artifactErr := core.RejectDelegatedSyncOptionsForSpec(spec, core.RunRequest{
			ArtifactGlobs: []string{"reports/**"},
		})
		if spec.Features.Has(core.FeatureRunArtifacts) {
			if artifactErr != nil {
				t.Fatalf("%s advertises %s but rejects --artifact-glob: %v", name, core.FeatureRunArtifacts, artifactErr)
			}
			artifactProviders++
		} else if artifactErr == nil {
			t.Fatalf("%s accepts --artifact-glob without %s", name, core.FeatureRunArtifacts)
		}

		downloadErr := core.RejectDelegatedSyncOptionsForSpec(spec, core.RunRequest{
			Downloads: []string{"reports/result.json=result.json"},
		})
		if spec.Features.Has(core.FeatureRunDownloads) {
			if downloadErr != nil {
				t.Fatalf("%s advertises %s but rejects --download: %v", name, core.FeatureRunDownloads, downloadErr)
			}
			downloadProviders++
		} else if downloadErr == nil {
			t.Fatalf("%s accepts --download without %s", name, core.FeatureRunDownloads)
		}

		requiredErr := core.RejectDelegatedSyncOptionsForSpec(spec, core.RunRequest{
			RequiredArtifactGlobs: []string{"reports/result.json"},
		})
		if hasAnyFeature(spec.Features, core.FeatureRunArtifacts, core.FeatureRunDownloads) {
			if requiredErr != nil {
				t.Fatalf("%s advertises run evidence features %v but rejects --require-artifact: %v", name, spec.Features, requiredErr)
			}
		} else if requiredErr == nil {
			t.Fatalf("%s accepts --require-artifact without run evidence features", name)
		}
	}
	if artifactProviders == 0 {
		t.Fatalf("no providers advertised %s; conformance test is stale", core.FeatureRunArtifacts)
	}
	if downloadProviders == 0 {
		t.Fatalf("no providers advertised %s; conformance test is stale", core.FeatureRunDownloads)
	}
}

func TestURLBridgeFeatureRequiresBridgeProvider(t *testing.T) {
	checked := 0
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		if !provider.Spec().Features.Has(core.FeatureURLBridge) {
			continue
		}
		cfg, ok := offlineConformanceConfig(name)
		if !ok {
			t.Fatalf("%s advertises %s; add an offline conformance config for it", name, core.FeatureURLBridge)
		}
		backend, err := provider.Configure(cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard})
		if err != nil {
			t.Fatalf("%s configure error: %v", name, err)
		}
		if _, ok := backend.(core.BridgeProvider); !ok {
			t.Fatalf("%s advertises %s but backend %T is not BridgeProvider", name, core.FeatureURLBridge, backend)
		}
		checked++
	}
	if checked == 0 {
		t.Fatalf("no providers advertised %s; conformance test is stale", core.FeatureURLBridge)
	}
}

func TestMCPFeatureRequiresCreateTimePassthrough(t *testing.T) {
	checked := 0
	for _, name := range allBuiltInProviderNames() {
		provider := mustProvider(t, name)
		if !provider.Spec().Features.Has(core.FeatureMCP) {
			continue
		}
		if name != "docker-sandbox" {
			t.Fatalf("%s advertises %s; add an offline conformance check for its MCP attachment contract", name, core.FeatureMCP)
		}
		checked++
	}
	if checked == 0 {
		t.Fatalf("no providers advertised %s; conformance test is stale", core.FeatureMCP)
	}
}

func hasAnyFeature(features core.FeatureSet, wants ...core.Feature) bool {
	for _, want := range wants {
		if features.Has(want) {
			return true
		}
	}
	return false
}

func offlineConformanceConfig(provider string) (core.Config, bool) {
	cfg := core.BaseConfig()
	cfg.Provider = provider
	switch provider {
	case "agent-sandbox":
		cfg.AgentSandbox.Context = "agent-context"
		cfg.AgentSandbox.WarmPool = "linux-pool"
		return cfg, true
	case "blacksmith-testbox":
		return cfg, true
	case "cloudflare-dynamic-workers":
		cfg.CloudflareDynamicWorkers.LoaderURL = "https://loader.example.test"
		cfg.CloudflareDynamicWorkers.Token = "test-token"
		return cfg, true
	case "cloudflare-sandbox":
		cfg.CloudflareSandbox.BridgeURL = "https://bridge.example.test"
		cfg.CloudflareSandbox.Token = "test-token"
		return cfg, true
	case "e2b":
		return cfg, true
	case "external":
		cfg.External.Command = "external-provider"
		return cfg, true
	case "codesandbox":
		return cfg, true
	case "hyperv":
		cfg.TargetOS = core.TargetWindows
		return cfg, true
	case "kubevirt":
		cfg.KubeVirt.Context = "agent-context"
		return cfg, true
	case "mxc":
		cfg.TargetOS = core.TargetWindows
		return cfg, true
	case "islo":
		return cfg, true
	case "railway":
		return cfg, true
	case "semaphore":
		cfg.Semaphore.Host = "semaphore.example.test"
		cfg.Semaphore.Token = "test-token"
		return cfg, true
	case "sprites":
		cfg.Sprites.Token = "test-token"
		return cfg, true
	case "windows-sandbox":
		cfg.TargetOS = core.TargetWindows
		cfg.WindowsMode = core.WindowsModeNormal
		return cfg, true
	default:
		return cfg, true
	}
}

func allBuiltInProviderNames() []string {
	return []string{
		"agent-sandbox",
		"apple-container",
		"apple-machine",
		"apple-vz",
		"ascii-box",
		"aws",
		"azure",
		"azure-dynamic-sessions",
		"blaxel",
		"blacksmith-testbox",
		"cloudflare",
		"cloudflare-dynamic-workers",
		"cloudflare-sandbox",
		"codesandbox",
		"daytona",
		"digitalocean",
		"docker-sandbox",
		"e2b",
		"exe-dev",
		"external",
		"fastapi-cloud",
		"freestyle",
		"gcp",
		"hetzner",
		"hostinger",
		"hyperv",
		"incus",
		"islo",
		"kubevirt",
		"linode",
		"local-container",
		"modal",
		"morph",
		"multipass",
		"mxc",
		"namespace-devbox",
		"namespace-instance",
		"nebius",
		"nvidia-brev",
		"opencomputer",
		"opensandbox",
		"ovh",
		"parallels",
		"phala",
		"proxmox",
		"railway",
		"runpod",
		"scaleway",
		"anthropic-sandbox-runtime",
		"semaphore",
		"smolvm",
		"sprites",
		"ssh",
		"superserve",
		"tart",
		"tenki",
		"tensorlake",
		"upstash-box",
		"vercel-sandbox",
		"vultr",
		"wandb",
		"windows-sandbox",
		"xcp-ng",
	}
}

func mustProvider(t *testing.T, name string) core.Provider {
	t.Helper()
	provider, err := core.ProviderFor(name)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}
