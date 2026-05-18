package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func (a App) doctor(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("doctor", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpAll())
	profile := fs.String("profile", defaults.Profile, "configured profile for remote prerequisite checks")
	id := fs.String("id", "", "remote lease id to inspect")
	targetFlags := registerTargetFlags(fs, defaults)
	providerFlags := registerProviderFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Profile = strings.TrimSpace(*profile)
	if err := applySelectedProfileConfig(&cfg); err != nil {
		return err
	}
	ok := true
	if problem := configFilePermissionProblem(writableConfigPath()); problem != "" {
		fmt.Fprintf(a.Stdout, "failed  config   %s: %s\n", writableConfigPath(), problem)
		ok = false
	} else if path := writableConfigPath(); path != "" {
		if _, err := os.Stat(path); err == nil {
			fmt.Fprintf(a.Stdout, "ok      config   %s permissions=0600\n", path)
		}
	}
	cfg.Provider = *provider
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	providerDef, err := ProviderFor(cfg.Provider)
	if err != nil {
		return err
	}
	for _, tool := range doctorLocalTools(providerDef.Spec()) {
		path, err := exec.LookPath(tool)
		if err != nil {
			fmt.Fprintf(a.Stdout, "missing %-8s\n", tool)
			ok = false
			continue
		}
		fmt.Fprintf(a.Stdout, "ok      %-8s %s\n", tool, path)
	}
	if *id != "" {
		_, target, leaseID, err := a.resolveLeaseTarget(ctx, cfg, *id)
		if err != nil {
			return err
		}
		remote := "printf 'git='; git --version; printf 'rsync='; rsync --version | head -1; printf 'curl='; curl --version | head -1; printf 'jq='; jq --version"
		if cfg.Profiles[cfg.Profile].Doctor.Enabled {
			if isWindowsNativeTarget(target) {
				return exit(2, "profile doctor is not supported for native Windows targets")
			}
			remote = remoteProfileDoctorCommand(cfg.Profile, cfg.Profiles[cfg.Profile].Doctor, profileDoctorWorkdirForLease(cfg, leaseID))
		}
		if isWindowsNativeTarget(target) {
			remote = windowsRemoteDoctor()
		}
		out, err := runSSHCombinedOutput(ctx, target, remote)
		if err != nil {
			if strings.TrimSpace(out) != "" {
				fmt.Fprintf(a.Stdout, "failed  remote  %s\n%s\n", *id, strings.TrimSpace(out))
			}
			return exit(7, "remote doctor failed for %s: %v", *id, err)
		}
		fmt.Fprintf(a.Stdout, "ok      remote  %s\n%s\n", *id, out)
	}
	if os.Getenv("CRABBOX_SERVER_TYPE") == "" {
		applyServerTypeFlagOverrides(&cfg, fs, "")
	}
	useCoordinator := false
	if shouldUseCoordinator(cfg, providerDef.Spec()) {
		if coord, coordinatorConfigured, err := newTargetCoordinatorClient(cfg); err != nil {
			fmt.Fprintf(a.Stdout, "failed  coord    %v\n", err)
			ok = false
		} else if coordinatorConfigured {
			useCoordinator = true
			if err := coord.Health(ctx); err != nil {
				fmt.Fprintf(a.Stdout, "failed  coord    %v\n", err)
				ok = false
			} else {
				fmt.Fprintf(a.Stdout, "ok      coord    %s access=%s\n", cfg.Coordinator, accessAuthState(cfg.Access))
				if whoami, err := coord.Whoami(ctx); err != nil {
					fmt.Fprintf(a.Stdout, "failed  broker   %v\n", err)
					ok = false
				} else {
					fmt.Fprintf(a.Stdout, "ok      broker   auth=%s owner=%s org=%s default_type=%s\n", whoami.Auth, whoami.Owner, whoami.Org, cfg.ServerType)
				}
				if coordinatorProviderReadinessSupported(cfg.Provider) {
					readiness, err := coord.ProviderReadiness(ctx, cfg.Provider)
					if err == nil {
						if readiness.Configured {
							fmt.Fprintf(a.Stdout, "ok      provider provider=%s coordinator_secrets=ready\n", readiness.Provider)
						} else {
							fmt.Fprintf(a.Stdout, "failed  provider provider=%s missing=%s\n", readiness.Provider, strings.Join(readiness.Missing, ","))
							ok = false
						}
					} else if !isCoordinatorNotFoundError(err) {
						fmt.Fprintf(a.Stdout, "failed  provider %v\n", err)
						ok = false
					}
				}
				if cfg.CoordAdminToken != "" {
					adminCfg := cfg
					adminCfg.CoordToken = cfg.CoordAdminToken
					adminCoord, _, err := newCoordinatorClient(adminCfg)
					if err != nil {
						return err
					}
					if machines, err := adminCoord.Pool(ctx, cfg); err != nil {
						if isCoordinatorUnauthorized(err) {
							fmt.Fprintf(a.Stdout, "warning admin    pool list unauthorized; user broker checks still passed\n")
						} else {
							fmt.Fprintf(a.Stdout, "failed  admin    %v\n", err)
							ok = false
						}
					} else {
						fmt.Fprintf(a.Stdout, "ok      admin    provider=%s machines=%d\n", cfg.Provider, len(machines))
					}
				}
			}
		}
	}

	if os.Getenv("CRABBOX_SSH_KEY") != "" {
		if _, err := os.Stat(cfg.SSHKey); err != nil {
			fmt.Fprintf(a.Stdout, "missing ssh key %s\n", cfg.SSHKey)
			ok = false
		} else if _, err := publicKeyFor(cfg.SSHKey); err != nil {
			fmt.Fprintf(a.Stdout, "missing ssh public key %s.pub\n", cfg.SSHKey)
			ok = false
		} else {
			fmt.Fprintf(a.Stdout, "ok      ssh-key  %s\n", cfg.SSHKey)
		}
	} else {
		fmt.Fprintf(a.Stdout, "ok      ssh-key  per-lease\n")
	}

	if useCoordinator {
		if !ok {
			return exit(1, "doctor found problems")
		}
		return nil
	}

	doctorProvider, doctorSupported := providerDef.(DoctorProvider)
	if doctorSupported {
		doctor, err := doctorProvider.ConfigureDoctor(cfg, runtimeForApp(a))
		if err != nil {
			fmt.Fprintf(a.Stdout, "failed  provider provider=%s %v\n", providerDef.Name(), err)
			ok = false
		} else {
			result, err := doctor.Doctor(ctx, DoctorRequest{})
			if err != nil {
				fmt.Fprintf(a.Stdout, "failed  provider provider=%s %v\n", doctor.Spec().Name, err)
				ok = false
			} else {
				fmt.Fprintf(a.Stdout, "ok      provider provider=%s %s\n", result.Provider, result.Message)
			}
		}
		if !ok {
			return exit(1, "doctor found problems")
		}
		return nil
	}

	if providerDef.Spec().Kind == ProviderKindDelegatedRun {
		if !ok {
			return exit(1, "doctor found problems")
		}
		if !doctorSupported {
			fmt.Fprintf(a.Stdout, "skip    provider provider=%s direct_doctor=unsupported\n", providerDef.Name())
		}
		return nil
	}

	if !ok {
		return exit(1, "doctor found problems")
	}
	fmt.Fprintf(a.Stdout, "skip    provider provider=%s direct_doctor=unsupported\n", providerDef.Name())
	return nil
}

func doctorLocalTools(spec ProviderSpec) []string {
	tools := []string{"git"}
	if spec.Kind == ProviderKindSSHLease || spec.Features.Has(FeatureSSH) {
		tools = append(tools, "ssh", "ssh-keygen")
	}
	if spec.Features.Has(FeatureCrabboxSync) {
		tools = append(tools, "rsync")
	}
	if spec.Features.Has(FeatureArchiveSync) || doctorProviderUsesLocalArchive(spec.Name) {
		tools = append(tools, "tar")
	}
	return tools
}

func doctorProviderUsesLocalArchive(provider string) bool {
	switch provider {
	case "daytona", "e2b", "islo", "tensorlake":
		return true
	default:
		return false
	}
}

func coordinatorProviderReadinessSupported(provider string) bool {
	p, err := ProviderFor(provider)
	return err == nil && p.Spec().Coordinator == CoordinatorSupported
}
