package azuresandbox

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	core "github.com/openclaw/crabbox/internal/cli"
)

var errSandboxOwnership = errors.New("ACA Sandbox exact ownership mismatch")

type sandboxOwnershipError string

func (e sandboxOwnershipError) Error() string { return string(e) }
func (e sandboxOwnershipError) Unwrap() error { return errSandboxOwnership }

var leaseKind = core.FixedLeaseKind{ClaimProvider: providerName, IntentVersion: 1, Label: "ACA Sandbox"}

type backend struct {
	cfg core.Config
	rt  core.Runtime
	api *client
}

func (*backend) Spec() core.ProviderSpec        { return (Provider{}).Spec() }
func (*backend) SupportsRequestedLeaseID() bool { return true }

func (b *backend) connect() (*client, error) {
	if b.api != nil {
		return b.api, nil
	}
	var err error
	b.api, err = newAPI(b.cfg, b.rt)
	return b.api, err
}

var newAPI = func(cfg core.Config, rt core.Runtime) (*client, error) {
	c := cfg.AzureSandbox
	var credential azcore.TokenCredential
	var err error
	if c.ClientID != "" {
		credential, err = azidentity.NewManagedIdentityCredential(&azidentity.ManagedIdentityCredentialOptions{ID: azidentity.ClientID(c.ClientID)})
	} else {
		credential, err = azidentity.NewAzureCLICredential(nil)
	}
	if err != nil {
		return nil, fmt.Errorf("ACA Sandbox allocator credential unavailable")
	}
	var transport http.RoundTripper
	if rt.HTTP != nil {
		transport = rt.HTTP.Transport
	}
	return newClient(c.Region, c.Subscription, c.ResourceGroup, c.Group, credential, transport)
}

func resourceBinding(s sandbox) core.FixedResourceBinding {
	return core.FixedResourceBinding{CloudID: s.ID, ImmutableID: s.ID, Labels: s.Labels}
}

func isMissing(err error) bool { var e *apiError; return errors.As(err, &e) && e.Status == 404 }

// Azure limits label values to 63 characters. Keep the full intent fingerprint
// in the journal and bind its digest using a 52-character, label-safe encoding.
func fingerprintLabel(fingerprint string) string {
	digest := sha256.Sum256([]byte(fingerprint))
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:])
}

func (b *backend) observe(ctx context.Context, tx *core.FixedTransaction, mode core.FixedObserveMode) (core.FixedObservation[sandbox], error) {
	var out core.FixedObservation[sandbox]
	claim := tx.Claim
	if claim.ProviderScope != (Provider{}).ClaimScope(b.cfg) {
		return out, sandboxOwnershipError("ACA Sandbox group differs from lease owner")
	}
	c, err := b.connect()
	if err != nil {
		return out, err
	}
	var candidates []sandbox
	if claim.CloudID != "" {
		s, err := c.Get(ctx, claim.CloudID)
		if isMissing(err) {
			out.AbsenceProven = mode == core.FixedObserveDelete
			return out, nil
		}
		if err != nil {
			return out, err
		}
		candidates = []sandbox{s}
	} else {
		// An empty inventory cannot settle an admitted create. A late resource
		// remains owned by the persisted attempt and can be adopted on replay.
		all, err := c.List(ctx)
		if err != nil {
			return out, err
		}
		for _, s := range all {
			if s.Labels["crabbox_lease"] == claim.LeaseID {
				candidates = append(candidates, s)
			}
		}
		out.CanSubmit = len(claim.FixedCreateIntent.Attempt) == 0 || claim.FixedCreateIntent.Attempt["submission"] == "pending"
		out.AbsenceProven = out.CanSubmit && mode == core.FixedObserveDelete
	}
	for _, s := range candidates {
		if s.ID == "" || s.Labels["crabbox_lease"] != claim.LeaseID || s.Labels["crabbox_fingerprint"] != fingerprintLabel(claim.FixedCreateIntent.Fingerprint) || s.Labels["crabbox_attempt"] == "" || s.Labels["crabbox_attempt"] != claim.FixedCreateIntent.Attempt["nonce"] {
			return out, sandboxOwnershipError("ACA Sandbox ownership does not match the exact lease attempt")
		}
		// A bad restore is unusable, but exact ownership still permits explicit cleanup.
		if snapshotID := claim.FixedCreateIntent.Attempt["snapshot_id"]; snapshotID != "" && mode != core.FixedObserveDelete {
			if s.SourcesRef.Snapshot == nil || s.SourcesRef.Snapshot.ID != snapshotID || s.SourcesRef.DiskImage == nil || s.SourcesRef.DiskImage.ID != claim.FixedCreateIntent.Attempt["source_disk_id"] {
				return out, sandboxOwnershipError("ACA Sandbox restored source differs from its exact lease attempt")
			}
		}
	}
	out.Candidates = candidates
	if len(candidates) == 1 {
		binding := resourceBinding(candidates[0])
		out.Binding = &binding
	}
	return out, nil
}

func (b *backend) acquire(ctx context.Context, req core.FixedWarmupRequest) (core.LeaseTarget, error) {
	retention, err := b.retentionPolicy()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if retention != nil && core.ControllerOwnsLeaseLifecycle() {
		return core.LeaseTarget{}, fmt.Errorf("controller does not support session disk retention; use the native retention lifecycle owner")
	}
	if retention != nil && req.Reclaim {
		return core.LeaseTarget{}, fmt.Errorf("retained session disks cannot be reclaimed into another owner")
	}
	if b.cfg.AzureSandbox.DiskID != "" && b.cfg.AzureSandbox.Disk != "" && b.cfg.AzureSandbox.Disk != core.AzureSandboxConfigDefaultDisk {
		return core.LeaseTarget{}, fmt.Errorf("ACA Sandbox private disk ID cannot be combined with a public disk override")
	}
	snapshot, err := b.snapshotBinding()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	c, err := b.connect()
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if req.ActionsRunner || !req.Keep {
		return core.LeaseTarget{}, fmt.Errorf("ACA Sandbox warmup requires --keep and does not support Actions runners")
	}
	if b.cfg.TTL <= 0 || b.cfg.IdleTimeout <= 0 {
		return core.LeaseTarget{}, fmt.Errorf("ACA Sandbox requires finite positive TTL and idle timeout")
	}
	cfg := b.cfg.AzureSandbox
	return core.AcquireFixedResource(ctx, core.FixedAcquireOptions{
		Now: b.now, Retention: retention, Kind: leaseKind, LeaseID: req.RequestedLeaseID, RepoRoot: req.Repo.Root,
		Reclaim: req.Reclaim, TargetOS: core.TargetLinux, TTL: b.cfg.TTL, IdleTimeout: b.cfg.IdleTimeout,
	}, core.FixedLeaseOperations[sandbox]{
		Admission: &core.FixedAdmission{PendingKey: "submission", PendingValue: "pending", SubmittedValue: "submitted"},
		// Read-only prerequisites must not consume the attempt's create admission.
		DeferredAdmission: true,
		DescribeIntent: func(_ context.Context, _ *core.LeaseClaim, _ bool) (core.FixedLeaseBinding, error) {
			fingerprint, err := core.FixedIntentFingerprint("aca-sandbox-v1", struct {
				Config    core.AzureSandboxConfig
				TTL, Idle time.Duration
				Slug      string
			}{cfg, b.cfg.TTL, b.cfg.IdleTimeout, req.RequestedSlug})
			return core.FixedLeaseBinding{ProviderScope: (Provider{}).ClaimScope(b.cfg), Fingerprint: fingerprint, AllocateSlug: true, RequestedSlug: req.RequestedSlug}, err
		},
		Plan: func(_ context.Context, claim core.LeaseClaim) (core.FixedAttemptPlan, error) {
			values := map[string]string{"submission": "pending", "expires_at": b.now().Add(b.cfg.TTL).UTC().Format(time.RFC3339Nano)}
			if snapshot != nil {
				values["snapshot_id"], values["source_disk_id"] = snapshot.SnapshotID, snapshot.SourceDiskID
			}
			return core.FixedAttemptPlan{Values: values, NonceBytes: 16, NonceKey: "nonce", NonceLabel: "crabbox_attempt", Labels: map[string]string{"crabbox_lease": claim.LeaseID, "crabbox_fingerprint": fingerprintLabel(claim.FixedCreateIntent.Fingerprint)}}, nil
		},
		ObserveExact: b.observe,
		Submit: func(ctx context.Context, tx *core.FixedTransaction) (sandbox, error) {
			if retention != nil {
				deadline, err := core.SessionDiskWindowDeadline(*tx.Claim, b.now())
				if err != nil {
					return sandbox{}, err
				}
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, deadline)
				defer cancel()
			}
			if err := b.verifySnapshotSource(ctx, snapshot); err != nil {
				return sandbox{}, err
			}
			// Read-only prerequisites cannot authorize creation after the window.
			if retention != nil {
				if _, err := core.SessionDiskWindowDeadline(*tx.Claim, b.now()); err != nil {
					return sandbox{}, err
				}
			}
			body := createRequest{Resources: map[string]string{"cpu": "2000m", "memory": "4096Mi"}, Labels: maps.Clone(tx.Claim.Labels), Lifecycle: map[string]any{
				"autoSuspendPolicy": map[string]any{"enabled": true, "interval": int(b.cfg.IdleTimeout.Seconds()), "mode": "Disk"},
				"autoDeletePolicy":  map[string]any{"enabled": retention == nil, "deleteIntervalInSeconds": int(b.cfg.TTL.Seconds())},
			}}
			if snapshot != nil {
				// CLI/server-qualified restore accepts fresh labels. Do not send
				// unqualified environment, resource or lifecycle overrides.
				body.SourcesRef.Snapshot = &snapshotReference{ID: snapshot.SnapshotID}
				body.Resources, body.Lifecycle = nil, nil
			} else if cfg.DiskID != "" {
				body.SourcesRef.DiskImage = &diskReference{ID: cfg.DiskID}
			} else {
				body.SourcesRef.DiskImage = &diskReference{Name: cfg.Disk, IsPublic: true}
			}
			if err := ctx.Err(); err != nil {
				return sandbox{}, err
			}
			if err := tx.Admit(); err != nil {
				return sandbox{}, err
			}
			s, err := c.Create(ctx, body)
			if err != nil {
				return s, err
			}
			if err := tx.Observe(resourceBinding(s)); err != nil {
				return s, err
			}
			return s, nil
		},
		PrepareAccess: func(ctx context.Context, tx *core.FixedTransaction, s sandbox) (core.LeaseTarget, error) {
			var deadline time.Time
			var err error
			if retention != nil {
				deadline, err = core.SessionDiskWindowDeadline(*tx.Claim, b.now())
				if err != nil {
					return core.LeaseTarget{}, fmt.Errorf("%w; inspect the same lease before explicit pause/resume recovery", err)
				}
			} else {
				deadline, err = time.Parse(time.RFC3339Nano, tx.Claim.FixedCreateIntent.Attempt["expires_at"])
				if err != nil || !deadline.After(b.now()) {
					return core.LeaseTarget{}, fmt.Errorf("ACA Sandbox allocation expired; release the lease")
				}
			}
			ctx, cancel := context.WithDeadline(ctx, deadline)
			defer cancel()
			// Readback and lifecycle policy precede any access preparation.
			if snapshot != nil {
				policy := snapshot.Lifecycle
				if retention != nil {
					policy.AutoDelete.Enabled = false
				}
				if err := b.configureSnapshotLifecycle(ctx, tx, s, policy); err != nil {
					return core.LeaseTarget{}, err
				}
			}
			if retention != nil {
				if err := b.verifyRetentionPolicy(*tx.Claim, s); err != nil {
					// A setter is reconciled by independent observation, never create reply.
					observed, e := b.inspect(ctx, *tx.Claim)
					if e != nil {
						return core.LeaseTarget{}, e
					}
					if e = b.verifyRetentionPolicy(*tx.Claim, observed); e != nil {
						return core.LeaseTarget{}, e
					}
				}
			}
			s, err = b.ensureRunning(ctx, *tx.Claim)
			if err != nil {
				return core.LeaseTarget{}, err
			}
			return core.LeaseTarget{LeaseID: tx.Claim.LeaseID, Server: core.Server{Provider: providerName, CloudID: s.ID, ImmutableID: s.ID, Labels: s.Labels, Status: s.State}}, nil
		},
	})
}
