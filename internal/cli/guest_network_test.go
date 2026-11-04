package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func testGuestPolicy() GuestNetworkPolicy {
	return GuestNetworkPolicy{Version: 1, ID: "guest-egress", Revision: "v1", Egress: []GuestNetworkRule{{CIDR: "1.1.1.1/32", Ports: []int{443}}}, SSHSources: []string{"8.8.8.8/32"}}
}
func testGuestReceipt(t *testing.T, policy GuestNetworkPolicy) *GuestNetworkReceipt {
	t.Helper()
	digest, err := GuestNetworkPolicyDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	return &GuestNetworkReceipt{Version: 1, PolicyID: policy.ID, PolicyRevision: policy.Revision, PolicyDigest: digest, LeaseID: "cbx_0123456789ab", Provider: "aws", ResourceID: "i-example", BoundaryID: "sg-example"}
}
func TestGuestNetworkReceiptRejectsMissingAndMismatchedBoundary(t *testing.T) {
	policy := testGuestPolicy()
	for _, tc := range []struct {
		name   string
		change func(*GuestNetworkReceipt)
	}{
		{"version", func(r *GuestNetworkReceipt) { r.Version = 99 }}, {"lease replay", func(r *GuestNetworkReceipt) { r.LeaseID = "cbx_abcdef012345" }}, {"provider", func(r *GuestNetworkReceipt) { r.Provider = "other" }}, {"resource", func(r *GuestNetworkReceipt) { r.ResourceID = "i-other" }}, {"policy", func(r *GuestNetworkReceipt) { r.PolicyDigest = "forged" }}, {"revision", func(r *GuestNetworkReceipt) { r.PolicyRevision = "v2" }}, {"boundary", func(r *GuestNetworkReceipt) { r.BoundaryID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := testGuestReceipt(t, policy)
			tc.change(r)
			if ValidateGuestNetworkReceipt(policy, r, "cbx_0123456789ab", "aws", "i-example") == nil {
				t.Fatal("mismatched receipt accepted")
			}
		})
	}
	if ValidateGuestNetworkReceipt(policy, nil, "cbx_0123456789ab", "aws", "i-example") == nil {
		t.Fatal("missing receipt accepted")
	}
	if err := ValidateGuestNetworkReceipt(policy, testGuestReceipt(t, policy), "cbx_0123456789ab", "aws", "i-example"); err != nil {
		t.Fatal(err)
	}
}
func TestGuestNetworkPolicyRejectsCounterexamples(t *testing.T) {
	for _, cidr := range []string{"0.0.0.0/0", "10.0.0.1/32", "169.254.169.254/32", "100.100.100.200/32", "127.0.0.1/32", "172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/4", "::/0", "::ffff:1.1.1.1/128", "1.1.1.1/24"} {
		t.Run(cidr, func(t *testing.T) {
			p := testGuestPolicy()
			p.Egress[0].CIDR = cidr
			if _, err := GuestNetworkPolicyDigest(p); err == nil {
				t.Fatal("unsafe CIDR accepted")
			}
		})
	}
	p := testGuestPolicy()
	p.Egress[0].Ports = []int{0}
	if _, err := GuestNetworkPolicyDigest(p); err == nil {
		t.Fatal("wildcard port accepted")
	}
	p = testGuestPolicy()
	p.Version = 2
	if _, err := GuestNetworkPolicyDigest(p); err == nil {
		t.Fatal("unknown schema accepted")
	}
}
func TestGuestNetworkPolicyDigestCanonicalAndImmutable(t *testing.T) {
	p := testGuestPolicy()
	p.Egress[0].Ports = []int{443, 80}
	a, err := GuestNetworkPolicyDigest(p)
	if err != nil {
		t.Fatal(err)
	}
	if p.Egress[0].Ports[0] != 443 {
		t.Fatal("policy mutated")
	}
	p.Egress[0].Ports = []int{80, 443}
	b, err := GuestNetworkPolicyDigest(p)
	if err != nil || a != b {
		t.Fatal("unstable policy digest")
	}
}
func TestGuestNetworkPolicyRequiresOperatorSource(t *testing.T) {
	t.Setenv("CRABBOX_UNTRUSTED_WORKLOAD", "true")
	t.Setenv("CRABBOX_GUEST_NETWORK_POLICY_FILE", "")
	if _, err := loadOperatorGuestNetworkPolicy(); err == nil {
		t.Fatal("missing policy accepted")
	}
	t.Setenv("CRABBOX_GUEST_NETWORK_POLICY_FILE", "./policy.json")
	if _, err := loadOperatorGuestNetworkPolicy(); err == nil {
		t.Fatal("relative policy accepted")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(path, []byte("{}"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_GUEST_NETWORK_POLICY_FILE", path)
	if _, err := loadOperatorGuestNetworkPolicy(); err == nil {
		t.Fatal("writable policy accepted")
	}
}
func TestGuestNetworkBackendUnsupportedAndCoordinatorFailClosed(t *testing.T) {
	p := testGuestPolicy()
	cfg := Config{GuestNetworkPolicy: &p}
	backend := guestNetworkTestBackend{}
	if validateGuestNetworkBackend(cfg, backend, false) == nil {
		t.Fatal("unsupported provider accepted")
	}
	if err := validateGuestNetworkBackend(cfg, backend, true); err != nil {
		t.Fatal("managed route must defer to authenticated handshake", err)
	}
	if err := validateGuestNetworkBackend(Config{}, backend, true); err != nil {
		t.Fatal("trusted workflow changed", err)
	}
}

type guestNetworkTestBackend struct{}

func TestGuestNetworkReceiptProtocolCustody(t *testing.T) {
	policy := testGuestPolicy()
	receipt := testGuestReceipt(t, policy)
	lease := LeaseTarget{LeaseID: receipt.LeaseID, Server: Server{CloudID: receipt.ResourceID}, GuestNetwork: receipt}
	identity := controllerAcquireIdentityFromLease("aws", lease)
	identityJSON, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(identityJSON, &legacy); err != nil {
		t.Fatal(err)
	}
	if _, exists := legacy["guestNetwork"]; exists {
		t.Fatal("network receipt serialized to legacy controller identity protocol")
	}
	if identity.LeaseID != receipt.LeaseID || identity.ResourceID != receipt.ResourceID {
		t.Fatal("controller allocation identity lost")
	}
	for _, value := range []any{TimingReport{GuestNetwork: receipt}, StatusView{GuestNetwork: receipt}} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			GuestNetwork *GuestNetworkReceipt `json:"guestNetwork"`
		}
		if err := json.Unmarshal(encoded, &envelope); err != nil {
			t.Fatal(err)
		}
		if err := ValidateGuestNetworkReceipt(policy, envelope.GuestNetwork, receipt.LeaseID, "aws", receipt.ResourceID); err != nil {
			t.Fatal(err)
		}
	}
	encoded, err := json.Marshal(TimingReport{})
	if err != nil {
		t.Fatal(err)
	}
	var trusted map[string]any
	if err := json.Unmarshal(encoded, &trusted); err != nil {
		t.Fatal(err)
	}
	if _, ok := trusted["guestNetwork"]; ok {
		t.Fatal("trusted protocol gained a synthetic guest receipt")
	}
}

func (guestNetworkTestBackend) Spec() ProviderSpec { return ProviderSpec{Name: "unsupported"} }
