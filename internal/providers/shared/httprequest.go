package shared

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// NewJSONRequest preserves Encoder's bytes and errors before constructing the
// request. Callers own headers, transport and retries; a nil body stays absent.
func NewJSONRequest(ctx context.Context, method, url string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
		reader = &buf
	}
	return http.NewRequestWithContext(ctx, method, url, reader)
}

// NewCompactJSONRequest uses Marshal's compact wire representation without the
// Encoder newline. A nil interface stays absent; typed-nil values encode as null.
func NewCompactJSONRequest(ctx context.Context, method, url string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	return http.NewRequestWithContext(ctx, method, url, reader)
}
