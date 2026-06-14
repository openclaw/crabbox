package codesandbox

import (
	"context"
	"encoding/base64"
	"io"
)

type SandboxSummary struct {
	ID      string   `json:"id"`
	Title   string   `json:"title,omitempty"`
	Privacy string   `json:"privacy,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	State   string   `json:"state,omitempty"`
	URL     string   `json:"url,omitempty"`
}

type ListSandboxesRequest struct {
	Limit int
}

type ListSandboxesResult struct {
	Sandboxes  []SandboxSummary
	TotalCount int
}

type CreateSandboxRequest struct {
	Title                  string
	Tags                   []string
	TemplateID             string
	Privacy                string
	VMTier                 string
	HibernationTimeoutSecs int
	AutomaticWakeupHTTP    bool
	AutomaticWakeupWS      bool
}

type CommandRequest struct {
	Command []string
	Cwd     string
	Env     map[string]string
	Timeout int
}

type CommandResult struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}

type PortInfo struct {
	Port int    `json:"port"`
	Host string `json:"host"`
	URL  string `json:"url,omitempty"`
}

type codeSandboxAPI interface {
	ListSandboxes(ctx context.Context, req ListSandboxesRequest) (ListSandboxesResult, error)
	CreateSandbox(ctx context.Context, req CreateSandboxRequest) (SandboxSummary, error)
	GetSandbox(ctx context.Context, id string) (SandboxSummary, error)
	DeleteSandbox(ctx context.Context, id string) error
	HibernateSandbox(ctx context.Context, id string) error
	ResumeSandbox(ctx context.Context, id string) (SandboxSummary, error)
	RunCommand(ctx context.Context, sandboxID string, req CommandRequest) (CommandResult, error)
	UploadFile(ctx context.Context, sandboxID, remotePath string, r io.Reader) error
	ListPorts(ctx context.Context, sandboxID string) ([]PortInfo, error)
	WaitForPortURL(ctx context.Context, sandboxID string, port int) (PortInfo, error)
}

type codeSandboxClient struct {
	cfg    CodeSandboxConfig
	rt     Runtime
	bridge *SDKBridge
	token  string
}

var newCodeSandboxClient = func(cfg Config, rt Runtime) (codeSandboxAPI, error) {
	token, _, ok := authFromEnv()
	if !ok {
		return nil, missingAuthError{}
	}
	return &codeSandboxClient{
		cfg:    cfg.CodeSandbox,
		rt:     rt,
		bridge: NewSDKBridge(cfg.CodeSandbox, rt),
		token:  token,
	}, nil
}

func (c *codeSandboxClient) ListSandboxes(ctx context.Context, req ListSandboxesRequest) (ListSandboxesResult, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = doctorListLimit(c.cfg)
	}
	resp, err := c.bridge.RoundTrip(ctx, c.token, BridgeRequest{
		Operation: "list_sandboxes",
		Limit:     limit,
	})
	if err != nil {
		return ListSandboxesResult{}, err
	}
	return ListSandboxesResult{Sandboxes: resp.Sandboxes, TotalCount: resp.TotalCount}, nil
}

func (c *codeSandboxClient) CreateSandbox(ctx context.Context, req CreateSandboxRequest) (SandboxSummary, error) {
	resp, err := c.bridge.RoundTrip(ctx, c.token, BridgeRequest{
		Operation:              "create_sandbox",
		Title:                  req.Title,
		Tags:                   req.Tags,
		TemplateID:             req.TemplateID,
		Privacy:                req.Privacy,
		VMTier:                 req.VMTier,
		HibernationTimeoutSecs: req.HibernationTimeoutSecs,
		AutomaticWakeupHTTP:    req.AutomaticWakeupHTTP,
		AutomaticWakeupWS:      req.AutomaticWakeupWS,
	})
	if err != nil {
		return SandboxSummary{}, err
	}
	return resp.Sandbox, nil
}

func (c *codeSandboxClient) GetSandbox(ctx context.Context, id string) (SandboxSummary, error) {
	resp, err := c.bridge.RoundTrip(ctx, c.token, BridgeRequest{Operation: "get_sandbox", SandboxID: id})
	if err != nil {
		return SandboxSummary{}, err
	}
	return resp.Sandbox, nil
}

func (c *codeSandboxClient) DeleteSandbox(ctx context.Context, id string) error {
	_, err := c.bridge.RoundTrip(ctx, c.token, BridgeRequest{Operation: "delete_sandbox", SandboxID: id})
	return err
}

func (c *codeSandboxClient) HibernateSandbox(ctx context.Context, id string) error {
	_, err := c.bridge.RoundTrip(ctx, c.token, BridgeRequest{Operation: "hibernate_sandbox", SandboxID: id})
	return err
}

func (c *codeSandboxClient) ResumeSandbox(ctx context.Context, id string) (SandboxSummary, error) {
	resp, err := c.bridge.RoundTrip(ctx, c.token, BridgeRequest{Operation: "resume_sandbox", SandboxID: id})
	if err != nil {
		return SandboxSummary{}, err
	}
	return resp.Sandbox, nil
}

func (c *codeSandboxClient) RunCommand(ctx context.Context, sandboxID string, req CommandRequest) (CommandResult, error) {
	resp, err := c.bridge.RoundTrip(ctx, c.token, BridgeRequest{
		Operation: "run_command",
		SandboxID: sandboxID,
		Command:   req.Command,
		Cwd:       req.Cwd,
		Env:       req.Env,
		Timeout:   req.Timeout,
	})
	if err != nil {
		return CommandResult{}, err
	}
	return resp.Command, nil
}

func (c *codeSandboxClient) UploadFile(ctx context.Context, sandboxID, remotePath string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	_, err = c.bridge.RoundTrip(ctx, c.token, BridgeRequest{
		Operation:     "write_file",
		SandboxID:     sandboxID,
		Path:          remotePath,
		ContentBase64: base64.StdEncoding.EncodeToString(data),
		Encoding:      "base64",
	})
	return err
}

func (c *codeSandboxClient) ListPorts(ctx context.Context, sandboxID string) ([]PortInfo, error) {
	resp, err := c.bridge.RoundTrip(ctx, c.token, BridgeRequest{Operation: "list_ports", SandboxID: sandboxID})
	if err != nil {
		return nil, err
	}
	return resp.Ports, nil
}

func (c *codeSandboxClient) WaitForPortURL(ctx context.Context, sandboxID string, port int) (PortInfo, error) {
	resp, err := c.bridge.RoundTrip(ctx, c.token, BridgeRequest{
		Operation: "get_port_url",
		SandboxID: sandboxID,
		Port:      port,
	})
	if err != nil {
		return PortInfo{}, err
	}
	return resp.Port, nil
}

type missingAuthError struct{}

func (missingAuthError) Error() string {
	return "codesandbox auth missing: set CRABBOX_CODESANDBOX_API_KEY or CSB_API_KEY"
}
