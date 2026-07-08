package cli

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

const diagnosticRedaction = "[redacted]"

var (
	diagnosticAuthorizationHeaderPattern = regexp.MustCompile("(?i)(^|[^?&[:alnum:]_-])(authorization|proxy-authorization)[ \\t]*[:=][ \\t]*[!#$%&'*+.^_`|~[:alnum:]-]+(?:(?:\\r?\\n[ \\t]+)|[^\\r\\n])*")
	diagnosticHeaderPattern              = regexp.MustCompile(`(?i)(^|[^?&[:alnum:]_-])(authorization|proxy-authorization|x-api-key|api-key|api_key|access-token|access_token|client-secret|client_secret|session-token|session_token|token|password)[ \t]*[:=][ \t]*(?:(?:bearer|basic)(?:[ \t]*:[ \t]*\r?\n[ \t]+|[ \t]*:[ \t]*|[ \t]*\r?\n[ \t]+|[ \t]+))?(?:\\.|[^\s"])+`)
	diagnosticJSONPattern                = regexp.MustCompile(`(?i)"(authorization|proxy-authorization|x-api-key|apiKey|api-key|api_key|accessToken|access-token|access_token|clientSecret|client-secret|client_secret|credential|credentials|privateKey|private-key|private_key|secret|sessionToken|session-token|session_token|token|password)"\s*:\s*"(?:\\[\s\S]|[^"\\])*(?:"|$)`)
	diagnosticQueryPattern               = regexp.MustCompile(`(?i)([?&](?:authorization|proxy-authorization|x-api-key|api[_-]?key|access[_-]?token|client[_-]?secret|session[_-]?token|password|token|signature|sig|x-amz-credential|x-amz-signature|x-amz-security-token|x-goog-credential|x-goog-signature|x-goog-security-token)=)[^&#\s]+`)
	diagnosticURLPattern                 = regexp.MustCompile(`(?i)\b(https?://)[^/@\s]+@`)
	diagnosticBearerPattern              = regexp.MustCompile(`(?i)\bbearer(?:[ \t]*:[ \t]*\r?\n[ \t]+|[ \t]*:[ \t]*|[ \t]*\r?\n[ \t]+|[ \t]+)(?:\\.|[^\s"])+`)
	diagnosticPEMPattern                 = regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)
)

// RedactDiagnosticSecrets removes credentials from untrusted diagnostic text
// while preserving non-secret routing and failure context.
func RedactDiagnosticSecrets(value string, secrets ...string) string {
	redacted := value
	for _, secret := range normalizedDiagnosticSecrets(secrets) {
		redacted = strings.ReplaceAll(redacted, secret, diagnosticRedaction)
	}
	redacted = diagnosticAuthorizationHeaderPattern.ReplaceAllStringFunc(redacted, redactDiagnosticAuthorization)
	redacted = diagnosticHeaderPattern.ReplaceAllStringFunc(redacted, redactDiagnosticAssignment)
	redacted = diagnosticJSONPattern.ReplaceAllStringFunc(redacted, redactDiagnosticJSONField)
	redacted = diagnosticQueryPattern.ReplaceAllString(redacted, `$1`+diagnosticRedaction)
	redacted = diagnosticURLPattern.ReplaceAllStringFunc(redacted, redactDiagnosticURLUserinfo)
	redacted = diagnosticBearerPattern.ReplaceAllString(redacted, "Bearer "+diagnosticRedaction)
	return diagnosticPEMPattern.ReplaceAllString(redacted, diagnosticRedaction)
}

func redactDiagnosticAuthorization(match string) string {
	indexes := diagnosticAuthorizationHeaderPattern.FindStringSubmatchIndex(match)
	if len(indexes) < 6 || indexes[5] < 0 {
		return match
	}
	separator := strings.IndexAny(match[indexes[5]:], ":=")
	if separator < 0 {
		return match
	}
	separator += indexes[5]
	fields := strings.Fields(match[separator+1:])
	if len(fields) > 0 {
		scheme := strings.ToLower(strings.TrimSuffix(fields[0], ":"))
		if scheme == "bearer" || scheme == "basic" {
			return diagnosticHeaderPattern.ReplaceAllStringFunc(match, redactDiagnosticAssignment)
		}
	}
	return match[:separator+1] + " " + diagnosticRedaction
}

func redactDiagnosticURLUserinfo(match string) string {
	separator := strings.Index(match, "://")
	if separator < 0 {
		return match
	}
	prefixEnd := separator + 3
	userinfo := strings.TrimSuffix(match[prefixEnd:], "@")
	if userinfo == "<redacted>" || userinfo == diagnosticRedaction {
		return match
	}
	return match[:prefixEnd] + diagnosticRedaction + "@"
}

func normalizedDiagnosticSecrets(values []string) []string {
	seen := make(map[string]bool, len(values))
	secrets := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			secrets = append(secrets, value)
		}
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return secrets
}

func redactDiagnosticAssignment(match string) string {
	indexes := diagnosticHeaderPattern.FindStringSubmatchIndex(match)
	if len(indexes) < 6 || indexes[5] < 0 {
		return match
	}
	separator := strings.IndexAny(match[indexes[5]:], ":=")
	if separator < 0 {
		return match
	}
	separator += indexes[5]
	return match[:separator+1] + " " + diagnosticRedaction
}

func redactDiagnosticJSONField(match string) string {
	separator := strings.Index(match, ":")
	if separator < 0 {
		return match
	}
	return match[:separator+1] + `"` + diagnosticRedaction + `"`
}

func redactDiagnosticDetails(details map[string]string, secrets []string) map[string]string {
	if details == nil {
		return nil
	}
	redacted := make(map[string]string, len(details))
	for key, value := range details {
		redactedKey := RedactDiagnosticSecrets(key, secrets...)
		redacted[redactedKey] = RedactDiagnosticSecrets(value, secrets...)
	}
	return redacted
}

func configuredDiagnosticSecrets(cfg Config) []string {
	seen := map[string]bool{}
	collectConfiguredDiagnosticSecrets(reflect.ValueOf(cfg), "", seen)
	for _, name := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AZURE_CLIENT_SECRET", "CLOUDFLARE_API_TOKEN", "DIGITALOCEAN_TOKEN",
		"HCLOUD_TOKEN", "HETZNER_TOKEN", "LAMBDA_API_KEY", "OVH_APPLICATION_KEY",
		"OVH_APPLICATION_SECRET", "OVH_CONSUMER_KEY", "SCW_ACCESS_KEY", "SCW_SECRET_KEY",
		"TENCENTCLOUD_SECRET_ID", "TENCENTCLOUD_SECRET_KEY", "TENCENTCLOUD_TOKEN",
		"TS_API_KEY", "VULTR_API_KEY",
	} {
		if value := strings.TrimSpace(os.Getenv(name)); len(value) >= 4 {
			seen[value] = true
		}
	}
	if name := strings.TrimSpace(cfg.Nomad.TokenEnv); name != "" {
		if value := strings.TrimSpace(os.Getenv(name)); len(value) >= 4 {
			seen[value] = true
		}
	}
	sources := make([]ProviderDiagnosticSecretSource, 0)
	for _, provider := range registeredProviders() {
		if source, ok := provider.(ProviderDiagnosticSecretSource); ok {
			sources = append(sources, source)
		}
	}
	collectProviderDiagnosticSecrets(cfg, sources, seen)
	secrets := make([]string, 0, len(seen))
	for secret := range seen {
		secrets = append(secrets, secret)
	}
	return secrets
}

func collectProviderDiagnosticSecrets(cfg Config, sources []ProviderDiagnosticSecretSource, seen map[string]bool) {
	for _, source := range sources {
		for _, value := range source.DiagnosticSecrets(cfg) {
			if value = strings.TrimSpace(value); len(value) >= 4 {
				seen[value] = true
			}
		}
	}
}

func collectConfiguredDiagnosticSecrets(value reflect.Value, name string, seen map[string]bool) {
	if !value.IsValid() {
		return
	}
	for value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface {
		if value.IsNil() {
			return
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.String:
		if diagnosticSecretField(name) {
			if secret := strings.TrimSpace(value.String()); len(secret) >= 4 {
				seen[secret] = true
			}
		}
	case reflect.Struct:
		typeOfValue := value.Type()
		for i := 0; i < value.NumField(); i++ {
			field := typeOfValue.Field(i)
			if field.PkgPath == "" {
				collectConfiguredDiagnosticSecrets(value.Field(i), field.Name, seen)
			}
		}
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			key := iterator.Key()
			fieldName := name
			if key.Kind() == reflect.String {
				fieldName = key.String()
			}
			collectConfiguredDiagnosticSecrets(iterator.Value(), fieldName, seen)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			collectConfiguredDiagnosticSecrets(value.Index(i), name, seen)
		}
	}
}

func diagnosticSecretField(name string) bool {
	name = strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(name))
	for _, suffix := range []string{"token", "secret", "password", "apikey", "authkey", "privatekey", "accesskey", "consumerkey", "applicationkey"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}
