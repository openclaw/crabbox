package phala

import (
	"context"
	"crypto/sha512"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"

	"github.com/google/go-tdx-guest/abi"
	pb "github.com/google/go-tdx-guest/proto/tdx"
	"github.com/google/go-tdx-guest/verify"

	core "github.com/openclaw/crabbox/internal/cli"
)

// tdxQuoteExtensionOID is the X.509 extension OID under which the dstack guest
// agent embeds the raw Intel TDX quote in the app TLS certificate
// (1.3.6.1.4.1.62397.1.1).
var tdxQuoteExtensionOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 62397, 1, 1}

const (
	// tdxQuoteMRTDOffset is the byte offset of the 48-byte MRTD measurement
	// inside a raw Intel TDX v4 quote (header 48 bytes + TD report MRTD field).
	tdxQuoteMRTDOffset = 184
	// tdxMeasurementLen is the SHA-384 digest length used for MRTD and every RTMR.
	tdxMeasurementLen = 48
	// tdxTeeType is the tee_type discriminator for Intel TDX in the quote header.
	tdxTeeType = 0x81
	// tappdInfoEndpoint is the dstack guest agent prpc endpoint that returns the
	// CVM identity + attestation bundle.
	tappdInfoEndpoint = "http://localhost/prpc/Tappd.Info"
	// tappdSocket is the unix socket the dstack guest agent (a Rocket HTTP
	// server) listens on inside the CVM.
	tappdSocket = "/var/run/tappd.sock"
)

// dstackInfo is the Tappd.Info response from the dstack guest agent. tcb_info is
// transported as a JSON STRING, so it is captured raw and parsed separately.
type dstackInfo struct {
	AppID        string `json:"app_id"`
	InstanceID   string `json:"instance_id"`
	AppName      string `json:"app_name"`
	AppCert      string `json:"app_cert"`
	TCBInfo      string `json:"tcb_info"`
	OSImageHash  string `json:"os_image_hash"`
	ComposeHash  string `json:"compose_hash"`
	MRAggregated string `json:"mr_aggregated"`
}

// tcbInfo is the inner JSON document carried (as a string) under
// dstackInfo.TCBInfo. It holds the canonical measurement registers and the
// boot-time event log that replays into the RTMRs.
type tcbInfo struct {
	MRTD         string       `json:"mrtd"`
	RTMR0        string       `json:"rtmr0"`
	RTMR1        string       `json:"rtmr1"`
	RTMR2        string       `json:"rtmr2"`
	RTMR3        string       `json:"rtmr3"`
	MRAggregated string       `json:"mr_aggregated"`
	OSImageHash  string       `json:"os_image_hash"`
	ComposeHash  string       `json:"compose_hash"`
	DeviceID     string       `json:"device_id"`
	EventLog     []eventEntry `json:"event_log"`
}

// eventEntry is one measured-boot event. The digest is folded into its target
// RTMR; named RTMR3 events additionally bind the CVM identity (app-id,
// compose-hash, os-image-hash, mr-kms).
type eventEntry struct {
	IMR          int    `json:"imr"`
	EventType    uint32 `json:"event_type"`
	Digest       string `json:"digest"`
	Event        string `json:"event"`
	EventPayload string `json:"event_payload"`
}

// rtmrFor returns the expected RTMR value for register i (0..3) from tcb_info.
func (t tcbInfo) rtmrFor(i int) string {
	switch i {
	case 0:
		return t.RTMR0
	case 1:
		return t.RTMR1
	case 2:
		return t.RTMR2
	case 3:
		return t.RTMR3
	default:
		return ""
	}
}

// AttestationReport is the verified TDX identity surfaced into the lease after a
// successful attestation check. Every field is derived from the SIGNED quote and
// the genuineness-checked event log, not from untrusted top-level Info fields.
type AttestationReport struct {
	AppID        string
	ComposeHash  string
	OSImageHash  string
	MrKms        string
	Rtmr3        string
	DCAPVerified bool
}

// extractTDXQuote parses the app TLS certificate chain, finds the dstack TDX
// quote extension (OID 1.3.6.1.4.1.62397.1.1), and carves the raw TDX quote out
// of the (possibly DER-OCTET-STRING-wrapped) extension value by locating the TDX
// quote header. The header is: version uint16-LE in {4,5}, att_key_type uint16-LE
// in {2,3}, tee_type uint32-LE == 0x81. Scanning for that pattern is robust to
// the one or two layers of OCTET STRING DER prefix that wrap the quote.
func extractTDXQuote(appCertPEM string) ([]byte, error) {
	ext, err := findTDXQuoteExtension(appCertPEM)
	if err != nil {
		return nil, err
	}
	quote, err := carveTDXQuote(ext)
	if err != nil {
		return nil, err
	}
	return quote, nil
}

// findTDXQuoteExtension returns the raw value bytes of the TDX quote extension
// from the first certificate in the PEM chain that carries it.
func findTDXQuoteExtension(appCertPEM string) ([]byte, error) {
	rest := []byte(appCertPEM)
	found := false
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		found = true
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			// Skip certs we cannot parse; a later cert in the chain may carry the
			// extension. Only fail at the end if no cert had it.
			continue
		}
		for _, ext := range cert.Extensions {
			if ext.Id.Equal(tdxQuoteExtensionOID) {
				if len(ext.Value) == 0 {
					return nil, fmt.Errorf("phala attestation: TDX quote extension %s is empty", tdxQuoteExtensionOID)
				}
				return ext.Value, nil
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("phala attestation: app_cert contained no PEM CERTIFICATE block")
	}
	return nil, fmt.Errorf("phala attestation: no certificate carried the TDX quote extension %s", tdxQuoteExtensionOID)
}

// carveTDXQuote locates the raw TDX quote inside the extension value by scanning
// for the TDX v4/v5 header byte pattern. The dstack agent wraps the quote in one
// or two layers of DER OCTET STRING, so the quote does not begin at offset 0.
func carveTDXQuote(ext []byte) ([]byte, error) {
	// The header is 8 bytes: version(2,LE) att_key_type(2,LE) tee_type(4,LE).
	for off := 0; off+8 <= len(ext); off++ {
		version := uint16(ext[off]) | uint16(ext[off+1])<<8
		if version != 4 && version != 5 {
			continue
		}
		attKeyType := uint16(ext[off+2]) | uint16(ext[off+3])<<8
		if attKeyType != 2 && attKeyType != 3 {
			continue
		}
		teeType := uint32(ext[off+4]) | uint32(ext[off+5])<<8 | uint32(ext[off+6])<<16 | uint32(ext[off+7])<<24
		if teeType != tdxTeeType {
			continue
		}
		quote := ext[off:]
		if len(quote) < tdxQuoteMRTDOffset+tdxMeasurementLen {
			return nil, fmt.Errorf("phala attestation: TDX quote header found at offset %d but only %d bytes follow", off, len(quote))
		}
		return quote, nil
	}
	return nil, fmt.Errorf("phala attestation: no TDX quote header (version{4,5}/att_key_type{2,3}/tee_type=0x81) found in %d-byte extension value", len(ext))
}

// replayRTMR recomputes RTMR register imr by folding every event_log entry with
// imr==imr into a running SHA-384 accumulator, starting from 48 zero bytes:
//
//	rtmr = 48 zero bytes
//	for each event with imr==imr (in order): rtmr = SHA384(rtmr || digest)
//
// A mismatch between the replayed value and tcb_info's recorded rtmr means the
// measured boot was tampered with. Returns nil if any event digest is not valid
// hex (the caller treats a nil replay as a verification failure).
func replayRTMR(events []eventEntry, imr int) []byte {
	acc := make([]byte, tdxMeasurementLen)
	for _, e := range events {
		if e.IMR != imr {
			continue
		}
		digest, err := hex.DecodeString(strings.TrimSpace(e.Digest))
		if err != nil {
			return nil
		}
		h := sha512.New384()
		h.Write(acc)
		h.Write(digest)
		acc = h.Sum(nil)
	}
	return acc
}

// rtmr3Event returns the event_payload of the named RTMR3 event (e.g. "app-id",
// "compose-hash"), or "" if absent.
func rtmr3Event(events []eventEntry, name string) string {
	for _, e := range events {
		if e.IMR == 3 && e.Event == name {
			return strings.TrimSpace(e.EventPayload)
		}
	}
	return ""
}

// normalizeAppID canonicalizes an app id for comparison: lowercased, trimmed,
// and stripped of an optional 0x prefix. The deploy-result app id and the
// event-log app-id payload are compared after this normalization.
func normalizeAppID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	id = strings.TrimPrefix(id, "0x")
	return id
}

// verifyAttestation runs the full TDX attestation check over a dstack Info
// response and returns the verified identity. It performs, in order:
//
//  2. parse the inner tcb_info JSON string;
//  3. replay all four RTMRs from the event log and assert they equal tcb_info
//     (genuineness of the measured boot);
//  4. extract the raw TDX quote from the app TLS certificate;
//  5. (if dcap) DCAP-verify the quote's signature chains to the Intel SGX/TDX
//     root CA, then assert the SIGNED quote's MRTD and RTMRs equal the replayed
//     tcb_info values (links the signature to the measurement);
//  6. bind identity: assert the RTMR3 app-id event equals expectedAppID.
//
// expectedAppID is the app id of the CVM crabbox just created; it proves the
// genuine, measured enclave is OUR deployment rather than some other attested
// box. dcap drives whether the network DCAP signature verification runs (it
// reaches Intel PCS), so callers can run the offline genuineness + binding
// checks deterministically and gate the network check separately.
func verifyAttestation(info dstackInfo, expectedAppID string, dcap bool) (AttestationReport, error) {
	// Step 2: parse the inner tcb_info JSON string.
	tcb, err := parseTCBInfo(info.TCBInfo)
	if err != nil {
		return AttestationReport{}, err
	}

	// Step 3: replay all four RTMRs and assert they match tcb_info. This is the
	// genuineness proof: any tampered event digest changes the replayed RTMR.
	for i := 0; i < 4; i++ {
		replayed := replayRTMR(tcb.EventLog, i)
		if replayed == nil {
			return AttestationReport{}, fmt.Errorf("phala attestation: RTMR%d event log contains a non-hex digest", i)
		}
		expected := strings.ToLower(strings.TrimSpace(tcb.rtmrFor(i)))
		if got := hex.EncodeToString(replayed); got != expected {
			return AttestationReport{}, fmt.Errorf("phala attestation: RTMR%d replay mismatch (replayed %s, tcb_info %s) -- measured boot is not genuine", i, got, expected)
		}
	}

	// Step 4: extract the raw TDX quote from the app certificate.
	quote, err := extractTDXQuote(info.AppCert)
	if err != nil {
		return AttestationReport{}, err
	}

	// Parse the quote proto so the SIGNED measurements can be cross-checked
	// against the replayed tcb_info values (this links signature to measurement
	// regardless of whether the network DCAP check runs).
	tdBody, err := quoteTDBody(quote)
	if err != nil {
		return AttestationReport{}, err
	}
	if err := assertQuoteMatchesTCB(tdBody, tcb); err != nil {
		return AttestationReport{}, err
	}

	dcapVerified := false
	if dcap {
		// Step 5: DCAP signature verification -- proves genuine Intel silicon by
		// chaining the quote signature to the Intel SGX/TDX Root CA. GetCollateral
		// fetches the verification collateral from Intel PCS over the network.
		if err := dcapVerifyQuote(quote); err != nil {
			return AttestationReport{}, fmt.Errorf("phala attestation: DCAP quote verification failed: %w", err)
		}
		dcapVerified = true
	}

	// Step 6: identity binding -- the RTMR3 app-id event must equal the app id of
	// the CVM crabbox created. Without this, a genuine-and-measured but UNRELATED
	// attested box would pass.
	appIDEvent := rtmr3Event(tcb.EventLog, "app-id")
	if appIDEvent == "" {
		return AttestationReport{}, fmt.Errorf("phala attestation: event log has no RTMR3 app-id event")
	}
	if normalizeAppID(appIDEvent) != normalizeAppID(expectedAppID) {
		return AttestationReport{}, fmt.Errorf("phala attestation: attested app-id %q does not match expected CVM app id %q -- attested box is not our deployment", appIDEvent, expectedAppID)
	}

	report := AttestationReport{
		AppID:        appIDEvent,
		ComposeHash:  rtmr3Event(tcb.EventLog, "compose-hash"),
		OSImageHash:  rtmr3Event(tcb.EventLog, "os-image-hash"),
		MrKms:        rtmr3Event(tcb.EventLog, "mr-kms"),
		Rtmr3:        strings.ToLower(strings.TrimSpace(tcb.RTMR3)),
		DCAPVerified: dcapVerified,
	}
	return report, nil
}

// parseTCBInfo decodes the inner tcb_info JSON string carried by dstackInfo.
func parseTCBInfo(raw string) (tcbInfo, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return tcbInfo{}, fmt.Errorf("phala attestation: Info response carried empty tcb_info")
	}
	var tcb tcbInfo
	if err := json.Unmarshal([]byte(raw), &tcb); err != nil {
		return tcbInfo{}, fmt.Errorf("phala attestation: parse tcb_info JSON: %w", err)
	}
	if len(tcb.EventLog) == 0 {
		return tcbInfo{}, fmt.Errorf("phala attestation: tcb_info carried an empty event log")
	}
	return tcb, nil
}

// quoteTDBody parses a raw TDX quote into its proto and returns the TD quote
// body holding MRTD and the RTMRs.
func quoteTDBody(quote []byte) (*pb.TDQuoteBody, error) {
	proto, err := abi.QuoteToProto(quote)
	if err != nil {
		return nil, fmt.Errorf("phala attestation: parse TDX quote: %w", err)
	}
	q, ok := proto.(*pb.QuoteV4)
	if !ok {
		return nil, fmt.Errorf("phala attestation: unexpected quote proto type %T (want *tdx.QuoteV4)", proto)
	}
	body := q.GetTdQuoteBody()
	if body == nil {
		return nil, fmt.Errorf("phala attestation: TDX quote has no TD report body")
	}
	return body, nil
}

// assertQuoteMatchesTCB checks that the MRTD and RTMRs embedded in the SIGNED
// quote equal the values in tcb_info, binding the cryptographically signed report
// to the event-log-replayed measurement.
func assertQuoteMatchesTCB(body *pb.TDQuoteBody, tcb tcbInfo) error {
	if got, want := hex.EncodeToString(body.GetMrTd()), strings.ToLower(strings.TrimSpace(tcb.MRTD)); got != want {
		return fmt.Errorf("phala attestation: quote MRTD %s != tcb_info mrtd %s", got, want)
	}
	rtmrs := body.GetRtmrs()
	if len(rtmrs) < 4 {
		return fmt.Errorf("phala attestation: quote has %d RTMRs, expected at least 4", len(rtmrs))
	}
	for i := 0; i < 4; i++ {
		if got, want := hex.EncodeToString(rtmrs[i]), strings.ToLower(strings.TrimSpace(tcb.rtmrFor(i))); got != want {
			return fmt.Errorf("phala attestation: quote RTMR%d %s != tcb_info rtmr%d %s", i, got, i, want)
		}
	}
	return nil
}

// dcapVerifyQuote runs the go-tdx-guest DCAP verification with collateral fetched
// from Intel PCS, confirming the quote signature chains to the Intel SGX/TDX Root
// CA (genuine Intel silicon). This reaches the network.
func dcapVerifyQuote(quote []byte) error {
	proto, err := abi.QuoteToProto(quote)
	if err != nil {
		return fmt.Errorf("parse TDX quote: %w", err)
	}
	opts := verify.DefaultOptions()
	opts.GetCollateral = true
	return verify.TdxQuote(proto, opts)
}

// fetchAttestation pulls the dstack guest agent's Tappd.Info over SSH by curling
// its unix socket inside the CVM, returning the decoded Info response. It uses
// the same captured-stdout SSH path the rest of the backend relies on.
func (b *backend) fetchAttestation(ctx context.Context, target core.SSHTarget) (dstackInfo, error) {
	out, err := core.RunSSHOutput(ctx, target, tappdInfoFetchCommand())
	if err != nil {
		return dstackInfo{}, fmt.Errorf("fetch dstack attestation over SSH: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return dstackInfo{}, fmt.Errorf("dstack guest agent returned no attestation Info (is %s present?)", tappdSocket)
	}
	// The curl call may emit transport noise on stderr (discarded) but the JSON
	// object on stdout; trim to the first '{' defensively in case a shell banner
	// precedes it.
	if idx := strings.IndexByte(out, '{'); idx > 0 {
		out = out[idx:]
	}
	var info dstackInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return dstackInfo{}, fmt.Errorf("decode dstack Tappd.Info JSON: %w", err)
	}
	return info, nil
}

// tappdInfoFetchCommand curls the dstack guest agent's Tappd.Info endpoint over
// its unix socket. The guest agent is a Rocket HTTP server on
// /var/run/tappd.sock; the response is the attestation bundle JSON.
func tappdInfoFetchCommand() string {
	return fmt.Sprintf("curl -s --unix-socket %s %s", tappdSocket, tappdInfoEndpoint)
}
