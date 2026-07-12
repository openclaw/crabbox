package cli

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

const (
	diagnosticRedaction          = "[redacted]"
	diagnosticAngleRedaction     = "<redacted>"
	maxDiagnosticRedactionPasses = 64
)

var (
	diagnosticAuthorizationHeaderPattern = regexp.MustCompile("(?i)(^|[^?&[:alnum:]_-])(authorization|proxy-authorization)[ \\t]*[:=][ \\t]*[!#$%&'*+.^_`|~[:alnum:]-]+(?:(?:\\r?\\n[ \\t]+)|[^\\r\\n])*")
	diagnosticHeaderPattern              = regexp.MustCompile(`(?i)(^|[^?&[:alnum:]_-])(authorization|proxy-authorization|x-api-key|api[-_]?key|access[-_]?token|refresh[-_]?token|id[-_]?token|client[-_]?secret|secret[-_]?access[-_]?key|api[-_]?secret|session[-_]?token|token|password)[ \t]*[:=][ \t]*(?:(?:bearer|basic)(?:[ \t]*:[ \t]*\r?\n[ \t]+|[ \t]*:[ \t]*|[ \t]*\r?\n[ \t]+|[ \t]+))?(?:\\.|[^\s"])+`)
	diagnosticJSONPattern                = regexp.MustCompile(`(?i)"(authorization|proxy-authorization|x-api-key|apiKey|api-key|api_key|accessToken|access-token|access_token|refreshToken|refresh-token|refresh_token|idToken|id-token|id_token|clientSecret|client-secret|client_secret|secretAccessKey|secret-access-key|secret_access_key|apiSecret|api-secret|api_secret|credential|credentials|privateKey|private-key|private_key|secret|sessionToken|session-token|session_token|token|password)"\s*:\s*"(?:\\(?:[\s\S]|$)|[^"\\])*(?:"|$)`)
	diagnosticQueryPattern               = regexp.MustCompile(`(?i)([?&](?:authorization|proxy-authorization|x-api-key|api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|client[_-]?secret|secret[_-]?access[_-]?key|api[_-]?secret|session[_-]?token|password|token|signature|sig|x-amz-credential|x-amz-signature|x-amz-security-token|x-goog-credential|x-goog-signature|x-goog-security-token)=)[^&#\s]+`)
	diagnosticURLPattern                 = regexp.MustCompile(`(?i)\b(https?://)([^/?#\s]+)@`)
	diagnosticBearerPattern              = regexp.MustCompile(`(?i)\bbearer(?:[ \t]*:[ \t]*\r?\n[ \t]+|[ \t]*:[ \t]*|[ \t]*\r?\n[ \t]+|[ \t]+)((?:\\.|[^\s"])+)`)
	diagnosticPEMPattern                 = regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)
)

// RedactDiagnosticSecrets removes credentials from untrusted diagnostic text
// while preserving non-secret routing and failure context.
func RedactDiagnosticSecrets(value string, secrets ...string) string {
	exactSecrets := normalizedDiagnosticSecrets(secrets)
	redacted := value
	for pass := 0; pass < maxDiagnosticRedactionPasses; pass++ {
		next := redactDiagnosticPass(redacted, exactSecrets)
		if next == redacted {
			return next
		}
		redacted = next
	}
	return diagnosticRedaction
}

type diagnosticRedactionRange struct {
	start, end int
	preserve   bool
}

func redactDiagnosticPass(value string, secrets []string) string {
	// Collect structural and exact spans from the same source text so one
	// rewrite cannot hide a still-unredacted part of another credential.
	markerRanges := diagnosticMarkerRanges(value)
	ranges := append([]diagnosticRedactionRange(nil), markerRanges...)
	added := false
	closeUnterminatedJSON := false
	addRedaction := func(start, end int) {
		if start >= end || diagnosticRangeInsideMarker(start, end, markerRanges) {
			return
		}
		ranges = append(ranges, diagnosticRedactionRange{start: start, end: end})
		added = true
	}

	for _, indexes := range diagnosticAuthorizationHeaderPattern.FindAllStringSubmatchIndex(value, -1) {
		end, keyEnd := indexes[1], indexes[5]
		separator := strings.IndexAny(value[keyEnd:end], ":=")
		if separator < 0 {
			continue
		}
		separator += keyEnd
		fields := strings.Fields(value[separator+1 : end])
		if len(fields) > 0 {
			scheme := strings.ToLower(strings.TrimSuffix(fields[0], ":"))
			if scheme == "bearer" || scheme == "basic" {
				continue
			}
		}
		addRedaction(diagnosticSkipHorizontalSpace(value, separator+1, end), end)
	}
	for _, indexes := range diagnosticHeaderPattern.FindAllStringSubmatchIndex(value, -1) {
		end, keyEnd := indexes[1], indexes[5]
		separator := strings.IndexAny(value[keyEnd:end], ":=")
		if separator < 0 {
			continue
		}
		separator += keyEnd
		addRedaction(diagnosticSkipHorizontalSpace(value, separator+1, end), end)
	}
	for _, indexes := range diagnosticJSONPattern.FindAllStringIndex(value, -1) {
		start, end := indexes[0], indexes[1]
		separator := strings.Index(value[start:end], ":")
		if separator < 0 {
			continue
		}
		separator += start
		quote := strings.Index(value[separator+1:end], `"`)
		if quote < 0 {
			continue
		}
		secretStart := separator + 1 + quote + 1
		secretEnd := end
		if secretEnd > secretStart && value[secretEnd-1] == '"' {
			secretEnd--
		} else if end == len(value) {
			closeUnterminatedJSON = true
		}
		addRedaction(secretStart, secretEnd)
	}
	for _, indexes := range diagnosticQueryPattern.FindAllStringSubmatchIndex(value, -1) {
		addRedaction(indexes[3], indexes[1])
	}
	for _, indexes := range diagnosticURLPattern.FindAllStringSubmatchIndex(value, -1) {
		userinfoStart, userinfoEnd := indexes[4], indexes[5]
		userinfo := value[userinfoStart:userinfoEnd]
		if userinfo != diagnosticRedaction && userinfo != diagnosticAngleRedaction {
			addRedaction(userinfoStart, userinfoEnd)
		}
	}
	for _, indexes := range diagnosticBearerPattern.FindAllStringSubmatchIndex(value, -1) {
		addRedaction(indexes[2], indexes[3])
	}
	for _, indexes := range diagnosticPEMPattern.FindAllStringIndex(value, -1) {
		addRedaction(indexes[0], indexes[1])
	}
	for _, secret := range secrets {
		for offset := 0; offset < len(value); {
			index := strings.Index(value[offset:], secret)
			if index < 0 {
				break
			}
			start := offset + index
			addRedaction(start, start+len(secret))
			offset = start + 1
		}
	}
	if !added && !closeUnterminatedJSON {
		return value
	}
	redacted := applyDiagnosticRedactionRanges(value, ranges)
	if closeUnterminatedJSON && diagnosticUnterminatedJSONAtEnd(redacted) {
		redacted += `"`
	}
	return redacted
}

func diagnosticUnterminatedJSONAtEnd(value string) bool {
	matches := diagnosticJSONPattern.FindAllStringIndex(value, -1)
	if len(matches) == 0 {
		return false
	}
	last := matches[len(matches)-1]
	return last[1] == len(value) && value[last[1]-1] != '"'
}

func diagnosticMarkerRanges(value string) []diagnosticRedactionRange {
	ranges := make([]diagnosticRedactionRange, 0)
	for offset := 0; offset < len(value); {
		squareIndex := strings.Index(value[offset:], diagnosticRedaction)
		angleIndex := strings.Index(value[offset:], diagnosticAngleRedaction)
		if squareIndex < 0 && angleIndex < 0 {
			break
		}
		index, markerLength := squareIndex, len(diagnosticRedaction)
		if index < 0 || (angleIndex >= 0 && angleIndex < index) {
			index, markerLength = angleIndex, len(diagnosticAngleRedaction)
		}
		start := offset + index
		ranges = append(ranges, diagnosticRedactionRange{start: start, end: start + markerLength, preserve: true})
		offset = start + markerLength
	}
	return ranges
}

func diagnosticRangeInsideMarker(start, end int, markers []diagnosticRedactionRange) bool {
	index := sort.Search(len(markers), func(i int) bool { return markers[i].start > start }) - 1
	return index >= 0 && end <= markers[index].end
}

func diagnosticSkipHorizontalSpace(value string, start, end int) int {
	for start < end && (value[start] == ' ' || value[start] == '\t') {
		start++
	}
	return start
}

func applyDiagnosticRedactionRanges(value string, ranges []diagnosticRedactionRange) string {
	sort.Slice(ranges, func(i, j int) bool {
		return ranges[i].start < ranges[j].start || (ranges[i].start == ranges[j].start && ranges[i].end < ranges[j].end)
	})
	merged := ranges[:0]
	for _, candidate := range ranges {
		if len(merged) == 0 || candidate.start >= merged[len(merged)-1].end {
			merged = append(merged, candidate)
			continue
		}
		merged[len(merged)-1].preserve = merged[len(merged)-1].preserve && candidate.preserve
		if candidate.end > merged[len(merged)-1].end {
			merged[len(merged)-1].end = candidate.end
		}
	}
	var redacted strings.Builder
	redacted.Grow(len(value))
	offset := 0
	for _, redaction := range merged {
		redacted.WriteString(value[offset:redaction.start])
		if redaction.preserve {
			redacted.WriteString(value[redaction.start:redaction.end])
		} else {
			redacted.WriteString(diagnosticRedaction)
		}
		offset = redaction.end
	}
	redacted.WriteString(value[offset:])
	return redacted.String()
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
