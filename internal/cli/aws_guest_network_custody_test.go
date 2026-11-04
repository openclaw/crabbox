package cli

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestAWSGuestNetworkCustodyPrecedesMutation(t *testing.T) {
	f := newAWSGuestNetworkFake()
	var saved []AWSGuestNetworkBoundary
	boundary, err := PrepareAWSGuestNetworkWithCustody(context.Background(), f, "cbx_0123456789ab", guestTestSubnet, awsGuestTestPolicy(), func(b AWSGuestNetworkBoundary) error {
		if len(saved) == 0 && slices.Contains(f.calls, "create") {
			t.Fatal("created group before durable cleanup custody")
		}
		saved = append(saved, b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 || saved[0].SecurityGroupID != "" || saved[0].SecurityGroupName == "" || saved[1].SecurityGroupID != boundary.SecurityGroupID {
		t.Fatalf("missing pre/post create custody: %#v", saved)
	}
}
func TestAWSGuestNetworkCustodyFailureDoesNotSubmit(t *testing.T) {
	f := newAWSGuestNetworkFake()
	boundary, err := PrepareAWSGuestNetworkWithCustody(context.Background(), f, "cbx_0123456789ab", guestTestSubnet, awsGuestTestPolicy(), func(AWSGuestNetworkBoundary) error { return errors.New("journal write refused") })
	if err == nil || slices.Contains(f.calls, "create") || boundary.SecurityGroupName == "" {
		t.Fatalf("lost handle or submitted before custody: %#v %v %v", boundary, f.calls, err)
	}
}
func TestAWSGuestNetworkUncertainCreateRetainsRecoveredCustody(t *testing.T) {
	f := newAWSGuestNetworkFake()
	f.createUncertain = true
	var saved []AWSGuestNetworkBoundary
	boundary, err := PrepareAWSGuestNetworkWithCustody(context.Background(), f, "cbx_0123456789ab", guestTestSubnet, awsGuestTestPolicy(), func(b AWSGuestNetworkBoundary) error { saved = append(saved, b); return nil })
	if err == nil || boundary.SecurityGroupID == "" || len(saved) != 2 || saved[1].SecurityGroupID != boundary.SecurityGroupID || f.group != nil {
		t.Fatalf("uncertain create lost custody/cleanup: %#v %#v %v", boundary, saved, err)
	}
	count := 0
	for _, call := range f.calls {
		if call == "create" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("uncertain create resubmitted", f.calls)
	}
}
func TestFixedClaimCloneKeepsCompanionCustodyIndependent(t *testing.T) {
	original := LeaseClaim{FixedCreateIntent: &FixedCreateIntent{Companions: map[string]string{"provider-network": "original"}}}
	copy := CloneLeaseClaim(original)
	copy.FixedCreateIntent.Companions["provider-network"] = "changed"
	if original.FixedCreateIntent.Companions["provider-network"] != "original" {
		t.Fatal("companion custody aliased across claim snapshots")
	}
}

func TestAWSGuestNetworkRequiresExplicitPlatformExceptionPolicy(t *testing.T) {
	for _, exceptions := range [][]string{nil, {"unknown"}} {
		f := newAWSGuestNetworkFake()
		policy := awsGuestTestPolicy()
		policy.PlatformExceptions = exceptions
		if _, err := PrepareAWSGuestNetwork(context.Background(), f, "cbx_0123456789ab", guestTestSubnet, policy); err == nil {
			t.Fatal("unacknowledged platform exceptions accepted")
		}
		if len(f.calls) != 0 {
			t.Fatal("provider called before policy admission", f.calls)
		}
	}
}
