package cli

import (
	"errors"
	"fmt"
	"testing"
)

func TestStreamFailureMessageSurfacesSpoolLimit(t *testing.T) {
	limit := &commandStreamLimitError{limit: 256 << 20}
	if got := streamFailureMessage(fmt.Errorf("wrapped: %w", errors.Join(limit, errors.New("killed"))), "remote command exited 1"); got != limit.Error() {
		t.Fatalf("message=%q, want spool limit text", got)
	}
	for _, err := range []error{nil, errors.New("exit status 1")} {
		if got := streamFailureMessage(err, "remote command exited 1"); got != "remote command exited 1" {
			t.Fatalf("message=%q, want fallback for %v", got, err)
		}
	}
}
