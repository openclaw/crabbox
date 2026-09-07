package cli

import (
	"context"
	"os"
	"path/filepath"
)

// Revision timing is observable by provider endpoint preparation. Ordinary
// mutations refresh before transformation; action-produced endpoints refresh
// only after the action elects to publish and preparation succeeds.
type claimRevisionPhase uint8

const (
	claimRevisionBeforeAction claimRevisionPhase = iota
	claimRevisionBeforeMutation
	claimRevisionAfterMutation
)

type claimDirectoryPolicy uint8

const (
	claimDirectoryExisting claimDirectoryPolicy = iota
	claimDirectoryCreate
	claimDirectoryDurableNamespace
)

type claimPublicationPolicy uint8

const (
	claimPublish claimPublicationPolicy = iota
	claimSkipEmpty
)

type claimActionDecision uint8

const (
	claimActionContinue claimActionDecision = iota
	claimActionKeepSnapshot
)

type leaseClaimTransaction struct {
	context       context.Context
	guard         func(leaseClaim, bool) error
	action        func() (claimActionDecision, error)
	mutate        func(*leaseClaim) error
	revision      claimRevisionPhase
	directory     claimDirectoryPolicy
	publication   claimPublicationPolicy
	write         func(string, leaseClaim) error
	syncDirectory func(string) error
}

// transactLeaseClaim owns the single-publication claim fence. Actions must not
// reenter claim operations for this ID: the lock spans guard, action, revision,
// transformation, atomic replacement, and required directory syncs. On write
// failure the returned candidate is not proof of publication or durability;
// rename may already have succeeded. Callers must honor the error.
func transactLeaseClaim(leaseID string, tx leaseClaimTransaction) (leaseClaim, error) {
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		return leaseClaim{}, err
	}
	write := tx.write
	if write == nil {
		write = writeLeaseClaimAtomic
	}
	if tx.directory == claimDirectoryDurableNamespace {
		// Capture the boundary before lock-directory creation can create ancestors.
		firstExistingDir, err := nearestExistingClaimDirectory(filepath.Dir(path))
		if err != nil {
			return leaseClaim{}, err
		}
		syncDirectory := tx.syncDirectory
		if syncDirectory == nil {
			syncDirectory = syncControllerDirectory
		}
		write = func(path string, claim leaseClaim) error {
			return writeLeaseClaimAtomicDurableWithSync(path, claim, firstExistingDir, syncDirectory)
		}
	}
	if tx.directory != claimDirectoryExisting {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return leaseClaim{}, exit(2, "create claim directory: %v", err)
		}
	}
	var updated leaseClaim
	ctx := tx.context
	if ctx == nil {
		ctx = context.Background()
	}
	err = withLeaseClaimLockContext(ctx, path, false, func() error {
		claim, exists, err := readLeaseClaimPathWithPresence(path)
		if err != nil {
			return err
		}
		if err := validateLeaseClaimFileIdentity(leaseID, claim, exists); err != nil {
			return err
		}
		if tx.guard != nil {
			if err := tx.guard(claim, exists); err != nil {
				return err
			}
		}
		original := cloneLeaseClaim(claim)
		if tx.revision == claimRevisionBeforeAction {
			if err := refreshLeaseClaimRevision(&claim); err != nil {
				return err
			}
		}
		if tx.action != nil {
			decision, err := tx.action()
			if decision == claimActionKeepSnapshot {
				updated = original
				return err
			}
			if err != nil {
				return err
			}
		}
		if tx.revision == claimRevisionBeforeMutation {
			if err := refreshLeaseClaimRevision(&claim); err != nil {
				return err
			}
		}
		if err := tx.mutate(&claim); err != nil {
			return err
		}
		if tx.publication == claimSkipEmpty && claim.LeaseID == "" {
			return nil
		}
		if tx.revision == claimRevisionAfterMutation {
			if err := refreshLeaseClaimRevision(&claim); err != nil {
				return err
			}
		}
		updated = cloneLeaseClaim(claim)
		return write(path, claim)
	})
	return updated, err
}

func claimTransactionAction(action func() error) func() (claimActionDecision, error) {
	if action == nil {
		return nil
	}
	return func() (claimActionDecision, error) { return claimActionContinue, action() }
}

type leaseClaimEndpointMode uint8

const (
	claimEndpointUpdate leaseClaimEndpointMode = iota
	claimEndpointReplace
)

type leaseClaimMetadataPolicy uint8

const (
	claimMetadataPreserve leaseClaimMetadataPolicy = iota
	claimMetadataAdmitProvider
)

type leaseClaimEndpointPolicy struct {
	mode     leaseClaimEndpointMode
	metadata leaseClaimMetadataPolicy
}

type leaseClaimTargetOptions struct {
	context   context.Context
	endpoint  leaseClaimEndpointMode
	directory claimDirectoryPolicy
	action    func() error
}
