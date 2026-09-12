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
