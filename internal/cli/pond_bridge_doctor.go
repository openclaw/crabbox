package cli

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// BridgePeerProbeResult records the result of a single HTTPS reachability
// probe against one BridgePeerTarget. The result is intentionally simple —
// the doctor surface is a smoke check, not a uptime monitor.
type BridgePeerProbeResult struct {
	Slug       string `json:"slug"`
	URL        string `json:"url"`
	Port       int    `json:"port"`
	StatusCode int    `json:"statusCode,omitempty"`
	State      string `json:"state"`
	Detail     string `json:"detail,omitempty"`
}

// ProbeBridgePeers issues a HEAD request against each peer's first published
// target with a strict per-probe timeout. The total wall-clock budget is
// `perProbeTimeout * len(peers)` in the worst case; callers should cap it via
// the parent context if they care.
//
// The state is one of:
//
//   - "reachable" — HEAD returned a status code (any code).
//   - "unreachable" — HEAD failed (network error, TLS error, dial timeout).
//   - "no-targets" — peer has no published bridge targets.
//   - "unsupported" — provider explicitly does not implement the bridge
//     plane (e.g. modal/cloudflare/tensorlake). The peer is still listed so
//     `crabbox pond peers` callers see the gap.
//   - "unsupported-provider" — provider has no BridgeProvider implementation
//     at all. Same semantic as "unsupported" but produced before an explicit
//     per-provider adapter is loaded.
//
// A peer that returned a 4xx/5xx is still recorded as "reachable" because the
// bridge plane only asserts that the public URL exists and routes; the user
// app served on the port may legitimately answer 404 to a HEAD request.
func ProbeBridgePeers(ctx context.Context, client *http.Client, peers []BridgePeer, perProbeTimeout time.Duration) []BridgePeerProbeResult {
	if client == nil {
		client = &http.Client{Timeout: perProbeTimeout}
	}
	if perProbeTimeout <= 0 {
		perProbeTimeout = 3 * time.Second
	}
	results := make([]BridgePeerProbeResult, 0, len(peers))
	for _, peer := range peers {
		if len(peer.Targets) == 0 {
			state := peer.BridgeState
			if state == "" {
				state = "no-targets"
			}
			results = append(results, BridgePeerProbeResult{
				Slug:  peer.Slug,
				State: state,
			})
			continue
		}
		target := peer.Targets[0]
		probeCtx, cancel := context.WithTimeout(ctx, perProbeTimeout)
		req, err := http.NewRequestWithContext(probeCtx, http.MethodHead, target.URL, nil)
		if err != nil {
			cancel()
			results = append(results, BridgePeerProbeResult{
				Slug:   peer.Slug,
				URL:    target.URL,
				Port:   target.Port,
				State:  "unreachable",
				Detail: err.Error(),
			})
			continue
		}
		resp, err := client.Do(req)
		cancel()
		if err != nil {
			results = append(results, BridgePeerProbeResult{
				Slug:   peer.Slug,
				URL:    target.URL,
				Port:   target.Port,
				State:  "unreachable",
				Detail: shortenProbeError(err.Error()),
			})
			continue
		}
		_ = resp.Body.Close()
		results = append(results, BridgePeerProbeResult{
			Slug:       peer.Slug,
			URL:        target.URL,
			Port:       target.Port,
			StatusCode: resp.StatusCode,
			State:      "reachable",
		})
	}
	return results
}

// shortenProbeError trims the verbose context/url prefix that net/http likes
// to put on dial errors so the doctor row stays inside a single terminal
// line. The full error is still available in the error chain if the caller
// asks for it.
func shortenProbeError(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.LastIndex(s, ": "); idx >= 0 && idx < len(s)-2 {
		return s[idx+2:]
	}
	return s
}

// ReachabilityCell records a single source→destination cell of the pond
// reachability matrix. Note carries the honest caveat for non-trivial cells
// (operator-side bridges, asymmetric reach, …).
type ReachabilityCell struct {
	From  string `json:"from"`
	To    string `json:"to"`
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
}

// PondReachabilityMatrix is the per-pond reachability matrix surfaced by
// `crabbox doctor --pond <name>`. Members lists every distinct transport
// observed in the pond (so callers can interpret the per-row meaning), and
// Cells is the dense matrix indexed by (from, to) transport pair.
type PondReachabilityMatrix struct {
	Pond       string             `json:"pond"`
	Members    []BridgePeer       `json:"members"`
	Breakdown  map[string]int     `json:"breakdown"`
	Transports []string           `json:"transports"`
	Cells      []ReachabilityCell `json:"cells"`
}
