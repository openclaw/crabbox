package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v8"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
)

type azureOrphanCredential struct{}

func (azureOrphanCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "test", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

type azureOrphanTransport func(*http.Request) (*http.Response, error)

func (f azureOrphanTransport) Do(req *http.Request) (*http.Response, error) { return f(req) }

type azureOrphanFixture struct {
	client     *AzureClient
	server     Server
	objects    map[string]map[string]any
	deletes    []string
	failDelete string
	failRead   string
	mu         sync.Mutex
}

func newAzureOrphanFixture(t *testing.T) *azureOrphanFixture {
	t.Helper()
	lease, slug := "cbx_123456abcdef", "orphan"
	name := LeaseProviderName(lease, slug)
	f := &azureOrphanFixture{server: Server{CloudID: name, Name: name, ImmutableID: "original-vm", Labels: map[string]string{
		"crabbox": "true", "created_by": "crabbox", "provider": "azure", "lease": lease, "slug": slug,
		"provider_key": ProviderKeyForLease(lease), "fixed_attempt": "attempt", "fixed_intent_sha256": strings.Repeat("a", 64),
	}}, objects: make(map[string]map[string]any)}
	prefix := "/subscriptions/sub/resourceGroups/rg/providers/"
	for suffix, kind := range map[string]string{"-nic": "Microsoft.Network/networkInterfaces", "-pip": "Microsoft.Network/publicIPAddresses", "-osdisk": "Microsoft.Compute/disks", "-q-nsg": "Microsoft.Network/networkSecurityGroups"} {
		f.objects[name+suffix] = map[string]any{"id": prefix + kind + "/" + name + suffix, "name": name + suffix, "tags": azureTagsFromLabels(f.server.Labels), "properties": map[string]any{"resourceGuid": suffix + "-guid", "uniqueId": suffix + "-guid", "diskState": "Unattached"}}
	}
	nicID := f.objects[name+"-nic"]["id"].(string)
	f.properties("-nic")["ipConfigurations"] = []any{map[string]any{"id": nicID + "/ipConfigurations/ipconfig", "properties": map[string]any{"publicIPAddress": map[string]any{"id": f.objects[name+"-pip"]["id"]}}}}
	f.properties("-pip")["ipConfiguration"] = map[string]any{"id": nicID + "/ipConfigurations/ipconfig"}
	f.properties("-q-nsg")["networkInterfaces"] = []any{map[string]any{"id": nicID}}
	transport := azureOrphanTransport(func(req *http.Request) (*http.Response, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		object, exists := f.objects[name]
		status, body := http.StatusOK, any(object)
		if !exists {
			status, body = http.StatusNotFound, map[string]any{"error": map[string]any{"code": "ResourceNotFound", "message": "absent"}}
		}
		if req.Method == http.MethodGet && name == f.failRead {
			status, body = http.StatusForbidden, map[string]any{"error": map[string]any{"code": "AuthorizationFailed", "message": "simulated read failure"}}
		}
		if req.Method == http.MethodDelete {
			f.deletes = append(f.deletes, name)
			if name == f.failDelete {
				status, body = http.StatusForbidden, map[string]any{"error": map[string]any{"code": "AuthorizationFailed", "message": "simulated failure"}}
			} else {
				delete(f.objects, name)
				if strings.HasSuffix(name, "-nic") {
					if nsg := f.objects[f.server.CloudID+"-q-nsg"]; nsg != nil {
						delete(nsg["properties"].(map[string]any), "networkInterfaces")
					}
					if ip := f.objects[f.server.CloudID+"-pip"]; ip != nil {
						delete(ip["properties"].(map[string]any), "ipConfiguration")
					}
				}
				status, body = http.StatusOK, map[string]any{}
			}
		} else if req.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", req.Method, req.URL)
		}
		data, _ := json.Marshal(body)
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data))), Request: req}, nil
	})
	opts := &arm.ClientOptions{ClientOptions: policy.ClientOptions{Transport: transport, Retry: policy.RetryOptions{MaxRetries: -1}}}
	network, err := armnetwork.NewClientFactory("sub", azureOrphanCredential{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	compute, err := armcompute.NewClientFactory("sub", azureOrphanCredential{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	f.client = &AzureClient{SubscriptionID: "sub", ResourceGroup: "rg", nicc: network.NewInterfacesClient(), pipc: network.NewPublicIPAddressesClient(), sgc: network.NewSecurityGroupsClient(), diskc: compute.NewDisksClient(), vmc: compute.NewVirtualMachinesClient()}
	return f
}

func (f *azureOrphanFixture) properties(suffix string) map[string]any {
	return f.objects[f.server.CloudID+suffix]["properties"].(map[string]any)
}

func TestAzureOrphanCleanup(t *testing.T) {
	for _, missing := range []string{"none", "disk", "nsg", "all"} {
		t.Run(missing, func(t *testing.T) {
			f := newAzureOrphanFixture(t)
			if missing == "disk" {
				delete(f.objects, f.server.CloudID+"-osdisk")
			}
			if missing == "nsg" {
				delete(f.objects, f.server.CloudID+"-q-nsg")
			}
			if missing == "all" {
				clear(f.objects)
			}
			wantDeletes := len(f.objects)
			prepared, err := f.client.PrepareOwnedServer(t.Context(), f.server)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Labels[AzureCleanupBindingLabel] != azureOrphanCleanupBindingVersion || len(f.deletes) != 0 {
				t.Fatal("missing read-only orphan attestation")
			}
			if err := f.client.DeleteOwnedServer(t.Context(), prepared); err != nil {
				t.Fatal(err)
			}
			if len(f.deletes) != wantDeletes || len(f.objects) != 0 {
				t.Fatalf("deletes=%v remaining=%v", f.deletes, f.objects)
			}
			if wantDeletes > 0 && f.deletes[0] != f.server.CloudID+"-nic" {
				t.Fatal("NIC must be deleted before its public IP")
			}
			if err := f.client.DeleteOwnedServer(t.Context(), prepared); err != nil {
				t.Fatalf("absent retry: %v", err)
			}
		})
	}
}

func TestAzureOrphanCleanupRejectsUnsafeResources(t *testing.T) {
	cases := map[string]func(*azureOrphanFixture){
		"read failure": func(f *azureOrphanFixture) { f.failRead = f.server.CloudID + "-nic" },
		"foreign NSG attachment": func(f *azureOrphanFixture) {
			f.properties("-q-nsg")["networkInterfaces"] = []any{map[string]any{"id": "other"}}
		},
		"subnet NSG attachment": func(f *azureOrphanFixture) { f.properties("-q-nsg")["subnets"] = []any{map[string]any{"id": "other"}} },
		"foreign lease": func(f *azureOrphanFixture) {
			f.objects[f.server.CloudID+"-nic"]["tags"].(map[string]string)["lease"] = "cbx_abcdef123456"
		},
		"foreign attempt": func(f *azureOrphanFixture) {
			f.objects[f.server.CloudID+"-pip"]["tags"].(map[string]string)["fixed_attempt"] = "other"
		},
		"foreign intent": func(f *azureOrphanFixture) {
			f.objects[f.server.CloudID+"-osdisk"]["tags"].(map[string]string)["fixed_intent_sha256"] = strings.Repeat("b", 64)
		},
		"untagged disk": func(f *azureOrphanFixture) { delete(f.objects[f.server.CloudID+"-osdisk"], "tags") },
		"cross scope": func(f *azureOrphanFixture) {
			f.objects[f.server.CloudID+"-nic"]["id"] = "/subscriptions/other/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/" + f.server.CloudID + "-nic"
		},
		"attached NIC":         func(f *azureOrphanFixture) { f.properties("-nic")["virtualMachine"] = map[string]any{"id": "other"} },
		"private endpoint":     func(f *azureOrphanFixture) { f.properties("-nic")["privateEndpoint"] = map[string]any{"id": "other"} },
		"foreign IP reference": func(f *azureOrphanFixture) { f.properties("-pip")["ipConfiguration"] = map[string]any{"id": "other"} },
		"NAT IP":               func(f *azureOrphanFixture) { f.properties("-pip")["natGateway"] = map[string]any{"id": "other"} },
		"attached disk":        func(f *azureOrphanFixture) { f.objects[f.server.CloudID+"-osdisk"]["managedBy"] = "other" },
		"shared disk": func(f *azureOrphanFixture) {
			f.objects[f.server.CloudID+"-osdisk"]["managedByExtended"] = []string{"other"}
		},
		"unknown disk state": func(f *azureOrphanFixture) { delete(f.properties("-osdisk"), "diskState") },
		"missing identity":   func(f *azureOrphanFixture) { delete(f.properties("-nic"), "resourceGuid") },
		"missing properties": func(f *azureOrphanFixture) { delete(f.objects[f.server.CloudID+"-nic"], "properties") },
		"VM reappeared":      func(f *azureOrphanFixture) { f.objects[f.server.CloudID] = map[string]any{"name": f.server.CloudID} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newAzureOrphanFixture(t)
			prepared, err := f.client.PrepareOwnedServer(t.Context(), f.server)
			if err != nil {
				t.Fatal(err)
			}
			mutate(f)
			if err := f.client.DeleteOwnedServer(t.Context(), prepared); err == nil {
				t.Fatal("unsafe deletion accepted")
			}
			if len(f.deletes) != 0 {
				t.Fatalf("unsafe deletes: %v", f.deletes)
			}
		})
	}
}

func TestAzureOrphanCleanupPartialFailureRetainsIdentity(t *testing.T) {
	f := newAzureOrphanFixture(t)
	prepared, err := f.client.PrepareOwnedServer(t.Context(), f.server)
	if err != nil {
		t.Fatal(err)
	}
	f.failDelete = f.server.CloudID + "-pip"
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.client.DeleteOwnedServer(ctx, prepared); err == nil {
		t.Fatal("expected partial failure")
	}
	if len(f.objects) != 1 {
		t.Fatalf("remaining resources=%v", f.objects)
	}
	f.properties("-pip")["resourceGuid"] = "replacement"
	before := len(f.deletes)
	if err := f.client.DeleteOwnedServer(t.Context(), prepared); err == nil || len(f.deletes) != before {
		t.Fatal("replacement resource accepted on retry")
	}
	f.properties("-pip")["resourceGuid"] = "-pip-guid"
	f.failDelete = ""
	if err := f.client.DeleteOwnedServer(t.Context(), prepared); err != nil {
		t.Fatal(err)
	}
}

func TestAzureOrphanCleanupDoesNotRecaptureAbsentResources(t *testing.T) {
	f := newAzureOrphanFixture(t)
	disk := f.objects[f.server.CloudID+"-osdisk"]
	delete(f.objects, f.server.CloudID+"-osdisk")
	prepared, err := f.client.PrepareOwnedServer(t.Context(), f.server)
	if err != nil {
		t.Fatal(err)
	}
	f.objects[f.server.CloudID+"-osdisk"] = disk
	if err := f.client.DeleteOwnedServer(t.Context(), prepared); err == nil || len(f.deletes) != 0 {
		t.Fatal("new companion accepted")
	}
	if _, err := f.client.PrepareCleanupServer(t.Context(), f.server, time.Now()); err == nil {
		t.Fatal("automatic cleanup captured orphan binding")
	}
}
