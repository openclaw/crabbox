package wandb

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sandboxv1 "github.com/openclaw/crabbox/internal/providers/wandb/gen/coreweave/sandbox/v1beta2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNormalizeStatus(t *testing.T) {
	for _, tc := range []struct {
		in   sandboxv1.SandboxStatus
		want string
	}{
		{sandboxv1.SandboxStatus_SANDBOX_STATUS_RUNNING, "running"},
		{sandboxv1.SandboxStatus_SANDBOX_STATUS_COMPLETED, "stopped"},
		{sandboxv1.SandboxStatus_SANDBOX_STATUS_FAILED, "failed"},
	} {
		if got := normalizeStatus(tc.in); got != tc.want {
			t.Fatalf("normalizeStatus(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatTimestampNil(t *testing.T) {
	if got := formatTimestamp(nil); got != "" {
		t.Fatalf("formatTimestamp(nil) = %q, want empty", got)
	}
}

func TestFormatTimestampValue(t *testing.T) {
	ts := timestamppb.New(time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC))
	if got := formatTimestamp(ts); got != "2026-05-25T12:00:00Z" {
		t.Fatalf("formatTimestamp(value) = %q", got)
	}
}

func TestMapRPCErrorExitCodes(t *testing.T) {
	for _, tc := range []struct {
		code codes.Code
		want int
	}{
		{codes.Unauthenticated, 77},
		{codes.PermissionDenied, 77},
		{codes.Unavailable, 69},
		{codes.ResourceExhausted, 69},
		{codes.DeadlineExceeded, 124},
		{codes.NotFound, 4},
	} {
		err := mapRPCError(status.Error(tc.code, "boom"), "op")
		var apiErr *wandbAPIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("code=%v: err = %T, want *wandbAPIError", tc.code, err)
		}
		if apiErr.ExitCode != tc.want {
			t.Fatalf("code=%v: exit = %d, want %d", tc.code, apiErr.ExitCode, tc.want)
		}
	}
}

func TestWandbAPIErrorAsExitError(t *testing.T) {
	err := &wandbAPIError{ExitCode: 77, Stderr: "auth failed", Code: codes.Unauthenticated}
	var ee ExitError
	if !errors.As(err, &ee) {
		t.Fatal("errors.As failed for *wandbAPIError -> ExitError")
	}
	if ee.Code != 77 || !strings.Contains(ee.Message, "auth failed") {
		t.Fatalf("ExitError = %#v", ee)
	}
}

func TestResolveAuthPrecedence(t *testing.T) {
	home := t.TempDir()
	netrc := "machine api.wandb.ai login user password netrc-key\n"
	if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte(netrc), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CRABBOX_WANDB_API_KEY", "crabbox-key")
	t.Setenv("WANDB_API_KEY", "wandb-key")
	t.Setenv("WANDB_ENTITY_NAME", "team")

	auth, err := resolveAuth(Config{Wandb: WandbConfig{APIKey: "cfg-key"}})
	if err != nil || auth.APIKey != "crabbox-key" || auth.Entity != "team" {
		t.Fatalf("CRABBOX precedence: auth=%#v err=%v", auth, err)
	}

	t.Setenv("CRABBOX_WANDB_API_KEY", "")
	auth, err = resolveAuth(Config{Wandb: WandbConfig{APIKey: "cfg-key"}})
	if err != nil || auth.APIKey != "cfg-key" {
		t.Fatalf("cfg precedence: auth=%#v err=%v", auth, err)
	}

	auth, err = resolveAuth(Config{})
	if err != nil || auth.APIKey != "wandb-key" {
		t.Fatalf("WANDB precedence: auth=%#v err=%v", auth, err)
	}

	t.Setenv("WANDB_API_KEY", "")
	auth, err = resolveAuth(Config{})
	if err != nil || auth.APIKey != "netrc-key" {
		t.Fatalf("netrc precedence: auth=%#v err=%v", auth, err)
	}
}

func TestReadNetrcWandbKeyHosts(t *testing.T) {
	for _, host := range []string{"api.wandb.ai", "api.wandb.com"} {
		t.Run(host, func(t *testing.T) {
			home := t.TempDir()
			netrc := "machine " + host + " login user password host-key\n"
			if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte(netrc), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			if got := readNetrcWandbKey(); got != "host-key" {
				t.Fatalf("readNetrcWandbKey() = %q, want host-key", got)
			}
		})
	}
}

func TestResolveEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name         string
		in           string
		wantTarget   string
		wantProtocol string
	}{
		{name: "raw target", in: "api.cwsandbox.com:443", wantTarget: "api.cwsandbox.com:443", wantProtocol: "tls"},
		{name: "https", in: "https://api.cwsandbox.com:443", wantTarget: "api.cwsandbox.com:443", wantProtocol: "tls"},
		{name: "http loopback", in: "http://127.0.0.1:50051/", wantTarget: "127.0.0.1:50051", wantProtocol: "insecure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, creds, err := resolveEndpoint(tc.in)
			if err != nil {
				t.Fatalf("resolveEndpoint(%q) err: %v", tc.in, err)
			}
			if target != tc.wantTarget {
				t.Fatalf("target = %q, want %q", target, tc.wantTarget)
			}
			if got := creds.Info().SecurityProtocol; got != tc.wantProtocol {
				t.Fatalf("security protocol = %q, want %q", got, tc.wantProtocol)
			}
		})
	}
}

func TestResolveEndpointRejectsURLPath(t *testing.T) {
	for _, endpoint := range []string{
		"https://api.cwsandbox.com:443/v2",
		"http://127.0.0.1:50051/service",
		"https://api.cwsandbox.com:443?debug=true",
		"https://api.cwsandbox.com:443#fragment",
	} {
		t.Run(endpoint, func(t *testing.T) {
			_, _, err := resolveEndpoint(endpoint)
			if err == nil {
				t.Fatalf("resolveEndpoint(%q) accepted URL path/query/fragment", endpoint)
			}
		})
	}
}

func TestResolveEndpointRejectsRemotePlaintext(t *testing.T) {
	for _, endpoint := range []string{
		"http://api.cwsandbox.com:443",
		"http://staging.example.com:50051",
		"http://192.0.2.10:50051",
	} {
		t.Run(endpoint, func(t *testing.T) {
			_, _, err := resolveEndpoint(endpoint)
			if err == nil || !strings.Contains(err.Error(), "non-loopback") {
				t.Fatalf("resolveEndpoint(%q) err = %v, want non-loopback rejection", endpoint, err)
			}
		})
	}
}

type versionGatewayServer struct {
	sandboxv1.UnimplementedGatewayServiceServer
}

func (versionGatewayServer) List(_ context.Context, req *sandboxv1.ListSandboxesRequest) (*sandboxv1.ListSandboxesResponse, error) {
	if req.GetPageSize() != 1 {
		return nil, status.Errorf(codes.InvalidArgument, "page_size = %d", req.GetPageSize())
	}
	return &sandboxv1.ListSandboxesResponse{}, nil
}

func TestWandbClientUsesPlaintextForHTTPOverride(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	sandboxv1.RegisterGatewayServiceServer(server, versionGatewayServer{})
	go func() {
		_ = server.Serve(lis)
	}()
	t.Cleanup(server.Stop)

	t.Setenv("CRABBOX_WANDB_API_KEY", "test-key")
	t.Setenv("WANDB_ENTITY_NAME", "test-entity")
	t.Setenv("CWSANDBOX_BASE_URL", "http://"+lis.Addr().String())
	api, err := newWandbClient(Config{}, Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	client, ok := api.(*wandbClient)
	if !ok {
		t.Fatalf("api = %T, want *wandbClient", api)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = client.Close()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	version, err := api.Version(ctx)
	if err != nil {
		t.Fatalf("Version with http override err: %v", err)
	}
	if version != "coreweave.sandbox.v1beta2" {
		t.Fatalf("version = %q", version)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close err: %v", err)
	}
	closed = true
	if got := client.conn.GetState(); got != connectivity.Shutdown {
		t.Fatalf("conn state = %s, want %s", got, connectivity.Shutdown)
	}
}

func TestStartupTimeout(t *testing.T) {
	if got := startupTimeout(0); got != defaultStartupTimeout {
		t.Fatalf("startupTimeout(0) = %s, want %s", got, defaultStartupTimeout)
	}
	if got := startupTimeout(60); got != time.Minute {
		t.Fatalf("startupTimeout(60) = %s, want 1m", got)
	}
}

func TestResolveAuthMissingKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRABBOX_WANDB_API_KEY", "")
	t.Setenv("WANDB_API_KEY", "")
	t.Setenv("WANDB_ENTITY_NAME", "team")
	_, err := resolveAuth(Config{})
	if err == nil || !strings.Contains(err.Error(), "W&B API key") {
		t.Fatalf("err = %v, want missing-key error", err)
	}
}

func TestResolveAuthRequiresEntity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRABBOX_WANDB_API_KEY", "crabbox-key")
	t.Setenv("WANDB_ENTITY_NAME", "")
	_, err := resolveAuth(Config{})
	if err == nil || !strings.Contains(err.Error(), "WANDB_ENTITY_NAME") {
		t.Fatalf("err = %v, want missing entity error", err)
	}
}

type stopGatewayClient struct {
	sandboxv1.GatewayServiceClient
	resp *sandboxv1.StopSandboxResponse
	err  error
}

func (f stopGatewayClient) Stop(context.Context, *sandboxv1.StopSandboxRequest, ...grpc.CallOption) (*sandboxv1.StopSandboxResponse, error) {
	return f.resp, f.err
}

func TestWandbStopChecksResponseSuccess(t *testing.T) {
	client := &wandbClient{gw: stopGatewayClient{resp: &sandboxv1.StopSandboxResponse{
		Success:      false,
		ErrorMessage: "quota cleanup blocked",
	}}}
	err := client.Stop(context.Background(), "sb-123", 10, false)
	if err == nil || !strings.Contains(err.Error(), "quota cleanup blocked") {
		t.Fatalf("err = %v, want stop failure", err)
	}
}

func TestWandbStopAllowsSuccessfulResponse(t *testing.T) {
	client := &wandbClient{gw: stopGatewayClient{resp: &sandboxv1.StopSandboxResponse{Success: true}}}
	if err := client.Stop(context.Background(), "sb-123", 10, false); err != nil {
		t.Fatalf("Stop err: %v", err)
	}
}

type execGatewayClient struct {
	sandboxv1.GatewayServiceClient
	resp *sandboxv1.ExecSandboxResponse
	err  error
}

func (f execGatewayClient) Exec(context.Context, *sandboxv1.ExecSandboxRequest, ...grpc.CallOption) (*sandboxv1.ExecSandboxResponse, error) {
	return f.resp, f.err
}

func TestWandbExecMapsNegativeExitCode(t *testing.T) {
	client := &wandbClient{gw: execGatewayClient{resp: &sandboxv1.ExecSandboxResponse{
		Result: &sandboxv1.ExecResponse{ExitCode: -1, Stderr: []byte("execution failed")},
	}}}
	var stderr strings.Builder
	exitCode, err := client.Exec(context.Background(), wandbExecRequest{
		SandboxID: "sb-123",
		Command:   []string{"echo"},
		Stderr:    &stderr,
	})
	if err == nil || !strings.Contains(err.Error(), "exit_code=-1") {
		t.Fatalf("err = %v, want negative sentinel error", err)
	}
	if exitCode != 1 {
		t.Fatalf("exitCode = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "execution failed") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
