package cli

import (
	"reflect"
	"testing"
)

func TestApplyOptionalPreservesAbsentAndCopiesZero(t *testing.T) {
	flag, count, name := true, 5, "inherited"
	applyOptional(&flag, nil)
	applyOptional(&count, nil)
	applyOptional(&name, nil)
	if !flag || count != 5 || name != "inherited" {
		t.Fatal("absent values changed inherited values")
	}
	disabled, zero, empty := false, 0, ""
	applyOptional(&flag, &disabled)
	applyOptional(&count, &zero)
	applyOptional(&name, &empty)
	disabled, zero, empty = true, 9, "changed"
	if flag || count != 0 || name != "" {
		t.Fatal("explicit zero values were not copied independently")
	}
}

func TestApplyFileConfigOptionalEmptyNestedKeepsDefaults(t *testing.T) {
	cfg := defaultConfig()
	want := cfg
	file := fileConfig{
		Broker: &fileBrokerConfig{}, Firecracker: &fileFirecrackerConfig{},
		Capacity: &fileCapacityConfig{},
		Actions:  &fileActionsConfig{}, External: &fileExternalConfig{},
		Vast: &fileVastConfig{}, Cua: &fileCuaConfig{},
		OpenComputer: &fileOpenComputerConfig{}, CodeSandbox: &fileCodeSandboxConfig{},
		OpenSandbox: &fileOpenSandboxConfig{}, Nomad: &fileNomadConfig{},
		CloudflareSandbox: &fileCloudflareSandboxConfig{}, Superserve: &fileSuperserveConfig{},
		Results: &fileResultsConfig{}, Cache: &fileCacheConfig{},
	}
	if err := applyFileConfigWithTrust(&cfg, file, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatal("empty nested config changed defaults")
	}
}

func TestApplyFileConfigOptionalPopulatedThenZero(t *testing.T) {
	cfg := defaultConfig()
	flag, count, name, price := true, 4, "custom", 0.5
	file := fileConfig{
		Desktop:      &flag,
		Firecracker:  &fileFirecrackerConfig{CPUs: &count},
		Sync:         &fileSyncConfig{Delete: &flag, Checksum: &flag},
		Vast:         &fileVastConfig{MaxDphTotal: &price, MinReliability: &price},
		Cua:          &fileCuaConfig{Image: &name, Kind: &name, Region: &name, Workdir: &name},
		OpenComputer: &fileOpenComputerConfig{CPU: &count, Burst: &flag},
		Cache:        &fileCacheConfig{Pnpm: &flag, Npm: &flag, Docker: &flag, Git: &flag, PurgeOnRelease: &flag},
	}
	for _, populated := range []bool{true, false} {
		if !populated {
			flag, count, name, price = false, 0, "", 0
		}
		if err := applyFileConfigWithTrust(&cfg, file, true); err != nil {
			t.Fatal(err)
		}
		if cfg.Desktop != flag || cfg.Firecracker.CPUs != count ||
			cfg.Sync.Delete != flag || cfg.Sync.Checksum != flag ||
			cfg.Vast.MaxDphTotal != price || cfg.Vast.MinReliability != price ||
			cfg.Cua.Image != name || cfg.Cua.Kind != name || cfg.Cua.Region != name || cfg.Cua.Workdir != name ||
			cfg.OpenComputer.CPU != count || cfg.OpenComputer.Burst != flag ||
			cfg.Cache.Pnpm != flag || cfg.Cache.Npm != flag || cfg.Cache.Docker != flag ||
			cfg.Cache.Git != flag || cfg.Cache.PurgeOnRelease != flag {
			t.Fatalf("optional overlay was not applied: populated=%v", populated)
		}
	}
}

func TestApplyCloudflareSandboxOptionalTrustAndAlias(t *testing.T) {
	bridge, alias, token, workdir := "https://bridge.example.com", "https://alias.example.com", "fixture-token", ""
	disabled, zero := false, 0
	file := &fileCloudflareSandboxConfig{
		BridgeURL: &bridge, URL: &alias, Token: &token, Workdir: &workdir,
		ExecTimeoutSecs: &zero, ForgetMissing: &disabled,
	}
	for _, trusted := range []bool{false, true} {
		cfg := defaultConfig()
		cfg.CloudflareSandbox.BridgeURL = "https://inherited.example.com"
		cfg.CloudflareSandbox.Token = "inherited-token"
		cfg.CloudflareSandbox.ForgetMissing = true
		want := cfg.CloudflareSandbox
		if trusted {
			want.BridgeURL, want.Token = alias, token
		}
		want.Workdir, want.ExecTimeoutSecs, want.ForgetMissing = "", 0, false
		if err := applyCloudflareSandboxFileConfig(&cfg, file, trusted); err != nil {
			t.Fatal(err)
		}
		if cfg.CloudflareSandbox != want {
			t.Fatalf("incorrect cloudflare-sandbox overlay: trusted=%v", trusted)
		}
	}
}

func TestApplyCloudflareSandboxOptionalTimeoutOrder(t *testing.T) {
	cfg := defaultConfig()
	before := cfg.CloudflareSandbox
	workdir, negative, forget := "/workspace/custom", -1, !before.ForgetMissing
	err := applyCloudflareSandboxFileConfig(&cfg, &fileCloudflareSandboxConfig{
		Workdir: &workdir, ExecTimeoutSecs: &negative, ForgetMissing: &forget,
	}, true)
	if err == nil || err.Error() != "cloudflare-sandbox execTimeoutSecs must be non-negative" {
		t.Fatalf("unexpected timeout error: %v", err)
	}
	if cfg.CloudflareSandbox.Workdir != workdir ||
		cfg.CloudflareSandbox.ExecTimeoutSecs != before.ExecTimeoutSecs ||
		cfg.CloudflareSandbox.ForgetMissing != before.ForgetMissing {
		t.Fatal("negative timeout changed application order")
	}
}

func TestApplyProfileAndJobOptionalOverlays(t *testing.T) {
	doctor := DoctorProfileConfig{Enabled: true, RequireDocker: true, RequireCompose: true}
	preset := PresetConfig{Shell: true, Preflight: true}
	enabled := true
	job := JobConfig{
		Desktop: &enabled, Browser: &enabled, Code: &enabled, Checksum: &enabled,
		Hydrate: JobHydrateConfig{Actions: true, GitHubRunner: true},
		Shell:   true, NoSync: true, SyncOnly: true, ForceSyncLarge: true,
	}
	if !reflect.DeepEqual(applyFileDoctorProfileConfig(doctor, fileDoctorProfileConfig{}), doctor) ||
		!reflect.DeepEqual(applyFilePresetConfig(preset, filePresetConfig{}), preset) ||
		!reflect.DeepEqual(applyFileJobConfig(job, fileJobConfig{Hydrate: &fileJobHydrateConfig{}}), job) {
		t.Fatal("absent options changed inherited profile or job values")
	}
	disabled := false
	doctor = applyFileDoctorProfileConfig(doctor, fileDoctorProfileConfig{
		Enabled: &disabled, RequireDocker: &disabled, RequireCompose: &disabled,
	})
	preset = applyFilePresetConfig(preset, filePresetConfig{Shell: &disabled, Preflight: &disabled})
	job = applyFileJobConfig(job, fileJobConfig{
		Desktop: &disabled, Browser: &disabled, Code: &disabled, Checksum: &disabled,
		Hydrate: &fileJobHydrateConfig{Actions: &disabled, GitHubRunner: &disabled},
		Shell:   &disabled, NoSync: &disabled, SyncOnly: &disabled, ForceSyncLarge: &disabled,
	})
	disabled = true
	if doctor.Enabled || doctor.RequireDocker || doctor.RequireCompose || preset.Shell || preset.Preflight ||
		job.Hydrate.Actions || job.Hydrate.GitHubRunner || job.Shell || job.NoSync || job.SyncOnly || job.ForceSyncLarge {
		t.Fatal("explicit false did not override inherited values independently")
	}
	for _, value := range []*bool{job.Desktop, job.Browser, job.Code, job.Checksum} {
		if value == nil || *value || value == &enabled || value == &disabled {
			t.Fatal("job option did not retain an independent pointer copy")
		}
	}
	if job.Desktop == job.Browser || job.Desktop == job.Code || job.Desktop == job.Checksum ||
		job.Browser == job.Code || job.Browser == job.Checksum || job.Code == job.Checksum {
		t.Fatal("job pointer options share their copies")
	}
}
