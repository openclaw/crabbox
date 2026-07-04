package wandb

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	sandboxv1 "github.com/openclaw/crabbox/internal/providers/wandb/gen/coreweave/sandbox/v1beta2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// defaultEndpoint is the current CoreWeave Sandboxes API endpoint. Overridable
// via CWSANDBOX_BASE_URL so the devops team can coordinate region or staging
// cutovers without a code change.
const defaultEndpoint = "api.cwsandbox.com:443"
const defaultStartupTimeout = 5 * time.Minute

// idleCommand keeps the sandbox container alive after Start so callers can
// invoke Exec into it on demand. Mirrors what cwsandbox.Sandbox.run() does
// when no main_command is supplied.
var (
	idleCommand = "/bin/sh"
	idleArgs    = []string{"-c", "trap : TERM INT; sleep infinity & wait"}
)

// wandbAPI is the test seam used by backend.go. The gRPC client below is the
// only production implementation; tests construct a fake of this interface.
type wandbAPI interface {
	Version(ctx context.Context) (string, error)
	Acquire(ctx context.Context, req wandbAcquireRequest) (wandbSandbox, error)
	Exec(ctx context.Context, req wandbExecRequest) (int, error)
	Stop(ctx context.Context, id string, gracefulSeconds int, missingOK bool) error
	List(ctx context.Context, tags []string, status string) ([]wandbSandbox, error)
	Status(ctx context.Context, id string) (wandbSandbox, error)
}

type wandbSandbox struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

type wandbAcquireRequest struct {
	Image           string
	MaxLifetimeSecs int
	Tags            []string
	EnvironmentVars map[string]string
}

type wandbExecRequest struct {
	SandboxID string
	Command   []string
	Timeout   int
	Stdout    io.Writer
	Stderr    io.Writer
}

// normalizeStatus converts the proto enum into Crabbox's status vocabulary.
// W&B reports COMPLETED for a stopped idle sandbox; map it to "stopped" so
// status --wait recognizes it as terminal.
func normalizeStatus(s sandboxv1.SandboxStatus) string {
	if s == sandboxv1.SandboxStatus_SANDBOX_STATUS_COMPLETED {
		return "stopped"
	}
	return strings.ToLower(strings.TrimPrefix(s.String(), "SANDBOX_STATUS_"))
}

// wandbAPIError carries a typed view of a gRPC failure. The public field set
// is kept stable so backend.go's branching logic does not need to change.
type wandbAPIError struct {
	ExitCode int
	Stderr   string
	Code     codes.Code
}

func (e *wandbAPIError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("wandb sandbox API error code=%s", e.Code)
	}
	return fmt.Sprintf("wandb sandbox API error code=%s: %s", e.Code, e.Stderr)
}

// As bridges wandbAPIError into cli.ExitError so cmd/crabbox's AsExitError
// path picks up the mapped sysexit code (77/69/124/…) as the process exit.
// Without this, mapRPCError's exit codes were inert — main printed and exited 1.
func (e *wandbAPIError) As(target any) bool {
	if t, ok := target.(*ExitError); ok {
		*t = ExitError{Code: e.ExitCode, Message: e.Error()}
		return true
	}
	return false
}

// Auth carries the gRPC metadata headers that CoreWeave Sandboxes accepts.
// The W&B API key (x-wandb-api-key) plus entity (x-entity-id) is the seamless
// onramp for users who already ran `wandb login` — no new account, no new
// credential.
type Auth struct {
	APIKey  string
	Entity  string
	Project string
}

// wandbClient is the gRPC-backed implementation of wandbAPI.
//
// One ClientConn is held per backend operation; HTTP/2 multiplexes the calls
// inside that operation, and the backend closes the connection before returning
// to avoid leaking sockets in long-lived Crabbox processes.
type wandbClient struct {
	conn *grpc.ClientConn
	gw   sandboxv1.GatewayServiceClient
}

func newWandbClient(cfg Config, _ Runtime) (wandbAPI, error) {
	auth, err := resolveAuth(cfg)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimSpace(os.Getenv("CWSANDBOX_BASE_URL"))
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	endpoint, creds, err := resolveEndpoint(endpoint)
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(128<<20),
			grpc.MaxCallSendMsgSize(128<<20),
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.WithUnaryInterceptor(authUnary(auth)),
		grpc.WithUserAgent("crabbox/wandb"),
	)
	if err != nil {
		return nil, fmt.Errorf("dial cwsandbox gateway %s: %w", endpoint, err)
	}
	return &wandbClient{conn: conn, gw: sandboxv1.NewGatewayServiceClient(conn)}, nil
}

// wandbProviderScope binds a local claim to the exact remote namespace without
// retaining the API key. The client validates that entity is present before a
// production operation reaches ownership checks.
func wandbProviderScope() (string, error) {
	endpoint := strings.TrimSpace(os.Getenv("CWSANDBOX_BASE_URL"))
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	resolved, _, err := resolveEndpoint(endpoint)
	if err != nil {
		return "", err
	}
	return "endpoint:" + url.QueryEscape(resolved) +
		"|entity:" + url.QueryEscape(strings.TrimSpace(os.Getenv("WANDB_ENTITY_NAME"))) +
		"|project:" + url.QueryEscape(strings.TrimSpace(os.Getenv("WANDB_PROJECT"))), nil
}

func (c *wandbClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// resolveAuth walks the documented credential precedence:
//
//  1. CRABBOX_WANDB_API_KEY   — explicit per-run override, CI-friendly
//  2. cfg.Wandb.APIKey        — config-file value (yaml: wandb.apiKey)
//  3. WANDB_API_KEY           — canonical env var
//  4. ~/.netrc                — what `wandb login` writes (machine api.wandb.ai)
//
// WANDB_ENTITY_NAME is required for W&B-authenticated sandboxes; WANDB_PROJECT
// is optional.
func resolveAuth(cfg Config) (Auth, error) {
	key := strings.TrimSpace(os.Getenv("CRABBOX_WANDB_API_KEY"))
	if key == "" {
		key = strings.TrimSpace(cfg.Wandb.APIKey)
	}
	if key == "" {
		key = strings.TrimSpace(os.Getenv("WANDB_API_KEY"))
	}
	if key == "" {
		key = readNetrcWandbKey()
	}
	if key == "" {
		return Auth{}, exit(2, "provider=%s requires a W&B API key (run `wandb login`, set CRABBOX_WANDB_API_KEY, or add `wandb.apiKey` to your crabbox config)", providerName)
	}
	entity := strings.TrimSpace(os.Getenv("WANDB_ENTITY_NAME"))
	if entity == "" {
		return Auth{}, exit(2, "provider=%s requires WANDB_ENTITY_NAME when using W&B credentials", providerName)
	}
	return Auth{
		APIKey:  key,
		Entity:  entity,
		Project: strings.TrimSpace(os.Getenv("WANDB_PROJECT")),
	}, nil
}

// readNetrcWandbKey returns the password for `machine api.wandb.ai` (or the
// `.com` variant) from ~/.netrc — which is exactly what `wandb login` writes.
// Minimal whitespace-tokenised parser; netrc is a flat key/value stream so a
// full grammar is unnecessary here.
func readNetrcWandbKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".netrc"))
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(data))
	matched := false
	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "machine":
			if i+1 < len(fields) {
				host := fields[i+1]
				matched = host == "api.wandb.ai" || host == "api.wandb.com"
				i++
			}
		case "default":
			matched = false
		case "password":
			if matched && i+1 < len(fields) {
				return strings.TrimSpace(fields[i+1])
			}
		}
	}
	return ""
}

func authUnary(a Auth) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, rep any, cc *grpc.ClientConn, inv grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		md := []string{"x-wandb-api-key", a.APIKey}
		if a.Entity != "" {
			md = append(md, "x-entity-id", a.Entity)
		}
		if a.Project != "" {
			md = append(md, "x-project-name", a.Project)
		}
		return inv(metadata.AppendToOutgoingContext(ctx, md...), method, req, rep, cc, opts...)
	}
}

func resolveEndpoint(raw string) (string, credentials.TransportCredentials, error) {
	endpoint := strings.TrimSpace(raw)
	if endpoint == "" {
		return "", nil, fmt.Errorf("cwsandbox endpoint is empty")
	}
	lower := strings.ToLower(endpoint)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return strings.TrimSuffix(endpoint, "/"), credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12}), nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", nil, fmt.Errorf("parse cwsandbox endpoint %q: %w", raw, err)
	}
	if parsed.Host == "" {
		return "", nil, fmt.Errorf("cwsandbox endpoint %q is missing host", raw)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", nil, fmt.Errorf("cwsandbox endpoint %q must not include a path", raw)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", nil, fmt.Errorf("cwsandbox endpoint %q must not include query or fragment", raw)
	}
	switch parsed.Scheme {
	case "https":
		return parsed.Host, credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12}), nil
	case "http":
		if !isLoopbackHost(parsed.Hostname()) {
			return "", nil, fmt.Errorf("cwsandbox endpoint %q uses plaintext http for non-loopback host %q", raw, parsed.Hostname())
		}
		return parsed.Host, insecure.NewCredentials(), nil
	default:
		return "", nil, fmt.Errorf("cwsandbox endpoint %q has unsupported scheme %q", raw, parsed.Scheme)
	}
}

func isLoopbackHost(host string) bool {
	normalized := strings.ToLower(strings.TrimSpace(host))
	if normalized == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(normalized, "[]"))
	return ip != nil && ip.IsLoopback()
}

// Version round-trips the cheapest authenticated call (List with page_size=1)
// to verify endpoint reachability + credentials, and returns the proto package
// version the client is wired against.
func (c *wandbClient) Version(ctx context.Context) (string, error) {
	if _, err := c.gw.List(ctx, &sandboxv1.ListSandboxesRequest{PageSize: 1}); err != nil {
		return "", mapRPCError(err, "version probe")
	}
	return "coreweave.sandbox.v1beta2", nil
}

// Acquire starts a sandbox running an idle keep-alive command, then polls Get
// with the upstream-matched backoff (200ms → 1.5x → 2s cap) until the sandbox
// reaches RUNNING. On failure we issue a best-effort Stop so we don't leak the
// pod (and the user doesn't get billed for orphaned state).
func (c *wandbClient) Acquire(ctx context.Context, req wandbAcquireRequest) (wandbSandbox, error) {
	if req.Image == "" {
		return wandbSandbox{}, fmt.Errorf("wandb acquire: container_image is required")
	}
	started, err := c.gw.Start(ctx, &sandboxv1.StartSandboxRequest{
		Command:              idleCommand,
		Args:                 idleArgs,
		ContainerImage:       req.Image,
		Tags:                 req.Tags,
		EnvironmentVariables: req.EnvironmentVars,
		MaxLifetimeSeconds:   int32(req.MaxLifetimeSecs),
	})
	if err != nil {
		return wandbSandbox{}, mapRPCError(err, "Start")
	}
	startupCtx, cancel := context.WithTimeout(ctx, startupTimeout(req.MaxLifetimeSecs))
	defer cancel()
	sb, perr := c.pollUntilRunning(startupCtx, started.SandboxId)
	if perr != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), wandbStopTimeout)
		defer cancel()
		_, _ = c.gw.Stop(stopCtx, &sandboxv1.StopSandboxRequest{SandboxId: started.SandboxId})
		return wandbSandbox{}, perr
	}
	return sb, nil
}

func (c *wandbClient) pollUntilRunning(ctx context.Context, id string) (wandbSandbox, error) {
	interval := 200 * time.Millisecond
	const cap = 2 * time.Second
	for {
		select {
		case <-ctx.Done():
			return wandbSandbox{}, ctx.Err()
		default:
		}
		resp, err := c.gw.Get(ctx, &sandboxv1.GetSandboxRequest{SandboxId: id})
		if err != nil {
			return wandbSandbox{}, mapRPCError(err, "Get(poll)")
		}
		switch resp.SandboxStatus {
		case sandboxv1.SandboxStatus_SANDBOX_STATUS_RUNNING:
			return wandbSandbox{
				ID:        id,
				Status:    normalizeStatus(resp.SandboxStatus),
				CreatedAt: formatTimestamp(resp.StartedAtTime),
			}, nil
		case sandboxv1.SandboxStatus_SANDBOX_STATUS_FAILED:
			return wandbSandbox{}, fmt.Errorf("sandbox %s failed during startup", id)
		case sandboxv1.SandboxStatus_SANDBOX_STATUS_COMPLETED,
			sandboxv1.SandboxStatus_SANDBOX_STATUS_TERMINATED,
			sandboxv1.SandboxStatus_SANDBOX_STATUS_TERMINATING:
			return wandbSandbox{}, fmt.Errorf("sandbox %s ended before reaching RUNNING (status=%s)", id, resp.SandboxStatus)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return wandbSandbox{}, ctx.Err()
		case <-timer.C:
		}
		if interval < cap {
			interval = interval * 3 / 2
			if interval > cap {
				interval = cap
			}
		}
	}
}

func startupTimeout(maxLifetimeSecs int) time.Duration {
	timeout := defaultStartupTimeout
	if maxLifetimeSecs > 0 {
		lifetime := time.Duration(maxLifetimeSecs) * time.Second
		if lifetime < timeout {
			timeout = lifetime
		}
	}
	return timeout
}

// Exec runs a single command in the sandbox via the unary Exec RPC. Streamed
// stdout/stderr (PTY interactive) is the StreamExec follow-up that we defer
// behind v1 of this provider.
func (c *wandbClient) Exec(ctx context.Context, req wandbExecRequest) (int, error) {
	if req.SandboxID == "" {
		return 0, fmt.Errorf("wandb exec: sandbox id is required")
	}
	if len(req.Command) == 0 {
		return 0, fmt.Errorf("wandb exec: command is required")
	}
	grpcReq := &sandboxv1.ExecSandboxRequest{
		SandboxId: req.SandboxID,
		Command:   req.Command,
	}
	if req.Timeout > 0 {
		grpcReq.MaxTimeoutSeconds = int32(req.Timeout)
	}
	resp, err := c.gw.Exec(ctx, grpcReq)
	if err != nil {
		return 0, mapRPCError(err, "Exec")
	}
	if resp.Result == nil {
		return 0, fmt.Errorf("wandb exec: empty response")
	}
	if req.Stdout != nil && len(resp.Result.Stdout) > 0 {
		_, _ = req.Stdout.Write(resp.Result.Stdout)
	}
	if req.Stderr != nil && len(resp.Result.Stderr) > 0 {
		_, _ = req.Stderr.Write(resp.Result.Stderr)
	}
	exitCode := int(resp.Result.ExitCode)
	if exitCode < 0 {
		return 1, fmt.Errorf("wandb exec failed with exit_code=%d", exitCode)
	}
	return exitCode, nil
}

func (c *wandbClient) Stop(ctx context.Context, id string, gracefulSeconds int, missingOK bool) error {
	if id == "" {
		return fmt.Errorf("wandb stop: sandbox id is required")
	}
	resp, err := c.gw.Stop(ctx, &sandboxv1.StopSandboxRequest{
		SandboxId:               id,
		GracefulShutdownSeconds: int32(gracefulSeconds),
	})
	if err != nil {
		if missingOK && status.Code(err) == codes.NotFound {
			return nil
		}
		return mapRPCError(err, "Stop")
	}
	if resp == nil {
		return fmt.Errorf("wandb stop %s: empty response", id)
	}
	if !resp.GetSuccess() {
		msg := strings.TrimSpace(resp.GetErrorMessage())
		if msg == "" {
			msg = "stop failed"
		}
		return fmt.Errorf("wandb stop %s: %s", id, msg)
	}
	return nil
}

func (c *wandbClient) List(ctx context.Context, tags []string, statusFilter string) ([]wandbSandbox, error) {
	// statusFilter mirrors backend.List(req.All): "" means current sandboxes
	// only (the gateway default), "all" includes terminal ones. Any other
	// value is treated as "" — callers don't pass per-status filters today.
	grpcReq := &sandboxv1.ListSandboxesRequest{
		Tags:           tags,
		PageSize:       100,
		IncludeStopped: strings.EqualFold(statusFilter, "all"),
	}
	out := []wandbSandbox{}
	for {
		resp, err := c.gw.List(ctx, grpcReq)
		if err != nil {
			return nil, mapRPCError(err, "List")
		}
		for _, sb := range resp.Sandboxes {
			out = append(out, wandbSandbox{
				ID:        sb.SandboxId,
				Status:    normalizeStatus(sb.SandboxStatus),
				CreatedAt: formatTimestamp(sb.StartedAtTime),
			})
		}
		if resp.NextPageToken == "" {
			break
		}
		grpcReq.PageToken = resp.NextPageToken
	}
	return out, nil
}

func (c *wandbClient) Status(ctx context.Context, id string) (wandbSandbox, error) {
	resp, err := c.gw.Get(ctx, &sandboxv1.GetSandboxRequest{SandboxId: id})
	if err != nil {
		return wandbSandbox{}, mapRPCError(err, "Get")
	}
	return wandbSandbox{
		ID:        id,
		Status:    normalizeStatus(resp.SandboxStatus),
		CreatedAt: formatTimestamp(resp.StartedAtTime),
	}, nil
}

// formatTimestamp safely renders a protobuf Timestamp as RFC3339 UTC; nil
// timestamps (the server may omit StartedAtTime for sandboxes that never
// reached RUNNING) are returned as an empty string.
func formatTimestamp(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return ""
	}
	return ts.AsTime().UTC().Format(time.RFC3339)
}

// mapRPCError converts a gRPC status into the wandbAPIError surface that
// callers (and tests) already expect. ExitCode mapping follows sysexits.h:
// 77 EX_NOPERM, 69 EX_UNAVAILABLE, 124 GNU timeout.
func mapRPCError(err error, op string) error {
	if err == nil {
		return nil
	}
	s, ok := status.FromError(err)
	if !ok {
		return &wandbAPIError{ExitCode: 1, Stderr: fmt.Sprintf("%s: %v", op, err)}
	}
	exit := 1
	switch s.Code() {
	case codes.OK:
		return nil
	case codes.Unauthenticated, codes.PermissionDenied:
		exit = 77
	case codes.NotFound:
		exit = 4
	case codes.DeadlineExceeded:
		exit = 124
	case codes.Unavailable, codes.ResourceExhausted:
		exit = 69
	}
	return &wandbAPIError{
		ExitCode: exit,
		Stderr:   fmt.Sprintf("%s: %s", op, s.Message()),
		Code:     s.Code(),
	}
}
