package cloudflaresandbox

import (
	"slices"
	"strings"
)

func cloudflareSandboxCommandEnv(input map[string]string) (map[string]string, []string) {
	out := make(map[string]string, len(input))
	stripped := make([]string, 0)
	for name, value := range input {
		switch normalizedCloudflareSandboxEnvName(name) {
		case "CRABBOX_CLOUDFLARE_SANDBOX_TOKEN", "CLOUDFLARE_API_TOKEN", "CLOUDFLARE_ACCOUNT_ID", "CF_API_TOKEN", "CF_ACCOUNT_ID":
			stripped = append(stripped, name)
		default:
			out[name] = value
		}
	}
	slices.Sort(stripped)
	return out, stripped
}

func normalizedCloudflareSandboxEnvName(name string) string {
	return strings.ToUpper(strings.TrimSpace(name))
}
