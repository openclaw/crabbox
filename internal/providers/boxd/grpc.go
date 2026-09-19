package boxd

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/boxd/boxdapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const defaultGRPCTarget = "boxd.sh:9443"

// apiClient talks to the boxd public gRPC API over TLS only. There is no
// plaintext dial, no fallback on a handshake failure, and no automatic write
// retry; per-call metadata carries a short-lived JWT from the API-key
// exchange, never the key itself.
type apiClient struct {
	api  boxdapi.BoxdApiClient
	auth *authSession
	org  string
}

type machine struct {
	ID           string
	Name         string
	Status       string
	PublicIP     string
	Isolated     bool
	SharedOrg    string
	BillingOrg   string
	BillingOrgID string
}

type forward struct {
	PublicPort int
	VMPort     int
	Protocol   string
}

type grpcStatusError struct{ Code codes.Code }

func (e *grpcStatusError) Error() string {
	if e.Code == codes.Unauthenticated || e.Code == codes.PermissionDenied {
		return fmt.Sprintf("boxd gRPC API returned %s; check that CRABBOX_BOXD_API_KEY / BOXD_API_KEY is a valid bxd_ key for the configured organization", e.Code)
	}
	return fmt.Sprintf("boxd gRPC API returned %s", e.Code)
}

// grpcTarget validates the TLS gRPC endpoint as a bare host:port.
func grpcTarget(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = defaultGRPCTarget
	}
	if strings.ContainsAny(raw, "/?#@ \t\n") || strings.Contains(raw, "://") {
		return "", core.Exit(2, "boxd.grpcUrl must be a bare host:port for the TLS gRPC endpoint (default boxd.sh:9443)")
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil || host == "" {
		return "", core.Exit(2, "boxd.grpcUrl must be a bare host:port for the TLS gRPC endpoint (default boxd.sh:9443)")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", core.Exit(2, "boxd.grpcUrl has an invalid port")
	}
	return net.JoinHostPort(strings.ToLower(host), port), nil
}

func validateMachineID(id string) error {
	if len(id) == 0 || len(id) > 128 {
		return core.Exit(2, "invalid boxd machine ID")
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return core.Exit(2, "invalid boxd machine ID")
		}
	}
	return nil
}

func newAPIClient(cfg core.Config, rt core.Runtime, key string) (*apiClient, error) {
	target, err := grpcTarget(cfg.Boxd.GRPCURL)
	if err != nil {
		return nil, err
	}
	auth, err := newAuthSession(cfg, rt, key)
	if err != nil {
		return nil, err
	}
	// The injected runtime HTTP client supplies the trust anchors for both the
	// HTTPS exchange and the gRPC dial (tests pin a local CA through it);
	// production uses the operating system trust store.
	var tlsConfig *tls.Config
	if rt.HTTP != nil {
		if transport, ok := rt.HTTP.Transport.(*http.Transport); ok && transport != nil && transport.TLSClientConfig != nil {
			tlsConfig = transport.TLSClientConfig.Clone()
		}
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		return nil, core.Exit(5, "boxd gRPC endpoint is invalid")
	}
	return &apiClient{api: boxdapi.NewBoxdApiClient(conn), auth: auth, org: cfg.Boxd.Org}, nil
}

// authed attaches the bearer JWT and a bounded deadline to one call.
func (c *apiClient) authed(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	jwt, err := c.auth.bearer(ctx)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+jwt), cancel, nil
}

// rpcError sanitizes a transport failure. Vendor status messages are
// withheld from diagnostics; only the status code is reported. A
// cancellation can surface as a stream status before the local context
// registers as expired, so those codes report as the context error.
func rpcError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	switch status.Code(err) {
	case codes.DeadlineExceeded:
		return context.DeadlineExceeded
	case codes.Canceled:
		return context.Canceled
	}
	if s, ok := status.FromError(err); ok {
		return &grpcStatusError{Code: s.Code()}
	}
	return core.Exit(5, "boxd gRPC request failed; details withheld")
}

func (c *apiClient) whoami(ctx context.Context) (string, error) {
	ctx, cancel, err := c.authed(ctx, 30*time.Second)
	if err != nil {
		return "", err
	}
	defer cancel()
	resp, err := c.api.Whoami(ctx, &boxdapi.WhoamiRequest{})
	if err != nil {
		return "", rpcError(ctx, err)
	}
	if resp.GetUserId() == "" {
		return "", core.Exit(5, "boxd identity response has no user ID")
	}
	return resp.GetUserId(), nil
}

// getVM reads one machine by its immutable ID. NOT_FOUND reports absence;
// a destroyed machine remains readable as a tombstone with status
// "destroyed", which is definitive destruction proof for exactly this ID.
func (c *apiClient) getVM(ctx context.Context, id string) (machine, bool, error) {
	if err := validateMachineID(id); err != nil {
		return machine{}, false, err
	}
	ctx, cancel, err := c.authed(ctx, 30*time.Second)
	if err != nil {
		return machine{}, false, err
	}
	defer cancel()
	resp, err := c.api.GetVm(ctx, &boxdapi.GetVmRequest{VmId: id})
	if err != nil {
		if ctx.Err() == nil && status.Code(err) == codes.NotFound {
			return machine{}, false, nil
		}
		return machine{}, false, rpcError(ctx, err)
	}
	if resp.GetVmId() != id {
		return machine{}, false, core.Exit(5, "boxd returned a machine whose immutable ID does not match the request")
	}
	return machine{
		ID:           resp.GetVmId(),
		Name:         resp.GetName(),
		Status:       resp.GetStatus(),
		PublicIP:     resp.GetPublicIp(),
		Isolated:     resp.GetIsolated(),
		SharedOrg:    resp.GetOrg(),
		BillingOrg:   resp.GetBillingOrg(),
		BillingOrgID: resp.GetBillingOrgId(),
	}, true, nil
}

func (c *apiClient) create(ctx context.Context, name string) (machine, error) {
	ctx, cancel, err := c.authed(ctx, 30*time.Second)
	if err != nil {
		return machine{}, err
	}
	defer cancel()
	resp, err := c.api.CreateVm(ctx, &boxdapi.CreateVmRequest{Name: name, Org: c.org, Isolated: true})
	if err != nil {
		return machine{}, rpcError(ctx, err)
	}
	// The create response proves neither isolation nor sharing/billing
	// context; independent GetVm reads do, before any guest access.
	return machine{ID: resp.GetVmId(), Name: resp.GetName(), Status: resp.GetStatus(), PublicIP: resp.GetPublicIp()}, nil
}

func (c *apiClient) action(ctx context.Context, id, action string) error {
	if err := validateMachineID(id); err != nil {
		return err
	}
	ctx, cancel, err := c.authed(ctx, 30*time.Second)
	if err != nil {
		return err
	}
	defer cancel()
	switch action {
	case "start":
		_, err = c.api.StartVm(ctx, &boxdapi.StartVmRequest{VmId: id})
	case "stop":
		_, err = c.api.StopVm(ctx, &boxdapi.StopVmRequest{VmId: id})
	case "destroy":
		_, err = c.api.DestroyVm(ctx, &boxdapi.DestroyVmRequest{VmId: id})
	default:
		cancel()
		return core.Exit(2, "invalid boxd machine action")
	}
	if err != nil {
		return rpcError(ctx, err)
	}
	return nil
}

func (c *apiClient) exposeSSH(ctx context.Context, id string) (forward, error) {
	if err := validateMachineID(id); err != nil {
		return forward{}, err
	}
	ctx, cancel, err := c.authed(ctx, 30*time.Second)
	if err != nil {
		return forward{}, err
	}
	defer cancel()
	resp, err := c.api.ExposePort(ctx, &boxdapi.ExposePortRequest{Vm: id, VmPort: 2222, Protocol: "tcp"})
	if err != nil {
		return forward{}, rpcError(ctx, err)
	}
	// The request addresses the immutable ID; the response echoes the machine
	// it actually landed on, so a name-resolved mismatch fails closed.
	if resp.GetVmId() != id || resp.GetVmPort() != 2222 || resp.GetProtocol() != "tcp" ||
		resp.GetPublicPort() < 1 || resp.GetPublicPort() > 65535 {
		return forward{}, core.Exit(5, "boxd returned an invalid SSH port forward")
	}
	return forward{PublicPort: int(resp.GetPublicPort()), VMPort: int(resp.GetVmPort()), Protocol: resp.GetProtocol()}, nil
}
