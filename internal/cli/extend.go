package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

func (a App) extend(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("extend", a.Stderr)
	provider := registerProviderSelectionFlag(fs, defaults, providerHelpAll())
	by := fs.Duration("by", 30*time.Minute, "add to the total lifetime from creation")
	ttl := fs.Duration("ttl", 0, "new total lifetime from creation")
	reason := fs.String("reason", "", "reason for extension (at most 200 characters)")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, reorderInterspersedFlags(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || flagWasSet(fs, "by") && flagWasSet(fs, "ttl") {
		return Exit(2, "usage: crabbox extend <id-or-slug> [--by 30m | --ttl 2h] [--reason text] [--json]")
	}
	if flagWasSet(fs, "ttl") {
		*by = 0
	}
	if *by < 0 || *ttl < 0 || *by == 0 && *ttl == 0 || *by%time.Second != 0 || *ttl%time.Second != 0 {
		return Exit(2, "extension must be a positive whole number of seconds")
	}
	if utf8.RuneCountInString(*reason) > 200 {
		return Exit(2, "extension reason must be at most 200 characters")
	}
	id := strings.TrimSpace(fs.Arg(0))
	cfg, err := loadLeaseTargetConfig(fs, *provider, targetFlagValues{}, networkModeFlagValues{}, leaseTargetConfigOptions{LeaseID: id})
	if err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	extension, ok := backend.(LeaseExtensionBackend)
	if !ok {
		return Exit(2, "extend is not supported for provider %s", backend.Spec().Name)
	}
	lease := LeaseTarget{LeaseID: id}
	if _, brokered := backend.(*coordinatorLeaseBackend); !brokered {
		resolver, ok := backend.(SSHLeaseBackend)
		if !ok {
			return Exit(2, "extend is not supported for provider %s", backend.Spec().Name)
		}
		lease, err = resolver.Resolve(ctx, ResolveRequest{Options: leaseOptionsFromConfig(cfg), ID: id, StatusOnly: true, NoLocalStateMutations: true})
		if err != nil {
			return err
		}
		claim, exists, err := statusLeaseExactClaim(ctx, backend, lease, backend.Spec().Name, leaseOptionsFromConfig(cfg).ProviderScope)
		if err != nil {
			return err
		}
		if !exists {
			return Exit(4, "lease %s has no exact local claim; refusing extension", id)
		}
		SetServerLeaseClaimSnapshot(&lease.Server, claim, true)
	}
	result, err := extension.ExtendLease(ctx, LeaseExtendRequest{Lease: lease, TTL: *ttl, By: *by, Reason: *reason})
	if *jsonOut && (err == nil || result.Error != "") {
		if encodeErr := json.NewEncoder(a.Stdout).Encode(result); encodeErr != nil {
			return encodeErr
		}
		return err
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.Stdout, "extended lease=%s expires=%s remaining=%s max_lifetime=%s\n", result.ID, result.ExpiresAt, result.Remaining, result.MaxLifetime)
	return err
}

func leaseExtendResult(id, expires, maximum string, runtime bool) LeaseExtendResult {
	result := LeaseExtendResult{ID: id, ExpiresAt: expires, MaxLifetime: maximum, RuntimeExtension: runtime}
	if deadline, err := time.Parse(time.RFC3339, expires); err == nil {
		result.Remaining = max(time.Duration(0), time.Until(deadline)).Round(time.Second).String()
	}
	return result
}

// PendingGuestExtension makes a failed file-backed refresh retryable without
// applying --by twice. It is local recovery state, never provider metadata.
type PendingGuestExtension struct {
	BySeconds    int64  `json:"bySeconds"`
	TTLSeconds   int64  `json:"ttlSeconds"`
	Reason       string `json:"reason,omitempty"`
	OldExpiresAt string `json:"oldExpiresAt"`
	NewTTL       string `json:"newTTL"`
}

// ExtendDirectLease serializes provider metadata, claim publication and guest
// refresh. Provider labels remain authoritative if the guest write fails.
func ExtendDirectLease(ctx context.Context, backend Backend, cfg Config, req LeaseExtendRequest, now time.Time, write func(context.Context, Server, LeaseClaim) error) (LeaseExtendResult, error) {
	expected, exists, set := ServerLeaseClaimSnapshot(req.Lease.Server)
	if !set || !exists {
		return LeaseExtendResult{}, Exit(4, "lease %s has no exact claim; refusing extension", req.Lease.LeaseID)
	}
	if err := AuthorizeCheckpointRelease(expected, ""); err != nil {
		return LeaseExtendResult{}, err
	}
	fileGuard := guestExpiryAdapter(cfg) == "file"
	var target SSHTarget
	var resolveErr error
	if fileGuard {
		target, resolveErr = resolveGuestLeaseExpiryTarget(ctx, backend, cfg, req.Lease)
	}
	var result LeaseExtendResult
	err := WithLeaseClaimUnchangedContext(ctx, req.Lease.LeaseID, expected, func() error {
		server := req.Lease.Server
		if expected.Labels["created_at"] == "" || server.Labels["created_at"] != expected.Labels["created_at"] {
			return Exit(4, "lease creation anchor differs from its exact claim; refusing extension")
		}
		claim := cloneLeaseClaim(expected)
		pending := claim.PendingGuestExtension
		if pending != nil {
			if pending.BySeconds != int64(req.By/time.Second) || pending.TTLSeconds != int64(req.TTL/time.Second) || pending.Reason != req.Reason {
				return Exit(4, "guest expiry refresh is pending; rerun crabbox extend with the original arguments before requesting another extension")
			}
			if server.Labels["ttl_secs"] != pending.NewTTL {
				return Exit(4, "lease lifetime changed since the pending guest expiry refresh; refusing extension")
			}
		} else {
			labels, err := ExtendedLeaseLabels(server.Labels, cfg, req, now)
			if err != nil {
				return err
			}
			server.Labels = labels
			if err := transformLeaseClaimEndpoint(&claim, server, req.Lease.SSH, leaseClaimEndpointPolicy{}); err != nil {
				return err
			}
			if fileGuard {
				pending = &PendingGuestExtension{BySeconds: int64(req.By / time.Second), TTLSeconds: int64(req.TTL / time.Second), Reason: req.Reason, OldExpiresAt: LeaseLabelTimeDisplay(req.Lease.Server.Labels["expires_at"]), NewTTL: labels["ttl_secs"]}
				claim.PendingGuestExtension = pending
			}
			if err := refreshLeaseClaimRevision(&claim); err != nil {
				return err
			}
			if err := write(ctx, server, expected); err != nil {
				return err
			}
			path, err := leaseClaimPath(req.Lease.LeaseID)
			if err != nil {
				return err
			}
			if err := writeLeaseClaimAtomic(path, claim); err != nil {
				return fmt.Errorf("provider deadline extended to %s, but saving the local extension claim failed: %w", LeaseLabelTimeDisplay(labels["expires_at"]), err)
			}
		}
		result = leaseExtendResult(req.Lease.LeaseID, LeaseLabelTimeDisplay(server.Labels["expires_at"]), EffectiveMaxTTL(cfg).String(), true)
		if !fileGuard {
			return nil
		}
		if resolveErr == nil {
			resolveErr = RefreshGuestLeaseExpiry(ctx, target, GuestLeaseExpiryFromLabels(server.Labels))
		}
		if resolveErr != nil {
			result.GuestExpiresAt = pending.OldExpiresAt
			result.GuestExpiryRefreshPending = true
			result.Error = fmt.Sprintf("provider deadline extended to %s, but the on-box guard still holds %s and will power the machine off then; rerun crabbox extend with the same arguments to retry: %v", result.ExpiresAt, result.GuestExpiresAt, resolveErr)
			return Exit(1, "%s", result.Error)
		}
		result.GuestExpiresAt = result.ExpiresAt
		claim.PendingGuestExtension = nil
		if err := refreshLeaseClaimRevision(&claim); err != nil {
			return err
		}
		path, err := leaseClaimPath(req.Lease.LeaseID)
		if err != nil {
			return err
		}
		if err := writeLeaseClaimAtomic(path, claim); err != nil {
			result.Error = fmt.Sprintf("provider and guest deadlines extended to %s, but clearing the local retry state failed: %v", result.ExpiresAt, err)
			return Exit(1, "%s", result.Error)
		}
		return nil
	})
	return result, err
}

func ExtendedLeaseLabels(labels map[string]string, cfg Config, req LeaseExtendRequest, now time.Time) (map[string]string, error) {
	created, ok := parseLeaseLabelTime(labels["created_at"])
	if !ok || created.After(now) {
		return nil, Exit(4, "lease creation time is unknown or in the future; refusing extension")
	}
	expiry, ok := parseLeaseLabelTime(labels["expires_at"])
	if !ok || !expiry.After(now) || statusTerminalState(labels["state"]) || labels["state"] == "cleanup" || labels["state"] == "deleting" {
		return nil, Exit(4, "lease is not active")
	}
	fromTTL, ok := parseDurationSecondsLabel(labels["ttl_secs"])
	if !ok {
		return nil, Exit(4, "lease total lifetime is unknown; refusing extension")
	}
	toTTL := req.TTL
	if toTTL == 0 {
		toTTL = fromTTL + req.By
	}
	toTTL = min(toTTL, EffectiveMaxTTL(cfg))
	if toTTL <= fromTTL {
		return nil, Exit(4, "lease extension does not advance the lifetime (maximum %s)", EffectiveMaxTTL(cfg))
	}
	touched, ok := parseLeaseLabelTime(labels["last_touched_at"])
	if !ok {
		return nil, Exit(4, "lease last activity time is unknown; refusing extension")
	}
	idle, ok := directLeaseIdleTimeout(labels)
	if !ok {
		return nil, Exit(4, "lease idle timeout is unknown; refusing extension")
	}
	next := make(map[string]string, len(labels)+1)
	for key, value := range labels {
		next[key] = value
	}
	next["ttl_secs"] = durationSecondsLabel(toTTL)
	next["max_ttl_secs"] = durationSecondsLabel(EffectiveMaxTTL(cfg))
	next["expires_at"] = LeaseLabelTime(directLeaseExpiresAtFrom(created, touched, toTTL, min(idle, toTTL)))
	return next, nil
}
