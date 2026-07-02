package daytona

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	apidaytona "github.com/daytonaio/daytona/libs/api-client-go"
	sdkdaytona "github.com/daytonaio/daytona/libs/sdk-go/pkg/daytona"
)

func (b *daytonaLeaseBackend) uploadDaytonaArchive(ctx context.Context, sandboxID, archivePath string, archive *os.File) error {
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	apiSandbox, err := client.GetSandbox(ctx, sandboxID)
	if err != nil {
		return daytonaError("get sandbox", err)
	}
	endpoint, err := daytonaToolboxUploadURL(apiSandbox, sandboxID, archivePath)
	if err != nil {
		return err
	}
	headers, err := daytonaToolboxHeaders(b.cfg)
	if err != nil {
		return err
	}
	httpClient := b.rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return uploadDaytonaFileStream(ctx, httpClient, endpoint, headers, archive, path.Base(archivePath))
}

func daytonaToolboxUploadURL(sandbox *apidaytona.Sandbox, sandboxID, remotePath string) (string, error) {
	proxyURL := strings.TrimRight(strings.TrimSpace(sandbox.GetToolboxProxyUrl()), "/")
	if proxyURL == "" {
		return "", fmt.Errorf("daytona sandbox %s has no toolbox proxy URL", sandboxID)
	}
	u, err := url.Parse(proxyURL + "/" + strings.Trim(sandboxID, "/") + "/files/upload")
	if err != nil {
		return "", fmt.Errorf("daytona toolbox upload URL: %w", err)
	}
	q := u.Query()
	q.Set("path", remotePath)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func daytonaToolboxHeaders(cfg Config) (map[string]string, error) {
	auth, err := daytonaAuthConfig(cfg)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{
		"Authorization":         "Bearer " + auth.token(),
		"User-Agent":            "sdk-go/" + sdkdaytona.Version,
		"X-Daytona-SDK-Version": sdkdaytona.Version,
		"X-Daytona-Source":      "sdk-go",
	}
	if auth.OrganizationID != "" {
		headers["X-Daytona-Organization-ID"] = auth.OrganizationID
	}
	return headers, nil
}

func uploadDaytonaFileStream(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, file io.Reader, filename string) error {
	if client == nil {
		client = http.DefaultClient
	}
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- writeMultipartFile(pw, writer, file, filename)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		_ = pw.CloseWithError(err)
		return err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		_ = pr.CloseWithError(err)
		return fmt.Errorf("daytona upload archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		_ = pr.CloseWithError(fmt.Errorf("daytona upload archive: %s", resp.Status))
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if len(body) > 0 {
			message := strings.TrimSpace(redactDaytonaSecrets(string(body), daytonaHeaderSecrets(headers)...))
			return fmt.Errorf("daytona upload archive: %s: %s", resp.Status, message)
		}
		return fmt.Errorf("daytona upload archive: %s", resp.Status)
	}
	writeErr := <-writeDone
	if writeErr != nil {
		return fmt.Errorf("daytona upload archive: %w", writeErr)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func writeMultipartFile(pipe *io.PipeWriter, writer *multipart.Writer, file io.Reader, filename string) error {
	part, err := writer.CreateFormFile("file", filename)
	if err == nil {
		_, err = io.Copy(part, file)
	}
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = pipe.CloseWithError(err)
		return err
	}
	return pipe.Close()
}

func daytonaHeaderSecrets(headers map[string]string) []string {
	var secrets []string
	for key, value := range headers {
		if !strings.EqualFold(key, "Authorization") {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		secrets = append(secrets, value)
		if strings.HasPrefix(strings.ToLower(value), "bearer ") {
			secrets = append(secrets, strings.TrimSpace(value[len("bearer "):]))
		}
	}
	return secrets
}
