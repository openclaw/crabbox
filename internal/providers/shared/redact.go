package shared

import (
	"regexp"
	"strings"
)

const redactedProviderSecret = "[redacted]"

var (
	providerBearerPattern = regexp.MustCompile(`(?i)\bbearer[ \t]+[^\s"',;\\}]+`)
	providerHeaderPattern = regexp.MustCompile(`(?i)\b(authorization|x-api-key|api-key|api_key)[ \t]*[:=][ \t]*[^\s"',;\\}]+`)
)

// RedactErrorSecrets removes credentials from untrusted provider response text
// while preserving the surrounding status and diagnostic detail.
func RedactErrorSecrets(value string, secrets ...string) string {
	redacted := value
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			redacted = strings.ReplaceAll(redacted, secret, redactedProviderSecret)
		}
	}
	redacted = providerBearerPattern.ReplaceAllString(redacted, "Bearer "+redactedProviderSecret)
	redacted = providerHeaderPattern.ReplaceAllStringFunc(redacted, func(match string) string {
		separator := strings.IndexAny(match, ":=")
		if separator < 0 {
			return match
		}
		return match[:separator+1] + " " + redactedProviderSecret
	})
	for _, key := range []string{"authorization", "apiKey", "api_key", "accessToken", "access_token", "token"} {
		redacted = redactProviderJSONField(redacted, key)
	}
	return redacted
}

func redactProviderJSONField(value, key string) string {
	pattern := regexp.MustCompile(`(?i)"` + regexp.QuoteMeta(key) + `"\s*:\s*"[^"]*(?:"|$)`)
	return pattern.ReplaceAllStringFunc(value, func(match string) string {
		separator := strings.Index(match, ":")
		if separator < 0 {
			return match
		}
		return match[:separator+1] + `"` + redactedProviderSecret + `"`
	})
}
