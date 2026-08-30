package tensorlake

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type tensorlakeScope struct {
	APIURL        string `json:"apiUrl"`
	SandboxAPIURL string `json:"sandboxApiUrl"`
	Organization  string `json:"organization"`
	Project       string `json:"project"`
	Namespace     string `json:"namespace"`
}

type sandboxIdentity struct{ ID, Name, Namespace, State string }
type sandboxBinding struct{ ID, Namespace, Scope string }

func canonicalTensorlakeURL(value string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" {
		return "", exit(2, "Tensorlake API URL must be an absolute URL without credentials, query, or fragment")
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", exit(2, "Tensorlake API URL requires HTTP or HTTPS")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

func validScopeValue(value string) bool {
	if value == "" || value == "-" || value == "." || value == ".." || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// whoami includes credential prefixes. No control response or native error may
// escape this capture path, including malformed JSON and nonzero exits.
func (c *tensorlakeCLI) control(ctx context.Context, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	const limit = 1024 * 1024
	result, err := c.rt.Exec.Run(ctx, LocalCommandRequest{
		Name: c.binary(), Args: append(c.globalArgs(), args...), Env: c.env(),
		MaxCapturedOutputBytes: limit, CancelGracePeriod: time.Second,
	})
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil || result.ExitCode != 0 || len(result.Stdout) >= limit || len(result.Stderr) >= limit {
		return "", exit(5, "Tensorlake ownership control command failed; verify authentication and native CLI support (response withheld)")
	}
	return result.Stdout, nil
}

func (c *tensorlakeCLI) observeScope(ctx context.Context) (string, error) {
	out, err := c.control(ctx, []string{"whoami", "--output", "json"})
	if err != nil {
		return "", err
	}
	var identity struct {
		Endpoints struct {
			CloudAPI   string `json:"cloudApi"`
			SandboxAPI string `json:"sandboxApi"`
		} `json:"endpoints"`
		APIKey struct {
			Organization string `json:"organizationId"`
			Project      string `json:"projectId"`
		} `json:"apiKey"`
	}
	if err := json.Unmarshal([]byte(out), &identity); err != nil {
		return "", exit(5, "Tensorlake whoami did not return valid scope JSON; upgrade the native CLI")
	}
	api, err := canonicalTensorlakeURL(identity.Endpoints.CloudAPI)
	if err != nil {
		return "", exit(5, "Tensorlake whoami returned an invalid cloud API scope")
	}
	sandboxAPI, err := canonicalTensorlakeURL(identity.Endpoints.SandboxAPI)
	if err != nil {
		return "", exit(5, "Tensorlake whoami returned an invalid sandbox API scope")
	}
	if api != c.cfg.Tensorlake.APIURL || !validScopeValue(identity.APIKey.Organization) || !validScopeValue(identity.APIKey.Project) {
		return "", exit(2, "Tensorlake ownership requires a verified API endpoint and API-key organization/project scope")
	}
	if org := strings.TrimSpace(c.cfg.Tensorlake.OrganizationID); org != "" && org != identity.APIKey.Organization {
		return "", exit(2, "Tensorlake API-key organization differs from the configured organization")
	}
	if project := strings.TrimSpace(c.cfg.Tensorlake.ProjectID); project != "" && project != identity.APIKey.Project {
		return "", exit(2, "Tensorlake API-key project differs from the configured project")
	}
	data, err := json.Marshal(tensorlakeScope{api, sandboxAPI, identity.APIKey.Organization, identity.APIKey.Project, c.cfg.Tensorlake.Namespace})
	return string(data), err
}

func parseSandboxIdentity(out string) (sandboxIdentity, error) {
	var item sandboxIdentity
	fields := map[string]*string{"ID": &item.ID, "Name": &item.Name, "Namespace": &item.Namespace, "Status": &item.State}
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, ":")
		field, known := fields[strings.TrimSpace(key)]
		if !ok || !known {
			continue
		}
		key = strings.TrimSpace(key)
		if seen[key] {
			return sandboxIdentity{}, exit(5, "Tensorlake describe returned duplicate identity fields")
		}
		seen[key] = true
		*field = strings.TrimSpace(value)
	}
	if !isLikelySandboxID(item.ID) || !validScopeValue(item.Namespace) || !validScopeValue(item.State) {
		return sandboxIdentity{}, exit(5, "Tensorlake describe did not return an exact sandbox ID, namespace, and state")
	}
	item.State = strings.ToLower(item.State)
	return item, nil
}

func (c *tensorlakeCLI) inspectIdentity(ctx context.Context, id string) (sandboxIdentity, error) {
	if !isLikelySandboxID(id) {
		return sandboxIdentity{}, exit(2, "Tensorlake ownership requires a canonical sandbox ID")
	}
	out, err := c.control(ctx, []string{"sbx", "describe", id})
	if err != nil {
		return sandboxIdentity{}, err
	}
	item, err := parseSandboxIdentity(out)
	if err != nil {
		return sandboxIdentity{}, err
	}
	if item.ID != id {
		return sandboxIdentity{}, exit(2, "Tensorlake describe returned a different sandbox ID")
	}
	return item, nil
}

func bindingForClaim(claim core.LeaseClaim) (shared.ClaimBinding, sandboxBinding, error) {
	binding := sandboxBinding{claim.CloudID, claim.Labels["tensorlake_namespace"], claim.ProviderScope}
	want := shared.ClaimBinding{Provider: providerName, LeaseID: leasePrefix + claim.CloudID, Slug: claim.Slug, CloudID: claim.CloudID, ProviderScope: claim.ProviderScope, ExactProviderScope: true, RequiredLabels: map[string]string{"tensorlake_namespace": binding.Namespace}}
	var scope tensorlakeScope
	if !isLikelySandboxID(binding.ID) || !validScopeValue(binding.Namespace) || claim.Slug == "" || json.Unmarshal([]byte(binding.Scope), &scope) != nil || scope.APIURL == "" || scope.SandboxAPIURL == "" || !validScopeValue(scope.Organization) || !validScopeValue(scope.Project) || !validScopeValue(scope.Namespace) {
		return want, binding, exit(2, "Tensorlake lease %q has no exact resource/account binding; retain the sandbox, inspect it with tensorlake sbx describe, and use manual cleanup or create a new lease; --reclaim cannot adopt legacy ownership", claim.LeaseID)
	}
	return want, binding, shared.ValidateClaimBinding(claim, want)
}

func (c *tensorlakeCLI) verifyBinding(ctx context.Context, binding sandboxBinding) (sandboxIdentity, error) {
	scope, err := c.observeScope(ctx)
	if err != nil {
		return sandboxIdentity{}, err
	}
	if scope != binding.Scope {
		return sandboxIdentity{}, exit(2, "Tensorlake API/account/namespace scope changed; retaining sandbox ownership")
	}
	item, err := c.inspectIdentity(ctx, binding.ID)
	if err != nil {
		return sandboxIdentity{}, err
	}
	if item.Namespace != binding.Namespace {
		return sandboxIdentity{}, exit(2, "Tensorlake sandbox namespace changed; retaining sandbox ownership")
	}
	return item, nil
}

func (c *tensorlakeCLI) terminateBound(ctx context.Context, binding sandboxBinding) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	item, err := c.verifyBinding(ctx, binding)
	if err != nil {
		return err
	}
	if item.State == "terminated" {
		return nil
	}
	if _, err := c.control(ctx, []string{"sbx", "terminate", binding.ID}); err != nil {
		return err
	}
	_, err = shared.Poll(ctx, 0, 500*time.Millisecond, shared.SleepContext,
		func(ctx context.Context) (sandboxIdentity, error) { return c.verifyBinding(ctx, binding) },
		func(_ context.Context, item sandboxIdentity, err error) (bool, error) {
			return item.State == "terminated", err
		}, nil)
	if err != nil {
		return fmt.Errorf("Tensorlake termination remains unconfirmed: %w", err)
	}
	return nil
}

func (c *tensorlakeCLI) removeBoundClaim(ctx context.Context, claim core.LeaseClaim) error {
	want, binding, err := bindingForClaim(claim)
	if err != nil {
		return err
	}
	return shared.RemoveExactClaimAfter(claim, want, func() error { return c.terminateBound(ctx, binding) })
}
