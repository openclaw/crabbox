package cli

import (
	"context"
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
	replace       func(context.Context, string, string, func() error) error
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
	replace := replaceClaimFile
	write := tx.write
	if write == nil {
		write = func(path string, claim leaseClaim) error {
			return writeLeaseClaimAtomicWithReplace(path, claim, func(dir string) error {
				fsyncDir(dir)
				return nil
			}, replace)
		}
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
			if err := writeLeaseClaimAtomicWithReplace(path, claim, syncDirectory, replace); err != nil {
				return err
			}
			return syncCreatedClaimDirectoryParentsWithSync(filepath.Dir(path), firstExistingDir, syncDirectory)
		}
	}
	if tx.directory != claimDirectoryExisting {
		if err := makePrivateClaimDirectories(filepath.Dir(path)); err != nil {
			return leaseClaim{}, Exit(2, "create claim directory: %v", err)
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
		replace = func(tmpPath, path string) error {
			publish := tx.replace
			if publish == nil {
				publish = replaceClaimFileWithRetry
			}
			return publish(ctx, tmpPath, path, func() error {
				current, present, err := readLeaseClaimPathWithPresence(path)
				if err != nil {
					return err
				}
				return unchangedLeaseClaimGuard(leaseID, original, exists)(current, present)
			})
		}
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
	cacheVolumesOverride *[]string
	context              context.Context
	endpoint             leaseClaimEndpointMode
	directory            claimDirectoryPolicy
	action               func() error
	idle                 claimIdlePolicy
}
