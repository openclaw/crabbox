package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func candidateIface(name string, flags net.Flags, addresses ...string) candidateInterface {
	iface := candidateInterface{Name: name, Flags: flags}
	for _, value := range addresses {
		ip, subnet, _ := net.ParseCIDR(value)
		subnet.IP = ip
		iface.Addresses = append(iface.Addresses, subnet)
	}
	return iface
}
func TestServeCandidateDetection(t *testing.T) {
	up := net.FlagUp | net.FlagBroadcast
	interfaces := []candidateInterface{
		candidateIface("en1", up, "192.168.1.20/24", "169.254.1.2/16", "8.8.8.8/24", "2001:db8::1/64"),
		candidateIface("utun0", net.FlagUp, "100.64.1.2/32", "10.4.1.1/24"),
		candidateIface("en0", up, "172.16.2.1/24", "10.1.2.3/24"),
		candidateIface("down", net.FlagBroadcast, "10.1.2.4/24"),
		candidateIface("lo0", up|net.FlagLoopback, "10.1.2.5/24"),
		candidateIface("point", net.FlagUp|net.FlagPointToPoint, "10.1.2.6/24"),
	}
	for _, prefix := range []string{"bridge", "vmnet", "docker", "br-", "veth", "llw", "awdl", "anpi"} {
		interfaces = append(interfaces, candidateIface(prefix+"0", up, "10.1.2.9/24"))
	}
	want := []addressCandidate{{"100.64.1.2", "tailnet"}, {"10.1.2.3", "lan"}, {"172.16.2.1", "lan"}, {"192.168.1.20", "lan"}}
	got, err := detectCandidates(nil, false, interfaces)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v", got, err)
	}
	got, err = detectCandidates(nil, true, interfaces)
	if err != nil || !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("no LAN: %+v %v", got, err)
	}
	got, err = detectCandidates([]string{"192.168.1.20", "100.64.1.2", "8.8.8.8"}, true, interfaces)
	if err != nil || len(got) != 3 || got[0].Address != "192.168.1.20" || got[2].Kind != "explicit" {
		t.Fatalf("explicit: %+v %v", got, err)
	}
	for _, address := range []string{"0.1.2.3", "240.1.2.3", "169.254.1.2"} {
		got, err := detectCandidates([]string{address}, false, interfaces)
		if err != nil || got[0].Address != address || got[0].Kind != "explicit" {
			t.Fatalf("explicit %s: %v %v", address, got, err)
		}
	}
	for _, values := range [][]string{{"127.0.0.2"}, {"0.0.0.0"}, {"224.0.0.1"}, {"::1"}, {"host"}, {"10.1.1.1", "10.1.1.1"}, {"10.1.1.1", "10.1.1.2", "10.1.1.3", "10.1.1.4", "10.1.1.5"}} {
		if _, err := detectCandidates(values, false, interfaces); err == nil {
			t.Fatalf("accepted %v", values)
		}
	}
	if _, err := detectCandidates(nil, true, []candidateInterface{}); err == nil {
		t.Fatal("expected no candidates")
	}
}
func TestServeCandidateMultiListenerRebind(t *testing.T) {
	lan := testServeLANAddress(t)
	// Loopback is a fixture transport only; production candidates reject it.
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port
	var log bytes.Buffer
	candidates := []addressCandidate{{"127.0.0.1", "explicit"}, {lan, "explicit"}}
	f, err := startFrontDoors(t.TempDir(), candidates, port, &log)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if len(f.candidates) != 1 || f.candidates[0].Address != lan || !strings.Contains(log.String(), "127.0.0.1") {
		t.Fatalf("%v %s", f.candidates, log.String())
	}
	retained := f.listeners[net.JoinHostPort(lan, strconv.Itoa(port))]
	occupied.Close()
	bound, err := f.rebind(candidates, port, &log)
	if err != nil || len(bound) != 2 || f.listeners[net.JoinHostPort(lan, strconv.Itoa(port))] != retained {
		t.Fatalf("rebind %v %v", bound, err)
	}
	before := f.signer.PublicKey().Marshal()
	bound, err = f.rebind([]addressCandidate{{"127.0.0.1", "explicit"}}, port, &log)
	if err != nil || len(bound) != 1 || !bytes.Equal(before, f.signer.PublicKey().Marshal()) {
		t.Fatalf("address change %v %v", bound, err)
	}
	if conn, err := net.Dial("tcp4", net.JoinHostPort(lan, strconv.Itoa(port))); err == nil {
		conn.Close()
		t.Fatal("old listener remains")
	}
	if _, err := f.rebind([]addressCandidate{{"192.0.2.254", "explicit"}}, port, &log); err == nil {
		t.Fatal("zero bound accepted")
	}
}
func TestServeCandidateFlagsOverrideConfiguration(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("user services are supported on macOS and Linux")
	}
	dir := t.TempDir()
	// Install exercises flag/config parsing and persistence without a container runtime.
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("CRABBOX_SERVE_ADVERTISE_ADDRESSES", "10.1.1.1,10.1.1.2")
	cfg := core.Config{Serve: core.ServeConfig{AdvertiseAddresses: []string{"10.2.1.1"}}}
	err := (Provider{}).RunAgent(context.Background(), cfg, core.Runtime{Exec: serviceRunner{}, Stdout: io.Discard, Stderr: io.Discard}, []string{"install", "--memory", "1g", "--advertise-address", "10.3.1.1", "--advertise-address", "10.3.1.2", "--no-lan-candidates"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "Library", "LaunchAgents", serviceName+".plist")
	if runtime.GOOS == "linux" {
		path = filepath.Join(dir, "systemd", "user", "crabbox-serve.service")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "10.3.1.1") || !strings.Contains(string(data), "10.3.1.2") || !strings.Contains(string(data), "--no-lan-candidates") || strings.Contains(string(data), "10.1.1.1") || strings.Contains(string(data), "10.2.1.1") {
		t.Fatal(string(data))
	}
	// serviceArguments is also used unchanged by both systemd and launchd.
	args := strings.Join(serviceArguments("crabbox", options{addresses: []string{"10.3.1.1", "10.3.1.2"}, noLAN: true}), " ")
	if !strings.Contains(args, "--advertise-address 10.3.1.1 --advertise-address 10.3.1.2 --no-lan-candidates") {
		t.Fatal(args)
	}
}

func TestServeCandidateHeartbeatRebind(t *testing.T) {
	lan := testServeLANAddress(t)
	f, err := startFrontDoors(t.TempDir(), []addressCandidate{{"127.0.0.1", "explicit"}}, 0, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	port := 0
	for _, listener := range f.listeners {
		port = listener.Addr().(*net.TCPAddr).Port
	}
	var advertised []machine
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m machine
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Error(err)
		}
		advertised = append(advertised, m)
		_ = json.NewEncoder(w).Encode(heartbeat{})
	}))
	defer server.Close()
	coord, err := core.NewCoordinatorClient(core.Config{Coordinator: server.URL, CoordToken: "synthetic-local-test"})
	if err != nil {
		t.Fatal(err)
	}
	candidates := []addressCandidate{{lan, "explicit"}}
	a := agent{frontDoor: f, opts: options{sshPort: port, stateDir: t.TempDir()}, state: localState{Machine: machine{MachineID: "test"}}, coord: coord, containers: &fakeContainers{}, rt: core.Runtime{Stderr: io.Discard}, detectAddresses: func() ([]addressCandidate, error) { return candidates, nil }}
	if err := a.poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	candidates = []addressCandidate{{"127.0.0.1", "explicit"}}
	if err := a.poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(advertised) != 2 || advertised[0].Address != lan || advertised[1].FrontDoor.Candidates[0].Address != "127.0.0.1" {
		t.Fatalf("%+v", advertised)
	}
	if conn, err := net.Dial("tcp4", net.JoinHostPort(lan, strconv.Itoa(port))); err == nil {
		conn.Close()
		t.Fatal("stale heartbeat listener")
	}
}

func testServeLANAddress(t *testing.T) string {
	t.Helper()
	interfaces, err := localCandidateInterfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		for _, address := range iface.Addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil && ip.IsPrivate() {
				return ip.String()
			}
		}
	}
	t.Skip("no private IPv4 interface for multi-address binding")
	return ""
}
