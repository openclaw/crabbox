package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type guestNetworkCallerFixture struct {
	FixtureVersion int `json:"fixtureVersion"`
	Discovery      struct {
		Provider     string                  `json:"provider"`
		Capabilities providerDescriptionCaps `json:"capabilities"`
	} `json:"discovery"`
	DirectConfig struct {
		GuestNetworkCapability GuestNetworkRouteCapability `json:"guestNetworkCapability"`
		GuestNetworkPolicy     GuestNetworkReceipt         `json:"guestNetworkPolicy"`
	} `json:"directConfig"`
	ManagedCoordinatorConfig struct {
		GuestNetworkCapability GuestNetworkRouteCapability `json:"guestNetworkCapability"`
	} `json:"managedCoordinatorConfig"`
	OperatorPolicy           GuestNetworkPolicy `json:"operatorPolicy"`
	ExpectedAllocatorBinding struct {
		LeaseID    string `json:"leaseId"`
		Provider   string `json:"provider"`
		ResourceID string `json:"resourceId"`
	} `json:"expectedAllocatorBinding"`
	AcquisitionTiming  TimingReport              `json:"acquisitionTiming"`
	Inspect            StatusView                `json:"inspect"`
	ControllerIdentity controllerAcquireIdentity `json:"controllerIdentity"`
}

func loadGuestNetworkCallerFixture(t *testing.T) guestNetworkCallerFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/guest-network-caller-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture guestNetworkCallerFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.FixtureVersion != 1 {
		t.Fatal("unknown caller fixture version")
	}
	return fixture
}

type guestNetworkContractBackend struct{ version int }

func (guestNetworkContractBackend) Spec() ProviderSpec         { return ProviderSpec{Name: "aws"} }
func (b guestNetworkContractBackend) GuestNetworkVersion() int { return b.version }

func TestGuestNetworkCallerContractAcquisitionAndInspectBindings(t *testing.T) {
	f := loadGuestNetworkCallerFixture(t)
	binding := f.ExpectedAllocatorBinding
	digest, err := GuestNetworkPolicyDigest(f.OperatorPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if f.DirectConfig.GuestNetworkPolicy.PolicyDigest != digest {
		t.Fatal("fixture policy digest differs from canonical effective policy")
	}
	for _, phase := range []struct {
		name    string
		receipt *GuestNetworkReceipt
	}{{"acquisition", f.AcquisitionTiming.GuestNetwork}, {"inspect", f.Inspect.GuestNetwork}} {
		t.Run(phase.name, func(t *testing.T) {
			if err := ValidateGuestNetworkReceipt(f.OperatorPolicy, phase.receipt, binding.LeaseID, binding.Provider, binding.ResourceID); err != nil {
				t.Fatal(err)
			}
			for _, change := range []struct {
				name  string
				apply func(*GuestNetworkReceipt)
			}{
				{"lease", func(r *GuestNetworkReceipt) { r.LeaseID = "cbx_abcdef012345" }}, {"resource", func(r *GuestNetworkReceipt) { r.ResourceID = "i-22222222" }}, {"policy", func(r *GuestNetworkReceipt) { r.PolicyDigest = "wrong" }}, {"provider", func(r *GuestNetworkReceipt) { r.Provider = "unsupported" }}, {"unknown version", func(r *GuestNetworkReceipt) { r.Version = 99 }}, {"missing boundary", func(r *GuestNetworkReceipt) { r.BoundaryID = "" }},
			} {
				t.Run(change.name, func(t *testing.T) {
					r := *phase.receipt
					change.apply(&r)
					if ValidateGuestNetworkReceipt(f.OperatorPolicy, &r, binding.LeaseID, binding.Provider, binding.ResourceID) == nil {
						t.Fatal("mismatched observed boundary accepted")
					}
				})
			}
			if ValidateGuestNetworkReceipt(f.OperatorPolicy, nil, binding.LeaseID, binding.Provider, binding.ResourceID) == nil {
				t.Fatal("missing receipt accepted")
			}
		})
	}
	if f.AcquisitionTiming.LeaseID != binding.LeaseID || f.Inspect.ID != binding.LeaseID || f.Inspect.ProviderResourceID != binding.ResourceID {
		t.Fatal("outer output identity differs from receipt binding")
	}
}
func TestGuestNetworkCallerContractRouteDiscovery(t *testing.T) {
	f := loadGuestNetworkCallerFixture(t)
	backend := guestNetworkContractBackend{version: 1}
	if !reflect.DeepEqual(guestNetworkDiscovery(backend), f.Discovery.Capabilities.GuestNetwork) {
		t.Fatal("discovery fixture drift")
	}
	if !reflect.DeepEqual(guestNetworkRouteCapability(Config{}, backend, false), f.DirectConfig.GuestNetworkCapability) {
		t.Fatal("direct route fixture drift")
	}
	if !reflect.DeepEqual(guestNetworkRouteCapability(Config{}, backend, true), f.ManagedCoordinatorConfig.GuestNetworkCapability) {
		t.Fatal("managed coordinator route fixture drift")
	}
	if guestNetworkDiscovery(guestNetworkContractBackend{version: 99}) != nil {
		t.Fatal("unknown version advertised")
	}
	if guestNetworkRouteCapability(Config{}, guestNetworkContractBackend{version: 99}, false).State != "unsupported" {
		t.Fatal("unknown route version accepted")
	}
	if guestNetworkRouteCapability(Config{}, guestNetworkTestBackend{}, false).State != "unsupported" {
		t.Fatal("unaudited provider accepted")
	}
	encoded, err := json.Marshal(f.ControllerIdentity)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 4 || fields["guestNetwork"] != nil {
		t.Fatal("legacy controller wire was extended without negotiation")
	}
}

type guestNetworkNoRequestTransport struct{ calls int }

func (t *guestNetworkNoRequestTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return nil, errors.New("unexpected coordinator request")
}
func TestGuestNetworkManagedCoordinatorRejectsUnqualifiedCapability(t *testing.T) {
	f := loadGuestNetworkCallerFixture(t)
	cfg := Config{Provider: "aws", GuestNetworkPolicy: &f.OperatorPolicy}
	transport := &guestNetworkNoRequestTransport{}
	client := &CoordinatorClient{Client: &http.Client{Transport: transport}}
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: client}
	for _, call := range []struct {
		name string
		run  func() error
	}{
		{"client create", func() error {
			_, err := client.CreateLease(context.Background(), cfg, "public-key-only", false, "cbx_0123456789ab", "example")
			return err
		}},
		{"acquire", func() error {
			_, err := backend.Acquire(context.Background(), AcquireRequest{RequestedLeaseID: "cbx_0123456789ab"})
			return err
		}},
		{"resolve", func() error {
			_, err := backend.Resolve(context.Background(), ResolveRequest{ID: "cbx_0123456789ab"})
			return err
		}},
		{"inspect", func() error {
			_, err := backend.Status(context.Background(), StatusRequest{ID: "cbx_0123456789ab", AuthoritativeProviderMetadata: true})
			return err
		}},
	} {
		t.Run(call.name, func(t *testing.T) {
			err := call.run()
			if err == nil || !strings.Contains(err.Error(), "managed guest network capability required") {
				t.Fatal("unsupported broker request did not fail at route guard", err)
			}
		})
	}
	if transport.calls == 0 {
		t.Fatal("managed capability discovery was not attempted")
	}
}

func TestGuestNetworkPolicyFileTrustProvenance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("operator file ACL custody is not implemented on Windows")
	}
	f := loadGuestNetworkCallerFixture(t)
	parent, err := os.MkdirTemp(".", "guest-policy-provenance-")
	if err != nil {
		t.Fatal(err)
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	repo := filepath.Join(parent, "workload")
	if err := os.MkdirAll(filepath.Join(repo, ".git", "objects"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".git", "refs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(f.OperatorPolicy)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "operator-policy.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(parent, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(parent, "config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "aws")
	t.Setenv("CRABBOX_UNTRUSTED_WORKLOAD", "true")
	t.Setenv("CRABBOX_GUEST_NETWORK_POLICY_FILE", path)
	t.Chdir(repo)
	if _, err := loadOperatorGuestNetworkPolicy(); err != nil {
		t.Fatal("operator-owned policy outside workload rejected", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".crabbox.yaml"), []byte("invalid: [repository helper configuration must not be parsed"), 0600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := loadConfigInputs(); err != nil || cfg.GuestNetworkPolicy == nil || cfg.Provider != "aws" {
		t.Fatal("repository policy/helper input affected trusted allocation config", err)
	}
	for _, tc := range []struct {
		name    string
		prepare func() string
	}{
		{"repository policy", func() string {
			p := filepath.Join(repo, "policy.json")
			if err := os.WriteFile(p, data, 0600); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"symlink policy", func() string {
			p := filepath.Join(parent, "linked-policy.json")
			if err := os.Symlink(path, p); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"writable policy", func() string {
			p := filepath.Join(parent, "writable-policy.json")
			if err := os.WriteFile(p, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, 0666); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"unknown policy version", func() string {
			p := filepath.Join(parent, "unknown-policy.json")
			policy := f.OperatorPolicy
			policy.Version = 99
			b, _ := json.Marshal(policy)
			if err := os.WriteFile(p, b, 0600); err != nil {
				t.Fatal(err)
			}
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CRABBOX_GUEST_NETWORK_POLICY_FILE", tc.prepare())
			if _, err := loadOperatorGuestNetworkPolicy(); err == nil {
				t.Fatal("untrusted/unknown policy source accepted")
			}
		})
	}
}
