package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
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
//     `crabbox crew peers` callers see the gap.
//   - "unsupported-provider" — provider has no BridgeProvider implementation
//     at all. Same semantic as "unsupported" but produced by the framework
//     fallback rather than an explicit per-provider adapter.
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

// Reachability cell states used by the crew doctor matrix. The mapping in
// reachabilityCell captures the asymmetric reality of the four planes — a
// peer reachable only by URL cannot be dialed from a peer that lives only
// on the tailnet (the tailnet member has no public endpoint to push toward).
const (
	reachOK   = "ok"
	reachWarn = "warn"
	reachNo   = "no"
)

// ReachabilityCell records a single source→destination cell of the crew
// reachability matrix. Note carries the honest caveat for non-trivial cells
// (operator-side bridges, asymmetric reach, …).
type ReachabilityCell struct {
	From  string `json:"from"`
	To    string `json:"to"`
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
}

// CrewReachabilityMatrix is the per-crew reachability matrix surfaced by
// `crabbox doctor --crew <name>`. Members lists every distinct transport
// observed in the crew (so callers can interpret the per-row meaning), and
// Cells is the dense matrix indexed by (from, to) transport pair.
type CrewReachabilityMatrix struct {
	Crew       string             `json:"crew"`
	Members    []BridgePeer       `json:"members"`
	Breakdown  map[string]int     `json:"breakdown"`
	Transports []string           `json:"transports"`
	Cells      []ReachabilityCell `json:"cells"`
}

// crewReachableTransports is the canonical ordering for matrix rows/cols.
// `pending` is folded into `tailnet` in the breakdown (class is known; only
// the live endpoint is missing); `none` is reported as a separate row so
// doctor can be honest about the gap.
var crewReachableTransports = []string{
	TransportTailnet,
	TransportURL,
	TransportSSH,
	TransportNone,
}

// reachabilityCell returns the ok/warn/no cell for a (from, to) transport
// pair plus the honest note attached to it. The mapping is asymmetric on
// purpose — see docs/features/crew.md for the rationale.
func reachabilityCell(from, to string) ReachabilityCell {
	cell := ReachabilityCell{From: from, To: to, State: reachNo}
	switch from {
	case TransportTailnet:
		switch to {
		case TransportTailnet:
			cell.State = reachOK
		case TransportURL:
			cell.State = reachOK
			cell.Note = "via outbound HTTPS"
		case TransportSSH:
			cell.State = reachWarn
			cell.Note = "requires operator-side bridge — see SSH-mesh DRAFT PR"
		case TransportNone:
			cell.Note = "destination has no published endpoint"
		}
	case TransportURL:
		switch to {
		case TransportTailnet:
			cell.Note = "no public endpoint on tailnet members"
		case TransportURL:
			cell.State = reachOK
		case TransportSSH:
			cell.State = reachWarn
			cell.Note = "requires operator-side bridge"
		case TransportNone:
			cell.Note = "destination has no published endpoint"
		}
	case TransportSSH:
		switch to {
		case TransportTailnet:
			cell.State = reachWarn
			cell.Note = "requires operator-side bridge"
		case TransportURL:
			cell.State = reachOK
			cell.Note = "via outbound HTTPS"
		case TransportSSH:
			cell.State = reachWarn
			cell.Note = "requires operator-side bridge — peers do not share a mesh"
		case TransportNone:
			cell.Note = "destination has no published endpoint"
		}
	case TransportNone:
		cell.Note = "source provider owns its own connectivity"
	}
	return cell
}

// buildCrewReachabilityMatrix folds a peer list into the doctor matrix. It
// only emits rows/cols for transports actually present in the crew, so a
// crew with no SSH-lease members will not get an "ssh →" row.
func buildCrewReachabilityMatrix(crew string, peers []BridgePeer) CrewReachabilityMatrix {
	breakdown := map[string]int{}
	for _, peer := range peers {
		key := peer.Transport
		if key == TransportPending {
			// Roll "pending" up into "tailnet" for the breakdown so users see
			// a single class count; the per-peer state is still visible in
			// the members list.
			key = TransportTailnet
		}
		breakdown[key]++
	}
	transports := observedTransports(breakdown)
	cells := make([]ReachabilityCell, 0, len(transports)*len(transports))
	for _, from := range transports {
		for _, to := range transports {
			cells = append(cells, reachabilityCell(from, to))
		}
	}
	return CrewReachabilityMatrix{
		Crew:       crew,
		Members:    peers,
		Breakdown:  breakdown,
		Transports: transports,
		Cells:      cells,
	}
}

func observedTransports(breakdown map[string]int) []string {
	seen := map[string]bool{}
	for transport, count := range breakdown {
		if count > 0 {
			seen[transport] = true
		}
	}
	out := make([]string, 0, len(seen))
	// Use the canonical ordering rather than alphabetical so the matrix reads
	// "tailnet, url, ssh, none" — the natural order from least-trust to
	// most-isolated source.
	for _, t := range crewReachableTransports {
		if seen[t] {
			out = append(out, t)
		}
	}
	return out
}

// renderCrewReachabilityMatrix prints the doctor matrix in the same shape
// shown in the user-facing docs. Cell glyphs are pure ASCII so the output
// stays readable on terminals without unicode font support.
func renderCrewReachabilityMatrix(w io.Writer, matrix CrewReachabilityMatrix) {
	fmt.Fprintf(w, "crew %q: %d members\n", matrix.Crew, len(matrix.Members))
	parts := make([]string, 0, len(matrix.Breakdown))
	for _, transport := range crewReachableTransports {
		if matrix.Breakdown[transport] > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", transport, matrix.Breakdown[transport]))
		}
	}
	sort.Strings(parts)
	fmt.Fprintf(w, "  transport breakdown: %s\n", strings.Join(parts, " "))
	if len(matrix.Cells) == 0 {
		return
	}
	fmt.Fprintln(w, "  reachability:")
	for _, cell := range matrix.Cells {
		glyph := reachabilityGlyph(cell.State)
		line := fmt.Sprintf("    %-7s -> %-7s : %s", cell.From, cell.To, glyph)
		if cell.Note != "" {
			line += " (" + cell.Note + ")"
		}
		fmt.Fprintln(w, line)
	}
}

func reachabilityGlyph(state string) string {
	switch state {
	case reachOK:
		return "OK"
	case reachWarn:
		return "WARN"
	case reachNo:
		return "NO"
	default:
		return state
	}
}

// doctorCrewReachabilitySummary builds the cross-provider reachability
// matrix from the local claim sidecar and returns the text rendering. The
// result is plugged into the existing doctor finish() helper without
// adding new top-level commands — doctor stays the single entry point.
func doctorCrewReachabilitySummary(crew string) (CrewReachabilityMatrix, string, error) {
	if crew == "" {
		return CrewReachabilityMatrix{}, "", nil
	}
	claims, err := listLeaseClaims()
	if err != nil {
		return CrewReachabilityMatrix{}, "", err
	}
	matches := filterClaimsForCrew(claims, crew, "")
	peers := make([]BridgePeer, 0, len(matches))
	for _, claim := range matches {
		peers = append(peers, bridgePeerFromClaim(claim, providerTransportClass(claim.Provider)))
	}
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].Slug == peers[j].Slug {
			return peers[i].LeaseID < peers[j].LeaseID
		}
		return peers[i].Slug < peers[j].Slug
	})
	matrix := buildCrewReachabilityMatrix(crew, peers)
	var buf strings.Builder
	renderCrewReachabilityMatrix(&buf, matrix)
	return matrix, buf.String(), nil
}
