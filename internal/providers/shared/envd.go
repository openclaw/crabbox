package shared

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
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
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		_ = pw.CloseWithError(err)
		return err
	}
	upload.SetHeaders(req)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	go func() {
		part, err := writer.CreateFormFile("file", upload.TargetPath)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, upload.Content); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if err := writer.Close(); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()
	resp, err := SecureHTTPClient(upload.HTTPClient, req.URL, upload.RedirectError).Do(req)
	if err != nil {
		_ = pr.CloseWithError(err)
		_ = pw.CloseWithError(err)
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
}

type EnvdProcessRequest struct {
	Endpoint       string
	Command        string
	CWD            string
	Env            map[string]string
	User           string
	Timeout        time.Duration
	Stdout         io.Writer
	Stderr         io.Writer
	AccessToken    string
	HTTPClient     *http.Client
	SetHeaders     func(*http.Request)
	RedirectError  func(*url.URL) error
	EncodeEnvelope func(any) ([]byte, error)
	ParseStream    func(io.Reader, io.Writer, io.Writer, ...string) (int, error)
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
	body, err := process.EncodeEnvelope(start)
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
	return process.ParseStream(resp.Body, process.Stdout, process.Stderr, process.AccessToken)
}

func durationMillisCeil(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return int64((duration + time.Millisecond - 1) / time.Millisecond)
}
