package daytona

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	api "github.com/daytonaio/daytona/libs/api-client-go"
	core "github.com/openclaw/crabbox/internal/cli"
)

// Forgetting is an explicit local recovery operation, not proof of provider
// deletion: even an application 404 can mean lost access to the original account.
func (b *daytonaLeaseBackend) forgetMissing(ctx context.Context, id string) error {
	claim, exists, err := resolveLeaseClaimForProvider(id, daytonaProvider)
	if err != nil {
		return err
	}
	if !exists || strings.TrimSpace(claim.CloudID) == "" {
		return exit(4, "Daytona local cleanup requires a claim bound to an exact sandbox ID")
	}
	if claim.FixedCreateIntent != nil || claim.Labels["fixed_claim_provider"] != "" {
		return exit(4, "Daytona fixed claims retain replay protection; use stop without --daytona-forget-missing to reconcile release")
	}
	if claim.CoordinatorRegistrationURL != "" || claim.RuntimeAdapterRegistrationID != "" || claim.RuntimeAdapterPendingRegistrationID != "" {
		return exit(4, "Daytona registered claims require normal release reconciliation; retain the local record")
	}
	client, err := newDaytonaClient(b.cfg, b.rt)
	if err != nil {
		return err
	}
	err = core.CleanupLeaseClaimIfUnchangedAfterContext(ctx, claim.LeaseID, claim, true, func() error {
		if err := core.AuthorizeCheckpointRelease(claim, ""); err != nil {
			return err
		}
		_, err := client.GetSandbox(ctx, claim.CloudID)
		if err == nil {
			return exit(4, "Daytona sandbox %s did not return a missing-sandbox response; retain its claim", claim.CloudID)
		}
		var apiErr *api.GenericOpenAPIError
		var response struct {
			StatusCode int    `json:"statusCode"`
			Error      string `json:"error"`
			Message    string `json:"message"`
		}
		if !daytonaIsNotFoundError(err) || !errors.As(err, &apiErr) ||
			json.Unmarshal(apiErr.Body(), &response) != nil || response.StatusCode != 404 ||
			response.Error != "Not Found" || strings.TrimSpace(response.Message) == "" {
			return daytonaError("verify missing sandbox; retain its claim", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(b.rt.Stderr, "forgot local Daytona claim lease=%s sandbox=%s; provider deletion was not verified\n", claim.LeaseID, claim.CloudID)
	return nil
}
