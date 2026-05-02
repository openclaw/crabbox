package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// mppxOptIn reports whether the user has asked for automatic 402 → mppx
// fallback. Off by default so existing flows are unaffected.
func mppxOptIn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CRABBOX_MPP_PAY"))) {
	case "auto", "1", "true", "yes":
		return true
	default:
		return false
	}
}

// mppxAvailable reports whether the `mppx` binary can be located.
func mppxAvailable() bool {
	_, err := exec.LookPath("mppx")
	return err == nil
}

// mppxJSONFetch shells out to the `mppx` CLI to perform a JSON HTTP request.
// mppx handles the 402 → sign → retry cycle internally using the user's
// configured account (MPPX_ACCOUNT, MPPX_PRIVATE_KEY, etc). Returns the
// parsed response status and body for the final (post-payment) response.
func mppxJSONFetch(
	ctx context.Context,
	url string,
	body []byte,
	headers http.Header,
) (int, []byte, error) {
	args := []string{"-i", "-s", "-X", http.MethodPost}
	for key, values := range headers {
		if strings.EqualFold(key, "authorization") || strings.EqualFold(key, "content-type") {
			continue
		}
		for _, v := range values {
			args = append(args, "-H", fmt.Sprintf("%s: %s", key, v))
		}
	}
	if len(body) > 0 {
		args = append(args, "-J", string(body))
	}
	args = append(args, url)
	cmd := exec.CommandContext(ctx, "mppx", args...)
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, nil, fmt.Errorf("mppx invocation failed: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parseMppxIncludeOutput(stdout.Bytes())
}

func parseMppxIncludeOutput(data []byte) (int, []byte, error) {
	scanner := bufio.NewReader(bytes.NewReader(data))
	statusLine, err := scanner.ReadString('\n')
	if err != nil {
		return 0, nil, fmt.Errorf("mppx response missing status line")
	}
	parts := strings.SplitN(strings.TrimSpace(statusLine), " ", 3)
	if len(parts) < 2 {
		return 0, nil, fmt.Errorf("mppx response invalid status: %q", statusLine)
	}
	status, convErr := strconv.Atoi(parts[1])
	if convErr != nil {
		return 0, nil, fmt.Errorf("mppx response invalid status code: %w", convErr)
	}
	for {
		line, err := scanner.ReadString('\n')
		if err != nil {
			return 0, nil, fmt.Errorf("mppx response missing header/body separator")
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	body, err := scanner.ReadBytes(0)
	if err != nil && len(body) == 0 {
		return status, nil, nil
	}
	return status, bytes.TrimRight(body, "\x00"), nil
}

// retryWithMPPX inspects an error from a coordinator request; if it's a 402
// AND mppx is available + opted in, re-issues the request via the mppx CLI
// and decodes the response into `out`. Returns ErrMppxNotApplicable when no
// retry is appropriate so the caller can pass the original error through.
func retryWithMPPX(
	ctx context.Context,
	c *CoordinatorClient,
	method, path string,
	body []byte,
	originalErr error,
	out any,
) error {
	if !mppxOptIn() {
		return errMppxNotApplicable
	}
	var httpErr *coordinatorHTTPError
	if !errors.As(originalErr, &httpErr) || httpErr.Status != http.StatusPaymentRequired {
		return errMppxNotApplicable
	}
	if !mppxAvailable() {
		return fmt.Errorf("%w; install mppx (npm i -g mppx) and configure an account to enable automatic payment", originalErr)
	}
	if method != http.MethodPost {
		return errMppxNotApplicable
	}
	headers := http.Header{}
	if owner := localCoordinatorOwner(); owner != "" {
		headers.Set("X-Crabbox-Owner", owner)
	}
	if org := os.Getenv("CRABBOX_ORG"); org != "" {
		headers.Set("X-Crabbox-Org", org)
	}
	status, respBody, err := mppxJSONFetch(ctx, c.BaseURL+path, body, headers)
	if err != nil {
		return fmt.Errorf("%w; mppx fallback failed: %v", originalErr, err)
	}
	if status < 200 || status >= 300 {
		return &coordinatorHTTPError{
			Method: method,
			Path:   path,
			Status: status,
			Body:   strings.TrimSpace(string(respBody)),
		}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

var errMppxNotApplicable = errors.New("mppx fallback not applicable")
