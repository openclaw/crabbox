package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

func (a App) readyPoolList(ctx context.Context, args []string) error {
	fs := newFlagSet("pool ready", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	args, key := extractFirstPositionalArg(args, nil)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	coord, err := readyPoolCoordinator()
	if err != nil {
		return err
	}
	var entries []CoordinatorReadyPoolEntry
	if key == "" {
		entries, err = coord.ReadyPools(ctx)
	} else {
		entries, err = coord.ReadyPool(ctx, key)
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(entries)
	}
	renderReadyPoolEntries(a.Stdout, entries)
	return nil
}

func (a App) readyPoolRegister(ctx context.Context, args []string) error {
	fs := newFlagSet("pool register", a.Stderr)
	id := fs.String("id", "", "lease id")
	repoFlag := fs.String("repo", "", "repository owner/name")
	ref := fs.String("ref", "", "source ref")
	commit := fs.String("commit", "", "source commit")
	fingerprint := fs.String("fingerprint", "", "repo setup fingerprint")
	compatibilityKey := fs.String("compatibility-key", "", "provider-neutral capability and size key")
	identityFile := fs.String("identity-file", "", "typed ready-pool identity JSON")
	image := fs.String("image", "", "base image id or name")
	sshHost := fs.String("ssh-host", "", "proven SSH host")
	sshUser := fs.String("ssh-user", "", "proven SSH user")
	sshPort := fs.String("ssh-port", "", "proven SSH port")
	workRoot := fs.String("work-root", "", "remote work root")
	jsonOut := fs.Bool("json", false, "print JSON")
	args, key := extractFirstPositionalArg(args, poolRegisterValueFlags())
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if key == "" || *id == "" {
		return exit(2, "usage: crabbox pool register <key> --id <lease-id>")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	repo, _ := findRepo()
	input := map[string]any{"leaseID": strings.TrimSpace(*id)}
	if repoValue := firstNonBlank(*repoFlag, cfg.Actions.Repo, bestEffortGitHubRepoSlug(repo, cfg)); repoValue != "" {
		input["repo"] = repoValue
	}
	refValue := firstNonBlank(*ref, cfg.Actions.Ref, repo.BaseRef)
	if refValue != "" {
		input["ref"] = refValue
	}
	if commitValue := readyPoolRegisterCommit(cfg, repo, refValue, *commit); commitValue != "" {
		input["commit"] = commitValue
	}
	addStringInput(input, "fingerprint", *fingerprint)
	addStringInput(input, "compatibilityKey", *compatibilityKey)
	addStringInput(input, "image", *image)
	addStringInput(input, "sshHost", firstNonBlank(*sshHost, readyPoolClaimSSHHost(*id)))
	addStringInput(input, "sshUser", *sshUser)
	addStringInput(input, "sshPort", firstNonBlank(*sshPort, readyPoolClaimSSHPort(*id)))
	addStringInput(input, "workRoot", firstNonBlank(*workRoot, readyPoolClaimWorkRoot(*id)))
	coord, err := readyPoolCoordinatorFromConfig(cfg)
	if err != nil {
		return err
	}
	var res CoordinatorReadyPoolResponse
	if strings.TrimSpace(*identityFile) == "" {
		res, err = coord.RegisterReadyPoolLease(ctx, key, input)
	} else {
		identity, identityErr := loadReadyPoolIdentity(*identityFile)
		if identityErr != nil {
			return identityErr
		}
		if seedErr := validateReadyPoolSeedIdentity(identity, readyPoolInputString(input, "repo"), readyPoolInputString(input, "ref"), readyPoolInputString(input, "commit"), readyPoolInputString(input, "fingerprint")); seedErr != nil {
			return seedErr
		}
		lease, leaseErr := coord.GetLease(ctx, strings.TrimSpace(*id))
		if leaseErr != nil {
			return leaseErr
		}
		if identityErr := readyPoolIdentityMatchesLease(identity, lease); identityErr != nil {
			return identityErr
		}
		_, target, _ := leaseToServerTarget(lease, cfg)
		if trustErr := prepareLeaseSSHTrust(&target, lease.ID); trustErr != nil {
			return trustErr
		}
		evidence, evidenceErr := readReadyPoolReadinessEvidence(ctx, target)
		if evidenceErr != nil {
			return evidenceErr
		}
		if evidenceErr := validateReadyPoolReadinessIdentity(identity, evidence); evidenceErr != nil {
			return evidenceErr
		}
		res, err = coord.RegisterTypedReadyPoolLease(ctx, key, typedReadyPoolRegisterRequest(input, identity, evidence))
		if err == nil {
			err = validateTypedReadyPoolResponseIdentity(res, identity)
		}
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(res)
	}
	fmt.Fprintf(a.Stdout, "registered pool=%s lease=%s state=%s repo=%s ref=%s commit=%s\n", res.Entry.Key, res.Entry.LeaseID, res.Entry.State, blank(res.Entry.Repo, "-"), blank(res.Entry.Ref, "-"), shortCommit(res.Entry.Commit))
	return nil
}

func (a App) readyPoolBorrow(ctx context.Context, args []string) error {
	fs := newFlagSet("pool borrow", a.Stderr)
	repoFlag := fs.String("repo", "", "repository owner/name")
	ref := fs.String("ref", "", "source ref")
	commit := fs.String("commit", "", "source commit")
	fingerprint := fs.String("fingerprint", "", "repo setup fingerprint")
	compatibilityKey := fs.String("compatibility-key", "", "provider-neutral capability and size key")
	identityFile := fs.String("identity-file", "", "typed ready-pool identity JSON")
	provider := fs.String("provider", "", "provider filter")
	target := fs.String("target", "", "target OS filter")
	jsonOut := fs.Bool("json", false, "print JSON")
	args, key := extractFirstPositionalArg(args, poolBorrowValueFlags())
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if key == "" {
		return exit(2, "usage: crabbox pool borrow <key>")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	coord, err := readyPoolCoordinatorFromConfig(cfg)
	if err != nil {
		return err
	}
	borrowInput := readyPoolBorrowInput(*repoFlag, *ref, *commit, *fingerprint, *compatibilityKey, *provider, *target)
	var res CoordinatorReadyPoolResponse
	if strings.TrimSpace(*identityFile) == "" {
		res, err = coord.BorrowReadyPoolLease(ctx, key, borrowInput)
	} else {
		repo, _ := findRepo()
		borrowInput = readyPoolManualTypedBorrowInput(
			cfg,
			repo,
			*repoFlag,
			*ref,
			*commit,
			*fingerprint,
			*compatibilityKey,
			*provider,
			*target,
		)
		identity, identityErr := loadReadyPoolIdentity(*identityFile)
		if identityErr != nil {
			return identityErr
		}
		if seedErr := validateReadyPoolSeedIdentity(identity, readyPoolInputString(borrowInput, "repo"), readyPoolInputString(borrowInput, "ref"), readyPoolInputString(borrowInput, "commit"), readyPoolInputString(borrowInput, "fingerprint")); seedErr != nil {
			return seedErr
		}
		res, err = borrowValidatedTypedReadyPoolLease(ctx, coord, key, typedReadyPoolBorrowRequest(borrowInput, identity), identity)
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(res)
	}
	fmt.Fprintf(a.Stdout, "borrowed pool=%s lease=%s state=%s token=%s ssh=%s@%s:%s\n", res.Entry.Key, res.Entry.LeaseID, res.Entry.State, res.Entry.BorrowToken, blank(res.Entry.SSHUser, res.Lease.SSHUser), blank(res.Entry.SSHHost, res.Lease.Host), blank(res.Entry.SSHPort, res.Lease.SSHPort))
	return nil
}

func (a App) readyPoolHeartbeat(ctx context.Context, args []string) error {
	fs := newFlagSet("pool heartbeat", a.Stderr)
	id := fs.String("id", "", "borrowed lease id")
	borrowToken := fs.String("borrow-token", "", "borrow token from pool borrow")
	jsonOut := fs.Bool("json", false, "print JSON")
	args, key := extractFirstPositionalArg(args, map[string]bool{"id": true, "borrow-token": true})
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if key == "" || *id == "" || *borrowToken == "" {
		return exit(2, "usage: crabbox pool heartbeat <key> --id <lease-id> --borrow-token <token>")
	}
	coord, err := readyPoolCoordinator()
	if err != nil {
		return err
	}
	res, err := coord.HeartbeatReadyPoolBorrow(ctx, key, *id, *borrowToken)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(res)
	}
	fmt.Fprintf(a.Stdout, "heartbeat pool=%s lease=%s state=%s expires=%s\n", res.Entry.Key, res.Entry.LeaseID, res.Entry.State, res.Entry.BorrowExpiresAt)
	return nil
}

func (a App) readyPoolReturn(ctx context.Context, args []string) error {
	fs := newFlagSet("pool return", a.Stderr)
	id := fs.String("id", "", "lease id")
	result := fs.String("result", "ready", "return result: ready, drain, or release")
	reason := fs.String("reason", "", "short reason")
	borrowToken := fs.String("borrow-token", "", "borrow token from pool borrow")
	identityFile := fs.String("identity-file", "", "typed ready-pool identity JSON")
	jsonOut := fs.Bool("json", false, "print JSON")
	args, key := extractFirstPositionalArg(args, map[string]bool{"id": true, "result": true, "reason": true, "borrow-token": true, "identity-file": true})
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if key == "" || *id == "" {
		return exit(2, "usage: crabbox pool return <key> --id <lease-id>")
	}
	if err := validateReadyPoolReturnResult(*result); err != nil {
		return err
	}
	coord, err := readyPoolCoordinator()
	if err != nil {
		return err
	}
	var res CoordinatorReadyPoolResponse
	if strings.TrimSpace(*identityFile) == "" {
		res, err = coord.ReturnReadyPoolLease(ctx, key, *id, *result, *reason, *borrowToken)
	} else {
		identity, identityErr := loadReadyPoolIdentity(*identityFile)
		if identityErr != nil {
			return identityErr
		}
		request := CoordinatorReadyPoolReturnIdentityRequest{
			LeaseID:     strings.TrimSpace(*id),
			Result:      strings.TrimSpace(*result),
			Reason:      strings.TrimSpace(*reason),
			BorrowToken: strings.TrimSpace(*borrowToken),
			Identity:    &identity,
		}
		res, err = returnTypedReadyPoolLeaseWithEvidence(ctx, coord, key, request, func() (CoordinatorReadyPoolReadinessEvidence, error) {
			cfg, cfgErr := loadConfig()
			if cfgErr != nil {
				return CoordinatorReadyPoolReadinessEvidence{}, cfgErr
			}
			lease, leaseErr := coord.GetLease(ctx, request.LeaseID)
			if leaseErr != nil {
				return CoordinatorReadyPoolReadinessEvidence{}, leaseErr
			}
			_, target, _ := leaseToServerTarget(lease, cfg)
			if trustErr := prepareLeaseSSHTrust(&target, lease.ID); trustErr != nil {
				return CoordinatorReadyPoolReadinessEvidence{}, trustErr
			}
			evidence, evidenceErr := readReadyPoolReadinessEvidence(ctx, target)
			if evidenceErr != nil {
				return CoordinatorReadyPoolReadinessEvidence{}, evidenceErr
			}
			if evidenceErr := validateReadyPoolReadinessIdentity(identity, evidence); evidenceErr != nil {
				return CoordinatorReadyPoolReadinessEvidence{}, evidenceErr
			}
			return evidence, nil
		})
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(res)
	}
	fmt.Fprintf(a.Stdout, "returned pool=%s lease=%s state=%s result=%s\n", res.Entry.Key, res.Entry.LeaseID, res.Entry.State, *result)
	return nil
}

func (a App) readyPoolEnsure(ctx context.Context, args []string) error {
	fs := newFlagSet("pool ensure", a.Stderr)
	minReady := fs.Int("min-ready", 1, "minimum ready leases")
	maxReady := fs.Int("max-ready", -1, "maximum ready, busy, and in-flight leases (default min-ready)")
	compatibilityKey := fs.String("compatibility-key", "", "provider-neutral capability and size key")
	identityFile := fs.String("identity-file", "", "typed ready-pool identity JSON")
	create := fs.Bool("create", false, "claim and create missing ready leases with prewarm")
	jsonOut := fs.Bool("json", false, "print JSON")
	args, key := extractFirstPositionalArg(args, map[string]bool{"min-ready": true, "max-ready": true, "compatibility-key": true, "identity-file": true})
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if key == "" {
		return exit(2, "usage: crabbox pool ensure <key> [--create] [prewarm flags...]")
	}
	if err := validateReadyPoolEnsurePrewarmArgs(fs.Args()); err != nil {
		return err
	}
	if *minReady < 0 || *minReady > 100 {
		return exit(2, "--min-ready must be between 0 and 100")
	}
	if *maxReady < 0 {
		*maxReady = *minReady
	}
	if *maxReady < *minReady || *maxReady > 100 {
		return exit(2, "--max-ready must be between min-ready and 100")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	coord, err := readyPoolCoordinatorFromConfig(cfg)
	if err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	repoSlug := cfg.Actions.Repo
	if repoSlug == "" {
		repoSlug = bestEffortGitHubRepoSlug(repo, cfg)
	}
	borrowInput := readyPoolRunBorrowInput(cfg, repo, repoSlug)
	addStringInput(borrowInput, "compatibilityKey", *compatibilityKey)
	borrowInput["minReady"] = *minReady
	borrowInput["maxReady"] = *maxReady
	borrowInput["claim"] = *create
	var typedIdentity *CoordinatorReadyPoolIdentityV1
	if strings.TrimSpace(*identityFile) != "" {
		identity, identityErr := loadReadyPoolIdentity(*identityFile)
		if identityErr != nil {
			return identityErr
		}
		if seedErr := validateReadyPoolSeedIdentity(identity, readyPoolInputString(borrowInput, "repo"), readyPoolInputString(borrowInput, "ref"), readyPoolInputString(borrowInput, "commit"), readyPoolInputString(borrowInput, "fingerprint")); seedErr != nil {
			return seedErr
		}
		typedIdentity = &identity
	}
	prewarmArgs := append([]string{}, fs.Args()...)
	prewarmArgs = append(prewarmArgs, "--pool", key)
	if strings.TrimSpace(*compatibilityKey) != "" {
		prewarmArgs = append(prewarmArgs, "--pool-compatibility-key", strings.TrimSpace(*compatibilityKey))
	}
	if typedIdentity != nil {
		prewarmArgs = append(prewarmArgs, "--pool-identity-file", strings.TrimSpace(*identityFile))
	}
	prewarmApp := a
	if *jsonOut {
		prewarmApp.Stdout = a.Stderr
	}
	for {
		var res CoordinatorReadyPoolReconcileResponse
		if typedIdentity == nil {
			res, err = coord.ReconcileReadyPool(ctx, key, borrowInput)
		} else {
			res, err = coord.ReconcileTypedReadyPool(ctx, key, typedReadyPoolReconcileRequest(borrowInput, *typedIdentity, *minReady, *maxReady, *create))
		}
		if err != nil {
			if typedIdentity == nil && readyPoolCoordinatorRouteUnsupported(err) {
				fmt.Fprintln(a.Stderr, "notice: coordinator does not support atomic ready-pool reconciliation; using legacy count-then-create fallback")
				entries, ready, legacyErr := ensureReadyPoolLegacy(
					ctx,
					coord,
					key,
					*minReady,
					*create,
					borrowInput,
					func() error { return prewarmApp.prewarm(ctx, prewarmArgs) },
				)
				if legacyErr != nil {
					return legacyErr
				}
				if ready < *minReady {
					if *jsonOut {
						if encodeErr := json.NewEncoder(a.Stdout).Encode(map[string]any{"key": key, "ready": ready, "minReady": *minReady, "entries": entries}); encodeErr != nil {
							return encodeErr
						}
					}
					return exit(5, "pool=%s ready=%d min_ready=%d create=%t", key, ready, *minReady, *create)
				}
				return renderReadyPoolLegacyResult(a.Stdout, key, ready, *minReady, entries, *jsonOut)
			}
			return err
		}
		if res.Counts.Ready >= *minReady {
			return renderReadyPoolReconcileResult(a.Stdout, res, *jsonOut)
		}
		if !*create {
			if *jsonOut {
				if err := json.NewEncoder(a.Stdout).Encode(res); err != nil {
					return err
				}
			}
			return exit(5, "pool=%s ready=%d min_ready=%d create=false", key, res.Counts.Ready, *minReady)
		}
		if res.Claim == nil {
			if *jsonOut {
				if err := json.NewEncoder(a.Stdout).Encode(res); err != nil {
					return err
				}
			}
			return exit(5, "pool=%s ready=%d in_flight=%d min_ready=%d max_ready=%d capped=%t", key, res.Counts.Ready, res.Counts.InFlight, *minReady, *maxReady, res.Capped)
		}
		if err := prewarmApp.prewarmWithPoolFillClaim(ctx, prewarmArgs, res.Claim.Token); err != nil {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			releaseErr := coord.ReleaseReadyPoolFillClaim(releaseCtx, key, res.Claim.Token)
			cancel()
			if releaseErr != nil {
				fmt.Fprintf(a.Stderr, "warning: release ready-pool fill claim failed: %v\n", releaseErr)
			}
			return err
		}
	}
}

func ensureReadyPoolLegacy(
	ctx context.Context,
	coord *CoordinatorClient,
	key string,
	minReady int,
	create bool,
	borrowInput map[string]any,
	prewarm func() error,
) ([]CoordinatorReadyPoolEntry, int, error) {
	entries, err := coord.ReadyPool(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	ready := countReadyPoolEntries(entries, borrowInput)
	if ready >= minReady || !create {
		return entries, ready, nil
	}
	for next := ready; next < minReady; next++ {
		if err := prewarm(); err != nil {
			return nil, 0, err
		}
	}
	entries, err = coord.ReadyPool(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	return entries, countReadyPoolEntries(entries, borrowInput), nil
}

func renderReadyPoolLegacyResult(
	w io.Writer,
	key string,
	ready int,
	minReady int,
	entries []CoordinatorReadyPoolEntry,
	jsonOut bool,
) error {
	if jsonOut {
		return json.NewEncoder(w).Encode(map[string]any{
			"key": key, "ready": ready, "minReady": minReady, "entries": entries,
		})
	}
	fmt.Fprintf(w, "pool=%s ready=%d min_ready=%d\n", key, ready, minReady)
	return nil
}

func renderReadyPoolReconcileResult(w io.Writer, res CoordinatorReadyPoolReconcileResponse, jsonOut bool) error {
	if jsonOut {
		return json.NewEncoder(w).Encode(res)
	}
	fmt.Fprintf(w, "pool=%s ready=%d busy=%d in_flight=%d min_ready=%d max_ready=%d satisfied=%t reconciling=%t\n", res.Desired.Key, res.Counts.Ready, res.Counts.Busy, res.Counts.InFlight, res.Desired.MinReady, res.Desired.MaxReady, res.Satisfied, res.Reconciling)
	return nil
}

func readyPoolCoordinator() (*CoordinatorClient, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return readyPoolCoordinatorFromConfig(cfg)
}

func validateReadyPoolEnsurePrewarmArgs(args []string) error {
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "--" {
			continue
		}
		switch {
		case arg == "--repo" || arg == "--ref" || strings.HasPrefix(arg, "--repo=") || strings.HasPrefix(arg, "--ref="):
			return exit(2, "pool ensure --create does not support forwarded --repo or --ref overrides")
		}
	}
	return nil
}

func readyPoolCoordinatorFromConfig(cfg Config) (*CoordinatorClient, error) {
	coord, ok, err := newCoordinatorClient(cfg)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, exit(2, "ready pools require broker.url or CRABBOX_COORDINATOR")
	}
	return coord, nil
}

func readyPoolBorrowInput(repo, ref, commit, fingerprint, compatibilityKey, provider, target string) map[string]any {
	input := map[string]any{}
	addStringInput(input, "repo", repo)
	addStringInput(input, "ref", ref)
	addStringInput(input, "commit", commit)
	addStringInput(input, "fingerprint", fingerprint)
	addStringInput(input, "compatibilityKey", compatibilityKey)
	addStringInput(input, "provider", provider)
	addStringInput(input, "target", target)
	return input
}

func readyPoolManualTypedBorrowInput(cfg Config, repo Repo, repoFlag, refFlag, commitFlag, fingerprint, compatibilityKey, provider, target string) map[string]any {
	repoValue := firstNonBlank(repoFlag, cfg.Actions.Repo, bestEffortGitHubRepoSlug(repo, cfg))
	refValue := firstNonBlank(refFlag, cfg.Actions.Ref, repo.BaseRef)
	commitValue := readyPoolRegisterCommit(cfg, repo, refValue, commitFlag)
	return readyPoolBorrowInput(repoValue, refValue, commitValue, fingerprint, compatibilityKey, provider, target)
}

func returnTypedReadyPoolLeaseWithEvidence(
	ctx context.Context,
	coord *CoordinatorClient,
	key string,
	request CoordinatorReadyPoolReturnIdentityRequest,
	loadEvidence func() (CoordinatorReadyPoolReadinessEvidence, error),
) (CoordinatorReadyPoolResponse, error) {
	var evidenceErr error
	if request.Result == "ready" {
		var evidence CoordinatorReadyPoolReadinessEvidence
		evidence, evidenceErr = loadEvidence()
		if evidenceErr == nil {
			request.ReadinessEvidence = &evidence
		} else {
			request.Result = "drain"
			request.Reason = "typed ready-pool readiness evidence unavailable or mismatched"
			request.ReadinessEvidence = nil
		}
	}
	response, returnErr := coord.ReturnTypedReadyPoolLease(ctx, key, request)
	return response, errors.Join(evidenceErr, returnErr)
}

func borrowValidatedTypedReadyPoolLease(
	ctx context.Context,
	coord *CoordinatorClient,
	key string,
	request CoordinatorReadyPoolBorrowIdentityRequest,
	expected CoordinatorReadyPoolIdentityV1,
) (CoordinatorReadyPoolResponse, error) {
	response, err := coord.BorrowTypedReadyPoolLease(ctx, key, request)
	if err != nil {
		return response, err
	}
	identityErr := validateTypedReadyPoolResponseIdentity(response, expected)
	if identityErr == nil {
		return response, nil
	}
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	returnIdentity := response.Entry.Identity
	if returnIdentity == nil {
		returnIdentity = &expected
	}
	_, drainErr := coord.ReturnTypedReadyPoolLease(drainCtx, key, CoordinatorReadyPoolReturnIdentityRequest{
		LeaseID:     response.Entry.LeaseID,
		Result:      "drain",
		Reason:      "coordinator returned a mismatched typed ready-pool identity",
		BorrowToken: response.Entry.BorrowToken,
		Identity:    returnIdentity,
	})
	if drainErr != nil {
		return response, errors.Join(identityErr, fmt.Errorf("drain mismatched typed ready-pool borrow: %w", drainErr))
	}
	return response, identityErr
}

func typedReadyPoolRegisterRequest(input map[string]any, identity CoordinatorReadyPoolIdentityV1, evidence CoordinatorReadyPoolReadinessEvidence) CoordinatorReadyPoolRegisterIdentityRequest {
	return CoordinatorReadyPoolRegisterIdentityRequest{
		LeaseID:           readyPoolInputString(input, "leaseID"),
		Repo:              readyPoolInputString(input, "repo"),
		Ref:               readyPoolInputString(input, "ref"),
		Commit:            readyPoolInputString(input, "commit"),
		Fingerprint:       readyPoolInputString(input, "fingerprint"),
		CompatibilityKey:  readyPoolInputString(input, "compatibilityKey"),
		FillClaimToken:    readyPoolInputString(input, "fillClaimToken"),
		Identity:          identity,
		ReadinessEvidence: evidence,
		SSHHost:           readyPoolInputString(input, "sshHost"),
		SSHUser:           readyPoolInputString(input, "sshUser"),
		SSHPort:           readyPoolInputString(input, "sshPort"),
		WorkRoot:          readyPoolInputString(input, "workRoot"),
	}
}

func typedReadyPoolBorrowRequest(input map[string]any, identity CoordinatorReadyPoolIdentityV1) CoordinatorReadyPoolBorrowIdentityRequest {
	allowMissingCommit, _ := input["allowMissingCommit"].(bool)
	heartbeat, _ := input["heartbeat"].(bool)
	return CoordinatorReadyPoolBorrowIdentityRequest{
		Repo:               readyPoolInputString(input, "repo"),
		Ref:                readyPoolInputString(input, "ref"),
		Commit:             readyPoolInputString(input, "commit"),
		AllowMissingCommit: allowMissingCommit,
		Fingerprint:        readyPoolInputString(input, "fingerprint"),
		CompatibilityKey:   readyPoolInputString(input, "compatibilityKey"),
		Heartbeat:          heartbeat,
		Provider:           readyPoolInputString(input, "provider"),
		Target:             readyPoolInputString(input, "target"),
		Identity:           identity,
	}
}

func typedReadyPoolReconcileRequest(input map[string]any, identity CoordinatorReadyPoolIdentityV1, minReady, maxReady int, claim bool) CoordinatorReadyPoolReconcileIdentityRequest {
	return CoordinatorReadyPoolReconcileIdentityRequest{
		CoordinatorReadyPoolBorrowIdentityRequest: typedReadyPoolBorrowRequest(input, identity),
		MinReady: minReady,
		MaxReady: maxReady,
		Claim:    claim,
	}
}

func readyPoolRegisterCommit(cfg Config, repo Repo, ref, explicitCommit string) string {
	if explicitCommit = strings.TrimSpace(explicitCommit); explicitCommit != "" {
		return explicitCommit
	}
	cfg.Actions.Ref = strings.TrimSpace(ref)
	return prewarmReadyPoolCommit(cfg, repo, false)
}

func readyPoolRunBorrowInput(cfg Config, repo Repo, repoSlug string) map[string]any {
	input := readyPoolBorrowInput(repoSlug, firstNonBlank(cfg.Actions.Ref, repo.BaseRef), readyPoolRunBorrowCommit(cfg, repo), "", "", "", "")
	if readyPoolRunAllowsMissingCommit(cfg, repo) {
		input["allowMissingCommit"] = true
	}
	return input
}

func readyPoolRunBorrowInputForRun(cfg Config, repo Repo, repoSlug string, noSync bool) (map[string]any, error) {
	input := readyPoolRunBorrowInput(cfg, repo, repoSlug)
	input["heartbeat"] = true
	if !noSync {
		return input, nil
	}
	if readyPoolInputString(input, "commit") == "" {
		return nil, exit(2, "--pool --no-sync requires an exact commit match; omit --no-sync or use a checked-out branch/SHA ref")
	}
	delete(input, "allowMissingCommit")
	return input, nil
}

func readyPoolRunBorrowCommit(cfg Config, repo Repo) string {
	ref := strings.TrimSpace(cfg.Actions.Ref)
	if ref == "" || isGitCommitSHA(ref) {
		return strings.TrimSpace(repo.Head)
	}
	if repo.Root == "" {
		return ""
	}
	branch := strings.TrimSpace(gitOutput(repo.Root, "rev-parse", "--abbrev-ref", "HEAD"))
	if branch != "" && (ref == branch || ref == "refs/heads/"+branch) {
		return strings.TrimSpace(repo.Head)
	}
	return ""
}

func readyPoolRunAllowsMissingCommit(cfg Config, repo Repo) bool {
	ref := strings.TrimSpace(cfg.Actions.Ref)
	if ref == "" {
		return true
	}
	if isGitCommitSHA(ref) {
		return false
	}
	return true
}

func addStringInput(input map[string]any, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		input[key] = value
	}
}

func bestEffortGitHubRepoSlug(repo Repo, cfg Config) string {
	ghRepo, err := resolveGitHubRepo(repo, cfg.Actions.Repo)
	if err != nil {
		return ""
	}
	return ghRepo.Slug()
}

func readyPoolClaimSSHHost(leaseID string) string {
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return ""
	}
	return claim.SSHHost
}

func readyPoolClaimSSHPort(leaseID string) string {
	claim, err := readLeaseClaim(leaseID)
	if err != nil || claim.SSHPort <= 0 {
		return ""
	}
	return strconv.Itoa(claim.SSHPort)
}

func readyPoolClaimWorkRoot(leaseID string) string {
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return ""
	}
	return claim.Labels["work_root"]
}

func poolRegisterValueFlags() map[string]bool {
	return map[string]bool{
		"id": true, "repo": true, "ref": true, "commit": true, "fingerprint": true,
		"compatibility-key": true, "identity-file": true, "image": true,
		"ssh-host": true, "ssh-user": true, "ssh-port": true, "work-root": true,
	}
}

func poolBorrowValueFlags() map[string]bool {
	return map[string]bool{
		"repo": true, "ref": true, "commit": true, "fingerprint": true,
		"compatibility-key": true, "identity-file": true, "provider": true, "target": true,
	}
}

func validateReadyPoolReturnResult(result string) error {
	switch strings.TrimSpace(result) {
	case "ready", "drain", "release":
		return nil
	default:
		return exit(2, "--result must be ready, drain, or release")
	}
}

func validateReadyPoolRunReturnPolicy(policy string) error {
	switch strings.TrimSpace(policy) {
	case "", "auto", "ready", "drain", "release":
		return nil
	default:
		return exit(2, "--pool-return must be auto, ready, drain, or release")
	}
}

func readyPoolRunNeedsTrustedRemote(policy string) bool {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "drain", "release":
		return false
	default:
		return true
	}
}

func readyPoolRunShouldScrub(policy string, runFailure error) bool {
	switch strings.TrimSpace(policy) {
	case "drain", "release":
		return false
	case "ready", "", "auto":
		return runFailure == nil
	default:
		return false
	}
}

func readyPoolRunReturnResult(policy string, runFailure error, scrubErr error, metadataCompatible bool) string {
	switch strings.TrimSpace(policy) {
	case "drain", "release":
		return strings.TrimSpace(policy)
	case "ready":
		if readyPoolRunShouldScrub(policy, runFailure) && scrubErr == nil && metadataCompatible {
			return "ready"
		}
		return "drain"
	case "", "auto":
		if readyPoolRunShouldScrub(policy, runFailure) && scrubErr == nil && metadataCompatible {
			return "ready"
		}
		return "drain"
	default:
		return "drain"
	}
}

func readyPoolPreparedCommitMatches(recordedCommit, preparedCommit string) bool {
	recordedCommit = strings.TrimSpace(recordedCommit)
	return recordedCommit == "" || strings.EqualFold(recordedCommit, strings.TrimSpace(preparedCommit))
}

func readyPoolEntryRequiresHydration(entry CoordinatorReadyPoolEntry) bool {
	return strings.TrimSpace(entry.Commit) == ""
}

func readyPoolRunRequiresHydrationProof(entry CoordinatorReadyPoolEntry, hydratedByActions bool) bool {
	return hydratedByActions || readyPoolEntryRequiresHydration(entry)
}

func readyPoolReturnNeedsHydrationStop(result string) bool {
	return result == "drain" || result == "release"
}

func readyPoolRunReturnReason(runFailure error, result, preparedCommit string, scrubErr error, metadataCompatible bool) string {
	if result == "ready" {
		outcome := "run succeeded"
		if runFailure != nil {
			outcome = "run failed"
		}
		if preparedCommit != "" {
			return outcome + "; scrubbed for reuse at " + preparedCommit
		}
		return outcome + "; scrubbed for reuse"
	}
	if scrubErr != nil {
		return "pool scrub failed"
	}
	if !metadataCompatible {
		return "pool hydration or recorded commit is stale"
	}
	if runFailure != nil {
		return "run lifecycle failed"
	}
	return "pool drain requested"
}

func applyReadyPoolEndpoint(target SSHTarget, entry CoordinatorReadyPoolEntry) SSHTarget {
	if entry.SSHHost != "" {
		target.Host = entry.SSHHost
	}
	if entry.SSHUser != "" {
		target.User = entry.SSHUser
	}
	if entry.SSHPort != "" {
		target.Port = entry.SSHPort
		target.FallbackPorts = nil
	}
	return target
}

func countReadyPoolEntries(entries []CoordinatorReadyPoolEntry, borrowInput map[string]any) int {
	ready := 0
	now := time.Now()
	for _, entry := range entries {
		expiresAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(entry.ExpiresAt))
		if entry.State == "ready" && err == nil && expiresAt.After(now) && readyPoolEntryMatchesLegacyBorrowInput(entry, borrowInput) {
			ready++
		}
	}
	return ready
}

func readyPoolEntryMatchesLegacyBorrowInput(entry CoordinatorReadyPoolEntry, input map[string]any) bool {
	if entry.Identity != nil {
		return false
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "repo", value: entry.Repo},
		{name: "ref", value: entry.Ref},
		{name: "fingerprint", value: entry.Fingerprint},
		{name: "provider", value: entry.Provider},
		{name: "target", value: entry.TargetOS},
	} {
		if !readyPoolLegacyStringMatches(field.value, readyPoolInputString(input, field.name)) {
			return false
		}
	}
	commit := readyPoolInputString(input, "commit")
	if commit == "" || entry.Commit == commit {
		return true
	}
	allowMissingCommit, _ := input["allowMissingCommit"].(bool)
	return allowMissingCommit && entry.Commit == ""
}

func readyPoolLegacyStringMatches(got, want string) bool {
	want = strings.TrimSpace(want)
	return want == "" || strings.TrimSpace(got) == want
}

func readyPoolInputString(input map[string]any, key string) string {
	value, _ := input[key].(string)
	return strings.TrimSpace(value)
}

func renderReadyPoolEntries(out io.Writer, entries []CoordinatorReadyPoolEntry) {
	for _, entry := range entries {
		fmt.Fprintf(out, "%-22s %-16s %-12s %-18s provider=%s type=%s repo=%s ref=%s commit=%s ssh=%s@%s:%s\n",
			entry.Key,
			entry.LeaseID,
			entry.State,
			blank(entry.UpdatedAt, "-"),
			blank(entry.Provider, "-"),
			blank(entry.ServerType, "-"),
			blank(entry.Repo, "-"),
			blank(entry.Ref, "-"),
			shortCommit(entry.Commit),
			blank(entry.SSHUser, "-"),
			blank(entry.SSHHost, "-"),
			blank(entry.SSHPort, "-"),
		)
	}
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return blank(commit, "-")
}
