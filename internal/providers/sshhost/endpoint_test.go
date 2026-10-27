package sshhost

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"golang.org/x/crypto/ssh"
)

func TestEndpointExecAuthenticationAndExpiry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	hostPrivate, hostPublic, err := generateHostKey()
	if err != nil {
		t.Fatal(err)
	}
	clientPrivate, clientPublic, err := generateHostKey()
	if err != nil {
		t.Fatal(err)
	}
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "endpoint.json")
	c := endpointConfig{PrivateKey: hostPrivate, PublicKey: clientPublic, Port: port, WorkRoot: t.TempDir(), Deadline: time.Now().Add(time.Minute).UnixMilli()}
	if err := writeJSON(path, c, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runEndpoint(ctx, path) }()
	if err := waitEndpoint(ctx, &runtimeState{HostKey: hostPublic, Port: port}); err != nil {
		t.Fatal(err)
	}
	signer, _ := ssh.ParsePrivateKey([]byte(clientPrivate))
	hostKey, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(hostPublic))
	config := &ssh.ClientConfig{User: "crabbox", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(hostKey), Timeout: time.Second}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	config.User = "operator"
	if client, err := ssh.Dial("tcp", address, config); err == nil {
		client.Close()
		t.Fatal("operator login accepted")
	}
	config.User = "crabbox"
	client, err := ssh.Dial("tcp", address, config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	input, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := session.Output("printf isolated-endpoint")
	session.Close()
	if err != nil || string(output) != "isolated-endpoint" {
		t.Fatalf("output=%q error=%v", output, err)
	}
	if connection, err := client.Dial("tcp", "127.0.0.1:22"); err == nil {
		connection.Close()
		t.Fatal("arbitrary forwarding accepted")
	}
	c.Deadline = time.Now().Add(-time.Second).UnixMilli()
	if err := writeJSON(path, c, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("endpoint did not close at expiry")
	}
	if _, err := client.NewSession(); err == nil {
		t.Fatal("expired endpoint accepted a session")
	}
}

func TestProviderAdmissionAndPoolIdentity(t *testing.T) {
	p := Provider{}
	if _, ok := p.CoordinatorRequestFields(core.Config{})["hostCapabilities"]; ok {
		t.Fatal("empty capabilities must be omitted, not null")
	}
	if !p.SupportsReadyPoolTarget(core.TargetWindows) || p.SupportsReadyPoolTarget(core.TargetMacOS) {
		t.Fatal("incorrect runtime targets")
	}
	req := core.ProviderReadyPoolImageIdentityRequest{Identity: core.CoordinatorReadyPoolImageIdentity{Provider: "ssh-host", Scope: "example-org", ID: "tools-v1"}, Lease: core.ProviderReadyPoolLeaseImageIdentity{Provider: "ssh-host", Region: "example-org", Image: &core.CoordinatorLeaseImage{Provider: "ssh-host", Kind: "host-template", Region: "example-org", ID: "tools-v1"}}}
	if !p.ReadyPoolImageIdentityMatchesLease(req) {
		t.Fatal("valid identity rejected")
	}
	req.Lease.Image.ID = "other"
	if p.ReadyPoolImageIdentityMatchesLease(req) {
		t.Fatal("changed template accepted")
	}
}

func TestPrivateStateAndAssignmentValidation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission fixture")
	}
	path := filepath.Join(t.TempDir(), "private")
	if err := secureStateDir(path); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateJSON(filepath.Join(path, "state.json"), map[string]string{"owned": "state"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(path, "state.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("state not private", err)
	}
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := secureStateDir(path); err == nil {
		t.Fatal("public state directory accepted")
	}
	_, err = newRuntimeState("host", hostAssignment{LeaseID: "../../foreign"})
	if err == nil {
		t.Fatal("invalid account identity accepted")
	}
	_, err = readEndpoint(filepath.Join(path, strings.Repeat("x", 10)))
	if err == nil {
		t.Fatal("missing endpoint accepted")
	}
}
