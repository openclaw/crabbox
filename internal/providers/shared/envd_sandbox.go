package shared

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

// E2B-compatible control and envd data contracts. Adapters retain creation
// payloads, credentials, endpoint routing, and process-completion policy.
type EnvdSandboxAPI interface {
	CreateSandbox(context.Context, EnvdSandboxCreateRequest) (EnvdSandbox, error)
	ConnectSandbox(context.Context, string, int) (EnvdSandboxSession, error)
	GetSandbox(context.Context, string) (EnvdSandbox, error)
	ListSandboxes(context.Context, map[string]string) ([]EnvdSandbox, error)
	DeleteSandbox(context.Context, string) error
	UploadFile(context.Context, EnvdSandboxSession, string, io.Reader) error
	StartProcess(context.Context, EnvdSandboxSession, EnvdSandboxProcessRequest) (int, error)
}

type EnvdSandboxCreateRequest struct {
	TemplateID          string
	TimeoutSeconds      int
	Metadata            map[string]string
	AllowInternetAccess bool
}

type EnvdSandbox struct {
	TemplateID      string            `json:"templateID"`
	SandboxID       string            `json:"sandboxID"`
	ClientID        string            `json:"clientID"`
	StartedAt       string            `json:"startedAt"`
	EndAt           string            `json:"endAt"`
	EnvdVersion     string            `json:"envdVersion"`
	EnvdAccessToken string            `json:"envdAccessToken"`
	TrafficToken    string            `json:"trafficAccessToken"`
	Alias           string            `json:"alias"`
	Domain          string            `json:"domain"`
	State           string            `json:"state"`
	CPUCount        int               `json:"cpuCount"`
	MemoryMB        int               `json:"memoryMB"`
	DiskSizeMB      int               `json:"diskSizeMB"`
	Metadata        map[string]string `json:"metadata"`
}

type EnvdSandboxSession struct {
	SandboxID       string
	EnvdVersion     string
	EnvdAccessToken string
	Domain          string
}

type EnvdSandboxProcessRequest struct {
	Command string
	CWD     string
	Env     map[string]string
	User    string
	Timeout time.Duration
	Stdout  io.Writer
	Stderr  io.Writer
}

// EnvdSandboxControl implements the compatible control-plane operations.
// Request retains adapter-owned authentication, transport, and error decoding.
// Creation payloads and envd session routing remain in each provider.
type EnvdSandboxControl struct {
	Request func(context.Context, string, string, url.Values, any, any) (http.Header, error)
}

func (c EnvdSandboxControl) CreateSandbox(ctx context.Context, body any, metadata map[string]string) (EnvdSandbox, error) {
	var sandbox EnvdSandbox
	if _, err := c.Request(ctx, http.MethodPost, "/sandboxes", nil, body, &sandbox); err != nil {
		return EnvdSandbox{}, err
	}
	if sandbox.Metadata == nil {
		sandbox.Metadata = metadata
	}
	if sandbox.State == "" {
		sandbox.State = "running"
	}
	return sandbox, nil
}

func (c EnvdSandboxControl) ConnectSandbox(ctx context.Context, id string, timeoutSeconds int) (EnvdSandbox, error) {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 300
	}
	var sandbox EnvdSandbox
	if _, err := c.Request(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/connect", nil, map[string]any{"timeout": timeoutSeconds}, &sandbox); err != nil {
		return EnvdSandbox{}, err
	}
	if ValidateResourceID(id, sandbox.SandboxID) != nil {
		return EnvdSandbox{}, errors.New("connect sandbox returned a different or missing sandbox ID")
	}
	return sandbox, nil
}

func (c EnvdSandboxControl) GetSandbox(ctx context.Context, id string) (EnvdSandbox, error) {
	var sandbox EnvdSandbox
	if _, err := c.Request(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil, nil, &sandbox); err != nil {
		return EnvdSandbox{}, err
	}
	if ValidateResourceID(id, sandbox.SandboxID) != nil {
		return EnvdSandbox{}, errors.New("get sandbox returned a different or missing sandbox ID")
	}
	if sandbox.Metadata == nil {
		sandbox.Metadata = map[string]string{}
	}
	return sandbox, nil
}

func (c EnvdSandboxControl) ListSandboxes(ctx context.Context, metadata map[string]string) ([]EnvdSandbox, error) {
	var all []EnvdSandbox
	nextToken := ""
	for {
		query := url.Values{}
		query.Set("limit", "100")
		query.Set("state", "running,paused")
		if nextToken != "" {
			query.Set("nextToken", nextToken)
		}
		if len(metadata) > 0 {
			values := url.Values{}
			for key, value := range metadata {
				values.Set(key, value)
			}
			query.Set("metadata", values.Encode())
		}
		var page []EnvdSandbox
		headers, err := c.Request(ctx, http.MethodGet, "/v2/sandboxes", query, nil, &page)
		if err != nil {
			return nil, err
		}
		for i := range page {
			if page[i].Metadata == nil {
				page[i].Metadata = map[string]string{}
			}
		}
		all = append(all, page...)
		nextToken = headers.Get("x-next-token")
		if nextToken == "" {
			return all, nil
		}
	}
}

func (c EnvdSandboxControl) DeleteSandbox(ctx context.Context, id string) error {
	_, err := c.Request(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id), nil, nil, nil)
	return err
}
