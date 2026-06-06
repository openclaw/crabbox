package xcpng

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
		{Name: "2/ip", Value: xmlRPCValue{String: "192.0.2.55"}},
	}}
	networks := xmlValueToStringMap(value)
	if ip := usableIPv4(networks["0/ip"]); ip != "" {
		t.Fatalf("loopback ip=%s", ip)
	}
	if ip := usableIPv4(networks["1/ip"]); ip != "" {
		t.Fatalf("ipv6 ip=%s", ip)
	}
	if ip := usableIPv4(networks["2/ip"]); ip != "192.0.2.55" {
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

func TestXAPIStatusEnvelopeUnwrapsValueAndFailure(t *testing.T) {
	success := xmlRPCValue{Struct: []xmlRPCMember{
		{Name: "Status", Value: xmlRPCValue{CharData: "Success"}},
		{Name: "Value", Value: xmlRPCValue{CharData: "OpaqueRef:vm"}},
	}}
	value, err := unwrapXAPIResponse(success)
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
	if _, err := unwrapXAPIResponse(failure); err == nil || !strings.Contains(err.Error(), "HANDLE_INVALID: VM") {
		t.Fatalf("err=%v", err)
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
		case "VM.set_affinity":
			writeXMLRPCString(t, w, "true")
		case "VM.get_VIFs":
			writeXMLRPCStringArray(t, w, []string{"OpaqueRef:vif"})
		case "VIF.set_network":
			writeXMLRPCString(t, w, "true")
		case "VM.provision":
			writeXMLRPCString(t, w, "true")
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
	_, err := client.CloneVM(context.Background(), xcpNgCloneRequest{
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
	got := strings.Join(methods, ",")
	for _, want := range []string{"VM.copy", "VM.set_affinity", "VM.get_VIFs", "VIF.set_network", "VM.remove_from_other_config", "VM.add_to_other_config", "VM.provision"} {
		if !strings.Contains(got, want) {
			t.Fatalf("methods=%s missing %s", got, want)
		}
	}
	if strings.Contains(got, "VM.clone") {
		t.Fatalf("methods=%s unexpectedly used VM.clone", got)
	}
}

func TestAttachConfigDriveCreatesImportsAndAttachesVDI(t *testing.T) {
	var methods []string
	var imported bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/import_raw_vdi/" {
			imported = true
			if r.URL.Query().Get("format") != "raw" || r.URL.Query().Get("vdi") != "OpaqueRef:vdi" {
				t.Fatalf("import query=%s", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VDI.create":
			writeXMLRPCString(t, w, "OpaqueRef:vdi")
		case "VBD.create":
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
	if got := strings.Join(methods, ","); got != "VDI.create,VBD.create" {
		t.Fatalf("methods=%s", got)
	}
}

func TestDeleteServerRemovesLeaseConfigDriveVDI(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VM.get_record":
			writeXMLRPCVMRecord(t, w, "cbx_lease")
		case "VM.get_guest_metrics":
			writeXMLRPCFault(t, w, "HANDLE_INVALID")
		case "VDI.get_all_records":
			writeXMLRPCVDIRecords(t, w)
		case "VBD.unplug", "VBD.destroy", "VDI.destroy", "VM.clean_shutdown", "VM.destroy":
			writeXMLRPCString(t, w, "true")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	if err := client.DeleteServer(context.Background(), "OpaqueRef:vm"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(methods, ",")
	for _, want := range []string{"VM.get_record", "VDI.get_all_records", "VBD.unplug", "VBD.destroy", "VDI.destroy", "VM.destroy"} {
		if !strings.Contains(got, want) {
			t.Fatalf("methods=%s missing %s", got, want)
		}
	}
}

func readXMLRPCMethod(t *testing.T, r *http.Request) string {
	t.Helper()
	var req struct {
		Method string `xml:"methodName"`
	}
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatal(err)
	}
	return req.Method
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
<member><name>uuid</name><value>vm-uuid</value></member>
<member><name>name_label</name><value>crabbox-xmlrpc</value></member>
<member><name>power_state</name><value>Running</value></member>
<member><name>other_config</name><value><struct><member><name>crabbox:labels</name><value><string>` + labels + `</string></value></member></struct></value></member>
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

func writeXMLRPCVMRecords(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	labels := "crabbox=true\ncreated_by=crabbox\nprovider=xcp-ng\nlease=cbx_xmlrpc\nslug=xmlrpc\nstate=ready\n"
	response := `<?xml version="1.0"?><methodResponse><params><param><value><struct>
<member><name>OpaqueRef:vm-1</name><value><struct>
<member><name>uuid</name><value><string>vm-uuid</string></value></member>
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
