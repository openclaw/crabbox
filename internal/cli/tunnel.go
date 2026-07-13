package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const tunnelLoopbackHost = "127.0.0.1"

func (a App) tunnel(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("tunnel", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpSSH())
	id := fs.String("id", "", "lease id or slug")
	port := fs.Int("port", 0, "remote loopback port")
	localPort := fs.Int("local-port", 0, "local loopback port (default: automatic)")
	jsonOut := fs.Bool("json", false, "print tunnel coordinates as JSON")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	networkFlags := registerNetworkModeFlag(fs, defaults)
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	idFlagSet := flagWasSet(fs, "id")
	setIDFromFirstArg(fs, id)
	if fs.NArg() > 1 || (idFlagSet && fs.NArg() > 0) || *port < 1 || *port > 65535 || *localPort < 0 || *localPort > 65535 {
		return exit(2, "usage: crabbox tunnel --id <lease-id-or-slug> --port <remote-port> [--local-port <local-port>] [--json]")
	}
	cfg, err := loadSSHCommandConfig(fs, *provider, providerFlags, targetFlags, networkFlags, leaseTargetConfigOptions{LeaseID: *id})
	if err != nil {
		return err
	}
	if err := requireLeaseID(*id, "crabbox tunnel --id <lease-id-or-slug> --port <remote-port>", cfg); err != nil {
		return err
	}
	lease, err := a.resolveNetworkLoginLeaseTargetForRepo(ctx, &cfg, *id, false, *reclaim, true)
	if err != nil {
		return err
	}
	if lease.SSH.AuthSecret {
		return exit(2, "crabbox tunnel does not support token-as-username SSH targets")
	}
	if err := a.claimAndTouchLeaseTarget(ctx, cfg, lease.Server, lease.SSH, lease.LeaseID, *reclaim); err != nil {
		return err
	}
	selectedLocalPort := *localPort
	if selectedLocalPort == 0 {
		selectedLocalPort, err = availableTunnelPort()
		if err != nil {
			return err
		}
	}
	stopLeaseActivity := a.startInteractiveSSHLeaseActivity(ctx, cfg, lease)
	defer stopLeaseActivity()
	return runSSHTunnel(ctx, lease.SSH, selectedLocalPort, *port, *jsonOut, a.Stdout, a.Stderr)
}

func availableTunnelPort() (int, error) {
	listener, err := net.Listen("tcp4", tunnelLoopbackHost+":0")
	if err != nil {
		return 0, exit(5, "reserve local tunnel port: %v", err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func sshTunnelArgs(target SSHTarget, localPort, remotePort int) []string {
	args := append(sshBaseArgs(target),
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "GatewayPorts=no",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "ControlPersist=no",
		"-L", fmt.Sprintf("%s:%d:%s:%d", tunnelLoopbackHost, localPort, tunnelLoopbackHost, remotePort),
		target.User+"@"+target.Host,
	)
	return args
}

func runSSHTunnel(ctx context.Context, target SSHTarget, localPort, remotePort int, jsonOut bool, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "ssh", sshTunnelArgs(target, localPort, remotePort)...)
	var details synchronizedBuffer
	cmd.Stdout = stderr
	cmd.Stderr = io.MultiWriter(stderr, &details)
	if err := cmd.Start(); err != nil {
		return exit(5, "start SSH tunnel: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case err := <-done:
			if text := strings.TrimSpace(details.String()); text != "" {
				return exit(5, "SSH tunnel exited before readiness: %s", text)
			}
			return exit(5, "SSH tunnel exited before readiness: %v", err)
		case <-deadline.C:
			_ = cmd.Process.Kill()
			<-done
			return exit(5, "timed out waiting for SSH tunnel on %s:%d", tunnelLoopbackHost, localPort)
		case <-poll.C:
			conn, err := net.DialTimeout("tcp4", net.JoinHostPort(tunnelLoopbackHost, strconv.Itoa(localPort)), 100*time.Millisecond)
			if err != nil {
				continue
			}
			_ = conn.Close()
			if jsonOut {
				if err := json.NewEncoder(stdout).Encode(map[string]int{"port": localPort, "remotePort": remotePort}); err != nil {
					_ = cmd.Process.Kill()
					<-done
					return err
				}
			} else {
				fmt.Fprintf(stdout, "%s:%d\n", tunnelLoopbackHost, localPort)
			}
			if err := <-done; err != nil && ctx.Err() == nil {
				return err
			}
			return context.Cause(ctx)
		}
	}
}
