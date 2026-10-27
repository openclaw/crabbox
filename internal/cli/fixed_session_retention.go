package cli

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"
)

// SessionDiskRetentionPolicy is immutable acquisition policy, not an extension
// of a legacy allocation deadline. It grants no compute by itself.
type SessionDiskRetentionPolicy struct {
	Mode                 string `json:"mode"`
	OwnerBinding         string `json:"ownerBinding"`
	ComputeWindowSeconds int    `json:"computeWindowSeconds"`
}

type RetainedComputeWindow struct {
	ID           string `json:"id"`
	AuthorizedAt string `json:"authorizedAt"`
	ExpiresAt    string `json:"expiresAt"`
	State        string `json:"state"`
}

type RetentionLifecycleOperation struct {
	ID                    string `json:"id"`
	Action                string `json:"action"`
	Phase                 string `json:"phase"`
	ExpectedClaimRevision string `json:"expectedClaimRevision"`
}

type SessionDiskRetentionState struct {
	DesiredState  string                       `json:"desiredState"`
	ComputeWindow RetainedComputeWindow        `json:"computeWindow"`
	Operation     *RetentionLifecycleOperation `json:"operation,omitempty"`
}

type RetainedProviderIdentity struct {
	LeaseID        string `json:"leaseId"`
	AttemptLeaseID string `json:"attemptLeaseId"`
	Slug           string `json:"slug"`
	ResourceID     string `json:"resourceId"`
}

type SessionDiskRetentionView struct {
	Identity RetainedProviderIdentity `json:"identity"`
	SessionDiskRetentionPolicy
	SessionDiskRetentionState
	Schema         string `json:"schema"`
	ClaimRevision  string `json:"claimRevision"`
	PolicyVerified bool   `json:"policyVerified"`
	ObservedState  string `json:"observedState"`
	StopPending    bool   `json:"stopPending"`
}

type RetentionLifecycleRequest struct {
	OperationID              string
	OwnerBinding             string
	ExpectedClaimRevision    string
	ExpectedProviderIdentity ProviderIdentityExpectation
}

func cloneFixedJournal(j *FixedLeaseJournal) *FixedLeaseJournal {
	if j == nil {
		return nil
	}
	copy := *j
	if j.Submission != nil {
		v := *j.Submission
		copy.Submission = &v
	}
	if j.Retention != nil {
		v := *j.Retention
		if v.Operation != nil {
			op := *v.Operation
			v.Operation = &op
		}
		copy.Retention = &v
	}
	return &copy
}

func initializeSessionDiskRetention(i *FixedCreateIntent, id string, now time.Time) error {
	p := i.Retention
	if p.Mode != "session-disk" || !FixedSHA256(p.OwnerBinding) || p.ComputeWindowSeconds <= 0 || int64(p.ComputeWindowSeconds) > int64((time.Duration(1<<63-1))/time.Second) {
		return fmt.Errorf("invalid session disk retention policy")
	}
	policy := *p
	i.Retention = &policy
	i.Journal.Version = 2 // Existing v1 decoders refuse before any provider operation.
	i.Journal.Retention = &SessionDiskRetentionState{DesiredState: "running", ComputeWindow: RetainedComputeWindow{ID: id, AuthorizedAt: now.UTC().Format(time.RFC3339Nano), ExpiresAt: now.Add(time.Duration(p.ComputeWindowSeconds) * time.Second).UTC().Format(time.RFC3339Nano), State: "active"}}
	return validateSessionDiskRetention(i)
}

func validateSessionDiskRetention(i *FixedCreateIntent) error {
	if i.Retention == nil {
		if i.Journal != nil && (i.Journal.Version != 1 || i.Journal.Retention != nil) {
			return Exit(4, "unsupported fixed retention journal")
		}
		return nil
	}
	p, j := i.Retention, i.Journal
	if p.Mode != "session-disk" || !FixedSHA256(p.OwnerBinding) || p.ComputeWindowSeconds <= 0 || int64(p.ComputeWindowSeconds) > int64(time.Duration(1<<63-1)/time.Second) || j == nil || j.Version != 2 || j.Retention == nil {
		return Exit(4, "invalid retained lease policy or journal")
	}
	s := j.Retention
	if s.DesiredState != "running" && s.DesiredState != "suspended" && s.DesiredState != "retired" {
		return Exit(4, "invalid retained lease disposition")
	}
	w := s.ComputeWindow
	start, e1 := time.Parse(time.RFC3339Nano, w.AuthorizedAt)
	end, e2 := time.Parse(time.RFC3339Nano, w.ExpiresAt)
	if w.ID == "" || e1 != nil || e2 != nil || !end.After(start) || end.Sub(start) != time.Duration(p.ComputeWindowSeconds)*time.Second || (w.State != "active" && w.State != "closed") {
		return Exit(4, "invalid retained compute window")
	}
	if s.DesiredState != "running" && w.State != "closed" {
		return Exit(4, "retained suspended or retired window is not closed")
	}
	if op := s.Operation; op != nil {
		if !validRetentionOperationID(op.ID) || (op.Action != "pause" && op.Action != "resume") || (op.Phase != "pending" && op.Phase != "observed") || op.ExpectedClaimRevision == "" {
			return Exit(4, "invalid retained lifecycle operation")
		}
	}
	return nil
}

func validRetentionOperationID(id string) bool {
	return len(id) > 0 && len(id) <= 128 && strings.TrimSpace(id) == id && strings.IndexFunc(id, func(r rune) bool { return r < 33 || r > 126 }) < 0
}

// SessionDiskExecutionDeadline admits work only in an already authorized
// window. Neither observation, activity nor a heartbeat can open another one.
func SessionDiskExecutionDeadline(claim LeaseClaim, now time.Time) (time.Time, error) {
	if claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != "acquired" || claim.CloudID == "" {
		return time.Time{}, Exit(4, "retained allocation is not acquired")
	}
	return SessionDiskWindowDeadline(claim, now)
}

// SessionDiskWindowDeadline also gates preparation of the original allocation.
func SessionDiskWindowDeadline(claim LeaseClaim, now time.Time) (time.Time, error) {
	i := claim.FixedCreateIntent
	if i == nil || i.Retention == nil {
		return time.Time{}, Exit(4, "lease has no session disk policy")
	}
	if err := validateFixedJournal(i); err != nil {
		return time.Time{}, err
	}
	s := i.Journal.Retention
	end, _ := time.Parse(time.RFC3339Nano, s.ComputeWindow.ExpiresAt)
	start, _ := time.Parse(time.RFC3339Nano, s.ComputeWindow.AuthorizedAt)
	if i.Journal.Phase == "deleting" || i.Journal.Phase == "released" || s.DesiredState != "running" || s.ComputeWindow.State != "active" || (s.Operation != nil && s.Operation.Phase != "observed") || !end.After(now) || now.Before(start) {
		return time.Time{}, Exit(4, "retained compute window is closed, expired or uncertain; explicit lifecycle recovery required")
	}
	return end, nil
}

// MutateFixedRetention extends the fixed claim transaction owner. Observations
// attest the exact resource and normalize its state. Reassertions converge the
// same durable operation on a stable opposite state, never minting a window.
func MutateFixedRetention(ctx context.Context, kind FixedLeaseKind, expected LeaseClaim, req RetentionLifecycleRequest, action string, now func() time.Time, observe func(context.Context, LeaseClaim) (string, error), submit func(context.Context, LeaseClaim, string) error) error {
	if now == nil {
		now = time.Now
	}
	if err := ValidateRetentionLifecycleRequest(expected, req, action); err != nil {
		return err
	}
	if op := expected.FixedCreateIntent.Journal.Retention.Operation; op != nil && op.ID == req.OperationID && op.Phase == "observed" {
		// Completed replays are observations, not mutations. Share the activity
		// fence so a duplicate resume neither cancels nor waits for admitted work.
		return WithLeaseClaimUnchangedShared(ctx, expected.LeaseID, expected, func() error {
			if !kind.IsFixedClaim(expected) || expected.FixedCreateIntent.Version != kind.IntentVersion {
				return Exit(4, "retained claim changed; inspect and retry")
			}
			state, err := observe(ctx, expected)
			if err != nil {
				return err
			}
			target := "suspended"
			var admissionErr error
			if action == "resume" {
				target = "running"
				_, admissionErr = SessionDiskExecutionDeadline(expected, now())
			}
			if state != target || admissionErr != nil {
				return Exit(4, "observed lifecycle no longer has an active target; inspect before new admission")
			}
			return nil
		})
	}
	return WithDurableLeaseClaimLockContext(ctx, expected.LeaseID, func(claim *LeaseClaim, exists bool, persist func() error) error {
		if !exists || !reflect.DeepEqual(*claim, expected) || !kind.IsFixedClaim(*claim) || claim.FixedCreateIntent.Version != kind.IntentVersion {
			return Exit(4, "retained claim changed; inspect and retry")
		}
		i := claim.FixedCreateIntent
		if i.State != "acquired" || i.Journal.Phase == "deleting" || claim.RecoveryHold != nil {
			return Exit(4, "retained ownership is unavailable for lifecycle admission")
		}
		s := i.Journal.Retention
		op := s.Operation
		replay := op != nil && op.ID == req.OperationID
		state, err := observe(ctx, *claim)
		if err != nil {
			return err
		}
		target := "suspended"
		if action == "resume" {
			target = "running"
		}
		expiredResume := func() bool {
			end, _ := time.Parse(time.RFC3339Nano, s.ComputeWindow.ExpiresAt)
			return action == "resume" && (!end.After(now()) || s.ComputeWindow.State == "closed")
		}
		if replay && op.Phase == "observed" {
			if state != target || expiredResume() {
				return Exit(4, "observed lifecycle no longer has an active target; inspect before new admission")
			}
			return nil
		}
		record := func() error { i.Journal.Revision++; return persist() }
		if !replay {
			if action == "resume" && (state != "suspended" || s.ComputeWindow.State != "closed") {
				return Exit(4, "resume requires a confirmed suspended resource and closed window")
			}
			s.DesiredState = target
			s.Operation = &RetentionLifecycleOperation{ID: req.OperationID, Action: action, Phase: "pending", ExpectedClaimRevision: req.ExpectedClaimRevision}
			s.ComputeWindow.State = "closed"
			if action == "resume" {
				at := now().UTC()
				s.ComputeWindow = RetainedComputeWindow{ID: req.OperationID, AuthorizedAt: at.Format(time.RFC3339Nano), ExpiresAt: at.Add(time.Duration(i.Retention.ComputeWindowSeconds) * time.Second).Format(time.RFC3339Nano), State: "active"}
			}
			if err := record(); err != nil {
				return err
			}
		}
		submitted := false
		for {
			expired := expiredResume()
			if expired {
				if target != "suspended" {
					submitted = false
				}
				target = "suspended"
				if s.DesiredState != "suspended" || s.ComputeWindow.State != "closed" {
					s.DesiredState = "suspended"
					s.ComputeWindow.State = "closed"
					if err := record(); err != nil {
						return err
					}
				}
			}
			if state == target {
				s.Operation.Phase = "observed"
				if err := record(); err != nil {
					return err
				}
				if expired {
					return Exit(4, "resume window expired; suspension confirmed before any new admission")
				}
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			// Transitional/unknown observations never authorize another mutation.
			if !submitted && (state == "running" || state == "suspended") {
				if err := record(); err != nil {
					return err
				}
				if err := submit(ctx, *claim, target); err != nil {
					return err
				}
				submitted = true
			}
			state, err = observe(ctx, *claim)
			if err != nil {
				return err
			}
			if state == target {
				continue
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	})
}

// ValidateSessionDiskRetentionClaim is also used by read-only provider status.
func ValidateSessionDiskRetentionClaim(claim LeaseClaim) error {
	return validateFixedJournal(claim.FixedCreateIntent)
}

// ValidateRetentionLifecycleRequest runs before transient activity cancellation;
// an obsolete or mismatched caller must not interrupt the current owner.
func ValidateRetentionLifecycleRequest(claim LeaseClaim, req RetentionLifecycleRequest, action string) error {
	if !validRetentionOperationID(req.OperationID) || req.ExpectedClaimRevision == "" || !IsCanonicalLeaseID(claim.LeaseID) || req.ExpectedProviderIdentity.LeaseID != claim.LeaseID || req.ExpectedProviderIdentity.AttemptLeaseID != claim.LeaseID || req.ExpectedProviderIdentity.Slug != claim.Slug || req.ExpectedProviderIdentity.ResourceID == "" {
		return Exit(2, "retained lifecycle requires operation, revision and complete exact provider identity")
	}
	if action != "pause" && action != "resume" {
		return Exit(2, "invalid retention lifecycle action")
	}
	i := claim.FixedCreateIntent
	if i == nil || i.Retention == nil || i.Retention.OwnerBinding != req.OwnerBinding {
		return Exit(4, "retained lifecycle owner mismatch")
	}
	if err := validateFixedJournal(i); err != nil {
		return err
	}
	if i.State != "acquired" || i.Journal.Phase == "deleting" || claim.RecoveryHold != nil {
		return Exit(4, "retained ownership is unavailable for lifecycle admission")
	}
	if err := ValidateLeaseTargetProviderIdentity(LeaseTarget{LeaseID: claim.LeaseID, Server: Server{CloudID: claim.CloudID, ImmutableID: claim.CloudImmutableID, Name: claim.Slug, Labels: map[string]string{"slug": claim.Slug}}}, req.ExpectedProviderIdentity); err != nil {
		return err
	}
	op := i.Journal.Retention.Operation
	if op != nil && op.ID == req.OperationID {
		if op.Action != action || op.ExpectedClaimRevision != req.ExpectedClaimRevision {
			return Exit(4, "retained operation identity conflict")
		}
		return nil
	}
	if claim.Revision != req.ExpectedClaimRevision || (op != nil && op.Phase == "pending") {
		return Exit(4, "retained lifecycle revision conflict")
	}
	return nil
}

// Legacy workspace-controller expiry means destructive release. It must not
// interpret a retained work-window deadline as permission to delete a disk.
func refuseControllerRetainedLease(id string) error {
	if os.Getenv(controllerProcessTreeOwnedEnv) != "1" {
		return nil
	}
	claim, exists, err := ResolveLeaseClaim(id)
	if err != nil {
		return err
	}
	if exists && claim.FixedCreateIntent != nil && claim.FixedCreateIntent.Retention != nil {
		return Exit(4, "workspace controller refuses retained session disk; use its native lifecycle owner")
	}
	return nil
}

// ControllerOwnsLeaseLifecycle identifies the legacy controller command path.
// Providers can refuse unsupported retained acquisition before external effects.
func ControllerOwnsLeaseLifecycle() bool {
	return controllerAcquireIdentityAcknowledgmentConfigured() || os.Getenv(controllerProcessTreeOwnedEnv) == "1"
}
