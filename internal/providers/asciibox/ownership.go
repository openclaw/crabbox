package asciibox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const boxCreationLabel = "ascii_box_created_at"
const boxDeletionLabel = "ascii_box_deletion_completed"
const boxDeletionBindingDomain = "completed-native-operation/v1\x00"
const boxDeletionOperationLabel = "ascii_box_deletion_operation"
const boxDeletionOperationBindingLabel = "ascii_box_deletion_operation_binding"
const boxDeletionOperationBindingDomain = "accepted-native-operation/v1\x00"
const boxReleaseTimeout = 3 * time.Minute

func asciiBoxOrg() string { return blank(strings.TrimSpace(os.Getenv("BOX_ORG")), "personal") }

func (Provider) ClaimScope(cfg Config) string {
	endpoint, err := validateAsciiBoxBaseURL(blank(strings.TrimSpace(cfg.AsciiBox.BaseURL), "https://ascii.dev"))
	if err != nil {
		return ""
	}
	encoded, _ := json.Marshal([]string{endpoint, asciiBoxOrg()})
	return string(encoded)
}

func boxCreationTime(box boxData) string {
	value, ok := box.CreatedAt.(string)
	if !ok {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return ""
	}
	return parsed.UTC().Format(time.RFC3339Nano)
}

func concreteBoxID(id string) bool {
	if !strings.HasPrefix(id, "bx_") || len(id) <= 3 {
		return false
	}
	for _, r := range id[3:] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func validateBoxIdentity(actual, expected boxData) error {
	if !concreteBoxID(expected.ID) || actual.ID != expected.ID || boxCreationTime(expected) == "" || boxCreationTime(actual) != boxCreationTime(expected) {
		return exit(2, "ascii-box %q identity is missing or changed; retaining resource and claim", expected.ID)
	}
	return nil
}

func boxClaimBinding(cfg Config, claim LeaseClaim) (shared.ClaimBinding, error) {
	scope := (Provider{}).ClaimScope(cfg)
	if !core.IsCanonicalLeaseID(claim.LeaseID) || claim.Slug == "" || !concreteBoxID(claim.CloudID) || claim.Revision == "" || claim.Labels[boxCreationLabel] == "" || scope == "" {
		return shared.ClaimBinding{}, exit(2, "ascii-box lease %q requires an exact local ownership claim; legacy and unclaimed resources are retained", claim.LeaseID)
	}
	want := shared.ClaimBinding{Provider: providerName, ProviderScope: scope, ExactProviderScope: true,
		LeaseID: claim.LeaseID, Slug: claim.Slug, CloudID: claim.CloudID,
		RequiredLabels: map[string]string{"box_id": claim.CloudID, "ascii_box_scope": scope, boxCreationLabel: claim.Labels[boxCreationLabel]},
	}
	if err := shared.ValidateClaimBinding(claim, want); err != nil {
		return want, exit(2, "ascii-box ownership mismatch: %v", err)
	}
	if boxCreationTime(boxFromClaim(claim)) == "" {
		return want, exit(2, "ascii-box claim creation identity is invalid")
	}
	if recorded, ok := claim.Labels[boxDeletionLabel]; ok && recorded != boxDeletionBinding(claim) {
		return want, exit(2, "ascii-box completed deletion witness does not match its claim; retaining claim")
	}
	if _, err := boxDeletionOperationFromClaim(claim); err != nil {
		return want, err
	}
	return want, nil
}

func boxFromClaim(claim LeaseClaim) boxData {
	operationID, _ := boxDeletionOperationFromClaim(claim)
	return boxData{
		ID: claim.CloudID, CreatedAt: claim.Labels[boxCreationLabel],
		deletionCompleted:   claim.Labels[boxDeletionLabel] == boxDeletionBinding(claim),
		deletionOperationID: operationID,
	}
}

func boxDeletionBinding(claim LeaseClaim) string {
	// Earlier unreleased witnesses proved only native request acceptance.
	return fmt.Sprintf("%x", sha256.Sum256(append([]byte(boxDeletionBindingDomain), boxDeletionClaimBytes(claim)...)))
}

func boxDeletionOperationBinding(claim LeaseClaim, operationID string) string {
	return fmt.Sprintf("%x", sha256.Sum256(append([]byte(boxDeletionOperationBindingDomain+operationID+"\x00"), boxDeletionClaimBytes(claim)...)))
}

func boxDeletionClaimBytes(claim LeaseClaim) []byte {
	// The durable claim transaction refreshes these two fields when recording
	// deletion evidence. All ownership, scope, and other claim content stays bound.
	claim.Revision = ""
	claim.LastUsedAt = ""
	claim.Labels = shared.CloneLabels(claim.Labels)
	delete(claim.Labels, boxDeletionLabel)
	delete(claim.Labels, boxDeletionOperationLabel)
	delete(claim.Labels, boxDeletionOperationBindingLabel)
	encoded, _ := json.Marshal(claim)
	return encoded
}

func boxDeletionOperationFromClaim(claim LeaseClaim) (string, error) {
	operationID, hasID := claim.Labels[boxDeletionOperationLabel]
	binding, hasBinding := claim.Labels[boxDeletionOperationBindingLabel]
	if !hasID && !hasBinding {
		return "", nil
	}
	if !hasID || !hasBinding || !boxDeletionIDRE.MatchString(operationID) || binding != boxDeletionOperationBinding(claim, operationID) {
		return "", exit(2, "ascii-box deletion operation reference does not match its claim; retaining claim")
	}
	return operationID, nil
}

func resolveOwnedBox(cfg Config, identifier string) (LeaseClaim, error) {
	id := strings.TrimSpace(identifier)
	if id == "" {
		return LeaseClaim{}, exit(2, "ascii-box requires a lease ID, slug, or concrete Box ID")
	}
	if core.IsCanonicalLeaseID(id) {
		claim, exists, err := core.ReadLeaseClaimWithPresence(id)
		if err != nil {
			return LeaseClaim{}, err
		}
		if !exists {
			return LeaseClaim{}, exit(2, "ascii-box %q has no exact local ownership claim", id)
		}
		_, err = boxClaimBinding(cfg, claim)
		return claim, err
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return LeaseClaim{}, err
	}
	var matches []LeaseClaim
	for _, claim := range claims {
		if claim.Provider == providerName && (id == claim.Slug || id == claim.CloudID) {
			matches = append(matches, claim)
		}
	}
	if len(matches) != 1 {
		return LeaseClaim{}, exit(2, "ascii-box %q requires one unambiguous exact local ownership claim (found %d)", id, len(matches))
	}
	_, err = boxClaimBinding(cfg, matches[0])
	return matches[0], err
}

func publishBoxClaim(cfg Config, leaseID, slug, repoRoot string, box boxData, keep bool) (LeaseClaim, error) {
	var published LeaseClaim
	err := core.WithDurableLeaseClaimLock(leaseID, func(claim *LeaseClaim, exists bool, persist func() error) error {
		if exists {
			return exit(2, "ascii-box lease %s acquired a claim during creation; retaining resource", leaseID)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		server := boxToServer(cfg, box, leaseID, slug, keep)
		*claim = LeaseClaim{LeaseID: leaseID, Slug: slug, Provider: providerName, ProviderScope: (Provider{}).ClaimScope(cfg),
			CloudID: box.ID, RepoRoot: repoRoot, ClaimedAt: now, LastUsedAt: now,
			IdleTimeoutSeconds: int(cfg.IdleTimeout.Seconds()), Labels: server.Labels}
		if err := persist(); err != nil {
			return err
		}
		published = *claim
		return nil
	})
	return published, err
}

// Generic SSH refreshes may update connection details, never ownership identity.
func (Provider) PrepareLeaseClaimEndpoint(existing LeaseClaim, provider, slug string, server Server, _ bool) (Server, error) {
	if provider != providerName || existing.Provider != providerName || slug != existing.Slug || server.CloudID != existing.CloudID ||
		server.Labels["lease"] != existing.LeaseID || server.Labels["slug"] != existing.Slug {
		return Server{}, exit(2, "refusing to retarget ascii-box lease %s", existing.LeaseID)
	}
	labels := shared.CloneLabels(server.Labels)
	for _, key := range []string{"provider", "box_id", "ascii_box_scope", boxCreationLabel, boxDeletionLabel, boxDeletionOperationLabel, boxDeletionOperationBindingLabel} {
		original := existing.Labels[key]
		if original != "" && labels[key] != "" && labels[key] != original {
			return Server{}, exit(2, "ascii-box lease %s changed %s", existing.LeaseID, key)
		}
		if original == "" {
			delete(labels, key)
		} else {
			labels[key] = original
		}
	}
	server.Labels = labels
	return server, nil
}

func (b *backend) ReleaseLeaseConnectionCleanupSafe() bool { return false }

func releaseClaimedBox(ctx context.Context, client api, claim LeaseClaim, beforeRelease func(boxData)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	nativeCompleted := false
	_, _, _, err := core.ResolveLeaseClaimAfterActionIfUnchanged(claim.LeaseID, claim, func() error {
		return releaseExactBox(ctx, client, boxFromClaim(claim), beforeRelease, func() {
			nativeCompleted = true
		})
	}, func(releaseErr error) (map[string]string, bool) {
		if nativeCompleted || releaseErr == nil {
			labels := shared.CloneLabels(claim.Labels)
			labels[boxDeletionLabel] = boxDeletionBinding(claim)
			return labels, releaseErr == nil
		}
		var incomplete *boxDeletionIncompleteError
		if !errors.As(releaseErr, &incomplete) || incomplete == nil || validateBoxDeletionOperation(incomplete.operation, claim.CloudID, "") != nil {
			return nil, false
		}
		existingOperation, err := boxDeletionOperationFromClaim(claim)
		if err != nil || existingOperation != "" && existingOperation != incomplete.operation.ID {
			return nil, false
		}
		labels := shared.CloneLabels(claim.Labels)
		delete(labels, boxDeletionLabel)
		labels[boxDeletionOperationLabel] = incomplete.operation.ID
		labels[boxDeletionOperationBindingLabel] = boxDeletionOperationBinding(claim, incomplete.operation.ID)
		return labels, false
	})
	return err
}

func releaseExactBox(ctx context.Context, client api, expected boxData, beforeRelease func(boxData), onDeletionCompleted func()) error {
	fresh, absent, err := exactBoxForRelease(ctx, client, expected)
	if err != nil {
		return err
	}
	if absent {
		return nil
	}
	if err := validateBoxIdentity(fresh, expected); err != nil {
		return err
	}
	validate := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		fresh, err := client.GetBox(ctx, expected.ID)
		if err != nil {
			return fmt.Errorf("ascii-box ownership lookup; retaining claim: %w", err)
		}
		return validateBoxIdentity(fresh, expected)
	}
	if err := validate(ctx); err != nil {
		return err
	}
	if beforeRelease != nil {
		fresh, err := client.GetBox(ctx, expected.ID)
		if err != nil {
			return err
		}
		if err := validateBoxIdentity(fresh, expected); err != nil {
			return err
		}
		beforeRelease(fresh)
	}
	if err := client.ReleaseBox(ctx, expected.ID, validate); err != nil {
		return err
	}
	if onDeletionCompleted != nil {
		onDeletionCompleted()
	}
	// A failed/pending delete can hide the Box from inventory. Only successful
	// native deletion completion followed by complete inventory is finalization.
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("ascii-box cleanup phase=inventory-confirmation; retaining claim: %w", err)
		}
		boxes, err := client.ListBoxes(ctx, true)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("ascii-box cleanup phase=inventory-confirmation; retaining claim: %w", ctxErr)
		}
		if err != nil {
			return fmt.Errorf("ascii-box cleanup phase=inventory-confirmation; retaining claim: %w", err)
		}
		found := false
		for _, box := range boxes {
			if box.ID == expected.ID {
				if err := validateBoxIdentity(box, expected); err != nil {
					return err
				}
				found = true
			}
		}
		if !found {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("ascii-box cleanup phase=inventory-confirmation; retaining claim: %w", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func exactBoxForRelease(ctx context.Context, client api, expected boxData) (boxData, bool, error) {
	if err := ctx.Err(); err != nil {
		return boxData{}, false, err
	}
	if expected.deletionOperationID != "" {
		operation, err := client.GetDeletionOperation(ctx, expected.ID, expected.deletionOperationID)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return boxData{}, false, fmt.Errorf("ascii-box cleanup phase=deletion-operation; retaining claim: %w", ctxErr)
		}
		if err != nil {
			return boxData{}, false, fmt.Errorf("ascii-box cleanup phase=deletion-operation lookup; retaining claim: %w", err)
		}
		if err := validateBoxDeletionOperation(operation, expected.ID, expected.deletionOperationID); err != nil {
			return boxData{}, false, err
		}
		if operation.Status != "completed" {
			return boxData{}, false, exit(2, "ascii-box cleanup phase=deletion-operation operation=%s last_observed_status=%s; retaining claim", operation.ID, operation.Status)
		}
		// Recheck the recorded operation inside the release fence; a reference
		// or an earlier resolution read is not completion authority.
		expected.deletionCompleted = true
	}
	fresh, err := client.GetBox(ctx, expected.ID)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return boxData{}, false, ctxErr
	}
	if err == nil {
		if expected.deletionCompleted {
			return boxData{}, false, exit(2, "ascii-box %s is still observable after recorded deletion completion; retaining claim", expected.ID)
		}
		return fresh, false, nil
	}
	if !isNotFound(err) {
		return boxData{}, false, fmt.Errorf("ascii-box ownership lookup; retaining claim: %w", err)
	}
	if !expected.deletionCompleted {
		return boxData{}, false, exit(2, "ascii-box %s has no completed native deletion witness; absence alone cannot prove deletion completion; retaining claim", expected.ID)
	}
	boxes, listErr := client.ListBoxes(ctx, true)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return boxData{}, false, fmt.Errorf("ascii-box cleanup phase=inventory-confirmation; retaining claim: %w", ctxErr)
	}
	if listErr != nil {
		return boxData{}, false, fmt.Errorf("ascii-box cleanup phase=inventory-confirmation; retaining claim: %w", listErr)
	}
	for _, box := range boxes {
		if box.ID != expected.ID {
			continue
		}
		if identityErr := validateBoxIdentity(box, expected); identityErr != nil {
			return boxData{}, false, identityErr
		}
		return boxData{}, false, fmt.Errorf("ascii-box ownership lookup; retaining claim: %w", err)
	}
	return boxData{}, true, nil
}
