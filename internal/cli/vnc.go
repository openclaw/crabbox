package cli

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func (a App) vnc(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("vnc", a.Stderr)
	provider := fs.String("provider", defaults.Provider, "provider: hetzner, aws, or ssh")
	id := fs.String("id", "", "lease id or slug")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	localPort := fs.String("local-port", "", "local VNC tunnel port")
	openClient := fs.Bool("open", false, "open the VNC client locally")
	hostManaged := fs.Bool("host-managed", false, "allow opening host-managed static VNC")
	managedLogin := fs.Bool("managed-login", defaults.Static.ManagedLogin, "create or reuse a Crabbox-managed static VNC login when supported")
	managedUser := fs.String("managed-user", firstNonEmpty(defaults.Static.ManagedUser, "crabbox"), "static managed VNC login user")
	targetFlags := registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" && fs.NArg() > 0 {
		*id = fs.Arg(0)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	cfg.Desktop = true
	cfg.Static.ManagedLogin = *managedLogin
	cfg.Static.ManagedUser = *managedUser
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return err
	}
	if isBlacksmithProvider(cfg.Provider) {
		return exit(2, "desktop/VNC is not supported for provider=%s; Blacksmith owns machine connectivity", cfg.Provider)
	}
	if *id == "" && !isStaticProvider(cfg.Provider) {
		return exit(2, "usage: crabbox vnc --id <lease-id-or-slug>")
	}
	if *openClient && isStaticProvider(cfg.Provider) && !*hostManaged && !cfg.Static.ManagedLogin {
		return exit(2, "static %s VNC is an existing host, not a Crabbox-created box; rerun with --host-managed only if you want to open that host's OS login prompt", cfg.TargetOS)
	}
	server, target, leaseID, err := a.resolveLeaseTarget(ctx, cfg, *id)
	if err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	if err := claimLeaseForRepoConfig(leaseID, serverSlug(server), cfg, repo.Root, cfg.IdleTimeout, *reclaim); err != nil {
		return err
	}
	a.touchActiveLeaseBestEffort(ctx, cfg, server, leaseID)
	login, err := ensureStaticManagedVNCLogin(ctx, cfg, target)
	if err != nil {
		return err
	}
	endpoint, err := resolveVNCEndpoint(ctx, cfg, target)
	if err != nil {
		return err
	}
	if *localPort == "" {
		*localPort = availableLocalVNCPort()
	}
	password := ""
	if endpoint.Managed {
		if login.User != "" {
			password = login.Password
		} else if target.TargetOS == targetLinux {
			password, _ = runSSHOutput(ctx, target, "cat "+shellQuote(vncPasswordPath))
		}
	}
	if target.TargetOS == targetLinux && !isStaticProvider(cfg.Provider) && password == "" {
		password, _ = runSSHOutput(ctx, target, "cat "+shellQuote(vncPasswordPath))
	}
	tunnel := vncTunnelCommand(target, *localPort)
	staticHostVNC := isStaticProvider(cfg.Provider) && !endpoint.Managed
	if staticHostVNC {
		fmt.Fprintf(a.Stdout, "target: static-host slug=%s provider=%s os=%s host=%s\n", blank(serverSlug(server), "-"), blank(server.Provider, cfg.Provider), blank(target.TargetOS, cfg.TargetOS), target.Host)
	} else {
		fmt.Fprintf(a.Stdout, "lease: %s slug=%s provider=%s target=%s\n", leaseID, blank(serverSlug(server), "-"), blank(server.Provider, cfg.Provider), blank(target.TargetOS, cfg.TargetOS))
	}
	if staticHostVNC {
		fmt.Fprintln(a.Stdout, "managed: false")
		fmt.Fprintln(a.Stdout, "note: this is an existing host VNC service, not a Crabbox-created box")
	} else {
		fmt.Fprintln(a.Stdout, "managed: true")
	}
	if target.TargetOS == targetLinux {
		fmt.Fprintf(a.Stdout, "display: %s\n", desktopDisplay)
	}
	if endpoint.Direct {
		fmt.Fprintln(a.Stdout, "direct vnc:")
		fmt.Fprintf(a.Stdout, "  %s:%s\n", endpoint.Host, endpoint.Port)
		directURL := fmt.Sprintf("vnc://%s:%s", endpoint.Host, endpoint.Port)
		if login.User != "" {
			directURL = fmt.Sprintf("vnc://%s@%s:%s", login.User, endpoint.Host, endpoint.Port)
		}
		fmt.Fprintf(a.Stdout, "  %s\n", directURL)
	} else {
		fmt.Fprintln(a.Stdout, "ssh tunnel:")
		fmt.Fprintf(a.Stdout, "  %s\n", tunnel)
	}
	fmt.Fprintln(a.Stdout, "vnc:")
	if endpoint.Direct {
		fmt.Fprintf(a.Stdout, "  %s:%s\n", endpoint.Host, endpoint.Port)
	} else {
		fmt.Fprintf(a.Stdout, "  localhost:%s\n", *localPort)
	}
	if strings.TrimSpace(password) != "" {
		if login.User != "" {
			fmt.Fprintf(a.Stdout, "username: %s\n", login.User)
			if login.PasswordPath != "" {
				fmt.Fprintf(a.Stdout, "password_file: %s\n", login.PasswordPath)
			}
		}
		fmt.Fprintf(a.Stdout, "password: %s\n", strings.TrimSpace(password))
	} else if staticHostVNC {
		fmt.Fprintln(a.Stdout, "credentials: host-managed")
		if target.TargetOS == targetMacOS {
			fmt.Fprintln(a.Stdout, "credential hint: use the macOS account or Screen Sharing password configured on that host")
		}
		if target.TargetOS == targetWindows {
			fmt.Fprintln(a.Stdout, "credential hint: use the Windows/VNC password configured on that host")
		}
	}
	if *openClient {
		if staticHostVNC {
			fmt.Fprintln(a.Stdout, "opening existing host VNC; expect that host's OS credential prompt")
		}
		url := fmt.Sprintf("vnc://%s:%s", endpoint.Host, endpoint.Port)
		if login.User != "" {
			url = fmt.Sprintf("vnc://%s@%s:%s", login.User, endpoint.Host, endpoint.Port)
		}
		if !endpoint.Direct {
			pid, err := startVNCTunnel(ctx, target, *localPort, endpoint.Host, endpoint.Port)
			if err != nil {
				return err
			}
			if pid > 0 {
				fmt.Fprintf(a.Stdout, "tunnel pid: %d\n", pid)
			} else {
				fmt.Fprintln(a.Stdout, "tunnel: started in background")
			}
			url = fmt.Sprintf("vnc://localhost:%s", *localPort)
			if login.User != "" {
				url = fmt.Sprintf("vnc://%s@localhost:%s", login.User, *localPort)
			}
		}
		if err := openLocalURL(url); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "opened: %s\n", url)
	}
	if endpoint.Direct {
		fmt.Fprintln(a.Stdout, "Connect directly to the printed VNC endpoint.")
	} else {
		fmt.Fprintln(a.Stdout, "Keep the tunnel process running while connected.")
	}
	return nil
}

func vncTunnelCommand(target SSHTarget, localPort string) string {
	return strings.Join(shellWords(append([]string{"ssh"}, vncTunnelArgs(target, localPort, "127.0.0.1", managedVNCPort)...)), " ")
}

func startVNCTunnel(ctx context.Context, target SSHTarget, localPort, remoteHost, remotePort string) (int, error) {
	cmd := exec.Command("ssh", vncTunnelBackgroundArgs(target, localPort, remoteHost, remotePort)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.TrimSpace(string(out)) != "" {
			return 0, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		return 0, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return 0, context.Cause(ctx)
		}
		if tcpReachable(ctx, "127.0.0.1", localPort, 200*time.Millisecond) {
			return 0, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, exit(5, "timed out starting VNC SSH tunnel on localhost:%s", localPort)
}

func vncTunnelArgs(target SSHTarget, localPort, remoteHost, remotePort string) []string {
	return []string{
		"-i", target.Key,
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + sshConfigFileValue(knownHostsFile(target)),
		"-o", "ConnectTimeout=10",
		"-o", "ConnectionAttempts=1",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=2",
		"-p", target.Port,
		"-N",
		"-L", fmt.Sprintf("%s:%s:%s", localPort, remoteHost, remotePort),
		target.User + "@" + target.Host,
	}
}

func vncTunnelBackgroundArgs(target SSHTarget, localPort, remoteHost, remotePort string) []string {
	args := vncTunnelArgs(target, localPort, remoteHost, remotePort)
	return append([]string{"-f"}, args...)
}

func openLocalURL(url string) error {
	name, args := openURLCommand(url)
	if name == "" {
		return exit(2, "opening VNC URLs is not supported on this local OS")
	}
	return exec.Command(name, args...).Start()
}

func openURLCommand(url string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "open", []string{url}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "linux":
		return "xdg-open", []string{url}
	default:
		return "", nil
	}
}
