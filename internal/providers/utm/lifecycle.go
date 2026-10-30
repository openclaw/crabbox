package utm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Guest teardown shares the release claim fence: an SSH identity can remain
// valid after the same VM has been reclaimed or its claim generation advances.
func (*backend) ReleaseLeaseConnectionCleanupSafe() bool { return false }

func (b *backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	unlock, err := b.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := b.lookup(req.Lease.LeaseID)
	if err != nil {
		return err
	}
	if req.Lease.Server.Provider != "utm" || req.Lease.Server.CloudID != r.VMID {
		return fmt.Errorf("UTM release target does not match ownership journal")
	}
	if err = core.ValidateLeaseTargetProviderIdentity(req.Lease, req.ExpectedProviderIdentity); err != nil {
		return err
	}
	expected, exists, snapshotSet := core.ServerLeaseClaimSnapshot(req.Lease.Server)
	if !snapshotSet && r.State == "ready" {
		return fmt.Errorf("UTM completed lease release requires its resolved claim snapshot; resolve the lease again")
	}
	// A partial acquisition may not have published a claim yet. Missing snapshot
	// authority can authorize only continued absence, never adopt a later owner.
	if !snapshotSet {
		expected = core.LeaseClaim{}
		exists = false
	}
	return b.releaseRecordWithAuthority(ctx, &r, req.CheckpointID, req.GuardedRemoteCleanup, releaseClaimAuthority{claim: expected, exists: exists})
}

var errCleanupActivity = errors.New("UTM lease activity renewed before cleanup")

type releaseClaimAuthority struct {
	claim  core.LeaseClaim
	exists bool
}

// Cleanup and acquisition rollback intentionally acquire fresh local authority.
// Public ReleaseLease instead passes the immutable snapshot carried by its target.
func (b *backend) releaseRecord(ctx context.Context, r *record, checkpointID string, teardown func(context.Context, core.LeaseTarget), cleanupOnly ...bool) error {
	claim, exists, err := core.ReadLeaseClaimWithPresence(r.LeaseID)
	if err != nil {
		return err
	}
	return b.releaseRecordWithAuthority(ctx, r, checkpointID, teardown, releaseClaimAuthority{claim: claim, exists: exists}, cleanupOnly...)
}

func (b *backend) releaseRecordWithAuthority(ctx context.Context, r *record, checkpointID string, teardown func(context.Context, core.LeaseTarget), authority releaseClaimAuthority, cleanupOnly ...bool) error {
	vms, err := b.inventory(ctx)
	if err != nil {
		return err
	}
	if r.VMID == "" {
		if err = b.reconcile(ctx, r, vms); err != nil {
			return err
		}
	}
	claim, exists := authority.claim, authority.exists
	if exists {
		if claim.LeaseID != r.LeaseID || claim.Slug != r.Slug || claim.Provider != "utm" || claim.CloudID != r.VMID || claim.ProviderScope != (Provider{}).ClaimScope(b.cfg) {
			return fmt.Errorf("UTM release claim identity mismatch")
		}
		if err = core.AuthorizeCheckpointRelease(claim, checkpointID); err != nil {
			return err
		}
	}
	action := func() error {
		// Refresh while holding the local claim fence; a cached inventory observation
		// cannot authorize a later deletion.
		current, err := b.inventory(ctx)
		if err != nil {
			return err
		}
		if len(cleanupOnly) > 0 && cleanupOnly[0] {
			eligible, _, checkErr := b.cleanupEligible(*r, b.project(*r, current))
			if checkErr != nil {
				return checkErr
			}
			if !eligible {
				return errCleanupActivity
			}
		}
		v, present := findVM(current, r.VMID)
		if present {
			if v.ID == r.SourceID {
				return fmt.Errorf("refusing to delete UTM source template")
			}
			if r.isMacOS() {
				if v.Backend != "apple" || v.Configuration.Notes != r.marker() {
					return fmt.Errorf("UTM Apple deletion ownership mismatch")
				}
				if r.Configured {
					if err = b.validateOwnedMacOS(ctx, *r, current, false); err != nil {
						return err
					}
				}
			}
			if r.Configured {
				if err = validateClone(v, *r); err != nil {
					return err
				}
			}
			if teardown != nil {
				teardown(ctx, b.target(*r, current))
			}
			r.State = "deleting"
			r.Labels["state"] = "deleting"
			if err = saveRecord(*r); err != nil {
				return err
			}
			if v.Status != "stopped" {
				if err = b.control(ctx, "stop", r.VMID); err != nil {
					return err
				}
				if err = b.waitObservation(ctx, r.VMID, false); err != nil {
					return err
				}
			}
			deleteErr := b.control(ctx, "delete", r.VMID)
			absenceErr := b.waitObservation(ctx, r.VMID, true)
			if absenceErr != nil {
				return errors.Join(deleteErr, absenceErr)
			}
		}
		return nil
	}
	if err = core.CleanupLeaseClaimIfUnchangedAfterContext(ctx, r.LeaseID, claim, exists, action); err != nil {
		return err
	}
	if err := b.removeGeneratedKey(*r); err != nil {
		return err
	}
	unlockActivity, err := lockActivity(ctx, r.LeaseID)
	if err != nil {
		return err
	}
	defer unlockActivity()
	if path, pathErr := activityPath(r.LeaseID); pathErr == nil {
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			return removeErr
		}
	}
	return removeRecord(r.LeaseID)
}
func (b *backend) waitObservation(ctx context.Context, id string, absent bool) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var last error
	for {
		vms, err := b.inventory(ctx)
		if err == nil {
			v, ok := findVM(vms, id)
			if absent && !ok || !absent && ok && v.Status == "stopped" {
				return nil
			}
			last = fmt.Errorf("UTM UUID %s status=%s present=%t", id, v.Status, ok)
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("UTM disposition not confirmed (journal retained): %w", errors.Join(last, ctx.Err()))
		case <-time.After(250 * time.Millisecond):
		}
	}
}
func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	unlock, err := b.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	records, err := b.records()
	if err != nil {
		return err
	}
	vms, err := b.inventory(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, r := range records {
		s := b.project(r, vms)
		eligible, reason, checkErr := b.cleanupEligible(r, s)
		if checkErr != nil {
			failures = append(failures, checkErr)
			continue
		}
		if !eligible {
			fmt.Fprintf(b.rt.Stderr, "skip UTM lease=%s reason=%s\n", r.LeaseID, reason)
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stdout, "would remove UTM lease=%s vm=%s reason=%s\n", r.LeaseID, core.Blank(r.VMID, "unresolved"), reason)
			continue
		}
		if err = b.releaseRecord(ctx, &r, "", nil, true); err != nil {
			if errors.Is(err, errCleanupActivity) {
				fmt.Fprintf(b.rt.Stderr, "skip UTM lease=%s reason=renewed activity\n", r.LeaseID)
				continue
			}
			failures = append(failures, fmt.Errorf("UTM lease=%s: %w", r.LeaseID, err))
			continue
		}
		fmt.Fprintf(b.rt.Stdout, "removed UTM lease=%s vm=%s\n", r.LeaseID, r.VMID)
	}
	return errors.Join(failures...)
}
func (b *backend) Doctor(ctx context.Context, _ core.DoctorRequest) (core.DoctorResult, error) {
	if b.cfg.UTM.Acquisition == "ipsw" {
		return b.doctorMacOS(ctx)
	}
	result := core.DoctorResult{Provider: "utm", Message: "local macOS + prepared Linux QEMU template; emulated network + unique loopback SSH; mutation=false", Checks: []core.DoctorCheck{}}
	add := func(name, status, message string) {
		result.Checks = append(result.Checks, core.DoctorCheck{Check: name, Status: status, Message: message})
	}
	_, err := b.command(ctx, b.cfg.UTM.AppPath+"/Contents/MacOS/utmctl", []string{"--help"}, nil)
	if err != nil {
		add("cli", "failed", err.Error())
		result.Status = "failed"
	} else {
		add("cli", "ok", "bundled utmctl executable is available; application control is checked separately")
	}
	vms, err := b.inventory(ctx)
	if err != nil {
		add("application-control", "failed", err.Error())
		result.Status = "failed"
		return result, nil
	}
	add("application-control", "ok", "published UTM scripting inventory is readable; no VM created or started")
	template, ok := findVM(vms, b.cfg.UTM.Template)
	if !ok {
		add("template", "failed", "set utm.template to the UUID of a stopped prepared Linux QEMU VM")
		result.Status = "failed"
	} else if err = validateTemplate(template); err != nil {
		add("template", "failed", err.Error())
		result.Status = "failed"
	} else {
		add("template", "ok", "stopped QEMU template with one emulated NIC; guest OS and tools remain unverified until authenticated acquisition")
	}
	if root, storageErr := b.storageRoot(); storageErr != nil {
		add("storage", "failed", storageErr.Error())
		result.Status = "failed"
	} else {
		add("storage", "ok", "clone bundle directory is accessible: "+root+"; mutation=false")
	}
	add("guest-readiness", "unchecked", "acquisition must verify authorized SSH, Linux architecture, clone MAC, bash, rsync and tar; UTM running status or guest-agent IP alone is insufficient")
	return result, nil
}
