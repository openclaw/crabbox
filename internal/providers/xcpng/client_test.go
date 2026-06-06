package xcpng

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestXMLRPCRequestEncodingKeepsCredentialOutOfURL(t *testing.T) {
	credential := "credential-value"
	body, err := encodeXMLRPCRequest("session.login_with_password", "root", credential, "1.0", "crabbox")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "<methodName>session.login_with_password</methodName>") || !strings.Contains(text, credential) {
		t.Fatalf("body=%s", text)
	}
	endpoint, err := xapiEndpoint("https://xcp-ng.example.test/jsonrpc")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(endpoint, credential) {
		t.Fatalf("endpoint leaked credential: %s", endpoint)
	}
}

func TestXAPIEndpointNormalizesBareHostAndRejectsPlainHTTP(t *testing.T) {
	endpoint, err := xapiEndpoint("xcp-ng.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://xcp-ng.example.test/" {
		t.Fatalf("endpoint=%q", endpoint)
	}
	hostPort, err := xapiEndpoint("xcp-ng.example.test:443")
	if err != nil {
		t.Fatal(err)
	}
	if hostPort != "https://xcp-ng.example.test:443/" {
		t.Fatalf("hostPort=%q", hostPort)
	}
	if _, err := xapiEndpoint("http://xcp-ng.example.test"); err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("err=%v", err)
	}
	loopback, err := xapiEndpoint("http://127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	if loopback != "http://127.0.0.1:8080/" {
		t.Fatalf("loopback=%q", loopback)
	}
}

func TestClientDoctorInventoryUsesListOnly(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "session.login_with_password":
			writeXMLRPCString(t, w, "OpaqueRef:session")
		case "VM.get_all_records":
			writeXMLRPCVMRecords(t, w)
		case "session.logout":
			writeXMLRPCString(t, w, "true")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.XCPNg.APIURL = server.URL
	client, err := newXAPIClient(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	servers, err := client.DoctorInventory(context.Background(), xcpNgProviderConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Labels["lease"] != "cbx_xmlrpc" {
		t.Fatalf("servers=%#v", servers)
	}
	got := strings.Join(methods, ",")
	if got != "session.login_with_password,VM.get_all_records,session.logout" {
		t.Fatalf("methods=%s", got)
	}
}

func TestGuestIPv4SkipsLoopbackAndNonIPv4(t *testing.T) {
	client := &xapiClient{session: "OpaqueRef:session"}
	value := xmlRPCValue{Struct: []xmlRPCMember{
		{Name: "0/ip", Value: xmlRPCValue{String: "127.0.0.1"}},
		{Name: "1/ip", Value: xmlRPCValue{String: "2001:db8::1"}},
		{Name: "2/ip", Value: xmlRPCValue{String: "169.254.10.20"}},
		{Name: "3/ip", Value: xmlRPCValue{String: "192.0.2.55"}},
	}}
	networks := xmlValueToStringMap(value)
	if ip := usableIPv4(networks["0/ip"]); ip != "" {
		t.Fatalf("loopback ip=%s", ip)
	}
	if ip := usableIPv4(networks["1/ip"]); ip != "" {
		t.Fatalf("ipv6 ip=%s", ip)
	}
	if ip := usableIPv4("169.254.10.20"); ip != "" {
		t.Fatalf("link-local ip=%s", ip)
	}
	if ip := usableIPv4(networks["3/ip"]); ip != "192.0.2.55" {
		t.Fatalf("ip=%s", ip)
	}
	_ = client
}

func TestXMLRPCFaultReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeXMLRPCFault(t, w, "MAP_KEY_NOT_FOUND")
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	if _, err := client.call(context.Background(), "VM.remove_from_other_config", "OpaqueRef:session", "OpaqueRef:vm", "missing"); err == nil || !strings.Contains(err.Error(), "MAP_KEY_NOT_FOUND") {
		t.Fatalf("err=%v", err)
	}
}

func TestXMLRPCFaultRedactsSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeXMLRPCFault(t, w, "debug echo secret-password")
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.XCPNg.APIURL = server.URL
	cfg.XCPNg.Password = "secret-password"
	_, err := newXAPIClient(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected XML-RPC fault")
	}
	text := err.Error()
	if strings.Contains(text, cfg.XCPNg.Password) {
		t.Fatalf("fault leaked password: %s", text)
	}
	if !strings.Contains(text, "<redacted>") {
		t.Fatalf("fault did not preserve redacted context: %s", text)
	}
}

func TestXAPIStatusEnvelopeUnwrapsValueAndFailure(t *testing.T) {
	success := xmlRPCValue{Struct: []xmlRPCMember{
		{Name: "Status", Value: xmlRPCValue{CharData: "Success"}},
		{Name: "Value", Value: xmlRPCValue{CharData: "OpaqueRef:vm"}},
	}}
	value, err := unwrapXAPIResponse(success, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := xmlValueToString(value); got != "OpaqueRef:vm" {
		t.Fatalf("value=%q", got)
	}
	failure := xmlRPCValue{Struct: []xmlRPCMember{
		{Name: "Status", Value: xmlRPCValue{CharData: "Failure"}},
		{Name: "ErrorDescription", Value: xmlRPCValue{Array: []xmlRPCValue{{CharData: "HANDLE_INVALID"}, {CharData: "VM"}}}},
	}}
	if _, err := unwrapXAPIResponse(failure, nil); err == nil || !strings.Contains(err.Error(), "HANDLE_INVALID: VM") {
		t.Fatalf("err=%v", err)
	}
}

func TestXAPIStatusEnvelopeRedactsSecrets(t *testing.T) {
	client := &xapiClient{session: "OpaqueRef:secret-session"}
	failure := xmlRPCValue{Struct: []xmlRPCMember{
		{Name: "Status", Value: xmlRPCValue{CharData: "Failure"}},
		{Name: "ErrorDescription", Value: xmlRPCValue{Array: []xmlRPCValue{{CharData: "HANDLE_INVALID"}, {CharData: "OpaqueRef:secret-session"}}}},
	}}
	_, err := unwrapXAPIResponse(failure, client.xapiSecrets("VM.get_record", client.session, "OpaqueRef:vm"))
	if err == nil {
		t.Fatal("expected status failure")
	}
	text := err.Error()
	if strings.Contains(text, "OpaqueRef:secret-session") || strings.Contains(text, "secret-session") {
		t.Fatalf("status error leaked session token: %s", text)
	}
	if !strings.Contains(text, "<redacted>") {
		t.Fatalf("status error did not preserve redacted context: %s", text)
	}
}

func TestXMLValueToStringAcceptsBareCharacterData(t *testing.T) {
	if got := xmlValueToString(xmlRPCValue{CharData: " OpaqueRef:vm\n"}); got != "OpaqueRef:vm" {
		t.Fatalf("value=%q", got)
	}
}

func TestSetVMOtherConfigRemovesBeforeAdding(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		writeXMLRPCString(t, w, "true")
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	if err := client.setVMOtherConfig(context.Background(), "OpaqueRef:vm", map[string]string{"crabbox:labels": "crabbox=true\n"}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(methods, ",")
	if got != "VM.remove_from_other_config,VM.add_to_other_config" {
		t.Fatalf("methods=%s", got)
	}
}

func TestCloneVMUsesCopyForSRAndRewiresVIFsForNetwork(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VM.copy":
			writeXMLRPCString(t, w, "OpaqueRef:vm")
		case "VM.get_VBDs":
			writeXMLRPCStringArray(t, w, []string{"OpaqueRef:root-vbd"})
		case "VBD.get_record":
			writeXMLRPCVBDRecord(t, w, "OpaqueRef:root-vdi")
		case "VDI.get_record":
			writeXMLRPCVDIRecord(t, w, "crabbox-root")
		case "VDI.remove_from_other_config":
			writeXMLRPCFault(t, w, "MAP_KEY_NOT_FOUND")
		case "VDI.add_to_other_config":
			writeXMLRPCString(t, w, "true")
		case "VM.set_affinity":
			writeXMLRPCString(t, w, "true")
		case "VM.get_VIFs":
			writeXMLRPCStringArray(t, w, []string{"OpaqueRef:vif"})
		case "VIF.set_network":
			writeXMLRPCString(t, w, "true")
		case "VM.provision":
			writeXMLRPCString(t, w, "true")
		case "VM.get_uuid":
			writeXMLRPCString(t, w, xcpNgTestVMUUID)
		case "VM.remove_from_other_config":
			writeXMLRPCFault(t, w, "MAP_KEY_NOT_FOUND")
		case "VM.add_to_other_config":
			writeXMLRPCString(t, w, "true")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	vm, err := client.CloneVM(context.Background(), xcpNgCloneRequest{
		TemplateRef: "OpaqueRef:tpl",
		SRRef:       "OpaqueRef:sr",
		NetworkRef:  "OpaqueRef:net",
		HostRef:     "OpaqueRef:host",
		LeaseID:     "cbx_lease",
		Slug:        "blue",
		Labels:      map[string]string{"crabbox": "true", "created_by": "crabbox", "provider": "xcp-ng", "lease": "cbx_lease"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if vm.Ref != "OpaqueRef:vm" || vm.UUID != xcpNgTestVMUUID {
		t.Fatalf("vm=%#v", vm)
	}
	got := strings.Join(methods, ",")
	for _, want := range []string{"VM.copy", "VM.set_affinity", "VM.get_VIFs", "VIF.set_network", "VM.remove_from_other_config", "VM.add_to_other_config", "VM.provision", "VM.get_uuid"} {
		if !strings.Contains(got, want) {
			t.Fatalf("methods=%s missing %s", got, want)
		}
	}
	if strings.Contains(got, "VM.clone") {
		t.Fatalf("methods=%s unexpectedly used VM.clone", got)
	}
}

func TestCloneVMRollbackDestroysCopiedDiskBeforeLabels(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VM.copy":
			writeXMLRPCString(t, w, "OpaqueRef:vm")
		case "VM.set_affinity":
			writeXMLRPCFault(t, w, "HOST_NOT_LIVE")
		case "VM.get_record":
			writeXMLRPCUnmanagedVMRecord(t, w)
		case "VM.get_guest_metrics":
			writeXMLRPCFault(t, w, "HANDLE_INVALID")
		case "VM.get_VBDs":
			writeXMLRPCStringArray(t, w, []string{"OpaqueRef:root-vbd"})
		case "VBD.get_record":
			writeXMLRPCVBDRecord(t, w, "OpaqueRef:root-vdi")
		case "VDI.get_record":
			writeXMLRPCVDIRecord(t, w, "crabbox-root")
		case "VDI.remove_from_other_config":
			writeXMLRPCFault(t, w, "MAP_KEY_NOT_FOUND")
		case "VDI.add_to_other_config":
			writeXMLRPCString(t, w, "true")
		case "VM.get_power_state":
			writeXMLRPCString(t, w, "Halted")
		case "VBD.unplug", "VBD.destroy", "VDI.destroy", "VM.destroy":
			writeXMLRPCString(t, w, "true")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	_, err := client.CloneVM(context.Background(), xcpNgCloneRequest{
		TemplateRef: "OpaqueRef:tpl",
		SRRef:       "OpaqueRef:sr",
		HostRef:     "OpaqueRef:bad-host",
		LeaseID:     "cbx_lease",
		Slug:        "blue",
		Labels:      map[string]string{"crabbox": "true", "created_by": "crabbox", "provider": "xcp-ng", "lease": "cbx_lease"},
	})
	if err == nil {
		t.Fatal("expected host affinity failure")
	}
	got := strings.Join(methods, ",")
	for _, want := range []string{"VM.copy", "VM.set_affinity", "VM.get_record", "VM.get_VBDs", "VBD.get_record", "VDI.get_record", "VBD.unplug", "VBD.destroy", "VDI.destroy", "VM.destroy"} {
		if !strings.Contains(got, want) {
			t.Fatalf("methods=%s missing %s", got, want)
		}
	}
	if strings.Index(got, "VDI.destroy") > strings.Index(got, "VM.destroy") {
		t.Fatalf("methods=%s destroyed VM before copied VDI", got)
	}
}

func TestCloneVMProvisionRollbackDestroysUnlabeledCopiedDisk(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VM.copy":
			writeXMLRPCString(t, w, "OpaqueRef:vm")
		case "VM.remove_from_other_config":
			writeXMLRPCFault(t, w, "MAP_KEY_NOT_FOUND")
		case "VM.add_to_other_config":
			writeXMLRPCString(t, w, "true")
		case "VM.provision":
			writeXMLRPCFault(t, w, "SR_BACKEND_FAILURE")
		case "VM.get_record":
			writeXMLRPCVMRecord(t, w, "cbx_lease")
		case "VM.get_guest_metrics":
			writeXMLRPCFault(t, w, "HANDLE_INVALID")
		case "VDI.get_all_records":
			writeXMLRPCEmptyRecordMap(t, w)
		case "VM.get_VBDs":
			writeXMLRPCStringArray(t, w, []string{"OpaqueRef:root-vbd"})
		case "VBD.get_record":
			writeXMLRPCVBDRecord(t, w, "OpaqueRef:root-vdi")
		case "VDI.get_record":
			writeXMLRPCVDIRecord(t, w, "crabbox-root")
		case "VM.get_power_state":
			writeXMLRPCString(t, w, "Halted")
		case "VBD.unplug", "VBD.destroy", "VDI.destroy", "VM.destroy":
			writeXMLRPCString(t, w, "true")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	_, err := client.CloneVM(context.Background(), xcpNgCloneRequest{
		TemplateRef: "OpaqueRef:tpl",
		SRRef:       "OpaqueRef:sr",
		LeaseID:     "cbx_lease",
		Slug:        "blue",
		Labels:      map[string]string{"crabbox": "true", "created_by": "crabbox", "provider": "xcp-ng", "lease": "cbx_lease"},
	})
	if err == nil {
		t.Fatal("expected provision failure")
	}
	got := strings.Join(methods, ",")
	for _, want := range []string{"VM.provision", "VDI.get_all_records", "VM.get_VBDs", "VDI.get_record", "VDI.destroy", "VM.destroy"} {
		if !strings.Contains(got, want) {
			t.Fatalf("methods=%s missing %s", got, want)
		}
	}
	if strings.Index(got, "VDI.destroy") > strings.Index(got, "VM.destroy") {
		t.Fatalf("methods=%s destroyed VM before copied VDI", got)
	}
}

func TestAttachConfigDriveCreatesImportsAndAttachesVDI(t *testing.T) {
	var methods []string
	var imported bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/import_raw_vdi/" {
			imported = true
			if r.URL.Query().Get("format") != "raw" || r.URL.Query().Get("vdi") != "OpaqueRef:vdi" || r.URL.Query().Get("task_id") != "OpaqueRef:task" {
				t.Fatalf("import query=%s", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		method, body := readXMLRPCBodyAndMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VDI.create":
			if !strings.Contains(body, "<name>read_only</name><value><boolean>0</boolean>") {
				t.Fatalf("VDI.create must stay writable for raw import, body=%s", body)
			}
			writeXMLRPCString(t, w, "OpaqueRef:vdi")
		case "task.create":
			writeXMLRPCString(t, w, "OpaqueRef:task")
		case "task.get_status":
			writeXMLRPCString(t, w, "success")
		case "task.destroy":
			writeXMLRPCString(t, w, "true")
		case "VBD.create":
			if !strings.Contains(body, "<name>mode</name><value><string>RO</string>") {
				t.Fatalf("VBD.create must attach config drive read-only, body=%s", body)
			}
			writeXMLRPCString(t, w, "OpaqueRef:vbd")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	drive, err := client.AttachConfigDrive(context.Background(), xcpNgConfigDriveRequest{
		VMRef:   "OpaqueRef:vm",
		SRRef:   "OpaqueRef:sr",
		LeaseID: "cbx_lease",
		Slug:    "blue",
		Payload: xcpNgCloudInitPayload{UserData: "#cloud-config\n", MetaData: "instance-id: cbx_lease\n"},
		Labels:  map[string]string{"crabbox": "true", "created_by": "crabbox", "provider": "xcp-ng", "lease": "cbx_lease"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if drive.VDIRef != "OpaqueRef:vdi" || drive.VBDRef != "OpaqueRef:vbd" || !imported {
		t.Fatalf("drive=%#v imported=%v", drive, imported)
	}
	if got := strings.Join(methods, ","); got != "VDI.create,task.create,task.get_status,task.destroy,VBD.create" {
		t.Fatalf("methods=%s", got)
	}
}

func TestGetServerAndSetLabelsResolveUUIDToFreshRef(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VM.get_by_uuid":
			writeXMLRPCString(t, w, "OpaqueRef:vm-fresh")
		case "VM.get_record":
			writeXMLRPCVMRecord(t, w, "cbx_lease")
		case "VM.get_guest_metrics":
			writeXMLRPCFault(t, w, "HANDLE_INVALID")
		case "VM.remove_from_other_config":
			if !strings.Contains(strings.Join(methods, ","), "VM.get_by_uuid") {
				t.Fatalf("set labels did not resolve UUID first; methods=%v", methods)
			}
			writeXMLRPCFault(t, w, "MAP_KEY_NOT_FOUND")
		case "VM.add_to_other_config":
			writeXMLRPCString(t, w, "true")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	serverView, err := client.GetServer(context.Background(), xcpNgTestVMUUID)
	if err != nil {
		t.Fatal(err)
	}
	if serverView.CloudID != xcpNgTestVMUUID {
		t.Fatalf("server=%#v", serverView)
	}
	if err := client.SetLabels(context.Background(), xcpNgTestVMUUID, map[string]string{"lease": "cbx_lease"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(methods, ","); !strings.Contains(got, "VM.get_by_uuid,VM.get_record") || !strings.Contains(got, "VM.get_by_uuid,VM.remove_from_other_config") {
		t.Fatalf("methods=%s", got)
	}
}

func TestGuestIPv4ForIDResolvesUUIDToFreshRef(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VM.get_by_uuid":
			writeXMLRPCString(t, w, "OpaqueRef:vm-fresh")
		case "VM.get_guest_metrics":
			writeXMLRPCString(t, w, "OpaqueRef:metrics")
		case "VM_guest_metrics.get_networks":
			writeXMLRPCNetworks(t, w, "192.0.2.55")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	ip, err := client.GuestIPv4ForID(context.Background(), xcpNgTestVMUUID)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.0.2.55" {
		t.Fatalf("ip=%q", ip)
	}
	if got := strings.Join(methods, ","); got != "VM.get_by_uuid,VM.get_guest_metrics,VM_guest_metrics.get_networks" {
		t.Fatalf("methods=%s", got)
	}
}

func TestDeleteServerRemovesLeaseConfigDriveVDI(t *testing.T) {
	var methods []string
	powerStateChecks := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VM.get_by_uuid":
			writeXMLRPCString(t, w, "OpaqueRef:vm")
		case "VM.get_record":
			writeXMLRPCVMRecord(t, w, "cbx_lease")
		case "VM.get_guest_metrics":
			writeXMLRPCFault(t, w, "HANDLE_INVALID")
		case "VDI.get_all_records":
			writeXMLRPCVDIRecords(t, w)
		case "VM.get_VBDs":
			writeXMLRPCStringArray(t, w, []string{"OpaqueRef:vbd", "OpaqueRef:root-vbd", "OpaqueRef:external-vbd"})
		case "VBD.get_record":
			switch countMethod(methods, "VBD.get_record") {
			case 1:
				writeXMLRPCVBDRecord(t, w, "OpaqueRef:vdi")
			case 2:
				writeXMLRPCVBDRecord(t, w, "OpaqueRef:root-vdi")
			default:
				writeXMLRPCVBDRecord(t, w, "OpaqueRef:external-vdi")
			}
		case "VDI.get_record":
			if countMethod(methods, "VDI.get_record") == 1 {
				writeXMLRPCOwnedVDIRecord(t, w, "crabbox-root", "cbx_lease")
			} else {
				writeXMLRPCVDIRecord(t, w, "external-disk")
			}
		case "VM.get_power_state":
			powerStateChecks++
			if powerStateChecks == 1 {
				writeXMLRPCString(t, w, "Running")
			} else {
				writeXMLRPCString(t, w, "Halted")
			}
		case "VBD.unplug", "VBD.destroy", "VDI.destroy", "VM.clean_shutdown", "VM.destroy":
			if method == "VM.destroy" && powerStateChecks < 2 {
				t.Fatalf("VM.destroy called before halted state was observed; methods=%v", methods)
			}
			writeXMLRPCString(t, w, "true")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	oldPoll := xcpNgShutdownPollInterval
	xcpNgShutdownPollInterval = time.Millisecond
	t.Cleanup(func() { xcpNgShutdownPollInterval = oldPoll })
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	if err := client.DeleteServer(context.Background(), xcpNgTestVMUUID); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(methods, ",")
	for _, want := range []string{"VM.get_by_uuid", "VM.get_record", "VDI.get_all_records", "VBD.unplug", "VBD.destroy", "VDI.destroy", "VM.get_power_state", "VM.clean_shutdown", "VM.destroy"} {
		if !strings.Contains(got, want) {
			t.Fatalf("methods=%s missing %s", got, want)
		}
	}
	if strings.Index(got, "VM.clean_shutdown") > strings.Index(got, "VM.destroy") {
		t.Fatalf("methods=%s destroyed before clean shutdown", got)
	}
	if strings.Index(got, "VM.clean_shutdown") > strings.Index(got, "VBD.unplug") {
		t.Fatalf("methods=%s unplugged config drive before shutdown", got)
	}
	if countMethod(methods, "VDI.destroy") != 2 {
		t.Fatalf("methods=%s should destroy config drive and owned cloned root VDI only", got)
	}
}

func TestDeleteServerRefusesDestroyWhenMetadataLookupFails(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VM.get_record":
			writeXMLRPCFault(t, w, "INTERNAL_ERROR")
		case "VM.destroy":
			t.Fatal("VM.destroy must not run when metadata lookup fails")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	if err := client.DeleteServer(context.Background(), "OpaqueRef:vm"); err == nil || !strings.Contains(err.Error(), "INTERNAL_ERROR") {
		t.Fatalf("err=%v", err)
	}
	if got := strings.Join(methods, ","); got != "VM.get_record" {
		t.Fatalf("methods=%s", got)
	}
}

func TestDeleteConfigDriveTreatsAlreadyDetachedVBDAsCleanedUp(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VBD.unplug":
			writeXMLRPCFault(t, w, "DEVICE_ALREADY_DETACHED")
		case "VBD.destroy", "VDI.destroy":
			writeXMLRPCString(t, w, "true")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	if err := client.DeleteConfigDrive(context.Background(), xcpNgConfigDrive{VBDRef: "OpaqueRef:vbd", VDIRef: "OpaqueRef:vdi"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(methods, ","); got != "VBD.unplug,VBD.destroy,VDI.destroy" {
		t.Fatalf("methods=%s", got)
	}
}

func TestXMLRPCHTTPErrorRedactsLoginPassword(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		http.Error(w, "debug echo "+string(body), http.StatusBadGateway)
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.XCPNg.APIURL = server.URL
	cfg.XCPNg.Password = "secret-password"
	_, err := newXAPIClient(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected login HTTP error")
	}
	text := err.Error()
	if strings.Contains(text, cfg.XCPNg.Password) {
		t.Fatalf("error leaked password: %s", text)
	}
	if !strings.Contains(text, "<redacted>") {
		t.Fatalf("error did not preserve redacted context: %s", text)
	}
}

func TestXMLRPCHTTPErrorRedactsSessionToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		http.Error(w, "debug echo "+string(body), http.StatusBadGateway)
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:secret-session", http: server.Client()}
	_, err := client.call(context.Background(), "VM.get_record", client.session, "OpaqueRef:vm")
	if err == nil {
		t.Fatal("expected XML-RPC HTTP error")
	}
	text := err.Error()
	if strings.Contains(text, "OpaqueRef:secret-session") || strings.Contains(text, "secret-session") {
		t.Fatalf("error leaked session token: %s", text)
	}
	if !strings.Contains(text, "<redacted>") {
		t.Fatalf("error did not preserve redacted context: %s", text)
	}
}

func TestShutdownVMFallsBackToHardShutdown(t *testing.T) {
	var methods []string
	powerStateChecks := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VM.get_power_state":
			powerStateChecks++
			if powerStateChecks == 1 {
				writeXMLRPCString(t, w, "Running")
			} else {
				writeXMLRPCString(t, w, "Halted")
			}
		case "VM.clean_shutdown":
			writeXMLRPCFault(t, w, "VM_LACKS_FEATURE_SHUTDOWN")
		case "VM.hard_shutdown":
			writeXMLRPCString(t, w, "true")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	oldPoll := xcpNgShutdownPollInterval
	xcpNgShutdownPollInterval = time.Millisecond
	t.Cleanup(func() { xcpNgShutdownPollInterval = oldPoll })
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	if err := client.shutdownVM(context.Background(), "OpaqueRef:vm"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(methods, ",")
	for _, want := range []string{"VM.get_power_state", "VM.clean_shutdown", "VM.hard_shutdown"} {
		if !strings.Contains(got, want) {
			t.Fatalf("methods=%s missing %s", got, want)
		}
	}
	if strings.Index(got, "VM.hard_shutdown") > strings.LastIndex(got, "VM.get_power_state") {
		t.Fatalf("methods=%s did not verify halted state after hard shutdown", got)
	}
}

func TestImportRawVDIRedactsSessionTokenFromTransportError(t *testing.T) {
	client := &xapiClient{
		endpoint: "http://xcp-ng.example.test/",
		session:  "OpaqueRef:secret-session",
		http: &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPost {
				return xmlRPCHTTPResponse("OpaqueRef:task"), nil
			}
			return nil, errors.New("dial failed for " + req.URL.String())
		})},
	}
	err := client.importRawVDI(context.Background(), "OpaqueRef:vdi", []byte("image"))
	if err == nil {
		t.Fatal("expected upload error")
	}
	text := err.Error()
	if strings.Contains(text, "OpaqueRef:secret-session") || strings.Contains(text, "secret-session") {
		t.Fatalf("error leaked session token: %s", text)
	}
	if !strings.Contains(text, "session_id") || !strings.Contains(text, "redacted") {
		t.Fatalf("error did not preserve redacted upload context: %s", text)
	}
}

func TestImportRawVDIRedactsSessionTokenFromHTTPErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeXMLRPCString(t, w, "OpaqueRef:task")
			return
		}
		http.Error(w, "failed request "+r.URL.String(), http.StatusBadGateway)
	}))
	defer server.Close()
	client := &xapiClient{
		endpoint: server.URL,
		session:  "OpaqueRef:secret-session",
		http:     server.Client(),
	}
	err := client.importRawVDI(context.Background(), "OpaqueRef:vdi", []byte("image"))
	if err == nil {
		t.Fatal("expected upload error")
	}
	text := err.Error()
	if strings.Contains(text, "OpaqueRef:secret-session") || strings.Contains(text, "secret-session") {
		t.Fatalf("error leaked session token: %s", text)
	}
	if !strings.Contains(text, "session_id") || !strings.Contains(text, "redacted") {
		t.Fatalf("error did not preserve redacted upload context: %s", text)
	}
}

func readXMLRPCMethod(t *testing.T, r *http.Request) string {
	t.Helper()
	method, _ := readXMLRPCBodyAndMethod(t, r)
	return method
}

func readXMLRPCBodyAndMethod(t *testing.T, r *http.Request) (string, string) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Method string `xml:"methodName"`
	}
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		t.Fatal(err)
	}
	return req.Method, string(body)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func xmlRPCHTTPResponse(value string) *http.Response {
	body := `<?xml version="1.0"?><methodResponse><params><param><value><string>` + value + `</string></value></param></params></methodResponse>`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func countMethod(methods []string, want string) int {
	count := 0
	for _, method := range methods {
		if method == want {
			count++
		}
	}
	return count
}

func writeXMLRPCString(t *testing.T, w http.ResponseWriter, value string) {
	t.Helper()
	_, err := w.Write([]byte(`<?xml version="1.0"?><methodResponse><params><param><value><string>` + value + `</string></value></param></params></methodResponse>`))
	if err != nil {
		t.Fatal(err)
	}
}

func writeXMLRPCStringArray(t *testing.T, w http.ResponseWriter, values []string) {
	t.Helper()
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><methodResponse><params><param><value><array><data>`)
	for _, value := range values {
		b.WriteString(`<value><string>`)
		b.WriteString(value)
		b.WriteString(`</string></value>`)
	}
	b.WriteString(`</data></array></value></param></params></methodResponse>`)
	if _, err := w.Write([]byte(b.String())); err != nil {
		t.Fatal(err)
	}
}

func writeXMLRPCNetworks(t *testing.T, w http.ResponseWriter, ip string) {
	t.Helper()
	response := `<?xml version="1.0"?><methodResponse><params><param><value><struct>
<member><name>0/ip</name><value><string>` + ip + `</string></value></member>
</struct></value></param></params></methodResponse>`
	if _, err := w.Write([]byte(response)); err != nil {
		t.Fatal(err)
	}
}

func writeXMLRPCFault(t *testing.T, w http.ResponseWriter, message string) {
	t.Helper()
	response := `<?xml version="1.0"?><methodResponse><fault><value><struct>` +
		`<member><name>faultCode</name><value><int>1</int></value></member>` +
		`<member><name>faultString</name><value><string>` + message + `</string></value></member>` +
		`</struct></value></fault></methodResponse>`
	if _, err := w.Write([]byte(response)); err != nil {
		t.Fatal(err)
	}
}

func writeXMLRPCVMRecord(t *testing.T, w http.ResponseWriter, leaseID string) {
	t.Helper()
	labels := "crabbox=true\ncreated_by=crabbox\nprovider=xcp-ng\nlease=" + leaseID + "\nslug=xmlrpc\nstate=ready\n"
	response := `<?xml version="1.0"?><methodResponse><params><param><value><struct>
<member><name>uuid</name><value>` + xcpNgTestVMUUID + `</value></member>
<member><name>name_label</name><value>crabbox-xmlrpc</value></member>
<member><name>power_state</name><value>Running</value></member>
<member><name>other_config</name><value><struct><member><name>crabbox:labels</name><value><string>` + labels + `</string></value></member></struct></value></member>
</struct></value></param></params></methodResponse>`
	if _, err := w.Write([]byte(response)); err != nil {
		t.Fatal(err)
	}
}

func writeXMLRPCUnmanagedVMRecord(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	response := `<?xml version="1.0"?><methodResponse><params><param><value><struct>
<member><name>uuid</name><value>` + xcpNgTestVMUUID + `</value></member>
<member><name>name_label</name><value>crabbox-unlabeled</value></member>
<member><name>power_state</name><value>Halted</value></member>
<member><name>other_config</name><value><struct></struct></value></member>
</struct></value></param></params></methodResponse>`
	if _, err := w.Write([]byte(response)); err != nil {
		t.Fatal(err)
	}
}

func writeXMLRPCVBDRecord(t *testing.T, w http.ResponseWriter, vdiRef string) {
	t.Helper()
	response := `<?xml version="1.0"?><methodResponse><params><param><value><struct>
<member><name>empty</name><value><boolean>0</boolean></value></member>
<member><name>VDI</name><value><string>` + vdiRef + `</string></value></member>
</struct></value></param></params></methodResponse>`
	if _, err := w.Write([]byte(response)); err != nil {
		t.Fatal(err)
	}
}

func writeXMLRPCVDIRecord(t *testing.T, w http.ResponseWriter, name string) {
	t.Helper()
	response := `<?xml version="1.0"?><methodResponse><params><param><value><struct>
<member><name>name_label</name><value><string>` + name + `</string></value></member>
<member><name>read_only</name><value><boolean>0</boolean></value></member>
<member><name>sharable</name><value><boolean>0</boolean></value></member>
<member><name>type</name><value><string>user</string></value></member>
<member><name>other_config</name><value><struct></struct></value></member>
</struct></value></param></params></methodResponse>`
	if _, err := w.Write([]byte(response)); err != nil {
		t.Fatal(err)
	}
}

func writeXMLRPCOwnedVDIRecord(t *testing.T, w http.ResponseWriter, name, leaseID string) {
	t.Helper()
	response := `<?xml version="1.0"?><methodResponse><params><param><value><struct>
<member><name>name_label</name><value><string>` + name + `</string></value></member>
<member><name>read_only</name><value><boolean>0</boolean></value></member>
<member><name>sharable</name><value><boolean>0</boolean></value></member>
<member><name>type</name><value><string>user</string></value></member>
<member><name>other_config</name><value><struct>
<member><name>crabbox</name><value>true</value></member>
<member><name>created_by</name><value>crabbox</value></member>
<member><name>provider</name><value>xcp-ng</value></member>
<member><name>lease</name><value>` + leaseID + `</value></member>
<member><name>resource</name><value>vm-disk</value></member>
</struct></value></member>
</struct></value></param></params></methodResponse>`
	if _, err := w.Write([]byte(response)); err != nil {
		t.Fatal(err)
	}
}

func writeXMLRPCVDIRecords(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	response := `<?xml version="1.0"?><methodResponse><params><param><value><struct>
<member><name>OpaqueRef:vdi</name><value><struct>
<member><name>name_label</name><value>crabbox-config</value></member>
<member><name>VBDs</name><value><array><data><value>OpaqueRef:vbd</value></data></array></value></member>
<member><name>other_config</name><value><struct>
<member><name>crabbox</name><value>true</value></member>
<member><name>created_by</name><value>crabbox</value></member>
<member><name>provider</name><value>xcp-ng</value></member>
<member><name>lease</name><value>cbx_lease</value></member>
<member><name>resource</name><value>config-drive</value></member>
</struct></value></member>
</struct></value></member>
<member><name>OpaqueRef:user-vdi</name><value><struct>
<member><name>name_label</name><value>user-disk</value></member>
<member><name>VBDs</name><value><array><data></data></array></value></member>
<member><name>other_config</name><value><struct><member><name>lease</name><value>cbx_lease</value></member></struct></value></member>
</struct></value></member>
</struct></value></param></params></methodResponse>`
	if _, err := w.Write([]byte(response)); err != nil {
		t.Fatal(err)
	}
}

func writeXMLRPCEmptyRecordMap(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	response := `<?xml version="1.0"?><methodResponse><params><param><value><struct></struct></value></param></params></methodResponse>`
	if _, err := w.Write([]byte(response)); err != nil {
		t.Fatal(err)
	}
}

func writeXMLRPCVMRecords(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	labels := "crabbox=true\ncreated_by=crabbox\nprovider=xcp-ng\nlease=cbx_xmlrpc\nslug=xmlrpc\nstate=ready\n"
	response := `<?xml version="1.0"?><methodResponse><params><param><value><struct>
<member><name>OpaqueRef:vm-1</name><value><struct>
<member><name>uuid</name><value><string>` + xcpNgTestVMUUID + `</string></value></member>
<member><name>name_label</name><value><string>crabbox-xmlrpc</string></value></member>
<member><name>power_state</name><value><string>Running</string></value></member>
<member><name>is_a_template</name><value><boolean>0</boolean></value></member>
<member><name>other_config</name><value><struct><member><name>crabbox:labels</name><value><string>` + labels + `</string></value></member></struct></value></member>
</struct></value></member>
<member><name>OpaqueRef:user-vm</name><value><struct>
<member><name>uuid</name><value><string>user-uuid</string></value></member>
<member><name>name_label</name><value><string>crabbox-prefix-only</string></value></member>
<member><name>power_state</name><value><string>Running</string></value></member>
<member><name>is_a_template</name><value><boolean>0</boolean></value></member>
<member><name>other_config</name><value><struct></struct></value></member>
</struct></value></member>
</struct></value></param></params></methodResponse>`
	_, err := w.Write([]byte(response))
	if err != nil {
		t.Fatal(err)
	}
}
