package blacksmith

import (
	"testing"
	"time"
)

func TestClaimExpiryBoundCoversGitHubRunCap(t *testing.T) {
	// Pruning a claim before its GitHub run must have settled would strand a
	// retryable stop; GitHub's hard cap on a workflow run is 35 days.
	if bound := (Provider{}).Spec().ClaimExpiryBound; bound <= 35*24*time.Hour {
		t.Fatalf("ClaimExpiryBound = %s, want longer than the 35-day GitHub workflow run cap", bound)
	}
}
