package aws

import (
	"context"
	"errors"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type guestReceiptAWSClient struct {
	*fakeAWSClient
	receipt    *core.GuestNetworkReceipt
	inspectErr error
	cleaned    int
	events     []string
}

func (c *guestReceiptAWSClient) InspectGuestNetworkServer(context.Context, core.Config, core.Server, string) (*core.GuestNetworkReceipt, error) {
	c.events = append(c.events, "inspect")
	return c.receipt, c.inspectErr
}
func (c *guestReceiptAWSClient) CleanupGuestNetworkForServer(context.Context, core.Server) error {
	c.events = append(c.events, "cleanup")
	c.cleaned++
	return nil
}
func TestGuestNetworkBeforeBootstrapRejectsAbsentAndReplayedReceipt(t *testing.T) {
	policy := core.GuestNetworkPolicy{Version: 1, ID: "operator-egress", Revision: "v1", Egress: []core.GuestNetworkRule{{CIDR: "1.1.1.1/32", Ports: []int{443}}}, SSHSources: []string{"8.8.8.8/32"}}
	digest, err := core.GuestNetworkPolicyDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"absent", "replay", "provider mismatch", "API refusal", "valid"} {
		t.Run(name, func(t *testing.T) {
			receipt := &core.GuestNetworkReceipt{Version: 1, PolicyID: policy.ID, PolicyRevision: policy.Revision, PolicyDigest: digest, LeaseID: "cbx_0123456789ab", Provider: "aws", ResourceID: "i-example", BoundaryID: "sg-example"}
			client := &guestReceiptAWSClient{fakeAWSClient: &fakeAWSClient{}, receipt: receipt}
			switch name {
			case "absent":
				client.receipt = nil
			case "replay":
				receipt.LeaseID = "cbx_abcdef012345"
			case "provider mismatch":
				receipt.Provider = "other"
			case "API refusal":
				client.inspectErr = errors.New("control-plane unavailable")
			}
			server := core.Server{CloudID: "i-example", Labels: map[string]string{"lease": "cbx_0123456789ab", "guest_network_group": "sg-example"}}
			bootstrapped := false
			_, err := prepareAWSGuestAccess(context.Background(), client, core.Config{GuestNetworkPolicy: &policy}, server, "cbx_0123456789ab", func() error { client.events = append(client.events, "bootstrap"); bootstrapped = true; return nil })
			if name == "valid" {
				if err != nil || !bootstrapped || client.cleaned != 0 {
					t.Fatalf("valid boundary: bootstrapped=%v cleanup=%d err=%v", bootstrapped, client.cleaned, err)
				}
				if len(client.events) != 2 || client.events[0] != "inspect" || client.events[1] != "bootstrap" {
					t.Fatal(client.events)
				}
			} else {
				if err == nil || bootstrapped || client.cleaned != 1 || len(client.deletedInstances) != 1 {
					t.Fatalf("rejected boundary: bootstrapped=%v cleanup=%d deleted=%v err=%v", bootstrapped, client.cleaned, client.deletedInstances, err)
				}
			}
		})
	}
}
func TestGuestNetworkTrustedAccessDoesNotRequireNewClientAPI(t *testing.T) {
	client := &fakeAWSClient{}
	called := false
	_, err := prepareAWSGuestAccess(context.Background(), client, core.Config{}, core.Server{}, "", func() error { called = true; return nil })
	if err != nil || !called {
		t.Fatal("trusted access changed", err)
	}
}
