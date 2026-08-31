package blacksmith

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const blacksmithDefaultAPI = "https://backend.blacksmith.sh"

type blacksmithRoute struct {
	API string `json:"api"`
	Org string `json:"org"`
}

func (r blacksmithRoute) canonical() (string, error) {
	u, err := url.Parse(r.API)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", exit(2, "Blacksmith ownership requires an API URL without credentials, query, or fragment")
	}
	if r.Org == "" || strings.TrimSpace(r.Org) != r.Org || strings.ContainsAny(r.Org, "\r\n\x00") {
		return "", exit(2, "Blacksmith ownership requires an explicit authenticated organization")
	}
	data, err := json.Marshal(r)
	return string(data), err
}

func (b *blacksmithBackend) withRoute(ctx context.Context) (*blacksmithBackend, error) {
	if b.route != nil {
		return b, nil
	}
	route := blacksmithRoute{API: strings.TrimSpace(os.Getenv("BLACKSMITH_API_URL")), Org: strings.TrimSpace(firstNonBlank(b.cfg.Blacksmith.Org, os.Getenv("BLACKSMITH_ORG")))}
	if route.API == "" {
		route.API = blacksmithDefaultAPI
	}
	if route.Org == "" {
		output, err := b.commandOutput(ctx, []string{"auth", "status"})
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 3 && fields[0] == "*" && fields[2] == "(current)" {
				if route.Org != "" {
					return nil, exit(2, "ambiguous Blacksmith organization; pass --blacksmith-org")
				}
				route.Org = fields[1]
			}
		}
	}
	if _, err := route.canonical(); err != nil {
		return nil, err
	}
	bound := *b
	bound.route = &route
	return &bound, nil
}

func blacksmithClaimBinding(claim core.LeaseClaim) (blacksmithRoute, shared.ClaimBinding, error) {
	var route blacksmithRoute
	want := shared.ClaimBinding{Provider: blacksmithTestboxProvider, LeaseID: claim.LeaseID, CloudID: claim.LeaseID, Slug: claim.Slug, ProviderScope: claim.ProviderScope, ExactProviderScope: true}
	if parseBlacksmithID(claim.LeaseID) != claim.LeaseID || claim.LeaseID == "" || claim.Revision == "" || claim.Slug == "" || claim.RepoRoot == "" || json.Unmarshal([]byte(claim.ProviderScope), &route) != nil {
		return route, want, exit(2, "Blacksmith lease has no exact ownership binding; inspect and clean up through Blacksmith directly; --reclaim cannot adopt legacy or lost state")
	}
	scope, err := route.canonical()
	if err != nil || scope != claim.ProviderScope {
		return route, want, exit(2, "Blacksmith lease has an invalid organization/API binding")
	}
	for _, key := range []string{"workflow", "job", "ref"} {
		if value := claim.Labels[key]; value == "" || strings.ContainsAny(value, "\r\n\x00") {
			return route, want, exit(2, "Blacksmith lease has no exact %s binding", key)
		}
	}
	return route, want, shared.ValidateClaimBinding(claim, want)
}

func resolveOwnedBlacksmithClaim(id string) (core.LeaseClaim, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return core.LeaseClaim{}, exit(2, "Blacksmith requires a claimed Testbox ID or slug")
	}
	claim, ok, exact, err := core.ResolveLeaseClaimForProviderWithExact(id, blacksmithTestboxProvider)
	if err != nil {
		return claim, err
	}
	if !ok || ((strings.HasPrefix(id, "tbx_") || core.IsCanonicalLeaseID(id)) && (!exact || claim.LeaseID != id)) {
		return claim, exit(4, "Blacksmith resource %q has no exact local ownership claim; use read-only status/list and native Blacksmith cleanup", id)
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return claim, err
	}
	for _, other := range claims {
		if other.LeaseID != claim.LeaseID && other.CloudID == claim.CloudID && claim.CloudID != "" {
			return claim, exit(2, "Blacksmith Testbox has conflicting local claims")
		}
	}
	_, _, err = blacksmithClaimBinding(claim)
	return claim, err
}

type blacksmithIdentity struct {
	ID, State, Workflow, Job, Ref string
}

var blacksmithStatusHeader = regexp.MustCompile(`^(ID) {2,}(STATUS) {2,}(IP) {2,}(WORKFLOW) {2,}(JOB) {2,}(REF) {2,}(CREATED) {2,}(RUN URL)$`)
var blacksmithStatusRunURL = regexp.MustCompile(`^https://github\.com/[^/\s]+/[^/\s]+/actions/runs/[0-9]+$`)

// Native status is a fixed-column table, including an empty IP for terminal
// resources. Do not infer completion from list absence or a discarded row.
func parseBlacksmithIdentity(output, id string) (blacksmithIdentity, error) {
	var identity blacksmithIdentity
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) != 2 {
		return identity, exit(2, "Blacksmith exact status requires one unambiguous row")
	}
	columns := blacksmithStatusHeader.FindStringSubmatchIndex(lines[0])
	if columns == nil || strings.ContainsAny(lines[1], "\t\r") {
		return identity, exit(2, "Blacksmith exact status has an unsupported table")
	}
	row := []rune(lines[1])
	var values [8]string
	for i := range values {
		start, end := columns[2+i*2], len(row)
		if i+1 < len(values) {
			end = columns[2+(i+1)*2]
		}
		if i >= 6 && start >= len(row) {
			continue // Queued records may not yet have a timestamp or Actions URL.
		}
		if start >= len(row) || end > len(row) {
			return identity, exit(2, "Blacksmith exact status has an incomplete row")
		}
		cell := string(row[start:end])
		if i+1 < len(values) && !strings.HasSuffix(cell, "  ") {
			return identity, exit(2, "Blacksmith exact status has misaligned columns")
		}
		values[i] = strings.TrimRight(cell, " ")
		if strings.HasPrefix(values[i], " ") {
			return identity, exit(2, "Blacksmith exact status has misaligned cells")
		}
	}
	identity = blacksmithIdentity{ID: values[0], State: values[1], Workflow: values[3], Job: values[4], Ref: values[5]}
	if identity.ID != id || identity.Workflow == "" || identity.Job == "" || identity.Ref == "" || !identity.knownState() {
		return identity, exit(2, "Blacksmith exact status has missing or mismatched identity/state")
	}
	if identity.terminal() && (values[6] == "" || !blacksmithStatusRunURL.MatchString(values[7])) {
		return identity, exit(2, "Blacksmith completion requires complete native metadata")
	}
	return identity, nil
}

func (i blacksmithIdentity) terminal() bool {
	return i.State == "completed"
}

func (i blacksmithIdentity) knownState() bool {
	return i.terminal() || i.State == "queued" || i.State == "hydrating" || i.State == "hydration_failed" || i.State == "ready" || i.State == "running" || i.State == "in_progress"
}

func (i blacksmithIdentity) usable() error {
	if i.State == "hydration_failed" {
		return exit(2, "Blacksmith Testbox hydration failed; stop the lease and create a new one")
	}
	if i.terminal() {
		return exit(2, "Blacksmith Testbox has finished; create a new lease")
	}
	return nil
}

func (b *blacksmithBackend) inspectTestbox(ctx context.Context, id string) (blacksmithIdentity, error) {
	result, err := b.runCommand(ctx, []string{"testbox", "status", "--id", id}, nil, nil)
	err = blacksmithContextError(ctx, err)
	if err == nil && result.ExitCode != 0 {
		err = exit(result.ExitCode, "Blacksmith exact status failed")
	}
	if err != nil {
		if diagnostic := strings.TrimSpace(result.Stderr); diagnostic != "" {
			err = fmt.Errorf("%w: %s", err, diagnostic)
		}
		return blacksmithIdentity{}, blacksmithExitDiagnostics(err)
	}
	return parseBlacksmithIdentity(result.Stdout, id)
}

// Present all diagnostics through AsExitError while retaining the native code
// and causes. The CLI prints only the first ExitError's Message.
type blacksmithCommandError struct {
	core.ExitError
	cause error
}

func (e blacksmithCommandError) Unwrap() []error { return []error{e.ExitError, e.cause} }

func blacksmithExitDiagnostics(err error) error {
	var exitErr core.ExitError
	if core.AsExitError(err, &exitErr) {
		exitErr.Message = err.Error()
		return blacksmithCommandError{ExitError: exitErr, cause: err}
	}
	return err
}

func blacksmithContextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		err = errors.Join(err, ctxErr)
	}
	if cause := context.Cause(ctx); cause != nil && !errors.Is(err, cause) {
		err = errors.Join(err, cause)
	}
	return err
}

func (b *blacksmithBackend) verifyTestbox(ctx context.Context, claim core.LeaseClaim) (blacksmithIdentity, error) {
	route, _, err := blacksmithClaimBinding(claim)
	if err != nil {
		return blacksmithIdentity{}, err
	}
	if b.route == nil || *b.route != route {
		return blacksmithIdentity{}, exit(2, "Blacksmith organization/API changed; retaining exact claim")
	}
	identity, err := b.inspectTestbox(ctx, claim.CloudID)
	if err != nil {
		return identity, err
	}
	if identity.Workflow != claim.Labels["workflow"] || identity.Job != claim.Labels["job"] || identity.Ref != claim.Labels["ref"] {
		return identity, exit(2, "Blacksmith Testbox workflow identity changed; retaining exact claim")
	}
	return identity, nil
}

func (b *blacksmithBackend) ownedTestbox(ctx context.Context, id, repoRoot string, reclaim bool) (*blacksmithBackend, core.LeaseClaim, error) {
	claim, err := resolveOwnedBlacksmithClaim(id)
	if err != nil {
		return nil, claim, err
	}
	var bound *blacksmithBackend
	verify := func() error {
		var err error
		bound, err = b.withRoute(ctx)
		if err != nil {
			return err
		}
		identity, err := bound.verifyTestbox(ctx, claim)
		if err == nil {
			err = identity.usable()
		}
		return err
	}
	if repoRoot == "" {
		err = core.WithLeaseClaimUnchangedShared(ctx, claim.LeaseID, claim, verify)
	} else {
		server := Server{Provider: blacksmithTestboxProvider, CloudID: claim.CloudID, Labels: shared.CloneLabels(claim.Labels)}
		cfg := b.cfg
		cfg.Provider = blacksmithTestboxProvider
		claim, err = core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurableAfterContext(ctx, claim.LeaseID, claim.Slug, cfg, claim.ProviderScope, server, core.SSHTarget{}, repoRoot, blacksmithIdleTimeout(b.cfg), reclaim, claim, true, verify)
	}
	if err == nil {
		copy := *bound
		copy.claim = &claim
		bound = &copy
	}
	return bound, claim, err
}

func (b *blacksmithBackend) withOwnedTestbox(ctx context.Context, claim core.LeaseClaim, action func() error) error {
	return core.WithLeaseClaimUnchangedShared(ctx, claim.LeaseID, claim, func() error {
		identity, err := b.verifyTestbox(ctx, claim)
		if err != nil {
			return err
		}
		if err := identity.usable(); err != nil {
			return err
		}
		return action()
	})
}

type blacksmithReconciledStop struct {
	result LocalCommandResult
	err    error
}

func (b *blacksmithBackend) printStopOutput(result LocalCommandResult) {
	if b.rt.Stdout != nil {
		fmt.Fprint(b.rt.Stdout, result.Stdout)
	}
	if b.rt.Stderr != nil {
		fmt.Fprint(b.rt.Stderr, result.Stderr)
	}
}

func (b *blacksmithBackend) terminateTestbox(ctx context.Context, claim core.LeaseClaim) (*blacksmithReconciledStop, error) {
	identity, err := b.verifyTestbox(ctx, claim)
	if err != nil || identity.terminal() {
		return nil, err
	}
	result, stopErr := b.runCommand(ctx, blacksmithStopArgs(b.cfg, claim.CloudID), nil, nil)
	if stopErr != nil {
		statusErr := blacksmithContextError(ctx, nil)
		if statusErr == nil {
			var observed blacksmithIdentity
			observed, statusErr = b.verifyTestbox(ctx, claim)
			if statusErr == nil && observed.terminal() {
				return &blacksmithReconciledStop{result: result, err: stopErr}, nil
			}
			if statusErr == nil {
				statusErr = fmt.Errorf("Testbox state=%s is not completed", observed.State)
			}
		}
		stopErr = errors.Join(stopErr, fmt.Errorf("Blacksmith stop verification failed: %w", statusErr))
	}
	// A native error is suppressed only by fresh exact completion evidence.
	b.printStopOutput(result)
	if stopErr != nil {
		return nil, stopErr
	}
	for {
		identity, err = b.verifyTestbox(ctx, claim)
		if err != nil || identity.terminal() {
			return nil, err
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, context.Cause(ctx)
		case <-timer.C:
		}
	}
}

// Only a unique bare receipt emitted by this warmup nominates a rollback
// resource. Arbitrary ID mentions in diagnostics are not creation receipts.
func blacksmithCreationReceipt(output string) string {
	id := ""
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "tbx_") && parseBlacksmithID(line) == line {
			if id != "" {
				return ""
			}
			id = line
		}
	}
	return id
}

func blacksmithIdentityClaim(id, slug, repoRoot string, route blacksmithRoute, identity blacksmithIdentity) core.LeaseClaim {
	scope, _ := route.canonical()
	return core.LeaseClaim{Provider: blacksmithTestboxProvider, LeaseID: id, CloudID: id, Slug: slug, RepoRoot: repoRoot, ProviderScope: scope, Revision: "creation-receipt", Labels: map[string]string{"provider": blacksmithTestboxProvider, "lease": id, "slug": slug, "workflow": identity.Workflow, "job": identity.Job, "ref": identity.Ref}}
}

func (b *blacksmithBackend) rollbackTestbox(id, pendingID, repoRoot string) {
	ctx, cancel := context.WithTimeout(context.Background(), blacksmithCleanupTimeout)
	defer cancel()
	err := core.CleanupLeaseClaimIfUnchangedAfterContext(ctx, id, core.LeaseClaim{}, false, func() error {
		if pendingID != id {
			path, err := testboxKeyPath(id)
			if err != nil {
				return err
			}
			if _, err := os.Lstat(filepath.Dir(path)); !os.IsNotExist(err) {
				return exit(2, "Blacksmith rollback conflicts with existing key state")
			}
		}
		identity, err := b.inspectTestbox(ctx, id)
		if err != nil {
			return err
		}
		claim := blacksmithIdentityClaim(id, "acquisition-rollback", repoRoot, *b.route, identity)
		if _, err := b.terminateTestbox(ctx, claim); err != nil {
			return err
		}
		if err := core.RemoveStoredTestboxConnectionArtifacts(pendingID); err != nil {
			return fmt.Errorf("Blacksmith local connection artifacts cleanup failed: %w", err)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: Blacksmith acquisition cleanup unconfirmed resource=%s pending_key=%s; inspect through Blacksmith directly: %v\n", id, pendingID, err)
	}
}

func moveFreshBlacksmithKey(pendingID, id string) error {
	path, err := testboxKeyPath(id)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Dir(path)); !os.IsNotExist(err) {
		return exit(2, "Blacksmith resource %s already has local key state; refusing to replace it", id)
	}
	pending, err := testboxKeyPath(pendingID)
	if err != nil {
		return err
	}
	for _, name := range []string{pending, pending + ".pub"} {
		info, err := os.Lstat(name)
		if err != nil || !info.Mode().IsRegular() {
			return exit(2, "Blacksmith pending key pair is missing or not regular")
		}
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(path)), 0o700); err != nil {
		return err
	}
	return os.Rename(filepath.Dir(pending), filepath.Dir(path))
}
