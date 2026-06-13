package codesandbox

import "context"

type SandboxSummary struct {
	ID      string   `json:"id"`
	Title   string   `json:"title,omitempty"`
	Privacy string   `json:"privacy,omitempty"`
	Tags    []string `json:"tags,omitempty"`
}

type ListSandboxesRequest struct {
	Limit int
}

type ListSandboxesResult struct {
	Sandboxes  []SandboxSummary
	TotalCount int
}

type SandboxLister interface {
	ListSandboxes(ctx context.Context, req ListSandboxesRequest) (ListSandboxesResult, error)
}

type codeSandboxClient struct {
	cfg    CodeSandboxConfig
	rt     Runtime
	bridge *SDKBridge
	token  string
}

var newCodeSandboxClient = func(cfg Config, rt Runtime) (SandboxLister, error) {
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

type missingAuthError struct{}

func (missingAuthError) Error() string {
	return "codesandbox auth missing: set CRABBOX_CODESANDBOX_API_KEY or CSB_API_KEY"
}
