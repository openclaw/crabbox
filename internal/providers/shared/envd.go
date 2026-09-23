package shared

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type EnvdUploadFileRequest struct {
	Endpoint       string
	TargetPath     string
	User           string
	AccessToken    string
	Content        io.Reader
	HTTPClient     *http.Client
	SetHeaders     func(*http.Request)
	RedirectError  func(*url.URL) error
	SummarizeError func([]byte) string
	APIError       func(int, string, string) error
}

func UploadEnvdFile(ctx context.Context, upload EnvdUploadFileRequest) error {
	endpoint, err := url.Parse(upload.Endpoint)
	if err != nil {
		return err
	}
	query := endpoint.Query()
	query.Set("path", upload.TargetPath)
	if strings.TrimSpace(upload.User) != "" {
		query.Set("username", upload.User)
	}
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), nil)
	if err != nil {
		return err
	}
	upload.SetHeaders(req)
	return WithMultipartFile(ctx, upload.TargetPath, upload.Content, func(body io.ReadCloser, contentType string) error {
		req.Body = body
		req.Header.Set("Content-Type", contentType)
		resp, err := SecureHTTPClient(upload.HTTPClient, req.URL, upload.RedirectError).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			body := RedactErrorSecrets(upload.SummarizeError(data), upload.AccessToken)
			return upload.APIError(resp.StatusCode, resp.Status, body)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}, func(err error) error {
		return ErrorWithMessage(RedactErrorSecrets(err.Error(), upload.AccessToken), err)
	})
}

type EnvdProcessRequest struct {
	EnvdSandboxProcessRequest
	Provider      string
	Endpoint      string
	AccessToken   string
	HTTPClient    *http.Client
	SetHeaders    func(*http.Request)
	RedirectError func(*url.URL) error
	// InterpretEnd retains the provider's completion policy. A returned error
	// stops decoding immediately with that code; nil continues the RPC stream.
	InterpretEnd   func(EnvdProcessEnd, io.Writer, ...string) (int, error)
	SummarizeError func([]byte) string
	APIError       func(int, string, string) error
}

func StartEnvdProcess(ctx context.Context, process EnvdProcessRequest) (int, error) {
	if process.Stdout == nil {
		process.Stdout = io.Discard
	}
	if process.Stderr == nil {
		process.Stderr = io.Discard
	}
	env := process.Env
	if env == nil {
		env = map[string]string{}
	}
	start := map[string]any{
		"process": map[string]any{
			"cmd":  "/bin/bash",
			"args": []string{"-l", "-c", process.Command},
			"envs": env,
			"cwd":  process.CWD,
		},
		"stdin": false,
	}
	body, err := encodeConnectJSONEnvelope(start)
	if err != nil {
		return 1, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, process.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 1, err
	}
	process.SetHeaders(httpReq)
	httpReq.Header.Set("Connect-Protocol-Version", "1")
	httpReq.Header.Set("Connect-Content-Encoding", "identity")
	httpReq.Header.Set("Content-Type", "application/connect+json")
	httpReq.Header.Set("Keepalive-Ping-Interval", "50")
	if process.User != "" {
		httpReq.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(process.User+":")))
	}
	if timeoutMs := durationMillisCeil(process.Timeout); timeoutMs > 0 {
		httpReq.Header.Set("Connect-Timeout-Ms", fmt.Sprint(timeoutMs))
	}
	resp, err := SecureHTTPClient(process.HTTPClient, httpReq.URL, process.RedirectError).Do(httpReq)
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		body := RedactErrorSecrets(process.SummarizeError(data), process.AccessToken)
		return 1, process.APIError(resp.StatusCode, resp.Status, body)
	}
	return ParseEnvdProcessStream(process.Provider, resp.Body, process.Stdout, process.Stderr, process.InterpretEnd, process.AccessToken)
}

func durationMillisCeil(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return int64((duration + time.Millisecond - 1) / time.Millisecond)
}
