package guestnetwork

import "testing"

func testGuestPolicy() Policy {
	return Policy{Version: 1, ID: "guest-egress", Revision: "v1", Egress: []Rule{{CIDR: "1.1.1.1/32", Ports: []int{443}}}, SSHSources: []string{"8.8.8.8/32"}}
}
func testGuestReceipt(t *testing.T, policy Policy) *Receipt {
	t.Helper()
	digest, err := Digest(policy)
	if err != nil {
		t.Fatal(err)
	}
	return &Receipt{Version: 1, PolicyID: policy.ID, PolicyRevision: policy.Revision, PolicyDigest: digest, LeaseID: "cbx_0123456789ab", Provider: "aws", ResourceID: "i-example", BoundaryID: "sg-example"}
}
func TestReceiptRejectsMissingAndMismatchedBoundary(t *testing.T) {
	policy := testGuestPolicy()
	for _, tc := range []struct {
		name   string
		change func(*Receipt)
	}{
		{"version", func(r *Receipt) { r.Version = 99 }}, {"lease replay", func(r *Receipt) { r.LeaseID = "cbx_abcdef012345" }}, {"provider", func(r *Receipt) { r.Provider = "other" }}, {"resource", func(r *Receipt) { r.ResourceID = "i-other" }}, {"policy", func(r *Receipt) { r.PolicyDigest = "forged" }}, {"revision", func(r *Receipt) { r.PolicyRevision = "v2" }}, {"boundary", func(r *Receipt) { r.BoundaryID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := testGuestReceipt(t, policy)
			tc.change(r)
			if ValidateReceipt(policy, r, "cbx_0123456789ab", "aws", "i-example") == nil {
				t.Fatal("mismatched receipt accepted")
			}
		})
	}
	if ValidateReceipt(policy, nil, "cbx_0123456789ab", "aws", "i-example") == nil {
		t.Fatal("missing receipt accepted")
	}
	if err := ValidateReceipt(policy, testGuestReceipt(t, policy), "cbx_0123456789ab", "aws", "i-example"); err != nil {
		t.Fatal(err)
	}
}
func TestPolicyRejectsCounterexamples(t *testing.T) {
	for _, cidr := range []string{"0.0.0.0/0", "10.0.0.1/32", "169.254.169.254/32", "100.100.100.200/32", "127.0.0.1/32", "172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/4", "::/0", "::ffff:1.1.1.1/128", "1.1.1.1/24"} {
		t.Run(cidr, func(t *testing.T) {
			p := testGuestPolicy()
			p.Egress[0].CIDR = cidr
			if _, err := Digest(p); err == nil {
				t.Fatal("unsafe CIDR accepted")
			}
		})
	}
	p := testGuestPolicy()
	p.Egress[0].Ports = []int{0}
	if _, err := Digest(p); err == nil {
		t.Fatal("wildcard port accepted")
	}
	p = testGuestPolicy()
	p.Version = 2
	if _, err := Digest(p); err == nil {
		t.Fatal("unknown schema accepted")
	}
}
func TestDigestCanonicalAndImmutable(t *testing.T) {
	p := testGuestPolicy()
	p.Egress[0].Ports = []int{443, 80}
	a, err := Digest(p)
	if err != nil {
		t.Fatal(err)
	}
	if p.Egress[0].Ports[0] != 443 {
		t.Fatal("policy mutated")
	}
	p.Egress[0].Ports = []int{80, 443}
	b, err := Digest(p)
	if err != nil || a != b {
		t.Fatal("unstable policy digest")
	}
}
