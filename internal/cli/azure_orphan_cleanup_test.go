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
	client     *AzureClient
	server     Server
	objects    map[string]map[string]any
	deletes    []string
	beforeRead func(string)
	readErr    error
	reads      []string
	failRead   string
	failStatus int
	failCode   string
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
