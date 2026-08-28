package cli

import (
	"net/url"
	"regexp"
	"strings"
)

var routingURLSchemePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)

// ClaimScopeURL preserves historical URL normalization used by stored claim keys.
func ClaimScopeURL(value string) string { return routingSafeURL(value) }

// RoutingSafeURL excludes credentials while retaining non-secret endpoint routing.
func RoutingSafeURL(value string) string { return publicRoutingURL(value) }

// providerRouting returns the canonical adapter's route, without common flags.
func providerRouting(cfg Config, request CommandRoutingRequest) CommandRouting {
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return CommandRouting{}
	}
	router, ok := provider.(ProviderCommandRouter)
	if !ok {
		return CommandRouting{}
	}
	cfg.Provider = provider.Name()
	return router.CommandRouting(cfg, request)
}

// CommandRoutingFor combines common selectors with the canonical adapter route.
func CommandRoutingFor(cfg Config, leaseID string, purpose CommandRoutingPurpose, resolvedTarget ...SSHTarget) CommandRouting {
	request := CommandRoutingRequest{LeaseID: leaseID, Purpose: purpose}
	if len(resolvedTarget) > 0 {
		request.Target = resolvedTarget[0]
		if request.Target.AuthSecret {
			request.Target.User = ""
		}
	}
	routing := providerRouting(cfg, request)
	var args []string
	if provider := strings.TrimSpace(cfg.Provider); provider != "" {
		if resolved, err := ProviderFor(provider); err == nil {
			provider = resolved.Name()
		}
		args = append(args, "--provider", provider)
	}
	if strings.TrimSpace(cfg.TargetOS) != "" {
		args = append(args, "--target", cfg.TargetOS)
	}
	if cfg.TargetOS == targetWindows && strings.TrimSpace(cfg.WindowsMode) != "" {
		args = append(args, "--windows-mode", cfg.WindowsMode)
	}
	routing.Args = append(args, routing.Args...)
	return routing
}

func (routing CommandRouting) ShellCommand(args []string) string {
	return readableShellCommand(append(append([]string(nil), routing.Env...), args...))
}

func publicRoutingURL(value string) string {
	value = strings.TrimSpace(value)
	value, _, _ = strings.Cut(value, "#")
	base, rawQuery, hasQuery := strings.Cut(value, "?")
	// A scheme delimiter in a path or query does not identify the authority.
	base = routingURLWithoutUserinfo(base, !routingURLSchemePattern.MatchString(base) && !strings.HasPrefix(base, "//"))
	// Legacy claim scopes can distinguish scheme spelling and a bare '?'.
	// Remove credential fields without reserializing the non-secret endpoint.
	if !hasQuery || rawQuery == "" {
		if hasQuery {
			return base + "?"
		}
		return base
	}
	query := strings.Split(rawQuery, "&")
	safeQuery := query[:0]
	for _, field := range query {
		key, _, _ := strings.Cut(field, "=")
		key, err := url.QueryUnescape(key)
		if err != nil || diagnosticSecretField(key) || diagnosticQueryPattern.MatchString("?"+key+"=value") || strings.EqualFold(key, "credential") || strings.EqualFold(key, "credentials") || strings.EqualFold(key, "auth") || strings.EqualFold(key, "key") || strings.EqualFold(key, "pwd") || strings.EqualFold(key, "secret") {
			continue
		}
		safeQuery = append(safeQuery, field)
	}
	if query := strings.Join(safeQuery, "&"); query != "" {
		return base + "?" + query
	}
	return base
}

func routingSafeURL(value string) string {
	// Keep this historical scheme test for stored claim identity compatibility.
	return routingURLWithoutUserinfo(value, !strings.Contains(value, "://"))
}

func routingURLWithoutUserinfo(value string, addedScheme bool) string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return value
	}
	parseValue := raw
	if addedScheme {
		parseValue = "https://" + parseValue
	}
	u, err := url.Parse(parseValue)
	if err != nil {
		return sanitizedMalformedConfigURL(parseValue, addedScheme)
	}
	if u.User == nil {
		return value
	}
	safe := *u
	safe.User = nil
	out := safe.String()
	if addedScheme {
		out = strings.TrimPrefix(out, "https://")
	}
	return out
}
