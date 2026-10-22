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

// httpStatusCoder is implemented by provider HTTP error types that expose a
// numeric status without requiring the classifier to switch on provider names.
type httpStatusCoder interface {
	HTTPStatusCode() int
}

// IsCapacityRefusal reports whether err is a capacity or quota refusal that
// shard create may retry. Non-capacity errors return false.
//
// Recognition order:
//  1. CapacityRefusal interface (preferred typed signal)
//  2. HTTP status via httpStatusCoder, CoordinatorHTTPError, HetznerHTTPError,
//     googleapi.Error, or azcore.ResponseError: 429, 503, and 402
//  3. Documented text fallback for providers that only return message text
//
// Covered signals (not an exhaustive runtime inventory of every provider binary):
//   - AWS: InsufficientInstanceCapacity, UnfulfillableCapacity,
//     MaxSpotInstanceCountExceeded, VcpuLimitExceeded
//   - Azure: SkuNotAvailable, QuotaExceeded, AllocationFailed,
//     ZonalAllocationFailed, OverconstrainedAllocationRequest
//   - GCP: quota exceeded / QuotaExceeded, rate limit, resource_pool_exhausted,
//     does not have enough resources (also googleapi 429)
//   - Hetzner: dedicated_core_limit, resource_limit_exceeded,
//     server_type_not_available, location_not_available (also HTTP 429/503)
//   - Islo and other billing-slot APIs: HTTP 402 / BILLING_NOT_ALLOWED
//   - Coordinator and generic HTTP APIs: 429 Too Many Requests, 503 Unavailable
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
	var coord CoordinatorHTTPError
	if errors.As(err, &coord) {
		return coord.StatusCode, true
	}
	var hetzner HetznerHTTPError
	if errors.As(err, &hetzner) {
		return hetzner.StatusCode, true
	}
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
