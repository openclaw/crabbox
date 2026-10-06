package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const (
	claimsPruneAge          = 7 * 24 * time.Hour
	claimsAutoPruneInterval = 24 * time.Hour
	claimsAutoPruneBudget   = 5 * time.Second
)

type fileClaimsConfig struct {
	AutoPrune *bool `yaml:"autoPrune,omitempty"`
}

type claimsPruneOutput struct {
	Version  int                 `json:"version"`
	Source   string              `json:"source"`
	DryRun   bool                `json:"dryRun"`
	Eligible []string            `json:"eligible"`
	Pruned   []string            `json:"pruned"`
	Kept     int                 `json:"kept"`
	Problems []localClaimProblem `json:"problems"`
	cursor   string
}

func (a App) claimsPrune(ctx context.Context, args []string) error {
	fs := newFlagSet("claims prune", a.Stderr)
	older := fs.String("older-than", "7d", "minimum claim age (duration or whole days, e.g. 7d)")
	dryRun := fs.Bool("dry-run", false, "report eligible claims without removing them")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Exit(2, "claims prune does not accept positional arguments")
	}
	age, err := parseCheckpointPruneDurationFlag("--older-than", *older)
	if err != nil {
		return err
	}
	if age <= 0 {
		return Exit(2, "--older-than must be positive")
	}
	output, pruneErr := pruneLeaseClaims(ctx, time.Now(), age, *dryRun, "", readLeaseClaimSnapshotWithPresence)
	if *jsonOut {
		err = json.NewEncoder(a.Stdout).Encode(output)
	} else {
		_, err = fmt.Fprintf(a.Stdout, "Local claims: eligible=%d pruned=%d kept=%d dry_run=%t\n", len(output.Eligible), len(output.Pruned), output.Kept, output.DryRun)
		if err == nil && len(output.Problems) != 0 {
			err = renderLocalClaims(a.Stdout, localClaimsListOutput{Problems: output.Problems})
		}
	}
	if err != nil {
		return err
	}
	if pruneErr != nil {
		return pruneErr
	}
	if len(output.Problems) != 0 {
		return ExitError{Code: 2}
	}
	return nil
}

func claimPruneEligible(claim leaseClaim, now time.Time, age time.Duration) (bool, error) {
	if claim.StaticHost != "" || claim.FixedCreateIntent != nil || claim.CheckpointCapture != nil ||
		claim.RuntimeAdapterPendingRegistrationID != "" || claim.RuntimeAdapterRegistrationID != "" || claim.CoordinatorRegistrationURL != "" {
		return false, nil
	}
	// Unbounded providers are never eligible, so their timestamps are irrelevant.
	provider, err := ProviderFor(claim.Provider)
	if err != nil || provider.Spec().ClaimExpiryBound <= 0 {
		return false, nil
	}
	used, err := time.Parse(time.RFC3339Nano, firstNonBlank(claim.LastUsedAt, claim.ClaimedAt))
	if err != nil {
		return false, &leaseClaimFileError{code: "invalid_timestamp", err: err}
	}
	return used.Before(now.Add(-age)) && used.Add(provider.Spec().ClaimExpiryBound).Before(now), nil
}

func pruneLeaseClaims(ctx context.Context, now time.Time, age time.Duration, dryRun bool, after string, read leaseClaimSnapshotReader) (claimsPruneOutput, error) {
	output := claimsPruneOutput{Version: 1, Source: localClaimsListSource, DryRun: dryRun, Eligible: []string{}, Pruned: []string{}, Problems: []localClaimProblem{}}
	problems := leaseClaimsSnapshot{invalid: make(map[string]error)}
	cursor, err := walkLeaseClaimsReadOnly(ctx, after, read, func(id string, claim leaseClaim, problem error) error {
		eligible := false
		if problem == nil {
			eligible, problem = claimPruneEligible(claim, now, age)
		}
		if problem != nil {
			// A concurrent stop may remove the claim after the directory listing;
			// a claim that is already gone needs no pruning and is not a problem.
			if path, err := leaseClaimPath(id); err == nil {
				if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
					return nil
				}
			}
			problems.invalid[id] = problem
			output.Kept++
			return nil
		}
		if !eligible {
			output.Kept++
			return nil
		}
		output.Eligible = append(output.Eligible, id)
		if dryRun {
			output.Kept++
			return nil
		}
		// The existing operation fence rereads the entire snapshot. Concurrent
		// use/replacement wins; only the claim file is removed, never its keys.
		if err := CleanupLeaseClaimIfUnchangedAfterContext(ctx, id, claim, true, nil); err != nil {
			output.Kept++
			if ctx.Err() != nil {
				return ctx.Err()
			}
			problems.invalid[id] = &leaseClaimFileError{code: "claim_not_removed", err: err}
			return nil
		}
		output.Pruned = append(output.Pruned, id)
		return nil
	})
	output.cursor = cursor
	output.Problems = projectLocalClaims(problems).Problems
	return output, err
}

func (a App) autoPruneClaims(ctx context.Context, cfg Config) {
	if !cfg.ClaimsAutoPrune {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, claimsAutoPruneBudget)
	defer cancel()
	// Stop/release has already completed and printed its result. Maintenance
	// never runs before acquisition or inside a provider's release fence.
	err := autoPruneLeaseClaims(ctx, time.Now(), readLeaseClaimSnapshotWithPresence)
	if debug, _ := getenvBool("CRABBOX_DEBUG"); debug && err != nil {
		fmt.Fprintf(a.Stderr, "debug: local claims auto-prune: %v\n", err)
	}
}

func autoPruneLeaseClaims(ctx context.Context, now time.Time, read leaseClaimSnapshotReader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := CrabboxStateDir()
	if err != nil {
		return err
	}
	if err := makePrivateClaimDirectories(dir); err != nil {
		return err
	}
	// Try once: another command's maintenance must not hold up this command.
	lock := flock.New(filepath.Join(dir, "claims-prune.lock"), flock.SetPermissions(0o600))
	locked, err := lock.TryLock()
	if err != nil || !locked {
		return err
	}
	defer lock.Unlock()
	stamp := filepath.Join(dir, "claims-prune.stamp")
	if data, err := os.ReadFile(stamp); err == nil {
		if last, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data))); err == nil && now.Sub(last) < claimsAutoPruneInterval {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	progress := filepath.Join(dir, "claims-prune.cursor")
	data, err := os.ReadFile(progress)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	output, err := pruneLeaseClaims(ctx, now, claimsPruneAge, false, string(data), read)
	if err != nil {
		// An incomplete pass never advances the daily stamp. Resume after kept
		// files too, so a large unprunable prefix cannot starve later entries.
		if output.cursor != "" {
			_ = writeClaimsPruneState(progress, output.cursor)
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeClaimsPruneState(stamp, now.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if err := os.Remove(progress); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(output.Problems) > 0 {
		return fmt.Errorf("%d local claim problems; files retained", len(output.Problems))
	}
	return nil
}

func writeClaimsPruneState(path, value string) error {
	return writeStateFileAtomic(path, []byte(value), syncControllerDirectory)
}
