package shared

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
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

func TestErrorWithMessagePreservesPresentationAndTypedCause(t *testing.T) {
	if got := ErrorWithMessage("unused", nil); got != nil {
		t.Fatalf("nil cause=%v", got)
	}
	original := &url.Error{Op: "Get", URL: "https://example.invalid/private", Err: context.Canceled}
	wrapped := ErrorWithMessage("prepared message", original)
	for _, got := range []string{wrapped.Error(), fmt.Sprint(wrapped), fmt.Sprintf("%+v", wrapped), fmt.Sprintf("%s", wrapped)} {
		if got != "prepared message" {
			t.Fatalf("unexpected display=%q", got)
		}
	}
	if got := fmt.Errorf("outer: %w", wrapped).Error(); got != "outer: prepared message" {
		t.Fatalf("wrapped display=%q", got)
	}
	if empty := ErrorWithMessage("", original); empty.Error() != "" || !errors.Is(empty, original) {
		t.Fatal("empty message fell back to raw cause")
	}
	var transport *url.Error
	if !errors.Is(wrapped, original) || !errors.Is(wrapped, context.Canceled) || !errors.As(wrapped, &transport) || transport != original {
		t.Fatal("cause identity lost")
	}
	var exit core.ExitError
	if errors.As(wrapped, &exit) {
		t.Fatal("wrapper invented public exit ownership")
	}
}

func TestErrorWithMessageDoesNotOwnInnerExitOrClassification(t *testing.T) {
	for _, code := range []int{0, -1, 7} {
		inner := core.ExitError{Code: code, Message: "inner diagnostic"}
		cause := errors.Join(inner, context.DeadlineExceeded)
		wrapped := ErrorWithMessage("outer presentation", cause)
		var selected core.ExitError
		if !core.AsExitError(wrapped, &selected) || selected != inner {
			t.Fatalf("inner selection changed: %+v", selected)
		}
		// This is the selection used by main; a presentation wrapper is not a safe-display boundary.
		if selected.Message != "inner diagnostic" || wrapped.Error() != "outer presentation" {
			t.Fatal("display boundaries conflated")
		}
		if !errors.Is(wrapped, cause) || !errors.Is(wrapped, context.DeadlineExceeded) {
			t.Fatal("joined causes lost")
		}
		if got, want := core.FinalizeRunResult(core.RunResult{}, wrapped), core.FinalizeRunResult(core.RunResult{}, cause); got.Status != want.Status || got.ErrorKind != want.ErrorKind {
			t.Fatalf("classification changed: %+v %+v", got, want)
		}
		if core.ExitCodeForError(wrapped, 19) != core.ExitCodeForError(cause, 19) {
			t.Fatal("selected/fallback code changed")
		}
	}
}

func TestErrorWithMessagePreservesPrimaryClassificationOverride(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	primary := PollTerminationError(ctx, errors.New("poll ended"), core.ExitError{Code: -1, Message: "public poll diagnostic"})
	cause := errors.Join(primary, context.DeadlineExceeded)
	wrapped := ErrorWithMessage("outer presentation", cause)
	if core.PrimaryRunClassificationCause(wrapped) != context.Canceled {
		t.Fatal("secondary failure replaced primary classification")
	}
	got := core.FinalizeRunResult(core.RunResult{}, wrapped)
	if got.Status != core.RunStatusCanceled || got.ErrorKind != core.RunErrorCanceled {
		t.Fatalf("classification=%+v", got)
	}
	var selected core.ExitError
	if !errors.As(wrapped, &selected) || selected.Code != -1 || selected.Message != "public poll diagnostic" || !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Fatal("public owner or secondary error identity changed")
	}
}

type sliceMessageCause []string

func (e sliceMessageCause) Error() string { return strings.Join(e, ",") }

func TestErrorWithMessageSupportsNonComparableCause(t *testing.T) {
	cause := sliceMessageCause{"original", "cause"}
	wrapped := ErrorWithMessage("prepared message", cause)
	if !errors.Is(wrapped, wrapped) {
		t.Fatal("wrapper did not match itself")
	}
	var typed sliceMessageCause
	if !errors.As(wrapped, &typed) || len(typed) != 2 || typed[0] != "original" || typed[1] != "cause" {
		t.Fatalf("non-comparable typed cause lost: %v", typed)
	}
	if wrapped.Error() != "prepared message" {
		t.Fatal("presentation changed")
	}
}
