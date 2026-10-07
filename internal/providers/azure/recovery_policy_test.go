package azure

import (
	"bytes"
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
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

type azureRecoveryCredential struct{}

func (azureRecoveryCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "test", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

type azureRecoveryTransport func(*http.Request) (*http.Response, error)

func (f azureRecoveryTransport) Do(req *http.Request) (*http.Response, error) { return f(req) }

type azureRecoveryFixture struct {
	client       *nativeAzureClient
	server       core.Server
	objects      map[string]map[string]any
	allowDelete  bool
	deletes      []string
	beforeRead   func(string)
	readErr      error
	reads        []string
	failRead     string
	failStatus   int
	failCode     string
	inventoryVMs []any
	mu           sync.Mutex
}

func newAzureRecoveryFixture(t *testing.T) *azureRecoveryFixture {
	t.Helper()
	lease, slug := "cbx_123456abcdef", "orphan"
	name := core.LeaseProviderName(lease, slug)
	f := &azureRecoveryFixture{server: core.Server{CloudID: name, Name: name, ImmutableID: "original-vm", Labels: map[string]string{
		"crabbox": "true", "created_by": "crabbox", "provider": "azure", "lease": lease, "slug": slug,
		"provider_key": core.ProviderKeyForLease(lease), "fixed_attempt": "attempt", "fixed_intent_sha256": strings.Repeat("a", 64),
	}}, objects: make(map[string]map[string]any)}
	prefix := "/subscriptions/sub/resourceGroups/rg/providers/"
	for suffix, kind := range map[string]string{"-nic": "Microsoft.Network/networkInterfaces", "-pip": "Microsoft.Network/publicIPAddresses", "-osdisk": "Microsoft.Compute/disks", "-q-nsg": "Microsoft.Network/networkSecurityGroups"} {
		f.objects[name+suffix] = map[string]any{"id": prefix + kind + "/" + name + suffix, "name": name + suffix, "tags": azureTagsFromLabels(f.server.Labels), "properties": map[string]any{"resourceGuid": suffix + "-guid", "uniqueId": suffix + "-guid", "diskState": "Unattached"}}
	}
	transport := azureRecoveryTransport(func(req *http.Request) (*http.Response, error) {
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
		if req.Method == http.MethodGet && name == "virtualMachines" {
			status, body = http.StatusOK, map[string]any{"value": f.inventoryVMs}
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
	network, err := armnetwork.NewClientFactory("sub", azureRecoveryCredential{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	compute, err := armcompute.NewClientFactory("sub", azureRecoveryCredential{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	f.client = &nativeAzureClient{AzureClient: &core.AzureClient{SubscriptionID: "sub", ResourceGroup: "rg"}, AzureResourceClients: core.AzureResourceClients{Interfaces: network.NewInterfacesClient(), PublicIPs: network.NewPublicIPAddressesClient(), SecurityGroups: network.NewSecurityGroupsClient(), Disks: compute.NewDisksClient(), VirtualMachines: compute.NewVirtualMachinesClient()}}
	return f
}

func TestAzureFixedRejectedCompanionSettlement(t *testing.T) {
	for _, scenario := range []string{"owned", "borrowed NIC", "changed attempt", "attached NIC", "borrowed public IP", "uncertain read", "remaining disk", "VM appeared", "blank identity"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAzureRecoveryFixture(t)
			f.server.ImmutableID = ""
			delete(f.objects, f.server.CloudID+"-osdisk")
			delete(f.objects, f.server.CloudID+"-q-nsg")
			f.allowDelete = true
			binding := core.AzureFixedCompanions{NICGUID: "-nic-guid", PublicIPGUID: "-pip-guid"}
			switch scenario {
			case "blank identity":
				binding.NICGUID, binding.PublicIPGUID = " ", " "
				f.objects[f.server.CloudID+"-nic"]["properties"].(map[string]any)["resourceGuid"] = " "
				f.objects[f.server.CloudID+"-pip"]["properties"].(map[string]any)["resourceGuid"] = " "
			case "borrowed NIC":
				f.objects[f.server.CloudID+"-nic"]["properties"].(map[string]any)["resourceGuid"] = "replacement"
			case "changed attempt":
				f.objects[f.server.CloudID+"-nic"]["tags"].(map[string]string)["fixed_attempt"] = "other"
			case "attached NIC":
				f.objects[f.server.CloudID+"-nic"]["properties"].(map[string]any)["virtualMachine"] = map[string]any{"id": "other"}
			case "borrowed public IP":
				f.objects[f.server.CloudID+"-pip"]["properties"].(map[string]any)["natGateway"] = map[string]any{"id": "other"}
			case "uncertain read":
				f.failRead = f.server.CloudID + "-nic"
			case "remaining disk":
				f.objects[f.server.CloudID+"-osdisk"] = map[string]any{"name": "unsettled"}
			case "VM appeared":
				f.objects[f.server.CloudID] = map[string]any{"name": "unexpected"}
			}
			err := f.client.SettleRejectedFixedCompanions(t.Context(), f.server, binding)
			if scenario == "owned" {
				if err != nil || len(f.deletes) != 2 || len(f.objects) != 0 {
					t.Fatalf("settlement err=%v deletes=%v remaining=%v", err, f.deletes, f.objects)
				}
			} else if err == nil || len(f.deletes) != 0 {
				t.Fatalf("uncertain custody settled: err=%v deletes=%v", err, f.deletes)
			}
		})
	}
}

func TestAzureFailedLeaseHoldRetainsCompanionsWithoutMutation(t *testing.T) {
	f := newAzureRecoveryFixture(t)
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

func TestAzureFailedLeaseHoldUntaggedDiskRequiresOriginalBinding(t *testing.T) {
	for _, scenario := range []string{"original disk", "replacement disk", "missing binding", "incomplete binding", "unbound disk", "conflicting tags", "attached disk"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAzureRecoveryFixture(t)
			f.server.Labels[core.AzureCleanupBindingLabel] = "v1"
			f.server.Labels[azureCleanupNICIdentityLabel] = "-nic-guid"
			f.server.Labels[azureCleanupPublicIPIdentityLabel] = "-pip-guid"
			f.server.Labels[azureCleanupDiskIdentityLabel] = "-osdisk-guid"
			disk := f.objects[f.server.CloudID+"-osdisk"]
			delete(disk, "tags") // Azure image-created disks do not inherit VM tags.
			switch scenario {
			case "replacement disk":
				disk["properties"].(map[string]any)["uniqueId"] = "replacement"
			case "missing binding":
				delete(f.server.Labels, core.AzureCleanupBindingLabel)
			case "incomplete binding":
				delete(f.server.Labels, azureCleanupNICIdentityLabel)
			case "unbound disk":
				delete(f.server.Labels, azureCleanupDiskIdentityLabel)
			case "conflicting tags":
				disk["tags"] = azureTagsFromLabels(f.server.Labels)
				disk["tags"].(map[string]string)["lease"] = "cbx_abcdef123456"
			case "attached disk":
				disk["managedBy"] = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/other"
			}
			receipt, err := f.client.InspectFailedLeaseHold(t.Context(), f.server)
			if scenario == "original disk" {
				if err != nil {
					t.Fatal(err)
				}
				if len(receipt.Resources) != 5 || receipt.Resources[3].State != "retained" || receipt.Resources[3].ImmutableID != "-osdisk-guid" {
					t.Fatalf("original untagged disk was not retained: %+v", receipt.Resources)
				}
			} else if err == nil {
				t.Fatal("unproven disk hold accepted")
			}
			if len(f.deletes) != 0 || len(f.objects) != 4 {
				t.Fatal("hold mutated Azure resources")
			}
		})
	}
}

func TestAzureFailedLeaseHoldRefusesUnprovenOwnership(t *testing.T) {
	for _, failure := range []string{"foreign tags", "fixed attempt", "attached disk", "public IP NAT", "read denied", "VM reappears"} {
		t.Run(failure, func(t *testing.T) {
			f := newAzureRecoveryFixture(t)
			disk := f.objects[f.server.CloudID+"-osdisk"]
			switch failure {
			case "foreign tags":
				disk["tags"] = map[string]string{}
			case "fixed attempt":
				disk["tags"].(map[string]string)["fixed_attempt"] = "other"
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

func TestAzureClaimlessHoldObservesDiskWithoutGrantingDelete(t *testing.T) {
	f := newAzureRecoveryFixture(t)
	name := f.server.CloudID
	disk := f.objects[name+"-osdisk"]
	disk["tags"] = map[string]string{} // Image-created disks need not inherit VM tags.
	clear(f.objects)
	f.objects[name+"-osdisk"] = disk
	receipt, err := f.client.InspectClaimlessFailedLeaseHold(t.Context(), f.server.Labels["lease"], f.server.Labels["slug"])
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Resources) != 5 || receipt.Resources[0].State != "absent" || receipt.Resources[3].Kind != "disk" ||
		receipt.Resources[3].State != "retained" || receipt.Resources[3].ImmutableID != "-osdisk-guid" {
		t.Fatalf("claimless hold did not preserve observed resource facts: %+v", receipt.Resources)
	}
	if len(f.deletes) != 0 || f.objects[name+"-osdisk"] == nil {
		t.Fatal("claimless hold mutated the retained disk")
	}
}

func TestAzureClaimlessHoldRefusesUnprovenVMOrInventory(t *testing.T) {
	for _, scenario := range []string{"VM present", "VM reappears", "same lease elsewhere", "foreign NIC", "attached disk", "wrong scope", "read denied", "inventory denied", "inventory missing", "nothing retained"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAzureRecoveryFixture(t)
			name := f.server.CloudID
			disk := f.objects[name+"-osdisk"]
			clear(f.objects)
			f.objects[name+"-osdisk"] = disk
			switch scenario {
			case "VM present":
				f.objects[name] = map[string]any{"name": name}
			case "VM reappears":
				reads := 0
				f.beforeRead = func(resource string) {
					if resource == name {
						reads++
						if reads == 2 {
							f.objects[name] = map[string]any{"name": name}
						}
					}
				}
			case "same lease elsewhere":
				f.inventoryVMs = []any{map[string]any{"id": "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/other", "name": "other", "tags": azureTagsFromLabels(f.server.Labels)}}
			case "foreign NIC":
				f.objects[name+"-nic"] = map[string]any{"id": "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/" + name + "-nic", "name": name + "-nic", "tags": map[string]string{"lease": "other"}, "properties": map[string]any{"resourceGuid": "foreign"}}
			case "attached disk":
				disk["managedBy"] = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/other"
			case "wrong scope":
				f.client.ResourceGroup = "other"
			case "read denied":
				f.failRead = name + "-osdisk"
			case "inventory denied":
				f.failRead = "virtualMachines"
			case "inventory missing":
				f.failRead, f.failStatus, f.failCode = "virtualMachines", http.StatusNotFound, "ResourceGroupNotFound"
			case "nothing retained":
				clear(f.objects)
			}
			if _, err := f.client.InspectClaimlessFailedLeaseHold(t.Context(), f.server.Labels["lease"], f.server.Labels["slug"]); err == nil {
				t.Fatal("unproven claimless hold was accepted")
			}
			if len(f.deletes) != 0 {
				t.Fatal("failed claimless hold mutated Azure")
			}
		})
	}
}

func TestAzureUnboundHoldUsesOriginalAttemptWithoutDeleteAuthority(t *testing.T) {
	for _, scenario := range []string{"owned", "untagged disk", "changed attempt", "changed network", "live VM", "denied read"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAzureRecoveryFixture(t)
			f.server.ImmutableID = ""
			binding := core.AzureFixedCompanions{NICGUID: "-nic-guid", PublicIPGUID: "-pip-guid"}
			switch scenario {
			case "untagged disk":
				delete(f.objects[f.server.CloudID+"-osdisk"], "tags")
			case "changed attempt":
				f.objects[f.server.CloudID+"-nic"]["tags"].(map[string]string)["fixed_attempt"] = "another-attempt"
			case "changed network":
				f.objects[f.server.CloudID+"-pip"]["properties"].(map[string]any)["resourceGuid"] = "replacement"
			case "live VM":
				f.objects[f.server.CloudID] = map[string]any{"name": f.server.CloudID}
			case "denied read":
				f.failRead = f.server.CloudID + "-nic"
			}
			receipt, err := f.client.InspectUnboundFailedLeaseHold(t.Context(), f.server, binding)
			wantOK := scenario == "owned" || scenario == "untagged disk"
			if (err == nil) != wantOK {
				t.Fatalf("hold error=%v", err)
			}
			if wantOK && (receipt.Status != "held" || receipt.Resources[0].ImmutableID != "" || core.HasAzureCleanupBinding(f.server.Labels)) {
				t.Fatal("observation invented original VM or disk custody")
			}
			if len(f.deletes) != 0 {
				t.Fatal("unbound hold mutated Azure")
			}
		})
	}
}

func TestAzureFixedVMShortageRequiresStructuredCapacityCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"allocation failed", &azcore.ResponseError{ErrorCode: "AllocationFailed"}, true},
		{"zonal allocation failed", &azcore.ResponseError{ErrorCode: "ZonalAllocationFailed"}, true},
		{"policy", &azcore.ResponseError{ErrorCode: "OperationNotAllowed"}, false},
		{"quota", &azcore.ResponseError{ErrorCode: "QuotaExceeded"}, false},
		{"missing", &azcore.ResponseError{ErrorCode: "ResourceNotFound"}, false},
		{"human prose", errors.New("AllocationFailed: out of capacity"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var shortage *azureFixedVMShortage
			if got := errors.As(classifyAzureFixedCreateError(&core.AzureVMCreateError{Terminal: true, Err: tc.err}), &shortage); got != tc.want {
				t.Fatalf("typed shortage=%v want=%v", got, tc.want)
			}
			if errors.As(classifyAzureFixedCreateError(&core.AzureVMCreateError{Terminal: false, Err: tc.err}), &shortage) {
				t.Fatal("nonterminal polling failure became a definitive rejection")
			}
		})
	}
}

func azureTagsFromLabels(labels map[string]string) map[string]string {
	tags := make(map[string]string, len(labels))
	for key, value := range labels {
		if !strings.HasPrefix(key, "_crabbox_azure_cleanup_") {
			tags[key] = value
		}
	}
	return tags
}

func TestAzureHoldFinalizationNativeAbsence(t *testing.T) {
	for _, scenario := range []string{"absent", "deleted group", "VM", "NIC", "public IP", "disk", "NSG", "denied read", "unknown disk absence", "transport failure", "incomplete inventory", "inventory denied", "inventory unknown missing", "other lease VM", "VM reappears"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAzureRecoveryFixture(t)
			objects := f.objects
			f.objects = make(map[string]map[string]any)
			name := f.server.CloudID
			switch scenario {
			case "VM":
				f.objects[name] = map[string]any{"name": name}
			case "NIC":
				f.objects[name+"-nic"] = objects[name+"-nic"]
			case "public IP":
				f.objects[name+"-pip"] = objects[name+"-pip"]
			case "disk":
				f.objects[name+"-osdisk"] = objects[name+"-osdisk"]
			case "NSG":
				f.objects[name+"-q-nsg"] = objects[name+"-q-nsg"]
			case "denied read":
				f.failRead = name + "-osdisk"
			case "unknown disk absence":
				f.failRead, f.failStatus, f.failCode = name+"-osdisk", http.StatusNotFound, "UnknownEndpoint"
			case "transport failure":
				f.readErr = errors.New("network unavailable")
			case "incomplete inventory":
				f.inventoryVMs = []any{map[string]any{"name": "", "id": ""}}
			case "inventory denied":
				f.failRead = "virtualMachines"
			case "inventory unknown missing":
				f.failRead, f.failStatus, f.failCode = "virtualMachines", http.StatusNotFound, "ResourceNotFound"
			case "deleted group":
				f.failRead, f.failStatus, f.failCode = "virtualMachines", http.StatusNotFound, "ResourceGroupNotFound"
			case "other lease VM":
				f.inventoryVMs = []any{map[string]any{"name": "other", "id": "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/other", "tags": azureTagsFromLabels(f.server.Labels)}}
			case "VM reappears":
				reads := 0
				f.beforeRead = func(resource string) {
					if resource == name {
						reads++
						if reads == 2 {
							f.objects[name] = map[string]any{"name": name}
						}
					}
				}
			}
			receipt, err := f.client.VerifyFailedLeaseHoldAbsent(t.Context(), f.server.Labels["lease"], f.server.Labels["slug"])
			wantOK := scenario == "absent" || scenario == "deleted group"
			if (err == nil) != wantOK {
				t.Fatalf("finalization absence err=%v", err)
			}
			if wantOK {
				if receipt.Schema != "crabbox.lease-hold.v1" || receipt.Status != "finalized" || receipt.UnacceptedChanges != "unknown" || len(receipt.Resources) != 5 {
					t.Fatalf("invalid finalization evidence: %+v", receipt)
				}
				for i, kind := range []string{"vm", "nic", "public-ip", "disk", "nsg"} {
					if receipt.Resources[i].Kind != kind || receipt.Resources[i].State != "absent" || receipt.Resources[i].ID == "" {
						t.Fatalf("invalid resource evidence: %+v", receipt.Resources[i])
					}
				}
			}
			if len(f.deletes) != 0 {
				t.Fatal("absence verification mutated Azure")
			}
		})
	}
}

func TestAzureHoldFinalizeThroughNativeClient(t *testing.T) {
	for _, scenario := range []string{"absent", "retained disk", "failed read"} {
		t.Run(scenario, func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			t.Chdir(t.TempDir())
			t.Setenv("CRABBOX_PROVIDER", "azure")
			t.Setenv("CRABBOX_COORDINATOR", "")
			f := newAzureRecoveryFixture(t)
			old := newAzureClient
			newAzureClient = func(context.Context, core.Config) (azureClient, error) { return f.client, nil }
			t.Cleanup(func() { newAzureClient = old })
			backend := NewAzureLeaseBackend(Provider{}.Spec(), core.BaseConfig(), core.Runtime{Stderr: io.Discard}).(*azureLeaseBackend)
			id, slug := f.server.Labels["lease"], f.server.Labels["slug"]
			if _, err := backend.HoldFailedLeaseWithSlug(t.Context(), id, slug); err != nil {
				t.Fatal(err)
			}
			before, err := core.ReadLeaseClaim(id)
			if err != nil {
				t.Fatal(err)
			}
			disk := f.objects[f.server.CloudID+"-osdisk"]
			clear(f.objects)
			if scenario == "retained disk" {
				f.objects[f.server.CloudID+"-osdisk"] = disk
			}
			if scenario == "failed read" {
				f.failRead = f.server.CloudID + "-osdisk"
			}
			var output bytes.Buffer
			app := core.App{Stdout: &output, Stderr: io.Discard}
			args := []string{"hold", "--provider", "azure", "--id", id, "--finalize", "--json"}
			err = app.Run(t.Context(), args)
			after, readErr := core.ReadLeaseClaim(id)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if scenario == "absent" {
				if err != nil || after.RecoveryHold.Status != "finalized" {
					t.Fatalf("native finalization failed: %v", err)
				}
				first := output.String()
				reads := len(f.reads)
				f.readErr = errors.New("terminal retry must not query Azure")
				output.Reset()
				if err := app.Run(t.Context(), args); err != nil || output.String() != first || len(f.reads) != reads {
					t.Fatalf("terminal replay changed result or touched Azure: %v", err)
				}
			} else if err == nil || output.Len() != 0 || !reflect.DeepEqual(before, after) {
				t.Fatalf("unverified finalization changed held custody: %v", err)
			}
			if len(f.deletes) != 0 {
				t.Fatal("native finalization mutated cloud resources")
			}
		})
	}
}
