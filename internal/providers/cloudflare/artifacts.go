package cloudflare

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const cloudflareArtifactProtocol = "CBX-CF-ARTIFACT-1"

func (c *cloudflareClient) checkArtifactCapability(ctx context.Context) error {
	var ready struct {
		OK               bool   `json:"ok"`
		Runner           string `json:"runner"`
		ArtifactProtocol string `json:"artifactProtocol"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/readiness", nil, &ready); err != nil {
		return err
	}
	if !ready.OK || ready.Runner != providerName || ready.ArtifactProtocol != cloudflareArtifactProtocol {
		return core.Exit(2, "cloudflare artifact downloads require runner protocol %s; update the deployed Cloudflare runner", cloudflareArtifactProtocol)
	}
	return nil
}

func (c *cloudflareClient) collectArtifacts(ctx context.Context, leaseID, workdir, script string) ([]byte, error) {
	req, err := shared.NewJSONRequest(ctx, http.MethodPost, c.baseURL+c.sandboxEndpoint(leaseID, "/artifacts"), map[string]string{
		"protocol": cloudflareArtifactProtocol, "cwd": workdir, "script": script,
	})
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("artifact transfer: %s", redactCloudflareRunnerSecrets(err.Error(), c.token))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.responseError(resp)
	}
	length, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil || length <= 0 || length > core.DelegatedRunArtifactDefaultMaxBytes || resp.Header.Get("X-Crabbox-Artifact-Protocol") != cloudflareArtifactProtocol || resp.Header.Get("Content-Type") != "application/gzip" {
		return nil, fmt.Errorf("invalid artifact transfer metadata")
	}
	archive, err := io.ReadAll(io.LimitReader(resp.Body, core.DelegatedRunArtifactDefaultMaxBytes+1))
	if err != nil || int64(len(archive)) != length {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("incomplete or oversized artifact transfer")
	}
	digest := sha256.Sum256(archive)
	if resp.Header.Get("X-Crabbox-Artifact-Sha256") != hex.EncodeToString(digest[:]) {
		return nil, fmt.Errorf("artifact transfer digest mismatch")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return archive, nil
}
