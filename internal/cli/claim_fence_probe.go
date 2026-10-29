//go:build crabbox_claim_fence_probe

package cli

import (
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// Test-only claim-fence probe for live authority proofs. Release builds do not
// compile this file. With CRABBOX_CLAIM_FENCE_PROBE=<site>:<revoke|reassign|none>
// the probe revokes or reassigns the lease claim when the named fence is
// entered: after the caller read the claim and ran its earlier effects, before
// the fence rereads it. It fires once per process and logs every fence entry,
// admission and outcome to stderr, so a live run shows which effects the real
// host and guest owners were admitted to perform.
const claimFenceProbeEnv = "CRABBOX_CLAIM_FENCE_PROBE"

// A documentation-only (TEST-NET-1) endpoint for a reassigned claim.
const claimFenceProbeReassignedHost = "192.0.2.77"

var (
	claimFenceProbeMu    sync.Mutex
	claimFenceProbeFired bool

	claimFenceProbeClosureSuffix = regexp.MustCompile(`(\.func\d+(\.\d+)*)+$`)
)

func claimFenceProbe(leaseID string, action func() error) (func() error, func(error) error) {
	spec := strings.TrimSpace(os.Getenv(claimFenceProbeEnv))
	if spec == "" {
		return action, passClaimFenceResult
	}
	site := claimFenceProbeSite()
	claimFenceProbeLogf("enter site=%s lease=%s", site, leaseID)
	wantSite, mode, ok := strings.Cut(spec, ":")
	if !ok {
		claimFenceProbeLogf("ignored malformed %s", claimFenceProbeEnv)
	} else if claimFenceProbeMatches(site, wantSite) && claimFenceProbeArm() {
		if err := claimFenceProbeSwap(leaseID, mode); err != nil {
			claimFenceProbeLogf("swap-failed mode=%s site=%s lease=%s err=%q", mode, site, leaseID, err.Error())
		} else {
			claimFenceProbeLogf("swap mode=%s site=%s lease=%s", mode, site, leaseID)
		}
	}
	admitted := false
	wrapped := action
	if action != nil {
		wrapped = func() error {
			admitted = true
			claimFenceProbeLogf("admitted site=%s lease=%s", site, leaseID)
			return action()
		}
	}
	return wrapped, func(err error) error {
		switch {
		case err == nil:
			claimFenceProbeLogf("result site=%s lease=%s outcome=ok", site, leaseID)
		case !admitted:
			claimFenceProbeLogf("result site=%s lease=%s outcome=refused err=%q", site, leaseID, err.Error())
		default:
			claimFenceProbeLogf("result site=%s lease=%s outcome=effect-error", site, leaseID)
		}
		return err
	}
}

func claimFenceProbeLogf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "claim-fence-probe: "+format+"\n", args...)
}

func passClaimFenceResult(err error) error { return err }

func claimFenceProbeArm() bool {
	claimFenceProbeMu.Lock()
	defer claimFenceProbeMu.Unlock()
	if claimFenceProbeFired {
		return false
	}
	claimFenceProbeFired = true
	return true
}

// claimFenceProbeSite names the first caller outside the fence helpers, e.g.
// "hyperv.(*backend).waitForResumedLease".
func claimFenceProbeSite() string {
	pcs := make([]uintptr, 32)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		frame, more := frames.Next()
		name := frame.Function
		if name != "" && !strings.Contains(name, "LeaseClaimUnchanged") && !strings.Contains(name, "claimFenceProbe") {
			name = strings.TrimPrefix(name, "github.com/openclaw/crabbox/internal/")
			name = strings.TrimPrefix(name, "providers/")
			return claimFenceProbeClosureSuffix.ReplaceAllString(name, "")
		}
		if !more {
			return "unknown"
		}
	}
}

func claimFenceProbeMatches(site, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	return site == want || site[strings.LastIndex(site, ".")+1:] == want
}

func claimFenceProbeSwap(leaseID, mode string) error {
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		return err
	}
	current, exists, err := readLeaseClaimPathWithPresence(path)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("claim already absent")
	}
	switch mode {
	case "none":
		return nil
	case "revoke":
		return RemoveLeaseClaimIfUnchanged(leaseID, current)
	case "reassign":
		replacement := cloneLeaseClaim(current)
		if replacement.ProviderScope != "" {
			replacement.ProviderScope += "-reassigned"
		}
		if replacement.CloudID != "" {
			replacement.CloudID += "-reassigned"
		}
		if replacement.SSHHost != "" {
			replacement.SSHHost = claimFenceProbeReassignedHost
		}
		_, err := ReplaceLeaseClaimIfUnchangedDurableReturning(leaseID, current, replacement)
		return err
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
}
