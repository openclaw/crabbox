package localcontainer

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net"
	"os"
	"slices"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestInternalSSHResolveAlwaysReadmitsDaemon(t *testing.T) {
	for _, label := range []string{"internal", " internal "} {
		t.Run(label, func(t *testing.T) {
			container, _ := internalSSHFixture(t)
			container.Config.Labels[sshTransportLabel] = label
			b := testBackend(&recordingRunner{})
			denied := errors.New("daemon admission denied")
			called := false
			b.captureRuntimeScope = func(context.Context, core.Config) (checkpointScope, error) {
				called = true
				return checkpointScope{}, denied
			}
			_, err := b.prepareLease(t.Context(), b.configForRun(), container, "cbx_fixture", "fixture", false)
			if !called || !errors.Is(err, denied) {
				t.Fatalf("SSH bypassed daemon admission: called=%t error=%v", called, err)
			}
		})
	}
}

func internalSSHFixture(t *testing.T) (inspectContainer, inspectInternalNetwork) {
	t.Helper()
	cid, nid, eid := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	var container inspectContainer
	var network inspectInternalNetwork
	c := `{"Id":"CID","Config":{"Labels":{"ssh_transport":"internal","ssh_network_id":"NID"}},"NetworkSettings":{"Ports":{"2222/tcp":null},"Networks":{"private":{"NetworkID":"NID","EndpointID":"EID","IPAddress":"172.30.0.2"}}}}`
	n := `{"Id":"NID","Name":"private","Driver":"bridge","Scope":"local","Internal":true,"Options":{},"IPAM":{"Config":[{"Subnet":"172.30.0.0/24","Gateway":"172.30.0.1"}]},"Containers":{"CID":{"EndpointID":"EID","IPv4Address":"172.30.0.2/24"}}}`
	replace := strings.NewReplacer("CID", cid, "NID", nid, "EID", eid)
	if err := json.Unmarshal([]byte(replace.Replace(c)), &container); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(replace.Replace(n)), &network); err != nil {
		t.Fatal(err)
	}
	return container, network
}

func TestInternalSSHMembership(t *testing.T) {
	tests := map[string]func(*inspectContainer, *inspectInternalNetwork){
		"second-network": func(c *inspectContainer, n *inspectInternalNetwork) {
			c.NetworkSettings.Networks["extra"] = inspectNetworkAttachment{}
		},
		"no-network":      func(c *inspectContainer, n *inspectInternalNetwork) { c.NetworkSettings.Networks = nil },
		"changed-network": func(c *inspectContainer, n *inspectInternalNetwork) { n.ID = strings.Repeat("d", 64) },
		"changed-endpoint": func(c *inspectContainer, n *inspectInternalNetwork) {
			a := c.NetworkSettings.Networks["private"]
			a.EndpointID = strings.Repeat("d", 64)
			c.NetworkSettings.Networks["private"] = a
		},
		"changed-container": func(c *inspectContainer, n *inspectInternalNetwork) { c.ID = strings.Repeat("d", 64) },
		"out-of-subnet": func(c *inspectContainer, n *inspectInternalNetwork) {
			a := c.NetworkSettings.Networks["private"]
			a.IPAddress = "172.31.0.2"
			c.NetworkSettings.Networks["private"] = a
		},
		"gateway-as-endpoint": func(c *inspectContainer, n *inspectInternalNetwork) {
			a := c.NetworkSettings.Networks["private"]
			a.IPAddress = "172.30.0.1"
			c.NetworkSettings.Networks["private"] = a
		},
		"loopback": func(c *inspectContainer, n *inspectInternalNetwork) {
			a := c.NetworkSettings.Networks["private"]
			a.IPAddress = "127.0.0.1"
			c.NetworkSettings.Networks["private"] = a
		},
		"unconfirmed-address": func(c *inspectContainer, n *inspectInternalNetwork) {
			a := n.Containers[c.ID]
			a.IPv4Address = "172.30.0.3/24"
			n.Containers[c.ID] = a
		},
		"changed-prefix": func(c *inspectContainer, n *inspectInternalNetwork) {
			a := n.Containers[c.ID]
			a.IPv4Address = "172.30.0.2/16"
			n.Containers[c.ID] = a
		},
		"published-ssh": func(c *inspectContainer, n *inspectInternalNetwork) {
			c.NetworkSettings.Ports["2222/tcp"] = []inspectPort{{HostIP: "127.0.0.1", HostPort: "12345"}}
		},
		"other-published-port": func(c *inspectContainer, n *inspectInternalNetwork) {
			c.NetworkSettings.Ports["80/tcp"] = []inspectPort{{HostIP: "0.0.0.0", HostPort: "8080"}}
		},
		"missing-binding": func(c *inspectContainer, n *inspectInternalNetwork) { delete(c.Config.Labels, sshNetworkLabel) },
	}
	c, n := internalSSHFixture(t)
	if err := validateInternalMembership(c, n); err != nil {
		t.Fatal(err)
	}
	host, port, err := containerSSHHostPort(c)
	if err != nil || host != "172.30.0.2" || port != "2222" {
		t.Fatalf("endpoint %s:%s: %v", host, port, err)
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			c, n := internalSSHFixture(t)
			mutate(&c, &n)
			if err := validateInternalMembership(c, n); err == nil {
				t.Fatal("unsafe endpoint accepted")
			}
		})
	}
}

func TestInternalSSHNetworkAndLocality(t *testing.T) {
	_, n := internalSSHFixture(t)
	bridge, prefix, gateway, err := validateInternalNetwork(n, n.Name)
	if err != nil || bridge != "br-"+n.ID[:12] {
		t.Fatalf("network: %s %v", bridge, err)
	}
	if err := validateInternalBridge([]net.Addr{&net.IPNet{IP: net.ParseIP("172.30.0.1"), Mask: net.CIDRMask(24, 32)}}, prefix, gateway); err != nil {
		t.Fatal(err)
	}
	if err := validateInternalBridge(nil, prefix, gateway); err == nil {
		t.Fatal("nonlocal bridge accepted")
	}
	if err := validateInternalBridge([]net.Addr{&net.IPNet{IP: net.ParseIP("172.30.0.1"), Mask: net.CIDRMask(16, 32)}}, prefix, gateway); err == nil {
		t.Fatal("wrong subnet accepted")
	}
	for _, name := range []string{"external", "overlay", "isolated", "no-ipv4", "missing-gateway", "wrong-id", "swarm", "bad-subnet"} {
		t.Run(name, func(t *testing.T) {
			_, n := internalSSHFixture(t)
			switch name {
			case "external":
				n.Internal = false
			case "overlay":
				n.Driver = "overlay"
			case "isolated":
				n.Options["com.docker.network.bridge.gateway_mode_ipv4"] = "isolated"
			case "no-ipv4":
				n.Options["com.docker.network.bridge.inhibit_ipv4"] = "true"
			case "missing-gateway":
				n.IPAM.Config[0].Gateway = ""
			case "wrong-id":
				n.ID = "short"
			case "swarm":
				n.Scope = "swarm"
			case "bad-subnet":
				n.IPAM.Config[0].Subnet = "invalid"
			}
			if _, _, _, err := validateInternalNetwork(n, "private"); err == nil {
				t.Fatal("invalid network accepted")
			}
		})
	}
}

func TestInternalSSHScope(t *testing.T) {
	base := checkpointScope{Runtime: "docker", Endpoint: "unix:///var/run/docker.sock", DaemonID: "daemon"}
	if err := validateInternalSSHScope(base, "linux"); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"", "tcp://127.0.0.1:2375", "ssh://example.invalid", "unix://remote/run/docker.sock", "unix:///run/docker.sock?x=1"} {
		scope := base
		scope.Endpoint = endpoint
		if err := validateInternalSSHScope(scope, "linux"); err == nil {
			t.Fatalf("accepted %q", endpoint)
		}
	}
	if err := validateInternalSSHScope(base, "darwin"); err == nil {
		t.Fatal("nonlinux accepted")
	}
	base.Runtime = "podman"
	if err := validateInternalSSHScope(base, "linux"); err == nil {
		t.Fatal("unqualified runtime accepted")
	}
}

func TestInternalSSHCreationAndConfiguration(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{commandKey([]string{"run"}): {Stdout: "container123456\n"}}}
	b := testBackend(runner)
	cfg := b.configForRun()
	fs := flag.NewFlagSet("transport", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{"--local-container-ssh-transport", "internal"}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	cfg.LocalContainer.SSHNetworkID = strings.Repeat("b", 64)
	_, bootstrap, err := b.createContainer(t.Context(), cfg, "crabbox-private", "cbx_123", "private", "ssh-ed25519 fixture", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrap) })
	args := runner.calls[0].Args
	if slices.Contains(args, "-p") || slices.Contains(args, "--publish") {
		t.Fatal("internal SSH published a port")
	}
	joined := strings.Join(args, "\n")
	for _, want := range []string{"--network\n" + cfg.LocalContainer.SSHNetworkID, "ssh_transport=internal", "ssh_network_id=" + cfg.LocalContainer.SSHNetworkID} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q", want)
		}
	}
	cfg.LocalContainer.SSHTransport = "automatic"
	if _, err := localContainerSSHNetworkArgs(cfg); err == nil {
		t.Fatal("unknown transport accepted")
	}
	cfg.LocalContainer.SSHTransport = "internal"
	cfg.LocalContainer.SSHNetworkID = ""
	if _, err := localContainerSSHNetworkArgs(cfg); err == nil {
		t.Fatal("unvalidated network accepted")
	}
}

func TestInternalSSHFixedIdentity(t *testing.T) {
	cfg := core.Config{LocalContainer: core.LocalContainerConfig{Runtime: "docker", Network: "private"}}
	base, err := fixedLocalContainerFingerprint(cfg, core.AcquireRequest{}, "key")
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocalContainer.SSHTransport = "published"
	explicit, err := fixedLocalContainerFingerprint(cfg, core.AcquireRequest{}, "key")
	if err != nil || explicit != base {
		t.Fatal("default fingerprint changed")
	}
	cfg.LocalContainer.SSHTransport = "internal"
	cfg.LocalContainer.SSHNetworkID = strings.Repeat("b", 64)
	internal, err := fixedLocalContainerFingerprint(cfg, core.AcquireRequest{}, "key")
	if err != nil || internal == base {
		t.Fatal("transport not bound")
	}
	cfg.LocalContainer.SSHNetworkID = strings.Repeat("c", 64)
	changed, err := fixedLocalContainerFingerprint(cfg, core.AcquireRequest{}, "key")
	if err != nil || changed == internal {
		t.Fatal("network identity not bound")
	}
}

// Checkpoint metadata can select this provider after another provider handled
// ordinary flags. The final mapper must retain only explicitly visited overrides.
func TestInternalSSHForkTransportOverride(t *testing.T) {
	for _, tc := range []struct{ name, prior, override, want string }{
		{"internal after provider selection", "published", "internal", "internal"},
		{"published override", "internal", "published", "published"},
		{"unvisited retains config", "internal", "", "internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := core.Config{Provider: "hetzner", LocalContainer: core.LocalContainerConfig{SSHTransport: tc.prior}}
			fs := flag.NewFlagSet("checkpoint fork", flag.ContinueOnError)
			values := registerFlags(fs, cfg)
			args := []string{"--local-container-network=private-runners"}
			if tc.override != "" {
				args = append(args, "--local-container-ssh-transport="+tc.override)
			}
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			cfg.Provider = providerName
			if err := (Provider{}).ApplyNativeCheckpointForkFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if cfg.LocalContainer.SSHTransport != tc.want || cfg.LocalContainer.Network != "private-runners" {
				t.Fatalf("final provider overrides: transport=%q network=%q; want %q and private-runners", cfg.LocalContainer.SSHTransport, cfg.LocalContainer.Network, tc.want)
			}
		})
	}
}

func TestInternalSSHRefusalIncludesExactLeaseRecovery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*inspectContainer)
		reason string
	}{
		{"network", func(c *inspectContainer) { c.NetworkSettings.Networks["extra"] = inspectNetworkAttachment{} }, "exactly its bound network"},
		{"ports", func(c *inspectContainer) {
			c.NetworkSettings.Ports["2222/tcp"] = []inspectPort{{HostIP: "127.0.0.1", HostPort: "12345"}}
		}, "published ports"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := internalSSHFixture(t)
			tc.mutate(&c)
			b := testBackend(&recordingRunner{})
			_, err := b.prepareLease(t.Context(), b.configForRun(), c, "cbx_fixture", "fixture", false)
			if err == nil || !strings.Contains(err.Error(), tc.reason) || !strings.Contains(err.Error(), "crabbox stop --provider local-container --id 'cbx_fixture'") {
				t.Fatalf("missing refusal/recovery: %v", err)
			}
			if !isExitCode(err, 2) {
				t.Fatalf("lost refusal exit code: %v", err)
			}
			var diagnostic core.ExitError
			if !core.AsExitError(err, &diagnostic) || !strings.Contains(diagnostic.Message, "crabbox stop --provider local-container --id 'cbx_fixture'") {
				t.Fatalf("CLI exit diagnostic omits recovery: %#v", diagnostic)
			}
		})
	}
}
