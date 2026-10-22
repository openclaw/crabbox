package cli

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"google.golang.org/api/googleapi"
)

type capacityRefusalMark struct{ error }

func (e capacityRefusalMark) CapacityRefusal() bool { return true }

func TestIsCapacityRefusalMarkerOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "plain", err: errors.New("boom"), want: false},
		{name: "typed CapacityRefusal", err: capacityRefusalMark{errors.New("slots full")}, want: true},
		{name: "MarkCapacityRefusal", err: MarkCapacityRefusal(errors.New("quota")), want: true},
		{name: "wrapped marker", err: fmt.Errorf("create failed: %w", MarkCapacityRefusal(errors.New("full"))), want: true},
		{name: "http 429 text alone is not capacity", err: errors.New("http 429 slow down"), want: false},
		{name: "AWS capacity text alone is not capacity", err: errors.New("InsufficientInstanceCapacity"), want: false},
		{name: "402 billing text alone is not capacity", err: errors.New("HTTP 402 BILLING_NOT_ALLOWED"), want: false},
		{name: "disk full text alone is not capacity", err: errors.New("no space left on device"), want: false},
		{name: "CoordinatorHTTPError 503 alone is not capacity", err: CoordinatorHTTPError{StatusCode: http.StatusServiceUnavailable, Message: "busy"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsCapacityRefusal(tc.err); got != tc.want {
				t.Fatalf("IsCapacityRefusal(%v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestMarkCapacityRefusalIdempotent(t *testing.T) {
	t.Parallel()
	err := errors.New("full")
	marked := MarkCapacityRefusal(err)
	if MarkCapacityRefusal(marked) != marked {
		t.Fatal("MarkCapacityRefusal should return an already-marked error unchanged")
	}
	if MarkCapacityRefusal(nil) != nil {
		t.Fatal("MarkCapacityRefusal(nil) should be nil")
	}
}

func TestAdapterCapacityTagging(t *testing.T) {
	t.Parallel()
	t.Run("aws", func(t *testing.T) {
		t.Parallel()
		err := markCapacityRefusalIf(errors.New("InsufficientInstanceCapacity: AZ full"), isAWSCapacityRefusal)
		if !IsCapacityRefusal(err) {
			t.Fatalf("aws capacity not tagged: %v", err)
		}
		for _, msg := range []string{
			"unauthorized",
			"InvalidParameterValue: invalid subnet",
			"no AWS AMI found in eu-west-1",
			"Unsupported: instance type",
		} {
			err = markCapacityRefusalIf(errors.New(msg), isAWSCapacityRefusal)
			if IsCapacityRefusal(err) {
				t.Fatalf("aws non-capacity tagged for %q: %v", msg, err)
			}
			if !isRetryableAWSProvisioningError(errors.New(msg)) && msg != "unauthorized" {
				t.Fatalf("aws fallback classifier should still retry %q", msg)
			}
		}
	})
	t.Run("hetzner", func(t *testing.T) {
		t.Parallel()
		err := markCapacityRefusalIf(errors.New("resource_limit_exceeded"), isRetryableProvisioningError)
		if !IsCapacityRefusal(err) {
			t.Fatalf("hetzner capacity not tagged: %v", err)
		}
		err = markCapacityRefusalIf(errors.New("unauthorized"), isRetryableProvisioningError)
		if IsCapacityRefusal(err) {
			t.Fatalf("hetzner non-capacity tagged: %v", err)
		}
	})
	t.Run("gcp", func(t *testing.T) {
		t.Parallel()
		apiErr := &googleapi.Error{Code: http.StatusForbidden, Message: "Quota exceeded for CPUs"}
		err := markCapacityRefusalIf(apiErr, isGCPCapacityRefusal)
		if !IsCapacityRefusal(err) {
			t.Fatalf("gcp capacity not tagged: %v", err)
		}
		err = markCapacityRefusalIf(errors.New("Invalid value for field machineType"), isGCPCapacityRefusal)
		if IsCapacityRefusal(err) {
			t.Fatalf("gcp config error tagged: %v", err)
		}
	})
	t.Run("azure", func(t *testing.T) {
		t.Parallel()
		err := markCapacityRefusalIf(errors.New("AllocationFailed: out of capacity"), isAzureCapacityRefusal)
		if !IsCapacityRefusal(err) {
			t.Fatalf("azure capacity not tagged: %v", err)
		}
		for _, msg := range []string{"OperationNotAllowed: policy", "NotAvailableForSubscription"} {
			err = markCapacityRefusalIf(errors.New(msg), isAzureCapacityRefusal)
			if IsCapacityRefusal(err) {
				t.Fatalf("azure non-capacity tagged for %q: %v", msg, err)
			}
		}
	})
	t.Run("coordinator capacity_unavailable", func(t *testing.T) {
		t.Parallel()
		err := markCapacityRefusalIf(CoordinatorHTTPError{
			StatusCode: http.StatusServiceUnavailable,
			Message:    `{"error":"capacity_unavailable","retryable":true}`,
		}, isCoordinatorCapacityRefusal)
		if !IsCapacityRefusal(err) {
			t.Fatalf("coordinator capacity_unavailable not tagged: %v", err)
		}
	})
	t.Run("coordinator cost_limit_exceeded not tagged", func(t *testing.T) {
		t.Parallel()
		err := markCapacityRefusalIf(CoordinatorHTTPError{
			StatusCode: http.StatusTooManyRequests,
			Message:    `{"error":"cost_limit_exceeded","message":"owner limit"}`,
		}, isCoordinatorCapacityRefusal)
		if IsCapacityRefusal(err) {
			t.Fatalf("cost_limit_exceeded must not be capacity: %v", err)
		}
	})
	t.Run("coordinator workspace_limit_exceeded not tagged", func(t *testing.T) {
		t.Parallel()
		err := markCapacityRefusalIf(CoordinatorHTTPError{
			StatusCode: http.StatusTooManyRequests,
			Message:    `{"error":"workspace_limit_exceeded"}`,
		}, isCoordinatorCapacityRefusal)
		if IsCapacityRefusal(err) {
			t.Fatalf("workspace_limit_exceeded must not be capacity: %v", err)
		}
	})
}
