package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

func (a App) readyPoolList(ctx context.Context, args []string) error {
	fs := newFlagSet("pool ready", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	identityFile := fs.String("identity-file", "", "generated typed ready-pool identity JSON")
	args, key := extractFirstPositionalArg(args, map[string]bool{"identity-file": true})
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	coord, err := readyPoolCoordinator()
	if err != nil {
		return err
	}
	var entries []CoordinatorReadyPoolEntry
	if flagWasSet(fs, "identity-file") {
		identity, loadErr := loadReadyPoolIdentity(*identityFile)
		if loadErr != nil {
			return loadErr
		}
		if key == "" {
			return exit(2, "typed ready-pool listing requires a pool key")
		}
		entries, err = coord.TypedReadyPool(ctx, key)
		if err == nil {
			filtered := entries[:0]
			for _, entry := range entries {
				if entry.Identity != nil && readyPoolIdentitiesEqual(*entry.Identity, identity) {
					filtered = append(filtered, entry)
				}
			}
			entries = filtered
		}
	} else if key == "" {
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

func (a App) readyPoolIdentity(ctx context.Context, args []string) error {
	fs := newFlagSet("pool identity", a.Stderr)
	id := fs.String("id", "", "existing coordinator lease id")
	repoFlag := fs.String("repo", "", "repository owner/name")
	ref := fs.String("ref", "", "source ref")
	commit := fs.String("commit", "", "source commit")
	fingerprint := fs.String("fingerprint", "", "repository setup fingerprint")
	cacheCompatibility := fs.String("cache-compatibility", "", "operator-declared exact-match cache compatibility")
	args, key := extractFirstPositionalArg(args, map[string]bool{
		"id": true, "repo": true, "ref": true, "commit": true, "fingerprint": true,
		"cache-compatibility": true,
	})
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if key == "" || strings.TrimSpace(*id) == "" || strings.TrimSpace(*cacheCompatibility) == "" {
		return exit(2, "usage: crabbox pool identity <key> --id <lease-id> --cache-compatibility <value>")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	repo, _ := findRepo()
	input := readyPoolIdentityMetadata(cfg, repo, *repoFlag, *ref, *commit, *fingerprint)
	input["leaseID"] = strings.TrimSpace(*id)
	input["cacheCompatibility"] = strings.TrimSpace(*cacheCompatibility)
	coord, err := readyPoolCoordinatorFromConfig(cfg)
	if err != nil {
		return err
	}
	identity, err := coord.GenerateReadyPoolIdentity(ctx, key, input)
	if err != nil {
		return err
	}
	if err := validateReadyPoolSeedIdentity(identity, input); err != nil {
		return err
	}
	encoder := json.NewEncoder(a.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(identity)
}

func (a App) readyPoolRegister(ctx context.Context, args []string) error {
	fs := newFlagSet("pool register", a.Stderr)
	id := fs.String("id", "", "lease id")
	repoFlag := fs.String("repo", "", "repository owner/name")
	ref := fs.String("ref", "", "source ref")
	commit := fs.String("commit", "", "source commit")
	fingerprint := fs.String("fingerprint", "", "repo setup fingerprint")
	compatibilityKey := fs.String("compatibility-key", "", "provider-neutral capability and size key")
	identityFile := fs.String("identity-file", "", "generated typed ready-pool identity JSON")
	cacheCompatibility := fs.String("cache-compatibility", "", "derive typed identity using this operator-declared cache compatibility")
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
	if flagWasSet(fs, "identity-file") && flagWasSet(fs, "cache-compatibility") {
		return exit(2, "--identity-file and --cache-compatibility are mutually exclusive")
	}
	if flagWasSet(fs, "cache-compatibility") && strings.TrimSpace(*cacheCompatibility) == "" {
		return exit(2, "--cache-compatibility must not be empty")
	}
	var identity *CoordinatorReadyPoolIdentityV1
	if flagWasSet(fs, "identity-file") {
		parsed, identityErr := loadReadyPoolIdentity(*identityFile)
		if identityErr != nil {
			return identityErr
		}
		identity = &parsed
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
	if flagWasSet(fs, "cache-compatibility") {
		generationInput := mapsCloneAny(input)
		generationInput["cacheCompatibility"] = strings.TrimSpace(*cacheCompatibility)
		generated, generationErr := coord.GenerateReadyPoolIdentity(ctx, key, generationInput)
		if generationErr != nil {
			return generationErr
		}
		identity = &generated
	}
	if identity != nil {
		if err := validateReadyPoolSeedIdentity(*identity, input); err != nil {
			return err
		}
		input["identity"] = *identity
		res, err = coord.RegisterTypedReadyPoolLease(ctx, key, input)
		if err == nil {
			err = validateTypedReadyPoolResponseIdentity(res, *identity)
		}
	} else {
		res, err = coord.RegisterReadyPoolLease(ctx, key, input)
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
	repo := fs.String("repo", "", "repository owner/name")
	ref := fs.String("ref", "", "source ref")
	commit := fs.String("commit", "", "source commit")
	fingerprint := fs.String("fingerprint", "", "repo setup fingerprint")
	compatibilityKey := fs.String("compatibility-key", "", "provider-neutral capability and size key")
	identityFile := fs.String("identity-file", "", "generated typed ready-pool identity JSON")
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
	var identity *CoordinatorReadyPoolIdentityV1
	if flagWasSet(fs, "identity-file") {
		parsed, identityErr := loadReadyPoolIdentity(*identityFile)
		if identityErr != nil {
			return identityErr
		}
		identity = &parsed
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	coord, err := readyPoolCoordinatorFromConfig(cfg)
	if err != nil {
		return err
	}
	input := readyPoolBorrowInput(*repo, *ref, *commit, *fingerprint, *compatibilityKey, *provider, *target)
	var res CoordinatorReadyPoolResponse
	if identity != nil {
		localRepo, _ := findRepo()
		metadata := readyPoolIdentityMetadata(cfg, localRepo, *repo, *ref, *commit, *fingerprint)
		for field, value := range metadata {
			input[field] = value
		}
		input["identity"] = *identity
		res, err = borrowValidatedTypedReadyPoolLease(ctx, coord, key, input, *identity)
	} else {
		res, err = coord.BorrowReadyPoolLease(ctx, key, input)
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
	identityFile := fs.String("identity-file", "", "generated typed ready-pool identity JSON")
	jsonOut := fs.Bool("json", false, "print JSON")
	args, key := extractFirstPositionalArg(args, map[string]bool{"id": true, "borrow-token": true, "identity-file": true})
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
	var res CoordinatorReadyPoolResponse
	if flagWasSet(fs, "identity-file") {
		if _, identityErr := loadReadyPoolIdentity(*identityFile); identityErr != nil {
			return identityErr
		}
		res, err = coord.HeartbeatTypedReadyPoolBorrow(ctx, key, *id, *borrowToken)
	} else {
		res, err = coord.HeartbeatReadyPoolBorrow(ctx, key, *id, *borrowToken)
	}
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
	identityFile := fs.String("identity-file", "", "generated typed ready-pool identity JSON")
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
	if flagWasSet(fs, "identity-file") {
		identity, identityErr := loadReadyPoolIdentity(*identityFile)
		if identityErr != nil {
			return identityErr
		}
		res, err = coord.ReturnTypedReadyPoolLease(ctx, key, map[string]any{
			"leaseID": *id, "result": *result, "reason": *reason,
			"borrowToken": *borrowToken, "identity": identity,
		})
	} else {
		res, err = coord.ReturnReadyPoolLease(ctx, key, *id, *result, *reason, *borrowToken)
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
	identityFile := fs.String("identity-file", "", "generated typed ready-pool identity JSON")
	create := fs.Bool("create", false, "claim and create missing ready leases with prewarm")
	jsonOut := fs.Bool("json", false, "print JSON")
	args, key := extractFirstPositionalArg(args, map[string]bool{"min-ready": true, "max-ready": true, "compatibility-key": true, "identity-file": true})
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if key == "" {
		return exit(2, "usage: crabbox pool ensure <key> [--create] [prewarm flags...]")
	}
	var identity *CoordinatorReadyPoolIdentityV1
	if flagWasSet(fs, "identity-file") {
		parsed, identityErr := loadReadyPoolIdentity(*identityFile)
		if identityErr != nil {
			return identityErr
		}
		identity = &parsed
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
	if identity != nil {
		delete(borrowInput, "allowMissingCommit")
		if err := validateReadyPoolSeedIdentity(*identity, borrowInput); err != nil {
			return err
		}
		borrowInput["identity"] = *identity
	}
	prewarmArgs := append([]string{}, fs.Args()...)
	prewarmArgs = append(prewarmArgs, "--pool", key)
	if strings.TrimSpace(*compatibilityKey) != "" {
		prewarmArgs = append(prewarmArgs, "--pool-compatibility-key", strings.TrimSpace(*compatibilityKey))
	}
	if identity != nil {
		prewarmArgs = append(prewarmArgs, "--pool-identity-file", *identityFile)
	}
	prewarmApp := a
	if *jsonOut {
		prewarmApp.Stdout = a.Stderr
	}
	for {
		var res CoordinatorReadyPoolReconcileResponse
		if identity != nil {
			res, err = coord.ReconcileTypedReadyPool(ctx, key, borrowInput)
		} else {
			res, err = coord.ReconcileReadyPool(ctx, key, borrowInput)
		}
		if err != nil {
			if identity == nil && readyPoolCoordinatorRouteUnsupported(err) {
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
			var releaseErr error
			if identity != nil {
				releaseErr = coord.ReleaseTypedReadyPoolFillClaim(releaseCtx, key, res.Claim.Token)
			} else {
				releaseErr = coord.ReleaseReadyPoolFillClaim(releaseCtx, key, res.Claim.Token)
			}
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

func readyPoolIdentityMetadata(cfg Config, repo Repo, repository, ref, commit, fingerprint string) map[string]any {
	input := map[string]any{}
	addStringInput(input, "repo", firstNonBlank(repository, cfg.Actions.Repo, bestEffortGitHubRepoSlug(repo, cfg)))
	refValue := firstNonBlank(ref, cfg.Actions.Ref, repo.BaseRef)
	addStringInput(input, "ref", refValue)
	addStringInput(input, "commit", readyPoolRegisterCommit(cfg, repo, refValue, commit))
	addStringInput(input, "fingerprint", fingerprint)
	return input
}

func mapsCloneAny(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func borrowValidatedTypedReadyPoolLease(ctx context.Context, coord *CoordinatorClient, key string, input map[string]any, identity CoordinatorReadyPoolIdentityV1) (CoordinatorReadyPoolResponse, error) {
	if err := validateReadyPoolSeedIdentity(identity, input); err != nil {
		return CoordinatorReadyPoolResponse{}, err
	}
	response, err := coord.BorrowTypedReadyPoolLease(ctx, key, input)
	if err != nil {
		return response, err
	}
	if err = validateTypedReadyPoolResponseIdentity(response, identity); err == nil {
		err = readyPoolIdentityMatchesLease(identity, response.Lease)
	}
	if err == nil {
		return response, nil
	}
	if response.Entry.LeaseID != "" && response.Entry.BorrowToken != "" {
		returnInput := map[string]any{
			"leaseID": response.Entry.LeaseID, "borrowToken": response.Entry.BorrowToken,
			"result": "drain", "reason": "typed ready-pool identity mismatch",
		}
		if response.Entry.Identity != nil {
			returnInput["identity"] = *response.Entry.Identity
		}
		drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_, drainErr := coord.ReturnTypedReadyPoolLease(drainCtx, key, returnInput)
		cancel()
		if drainErr != nil {
			return response, fmt.Errorf("%w; drain mismatched ready-pool entry: %v", err, drainErr)
		}
	}
	return response, err
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
		"compatibility-key": true, "identity-file": true, "cache-compatibility": true, "image": true,
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
