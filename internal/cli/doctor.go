package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const doctorProviderTimeout = 10 * time.Second

type doctorJSONOutput struct {
	OK       bool              `json:"ok"`
	Provider string            `json:"provider"`
	Checks   []doctorJSONCheck `json:"checks"`
}

type doctorJSONCheck struct {
	Status   string            `json:"status"`
	Check    string            `json:"check"`
	Provider string            `json:"provider,omitempty"`
	Message  string            `json:"message,omitempty"`
	Details  map[string]string `json:"details,omitempty"`
}

func (a App) doctor(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("doctor", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpAll())
	profile := fs.String("profile", defaults.Profile, "configured profile for remote prerequisite checks")
	id := fs.String("id", "", "remote lease id to inspect")
	jsonOut := fs.Bool("json", false, "print JSON")
	probeSSH := fs.Bool("doctor-probe-ssh", false, "probe static SSH reachability during doctor")
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
	var checks []doctorJSONCheck
	record := func(status, check, message string, details map[string]string) {
		if *jsonOut {
			item := doctorJSONCheck{Status: status, Check: check, Message: message, Details: details}
			if details != nil {
				item.Provider = details["provider"]
			}
			checks = append(checks, item)
			return
		}
		if message == "" {
			fmt.Fprintf(a.Stdout, "%-7s %s\n", status, check)
			return
		}
		fmt.Fprintf(a.Stdout, "%-7s %-8s %s\n", status, check, message)
	}
	finish := func() error {
		if *jsonOut {
			if err := json.NewEncoder(a.Stdout).Encode(doctorJSONOutput{OK: ok, Provider: cfg.Provider, Checks: checks}); err != nil {
				return err
			}
		}
		if !ok {
			return exit(1, "doctor found problems")
		}
		return nil
	}
	if problem := configFilePermissionProblem(writableConfigPath()); problem != "" {
		record("failed", "config", fmt.Sprintf("%s: %s", writableConfigPath(), problem), nil)
		ok = false
	} else if path := writableConfigPath(); path != "" {
		if _, err := os.Stat(path); err == nil {
			record("ok", "config", fmt.Sprintf("%s permissions=0600", path), map[string]string{"path": path, "permissions": "0600"})
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
			record("missing", tool, "", map[string]string{"tool": tool})
			ok = false
			continue
		}
		record("ok", tool, path, map[string]string{"tool": tool, "path": path})
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
			ok = false
			if strings.TrimSpace(out) != "" {
				record("failed", "remote", fmt.Sprintf("%s\n%s", *id, strings.TrimSpace(out)), map[string]string{"id": *id})
			}
			if *jsonOut {
				_ = json.NewEncoder(a.Stdout).Encode(doctorJSONOutput{OK: false, Provider: cfg.Provider, Checks: checks})
			}
			return exit(7, "remote doctor failed for %s: %v", *id, err)
		}
		record("ok", "remote", fmt.Sprintf("%s\n%s", *id, out), map[string]string{"id": *id})
	}
	if os.Getenv("CRABBOX_SERVER_TYPE") == "" {
		applyServerTypeFlagOverrides(&cfg, fs, "")
	}
	useCoordinator := false
	if shouldUseCoordinator(cfg, providerDef.Spec()) {
		if coord, coordinatorConfigured, err := newTargetCoordinatorClient(cfg); err != nil {
			record("failed", "coord", err.Error(), nil)
			ok = false
		} else if coordinatorConfigured {
			useCoordinator = true
			if err := coord.Health(ctx); err != nil {
				record("failed", "coord", err.Error(), nil)
				ok = false
			} else {
				record("ok", "coord", fmt.Sprintf("%s access=%s", cfg.Coordinator, accessAuthState(cfg.Access)), map[string]string{"url": cfg.Coordinator, "access": accessAuthState(cfg.Access)})
				if whoami, err := coord.Whoami(ctx); err != nil {
					record("failed", "broker", err.Error(), nil)
					ok = false
				} else {
					record("ok", "broker", fmt.Sprintf("auth=%s owner=%s org=%s default_type=%s", whoami.Auth, whoami.Owner, whoami.Org, cfg.ServerType), map[string]string{"auth": whoami.Auth, "owner": whoami.Owner, "org": whoami.Org, "default_type": cfg.ServerType})
				}
				if coordinatorProviderReadinessSupported(cfg.Provider) {
					readiness, err := coord.ProviderReadiness(ctx, cfg.Provider)
					if err == nil {
						if readiness.Configured {
							record("ok", "provider", fmt.Sprintf("provider=%s coordinator_secrets=ready", readiness.Provider), map[string]string{"provider": readiness.Provider, "coordinator_secrets": "ready"})
						} else {
							hint := doctorErrorHint(readiness.Provider, "config")
							record("failed", "provider", fmt.Sprintf("provider=%s missing=%s class=config hint=%s", readiness.Provider, strings.Join(readiness.Missing, ","), hint), map[string]string{"provider": readiness.Provider, "missing": strings.Join(readiness.Missing, ","), "class": "config", "hint": hint})
							ok = false
						}
					} else if !isCoordinatorNotFoundError(err) {
						class := doctorErrorClass(err)
						hint := doctorErrorHint(cfg.Provider, class)
						record("failed", "provider", fmt.Sprintf("provider=%s class=%s hint=%s %v", cfg.Provider, class, hint, err), map[string]string{"provider": cfg.Provider, "class": class, "hint": hint, "error": err.Error()})
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
							record("warning", "admin", "pool list unauthorized; user broker checks still passed", nil)
						} else {
							record("failed", "admin", err.Error(), nil)
							ok = false
						}
					} else {
						record("ok", "admin", fmt.Sprintf("provider=%s machines=%d", cfg.Provider, len(machines)), map[string]string{"provider": cfg.Provider, "machines": fmt.Sprintf("%d", len(machines))})
					}
				}
			}
		}
	}

	if os.Getenv("CRABBOX_SSH_KEY") != "" {
		if _, err := os.Stat(cfg.SSHKey); err != nil {
			record("missing", "ssh-key", cfg.SSHKey, map[string]string{"path": cfg.SSHKey})
			ok = false
		} else if _, err := publicKeyFor(cfg.SSHKey); err != nil {
			record("missing", "ssh-key", cfg.SSHKey+".pub", map[string]string{"path": cfg.SSHKey + ".pub"})
			ok = false
		} else {
			record("ok", "ssh-key", cfg.SSHKey, map[string]string{"path": cfg.SSHKey})
		}
	} else {
		record("ok", "ssh-key", "per-lease", map[string]string{"mode": "per-lease"})
	}

	if useCoordinator {
		return finish()
	}

	doctorProvider, doctorSupported := providerDef.(DoctorProvider)
	if doctorSupported {
		doctor, err := doctorProvider.ConfigureDoctor(cfg, runtimeForApp(a))
		if err != nil {
			class := doctorErrorClass(err)
			hint := doctorErrorHint(providerDef.Name(), class)
			record("failed", "provider", fmt.Sprintf("provider=%s class=%s hint=%s %v", providerDef.Name(), class, hint, err), map[string]string{"provider": providerDef.Name(), "class": class, "hint": hint, "error": err.Error()})
			ok = false
		} else {
			doctorCtx, cancel := context.WithTimeout(ctx, doctorProviderTimeout)
			result, err := doctor.Doctor(doctorCtx, DoctorRequest{ProbeSSH: *probeSSH})
			cancel()
			if err != nil {
				class := doctorErrorClass(err)
				hint := doctorErrorHint(doctor.Spec().Name, class)
				record("failed", "provider", fmt.Sprintf("provider=%s class=%s hint=%s %v", doctor.Spec().Name, class, hint, err), map[string]string{"provider": doctor.Spec().Name, "class": class, "hint": hint, "error": err.Error(), "timeout": doctorProviderTimeout.String()})
				ok = false
			} else {
				message := fmt.Sprintf("provider=%s timeout=%s %s", result.Provider, doctorProviderTimeout, result.Message)
				details := parseDoctorDetails(result.Message)
				details["provider"] = result.Provider
				details["timeout"] = doctorProviderTimeout.String()
				record("ok", "provider", message, details)
			}
		}
		return finish()
	}

	if providerDef.Spec().Kind == ProviderKindDelegatedRun {
		if !doctorSupported {
			record("skip", "provider", fmt.Sprintf("provider=%s direct_doctor=unsupported", providerDef.Name()), map[string]string{"provider": providerDef.Name(), "direct_doctor": "unsupported"})
		}
		return finish()
	}

	record("skip", "provider", fmt.Sprintf("provider=%s direct_doctor=unsupported", providerDef.Name()), map[string]string{"provider": providerDef.Name(), "direct_doctor": "unsupported"})
	return finish()
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

func parseDoctorDetails(message string) map[string]string {
	details := make(map[string]string)
	for _, field := range strings.Fields(message) {
		key, value, ok := strings.Cut(field, "=")
		if !ok || key == "" {
			continue
		}
		details[key] = value
	}
	return details
}

func doctorErrorClass(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "timed out") || strings.Contains(message, "timeout") || strings.Contains(message, "deadline"):
		return "timeout"
	case strings.Contains(message, "executable file not found") || strings.Contains(message, "not found in $path") || strings.Contains(message, "no such file"):
		return "tool"
	case strings.Contains(message, "missing") || strings.Contains(message, "required") || strings.Contains(message, "not configured") || strings.Contains(message, "empty config"):
		return "config"
	case strings.Contains(message, "unauthorized") || strings.Contains(message, "forbidden") || strings.Contains(message, "permission") || strings.Contains(message, "denied") || strings.Contains(message, "credential") || strings.Contains(message, "api key") || strings.Contains(message, "token") || strings.Contains(message, "401") || strings.Contains(message, "403"):
		return "auth"
	case strings.Contains(message, "connection refused") || strings.Contains(message, "no such host") || strings.Contains(message, "network") || strings.Contains(message, "dial") || strings.Contains(message, "tls"):
		return "network"
	default:
		return "provider"
	}
}

func doctorErrorHint(provider, class string) string {
	if class == "timeout" {
		return "retry_or_check_provider_status"
	}
	if class == "tool" {
		return "install_provider_cli"
	}
	if class == "network" {
		return "check_network_and_provider_endpoint"
	}
	switch provider {
	case "aws":
		return "check_aws_credentials_and_ec2_describe_instances"
	case "azure":
		return "check_azure_login_subscription_and_virtualmachines_read"
	case "gcp":
		return "check_gcp_project_credentials_and_compute_instances_list"
	case "hetzner":
		return "check_hcloud_token_and_servers_read"
	case "proxmox":
		return "check_proxmox_url_token_and_vm_audit"
	case "blacksmith-testbox":
		return "check_blacksmith_cli_auth_and_testbox_list"
	case "daytona":
		return "check_daytona_auth_profile_and_sandboxes_list"
	case "e2b":
		return "check_e2b_api_key_and_sandbox_list"
	case "islo":
		return "check_islo_api_key_and_sandbox_list"
	case "modal":
		return "check_modal_profile_and_sandbox_list"
	case "namespace-devbox":
		return "check_namespace_cli_auth_and_devbox_list"
	case "semaphore":
		return "check_semaphore_token_project_and_jobs_read"
	case "sprites":
		return "check_sprites_cli_auth_and_sprite_list"
	case "tensorlake":
		return "check_tensorlake_cli_auth_and_sbx_ls"
	case "cloudflare":
		return "check_cloudflare_readiness_url_and_credentials"
	case "ssh":
		return "check_static_host_user_key_and_network"
	default:
		if class == "config" {
			return "check_crabbox_provider_config"
		}
		return "check_provider_auth_and_config"
	}
}
