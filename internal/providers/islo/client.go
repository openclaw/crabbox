package islo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
	"github.com/islo-labs/go-sdk/client"
	"github.com/islo-labs/go-sdk/customauth"
	"github.com/islo-labs/go-sdk/option"
)

type isloAPI interface {
	CreateSandbox(context.Context, *gosdk.SandboxCreate) (*gosdk.SandboxResponse, error)
	GetSandbox(context.Context, string) (*gosdk.SandboxResponse, error)
	PauseSandbox(context.Context, string) (*gosdk.SandboxResponse, error)
	ResumeSandbox(context.Context, string) (*gosdk.SandboxResponse, error)
	ListSandboxes(context.Context) ([]*gosdk.SandboxResponse, error)
	DeleteSandbox(context.Context, string) error
	UploadArchive(context.Context, string, string, io.Reader) error
	ExecStream(context.Context, string, *gosdk.ExecRequest, io.Writer, io.Writer) (int, error)
	CreateShare(ctx context.Context, sandboxName string, port int, ttl time.Duration) (IsloShare, error)
	ListShares(ctx context.Context, sandboxName string) ([]IsloShare, error)
}

// IsloShare describes a per-port public HTTPS share produced by the islo
// `POST /sandboxes/{name}/shares` API. It is the islo-specific shape of the
// generic BridgePeer entry surfaced by the pond bridge plane.
type IsloShare struct {
	ShareID      string    `json:"share_id"`
	URL          string    `json:"url"`
	Port         int       `json:"port"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	ExpiresAtSet bool      `json:"-"`
}

type isloSDKClient struct {
	sdk        *client.Client
	auth       *customauth.Provider
	baseURL    string
	httpClient *http.Client
}

const isloDefaultResponseHeaderTimeout = 30 * time.Second

var isloCleanupTimeout = 15 * time.Second

var newIsloClient = func(cfg Config, rt Runtime) (isloAPI, error) {
	apiKey := strings.TrimSpace(cfg.Islo.APIKey)
	if apiKey == "" {
		return nil, exit(2, "provider=islo requires ISLO_API_KEY")
	}
	baseURL := strings.TrimRight(blank(cfg.Islo.BaseURL, "https://api.islo.dev"), "/")
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = defaultIsloHTTPClient()
	}
	auth := customauth.NewProvider(baseURL, apiKey, 0, httpClient)
	var baseTransport http.RoundTripper
	var timeout time.Duration
	if httpClient != nil {
		baseTransport = httpClient.Transport
		timeout = httpClient.Timeout
	}
	sdkHTTPClient := &http.Client{
		Transport: customauth.NewTransport(baseTransport, auth),
		Timeout:   timeout,
	}
	sdk := client.NewClient(option.WithBaseURL(baseURL), option.WithHTTPClient(sdkHTTPClient))
	return &isloSDKClient{sdk: sdk, auth: auth, baseURL: baseURL, httpClient: httpClient}, nil
}

func isloCleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), isloCleanupTimeout)
}

func defaultIsloHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = isloDefaultResponseHeaderTimeout
	return &http.Client{Transport: transport}
}

func (c *isloSDKClient) CreateSandbox(ctx context.Context, req *gosdk.SandboxCreate) (*gosdk.SandboxResponse, error) {
	return c.sdk.Sandboxes.CreateSandbox(ctx, req)
}

func (c *isloSDKClient) GetSandbox(ctx context.Context, name string) (*gosdk.SandboxResponse, error) {
	return c.sdk.Sandboxes.GetSandbox(ctx, &gosdk.GetSandboxRequest{SandboxName: name})
}

func (c *isloSDKClient) PauseSandbox(ctx context.Context, name string) (*gosdk.SandboxResponse, error) {
	return c.sdk.Sandboxes.PauseSandbox(ctx, &gosdk.PauseSandboxRequest{SandboxName: name})
}

func (c *isloSDKClient) ResumeSandbox(ctx context.Context, name string) (*gosdk.SandboxResponse, error) {
	return c.sdk.Sandboxes.ResumeSandbox(ctx, &gosdk.ResumeSandboxRequest{SandboxName: name})
}

func (c *isloSDKClient) ListSandboxes(ctx context.Context) ([]*gosdk.SandboxResponse, error) {
	limit := 100
	var all []*gosdk.SandboxResponse
	for offset := 0; ; offset += limit {
		page, err := c.sdk.Sandboxes.ListSandboxes(ctx, &gosdk.ListSandboxesRequest{Limit: &limit, Offset: &offset})
		if err != nil {
			return nil, err
		}
		if page == nil {
			return all, nil
		}
		items := page.GetItems()
		all = append(all, items...)
		if len(items) < limit {
			return all, nil
		}
		if total := page.GetTotal(); total > 0 && offset+len(items) >= total {
			return all, nil
		}
	}
}

func (c *isloSDKClient) DeleteSandbox(ctx context.Context, name string) error {
	// The Islo delete endpoint returns an empty body (202/204), which the
	// generated SDK decoder rejects ("expected a response, but the server
	// responded with nothing"). Issue the DELETE directly so an empty success
	// body is handled correctly, and treat an already-gone sandbox (404) as a
	// successful idempotent delete.
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/sandboxes/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	if err := c.authorize(ctx, httpReq); err != nil {
		return err
	}
	httpReq.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Mirror the >=400 failure convention used by the other raw endpoints, with
	// 404 carved out so an already-gone sandbox is an idempotent success.
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("islo delete sandbox %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *isloSDKClient) UploadArchive(ctx context.Context, name, targetPath string, archive io.Reader) error {
	u, err := url.Parse(c.baseURL + "/sandboxes/" + url.PathEscape(name) + "/files-archive")
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("path", targetPath)
	u.RawQuery = q.Encode()
	body, contentType := multipartArchiveBody(archive)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), body)
	if err != nil {
		return err
	}
	token, err := c.auth.Token(ctx)
	if err != nil {
		return fmt.Errorf("islo auth: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", contentType)
	httpReq.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("islo upload archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("islo upload archive %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func multipartArchiveBody(archive io.Reader) (io.Reader, string) {
	writer := multipart.NewWriter(io.Discard)
	boundary := writer.Boundary()
	var prefix strings.Builder
	prefix.WriteString("--")
	prefix.WriteString(boundary)
	prefix.WriteString("\r\n")
	prefix.WriteString("Content-Disposition: form-data; name=\"file\"; filename=\"archive.tar.gz\"\r\n")
	prefix.WriteString("Content-Type: application/gzip\r\n\r\n")
	suffix := "\r\n--" + boundary + "--\r\n"
	return io.MultiReader(strings.NewReader(prefix.String()), archive, strings.NewReader(suffix)), writer.FormDataContentType()
}

type isloCreateShareRequest struct {
	Port       int  `json:"port"`
	TTLSeconds *int `json:"ttl_seconds,omitempty"`
}

type isloShareResponse struct {
	ShareID   string  `json:"share_id"`
	URL       string  `json:"url"`
	Port      int     `json:"port"`
	CreatedAt string  `json:"created_at"`
	ExpiresAt *string `json:"expires_at"`
}

func (c *isloSDKClient) CreateShare(ctx context.Context, name string, port int, ttl time.Duration) (IsloShare, error) {
	reqBody := isloCreateShareRequest{Port: port}
	if ttl > 0 {
		seconds := int(ttl.Seconds())
		// Islo accepts 60s..7d. Snap into range so the bridge plane uses the
		// closest legal TTL rather than refusing the call — the user-facing
		// flag already validates the original range.
		if seconds < 60 {
			seconds = 60
		}
		if seconds > 7*24*3600 {
			seconds = 7 * 24 * 3600
		}
		reqBody.TTLSeconds = &seconds
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return IsloShare{}, fmt.Errorf("encode share request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/sandboxes/"+url.PathEscape(name)+"/shares", bytes.NewReader(body))
	if err != nil {
		return IsloShare{}, err
	}
	if err := c.authorize(ctx, httpReq); err != nil {
		return IsloShare{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return IsloShare{}, fmt.Errorf("islo create share: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return IsloShare{}, fmt.Errorf("islo create share %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	var raw isloShareResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return IsloShare{}, fmt.Errorf("decode share response: %w", err)
	}
	return isloShareFromAPI(raw), nil
}

func (c *isloSDKClient) ListShares(ctx context.Context, name string) ([]IsloShare, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/sandboxes/"+url.PathEscape(name)+"/shares", nil)
	if err != nil {
		return nil, err
	}
	if err := c.authorize(ctx, httpReq); err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("islo list shares: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("islo list shares %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	var raw []isloShareResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode share list response: %w", err)
	}
	out := make([]IsloShare, 0, len(raw))
	for _, item := range raw {
		out = append(out, isloShareFromAPI(item))
	}
	return out, nil
}

func (c *isloSDKClient) authorize(ctx context.Context, req *http.Request) error {
	token, err := c.auth.Token(ctx)
	if err != nil {
		return fmt.Errorf("islo auth: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func isloShareFromAPI(raw isloShareResponse) IsloShare {
	share := IsloShare{
		ShareID: raw.ShareID,
		URL:     raw.URL,
		Port:    raw.Port,
	}
	if t, err := time.Parse(time.RFC3339, raw.CreatedAt); err == nil {
		share.CreatedAt = t
	}
	if raw.ExpiresAt != nil && *raw.ExpiresAt != "" {
		share.ExpiresAtSet = true
		if t, err := time.Parse(time.RFC3339, *raw.ExpiresAt); err == nil {
			share.ExpiresAt = t
		}
	}
	return share
}

func (c *isloSDKClient) ExecStream(ctx context.Context, name string, req *gosdk.ExecRequest, stdout, stderr io.Writer) (int, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return 1, fmt.Errorf("encode exec request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/sandboxes/"+name+"/exec/stream", bytes.NewReader(body))
	if err != nil {
		return 1, err
	}
	token, err := c.auth.Token(ctx)
	if err != nil {
		return 1, fmt.Errorf("islo auth: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 1, fmt.Errorf("islo exec stream %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	return parseIsloSSE(resp.Body, stdout, stderr)
}

func parseIsloSSE(r io.Reader, stdout, stderr io.Writer) (int, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	exitCode := 0
	seenExit := false
	streamErr := ""
	event := ""
	var data []string
	flush := func() error {
		if event == "" && len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		switch event {
		case "stdout":
			_, _ = stdout.Write([]byte(payload))
		case "stderr":
			_, _ = stderr.Write([]byte(payload))
		case "exit":
			n, err := strconv.Atoi(strings.TrimSpace(payload))
			if err != nil {
				return fmt.Errorf("islo exec stream invalid exit event %q: %w", payload, err)
			}
			exitCode = n
			seenExit = true
		case "error":
			// The Islo exec SSE stream emits an "error" event for stream or
			// VM-level failures. Capture the last one so we can surface a
			// meaningful message instead of a generic missing-exit error when
			// the stream ends without an exit event.
			if msg := strings.TrimSpace(payload); msg != "" {
				streamErr = msg
			}
		}
		event = ""
		data = data[:0]
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return 1, err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field = line
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := flush(); err != nil {
		return 1, err
	}
	if err := scanner.Err(); err != nil {
		return 1, err
	}
	if !seenExit {
		if streamErr != "" {
			return 1, fmt.Errorf("islo exec stream error: %s", streamErr)
		}
		return 1, fmt.Errorf("islo exec stream ended without exit event")
	}
	return exitCode, nil
}
