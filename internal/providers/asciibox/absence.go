package asciibox

import (
	"context"
	"errors"
	"fmt"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (*backend) ReconcileAbsenceOnOrdinaryStop() bool { return true }

func (b *backend) VerifyResourceAbsent(ctx context.Context, claim core.LeaseClaim) (core.AbsenceEvidence, error) {
	cfg, err := b.configForRun()
	if err != nil {
		return core.AbsenceEvidence{}, err
	}
	if _, err := boxClaimBinding(cfg, claim); err != nil {
		return core.AbsenceEvidence{}, err
	}
	client, err := newAPI(cfg, b.rt)
	if err != nil {
		return core.AbsenceEvidence{}, err
	}
	return boxAbsenceVerifier(client).VerifyResourceAbsent(ctx, claim)
}

// Release callers have already checked configuration scope with boxClaimBinding.
func boxAbsenceVerifier(client api) core.AbsenceVerifierFunc {
	return func(ctx context.Context, claim core.LeaseClaim) (core.AbsenceEvidence, error) {
		box, err := client.GetBox(ctx, claim.CloudID)
		if ctx.Err() != nil {
			return core.AbsenceEvidence{}, fmt.Errorf("ascii-box cleanup phase=ownership-check; retaining claim: %w", ctx.Err())
		}
		if err == nil {
			return core.AbsenceEvidence{}, validateBoxIdentity(box, boxFromClaim(claim))
		}
		var missing *boxNotFoundError
		if !errors.As(err, &missing) || missing.id != claim.CloudID {
			return core.AbsenceEvidence{}, fmt.Errorf("ascii-box ownership lookup; retaining claim: %w", err)
		}
		boxes, err := client.ListBoxes(ctx, true)
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			return core.AbsenceEvidence{}, fmt.Errorf("ascii-box cleanup phase=inventory-confirmation; retaining claim: %w", err)
		}
		for _, box := range boxes {
			if box.ID == claim.CloudID {
				return core.AbsenceEvidence{}, core.Exit(4, "ascii-box resource is still in inventory; retaining claim")
			}
		}
		return core.AbsenceEvidence{Claim: claim, ExactNotFound: true, InventoryComplete: true}, nil
	}
}
