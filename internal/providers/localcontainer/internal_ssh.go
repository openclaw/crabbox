package localcontainer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"runtime"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	internalSSHTransport = "internal"
	sshTransportLabel    = "ssh_transport"
	sshNetworkLabel      = "ssh_network_id"
)

var dockerNetworkIdentity = regexp.MustCompile(`^[0-9a-f]{64}$`)

type inspectNetworkAttachment struct {
	NetworkID  string `json:"NetworkID"`
	EndpointID string `json:"EndpointID"`
	IPAddress  string `json:"IPAddress"`
}

type inspectInternalNetwork struct {
	ID       string            `json:"Id"`
	Name     string            `json:"Name"`
	Driver   string            `json:"Driver"`
	Internal bool              `json:"Internal"`
	Scope    string            `json:"Scope"`
	Options  map[string]string `json:"Options"`
	IPAM     struct {
		Config []struct{ Subnet, Gateway string } `json:"Config"`
	} `json:"IPAM"`
	Containers map[string]struct{ EndpointID, IPv4Address string } `json:"Containers"`
}

// Empty preserves the historical published-transport fixed-ID fingerprint.
func localContainerSSHTransport(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "", "published":
		return "", nil
	case internalSSHTransport:
		return internalSSHTransport, nil
	default:
		return "", core.Exit(2, "local-container sshTransport must be published or internal")
	}
}

func localContainerSSHNetworkArgs(cfg core.Config) ([]string, error) {
	mode, err := localContainerSSHTransport(cfg.LocalContainer.SSHTransport)
	if err != nil {
		return nil, err
	}
	if mode == internalSSHTransport {
		if !dockerNetworkIdentity.MatchString(cfg.LocalContainer.SSHNetworkID) {
			return nil, core.Exit(2, "internal SSH requires a validated network identity")
		}
		return []string{"--network", cfg.LocalContainer.SSHNetworkID}, nil
	}
	return []string{"--network", cfg.LocalContainer.Network, "-p", "127.0.0.1::" + sshPort}, nil
}

func (b *backend) configureSSHTransport(ctx context.Context, cfg *core.Config) error {
	mode, err := localContainerSSHTransport(cfg.LocalContainer.SSHTransport)
	if err != nil {
		return err
	}
	cfg.LocalContainer.SSHTransport = mode
	cfg.LocalContainer.SSHNetworkID = ""
	if mode == "" {
		return nil
	}
	network, err := b.internalSSHNetwork(ctx, *cfg, cfg.LocalContainer.Network)
	if err != nil {
		return err
	}
	cfg.LocalContainer.SSHNetworkID = network.ID
	return nil
}

func validateInternalSSHScope(scope checkpointScope, hostOS string) error {
	u, err := url.Parse(scope.Endpoint)
	if hostOS != "linux" || !isDockerRuntime(scope.Runtime) || err != nil ||
		u.Scheme != "unix" || u.Host != "" || !strings.HasPrefix(u.Path, "/") ||
		u.RawQuery != "" || u.Fragment != "" || scope.DaemonID == "" {
		return core.Exit(2, "internal SSH requires a colocated Linux Docker daemon over a Unix socket")
	}
	return nil
}

func validateInternalNetwork(network inspectInternalNetwork, reference string) (string, netip.Prefix, netip.Addr, error) {
	if !dockerNetworkIdentity.MatchString(network.ID) ||
		(reference != network.ID && reference != network.Name) ||
		!network.Internal || network.Driver != "bridge" || network.Scope != "local" ||
		network.Options["com.docker.network.bridge.gateway_mode_ipv4"] == "isolated" ||
		network.Options["com.docker.network.bridge.inhibit_ipv4"] == "true" {
		return "", netip.Prefix{}, netip.Addr{}, core.Exit(2, "internal SSH requires the exact local internal IPv4 bridge with host access")
	}
	bridge := network.Options["com.docker.network.bridge.name"]
	if bridge == "" {
		bridge = "br-" + network.ID[:12]
	}
	var prefix netip.Prefix
	var gateway netip.Addr
	for _, pool := range network.IPAM.Config {
		p, err := netip.ParsePrefix(pool.Subnet)
		if err != nil {
			return "", prefix, gateway, fmt.Errorf("invalid network subnet: %w", err)
		}
		if !p.Addr().Is4() {
			continue
		}
		g, err := netip.ParseAddr(pool.Gateway)
		if prefix.IsValid() || err != nil || !g.Is4() || !g.IsGlobalUnicast() || !p.Contains(g) {
			return "", prefix, gateway, core.Exit(2, "internal SSH requires one IPv4 subnet with a valid gateway")
		}
		prefix, gateway = p, g
	}
	if !prefix.IsValid() {
		return "", prefix, gateway, core.Exit(2, "internal SSH network has no IPv4 subnet")
	}
	return bridge, prefix, gateway, nil
}

// A Unix socket alone also describes VM-backed Docker installations. Require
// the bridge and its gateway in the controller's own network namespace.
func validateInternalBridge(addrs []net.Addr, prefix netip.Prefix, gateway netip.Addr) error {
	for _, address := range addrs {
		local, err := netip.ParsePrefix(address.String())
		if err == nil && local.Addr() == gateway && local.Masked() == prefix.Masked() {
			return nil
		}
	}
	return core.Exit(2, "internal SSH bridge gateway is not in the controller network namespace")
}

func (b *backend) internalSSHNetwork(ctx context.Context, cfg core.Config, reference string) (inspectInternalNetwork, error) {
	var network inspectInternalNetwork
	scope, err := b.captureRuntimeScope(ctx, cfg)
	if err != nil {
		return network, err
	}
	if err := validateInternalSSHScope(scope, runtime.GOOS); err != nil {
		return network, err
	}
	if reference == "" || strings.HasPrefix(reference, "-") {
		return network, core.Exit(2, "internal SSH requires an explicit network name or identity")
	}
	// Use the captured daemon, not a context that could change between commands.
	cfg.LocalContainer.CheckpointMetadata = checkpointScopeMetadata(scope)
	req := containerRuntimeRequest(cfg, []string{"network", "inspect", reference})
	req.MaxCapturedOutputBytes = 1 << 20
	result, err := b.rt.Exec.Run(ctx, req)
	if err != nil {
		return network, shared.LocalCommandError("inspect internal SSH network", result, err)
	}
	var networks []inspectInternalNetwork
	if err := json.Unmarshal([]byte(result.Stdout), &networks); err != nil {
		return network, err
	}
	if len(networks) != 1 {
		return network, core.Exit(2, "internal SSH network identity is ambiguous")
	}
	network = networks[0]
	bridge, prefix, gateway, err := validateInternalNetwork(network, reference)
	if err != nil {
		return network, err
	}
	iface, err := net.InterfaceByName(bridge)
	if err != nil {
		return network, fmt.Errorf("internal SSH bridge is not local: %w", err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return network, err
	}
	return network, validateInternalBridge(addrs, prefix, gateway)
}

func internalContainerSSHHostPort(container inspectContainer) (string, string, error) {
	networkID := container.Config.Labels[sshNetworkLabel]
	if !dockerNetworkIdentity.MatchString(networkID) || len(container.NetworkSettings.Networks) != 1 {
		return "", "", core.Exit(2, "internal SSH container must have exactly its bound network")
	}
	for _, bindings := range container.NetworkSettings.Ports {
		if len(bindings) != 0 {
			return "", "", core.Exit(2, "internal SSH container has published ports")
		}
	}
	for _, endpoint := range container.NetworkSettings.Networks {
		ip, err := netip.ParseAddr(endpoint.IPAddress)
		if endpoint.NetworkID != networkID || !dockerNetworkIdentity.MatchString(endpoint.EndpointID) ||
			err != nil || !ip.Is4() || !ip.IsGlobalUnicast() {
			return "", "", core.Exit(2, "internal SSH container endpoint is invalid or changed")
		}
		return ip.String(), sshPort, nil
	}
	return "", "", core.Exit(2, "internal SSH container endpoint is absent")
}

func validateInternalMembership(container inspectContainer, network inspectInternalNetwork) error {
	host, _, err := internalContainerSSHHostPort(container)
	if err != nil {
		return err
	}
	_, prefix, gateway, err := validateInternalNetwork(network, container.Config.Labels[sshNetworkLabel])
	if err != nil {
		return err
	}
	ip := netip.MustParseAddr(host)
	if !prefix.Contains(ip) || ip == gateway || ip == prefix.Masked().Addr() {
		return core.Exit(2, "internal SSH container address is outside its network or reserved")
	}
	member, found := network.Containers[container.ID]
	address, err := netip.ParsePrefix(member.IPv4Address)
	if !found || err != nil || address.Addr() != ip || address.Bits() != prefix.Bits() {
		return core.Exit(2, "internal SSH network does not confirm the exact container address")
	}
	for _, endpoint := range container.NetworkSettings.Networks {
		if endpoint.EndpointID != member.EndpointID {
			return core.Exit(2, "internal SSH endpoint identity changed")
		}
	}
	return nil
}

func (b *backend) validateInternalSSHContainer(ctx context.Context, cfg core.Config, container inspectContainer) error {
	network, err := b.internalSSHNetwork(ctx, cfg, container.Config.Labels[sshNetworkLabel])
	if err != nil {
		return err
	}
	return validateInternalMembership(container, network)
}

// Exact-lease release does not require SSH admission, so it remains available
// when a retained container no longer satisfies the transport boundary.
func internalSSHRecoveryError(err error, leaseID string) error {
	// The CLI renders the selected ExitError message, not an outer fmt wrapper.
	// Keep the original cause for Is/As while making the recovery visible there.
	diagnostic := core.Exit(core.ExitCodeForError(err, 1), "%s; release this lease with: crabbox stop --provider %s --id %s", err, providerName, core.ShellQuote(leaseID))
	return errors.Join(diagnostic, err)
}
