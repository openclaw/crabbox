package cli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	vncLoopbackHost                     = "127.0.0.1"
	vncTunnelSSHConnectTimeout          = 10 * time.Second
	vncTunnelListenerVerificationWindow = 5 * time.Second
)

func vncTunnelReadinessTimeout() time.Duration {
	return vncTunnelSSHConnectTimeout + vncTunnelListenerVerificationWindow
}

func (a App) vnc(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("vnc", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	localPort := fs.String("local-port", "", "local VNC tunnel port")
	openClient := fs.Bool("open", false, "open the VNC client locally")
	hostManaged := fs.Bool("host-managed", false, "allow opening host-managed static VNC")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id, Desktop: true})
	if err != nil {
		return err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	if isBlacksmithProvider(cfg.Provider) {
		return exit(2, "desktop/VNC is not supported for provider=%s; Blacksmith owns machine connectivity", cfg.Provider)
	}
	if err := requireLeaseID(*id, "crabbox vnc --id <lease-id-or-slug>", cfg); err != nil {
		return err
	}
	if *openClient && isStaticProvider(cfg.Provider) && !*hostManaged {
		return exit(2, "static %s VNC is an existing host, not a Crabbox-created box; rerun with --host-managed only if you want to open that host's OS login prompt", cfg.TargetOS)
	}
	server, target, leaseID, err := a.resolveNetworkLeaseTargetForRepo(ctx, cfg, *id, true, *reclaim)
	if err != nil {
		return err
	}
	if err := enforceManagedLeaseCapabilities(cfg, server, leaseID); err != nil {
		return err
	}
	if err := a.claimAndTouchLeaseTarget(ctx, cfg, server, target, leaseID, *reclaim); err != nil {
		return err
	}
	endpoint, err := resolveVNCEndpoint(ctx, cfg, &target)
	if err != nil {
		return err
	}
	if *localPort == "" {
		*localPort = availableLocalVNCPort()
	}
	password := ""
	if endpoint.Managed {
		password, _ = runSSHOutput(ctx, target, vncPasswordCommand(target))
	}
	if !isStaticProvider(cfg.Provider) && password == "" {
		password, _ = runSSHOutput(ctx, target, vncPasswordCommand(target))
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
		fmt.Fprintf(a.Stdout, "  vnc://%s:%s\n", endpoint.Host, endpoint.Port)
	} else {
		fmt.Fprintln(a.Stdout, "ssh tunnel:")
		fmt.Fprintf(a.Stdout, "  %s\n", tunnel)
	}
	fmt.Fprintln(a.Stdout, "vnc:")
	if endpoint.Direct {
		fmt.Fprintf(a.Stdout, "  %s:%s\n", endpoint.Host, endpoint.Port)
	} else {
		fmt.Fprintf(a.Stdout, "  %s:%s\n", vncLoopbackHost, *localPort)
	}
	if strings.TrimSpace(password) != "" {
		fmt.Fprintf(a.Stdout, "password: %s\n", strings.TrimSpace(password))
		if endpoint.Managed && target.TargetOS == targetWindows {
			fmt.Fprintf(a.Stdout, "windows username: %s\n", target.User)
			fmt.Fprintf(a.Stdout, "windows password: %s\n", strings.TrimSpace(password))
		}
		if endpoint.Managed && target.TargetOS == targetMacOS {
			fmt.Fprintf(a.Stdout, "macos username: %s\n", target.User)
			fmt.Fprintf(a.Stdout, "macos password: %s\n", strings.TrimSpace(password))
		}
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
			url = fmt.Sprintf("vnc://%s:%s", vncLoopbackHost, *localPort)
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
	cmd := exec.Command("ssh", vncTunnelArgs(target, localPort, remoteHost, remotePort)...)
	configureDaemonCommand(cmd)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	deadline := time.Now().Add(vncTunnelReadinessTimeout())
	var listenerErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			_ = stopDaemonProcess(cmd.Process, cmd.Process.Pid)
			_ = cmd.Wait()
			return 0, context.Cause(ctx)
		}
		if ready, err := startedTunnelListenerReady(ctx, localPort, cmd.Process.Pid); ready {
			pid := cmd.Process.Pid
			if err := cmd.Process.Release(); err != nil {
				_ = stopDaemonProcess(cmd.Process, pid)
				_ = cmd.Wait()
				return 0, err
			}
			return pid, nil
		} else {
			listenerErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = stopDaemonProcess(cmd.Process, cmd.Process.Pid)
	_ = cmd.Wait()
	if text := strings.TrimSpace(output.String()); text != "" {
		return 0, exit(5, "start VNC SSH tunnel on 127.0.0.1:%s: %s", localPort, text)
	}
	if listenerErr != nil {
		return 0, exit(5, "verify VNC SSH tunnel listener on %s:%s: %v", vncLoopbackHost, localPort, listenerErr)
	}
	return 0, exit(5, "timed out starting VNC SSH tunnel on %s:%s", vncLoopbackHost, localPort)
}

func vncTunnelArgs(target SSHTarget, localPort, remoteHost, remotePort string) []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + sshConfigFileValue(knownHostsFile(target)),
		"-o", "ConnectTimeout=" + strconv.Itoa(int(vncTunnelSSHConnectTimeout/time.Second)),
		"-o", "ConnectionAttempts=1",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "GatewayPorts=no",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "ControlPersist=no",
		"-o", "ForkAfterAuthentication=no",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=2",
		"-p", target.Port,
	}
	if target.Key != "" {
		args = append([]string{"-i", target.Key, "-o", "IdentitiesOnly=yes"}, args...)
	}
	if target.ProxyCommand != "" {
		args = append(args, "-o", "ProxyCommand="+target.ProxyCommand)
	}
	args = append(args,
		"-N",
		"-L", fmt.Sprintf("%s:%s:%s:%s", vncLoopbackHost, localPort, remoteHost, remotePort),
		target.User+"@"+target.Host,
	)
	return args
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
