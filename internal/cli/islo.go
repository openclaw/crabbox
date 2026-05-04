package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
	"github.com/islo-labs/go-sdk/client"
	"github.com/islo-labs/go-sdk/customauth"
	"github.com/islo-labs/go-sdk/option"
)

const (
	isloProvider     = "islo"
	isloLeasePrefix  = "isb_"
	isloDefaultBase  = "https://api.islo.dev"
	isloEnvAPIKey    = "ISLO_API_KEY"
	isloEnvBaseURL   = "ISLO_BASE_URL"
	isloStatusPoll   = 2 * time.Second
	isloRunStatusPoll = 1 * time.Second
)

func isIsloProvider(provider string) bool {
	return provider == isloProvider
}

type isloRunOptions struct {
	ID         string
	Keep       bool
	Reclaim    bool
	Command    []string
	TimingJSON bool
}

type isloFlagValues struct {
	Image          *string
	Workdir        *string
	GatewayProfile *string
}

func registerIsloFlags(fs *flag.FlagSet, defaults Config) isloFlagValues {
	return isloFlagValues{
		Image:          fs.String("islo-image", defaults.Islo.Image, "islo sandbox image (e.g. docker.io/library/ubuntu:24.04)"),
		Workdir:        fs.String("islo-workdir", defaults.Islo.Workdir, "islo sandbox working directory"),
		GatewayProfile: fs.String("islo-gateway-profile", defaults.Islo.GatewayProfile, "islo gateway profile name or id"),
	}
}

func applyIsloFlagOverrides(cfg *Config, fs *flag.FlagSet, values isloFlagValues) {
	if flagWasSet(fs, "islo-image") {
		cfg.Islo.Image = *values.Image
	}
	if flagWasSet(fs, "islo-workdir") {
		cfg.Islo.Workdir = *values.Workdir
	}
	if flagWasSet(fs, "islo-gateway-profile") {
		cfg.Islo.GatewayProfile = *values.GatewayProfile
	}
}

type isloListItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Image     string `json:"image"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// IsloClient abstracts the SDK calls used by the islo provider so tests can
// inject a fake. The defaults wired through isloClientFactory call the real
// islo Go SDK.
type IsloClient interface {
	CreateSandbox(ctx context.Context, req *gosdk.SandboxCreate) (*gosdk.SandboxResponse, error)
	GetSandbox(ctx context.Context, name string) (*gosdk.SandboxResponse, error)
	ListSandboxes(ctx context.Context) ([]*gosdk.SandboxResponse, error)
	DeleteSandbox(ctx context.Context, name string) error
	ExecStream(ctx context.Context, name string, req *gosdk.ExecRequest, stdout, stderr io.Writer) (exitCode int, err error)
}

// isloClientFactory builds an IsloClient from an IsloConfig. Tests override
// it; production uses the SDK-backed implementation.
var isloClientFactory = func(_ Config) (IsloClient, error) {
	apiKey := getenv(isloEnvAPIKey, "")
	if apiKey == "" {
		return nil, exit(2, "provider=islo requires %s; set it to an islo API key (ak_...)", isloEnvAPIKey)
	}
	baseURL := getenv(isloEnvBaseURL, isloDefaultBase)
	c := client.NewIslo(option.WithAPIKey(apiKey), option.WithBaseURL(baseURL))
	auth := customauth.NewProvider(baseURL, apiKey, 0, nil)
	return &isloSDKClient{sdk: c, auth: auth, baseURL: baseURL}, nil
}

type isloSDKClient struct {
	sdk     *client.Client
	auth    *customauth.Provider
	baseURL string
}

func (c *isloSDKClient) CreateSandbox(ctx context.Context, req *gosdk.SandboxCreate) (*gosdk.SandboxResponse, error) {
	return c.sdk.Sandboxes.CreateSandbox(ctx, req)
}

func (c *isloSDKClient) GetSandbox(ctx context.Context, name string) (*gosdk.SandboxResponse, error) {
	return c.sdk.Sandboxes.GetSandbox(ctx, &gosdk.GetSandboxRequest{SandboxName: name})
}

func (c *isloSDKClient) ListSandboxes(ctx context.Context) ([]*gosdk.SandboxResponse, error) {
	page, err := c.sdk.Sandboxes.ListSandboxes(ctx, &gosdk.ListSandboxesRequest{})
	if err != nil {
		return nil, err
	}
	if page == nil {
		return nil, nil
	}
	return page.Items, nil
}

func (c *isloSDKClient) DeleteSandbox(ctx context.Context, name string) error {
	_, err := c.sdk.Sandboxes.DeleteSandbox(ctx, &gosdk.DeleteSandboxRequest{SandboxName: name})
	return err
}

// ExecStream bypasses the SDK's JSON-coalescing stream wrapper so callers see
// stdout/stderr deltas as they arrive. Auth and baseURL are reused from the
// SDK so this stays in sync with NewIslo.
func (c *isloSDKClient) ExecStream(ctx context.Context, name string, req *gosdk.ExecRequest, stdout, stderr io.Writer) (int, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return 1, fmt.Errorf("encode exec request: %w", err)
	}
	url := strings.TrimRight(c.baseURL, "/") + "/sandboxes/" + name + "/exec/stream"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 1, err
	}
	token, err := c.auth.Token(ctx)
	if err != nil {
		return 1, fmt.Errorf("islo auth: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 1, fmt.Errorf("islo exec stream %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	return parseIsloSSE(resp.Body, stdout, stderr)
}

// parseIsloSSE consumes the islo exec stream and writes stdout/stderr deltas
// to the matching writers. The wire format is standard SSE: each event has an
// `event:` type line, one or more `data:` lines (joined with \n per spec), and
// a blank-line terminator. Recognised event types are `stdout`, `stderr`, and
// `exit` (whose data is the exit code as a decimal string).
func parseIsloSSE(r io.Reader, stdout, stderr io.Writer) (int, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	exitCode := 0
	var event string
	var data []string
	flush := func() {
		if event == "" && len(data) == 0 {
			return
		}
		payload := strings.Join(data, "\n")
		switch event {
		case "stdout":
			_, _ = stdout.Write([]byte(payload))
		case "stderr":
			_, _ = stderr.Write([]byte(payload))
		case "exit":
			if n, err := strconv.Atoi(strings.TrimSpace(payload)); err == nil {
				exitCode = n
			}
		}
		event = ""
		data = data[:0]
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field = line
			value = ""
		}
		// Per SSE spec, a single leading space after the colon is stripped.
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		case "id", "retry":
			// ignored
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return exitCode, err
	}
	return exitCode, nil
}

func (a App) isloWarmup(ctx context.Context, cfg Config, repo Repo, keep, reclaim, timingJSON bool) error {
	started := time.Now()
	leaseID, sandboxName, slug, err := a.isloAcquireSandbox(ctx, cfg, repo, "", reclaim)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "leased %s slug=%s provider=%s sandbox=%s\n", leaseID, slug, isloProvider, sandboxName)
	if !keep {
		fmt.Fprintf(a.Stderr, "warning: islo warmup keeps the sandbox until idle expiry or explicit stop\n")
	}
	fmt.Fprintf(a.Stdout, "warmup complete total=%s\n", time.Since(started).Round(time.Millisecond))
	if timingJSON {
		total := time.Since(started)
		if err := writeTimingJSON(a.Stderr, timingReport{
			Provider: isloProvider,
			LeaseID:  leaseID,
			Slug:     slug,
			TotalMs:  total.Milliseconds(),
			ExitCode: 0,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (a App) isloRun(ctx context.Context, cfg Config, repo Repo, opts isloRunOptions) error {
	started := time.Now()
	c, err := isloClientFactory(cfg)
	if err != nil {
		return err
	}
	leaseID := ""
	sandboxName := ""
	acquired := false
	if opts.ID == "" {
		leaseID, sandboxName, _, err = a.isloAcquireSandboxWithClient(ctx, c, cfg, repo, "", opts.Reclaim)
		if err != nil {
			return err
		}
		acquired = true
	} else {
		leaseID, sandboxName, err = resolveIsloLeaseID(opts.ID, repo.Root, opts.Reclaim)
		if err != nil {
			return err
		}
		slug, err := isloClaimSlug(opts.ID, leaseID)
		if err != nil {
			return err
		}
		if err := claimLeaseForRepoProvider(leaseID, slug, isloProvider, repo.Root, cfg.IdleTimeout, opts.Reclaim); err != nil {
			return err
		}
	}
	if acquired && !opts.Keep {
		defer func() {
			if err := c.DeleteSandbox(context.Background(), sandboxName); err != nil {
				fmt.Fprintf(a.Stderr, "warning: islo stop failed for %s: %v\n", sandboxName, err)
				return
			}
			removeLeaseClaim(leaseID)
		}()
	}
	fmt.Fprintf(a.Stderr, "provider=islo id=%s sandbox=%s\n", leaseID, sandboxName)
	commandStart := time.Now()
	exitCodeVal, runErr := a.runIslo(ctx, c, cfg, sandboxName, opts.Command)
	commandDuration := time.Since(commandStart)
	total := time.Since(started)
	fmt.Fprintf(a.Stderr, "islo run summary command=%s total=%s exit=%d\n", commandDuration.Round(time.Millisecond), total.Round(time.Millisecond), exitCodeVal)
	if opts.TimingJSON {
		if err := writeTimingJSON(a.Stderr, timingReport{
			Provider:      isloProvider,
			LeaseID:       leaseID,
			SyncDelegated: true,
			SyncPhases:    []timingPhase{{Name: "delegated", Skipped: true, Reason: "islo owns sandbox state"}},
			CommandMs:     commandDuration.Milliseconds(),
			TotalMs:       total.Milliseconds(),
			ExitCode:      exitCodeVal,
		}); err != nil {
			return err
		}
	}
	if runErr != nil {
		return ExitError{Code: 1, Message: fmt.Sprintf("islo run failed: %v", runErr)}
	}
	if exitCodeVal != 0 {
		return ExitError{Code: exitCodeVal, Message: fmt.Sprintf("islo run exited %d", exitCodeVal)}
	}
	return nil
}

func (a App) isloList(ctx context.Context, cfg Config, jsonOut bool) error {
	c, err := isloClientFactory(cfg)
	if err != nil {
		return err
	}
	sandboxes, err := c.ListSandboxes(ctx)
	if err != nil {
		return isloError("list sandboxes", err)
	}
	items := make([]isloListItem, 0, len(sandboxes))
	for _, s := range sandboxes {
		if s == nil {
			continue
		}
		item := isloListItem{ID: s.ID, Name: s.Name, Status: s.Status, Image: s.Image}
		if s.CreatedAt != nil {
			item.CreatedAt = s.CreatedAt.UTC().Format(time.RFC3339)
		}
		items = append(items, item)
	}
	if jsonOut {
		enc := json.NewEncoder(a.Stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(items)
	}
	for _, item := range items {
		fmt.Fprintf(a.Stdout, "%-32s %-12s %-32s %s\n", item.Name, item.Status, item.Image, item.CreatedAt)
	}
	return nil
}

func (a App) isloStatus(ctx context.Context, cfg Config, id string, wait bool, waitTimeout time.Duration, jsonOut bool) error {
	c, err := isloClientFactory(cfg)
	if err != nil {
		return err
	}
	sandboxName, err := isloSandboxNameFromID(id)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(waitTimeout)
	for {
		sb, err := c.GetSandbox(ctx, sandboxName)
		if err != nil {
			return isloError(fmt.Sprintf("get sandbox %s", sandboxName), err)
		}
		ready := sb != nil && strings.EqualFold(sb.Status, "running")
		if jsonOut {
			if !wait || ready {
				enc := json.NewEncoder(a.Stdout)
				enc.SetEscapeHTML(false)
				return enc.Encode(sb)
			}
		} else {
			fmt.Fprintf(a.Stdout, "%s status=%s image=%s ready=%t\n", sb.Name, sb.Status, sb.Image, ready)
		}
		if !wait || ready {
			return nil
		}
		if time.Now().After(deadline) {
			return exit(5, "timed out waiting for sandbox %s to become ready", sandboxName)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(isloStatusPoll):
		}
	}
}

func (a App) isloStop(ctx context.Context, cfg Config, id string) error {
	c, err := isloClientFactory(cfg)
	if err != nil {
		return err
	}
	leaseID, sandboxName, err := resolveIsloLeaseID(id, "", false)
	if err != nil {
		return err
	}
	if err := c.DeleteSandbox(ctx, sandboxName); err != nil {
		return isloError(fmt.Sprintf("delete sandbox %s", sandboxName), err)
	}
	removeLeaseClaim(leaseID)
	fmt.Fprintf(a.Stderr, "released lease=%s sandbox=%s\n", leaseID, sandboxName)
	return nil
}

func (a App) isloAcquireSandbox(ctx context.Context, cfg Config, repo Repo, requestedName string, reclaim bool) (string, string, string, error) {
	c, err := isloClientFactory(cfg)
	if err != nil {
		return "", "", "", err
	}
	return a.isloAcquireSandboxWithClient(ctx, c, cfg, repo, requestedName, reclaim)
}

func (a App) isloAcquireSandboxWithClient(ctx context.Context, c IsloClient, cfg Config, repo Repo, requestedName string, reclaim bool) (string, string, string, error) {
	name := requestedName
	if name == "" {
		name = newIsloSandboxName(repo)
	}
	create := &gosdk.SandboxCreate{Name: stringPtr(name)}
	if cfg.Islo.Image != "" {
		create.Image = stringPtr(cfg.Islo.Image)
	}
	if cfg.Islo.Workdir != "" {
		create.Workdir = stringPtr(cfg.Islo.Workdir)
	}
	if cfg.Islo.GatewayProfile != "" {
		create.GatewayProfile = stringPtr(cfg.Islo.GatewayProfile)
	}
	if len(cfg.Islo.Env) > 0 {
		create.Env = make(map[string]*string, len(cfg.Islo.Env))
		for k, v := range cfg.Islo.Env {
			create.Env[k] = stringPtr(v)
		}
	}
	sb, err := c.CreateSandbox(ctx, create)
	if err != nil {
		return "", "", "", isloError(fmt.Sprintf("create sandbox %s", name), err)
	}
	if sb == nil || sb.Name == "" {
		return "", "", "", exit(5, "islo create sandbox returned no name")
	}
	leaseID := isloLeasePrefix + sb.Name
	slug := newLeaseSlug(leaseID)
	if err := claimLeaseForRepoProvider(leaseID, slug, isloProvider, repo.Root, cfg.IdleTimeout, reclaim); err != nil {
		_ = c.DeleteSandbox(context.Background(), sb.Name)
		return "", "", "", err
	}
	return leaseID, sb.Name, slug, nil
}

func (a App) runIslo(ctx context.Context, c IsloClient, cfg Config, sandboxName string, command []string) (int, error) {
	if len(command) == 0 {
		return 2, errors.New("missing command")
	}
	req := &gosdk.ExecRequest{Command: command}
	if cfg.Islo.Workdir != "" {
		req.Workdir = stringPtr(cfg.Islo.Workdir)
	}
	if len(cfg.Islo.Env) > 0 {
		req.Env = make(map[string]*string, len(cfg.Islo.Env))
		for k, v := range cfg.Islo.Env {
			req.Env[k] = stringPtr(v)
		}
	}
	return c.ExecStream(ctx, sandboxName, req, a.Stdout, a.Stderr)
}

func newIsloSandboxName(repo Repo) string {
	suffix := isloRandomSuffix()
	base := normalizeLeaseSlug(repo.Name)
	if base == "" {
		base = "crabbox"
	}
	return base + "-" + suffix
}

func isloRandomSuffix() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return time.Now().UTC().Format("150405")
	}
	return hex.EncodeToString(buf[:])
}

func resolveIsloLeaseID(identifier, repoRoot string, reclaim bool) (string, string, error) {
	if identifier == "" {
		return "", "", exit(2, "provider=islo requires --id <lease-or-sandbox-name>")
	}
	if strings.HasPrefix(identifier, isloLeasePrefix) {
		name := strings.TrimPrefix(identifier, isloLeasePrefix)
		if name == "" {
			return "", "", exit(2, "invalid islo lease %q", identifier)
		}
		return identifier, name, nil
	}
	claim, ok, err := resolveLeaseClaim(identifier)
	if err != nil {
		return "", "", err
	}
	if ok {
		if claim.Provider != "" && claim.Provider != isloProvider {
			return "", "", exit(4, "%q is claimed by provider %s", identifier, claim.Provider)
		}
		if repoRoot != "" && claim.RepoRoot != "" && claim.RepoRoot != repoRoot && !reclaim {
			return "", "", exit(2, "lease %s is claimed by repo %s; use --reclaim to claim it for %s", claim.LeaseID, claim.RepoRoot, repoRoot)
		}
		return claim.LeaseID, strings.TrimPrefix(claim.LeaseID, isloLeasePrefix), nil
	}
	leaseID := isloLeasePrefix + identifier
	return leaseID, identifier, nil
}

func isloSandboxNameFromID(id string) (string, error) {
	if id == "" {
		return "", exit(2, "usage: crabbox status --id <lease-or-sandbox-name>")
	}
	leaseID, name, err := resolveIsloLeaseID(id, "", false)
	if err != nil {
		return "", err
	}
	_ = leaseID
	return name, nil
}

func isloClaimSlug(identifier, leaseID string) (string, error) {
	for _, candidate := range []string{identifier, leaseID} {
		claim, ok, err := resolveLeaseClaim(candidate)
		if err != nil {
			return "", err
		}
		if ok && claim.LeaseID == leaseID {
			return claim.Slug, nil
		}
	}
	return newLeaseSlug(leaseID), nil
}

func isloError(action string, err error) error {
	if err == nil {
		return nil
	}
	var unauth *gosdk.UnauthorizedError
	if errors.As(err, &unauth) {
		return exit(2, "%s: islo auth rejected; check %s is set to a valid ak_... key", action, isloEnvAPIKey)
	}
	var notFound *gosdk.NotFoundError
	if errors.As(err, &notFound) {
		return exit(4, "%s: not found", action)
	}
	var conflict *gosdk.ConflictError
	if errors.As(err, &conflict) {
		return exit(4, "%s: conflict (%v)", action, err)
	}
	var payment *gosdk.PaymentRequiredError
	if errors.As(err, &payment) {
		return exit(2, "%s: payment required (%v)", action, err)
	}
	return ExitError{Code: 5, Message: fmt.Sprintf("%s: %v", action, err)}
}

func stringPtr(s string) *string { return &s }
