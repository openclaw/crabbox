package xcpng

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
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

func TestXMLRPCRequestEncodesXenAPIIntegersAsDecimalStrings(t *testing.T) {
	body, err := encodeXMLRPCRequest("VM.set_memory_static_max", "session", "vm", int64(24*1024*1024*1024), 1500)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{"<string>25769803776</string>", "<string>1500</string>"} {
		if !strings.Contains(text, want) {
			t.Fatalf("body missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "<int>") || strings.Contains(text, "<i4>") {
		t.Fatalf("XenAPI integer encoded as XML-RPC integer: %s", text)
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

func TestXAPIEndpointStripsUserinfoBeforeRequests(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "full URL",
			raw:  "https://api-user:api-password@xcp-ng.example.test/jsonrpc",
			want: "https://xcp-ng.example.test/jsonrpc",
		},
		{
			name: "scheme-less URL",
			raw:  "api-user:api-password@xcp-ng.example.test",
			want: "https://xcp-ng.example.test/",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, err := xapiEndpoint(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if endpoint != tc.want {
				t.Fatalf("endpoint=%q want %q", endpoint, tc.want)
			}
			parsed, err := url.Parse(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.User != nil {
				t.Fatalf("endpoint retained userinfo: %s", endpoint)
			}
			for _, secret := range []string{"api-user", "api-password", "api-user:api-password"} {
				if strings.Contains(endpoint, secret) {
					t.Fatalf("endpoint leaked %q: %s", secret, endpoint)
				}
			}
		})
	}
}

func TestXAPIEndpointRedactsUserinfoFromMalformedURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"full URL with bad escape in host", "https://pool-user:pool-pass@%zz"},
		{"full URL with bad escape in path", "https://pool-user:pool-pass@xcp-ng.example.test/%zz"},
		{"full URL with bad port", "https://pool-user:pool-pass@xcp-ng.example.test:abc"},
		{"full URL with extra at in password", "https://pool-user:pool@pass@%zz"},
		{"full URL with slash in password", "https://pool-user:pool/pass@host/%zz"},
		{"full URL with query delimiter in password", "https://pool-user:pool?pass@host/%zz"},
		{"full URL with fragment delimiter in password", "https://pool-user:pool#pass@host/%zz"},
		{"scheme-less URL with bad escape in host", "pool-user:pool-pass@%zz"},
		{"scheme-less URL with extra at in password", "pool-user:pool@pass@%zz"},
		{"scheme-less URL with bad escape in path", "pool-user:pool-pass@%zz/path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := xapiEndpoint(tc.raw)
			if err == nil {
				t.Fatalf("expected error for malformed URL %q", tc.raw)
			}
			text := err.Error()
			for _, secret := range []string{"pool-user", "pool-pass", "pool/pass", "pool?pass", "pool#pass", "pool@pass", "pool-user:pool-pass"} {
				if strings.Contains(text, secret) {
					t.Fatalf("error leaked %q for %q: %s", secret, tc.raw, text)
				}
			}
		})
	}
}

func TestXAPIEndpointForMasterRedactsUserinfoFromMalformedCurrentURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current string
		master  string
	}{
		{"full current with bad escape in host", "https://pool-user:pool-pass@%zz", "xcp-ng.example.test"},
		{"full current with bad escape in path", "https://pool-user:pool-pass@xcp-ng.example.test/%zz", "xcp-ng.example.test"},
		{"scheme-less current with bad escape in host", "pool-user:pool-pass@%zz", "xcp-ng.example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := xapiEndpointForMaster(tc.current, tc.master)
			if err == nil {
				t.Fatalf("expected error for malformed current %q", tc.current)
			}
			text := err.Error()
			for _, secret := range []string{"pool-user", "pool-pass", "pool-user:pool-pass"} {
				if strings.Contains(text, secret) {
					t.Fatalf("error leaked %q for %q: %s", secret, tc.current, text)
				}
			}
		})
	}
}

func TestCloseRetainsSessionWhenLogoutFailsSoItCanRetry(t *testing.T) {
	var methods []string
	client := &xapiClient{
		endpoint: "http://xcp-ng.example.test/",
		session:  "OpaqueRef:session",
		http: &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			method := readXMLRPCMethod(t, req)
			methods = append(methods, method)
			if method != "session.logout" {
				t.Fatalf("unexpected method %s", method)
			}
			if len(methods) == 1 {
				return &http.Response{
					StatusCode: http.StatusBadGateway,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("temporary logout failure")),
				}, nil
			}
			return xmlRPCHTTPResponse("true"), nil
		})},
	}

	if err := client.Close(context.Background()); err == nil {
		t.Fatal("expected failed logout")
	}
	if client.session != "OpaqueRef:session" {
		t.Fatalf("failed logout cleared session %q", client.session)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.session != "" {
		t.Fatalf("successful logout kept session %q", client.session)
	}
	if got := strings.Join(methods, ","); got != "session.logout,session.logout" {
		t.Fatalf("methods=%s", got)
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

func TestNewXAPIClientFollowsHostIsSlaveMasterRedirect(t *testing.T) {
	var slaveMethods []string
	var masterMethods []string
	var masterHost string
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		masterMethods = append(masterMethods, method)
		switch method {
		case "session.login_with_password":
			writeXMLRPCString(t, w, "OpaqueRef:master-session")
		case "VM.get_all_records":
			writeXMLRPCVMRecords(t, w)
		case "session.logout":
			writeXMLRPCString(t, w, "true")
		default:
			t.Fatalf("unexpected master method %s", method)
		}
	}))
	defer master.Close()
	masterHost = strings.TrimPrefix(master.URL, "http://")
	slave := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		slaveMethods = append(slaveMethods, method)
		if method != "session.login_with_password" {
			t.Fatalf("unexpected slave method %s", method)
		}
		writeXAPIStatusFailure(t, w, []string{"HOST_IS_SLAVE", masterHost})
	}))
	defer slave.Close()

	cfg := testConfig()
	cfg.XCPNg.APIURL = slave.URL
	client, err := newXAPIClient(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := client.endpoint; got != master.URL+"/" {
		t.Fatalf("endpoint=%q want %q", got, master.URL+"/")
	}
	if _, err := client.DoctorInventory(context.Background(), xcpNgProviderConfig(cfg)); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(slaveMethods, ","); got != "session.login_with_password" {
		t.Fatalf("slave methods=%s", got)
	}
	if got := strings.Join(masterMethods, ","); got != "session.login_with_password,VM.get_all_records,session.logout" {
		t.Fatalf("master methods=%s", got)
	}
}

func TestXAPICallReconnectsOnHostIsSlaveRedirect(t *testing.T) {
	var slaveMethods []string
	var masterMethods []string
	var masterHost string
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		masterMethods = append(masterMethods, method)
		switch method {
		case "session.login_with_password":
			writeXMLRPCString(t, w, "OpaqueRef:master-session")
		case "VM.get_all_records":
			writeXMLRPCVMRecords(t, w)
		case "session.logout":
			writeXMLRPCString(t, w, "true")
		default:
			t.Fatalf("unexpected master method %s", method)
		}
	}))
	defer master.Close()
	masterHost = strings.TrimPrefix(master.URL, "http://")
	slave := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		slaveMethods = append(slaveMethods, method)
		switch method {
		case "session.login_with_password":
			writeXMLRPCString(t, w, "OpaqueRef:slave-session")
		case "VM.get_all_records":
			writeXAPIStatusFailure(t, w, []string{"HOST_IS_SLAVE", masterHost})
		case "session.logout":
			writeXMLRPCString(t, w, "true")
		default:
			t.Fatalf("unexpected slave method %s", method)
		}
	}))
	defer slave.Close()

	cfg := testConfig()
	cfg.XCPNg.APIURL = slave.URL
	client, err := newXAPIClient(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.DoctorInventory(context.Background(), xcpNgProviderConfig(cfg)); err != nil {
		t.Fatal(err)
	}
	if got := client.endpoint; got != master.URL+"/" {
		t.Fatalf("endpoint=%q want %q", got, master.URL+"/")
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(slaveMethods, ","); got != "session.login_with_password,VM.get_all_records,session.logout" {
		t.Fatalf("slave methods=%s", got)
	}
	if got := strings.Join(masterMethods, ","); got != "session.login_with_password,VM.get_all_records,session.logout" {
		t.Fatalf("master methods=%s", got)
	}
}

func TestGuestIPv4SkipsLoopbackAndNonIPv4(t *testing.T) {
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
}

func TestGuestIPv4FromNetworksSelectsPrimaryOrConfiguredNetwork(t *testing.T) {
	networks := map[string]string{
		"0/ip":     "192.0.2.55",
		"0/ipv4/0": "192.0.2.55",
		"1/ip":     "10.0.0.55",
	}
	ip, err := guestIPv4FromNetworks(networks, "")
	if err != nil || ip != "192.0.2.55" {
		t.Fatalf("primary ip=%q err=%v", ip, err)
	}
	ip, err = guestIPv4FromNetworks(networks, "10.0.0.0/24")
	if err != nil || ip != "10.0.0.55" {
		t.Fatalf("cidr ip=%q err=%v", ip, err)
	}
	if _, err := guestIPv4FromNetworks(map[string]string{"1/ip": "192.0.2.55", "2/ip": "10.0.0.55"}, ""); err == nil || !strings.Contains(err.Error(), "multiple guest ipv4") {
		t.Fatalf("ambiguity err=%v", err)
	}
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

func TestSetVMOtherConfigUsesKeyScopedUpdates(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VM.get_other_config":
			writeXMLRPCStringMap(t, w, map[string]string{"crabbox:labels": "old"})
		case "VM.remove_from_other_config", "VM.add_to_other_config":
			writeXMLRPCString(t, w, "true")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	if err := client.setVMOtherConfig(context.Background(), "OpaqueRef:vm", map[string]string{"crabbox:labels": "crabbox=true\n"}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(methods, ",")
	if got != "VM.get_other_config,VM.remove_from_other_config,VM.add_to_other_config" {
		t.Fatalf("methods=%s", got)
	}
}

func TestSetVMOtherConfigRestoresPreviousValueOnAddFailure(t *testing.T) {
	var addCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch method := readXMLRPCMethod(t, r); method {
		case "VM.get_other_config":
			writeXMLRPCStringMap(t, w, map[string]string{"crabbox:labels": "old-labels"})
		case "VM.remove_from_other_config":
			writeXMLRPCString(t, w, "true")
		case "VM.add_to_other_config":
			addCalls++
			if addCalls == 1 {
				writeXMLRPCFault(t, w, "SR_BACKEND_FAILURE")
			} else {
				writeXMLRPCString(t, w, "true")
			}
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	if err := client.setVMOtherConfig(context.Background(), "OpaqueRef:vm", map[string]string{"crabbox:labels": "new-labels"}); err == nil {
		t.Fatal("expected add failure")
	}
	if addCalls != 2 {
		t.Fatalf("add calls=%d want failed update plus restore", addCalls)
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
		case "VM.get_other_config":
			writeXMLRPCStringMap(t, w, map[string]string{"existing": "preserved"})
		case "VM.remove_from_other_config", "VM.add_to_other_config":
			writeXMLRPCString(t, w, "true")
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
		case "VIF.move":
			writeXMLRPCString(t, w, "true")
		case "VM.provision":
			writeXMLRPCString(t, w, "true")
		case "VM.get_uuid":
			writeXMLRPCString(t, w, xcpNgTestVMUUID)
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
	for _, want := range []string{"VM.copy", "VM.get_other_config", "VM.remove_from_other_config", "VM.add_to_other_config", "VM.set_affinity", "VM.get_VIFs", "VIF.move", "VM.provision", "VM.get_uuid"} {
		if !strings.Contains(got, want) {
			t.Fatalf("methods=%s missing %s", got, want)
		}
	}
	if strings.Contains(got, "VM.clone") {
		t.Fatalf("methods=%s unexpectedly used VM.clone", got)
	}
}

func TestCloneVMLabelsBeforeAffinityAndRollsBackCopiedDisk(t *testing.T) {
	var methods []string
	uuidShapedRef := "OpaqueRef:22222222-2222-2222-2222-222222222222"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VM.copy":
			writeXMLRPCString(t, w, uuidShapedRef)
		case "VM.get_other_config":
			writeXMLRPCStringMap(t, w, map[string]string{})
		case "VM.remove_from_other_config", "VM.add_to_other_config":
			writeXMLRPCString(t, w, "true")
		case "VM.set_affinity":
			writeXMLRPCFault(t, w, "HOST_NOT_LIVE")
		case "VM.get_record":
			writeXMLRPCUnmanagedVMRecord(t, w)
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
	for _, want := range []string{"VM.copy", "VM.get_other_config", "VM.remove_from_other_config", "VM.add_to_other_config", "VM.set_affinity", "VM.get_record", "VM.get_VBDs", "VBD.get_record", "VDI.get_record", "VBD.unplug", "VBD.destroy", "VDI.destroy", "VM.destroy"} {
		if !strings.Contains(got, want) {
			t.Fatalf("methods=%s missing %s", got, want)
		}
	}
	if strings.Index(got, "VDI.destroy") > strings.Index(got, "VM.destroy") {
		t.Fatalf("methods=%s destroyed VM before copied VDI", got)
	}
	if strings.Index(got, "VM.add_to_other_config") > strings.Index(got, "VM.set_affinity") {
		t.Fatalf("methods=%s labeled VM after affinity", got)
	}
	if strings.Contains(got, "VM.get_by_uuid") {
		t.Fatalf("methods=%s unexpectedly resolved UUID-shaped OpaqueRef as UUID", got)
	}
}

func TestXCPNgRollbackContextIsBoundedAndDetachedFromCancellation(t *testing.T) {
	ctx, cancel := xcpNgRollbackContext(context.Background())
	defer cancel()
	deadline, bounded := ctx.Deadline()
	if !bounded {
		t.Fatal("rollback context has no deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > xcpNgPartialRollbackTimeout {
		t.Fatalf("rollback deadline remaining=%s", remaining)
	}

	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	canceledCtx, cancelCanceled := xcpNgRollbackContext(parent)
	defer cancelCanceled()
	if canceledCtx.Err() != nil {
		t.Fatalf("rollback context inherited cancellation: %v", canceledCtx.Err())
	}
	if _, bounded := canceledCtx.Deadline(); !bounded {
		t.Fatal("detached rollback context has no deadline")
	}
}

func TestXAPIRequestTimeoutsPreserveLongOperations(t *testing.T) {
	client := newXAPIHTTPClient(false)
	if client.Timeout != 0 {
		t.Fatalf("http client timeout=%s", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport=%T", client.Transport)
	}
	if transport.TLSHandshakeTimeout != 30*time.Second {
		t.Fatalf("TLS handshake timeout=%s", transport.TLSHandshakeTimeout)
	}
	if got := xcpNgRequestTimeoutForMethod("VM.get_record"); got != 5*time.Minute {
		t.Fatalf("routine request timeout=%s", got)
	}
	for _, method := range []string{"VM.clone", "VM.copy", "VM.provision"} {
		if got := xcpNgRequestTimeoutForMethod(method); got != 90*time.Minute {
			t.Fatalf("%s timeout=%s", method, got)
		}
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
		case "VM.get_other_config":
			writeXMLRPCStringMap(t, w, map[string]string{})
		case "VM.remove_from_other_config", "VM.add_to_other_config":
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

func TestCloneVMReturnsRecoveryHandleWhenRollbackFails(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VM.copy":
			writeXMLRPCString(t, w, "OpaqueRef:vm")
		case "VM.get_other_config":
			writeXMLRPCStringMap(t, w, map[string]string{})
		case "VM.remove_from_other_config", "VM.add_to_other_config":
			writeXMLRPCString(t, w, "true")
		case "VM.set_affinity":
			writeXMLRPCFault(t, w, "HOST_NOT_LIVE")
		case "VM.get_record":
			writeXMLRPCVMRecord(t, w, "cbx_lease")
		case "VM.get_guest_metrics":
			writeXMLRPCFault(t, w, "HANDLE_INVALID")
		case "VDI.get_all_records":
			writeXMLRPCEmptyRecordMap(t, w)
		case "VM.get_VBDs":
			writeXMLRPCStringArray(t, w, []string{})
		case "VM.get_power_state":
			writeXMLRPCString(t, w, "Halted")
		case "VM.destroy":
			writeXMLRPCFault(t, w, "SR_BACKEND_FAILURE")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	vm, err := client.CloneVM(context.Background(), xcpNgCloneRequest{
		TemplateRef: "OpaqueRef:tpl",
		SRRef:       "OpaqueRef:sr",
		HostRef:     "OpaqueRef:bad-host",
		LeaseID:     "cbx_lease",
		Slug:        "blue",
		Labels:      map[string]string{"crabbox": "true", "created_by": "crabbox", "provider": "xcp-ng", "lease": "cbx_lease"},
	})
	if err == nil || !strings.Contains(err.Error(), "rollback copied xcp-ng VM") {
		t.Fatalf("err=%v", err)
	}
	if vm.Ref != "OpaqueRef:vm" || vm.Labels["lease"] != "cbx_lease" {
		t.Fatalf("recovery vm=%#v", vm)
	}
	if strings.Index(strings.Join(methods, ","), "VM.add_to_other_config") > strings.Index(strings.Join(methods, ","), "VM.set_affinity") {
		t.Fatalf("methods=%v", methods)
	}
}

func TestVMRecordsIncludesManagedTemplateStageCopy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if method := readXMLRPCMethod(t, r); method != "VM.get_all_records" {
			t.Fatalf("unexpected method %s", method)
		}
		writeXMLRPCManagedTemplateRecords(t, w)
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	vms, err := client.vmRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 1 || vms[0].Ref != "OpaqueRef:managed-template-copy" || vms[0].Labels["lease"] != "cbx_recovery" {
		t.Fatalf("vms=%#v", vms)
	}
}

func TestCreateFreshVMPreservesTemplateDefaults(t *testing.T) {
	var removedBootKeys []string
	var addedBootValues []string
	var removedPlatformKeys []string
	var addedPlatformValues []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, body := readXMLRPCBodyAndMethod(t, r)
		switch method {
		case "VM.get_by_name_label":
			writeXMLRPCStringArray(t, w, []string{"OpaqueRef:tpl"})
		case "VM.get_is_a_template":
			writeXMLRPCString(t, w, "true")
		case "VM.clone":
			writeXMLRPCString(t, w, "OpaqueRef:vm")
		case "VM.set_is_a_template", "VM.set_memory_static_max", "VM.set_memory_dynamic_max", "VM.set_memory_dynamic_min", "VM.set_memory_static_min", "VM.set_VCPUs_max", "VM.set_VCPUs_at_startup":
			writeXMLRPCString(t, w, "true")
		case "VM.get_HVM_boot_params":
			writeXMLRPCStringMap(t, w, map[string]string{"firmware": "bios", "order": "cd"})
		case "VM.remove_from_HVM_boot_params":
			switch {
			case strings.Contains(body, "<string>order</string>"):
				removedBootKeys = append(removedBootKeys, "order")
			case strings.Contains(body, "<string>firmware</string>"):
				removedBootKeys = append(removedBootKeys, "firmware")
			default:
				t.Fatalf("unexpected remove boot body=%s", body)
			}
			writeXMLRPCString(t, w, "true")
		case "VM.add_to_HVM_boot_params":
			switch {
			case strings.Contains(body, "<string>order</string>") && strings.Contains(body, "<string>dc</string>"):
				addedBootValues = append(addedBootValues, "order=dc")
			case strings.Contains(body, "<string>firmware</string>") && strings.Contains(body, "<string>uefi</string>"):
				addedBootValues = append(addedBootValues, "firmware=uefi")
			default:
				t.Fatalf("unexpected add boot body=%s", body)
			}
			writeXMLRPCString(t, w, "true")
		case "VM.get_platform":
			writeXMLRPCStringMap(t, w, map[string]string{
				"acpi":       "1",
				"apic":       "true",
				"hpet":       "true",
				"nx":         "true",
				"pae":        "true",
				"secureboot": "false",
				"viridian":   "true",
			})
		case "VM.remove_from_platform":
			switch {
			case strings.Contains(body, "<string>secureboot</string>"):
				removedPlatformKeys = append(removedPlatformKeys, "secureboot")
			case strings.Contains(body, "<string>nx</string>"):
				removedPlatformKeys = append(removedPlatformKeys, "nx")
			case strings.Contains(body, "<string>acpi</string>"):
				removedPlatformKeys = append(removedPlatformKeys, "acpi")
			case strings.Contains(body, "<string>apic</string>"):
				removedPlatformKeys = append(removedPlatformKeys, "apic")
			case strings.Contains(body, "<string>pae</string>"):
				removedPlatformKeys = append(removedPlatformKeys, "pae")
			case strings.Contains(body, "<string>hpet</string>"):
				removedPlatformKeys = append(removedPlatformKeys, "hpet")
			case strings.Contains(body, "<string>viridian</string>"):
				removedPlatformKeys = append(removedPlatformKeys, "viridian")
			default:
				t.Fatalf("unexpected remove platform body=%s", body)
			}
			writeXMLRPCString(t, w, "true")
		case "VM.add_to_platform":
			switch {
			case strings.Contains(body, "<string>secureboot</string>") && strings.Contains(body, "<string>true</string>"):
				addedPlatformValues = append(addedPlatformValues, "secureboot=true")
			case strings.Contains(body, "<string>device-model</string>") && strings.Contains(body, "<string>qemu-upstream-uefi</string>"):
				addedPlatformValues = append(addedPlatformValues, "device-model=qemu-upstream-uefi")
			default:
				t.Fatalf("unexpected add platform body=%s", body)
			}
			writeXMLRPCString(t, w, "true")
		case "VM.get_other_config":
			writeXMLRPCStringMap(t, w, map[string]string{})
		case "VM.remove_from_other_config", "VM.add_to_other_config":
			writeXMLRPCString(t, w, "true")
		case "VM.get_uuid":
			writeXMLRPCString(t, w, xcpNgTestVMUUID)
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	vm, err := client.CreateFreshVM(context.Background(), xcpNgFreshVMRequest{
		Name:   "crabbox-test",
		Labels: map[string]string{"lease": "cbx_lease"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if vm.VM.UUID != xcpNgTestVMUUID {
		t.Fatalf("vm=%#v", vm)
	}
	if got := strings.Join(removedBootKeys, ","); got != "order" {
		t.Fatalf("removedBootKeys=%v", removedBootKeys)
	}
	if got := strings.Join(addedBootValues, ","); got != "order=dc" {
		t.Fatalf("addedBootValues=%v", addedBootValues)
	}
	if len(removedPlatformKeys) != 0 {
		t.Fatalf("removedPlatformKeys=%v", removedPlatformKeys)
	}
	if len(addedPlatformValues) != 0 {
		t.Fatalf("addedPlatformValues=%v", addedPlatformValues)
	}
}

func TestCreateFreshVMSecureBootOverridesTemplateWithoutDroppingPlatformDefaults(t *testing.T) {
	var removedBootKeys []string
	var addedBootValues []string
	var removedPlatformKeys []string
	var addedPlatformValues []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, body := readXMLRPCBodyAndMethod(t, r)
		switch method {
		case "VM.get_by_name_label":
			writeXMLRPCStringArray(t, w, []string{"OpaqueRef:tpl"})
		case "VM.get_is_a_template":
			writeXMLRPCString(t, w, "true")
		case "VM.clone":
			writeXMLRPCString(t, w, "OpaqueRef:vm")
		case "VM.set_is_a_template", "VM.set_memory_static_max", "VM.set_memory_dynamic_max", "VM.set_memory_dynamic_min", "VM.set_memory_static_min", "VM.set_VCPUs_max", "VM.set_VCPUs_at_startup", "VM.set_HVM_boot_policy":
			writeXMLRPCString(t, w, "true")
		case "VTPM.create":
			writeXMLRPCString(t, w, "OpaqueRef:vtpm")
		case "VM.get_HVM_boot_params":
			writeXMLRPCStringMap(t, w, map[string]string{"firmware": "bios", "order": "cd"})
		case "VM.remove_from_HVM_boot_params":
			switch {
			case strings.Contains(body, "<string>order</string>"):
				removedBootKeys = append(removedBootKeys, "order")
			case strings.Contains(body, "<string>firmware</string>"):
				removedBootKeys = append(removedBootKeys, "firmware")
			default:
				t.Fatalf("unexpected remove boot body=%s", body)
			}
			writeXMLRPCString(t, w, "true")
		case "VM.add_to_HVM_boot_params":
			switch {
			case strings.Contains(body, "<string>order</string>") && strings.Contains(body, "<string>dc</string>"):
				addedBootValues = append(addedBootValues, "order=dc")
			case strings.Contains(body, "<string>firmware</string>") && strings.Contains(body, "<string>uefi</string>"):
				addedBootValues = append(addedBootValues, "firmware=uefi")
			default:
				t.Fatalf("unexpected add boot body=%s", body)
			}
			writeXMLRPCString(t, w, "true")
		case "VM.get_platform":
			writeXMLRPCStringMap(t, w, map[string]string{
				"acpi":       "1",
				"apic":       "true",
				"hpet":       "true",
				"nx":         "true",
				"pae":        "true",
				"secureboot": "false",
				"viridian":   "true",
			})
		case "VM.remove_from_platform":
			switch {
			case strings.Contains(body, "<string>secureboot</string>"):
				removedPlatformKeys = append(removedPlatformKeys, "secureboot")
			case strings.Contains(body, "<string>nx</string>"):
				removedPlatformKeys = append(removedPlatformKeys, "nx")
			case strings.Contains(body, "<string>acpi</string>"):
				removedPlatformKeys = append(removedPlatformKeys, "acpi")
			case strings.Contains(body, "<string>apic</string>"):
				removedPlatformKeys = append(removedPlatformKeys, "apic")
			case strings.Contains(body, "<string>pae</string>"):
				removedPlatformKeys = append(removedPlatformKeys, "pae")
			case strings.Contains(body, "<string>hpet</string>"):
				removedPlatformKeys = append(removedPlatformKeys, "hpet")
			case strings.Contains(body, "<string>viridian</string>"):
				removedPlatformKeys = append(removedPlatformKeys, "viridian")
			default:
				t.Fatalf("unexpected remove platform body=%s", body)
			}
			writeXMLRPCString(t, w, "true")
		case "VM.add_to_platform":
			switch {
			case strings.Contains(body, "<string>secureboot</string>") && strings.Contains(body, "<string>true</string>"):
				addedPlatformValues = append(addedPlatformValues, "secureboot=true")
			case strings.Contains(body, "<string>device-model</string>") && strings.Contains(body, "<string>qemu-upstream-uefi</string>"):
				addedPlatformValues = append(addedPlatformValues, "device-model=qemu-upstream-uefi")
			default:
				t.Fatalf("unexpected add platform body=%s", body)
			}
			writeXMLRPCString(t, w, "true")
		case "VM.get_other_config":
			writeXMLRPCStringMap(t, w, map[string]string{})
		case "VM.remove_from_other_config", "VM.add_to_other_config":
			writeXMLRPCString(t, w, "true")
		case "VM.get_uuid":
			writeXMLRPCString(t, w, xcpNgTestVMUUID)
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	vm, err := client.CreateFreshVM(context.Background(), xcpNgFreshVMRequest{
		Name:       "crabbox-test",
		Labels:     map[string]string{"lease": "cbx_lease"},
		SecureBoot: true,
		VTPM:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if vm.VM.UUID != xcpNgTestVMUUID {
		t.Fatalf("vm=%#v", vm)
	}
	if vm.VTPMRef != "OpaqueRef:vtpm" {
		t.Fatalf("VTPMRef=%q", vm.VTPMRef)
	}
	sort.Strings(removedBootKeys)
	sort.Strings(addedBootValues)
	sort.Strings(addedPlatformValues)
	if got := strings.Join(removedBootKeys, ","); got != "firmware,order" {
		t.Fatalf("removedBootKeys=%v", removedBootKeys)
	}
	if got := strings.Join(addedBootValues, ","); got != "firmware=uefi,order=dc" {
		t.Fatalf("addedBootValues=%v", addedBootValues)
	}
	if got := strings.Join(removedPlatformKeys, ","); got != "secureboot" {
		t.Fatalf("removedPlatformKeys=%v", removedPlatformKeys)
	}
	if got := strings.Join(addedPlatformValues, ","); got != "device-model=qemu-upstream-uefi,secureboot=true" {
		t.Fatalf("addedPlatformValues=%v", addedPlatformValues)
	}
}

func TestAttachDiskCreatesRawVDI(t *testing.T) {
	var sawRawVDICreate bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, body := readXMLRPCBodyAndMethod(t, r)
		switch method {
		case "VDI.create":
			if strings.Contains(body, `<name>sm_config</name><value><struct><member><name>type</name><value><string>raw</string></value></member></struct></value>`) {
				sawRawVDICreate = true
			}
			writeXMLRPCString(t, w, "OpaqueRef:vdi")
		case "VBD.create":
			writeXMLRPCString(t, w, "OpaqueRef:vbd")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	drive, err := client.AttachDisk(context.Background(), xcpNgDiskAttachRequest{
		VMRef:      "OpaqueRef:vm",
		SRRef:      "OpaqueRef:sr",
		Name:       "install-disk",
		SizeBytes:  24 * 1024 * 1024 * 1024,
		DestroyVDI: true,
		Labels:     map[string]string{"lease": "cbx_lease"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawRawVDICreate {
		t.Fatal("VDI.create did not request sm_config:type=raw")
	}
	if drive.VDIRef != "OpaqueRef:vdi" || drive.VBDRef != "OpaqueRef:vbd" || !drive.DestroyVDI {
		t.Fatalf("drive=%#v", drive)
	}
}

func TestAttachDiskReturnsRecoveryHandleWhenRollbackFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch method := readXMLRPCMethod(t, r); method {
		case "VDI.create":
			writeXMLRPCString(t, w, "OpaqueRef:vdi")
		case "VBD.create":
			writeXMLRPCFault(t, w, "SR_BACKEND_FAILURE")
		case "VDI.destroy":
			writeXMLRPCFault(t, w, "VDI_IN_USE")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}

	drive, err := client.AttachDisk(context.Background(), xcpNgDiskAttachRequest{
		VMRef:      "OpaqueRef:vm",
		SRRef:      "OpaqueRef:sr",
		DestroyVDI: true,
	})

	if err == nil || !strings.Contains(err.Error(), "rollback xcp-ng drive OpaqueRef:vdi") {
		t.Fatalf("err=%v", err)
	}
	if drive.VDIRef != "OpaqueRef:vdi" || !drive.DestroyVDI {
		t.Fatalf("recovery drive=%#v", drive)
	}
}

func TestImportISOReturnsRecoveryHandleWhenRollbackFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installer.iso")
	if err := os.WriteFile(path, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/import_raw_vdi/" {
			http.Error(w, "upload failed", http.StatusInternalServerError)
			return
		}
		switch method := readXMLRPCMethod(t, r); method {
		case "VDI.create":
			writeXMLRPCString(t, w, "OpaqueRef:vdi")
		case "task.create":
			writeXMLRPCString(t, w, "OpaqueRef:task")
		case "task.destroy":
			writeXMLRPCString(t, w, "true")
		case "VDI.destroy":
			writeXMLRPCFault(t, w, "VDI_IN_USE")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}

	drive, err := client.ImportISO(context.Background(), xcpNgImportISORequest{
		SRRef:      "OpaqueRef:sr",
		Path:       path,
		DestroyVDI: true,
	})

	if err == nil || !strings.Contains(err.Error(), "rollback xcp-ng drive OpaqueRef:vdi") {
		t.Fatalf("err=%v", err)
	}
	if drive.VDIRef != "OpaqueRef:vdi" || !drive.DestroyVDI {
		t.Fatalf("recovery drive=%#v", drive)
	}
}

func TestAttachConfigDriveCreatesImportsAndAttachesVDI(t *testing.T) {
	var methods []string
	var imported bool
	var sawRawVDICreate bool
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
			if strings.Contains(body, `<name>sm_config</name><value><struct><member><name>type</name><value><string>raw</string></value></member></struct></value>`) {
				sawRawVDICreate = true
			}
			for _, want := range []string{"xenstore_data", "sm_config", "tags"} {
				if !strings.Contains(body, "<name>"+want+"</name>") {
					t.Fatalf("VDI.create missing %s, body=%s", want, body)
				}
			}
			writeXMLRPCString(t, w, "OpaqueRef:vdi")
		case "task.create":
			writeXMLRPCString(t, w, "OpaqueRef:task")
		case "task.get_status":
			writeXMLRPCString(t, w, "success")
		case "task.destroy":
			writeXMLRPCString(t, w, "true")
		case "VBD.create":
			if !strings.Contains(body, "<name>mode</name><value><string>RW</string>") {
				t.Fatalf("VBD.create must attach HVM config drive read-write, body=%s", body)
			}
			for _, want := range []string{"qos_algorithm_type", "qos_algorithm_params", "qos_supported_algorithms"} {
				if !strings.Contains(body, "<name>"+want+"</name>") {
					t.Fatalf("VBD.create missing %s, body=%s", want, body)
				}
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
	if !sawRawVDICreate {
		t.Fatal("VDI.create did not request sm_config:type=raw")
	}
	if got := strings.Join(methods, ","); got != "VDI.create,task.create,task.get_status,task.destroy,VBD.create" {
		t.Fatalf("methods=%s", got)
	}
}

func TestImportISOCreatesRawVDI(t *testing.T) {
	dir := t.TempDir()
	isoPath := filepath.Join(dir, "installer.iso")
	if err := os.WriteFile(isoPath, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	var methods []string
	var imported bool
	var sawRawVDICreate bool
	var setReadOnly bool
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
			if strings.Contains(body, `<name>sm_config</name><value><struct><member><name>type</name><value><string>raw</string></value></member></struct></value>`) {
				sawRawVDICreate = true
			}
			writeXMLRPCString(t, w, "OpaqueRef:vdi")
		case "task.create":
			writeXMLRPCString(t, w, "OpaqueRef:task")
		case "task.get_status":
			writeXMLRPCString(t, w, "success")
		case "task.destroy":
			writeXMLRPCString(t, w, "true")
		case "VDI.set_read_only":
			setReadOnly = true
			writeXMLRPCString(t, w, "true")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	drive, err := client.ImportISO(context.Background(), xcpNgImportISORequest{
		SRRef:        "OpaqueRef:sr",
		Path:         isoPath,
		Name:         "installer.iso",
		Description:  "Crabbox imported installer media",
		DestroyVDI:   true,
		MarkReadOnly: true,
		Labels:       map[string]string{"lease": "cbx_lease"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawRawVDICreate {
		t.Fatal("VDI.create did not request sm_config:type=raw")
	}
	if !imported || !setReadOnly {
		t.Fatalf("imported=%v setReadOnly=%v", imported, setReadOnly)
	}
	if drive.VDIRef != "OpaqueRef:vdi" || !drive.DestroyVDI {
		t.Fatalf("drive=%#v", drive)
	}
	if got := strings.Join(methods, ","); got != "VDI.create,task.create,task.get_status,task.destroy,VDI.set_read_only" {
		t.Fatalf("methods=%s", got)
	}
}

func TestDiscoverIPv4ByMACFallsBackToLocalProbe(t *testing.T) {
	oldReadARPTable := xcpNgReadARPTable
	oldLocalIPv4Networks := xcpNgLocalIPv4Networks
	oldProbeTCPAddress := xcpNgProbeTCPAddress
	var arpReads int
	var probeCount atomic.Int32
	xcpNgReadARPTable = func(context.Context) (map[string]string, error) {
		arpReads++
		if arpReads == 1 {
			return map[string]string{}, nil
		}
		return map[string]string{"aa:bb:cc:dd:ee:ff": "192.0.2.88"}, nil
	}
	xcpNgLocalIPv4Networks = func() ([]net.IPNet, error) {
		return []net.IPNet{{IP: net.IPv4(192, 0, 2, 0), Mask: net.CIDRMask(24, 32)}}, nil
	}
	xcpNgProbeTCPAddress = func(_ context.Context, address string, _ time.Duration) error {
		probeCount.Add(1)
		return errors.New("connection refused")
	}
	t.Cleanup(func() {
		xcpNgReadARPTable = oldReadARPTable
		xcpNgLocalIPv4Networks = oldLocalIPv4Networks
		xcpNgProbeTCPAddress = oldProbeTCPAddress
	})
	ip, err := discoverIPv4ByMAC(context.Background(), []string{"AA-BB-CC-DD-EE-FF"}, "192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.0.2.88" {
		t.Fatalf("ip=%s", ip)
	}
	if probeCount.Load() == 0 {
		t.Fatal("expected TCP probe sweep")
	}
}

func TestGuestProbeNetworksRequiresBoundedAttachedCIDR(t *testing.T) {
	networks := []net.IPNet{{IP: net.IPv4(192, 0, 2, 0), Mask: net.CIDRMask(24, 32)}}

	got, err := guestProbeNetworks(networks, "192.0.2.64/26")
	if err != nil || len(got) != 1 || got[0].String() != "192.0.2.64/26" {
		t.Fatalf("networks=%v err=%v", got, err)
	}
	for _, cidr := range []string{"", "198.51.100.0/24", "192.0.0.0/16", "not-a-cidr"} {
		if _, err := guestProbeNetworks(networks, cidr); err == nil {
			t.Fatalf("guest CIDR %q unexpectedly accepted", cidr)
		}
	}
	halfSubnet := []net.IPNet{{IP: net.IPv4(192, 0, 2, 128), Mask: net.CIDRMask(25, 32)}}
	if _, err := guestProbeNetworks(halfSubnet, "192.0.2.130/24"); err == nil {
		t.Fatal("guest CIDR extending outside local interface was accepted")
	}
}

func TestEnumerateIPv4HostsSupportsPointToPointAndSingleHostRanges(t *testing.T) {
	for _, tc := range []struct {
		cidr string
		want []string
	}{
		{cidr: "192.0.2.10/32", want: []string{"192.0.2.10"}},
		{cidr: "192.0.2.10/31", want: []string{"192.0.2.10", "192.0.2.11"}},
	} {
		_, network, err := net.ParseCIDR(tc.cidr)
		if err != nil {
			t.Fatal(err)
		}
		if got := enumerateIPv4Hosts(*network); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("cidr=%s hosts=%v want %v", tc.cidr, got, tc.want)
		}
	}
}

func TestClampIPv4NetworkPreservesPointToPointAndSingleHostMasks(t *testing.T) {
	for _, cidr := range []string{"10.0.1.10/16", "192.0.2.10/31", "192.0.2.10/32"} {
		ip, network, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		want := network.String()
		network.IP = ip
		got, ok := clampIPv4Network(*network)
		if !ok || got.String() != want {
			t.Fatalf("cidr=%s got=%s want=%s ok=%v", cidr, got.String(), want, ok)
		}
	}
}

func TestGuestProbeNetworksAcceptsBoundedCIDRWithinBroadLocalInterface(t *testing.T) {
	networks := []net.IPNet{{IP: net.IPv4(10, 0, 0, 0), Mask: net.CIDRMask(16, 32)}}
	got, err := guestProbeNetworks(networks, "10.0.2.0/24")
	if err != nil || len(got) != 1 || got[0].String() != "10.0.2.0/24" {
		t.Fatalf("networks=%v err=%v", got, err)
	}
}

func TestReadARPTableAcceptsSuccessfulEmptyReader(t *testing.T) {
	oldRun := xcpNgRunNeighborCommand
	xcpNgRunNeighborCommand = func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name == "arp" {
			return nil, exec.ErrNotFound
		}
		return []byte{}, nil
	}
	t.Cleanup(func() { xcpNgRunNeighborCommand = oldRun })

	table, err := readARPTable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(table) != 0 {
		t.Fatalf("table=%v", table)
	}
}

func TestResolveISOMediaAcceptsWritableSRVDI(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VDI.get_by_uuid":
			writeXMLRPCString(t, w, "OpaqueRef:vdi")
		case "VDI.get_record":
			writeXMLRPCVDIRecord(t, w, "installer.iso")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	iso, err := client.ResolveISOMedia(context.Background(), xcpNgProviderConfig(testConfig()), xcpNgTestVMUUID)
	if err != nil {
		t.Fatal(err)
	}
	if iso.VDIRef != "OpaqueRef:vdi" || iso.NameLabel != "installer.iso" || iso.Source != "sr-vdi" {
		t.Fatalf("iso=%#v", iso)
	}
	if got := strings.Join(methods, ","); got != "VDI.get_by_uuid,VDI.get_record" {
		t.Fatalf("methods=%s", got)
	}
}

func TestStartVMCallsXAPIStartWithPausedAndForceFlagsFalse(t *testing.T) {
	var body string
	var methods []string
	domidPolls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, requestBody := readXMLRPCBodyAndMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VM.start":
			body = requestBody
			writeXMLRPCString(t, w, "true")
		case "VM.get_power_state":
			writeXMLRPCString(t, w, "Running")
		case "VM.get_domid":
			domidPolls++
			if domidPolls == 1 {
				writeXMLRPCString(t, w, "-1")
				return
			}
			writeXMLRPCString(t, w, "56")
		default:
			t.Fatalf("method=%s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	if err := client.StartVM(context.Background(), "OpaqueRef:vm"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<methodName>VM.start</methodName>",
		"<string>OpaqueRef:session</string>",
		"<string>OpaqueRef:vm</string>",
		"<boolean>0</boolean>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %s: %s", want, body)
		}
	}
	if strings.Count(body, "<boolean>0</boolean>") != 2 {
		t.Fatalf("VM.start should pass paused=false and force=false, body=%s", body)
	}
	if got := strings.Join(methods, ","); got != "VM.start,VM.get_power_state,VM.get_domid,VM.get_domid" {
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
		case "VM.get_other_config":
			if !strings.Contains(strings.Join(methods, ","), "VM.get_by_uuid") {
				t.Fatalf("set labels did not resolve UUID first; methods=%v", methods)
			}
			writeXMLRPCStringMap(t, w, map[string]string{"existing": "preserved"})
		case "VM.remove_from_other_config", "VM.add_to_other_config":
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
	if got := strings.Join(methods, ","); !strings.Contains(got, "VM.get_by_uuid,VM.get_record") || !strings.Contains(got, "VM.get_by_uuid,VM.get_other_config,VM.remove_from_other_config,VM.add_to_other_config") {
		t.Fatalf("methods=%s", got)
	}
	if strings.Contains(strings.Join(methods, ","), "VM.get_guest_metrics") {
		t.Fatalf("metadata lookup queried guest metrics: %v", methods)
	}
}

func TestRenderedLabelsRoundTripEmbeddedNewlinesWithoutInjection(t *testing.T) {
	labels := map[string]string{
		"provider":  "xcp-ng",
		"work_root": "/work/crabbox\nprovider=other",
	}

	got := parseRenderedLabels(renderLabels(labels))

	if !reflect.DeepEqual(got, labels) {
		t.Fatalf("labels=%#v want %#v", got, labels)
	}
}

func TestParseRenderedLabelsSupportsLegacyRecords(t *testing.T) {
	got := parseRenderedLabels("provider=xcp-ng\nwork_root=/work/crabbox\n")
	if got["provider"] != "xcp-ng" || got["work_root"] != "/work/crabbox" {
		t.Fatalf("labels=%#v", got)
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

func TestDiscoverGuestIPv4ResolvesUUIDToFreshRef(t *testing.T) {
	oldReadARPTable := xcpNgReadARPTable
	xcpNgReadARPTable = func(context.Context) (map[string]string, error) {
		return map[string]string{"02:00:00:00:00:55": "192.0.2.55"}, nil
	}
	t.Cleanup(func() {
		xcpNgReadARPTable = oldReadARPTable
	})

	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, body := readXMLRPCBodyAndMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VM.get_by_uuid":
			writeXMLRPCString(t, w, "OpaqueRef:vm-fresh")
		case "VM.get_VIFs":
			if !strings.Contains(body, "<string>OpaqueRef:vm-fresh</string>") {
				t.Fatalf("VM.get_VIFs did not use resolved ref, body=%s", body)
			}
			writeXMLRPCStringArray(t, w, []string{"OpaqueRef:vif"})
		case "VIF.get_MAC":
			writeXMLRPCString(t, w, "02:00:00:00:00:55")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	ip, err := client.DiscoverGuestIPv4(context.Background(), xapiRef(xcpNgTestVMUUID))
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.0.2.55" {
		t.Fatalf("ip=%q", ip)
	}
	if got := strings.Join(methods, ","); got != "VM.get_by_uuid,VM.get_VIFs,VIF.get_MAC" {
		t.Fatalf("methods=%s", got)
	}
}

func TestResolveTemplateSRAndNetworkChooseUUIDNameOrEmpty(t *testing.T) {
	tests := []struct {
		name       string
		resolve    func(context.Context, *xapiClient) (xapiRef, error)
		wantMethod string
		wantRef    xapiRef
		wantErr    string
	}{
		{
			name: "template uuid",
			resolve: func(ctx context.Context, client *xapiClient) (xapiRef, error) {
				return client.ResolveTemplate(ctx, xcpNgConfig{TemplateUUID: xcpNgTestVMUUID, Template: "ignored"})
			},
			wantMethod: "VM.get_by_uuid",
			wantRef:    "OpaqueRef:by-uuid",
		},
		{
			name: "template name",
			resolve: func(ctx context.Context, client *xapiClient) (xapiRef, error) {
				return client.ResolveTemplate(ctx, xcpNgConfig{Template: "Ubuntu Ready"})
			},
			wantMethod: "VM.get_by_name_label",
			wantRef:    "OpaqueRef:by-name",
		},
		{
			name: "missing template",
			resolve: func(ctx context.Context, client *xapiClient) (xapiRef, error) {
				return client.ResolveTemplate(ctx, xcpNgConfig{})
			},
			wantErr: "template or template UUID is required",
		},
		{
			name: "sr uuid",
			resolve: func(ctx context.Context, client *xapiClient) (xapiRef, error) {
				return client.ResolveSR(ctx, xcpNgConfig{SRUUID: "33333333-3333-3333-3333-333333333333", SR: "ignored"})
			},
			wantMethod: "SR.get_by_uuid",
			wantRef:    "OpaqueRef:by-uuid",
		},
		{
			name: "sr name",
			resolve: func(ctx context.Context, client *xapiClient) (xapiRef, error) {
				return client.ResolveSR(ctx, xcpNgConfig{SR: "default-sr"})
			},
			wantMethod: "SR.get_by_name_label",
			wantRef:    "OpaqueRef:by-name",
		},
		{
			name: "missing sr",
			resolve: func(ctx context.Context, client *xapiClient) (xapiRef, error) {
				return client.ResolveSR(ctx, xcpNgConfig{})
			},
			wantErr: "sr or sr UUID is required",
		},
		{
			name: "network uuid",
			resolve: func(ctx context.Context, client *xapiClient) (xapiRef, error) {
				return client.ResolveNetwork(ctx, xcpNgConfig{NetworkUUID: "44444444-4444-4444-4444-444444444444", Network: "ignored"})
			},
			wantMethod: "network.get_by_uuid",
			wantRef:    "OpaqueRef:by-uuid",
		},
		{
			name: "network name",
			resolve: func(ctx context.Context, client *xapiClient) (xapiRef, error) {
				return client.ResolveNetwork(ctx, xcpNgConfig{Network: "pool-network"})
			},
			wantMethod: "network.get_by_name_label",
			wantRef:    "OpaqueRef:by-name",
		},
		{
			name: "missing optional network",
			resolve: func(ctx context.Context, client *xapiClient) (xapiRef, error) {
				return client.ResolveNetwork(ctx, xcpNgConfig{})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var methods []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method := readXMLRPCMethod(t, r)
				methods = append(methods, method)
				switch method {
				case "VM.get_by_uuid", "SR.get_by_uuid", "network.get_by_uuid":
					writeXMLRPCString(t, w, "OpaqueRef:by-uuid")
				case "VM.get_by_name_label", "SR.get_by_name_label", "network.get_by_name_label":
					writeXMLRPCStringArray(t, w, []string{"OpaqueRef:by-name"})
				case "VM.get_is_a_template":
					writeXMLRPCString(t, w, "true")
				default:
					t.Fatalf("unexpected method %s", method)
				}
			}))
			defer server.Close()
			client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
			ref, err := tt.resolve(context.Background(), client)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v want %q", err, tt.wantErr)
				}
				if len(methods) != 0 {
					t.Fatalf("methods=%v, want none", methods)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ref != tt.wantRef {
				t.Fatalf("ref=%q want %q", ref, tt.wantRef)
			}
			wantMethods := tt.wantMethod
			if strings.HasPrefix(tt.wantMethod, "VM.") {
				wantMethods += ",VM.get_is_a_template"
			}
			if got := strings.Join(methods, ","); got != wantMethods {
				t.Fatalf("methods=%s want %s", got, wantMethods)
			}
		})
	}
}

func TestResolveTemplateRejectsOrdinaryVM(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch method := readXMLRPCMethod(t, r); method {
		case "VM.get_by_uuid":
			writeXMLRPCString(t, w, "OpaqueRef:ordinary-vm")
		case "VM.get_is_a_template":
			writeXMLRPCString(t, w, "false")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	if _, err := client.ResolveTemplate(context.Background(), xcpNgConfig{TemplateUUID: xcpNgTestVMUUID}); err == nil || !strings.Contains(err.Error(), "is not a template") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveByNameRejectsMissingAndAmbiguousMatches(t *testing.T) {
	tests := []struct {
		name    string
		refs    []string
		wantErr string
	}{
		{name: "missing", refs: nil, wantErr: "not found by name"},
		{name: "ambiguous", refs: []string{"OpaqueRef:one", "OpaqueRef:two"}, wantErr: "name is ambiguous"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if method := readXMLRPCMethod(t, r); method != "VM.get_by_name_label" {
					t.Fatalf("method=%s", method)
				}
				writeXMLRPCStringArray(t, w, tt.refs)
			}))
			defer server.Close()
			client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
			_, err := client.ResolveTemplate(context.Background(), xcpNgConfig{Template: "Ubuntu Ready"})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err=%v want %q", err, tt.wantErr)
			}
		})
	}
}

func TestResolveHostUsesUUIDOnlyForStrictUUIDs(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		wantMethod string
	}{
		{name: "strict UUID", host: xcpNgTestVMUUID, wantMethod: "host.get_by_uuid"},
		{name: "long hyphenated name", host: "pool-master-11111111-1111-1111-1111-node", wantMethod: "host.get_by_name_label"},
		{name: "opaque ref shaped like UUID", host: "OpaqueRef:22222222-2222-2222-2222-222222222222", wantMethod: "host.get_by_name_label"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var methods []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method := readXMLRPCMethod(t, r)
				methods = append(methods, method)
				switch method {
				case "host.get_by_uuid":
					writeXMLRPCString(t, w, "OpaqueRef:host-by-uuid")
				case "host.get_by_name_label":
					writeXMLRPCStringArray(t, w, []string{"OpaqueRef:host-by-name"})
				default:
					t.Fatalf("unexpected method %s", method)
				}
			}))
			defer server.Close()
			client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
			ref, err := client.ResolveHost(context.Background(), xcpNgConfig{Host: tt.host})
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(methods, ","); got != tt.wantMethod {
				t.Fatalf("methods=%s want %s", got, tt.wantMethod)
			}
			if ref == "" {
				t.Fatal("expected host ref")
			}
		})
	}
}

func TestWaitForTaskSuccessReportsFailureInfo(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "task.get_status":
			writeXMLRPCString(t, w, "failure")
		case "task.get_error_info":
			writeXMLRPCStringArray(t, w, []string{"SR_BACKEND_FAILURE", "disk full"})
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	err := client.waitForTaskSuccess(context.Background(), "OpaqueRef:task")
	if err == nil || !strings.Contains(err.Error(), "failure SR_BACKEND_FAILURE: disk full") {
		t.Fatalf("err=%v", err)
	}
	if got := strings.Join(methods, ","); got != "task.get_status,task.get_error_info" {
		t.Fatalf("methods=%s", got)
	}
}

func TestWaitForTaskSuccessRedactsSessionFromTaskErrorInfo(t *testing.T) {
	var methods []string
	session := "OpaqueRef:session-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "task.get_status":
			writeXMLRPCString(t, w, "failure")
		case "task.get_error_info":
			writeXMLRPCStringArray(t, w, []string{"UPLOAD_FAILED", "session_id=" + session, "raw=" + session})
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: session, http: server.Client()}
	err := client.waitForTaskSuccess(context.Background(), "OpaqueRef:task")
	if err == nil {
		t.Fatal("expected task failure")
	}
	if strings.Contains(err.Error(), session) {
		t.Fatalf("task error leaked session: %v", err)
	}
	for _, want := range []string{"UPLOAD_FAILED", "session_id=<redacted>", "raw=<redacted>"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%v missing %q", err, want)
		}
	}
	if got := strings.Join(methods, ","); got != "task.get_status,task.get_error_info" {
		t.Fatalf("methods=%s", got)
	}
}

func TestWaitForTaskSuccessTimesOutWithoutSleeping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if method := readXMLRPCMethod(t, r); method != "task.get_status" {
			t.Fatalf("method=%s", method)
		}
		writeXMLRPCString(t, w, "pending")
	}))
	defer server.Close()
	oldTimeout := xcpNgTaskTimeout
	xcpNgTaskTimeout = 0
	t.Cleanup(func() { xcpNgTaskTimeout = oldTimeout })
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	err := client.waitForTaskSuccess(context.Background(), "OpaqueRef:task")
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for xcp-ng upload task OpaqueRef:task") {
		t.Fatalf("err=%v", err)
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
		case "VM.get_is_a_template":
			writeXMLRPCString(t, w, "false")
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

func TestDeleteFreshServerForcesUnlabeledAllocationCleanup(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VM.get_record":
			writeXMLRPCUnmanagedVMRecord(t, w)
		case "VM.get_VBDs":
			writeXMLRPCStringArray(t, w, nil)
		case "VM.get_power_state":
			writeXMLRPCString(t, w, "Halted")
		case "VTPM.destroy", "VM.destroy":
			writeXMLRPCString(t, w, "true")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}

	if err := client.DeleteFreshServer(context.Background(), "OpaqueRef:vm", "OpaqueRef:vtpm"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(methods, ",")
	haltIndex := strings.Index(got, "VM.get_power_state")
	vtpmIndex := strings.Index(got, "VTPM.destroy")
	vmIndex := strings.Index(got, "VM.destroy")
	if haltIndex < 0 || vtpmIndex <= haltIndex || vmIndex <= vtpmIndex {
		t.Fatalf("delete order=%s", got)
	}
}

func TestAttachedDestroyableDisksExcludesExternalCDMedia(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VM.get_VBDs":
			writeXMLRPCStringArray(t, w, []string{"OpaqueRef:external-cd-vbd", "OpaqueRef:install-disk-vbd"})
		case "VBD.get_record":
			if countMethod(methods, "VBD.get_record") == 1 {
				writeXMLRPCVBDRecordWithType(t, w, "OpaqueRef:external-iso-vdi", "CD")
			} else {
				writeXMLRPCVBDRecordWithType(t, w, "OpaqueRef:install-disk-vdi", "Disk")
			}
		case "VDI.get_record":
			writeXMLRPCVDIRecord(t, w, "crabbox-install-disk")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}

	disks, err := client.attachedDestroyableDisks(context.Background(), "OpaqueRef:vm", map[string]struct{}{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 1 || disks[0].VDIRef != "OpaqueRef:install-disk-vdi" {
		t.Fatalf("disks=%#v", disks)
	}
	if countMethod(methods, "VDI.get_record") != 1 {
		t.Fatalf("external CD VDI was inspected as destroyable: methods=%v", methods)
	}
}

func TestDeleteVTPMTreatsMissingHandleAsDeleted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if method := readXMLRPCMethod(t, r); method != "VTPM.destroy" {
			t.Fatalf("unexpected method %s", method)
		}
		writeXMLRPCFault(t, w, "HANDLE_INVALID")
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	if err := client.DeleteVTPM(context.Background(), "OpaqueRef:missing"); err != nil {
		t.Fatal(err)
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

func TestDeleteServerRefusesUnmanagedTemplateBeforeDiskCleanup(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VM.get_record":
			writeXMLRPCUnmanagedVMRecord(t, w)
		case "VM.get_is_a_template", "VM.get_VBDs", "VDI.destroy", "VM.destroy":
			t.Fatalf("%s must not run for unmanaged template", method)
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	if err := client.DeleteServer(context.Background(), "OpaqueRef:user-template"); err == nil || !strings.Contains(err.Error(), "refusing to delete non-Crabbox") {
		t.Fatalf("err=%v", err)
	}
	if got := strings.Join(methods, ","); got != "VM.get_record" {
		t.Fatalf("methods=%s", got)
	}
}

func TestDiscoverIPv4ByMACFiltersNeighborEntriesByGuestCIDR(t *testing.T) {
	oldReadARPTable := xcpNgReadARPTable
	xcpNgReadARPTable = func(context.Context) (map[string]string, error) {
		return map[string]string{
			"aa:bb:cc:dd:ee:01": "10.0.0.88",
			"aa:bb:cc:dd:ee:02": "192.0.2.88",
		}, nil
	}
	t.Cleanup(func() {
		xcpNgReadARPTable = oldReadARPTable
	})

	ip, err := discoverIPv4ByMAC(context.Background(), []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}, "192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.0.2.88" {
		t.Fatalf("ip=%q", ip)
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
	if err := client.DeleteConfigDrive(context.Background(), xcpNgConfigDrive{VBDRef: "OpaqueRef:vbd", VDIRef: "OpaqueRef:vdi", DestroyVDI: true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(methods, ","); got != "VBD.unplug,VBD.destroy,VDI.destroy" {
		t.Fatalf("methods=%s", got)
	}
}

func TestDeleteConfigDriveTreatsHaltedPowerStateUnplugFaultAsCleanedUp(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VBD.unplug":
			writeXAPIStatusFailure(t, w, []string{"VM_BAD_POWER_STATE", "OpaqueRef:vm", "running", "halted"})
		case "VBD.destroy", "VDI.destroy":
			writeXMLRPCString(t, w, "true")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	if err := client.DeleteConfigDrive(context.Background(), xcpNgConfigDrive{VBDRef: "OpaqueRef:vbd", VDIRef: "OpaqueRef:vdi", DestroyVDI: true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(methods, ","); got != "VBD.unplug,VBD.destroy,VDI.destroy" {
		t.Fatalf("methods=%s", got)
	}
}

func TestHaltedPowerStateFaultRequiresActualHaltedState(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "actual halted",
			err: xapiStatusError{Fields: map[string]xmlRPCValue{
				"ErrorDescription": {Array: []xmlRPCValue{
					{String: "VM_BAD_POWER_STATE"},
					{String: "OpaqueRef:vm"},
					{String: "running"},
					{String: "halted"},
				}},
			}},
			want: true,
		},
		{
			name: "actual running",
			err: xapiStatusError{Fields: map[string]xmlRPCValue{
				"ErrorDescription": {Array: []xmlRPCValue{
					{String: "VM_BAD_POWER_STATE"},
					{String: "OpaqueRef:vm"},
					{String: "halted"},
					{String: "running"},
				}},
			}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isXAPIHaltedPowerStateFault(tt.err); got != tt.want {
				t.Fatalf("isXAPIHaltedPowerStateFault()=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeleteConfigDriveTreatsNotUnpluggableVBDAsDestroyable(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := readXMLRPCMethod(t, r)
		methods = append(methods, method)
		switch method {
		case "VBD.unplug":
			writeXAPIStatusFailure(t, w, []string{"VBD_NOT_UNPLUGGABLE", "OpaqueRef:vbd"})
		case "VBD.destroy", "VDI.destroy":
			writeXMLRPCString(t, w, "true")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	if err := client.DeleteConfigDrive(context.Background(), xcpNgConfigDrive{VBDRef: "OpaqueRef:vbd", VDIRef: "OpaqueRef:vdi", DestroyVDI: true}); err != nil {
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
	cfg.XCPNg.Password = "pa&<ss"
	_, err := newXAPIClient(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected login HTTP error")
	}
	text := err.Error()
	if strings.Contains(text, cfg.XCPNg.Password) {
		t.Fatalf("error leaked password: %s", text)
	}
	if strings.Contains(text, "pa&amp;&lt;ss") {
		t.Fatalf("error leaked XML-escaped password: %s", text)
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

func TestShutdownVMTreatsBadPowerStateAsHaltedWhenStateConfirms(t *testing.T) {
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
			writeXAPIStatusFailure(t, w, []string{"VM_BAD_POWER_STATE", "OpaqueRef:vm", "running", "halted"})
		case "VM.hard_shutdown":
			t.Fatal("hard shutdown should not run once XAPI reports the VM is already halted")
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}))
	defer server.Close()
	client := &xapiClient{endpoint: server.URL, session: "OpaqueRef:session", http: server.Client()}
	if err := client.shutdownVM(context.Background(), "OpaqueRef:vm"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(methods, ",")
	if got != "VM.get_power_state,VM.clean_shutdown,VM.get_power_state" {
		t.Fatalf("methods=%s", got)
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

func TestImportRawVDIRedactsEndpointUserinfoFromTransportError(t *testing.T) {
	client := &xapiClient{
		endpoint: "http://api-user:api-password@xcp-ng.example.test/",
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
	for _, secret := range []string{"api-user", "api-password", "api-user:api-password"} {
		if strings.Contains(text, secret) {
			t.Fatalf("error leaked endpoint userinfo %q: %s", secret, text)
		}
	}
	if strings.Contains(text, "***") {
		t.Fatalf("error leaked masked password context instead of redacting userinfo: %s", text)
	}
	if !strings.Contains(text, "http://<redacted>@xcp-ng.example.test/import_raw_vdi/") {
		t.Fatalf("error did not preserve redacted upload URL context: %s", text)
	}
}

func TestXMLRPCTransportErrorRedactsEndpointUserinfo(t *testing.T) {
	client := &xapiClient{
		endpoint: "http://api-user:api-password@xcp-ng.example.test/",
		http: &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return nil, errors.New("dial failed for " + req.URL.String())
		})},
	}
	_, err := client.callRaw(context.Background(), "session.login_with_password", "api-user", "api-password", "1.0", "crabbox")
	if err == nil {
		t.Fatal("expected XML-RPC transport error")
	}
	text := err.Error()
	for _, secret := range []string{"api-user", "api-password", "api-user:api-password"} {
		if strings.Contains(text, secret) {
			t.Fatalf("error leaked endpoint userinfo %q: %s", secret, text)
		}
	}
	if strings.Contains(text, "***") {
		t.Fatalf("error leaked masked password context instead of redacting userinfo: %s", text)
	}
	if !strings.Contains(text, "http://<redacted>@xcp-ng.example.test/") {
		t.Fatalf("error did not preserve redacted endpoint context: %s", text)
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

func TestImportRawVDIRedactsEndpointUserinfoFromHTTPErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeXMLRPCString(t, w, "OpaqueRef:task")
			return
		}
		http.Error(w, "proxy echoed http://api-user:api-password@"+r.Host+r.URL.String(), http.StatusBadGateway)
	}))
	defer server.Close()
	endpoint := strings.Replace(server.URL, "http://", "http://api-user:api-password@", 1)
	client := &xapiClient{
		endpoint: endpoint,
		session:  "OpaqueRef:secret-session",
		http:     server.Client(),
	}
	err := client.importRawVDI(context.Background(), "OpaqueRef:vdi", []byte("image"))
	if err == nil {
		t.Fatal("expected upload error")
	}
	text := err.Error()
	for _, secret := range []string{"api-user", "api-password", "api-user:api-password", "OpaqueRef:secret-session", "secret-session"} {
		if strings.Contains(text, secret) {
			t.Fatalf("error leaked %q: %s", secret, text)
		}
	}
	if strings.Contains(text, "***") {
		t.Fatalf("error leaked masked password context instead of redacting userinfo: %s", text)
	}
	if !strings.Contains(text, "http://<redacted>@") || !strings.Contains(text, "session_id=<redacted>") {
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

func writeXMLRPCStringMap(t *testing.T, w http.ResponseWriter, values map[string]string) {
	t.Helper()
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><methodResponse><params><param><value><struct>`)
	for _, key := range keys {
		b.WriteString(`<member><name>`)
		b.WriteString(key)
		b.WriteString(`</name><value><string>`)
		b.WriteString(values[key])
		b.WriteString(`</string></value></member>`)
	}
	b.WriteString(`</struct></value></param></params></methodResponse>`)
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

func writeXAPIStatusFailure(t *testing.T, w http.ResponseWriter, values []string) {
	t.Helper()
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><methodResponse><params><param><value><struct>`)
	b.WriteString(`<member><name>Status</name><value><string>Failure</string></value></member>`)
	b.WriteString(`<member><name>ErrorDescription</name><value><array><data>`)
	for _, value := range values {
		b.WriteString(`<value><string>`)
		b.WriteString(value)
		b.WriteString(`</string></value>`)
	}
	b.WriteString(`</data></array></value></member>`)
	b.WriteString(`</struct></value></param></params></methodResponse>`)
	if _, err := w.Write([]byte(b.String())); err != nil {
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
	writeXMLRPCVBDRecordWithType(t, w, vdiRef, "Disk")
}

func writeXMLRPCVBDRecordWithType(t *testing.T, w http.ResponseWriter, vdiRef, vbdType string) {
	t.Helper()
	response := `<?xml version="1.0"?><methodResponse><params><param><value><struct>
<member><name>empty</name><value><boolean>0</boolean></value></member>
<member><name>type</name><value><string>` + vbdType + `</string></value></member>
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

func writeXMLRPCManagedTemplateRecords(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	labels := "crabbox=true\ncreated_by=crabbox\nprovider=xcp-ng\nlease=cbx_recovery\nslug=recovery\nstate=provisioning\n"
	response := `<?xml version="1.0"?><methodResponse><params><param><value><struct>
<member><name>OpaqueRef:managed-template-copy</name><value><struct>
<member><name>uuid</name><value><string>managed-template-uuid</string></value></member>
<member><name>name_label</name><value><string>crabbox-recovery</string></value></member>
<member><name>power_state</name><value><string>Halted</string></value></member>
<member><name>is_a_template</name><value><boolean>1</boolean></value></member>
<member><name>other_config</name><value><struct><member><name>crabbox:labels</name><value><string>` + labels + `</string></value></member></struct></value></member>
</struct></value></member>
<member><name>OpaqueRef:unmanaged-template</name><value><struct>
<member><name>uuid</name><value><string>unmanaged-template-uuid</string></value></member>
<member><name>name_label</name><value><string>user-template</string></value></member>
<member><name>power_state</name><value><string>Halted</string></value></member>
<member><name>is_a_template</name><value><boolean>1</boolean></value></member>
<member><name>other_config</name><value><struct></struct></value></member>
</struct></value></member>
</struct></value></param></params></methodResponse>`
	if _, err := w.Write([]byte(response)); err != nil {
		t.Fatal(err)
	}
}
