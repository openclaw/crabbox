package shared

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DecodeUnboundedJSONResponse borrows the body; the caller must close it.
// Read errors take precedence over HTTP status. Only a zero-length body skips
// decoding, and read and JSON errors are returned unchanged. The adapter owns
// its typed API error and any body summarization or redaction.
func DecodeUnboundedJSONResponse(resp *http.Response, out any, apiError func(int, string, []byte) error) error {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(resp.StatusCode, resp.Status, data)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return err
		}
	}
	return nil
}

// DecodeBoundedJSONResponse consumes and closes a finite control-plane response.
// Read failures and overflow take precedence over HTTP status; adapters retain
// their typed API errors and redaction policy. limit must be positive and below
// the maximum int64 value. Streaming responses use a different contract.
func DecodeBoundedJSONResponse(resp *http.Response, limit int64, out any, provider string, apiError func(int, string, string) error) error {
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("%s response exceeds %d bytes", provider, limit)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(resp.StatusCode, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s data: %w", provider, err)
		}
	}
	return nil
}
