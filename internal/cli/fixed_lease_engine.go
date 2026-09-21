package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"time"
)

// FixedLeaseJournal records engine progress alongside the original native
// attempt dialect. Native submission/deletion witnesses remain authoritative;
// in particular, a legacy empty attempt is not proof of non-submission.
type FixedLeaseJournal struct {
	Version  int    `json:"version"`
	Phase    string `json:"phase"`
	Revision uint64 `json:"revision"`
}

// FixedIntentFingerprint hashes a provider's canonical schema without changing
// its JSON encoding or domain prefix, both of which are persisted contracts.
func FixedIntentFingerprint(domain string, intent any) (string, error) {
	data, err := json.Marshal(intent)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(append([]byte(domain), data...))), nil
}

// FixedTransaction is valid only while core holds the durable claim lock.
// Adapters assemble native evidence in Claim; Record checks immutable custody
// before publishing it. An error never clears the last durable attempt.
type FixedTransaction struct {
	Claim       *LeaseClaim
	Fresh       bool
	initial     FixedCreateIntent
	cloudID     string
	immutableID string
	leaseID     string
	provider    string
	attempt     map[string]string
	failures    []string
	persist     func() error
	admission   *FixedAdmission
}

func newFixedTransaction(claim *LeaseClaim, fresh bool, persist func() error) (*FixedTransaction, error) {
	if err := validateFixedJournal(claim.FixedCreateIntent); err != nil {
		return nil, err
	}
	return &FixedTransaction{Claim: claim, Fresh: fresh, initial: *claim.FixedCreateIntent,
		cloudID: claim.CloudID, immutableID: claim.CloudImmutableID, leaseID: claim.LeaseID, provider: claim.Provider,
		attempt: maps.Clone(claim.FixedCreateIntent.Attempt), failures: append([]string(nil), claim.FixedCreateIntent.FailedAttempts...), persist: persist}, nil
}

func validateFixedJournal(intent *FixedCreateIntent) error {
	if intent == nil {
		return Exit(4, "lease_id_conflict: missing fixed create intent")
	}
	if j := intent.Journal; j != nil {
		if j.Version != 1 || j.Revision == 0 {
			return Exit(4, "lease_id_conflict: unsupported fixed lease journal")
		}
		switch j.Phase {
		case "prepared", "observed", "submitting", "bound", "acquired", "deleting", "released":
		default:
			return Exit(4, "lease_id_conflict: invalid fixed lease journal phase %q", j.Phase)
		}
	}
	return nil
}

func (tx *FixedTransaction) Record(phase string) error {
	i := tx.Claim.FixedCreateIntent
	if tx.Claim.LeaseID != tx.leaseID || tx.Claim.Provider != tx.provider || tx.Claim.Slug != tx.initial.Slug || i == nil || i.Version != tx.initial.Version || i.Fingerprint != tx.initial.Fingerprint ||
		i.ProviderScope != tx.initial.ProviderScope || i.CheckpointID != tx.initial.CheckpointID ||
		i.Slug != tx.initial.Slug || i.CreatedAt != tx.initial.CreatedAt {
		return Exit(4, "lease_id_conflict: fixed intent changed during transaction")
	}
	if err := validateFixedJournal(i); err != nil {
		return err
	}
	if phase == "submitting" && (i.State != "prepared" || len(i.Attempt) == 0) {
		return Exit(4, "lease_id_conflict: submission has no prepared native attempt")
	}
	if (tx.cloudID != "" && tx.Claim.CloudID != tx.cloudID) ||
		(tx.immutableID != "" && tx.Claim.CloudImmutableID != tx.immutableID) {
		return Exit(4, "lease_id_conflict: bound fixed resource identity changed during transaction")
	}
	// Re-observing an acquired identity renews access, not the create intent.
	if i.State == "acquired" && phase == "bound" {
		phase = "acquired"
	}
	revision := uint64(1)
	if i.Journal != nil {
		revision = i.Journal.Revision
		if i.Journal.Phase != phase || !maps.Equal(i.Attempt, tx.attempt) || !reflect.DeepEqual(i.FailedAttempts, tx.failures) {
			revision++
		}
	}
	i.Journal = &FixedLeaseJournal{Version: 1, Phase: phase, Revision: revision}
	if err := validateFixedJournal(i); err != nil {
		return err
	}
	if err := tx.persist(); err != nil {
		return err
	}
	tx.cloudID, tx.immutableID = tx.Claim.CloudID, tx.Claim.CloudImmutableID
	tx.attempt, tx.failures = maps.Clone(i.Attempt), append([]string(nil), i.FailedAttempts...)
	return nil
}

// FixedObservation carries only fully attested candidates. CanSubmit is native
// proof of non-submission (or safe same-identity resubmission), never inferred
// from an empty inventory. AbsenceProven applies only to release.
type FixedObservation[T any] struct {
	Candidates    []T
	CanSubmit     bool
	AbsenceProven bool
	Conflict      string
}

type FixedObserveMode uint8

const (
	FixedObserveAcquire FixedObserveMode = iota
	FixedObserveInspect
	FixedObserveDelete
)

// FixedLeaseOperations separates native evidence and effects from core's
// transaction. PlanAttempt only prepares evidence; Submit records admission at
// the native mutation boundary through tx.Record("submitting").
type FixedLeaseOperations[T any] struct {
	// Some APIs resolve prerequisite IDs inside submission. Each resolved
	// attempt must still be journaled before admitting the target allocation.
	PlanDuringSubmit bool
	Admission        *FixedAdmission
	Plan             func(context.Context, LeaseClaim) (FixedAttemptPlan, error)
	Identity         func(T) FixedResourceBinding
	// DeferredAdmission fences native prerequisites before recording submission.
	DeferredAdmission bool
	DescribeIntent    func(context.Context, *LeaseClaim, bool) (FixedLeaseBinding, error)
	PlanAttempt       func(context.Context, *FixedTransaction) error
	ObserveExact      func(context.Context, *FixedTransaction, FixedObserveMode) (FixedObservation[T], error)
	Submit            func(context.Context, *FixedTransaction) (T, error)
	PrepareAccess     func(context.Context, *FixedTransaction, T) (LeaseTarget, error)
	DeleteExact       func(context.Context, *FixedTransaction, T) error
}

// AcquireFixedResource keeps the existing claim envelope and all native
// fingerprint/attempt dialects readable while writing the engine journal.
func AcquireFixedResource[T any](ctx context.Context, opts FixedAcquireOptions, ops FixedLeaseOperations[T]) (LeaseTarget, error) {
	if ops.DescribeIntent == nil || (ops.PlanAttempt == nil && ops.Plan == nil) || ops.ObserveExact == nil || ops.Submit == nil || ops.PrepareAccess == nil {
		return LeaseTarget{}, fmt.Errorf("fixed lease engine requires all acquisition operations")
	}
	fresh := false
	opts.journal = true
	return AcquireFixedLease(opts, func(ctx context.Context, claim *LeaseClaim, exists bool) (FixedLeaseBinding, error) {
		fresh = !exists
		if exists {
			if err := validateFixedJournal(claim.FixedCreateIntent); err != nil {
				return FixedLeaseBinding{}, err
			}
			if j := claim.FixedCreateIntent.Journal; j != nil && (j.Phase == "deleting" || j.Phase == "released") {
				return FixedLeaseBinding{}, Exit(4, "lease_id_conflict: fixed lease has entered cleanup; retry stop")
			}
		}
		return ops.DescribeIntent(ctx, claim, exists)
	}, func(ctx context.Context, claim *LeaseClaim, _ *FixedCreateIntent, persist func() error) (LeaseTarget, error) {
		tx, err := newFixedTransaction(claim, fresh, persist)
		if err != nil {
			return LeaseTarget{}, err
		}
		tx.admission = ops.Admission
		observation, err := ops.ObserveExact(ctx, tx, FixedObserveAcquire)
		if err != nil {
			return LeaseTarget{}, err
		}
		if err := fixedObservationConflict(opts.Kind, claim.LeaseID, observation); err != nil {
			return LeaseTarget{}, err
		}
		var resource T
		if len(observation.Candidates) == 1 {
			resource = observation.Candidates[0]
		} else {
			if !observation.CanSubmit || (ops.Admission != nil && !ops.Admission.permits(tx)) || claim.FixedCreateIntent.State != "prepared" || claim.CloudID != "" || claim.CloudNumericID != 0 || claim.CloudImmutableID != "" {
				return LeaseTarget{}, Exit(4, "lease_id_conflict: fixed %s lease %s has an unresolved or missing resource; retain its claim for recovery", opts.Kind.Label, claim.LeaseID)
			}
			if ops.Plan != nil {
				plan, err := ops.Plan(ctx, *claim)
				if err != nil {
					return LeaseTarget{}, err
				}
				if err := ctx.Err(); err != nil {
					return LeaseTarget{}, err
				}
				if err := tx.plan(plan); err != nil {
					return LeaseTarget{}, err
				}
			} else if err := ops.PlanAttempt(ctx, tx); err != nil {
				return LeaseTarget{}, err
			}
			if len(claim.FixedCreateIntent.Attempt) == 0 && !ops.PlanDuringSubmit {
				return LeaseTarget{}, Exit(4, "lease_id_conflict: fixed lease submission has no native attempt")
			}
			if err := tx.Record("prepared"); err != nil {
				return LeaseTarget{}, err
			}
			if ops.Admission != nil && !ops.DeferredAdmission {
				if err := tx.Admit(); err != nil {
					return LeaseTarget{}, err
				}
			}
			resource, err = ops.Submit(ctx, tx)
			if err != nil {
				return LeaseTarget{}, err
			}
			if len(claim.FixedCreateIntent.Attempt) == 0 {
				return LeaseTarget{}, Exit(4, "lease_id_conflict: fixed %s lease %s has no valid durable launch attempt after provisioning", opts.Kind.Label, claim.LeaseID)
			}
		}
		if ops.Identity != nil {
			if err := tx.Bind(ops.Identity(resource)); err != nil {
				return LeaseTarget{}, err
			}
		}
		lease, err := ops.PrepareAccess(ctx, tx, resource)
		if err != nil {
			return LeaseTarget{}, err
		}
		if lease.LeaseID != claim.LeaseID || (claim.CloudID != "" && claim.CloudID != lease.Server.CloudID) ||
			(claim.CloudImmutableID != "" && claim.CloudImmutableID != lease.Server.ImmutableID) {
			return LeaseTarget{}, Exit(4, "lease_id_conflict: prepared access changed the fixed resource identity")
		}
		if err := tx.Record("bound"); err != nil {
			return LeaseTarget{}, err
		}
		return lease, nil
	}, ctx)
}

func fixedObservationConflict[T any](kind FixedLeaseKind, leaseID string, observation FixedObservation[T]) error {
	if observation.Conflict != "" {
		return Exit(4, "lease_id_conflict: %s", observation.Conflict)
	}
	if len(observation.Candidates) > 1 {
		resources := kind.ResourcePlural
		if resources == "" {
			resources = kind.Label + " resources"
		}
		return Exit(4, "lease_id_conflict: multiple %s match fixed lease %s", resources, leaseID)
	}
	return nil
}

// InspectFixedResource does not journal or prepare access. Provider observations
// cannot accidentally publish a binding through a read-only operation.
func InspectFixedResource[T any](ctx context.Context, kind FixedLeaseKind, claim LeaseClaim, ops FixedLeaseOperations[T]) (FixedObservation[T], error) {
	if !kind.IsFixedClaim(claim) || claim.FixedCreateIntent.Version != kind.IntentVersion || ops.ObserveExact == nil {
		return FixedObservation[T]{}, Exit(4, "lease_id_conflict: fixed inspection has no matching ownership dialect")
	}
	claim.Labels = maps.Clone(claim.Labels)
	if claim.FixedCreateIntent != nil {
		i := *claim.FixedCreateIntent
		i.Attempt = maps.Clone(i.Attempt)
		i.FailedAttempts = append([]string(nil), i.FailedAttempts...)
		if i.Journal != nil {
			journal := *i.Journal
			i.Journal = &journal
		}
		claim.FixedCreateIntent = &i
	}
	tx, err := newFixedTransaction(&claim, false, func() error { return Exit(4, "fixed lease inspection cannot mutate state") })
	if err != nil {
		return FixedObservation[T]{}, err
	}
	observed, err := ops.ObserveExact(ctx, tx, FixedObserveInspect)
	if err == nil {
		err = fixedObservationConflict(kind, claim.LeaseID, observed)
	}
	return observed, err
}

// DeleteFixedResource holds the same claim CAS across attestation, native
// deletion, and terminal publication. Native DeleteExact must attest completion,
// including any child resources, before returning nil.
func DeleteFixedResource[T any](ctx context.Context, kind FixedLeaseKind, expected LeaseClaim, ops FixedLeaseOperations[T], clock ...func() time.Time) error {
	if !kind.IsFixedClaim(expected) || expected.FixedCreateIntent.Version != kind.IntentVersion {
		return Exit(4, "lease_id_conflict: fixed deletion has no matching ownership dialect")
	}
	if ops.ObserveExact == nil || ops.DeleteExact == nil {
		return fmt.Errorf("fixed lease engine requires observation and exact deletion")
	}
	return WithDurableLeaseClaimLockContext(ctx, expected.LeaseID, func(claim *LeaseClaim, exists bool, persist func() error) error {
		if !exists || !reflect.DeepEqual(*claim, expected) {
			return Exit(4, "lease_id_conflict: fixed claim changed before release; retry")
		}
		tx, err := newFixedTransaction(claim, false, persist)
		if err != nil {
			return err
		}
		observed, err := ops.ObserveExact(ctx, tx, FixedObserveDelete)
		if err != nil {
			return err
		}
		if err := fixedObservationConflict(kind, claim.LeaseID, observed); err != nil {
			return err
		}
		if claim.FixedCreateIntent.State == "released" {
			if err := kind.ValidateTerminalClaim(*claim, expected, claim.LeaseID, nil); err != nil {
				return err
			}
			if kind.AfterTerminal != nil {
				return kind.AfterTerminal(*claim)
			}
			return nil
		}
		if len(observed.Candidates) == 0 {
			if !observed.AbsenceProven {
				return Exit(4, "lease_id_conflict: fixed %s absence is unverified; claim retained", kind.Label)
			}
		}
		if kind.DeletionState != "" {
			claim.FixedCreateIntent.State = kind.DeletionState
			if err := tx.Record("deleting"); err != nil {
				return err
			}
		}
		if len(observed.Candidates) != 0 {
			if err := ops.DeleteExact(ctx, tx, observed.Candidates[0]); err != nil {
				return err
			}
		}
		now := time.Now
		if len(clock) != 0 && clock[0] != nil {
			now = clock[0]
		}
		*claim = kind.TerminalClaim(*claim, now().UTC())
		if err := persist(); err != nil {
			return err
		}
		if kind.AfterTerminal != nil {
			return kind.AfterTerminal(*claim)
		}
		return nil
	})
}

func (tx *FixedTransaction) FailedAttemptSet() map[string]bool {
	failed := make(map[string]bool, len(tx.Claim.FixedCreateIntent.FailedAttempts))
	for _, token := range tx.Claim.FixedCreateIntent.FailedAttempts {
		failed[token] = true
	}
	return failed
}

// RejectAttempt requires a provider-certified definite failure. Transport
// uncertainty must never call this: it retains the current attempt instead.
func (tx *FixedTransaction) RejectAttempt(kind FixedLeaseKind, token string, terminal bool, now time.Time) error {
	intent := tx.Claim.FixedCreateIntent
	if !kind.IsFixedClaim(*tx.Claim) || intent.Version != kind.IntentVersion || intent.State != "prepared" || token == "" || tx.Claim.CloudID != "" || tx.Claim.CloudNumericID != 0 || tx.Claim.CloudImmutableID != "" {
		return Exit(4, "lease_id_conflict: cannot reject a bound or unidentified fixed attempt")
	}
	if terminal {
		*tx.Claim = kind.TerminalClaim(*tx.Claim, now)
		return tx.persist()
	}
	if !tx.FailedAttemptSet()[token] {
		intent.FailedAttempts = append(intent.FailedAttempts, token)
	}
	intent.Attempt = nil
	return tx.Record("prepared")
}

func finalizeFixedLeaseWithArtifacts(kind FixedLeaseKind, expected, terminal LeaseClaim, deleteExact func() error) error {
	return WithDurableLeaseClaimLock(expected.LeaseID, func(claim *LeaseClaim, exists bool, persist func() error) error {
		if !exists || !reflect.DeepEqual(*claim, expected) {
			return Exit(2, "lease %s claim changed; retry", expected.LeaseID)
		}
		if err := deleteExact(); err != nil {
			return err
		}
		*claim = terminal
		if err := persist(); err != nil {
			return err
		}
		return kind.AfterTerminal(*claim)
	})
}

func terminalFixedJournal(intent *FixedCreateIntent) {
	revision := uint64(1)
	if intent.Journal != nil {
		revision = intent.Journal.Revision + 1
	}
	intent.Journal = &FixedLeaseJournal{Version: 1, Phase: "released", Revision: revision}
}
