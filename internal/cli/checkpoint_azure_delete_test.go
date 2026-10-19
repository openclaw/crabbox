package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v8"
)

const azureCheckpointSnapshotID = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/snapshots/checkpoint"

func testAzureCheckpointClient(t *testing.T, handler func(*http.Request) (int, string)) *AzureClient {
	t.Helper()
	transport := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		code, body := handler(req)
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	factory, err := armcompute.NewClientFactory("sub", azureTestCredential{}, &arm.ClientOptions{ClientOptions: policy.ClientOptions{
		Transport: transport, Retry: policy.RetryOptions{MaxRetries: -1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return &AzureClient{SubscriptionID: "sub", ResourceGroup: "rg", Location: "eastus", snapc: factory.NewSnapshotsClient(), vmc: factory.NewVirtualMachinesClient()}
}

func TestAzureCheckpointDeleteIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, savedID, savedUniqueID, currentID, currentUniqueID string
		readStatus, deleteStatus                                 int
		wantDetached, wantRetained                               bool
		wantError                                                string
		wantCalls                                                []string
	}{
		{name: "same identity", wantCalls: []string{"GET", "DELETE"}},
		{name: "case insensitive ARM ID", currentID: strings.ToUpper(azureCheckpointSnapshotID), wantCalls: []string{"GET", "DELETE"}},
		{name: "replacement", currentUniqueID: "replacement", wantDetached: true, wantRetained: true, wantError: "does not match", wantCalls: []string{"GET"}},
		{name: "legacy", savedUniqueID: "missing", wantDetached: true, wantRetained: true, wantError: "legacy"},
		{name: "current identity missing", currentUniqueID: "missing", wantDetached: true, wantRetained: true, wantError: "does not match", wantCalls: []string{"GET"}},
		{name: "current resource ID missing", currentID: "missing", wantDetached: true, wantRetained: true, wantError: "does not match", wantCalls: []string{"GET"}},
		{name: "contradictory resource ID", currentID: strings.Replace(azureCheckpointSnapshotID, "/rg/", "/other/", 1), wantDetached: true, wantRetained: true, wantError: "does not match", wantCalls: []string{"GET"}},
		{name: "wrong scope", savedID: strings.Replace(azureCheckpointSnapshotID, "/rg/", "/other/", 1), wantDetached: true, wantRetained: true, wantError: "deletion scope"},
		{name: "name only", savedID: "checkpoint", wantDetached: true, wantRetained: true, wantError: "deletion scope"},
		{name: "absent", readStatus: http.StatusNotFound, wantCalls: []string{"GET"}},
		{name: "inspection failure", readStatus: http.StatusForbidden, wantRetained: true, wantError: "verify OS disk snapshot identity", wantCalls: []string{"GET"}},
		{name: "deletion failure", deleteStatus: http.StatusForbidden, wantRetained: true, wantError: "begin delete", wantCalls: []string{"GET", "DELETE"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
			record := checkpointRecord{ID: "chk_0123456789abcdef", Kind: checkpointKindAzureOS, Provider: "azure"}
			record.Native.ImageID = firstNonBlank(tc.savedID, azureCheckpointSnapshotID)
			record.Native.Resource = record.Native.ImageID
			record.Native.Region, record.Native.Direct, record.Native.State = "eastus", true, "available"
			record.Native.ResourceUniqueID = firstNonBlank(tc.savedUniqueID, "original")
			if tc.savedUniqueID == "missing" {
				record.Native.ResourceUniqueID = ""
			}
			store := checkpointStore{root: t.TempDir()}
			if _, _, err := store.Reserve(record); err != nil {
				t.Fatal(err)
			}
			var calls []string
			client := testAzureCheckpointClient(t, func(req *http.Request) (int, string) {
				calls = append(calls, req.Method)
				if req.URL.Path != azureCheckpointSnapshotID || req.URL.Query().Get("api-version") != "2026-03-02" {
					t.Errorf("unexpected request: %s %s", req.Method, req.URL)
				}
				if req.Method == http.MethodDelete {
					if tc.deleteStatus != 0 {
						return tc.deleteStatus, `{"error":{"code":"AuthorizationFailed","message":"fixture"}}`
					}
					return http.StatusNoContent, ""
				}
				if tc.readStatus != 0 {
					code := "AuthorizationFailed"
					if tc.readStatus == http.StatusNotFound {
						code = "ResourceNotFound"
					}
					return tc.readStatus, fmt.Sprintf(`{"error":{"code":%q,"message":"fixture"}}`, code)
				}
				id, uniqueID := firstNonBlank(tc.currentID, azureCheckpointSnapshotID), firstNonBlank(tc.currentUniqueID, "original")
				if tc.currentID == "missing" {
					id = ""
				}
				if tc.currentUniqueID == "missing" {
					uniqueID = ""
				}
				return http.StatusOK, fmt.Sprintf(`{"id":%q,"properties":{"uniqueId":%q,"provisioningState":"Succeeded"}}`, id, uniqueID)
			})
			originalFactory := newAzureCheckpointClient
			newAzureCheckpointClient = func(context.Context, Config) (*AzureClient, error) { return client, nil }
			t.Cleanup(func() { newAzureCheckpointClient = originalFactory })
			err := deleteCheckpoint(t.Context(), store, record.ID, false)
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("delete error=%v, want %q", err, tc.wantError)
			}
			if !reflect.DeepEqual(calls, tc.wantCalls) {
				t.Fatalf("calls=%v want=%v", calls, tc.wantCalls)
			}
			stored, _, readErr := store.Read(record.ID)
			if !tc.wantRetained {
				if !isCheckpointNotFound(readErr) {
					t.Fatalf("deleted checkpoint retained: %v", readErr)
				}
				return
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
			if (stored.Native.State == "detached") != tc.wantDetached || (stored.Native.DetachedReason != "") != tc.wantDetached {
				t.Fatalf("unexpected retained state: %+v", stored.Native)
			}
			if tc.wantDetached {
				if !strings.Contains(err.Error(), "manual cleanup") || !strings.Contains(err.Error(), "--local-only") {
					t.Fatalf("missing recovery instructions: %v", err)
				}
				previousCalls := len(calls)
				if retryErr := deleteCheckpoint(t.Context(), store, record.ID, false); retryErr == nil || !strings.Contains(retryErr.Error(), "detached") {
					t.Fatalf("retry: %v", retryErr)
				}
				audit, auditErr := (App{}).verifyCheckpointRecord(t.Context(), store, stored)
				if auditErr != nil || audit.ProviderState != "detached" || audit.NextAction != "manual_cleanup" {
					t.Fatalf("audit=%+v err=%v", audit, auditErr)
				}
				var output bytes.Buffer
				printCheckpointInspect(&output, stored)
				if !strings.Contains(output.String(), "detached_reason=") {
					t.Fatalf("inspect hid detached reason: %s", output.String())
				}
				if len(calls) != previousCalls {
					t.Fatal("detached record contacted Azure again")
				}
			}
			beforeLocalOnly := len(calls)
			if err := deleteCheckpoint(t.Context(), store, record.ID, true); err != nil {
				t.Fatal(err)
			}
			if len(calls) != beforeLocalOnly {
				t.Fatal("local-only deletion contacted Azure")
			}
			if _, _, err := store.Read(record.ID); !isCheckpointNotFound(err) {
				t.Fatalf("local-only retained record: %v", err)
			}
		})
	}
}

func TestAzureCheckpointCapturePersistsIdentity(t *testing.T) {
	restarts := 0
	client := testAzureCheckpointClient(t, func(req *http.Request) (int, string) {
		switch {
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/virtualMachines/"):
			return http.StatusOK, `{"properties":{"storageProfile":{"osDisk":{"managedDisk":{"id":"/disks/source"}}}}}`
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/deallocate"):
			return http.StatusOK, ""
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/start"):
			restarts++
			return http.StatusOK, ""
		case req.Method == http.MethodPut && req.URL.Path == azureCheckpointSnapshotID:
			return http.StatusOK, fmt.Sprintf(`{"id":%q,"properties":{"uniqueId":"captured-unique-id","provisioningState":"Succeeded"}}`, azureCheckpointSnapshotID)
		default:
			t.Errorf("unexpected capture request: %s %s", req.Method, req.URL)
			return http.StatusBadRequest, `{"error":{"code":"UnexpectedRequest"}}`
		}
	})
	originalFactory := newAzureCheckpointClient
	newAzureCheckpointClient = func(context.Context, Config) (*AzureClient, error) { return client, nil }
	t.Cleanup(func() { newAzureCheckpointClient = originalFactory })
	image, err := (directAzureOSDiskCheckpointDriver{}).Create(t.Context(), NativeCheckpointCreateRequest{
		Name: "checkpoint", Server: Server{CloudID: "source"}, Strategy: checkpointStrategyDiskSnapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	record := checkpointRecord{ID: "chk_0123456789abcdef"}
	record.applyNativeImage(image, false)
	store := checkpointStore{root: t.TempDir()}
	if _, _, err := store.Reserve(record); err != nil {
		t.Fatal(err)
	}
	stored, _, err := store.Read(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Native.Resource != azureCheckpointSnapshotID || stored.Native.ResourceUniqueID != "captured-unique-id" || restarts != 1 {
		t.Fatalf("capture identity lost: %+v restarts=%d", stored.Native, restarts)
	}
}
