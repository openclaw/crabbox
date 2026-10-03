package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
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
	client      *AzureClient
	server      Server
	objects     map[string]map[string]any
	allowDelete bool
	deletes     []string
	beforeRead  func(string)
	readErr     error
	reads       []string
	failRead    string
	failStatus  int
	failCode    string
	mu          sync.Mutex
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
	transport := azureOrphanTransport(func(req *http.Request) (*http.Response, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		f.reads = append(f.reads, name)
		if f.beforeRead != nil {
			f.beforeRead(name)
		}
		if f.readErr != nil {
			return nil, f.readErr
		}
		object, exists := f.objects[name]
		status, body := http.StatusOK, any(object)
		if !exists {
			status, body = http.StatusNotFound, map[string]any{"error": map[string]any{"code": "ResourceNotFound", "message": "absent"}}
		}
		if req.Method == http.MethodGet && name == f.failRead {
			status, code := f.failStatus, f.failCode
			if status == 0 {
				status, code = http.StatusForbidden, "AuthorizationFailed"
			}
			data, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": "simulated read failure"}})
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data))), Request: req}, nil
		}
		if req.Method == http.MethodDelete && f.allowDelete {
			f.deletes = append(f.deletes, name)
			delete(f.objects, name)
			return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		}
		if req.Method != http.MethodGet {
			f.deletes = append(f.deletes, name)
			t.Errorf("recovery attempted mutation: %s %s", req.Method, req.URL.Path)
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

func TestAzureOrphanCleanupAbsent(t *testing.T) {
	f := newAzureOrphanFixture(t)
	clear(f.objects)
	prepared, err := f.client.PrepareOwnedServer(t.Context(), f.server)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prepared, f.server) {
		t.Fatal("absence recovery changed claim format")
	}
	for range 2 {
		if err := f.client.DeleteOwnedServer(t.Context(), prepared); err != nil {
			t.Fatal(err)
		}
	}
	for _, suffix := range []string{"", "-nic", "-pip", "-osdisk", "-q-nsg"} {
		found := false
		for _, name := range f.reads {
			if name == f.server.CloudID+suffix {
				found = true
			}
		}
		if !found {
			t.Fatalf("did not verify %s absence", suffix)
		}
	}
	if len(f.deletes) != 0 {
		t.Fatal("absence recovery mutated Azure")
	}
	if _, err := f.client.PrepareCleanupServer(t.Context(), f.server, time.Now()); err == nil {
		t.Fatal("automatic cleanup initiated recovery")
	}
}

func TestAzureFailedLeaseHoldRetainsCompanionsWithoutMutation(t *testing.T) {
	f := newAzureOrphanFixture(t)
	receipt, err := f.client.InspectFailedLeaseHold(t.Context(), f.server)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Schema != "crabbox.lease-hold.v1" || receipt.LeaseID != f.server.Labels["lease"] || receipt.Status != "held" || receipt.UnacceptedChanges != "unknown" {
		t.Fatalf("unexpected hold receipt: %+v", receipt)
	}
	if len(receipt.Resources) != 5 {
		t.Fatalf("resource inventory: %+v", receipt.Resources)
	}
	for _, resource := range receipt.Resources {
		if resource.Kind == "vm" {
			if resource.State != "absent" {
				t.Fatal("VM absence was not proved")
			}
		} else if resource.State != "retained" || resource.ImmutableID == "" {
			t.Fatalf("companion was not retained: %+v", resource)
		}
	}
	if len(f.deletes) != 0 || len(f.objects) != 4 {
		t.Fatal("hold changed Azure resources")
	}
}

func TestAzureFailedLeaseHoldRefusesUnprovenOwnership(t *testing.T) {
	for _, failure := range []string{"foreign tags", "fixed attempt", "attached disk", "public IP NAT", "read denied", "VM reappears"} {
		t.Run(failure, func(t *testing.T) {
			f := newAzureOrphanFixture(t)
			disk := f.objects[f.server.CloudID+"-osdisk"]
			switch failure {
			case "foreign tags":
				disk["tags"] = map[string]string{}
			case "fixed attempt":
				disk["tags"].(map[string]string)[azureLabelToTagKey("fixed_attempt")] = "other"
			case "attached disk":
				disk["managedBy"] = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/other"
			case "public IP NAT":
				f.objects[f.server.CloudID+"-pip"]["properties"].(map[string]any)["natGateway"] = map[string]any{"id": "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/natGateways/shared"}
			case "read denied":
				f.failRead = f.server.CloudID + "-nic"
			case "VM reappears":
				reads := 0
				f.beforeRead = func(name string) {
					if name == f.server.CloudID {
						reads++
						if reads == 2 {
							f.objects[name] = map[string]any{"name": name}
						}
					}
				}
			}
			if _, err := f.client.InspectFailedLeaseHold(t.Context(), f.server); err == nil {
				t.Fatal("unproven hold accepted")
			}
			if len(f.deletes) != 0 {
				t.Fatal("failed hold changed Azure")
			}
		})
	}
}

func TestAzureOrphanCleanupRejectsRemainingResources(t *testing.T) {
	for _, suffix := range []string{"", "-nic", "-pip", "-osdisk", "-q-nsg"} {
		for _, tags := range []string{"owned", "foreign", "untagged"} {
			t.Run(suffix+"/"+tags, func(t *testing.T) {
				f := newAzureOrphanFixture(t)
				object := f.objects[f.server.CloudID+suffix]
				if object == nil {
					object = map[string]any{"name": f.server.CloudID}
				}
				if tags == "foreign" {
					object["tags"] = map[string]string{"lease": "cbx_abcdef123456"}
				}
				if tags == "untagged" {
					delete(object, "tags")
				}
				clear(f.objects)
				prepared, err := f.client.PrepareOwnedServer(t.Context(), f.server)
				if err != nil {
					t.Fatal(err)
				}
				f.objects[f.server.CloudID+suffix] = object
				if err := f.client.DeleteOwnedServer(t.Context(), prepared); err == nil {
					t.Fatal("replacement accepted")
				}
				if _, err := f.client.PrepareOwnedServer(t.Context(), f.server); err == nil {
					t.Fatal("remaining resource accepted")
				}
				if len(f.deletes) != 0 || len(f.objects) != 1 {
					t.Fatal("remaining resource mutated")
				}
			})
		}
	}
}

func TestAzureOrphanCleanupReadFailuresRetainClaim(t *testing.T) {
	for _, suffix := range []string{"", "-nic", "-pip", "-osdisk", "-q-nsg"} {
		t.Run(suffix, func(t *testing.T) {
			f := newAzureOrphanFixture(t)
			clear(f.objects)
			f.failRead = f.server.CloudID + suffix
			if _, err := f.client.PrepareOwnedServer(t.Context(), f.server); err == nil {
				t.Fatal("failed read accepted")
			}
			if err := f.client.DeleteOwnedServer(t.Context(), f.server); err == nil {
				t.Fatal("failed final read accepted")
			}
			f.failRead = ""
			if err := f.client.DeleteOwnedServer(t.Context(), f.server); err != nil {
				t.Fatal(err)
			}
		})
	}
	f := newAzureOrphanFixture(t)
	clear(f.objects)
	f.readErr = errors.New("transport failure mentioning ResourceNotFound")
	if err := f.client.DeleteOwnedServer(t.Context(), f.server); err == nil {
		t.Fatal("text-only not-found accepted")
	}
}

func TestAzureOrphanCleanupVMReappearsDuringVerification(t *testing.T) {
	f := newAzureOrphanFixture(t)
	clear(f.objects)
	f.beforeRead = func(name string) {
		if strings.HasSuffix(name, "-q-nsg") {
			f.objects[f.server.CloudID] = map[string]any{"name": f.server.CloudID}
		}
	}
	if _, err := f.client.PrepareOwnedServer(t.Context(), f.server); err == nil {
		t.Fatal("reappearing VM accepted")
	}
	if len(f.deletes) != 0 {
		t.Fatal("reappearing VM mutated")
	}
}

func TestAzureOrphanCleanupRequiresFixedIdentity(t *testing.T) {
	for _, key := range []string{"lease", "slug", "provider_key", "fixed_attempt", "fixed_intent_sha256"} {
		t.Run(key, func(t *testing.T) {
			f := newAzureOrphanFixture(t)
			clear(f.objects)
			delete(f.server.Labels, key)
			if err := f.client.DeleteOwnedServer(t.Context(), f.server); err == nil {
				t.Fatal("incomplete claim accepted")
			}
			if len(f.reads) != 0 {
				t.Fatal("invalid identity reached Azure")
			}
		})
	}
}

func TestAzureOrphanCleanupRequiresNativeAbsence(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		absent bool
	}{
		{http.StatusNotFound, "ResourceNotFound", true},
		{http.StatusNotFound, "ResourceGroupNotFound", true},
		{http.StatusNotFound, "SubscriptionNotFound", false},
		{http.StatusForbidden, "ResourceNotFound", false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			f := newAzureOrphanFixture(t)
			clear(f.objects)
			f.failRead, f.failStatus, f.failCode = f.server.CloudID+"-nic", tc.status, tc.code
			if err := f.client.DeleteOwnedServer(t.Context(), f.server); (err == nil) != tc.absent {
				t.Fatalf("absence=%t: %v", tc.absent, err)
			}
		})
	}
}

// Model the actual SDK request shape and relationships at preparation time.
// DELETE remains name-addressed, exactly as in the ordinary provider path.
func (f *azureOrphanFixture) addPreparationVM() {
	name := f.server.CloudID
	vmID := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/" + name
	nicID := f.objects[name+"-nic"]["id"]
	pipID := f.objects[name+"-pip"]["id"]
	diskID := f.objects[name+"-osdisk"]["id"]
	nsgID := f.objects[name+"-q-nsg"]["id"]
	f.objects[name] = map[string]any{"id": vmID, "name": name, "location": "eastus", "tags": azureTagsFromLabels(f.server.Labels), "properties": map[string]any{
		"vmId":           f.server.ImmutableID,
		"networkProfile": map[string]any{"networkInterfaces": []any{map[string]any{"id": nicID}}},
		"storageProfile": map[string]any{"osDisk": map[string]any{"managedDisk": map[string]any{"id": diskID}}},
	}}
	nic := f.objects[name+"-nic"]["properties"].(map[string]any)
	nic["ipConfigurations"] = []any{map[string]any{"name": "primary", "properties": map[string]any{"publicIPAddress": map[string]any{"id": pipID}}}}
	nic["networkSecurityGroup"] = map[string]any{"id": nsgID}
}

func TestAzurePreparedOrphanCleanup(t *testing.T) {
	f := newAzureOrphanFixture(t)
	f.addPreparationVM()
	prepared, err := f.client.PrepareOwnedServer(t.Context(), f.server)
	if err != nil {
		t.Fatal(err)
	}
	if !HasAzureCleanupBinding(prepared.Labels) || prepared.Labels[azureCleanupDiskIdentityLabel] != "-osdisk-guid" {
		t.Fatal("preparation did not capture genuine companion identities")
	}
	delete(f.objects, f.server.CloudID) // Only the VM is subsequently lost.
	f.objects["unrelated"] = map[string]any{"name": "unrelated"}
	f.allowDelete = true
	if err := f.client.DeleteOwnedServer(t.Context(), prepared); err != nil {
		t.Fatal(err)
	}
	if len(f.deletes) != 4 || len(f.objects) != 1 || f.objects["unrelated"] == nil {
		t.Fatalf("cleanup did not isolate companions: deleted=%v remaining=%v", f.deletes, f.objects)
	}
	// Existing all-kind absence confirmation is idempotent; no count refund.
	if err := f.client.DeleteOwnedServer(t.Context(), prepared); err != nil {
		t.Fatal(err)
	}
	if len(f.deletes) != 4 {
		t.Fatal("absence replay issued duplicate deletion")
	}
}

func TestAzurePreparedOrphanRejectsReplacementAndForeign(t *testing.T) {
	for _, suffix := range []string{"", "-nic", "-pip", "-osdisk", "-q-nsg"} {
		t.Run("replacement"+suffix, func(t *testing.T) {
			f := newAzureOrphanFixture(t)
			f.addPreparationVM()
			prepared, err := f.client.PrepareOwnedServer(t.Context(), f.server)
			if err != nil {
				t.Fatal(err)
			}
			if suffix != "" {
				delete(f.objects, f.server.CloudID)
			}
			properties := f.objects[f.server.CloudID+suffix]["properties"].(map[string]any)
			key := "resourceGuid"
			if suffix == "" {
				key = "vmId"
			} else if suffix == "-osdisk" {
				key = "uniqueId"
			}
			properties[key] = "replacement"
			f.allowDelete = true
			if err := f.client.DeleteOwnedServer(t.Context(), prepared); err == nil || len(f.deletes) != 0 {
				t.Fatalf("observed replacement accepted: err=%v deletes=%v", err, f.deletes)
			}
			if suffix != "" {
				f.addPreparationVM()
				if _, err := f.client.PrepareOwnedServer(t.Context(), prepared); err == nil {
					t.Fatal("replay recaptured replacement identity")
				}
			}
		})
	}
	for _, suffix := range []string{"-nic", "-pip", "-q-nsg"} {
		t.Run("foreign"+suffix, func(t *testing.T) {
			f := newAzureOrphanFixture(t)
			f.addPreparationVM()
			prepared, err := f.client.PrepareOwnedServer(t.Context(), f.server)
			if err != nil {
				t.Fatal(err)
			}
			delete(f.objects, f.server.CloudID)
			f.objects[f.server.CloudID+suffix]["tags"].(map[string]string)["lease"] = "cbx_abcdef123456"
			f.allowDelete = true
			if err := f.client.DeleteOwnedServer(t.Context(), prepared); err == nil || len(f.deletes) != 0 {
				t.Fatalf("foreign companion accepted: err=%v deletes=%v", err, f.deletes)
			}
		})
	}
}

func TestAzurePreparationRejectsUnknownIdentityAndForeignLink(t *testing.T) {
	for _, failure := range []string{"identity", "link"} {
		t.Run(failure, func(t *testing.T) {
			f := newAzureOrphanFixture(t)
			f.addPreparationVM()
			nic := f.objects[f.server.CloudID+"-nic"]["properties"].(map[string]any)
			if failure == "identity" {
				delete(nic, "resourceGuid")
			} else {
				nic["networkSecurityGroup"] = map[string]any{"id": "/subscriptions/foreign/resourceGroups/rg/providers/Microsoft.Network/networkSecurityGroups/shared"}
			}
			if _, err := f.client.PrepareOwnedServer(t.Context(), f.server); err == nil || len(f.deletes) != 0 {
				t.Fatalf("unknown/foreign capture accepted: %v", err)
			}
		})
	}
}
