package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

func (a App) doctor(ctx context.Context, args []string) error {
	fs := newFlagSet("doctor", a.Stderr)
	provider := fs.String("provider", defaultConfig().Provider, "provider: hetzner, aws, or ssh")
	id := fs.String("id", "", "remote lease id to inspect")
	targetFlags := registerTargetFlags(fs, defaultConfig())
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	ok := true
	for _, tool := range []string{"git", "ssh", "ssh-keygen", "rsync", "curl"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			fmt.Fprintf(a.Stdout, "missing %-8s\n", tool)
			ok = false
			continue
		}
		fmt.Fprintf(a.Stdout, "ok      %-8s %s\n", tool, path)
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
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
	if *id != "" {
		_, target, _, err := a.resolveLeaseTarget(ctx, cfg, *id)
		if err != nil {
			return err
		}
		remote := "printf 'git='; git --version; printf 'rsync='; rsync --version | head -1; printf 'curl='; curl --version | head -1; printf 'jq='; jq --version"
		if isWindowsNativeTarget(target) {
			remote = windowsRemoteDoctor()
		}
		out, err := runSSHOutput(ctx, target, remote)
		if err != nil {
			return exit(7, "remote doctor failed for %s: %v", *id, err)
		}
		fmt.Fprintf(a.Stdout, "ok      remote  %s\n%s\n", *id, out)
	}
	if os.Getenv("CRABBOX_SERVER_TYPE") == "" {
		applyServerTypeFlagOverrides(&cfg, fs, "")
	}
	useCoordinator := false
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
			if cfg.CoordAdminToken != "" {
				adminCfg := cfg
				adminCfg.CoordToken = cfg.CoordAdminToken
				adminCoord, _, err := newCoordinatorClient(adminCfg)
				if err != nil {
					return err
				}
				if machines, err := adminCoord.Pool(ctx, cfg); err != nil {
					fmt.Fprintf(a.Stdout, "failed  admin    %v\n", err)
					ok = false
				} else {
					fmt.Fprintf(a.Stdout, "ok      admin    provider=%s machines=%d\n", cfg.Provider, len(machines))
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

	switch cfg.Provider {
	case "ssh", "static", "static-ssh":
		if cfg.Static.Host == "" {
			fmt.Fprintf(a.Stdout, "failed  static   missing static.host\n")
			ok = false
		} else {
			fmt.Fprintf(a.Stdout, "ok      static   target=%s windows_mode=%s host=%s\n", cfg.TargetOS, cfg.WindowsMode, cfg.Static.Host)
		}
	case "aws":
		client, err := newAWSClient(ctx, cfg)
		if err != nil {
			fmt.Fprintf(a.Stdout, "failed  aws      %v\n", err)
			ok = false
			break
		}
		servers, err := client.ListCrabboxServers(ctx)
		if err != nil {
			fmt.Fprintf(a.Stdout, "failed  aws      %v\n", err)
			ok = false
		} else {
			fmt.Fprintf(a.Stdout, "ok      aws      crabbox_servers=%d region=%s default_type=%s\n", len(servers), cfg.AWSRegion, cfg.ServerType)
		}
	default:
		client, err := newHetznerClient()
		if err != nil {
			fmt.Fprintf(a.Stdout, "missing hcloud token\n")
			ok = false
		} else {
			servers, err := client.ListCrabboxServers(ctx)
			if err != nil {
				fmt.Fprintf(a.Stdout, "failed  hcloud   %v\n", err)
				ok = false
			} else {
				fmt.Fprintf(a.Stdout, "ok      hcloud   crabbox_servers=%d default_type=%s\n", len(servers), cfg.ServerType)
			}
		}
	}

	if !ok {
		return exit(1, "doctor found problems")
	}
	return nil
}
