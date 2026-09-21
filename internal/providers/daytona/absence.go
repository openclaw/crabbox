package daytona

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	daytona "github.com/daytonaio/daytona/libs/api-client-go"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *daytonaLeaseBackend) VerifyResourceAbsent(ctx context.Context, claim core.LeaseClaim) (core.AbsenceEvidence, error) {
	if !strings.HasPrefix(claim.ProviderScope, "daytona:organization:v1:") {
		return core.AbsenceEvidence{}, core.Exit(4, "Daytona claim predates account binding; manual recovery per docs/providers/daytona.md")
	}
	if claim.Provider != daytonaProvider || claim.CloudID == "" || claim.CloudImmutableID != claim.CloudID {
		return core.AbsenceEvidence{}, core.Exit(4, "Daytona absence recovery requires the original immutable sandbox identity")
	}
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return core.AbsenceEvidence{}, err
	}
	scope, organization, err := daytonaAccountContext(ctx, client, false)
	if err != nil {
		return core.AbsenceEvidence{}, err
	}
	if scope != claim.ProviderScope {
		return core.AbsenceEvidence{}, core.Exit(4, "Daytona endpoint or authenticated organization differs from the claim; retaining claim")
	}
	verifier, ok := client.(interface {
		resourceAbsent(context.Context, string, string) (bool, error)
	})
	if !ok {
		return core.AbsenceEvidence{}, core.Exit(4, "Daytona client cannot verify complete resource absence")
	}
	absent, err := verifier.resourceAbsent(ctx, claim.CloudID, organization)
	if err != nil || !absent {
		return core.AbsenceEvidence{}, err
	}
	return core.AbsenceEvidence{Claim: claim, ExactNotFound: true, InventoryComplete: true}, nil
}

func (c *daytonaSDKClient) resourceAbsent(ctx context.Context, id, organization string) (bool, error) {
	req := c.api.SandboxAPI.GetSandbox(c.ctx(ctx), id).Verbose(true)
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	item, response, err := req.Execute()
	if err == nil {
		if item == nil || item.GetId() != id || item.GetOrganizationId() != organization {
			return false, core.Exit(4, "Daytona exact lookup returned a different resource or organization")
		}
		return false, nil
	}
	if response == nil || response.StatusCode != http.StatusNotFound || !daytonaIsNotFoundError(err) {
		return false, c.redactError(err)
	}
	var body interface{ Body() []byte }
	// A malformed gateway response is not a provider not-found receipt.
	if !errors.As(err, &body) {
		return false, core.Exit(4, "Daytona not-found response has no structured body")
	}
	var missing struct {
		Message    string `json:"message"`
		StatusCode *int   `json:"statusCode"`
	}
	duplicate, decodeErr := core.JSONHasDuplicateKeys(json.NewDecoder(bytes.NewReader(body.Body())))
	if decodeErr != nil || duplicate || json.Unmarshal(body.Body(), &missing) != nil || missing.Message == "" || missing.StatusCode != nil && *missing.StatusCode != http.StatusNotFound {
		return false, core.Exit(4, "Daytona not-found response is malformed; retaining claim")
	}
	return c.inventoryOmitsResource(ctx, id, organization)
}

// Unlike discovery, absence proof must include sandboxes whose labels changed.
func (c *daytonaSDKClient) inventoryOmitsResource(ctx context.Context, id, organization string) (bool, error) {
	seenCursors, seenIDs := map[string]bool{}, map[string]bool{}
	cursor := ""
	for range 100 {
		page, err := c.absenceInventoryPage(ctx, cursor)
		if err != nil {
			return false, c.redactError(err)
		}
		if page == nil || page.Items == nil || len(page.Items) > 100 {
			return false, core.Exit(4, "Daytona inventory response is incomplete or oversized")
		}
		for _, item := range page.Items {
			if item.Id == "" || strings.TrimSpace(item.Id) != item.Id || item.OrganizationId != organization || seenIDs[item.Id] {
				return false, core.Exit(4, "Daytona inventory resource identity is missing, duplicated, or outside the claimed organization")
			}
			if item.Id == id {
				return false, core.Exit(4, "Daytona resource remains in inventory; retaining claim")
			}
			seenIDs[item.Id] = true
		}
		next := page.GetNextCursor()
		if next == "" {
			return true, nil
		}
		if strings.TrimSpace(next) != next || len(page.Items) == 0 || seenCursors[next] {
			return false, core.Exit(4, "Daytona inventory pagination is incomplete or repeated")
		}
		seenCursors[next], cursor = true, next
	}
	return false, fmt.Errorf("Daytona inventory exceeded the 100-page absence verification limit")
}

func (c *daytonaSDKClient) absenceInventoryPage(ctx context.Context, cursor string) (*daytona.ListSandboxesResponse, error) {
	query := url.Values{"limit": {"100"}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.apiURL, "/")+"/sandbox?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.orgID != "" {
		req.Header.Set("X-Daytona-Organization-ID", c.orgID)
	}
	response, err := c.api.GetConfig().HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, core.Exit(4, "Daytona absence inventory returned HTTP %d", response.StatusCode)
	}
	const limit = 8 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, core.Exit(4, "Daytona absence inventory exceeds the response limit")
	}
	duplicate, err := core.JSONHasDuplicateKeys(json.NewDecoder(bytes.NewReader(data)))
	if err != nil || duplicate {
		return nil, core.Exit(4, "Daytona absence inventory is malformed or has duplicate fields")
	}
	var page daytona.ListSandboxesResponse
	if err := json.Unmarshal(data, &page); err != nil {
		return nil, core.Exit(4, "Daytona absence inventory is malformed")
	}
	return &page, nil
}
