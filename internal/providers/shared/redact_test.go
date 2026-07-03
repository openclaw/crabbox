package shared

import (
	"strings"
	"testing"
)

func TestRedactErrorSecrets(t *testing.T) {
	secret := "provider-secret-token"
	value := `request failed: Bearer ` + secret + ` X-API-Key: ` + secret + ` {"accessToken":"derived-token","message":"quota exceeded"}`
	got := RedactErrorSecrets(value, secret, " ")
	for _, leaked := range []string{secret, "derived-token"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("redacted error leaked %q: %s", leaked, got)
		}
	}
	if !strings.Contains(got, "quota exceeded") || strings.Count(got, redactedProviderSecret) < 3 {
		t.Fatalf("redacted error lost useful detail: %s", got)
	}
	if plain := RedactErrorSecrets("ordinary provider failure", ""); plain != "ordinary provider failure" {
		t.Fatalf("plain error changed: %q", plain)
	}
	truncated := RedactErrorSecrets(`{"token":"` + strings.Repeat("secret-prefix", 50))
	if strings.Contains(truncated, "secret-prefix") || !strings.Contains(truncated, `"token":"[redacted]"`) {
		t.Fatalf("truncated JSON credential was not redacted: %s", truncated)
	}
}
