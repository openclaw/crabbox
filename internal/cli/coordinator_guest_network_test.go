package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func guestBootstrapTestPublicKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}
func managedGuestTestPolicy() GuestNetworkPolicy {
	p := testGuestPolicy()
	p.BootstrapMode = GuestBootstrapPublicKeyV1
	p.PlatformExceptions = []string{"aws-platform-services-v1", "aws-roleless-imdsv2-public-bootstrap-v1"}
	return p
}
func TestGuestNetworkManagedVersionedCaller(t *testing.T) {
	for _, mutation := range []string{"valid", "old-server", "version", "policy", "lease", "resource", "receipt", "mode", "host-key", "inspect-resource"} {
		t.Run(mutation, func(t *testing.T) {
			p := managedGuestTestPolicy()
			identity, err := guestNetworkPolicyIdentity(p)
			if err != nil {
				t.Fatal(err)
			}
			id := "cbx_0123456789ab"
			host := guestBootstrapTestPublicKey(t)
			puts, inspects := 0, 0
			lease := CoordinatorLease{ID: id, Provider: "aws", CloudID: "i-0123456789abcdef0", UntrustedWorkload: true, SSHHostKey: host}
			lease.GuestNetwork = &GuestNetworkReceipt{Version: 1, PolicyID: identity.PolicyID, PolicyRevision: identity.PolicyRevision, PolicyDigest: identity.PolicyDigest, BootstrapMode: p.BootstrapMode, LeaseID: id, Provider: "aws", ResourceID: lease.CloudID, BoundaryID: "sg-fixture", Facts: map[string]string{"instanceProfile": "none", "imds": "roleless-v2", "bootstrapMode": p.BootstrapMode, "bootstrapSha256": strings.Repeat("a", 64), "sshHostKey": host}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/guest-network/capability" {
					if mutation == "old-server" {
						http.NotFound(w, r)
						return
					}
					v := 1
					ident := identity
					if mutation == "version" {
						v = 2
					}
					if mutation == "policy" {
						ident.PolicyDigest = strings.Repeat("0", 64)
					}
					json.NewEncoder(w).Encode(map[string]any{"version": v, "provider": "aws", "allocationRoute": "managed-coordinator", "state": "implemented", "policy": ident})
					return
				}
				if r.URL.Path != "/v1/guest-network/leases/"+id {
					t.Errorf("legacy/unexpected path: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				if r.Method == "PUT" {
					puts++
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body["untrustedWorkload"] != true || body["expectedGuestNetworkPolicy"] == nil {
						t.Error("missing versioned admission fields")
					}
					for _, field := range []string{"guestNetworkPolicy", "guestNetworkPolicyFile", "guestNetworkReceipt", "guestNetworkBoundary"} {
						if body[field] != nil {
							t.Errorf("client authority serialized: %s", field)
						}
					}
					w.WriteHeader(http.StatusCreated)
				} else if r.Method == "GET" {
					inspects++
				} else {
					t.Errorf("method %s", r.Method)
				}
				observed := lease
				receipt := *lease.GuestNetwork
				observed.GuestNetwork = &receipt
				switch mutation {
				case "lease":
					observed.ID = "cbx_ffffffffffff"
				case "resource":
					receipt.ResourceID = "i-other"
				case "receipt":
					observed.GuestNetwork = nil
				case "mode":
					observed.UntrustedWorkload = false
				case "host-key":
					observed.SSHHostKey = ""
				case "inspect-resource":
					if r.Method == "GET" {
						receipt.ResourceID = "i-other"
					}
				}
				json.NewEncoder(w).Encode(map[string]any{"lease": observed, "policy": identity})
			}))
			defer server.Close()
			client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
			cfg := Config{Provider: "aws", UntrustedWorkload: true, GuestNetworkPolicy: &p, OSImage: "ubuntu-24.04"}
			_, err = client.EnsureLease(context.Background(), cfg, guestBootstrapTestPublicKey(t), false, id, "example")
			if mutation == "valid" || mutation == "inspect-resource" {
				if err != nil {
					t.Fatal(err)
				}
				_, err = client.GetLease(context.Background(), id)
				if mutation == "valid" && err != nil {
					t.Fatal(err)
				}
				if mutation == "inspect-resource" && err == nil {
					t.Fatal("inspect accepted mismatched resource")
				}
				if puts != 1 || inspects != 1 {
					t.Fatalf("requests puts=%d inspect=%d", puts, inspects)
				}
			} else if err == nil {
				t.Fatal("accepted unqualified/mismatched managed allocation")
			}
			if (mutation == "old-server" || mutation == "version" || mutation == "policy") && puts != 0 {
				t.Fatal("allocation before capability/policy qualification")
			}
		})
	}
}

func TestGuestBootstrapPublicKeyOnly(t *testing.T) {
	key := guestBootstrapTestPublicKey(t)
	encoded, err := GuestPublicKeyUserData(key+" ignored-comment-secret", "ubuntu", "/work/project")
	if err != nil {
		t.Fatal(err)
	}
	if encoded == "" {
		t.Fatal("empty userdata")
	}
	for _, bad := range []struct{ k, u, w string }{{"not-a-key", "ubuntu", "/work"}, {key, "root;touch /bad", "/work"}, {key, "ubuntu", "../work"}, {key + "\n" + key, "ubuntu", "/work"}} {
		if _, err := GuestPublicKeyUserData(bad.k, bad.u, bad.w); err == nil {
			t.Fatal("malformed public bootstrap accepted")
		}
	}
}

func TestGuestNetworkManagedRetainedCallerContract(t *testing.T) {
	data, err := os.ReadFile("testdata/guest-network-caller-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		ManagedOperatorPolicy      GuestNetworkPolicy
		ManagedServerCapability    struct{ Policy GuestNetworkPolicyIdentity }
		ManagedAcquisitionResponse struct {
			Lease  CoordinatorLease
			Policy GuestNetworkPolicyIdentity
		}
		ManagedInspectResponse struct {
			Lease  CoordinatorLease
			Policy GuestNetworkPolicyIdentity
		}
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	identity, err := guestNetworkPolicyIdentity(f.ManagedOperatorPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if identity != f.ManagedServerCapability.Policy {
		t.Fatal("cross-language policy digest drift")
	}
	c := &CoordinatorClient{guestNetworkExpected: &identity, guestNetworkProvider: "aws"}
	for _, lease := range []CoordinatorLease{f.ManagedAcquisitionResponse.Lease, f.ManagedInspectResponse.Lease} {
		if err := c.validateManagedGuestLease(lease, "cbx_0123456789ab"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGuestNetworkManagedCachedPolicyFence(t *testing.T) {
	p := managedGuestTestPolicy()
	identity, err := guestNetworkPolicyIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	c := &CoordinatorClient{guestNetworkExpected: &identity, guestNetworkProvider: "aws"}
	cfg := Config{Provider: "aws", UntrustedWorkload: true, GuestNetworkPolicy: &p}
	if err := c.prepareManagedGuestNetwork(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	changed := p
	changed.Revision = "new-revision"
	cfg.GuestNetworkPolicy = &changed
	if c.prepareManagedGuestNetwork(context.Background(), cfg) == nil {
		t.Fatal("cached handshake ignored new operator policy")
	}
	cfg.GuestNetworkPolicy = &p
	cfg.Provider = "gcp"
	if c.prepareManagedGuestNetwork(context.Background(), cfg) == nil {
		t.Fatal("cached handshake crossed providers")
	}
}
