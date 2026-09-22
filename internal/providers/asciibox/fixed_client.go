package asciibox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// The public Box API retains Idempotency-Key bindings for 24 hours.
const asciiBoxIdempotencyWindow = 24 * time.Hour

func (c *client) createKeyedBox(ctx context.Context, create createRequest) (boxData, error) {
	body := map[string]any{}
	if create.TTL > 0 {
		body["ttlSeconds"] = int(create.TTL.Round(time.Second).Seconds())
	}
	if c.org != "" && c.org != "personal" {
		body["org"] = c.org
	}
	req, err := shared.NewCompactJSONRequest(ctx, http.MethodPost, c.apiURL+"/api/box/v1/boxes", body)
	if err != nil {
		return boxData{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", create.IdempotencyKey)
	// Neither redirects nor transport retries may hide another submission.
	req.GetBody = nil
	transport := c.http
	if transport == nil {
		transport = http.DefaultClient
	}
	httpClient := *transport
	if httpClient.Timeout == 0 || httpClient.Timeout > 30*time.Second {
		httpClient.Timeout = 30 * time.Second
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := httpClient.Do(req)
	if err != nil {
		return boxData{}, fmt.Errorf("ascii-box keyed create outcome uncertain: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, boxCommandOutputLimit+1))
	if err != nil || len(data) > boxCommandOutputLimit {
		return boxData{}, fmt.Errorf("ascii-box keyed create response incomplete; attempt retained")
	}
	if response.StatusCode != http.StatusAccepted {
		return boxData{}, fmt.Errorf("ascii-box keyed create HTTP %d; attempt retained", response.StatusCode)
	}
	duplicate, parseErr := core.JSONHasDuplicateKeys(json.NewDecoder(bytes.NewReader(data)))
	if parseErr != nil || duplicate {
		return boxData{}, fmt.Errorf("ascii-box keyed create response is ambiguous; attempt retained")
	}
	var result struct {
		OK      bool    `json:"ok"`
		Type    string  `json:"type"`
		Box     boxData `json:"box"`
		Sandbox boxData `json:"sandbox"`
	}
	if err := json.Unmarshal(data, &result); err != nil || !result.OK || (result.Type != "box.created" && result.Type != "sandbox.created") || (result.Box.ID != "" && result.Sandbox.ID != "") {
		return boxData{}, fmt.Errorf("ascii-box keyed create returned invalid creation evidence; attempt retained")
	}
	if result.Box.ID == "" {
		result.Box = result.Sandbox
	}
	if !concreteBoxID(result.Box.ID) {
		return boxData{}, fmt.Errorf("ascii-box keyed create returned no concrete Box ID; attempt retained")
	}
	result.Box.createdID = result.Box.ID
	return result.Box, nil
}
