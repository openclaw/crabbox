package cli

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"google.golang.org/api/googleapi"
)

type capacityRefusalMark struct{ error }

func (e capacityRefusalMark) CapacityRefusal() bool { return true }

type statusCodeErr struct {
	code int
	msg  string
}

func (e statusCodeErr) Error() string       { return e.msg }
func (e statusCodeErr) HTTPStatusCode() int { return e.code }

func TestIsCapacityRefusal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "plain", err: errors.New("boom"), want: false},
		{name: "typed CapacityRefusal", err: capacityRefusalMark{errors.New("slots full")}, want: true},
		{name: "httpStatusCoder 429", err: statusCodeErr{code: http.StatusTooManyRequests, msg: "busy"}, want: true},
		{name: "httpStatusCoder 503", err: statusCodeErr{code: http.StatusServiceUnavailable, msg: "unavailable"}, want: true},
		{name: "httpStatusCoder 402", err: statusCodeErr{code: http.StatusPaymentRequired, msg: "billing"}, want: true},
		{name: "httpStatusCoder 400", err: statusCodeErr{code: http.StatusBadRequest, msg: "bad"}, want: false},
		{name: "CoordinatorHTTPError 429", err: CoordinatorHTTPError{StatusCode: http.StatusTooManyRequests, Message: "slow down"}, want: true},
		{name: "CoordinatorHTTPError 500", err: CoordinatorHTTPError{StatusCode: http.StatusInternalServerError, Message: "oops"}, want: false},
		{name: "HetznerHTTPError 503", err: HetznerHTTPError{StatusCode: http.StatusServiceUnavailable, Detail: "busy"}, want: true},
		{name: "googleapi 429", err: &googleapi.Error{Code: http.StatusTooManyRequests, Message: "rate"}, want: true},
		{name: "azcore 429", err: &azcore.ResponseError{StatusCode: http.StatusTooManyRequests, ErrorCode: "TooManyRequests"}, want: true},
		{name: "AWS InsufficientInstanceCapacity", err: errors.New("InsufficientInstanceCapacity: AZ full"), want: true},
		{name: "AWS UnfulfillableCapacity", err: errors.New("UnfulfillableCapacity"), want: true},
		{name: "AWS MaxSpotInstanceCountExceeded", err: errors.New("MaxSpotInstanceCountExceeded: quota"), want: true},
		{name: "AWS VcpuLimitExceeded", err: errors.New("VcpuLimitExceeded: regional quota"), want: true},
		{name: "Azure AllocationFailed", err: errors.New("AllocationFailed: out of capacity"), want: true},
		{name: "Azure SkuNotAvailable", err: errors.New("SkuNotAvailable"), want: true},
		{name: "Azure QuotaExceeded", err: errors.New("QuotaExceeded"), want: true},
		{name: "GCP resource_pool_exhausted", err: errors.New("resource_pool_exhausted in zone"), want: true},
		{name: "GCP quota exceeded", err: errors.New("QUOTA_EXCEEDED: CPUS"), want: true},
		{name: "Hetzner resource_limit_exceeded", err: errors.New("resource_limit_exceeded"), want: true},
		{name: "Hetzner dedicated_core_limit", err: errors.New("dedicated_core_limit"), want: true},
		{name: "Islo BILLING_NOT_ALLOWED text", err: errors.New("provider=islo: HTTP 402 BILLING_NOT_ALLOWED"), want: true},
		{name: "Docker resource exhausted", err: errors.New("container create: resource exhausted"), want: true},
		{name: "Docker no space", err: errors.New("no space left on device"), want: true},
		{name: "auth failure not capacity", err: errors.New("unauthorized: bad token"), want: false},
		{name: "image not found not capacity", err: errors.New("image not found"), want: false},
		{name: "wrapped capacity", err: fmt.Errorf("create failed: %w", errors.New("VcpuLimitExceeded")), want: true},
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
