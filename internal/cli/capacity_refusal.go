package cli

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"google.golang.org/api/googleapi"
)

// CapacityRefusal marks a lease-create error that may clear when capacity frees
// (quota, concurrent-box caps, transient resource exhaustion). Providers may
// implement this interface; IsCapacityRefusal also recognizes typed HTTP status
// codes and a documented text fallback for providers that only return strings.
type CapacityRefusal interface {
	error
	CapacityRefusal() bool
}

// httpStatusCoder is implemented by HTTP error types that expose a numeric
// status. Crabbox transport errors implement this so the shard retry path never
// switches on provider names.
type httpStatusCoder interface {
	HTTPStatusCode() int
}

// IsCapacityRefusal reports whether err is a capacity or quota refusal that
// shard create may retry. Non-capacity errors return false.
//
// Recognition order (shared across every provider):
//  1. CapacityRefusal interface (preferred typed signal)
//  2. HTTP status via httpStatusCoder or common SDK HTTP errors: 429, 503, 402
//  3. Documented text fallback for providers that only return message text
//
// Covered signals (examples; recognition is signal-based, not provider-switched):
//   - AWS-style codes: InsufficientInstanceCapacity, UnfulfillableCapacity,
//     MaxSpotInstanceCountExceeded, VcpuLimitExceeded
//   - Azure-style codes: SkuNotAvailable, QuotaExceeded, AllocationFailed,
//     ZonalAllocationFailed, OverconstrainedAllocationRequest
//   - GCP-style text / googleapi 429: quota exceeded, rate limit,
//     resource_pool_exhausted, does not have enough resources
//   - Hetzner-style codes: dedicated_core_limit, resource_limit_exceeded,
//     server_type_not_available, location_not_available
//   - Billing-slot APIs (including Islo): HTTP 402 / BILLING_NOT_ALLOWED
//   - Coordinator and generic HTTP APIs: 429, 503
//   - Docker / local host: resource exhausted, no space left on device,
//     cannot allocate, too many containers
func IsCapacityRefusal(err error) bool {
	if err == nil {
		return false
	}
	var marked CapacityRefusal
	if errors.As(err, &marked) {
		return marked.CapacityRefusal()
	}
	if code, ok := capacityHTTPStatus(err); ok {
		switch code {
		case http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusPaymentRequired:
			return true
		}
	}
	return capacityRefusalMessage(err.Error())
}

func capacityHTTPStatus(err error) (int, bool) {
	var coder httpStatusCoder
	if errors.As(err, &coder) {
		return coder.HTTPStatusCode(), true
	}
	// Common cloud SDK HTTP errors (not crabbox provider switches).
	var googleErr *googleapi.Error
	if errors.As(err, &googleErr) && googleErr != nil {
		return googleErr.Code, true
	}
	var azureErr *azcore.ResponseError
	if errors.As(err, &azureErr) && azureErr != nil {
		return azureErr.StatusCode, true
	}
	return 0, false
}

func capacityRefusalMessage(message string) bool {
	s := strings.ToLower(message)
	if s == "" {
		return false
	}
	// Stable provider API codes and phrases. Keep this list signal-based; do
	// not branch on provider names here.
	needles := []string{
		"insufficientinstancecapacity",
		"unfulfillablecapacity",
		"maxspotinstancecountexceeded",
		"vcpulimitexceeded",
		"skunotavailable",
		"quotaexceeded",
		"quota_exceeded",
		"quota exceeded",
		"exceeded quota",
		"allocationfailed",
		"zonalallocationfailed",
		"overconstrainedallocationrequest",
		"resource_pool_exhausted",
		"does not have enough resources",
		"dedicated_core_limit",
		"resource_limit_exceeded",
		"server_type_not_available",
		"location_not_available",
		"billing_not_allowed",
		"resource exhausted",
		"resourceexhaust",
		"no space left on device",
		"cannot allocate",
		"too many containers",
		"rate limit",
		"ratelimit",
		"out of capacity",
	}
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
