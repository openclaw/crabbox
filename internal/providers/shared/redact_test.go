package shared

import (
	"errors"
	"strings"
	"testing"
)

func TestRedactedResponseBodyPreservesFormattingAndAdapterPolicy(t *testing.T) {
	redact := func(value string) string { return strings.ReplaceAll(value, "synthetic-secret", "<hidden>") }
	for _, tc := range []struct {
		name, body string
		readErr    error
		limit      int
		want       string
	}{
		{name: "trimmed body", body: "  quota exceeded \n", limit: 400, want: "quota exceeded"},
		{name: "redact before cutoff", body: "prefix synthetic-secret", limit: 15, want: "prefix <hidden>"},
		{name: "no cutoff", body: "synthetic-secret", want: "<hidden>"},
		{name: "read error only", readErr: errors.New("synthetic-secret interrupted"), limit: 400, want: "response body read failed: <hidden> interrupted"},
		{name: "body and read error", body: "partial", readErr: errors.New("synthetic-secret interrupted"), limit: 400, want: "partial; response body read failed: <hidden> interrupted"},
		{name: "read diagnostic outside cutoff", body: "long body", readErr: errors.New("unexpected EOF"), limit: 4, want: "long; response body read failed: unexpected EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactedResponseBody([]byte(tc.body), tc.readErr, tc.limit, redact); got != tc.want {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestRedactErrorSecrets(t *testing.T) {
	secret := "provider-secret-token"
	value := `request failed: Bearer ` + secret + ` X-API-Key: ` + secret + ` {"accessToken":"derived-token","clientSecret":"client-secret","message":"quota exceeded"} https://user:pass@example.test/path?token=query-secret -----BEGIN PRIVATE KEY-----
private-material
-----END PRIVATE KEY-----`
	got := RedactErrorSecrets(value, secret, " ")
	for _, leaked := range []string{secret, "derived-token", "client-secret", "user", "pass", "query-secret", "private-material"} {
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
