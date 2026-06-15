package phala

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-tdx-guest/abi"
	pb "github.com/google/go-tdx-guest/proto/tdx"

	core "github.com/openclaw/crabbox/internal/cli"
)

// realExpectedAppID is the app id of the live dstack CVM the test fixtures were
// captured from. It is the value crabbox would pass as expectedAppID after
// deploying that CVM.
const realExpectedAppID = "944edab771280ede410e7b9e66fcaee6b2c4a12c"

func loadRealInfo(t *testing.T) dstackInfo {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "real_attestation_info.json"))
	if err != nil {
		t.Fatalf("read real_attestation_info.json: %v", err)
	}
	var info dstackInfo
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("decode real_attestation_info.json: %v", err)
	}
	return info
}

func loadRealTCB(t *testing.T, info dstackInfo) tcbInfo {
	t.Helper()
	tcb, err := parseTCBInfo(info.TCBInfo)
	if err != nil {
		t.Fatalf("parse tcb_info: %v", err)
	}
	return tcb
}

// TestParseDstackInfo covers the deterministic parsing split out of the SSH
// fetch: a clean Info object, a leading shell banner before the JSON, and the
// empty / non-JSON rejections.
func TestParseDstackInfo(t *testing.T) {
	ok := `{"app_id":"` + realExpectedAppID + `","instance_id":"ce0b","app_cert":"x","tcb_info":"{}"}`
	for _, tc := range []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "clean json", in: ok},
		{name: "leading shell banner trimmed", in: "Warning: something\n" + ok},
		{name: "surrounding whitespace", in: "  \n" + ok + "\n "},
		{name: "empty body rejected", in: "   \n  ", wantErr: true},
		{name: "non-json rejected", in: "curl: (7) Failed to connect to /var/run/tappd.sock", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, err := parseDstackInfo(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseDstackInfo(%q) = %+v, want error", tc.in, info)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDstackInfo(%q): %v", tc.in, err)
			}
			if info.AppID != realExpectedAppID {
				t.Fatalf("AppID=%q want %q", info.AppID, realExpectedAppID)
			}
		})
	}
}

// TestExtractTDXQuoteFromRealAppCert pins that the quote is carved out of the
// real app certificate's TDX extension and that it is a well-formed v4 quote
// whose MRTD sits at offset 184 and equals tcb_info.mrtd.
func TestExtractTDXQuoteFromRealAppCert(t *testing.T) {
	info := loadRealInfo(t)
	tcb := loadRealTCB(t, info)

	quote, err := extractTDXQuote(info.AppCert)
	if err != nil {
		t.Fatalf("extractTDXQuote: %v", err)
	}
	if len(quote) < 5000 {
		t.Fatalf("quote length = %d, want >= 5000", len(quote))
	}
	if version := uint16(quote[0]) | uint16(quote[1])<<8; version != 4 {
		t.Fatalf("quote version (first uint16-LE) = %d, want 4", version)
	}
	mrtdFromQuote := hex.EncodeToString(quote[tdxQuoteMRTDOffset : tdxQuoteMRTDOffset+tdxMeasurementLen])
	if mrtdFromQuote != strings.ToLower(tcb.MRTD) {
		t.Fatalf("MRTD at offset %d = %s, want tcb_info mrtd %s", tdxQuoteMRTDOffset, mrtdFromQuote, tcb.MRTD)
	}
}

// TestReplayRTMRMatchesRealHardware is the genuineness proof: all four RTMRs
// replayed from the real event log must equal the values recorded in tcb_info.
func TestReplayRTMRMatchesRealHardware(t *testing.T) {
	info := loadRealInfo(t)
	tcb := loadRealTCB(t, info)

	for i := 0; i < 4; i++ {
		replayed := replayRTMR(tcb.EventLog, i)
		if replayed == nil {
			t.Fatalf("RTMR%d replay returned nil", i)
		}
		got := hex.EncodeToString(replayed)
		want := strings.ToLower(strings.TrimSpace(tcb.rtmrFor(i)))
		if got != want {
			t.Fatalf("RTMR%d replay = %s, want tcb_info %s", i, got, want)
		}
	}
}

// TestReplayRTMRMutationBreaks confirms the replay is a real integrity check:
// mutating any single event digest in an RTMR's chain changes the replayed
// value so it no longer matches tcb_info.
func TestReplayRTMRMutationBreaks(t *testing.T) {
	info := loadRealInfo(t)
	tcb := loadRealTCB(t, info)

	// Find the first RTMR3 event and flip one byte of its digest.
	mutated := make([]eventEntry, len(tcb.EventLog))
	copy(mutated, tcb.EventLog)
	flipped := false
	for i := range mutated {
		if mutated[i].IMR == 3 {
			b, err := hex.DecodeString(mutated[i].Digest)
			if err != nil {
				t.Fatalf("decode digest: %v", err)
			}
			b[0] ^= 0xff
			mutated[i].Digest = hex.EncodeToString(b)
			flipped = true
			break
		}
	}
	if !flipped {
		t.Fatal("no RTMR3 event found to mutate")
	}
	replayed := replayRTMR(mutated, 3)
	if hex.EncodeToString(replayed) == strings.ToLower(tcb.RTMR3) {
		t.Fatal("RTMR3 replay still matched after mutating an event digest; replay is not integrity-checking")
	}
}

// TestQuoteRTMRsMatchTcbInfo parses the standalone raw quote fixture with
// go-tdx-guest and asserts the SIGNED quote's MRTD and RTMR0..3 equal the
// tcb_info values, linking the cryptographically signed report to the
// event-log-replayed measurement.
func TestQuoteRTMRsMatchTcbInfo(t *testing.T) {
	info := loadRealInfo(t)
	tcb := loadRealTCB(t, info)

	raw, err := os.ReadFile(filepath.Join("testdata", "real_tdx_quote.bin"))
	if err != nil {
		t.Fatalf("read real_tdx_quote.bin: %v", err)
	}
	proto, err := abi.QuoteToProto(raw)
	if err != nil {
		t.Fatalf("QuoteToProto: %v", err)
	}
	q, ok := proto.(*pb.QuoteV4)
	if !ok {
		t.Fatalf("quote proto type = %T, want *tdx.QuoteV4", proto)
	}
	body := q.GetTdQuoteBody()
	if body == nil {
		t.Fatal("quote has no TD body")
	}
	if got := hex.EncodeToString(body.GetMrTd()); got != strings.ToLower(tcb.MRTD) {
		t.Fatalf("quote MRTD = %s, want %s", got, tcb.MRTD)
	}
	rtmrs := body.GetRtmrs()
	if len(rtmrs) < 4 {
		t.Fatalf("quote RTMRs len = %d, want >= 4", len(rtmrs))
	}
	for i := 0; i < 4; i++ {
		if got, want := hex.EncodeToString(rtmrs[i]), strings.ToLower(tcb.rtmrFor(i)); got != want {
			t.Fatalf("quote RTMR%d = %s, want %s", i, got, want)
		}
	}
}

// TestVerifyAttestationBindsExpectedAppID runs the full offline verification
// (dcap=false) and checks the verified report binds to the expected app id and
// surfaces the event-log identity payloads. A wrong expected app id must be
// rejected.
func TestVerifyAttestationBindsExpectedAppID(t *testing.T) {
	info := loadRealInfo(t)

	report, err := verifyAttestation(info, realExpectedAppID, false)
	if err != nil {
		t.Fatalf("verifyAttestation: %v", err)
	}
	if report.DCAPVerified {
		t.Fatal("report.DCAPVerified = true with dcap=false")
	}
	if report.AppID != realExpectedAppID {
		t.Fatalf("report.AppID = %q, want %q", report.AppID, realExpectedAppID)
	}
	if report.ComposeHash == "" {
		t.Fatal("report.ComposeHash is empty")
	}
	if report.OSImageHash == "" {
		t.Fatal("report.OSImageHash is empty")
	}
	if report.Rtmr3 == "" {
		t.Fatal("report.Rtmr3 is empty")
	}
	// The report fields must come from the event-log payloads, not the untrusted
	// top-level Info fields; cross-check against the event log directly.
	tcb := loadRealTCB(t, info)
	if want := rtmr3Event(tcb.EventLog, "compose-hash"); report.ComposeHash != want {
		t.Fatalf("report.ComposeHash = %q, want event-log %q", report.ComposeHash, want)
	}
	if want := rtmr3Event(tcb.EventLog, "app-id"); report.AppID != want {
		t.Fatalf("report.AppID = %q, want event-log %q", report.AppID, want)
	}

	// A wrong expected app id must be refused.
	if _, err := verifyAttestation(info, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", false); err == nil {
		t.Fatal("verifyAttestation accepted a wrong expected app id")
	}
}

// TestVerifyAttestationRejectsTamperedMeasurement flips a byte in an RTMR3 event
// digest and confirms the RTMR replay mismatch causes verifyAttestation to fail.
func TestVerifyAttestationRejectsTamperedMeasurement(t *testing.T) {
	info := loadRealInfo(t)
	tcb := loadRealTCB(t, info)

	// Mutate one RTMR3 event digest, then re-encode tcb_info back into the Info
	// response so verifyAttestation sees the tampered event log.
	flipped := false
	for i := range tcb.EventLog {
		if tcb.EventLog[i].IMR == 3 {
			b, err := hex.DecodeString(tcb.EventLog[i].Digest)
			if err != nil {
				t.Fatalf("decode digest: %v", err)
			}
			b[0] ^= 0xff
			tcb.EventLog[i].Digest = hex.EncodeToString(b)
			flipped = true
			break
		}
	}
	if !flipped {
		t.Fatal("no RTMR3 event found to mutate")
	}
	tampered, err := json.Marshal(tcb)
	if err != nil {
		t.Fatalf("marshal tampered tcb_info: %v", err)
	}
	info.TCBInfo = string(tampered)

	if _, err := verifyAttestation(info, realExpectedAppID, false); err == nil {
		t.Fatal("verifyAttestation accepted a tampered measurement")
	} else if !strings.Contains(err.Error(), "RTMR") {
		t.Fatalf("expected an RTMR replay mismatch error, got: %v", err)
	}
}

// TestDCAPVerifyRealQuote runs the full network DCAP verification (signature
// chains to the Intel SGX/TDX Root CA) against the real quote. It is skipped by
// default because it reaches Intel PCS and the fixture's TCB can expire; set
// CRABBOX_TDX_DCAP_NETWORK_TEST=1 to run it.
func TestDCAPVerifyRealQuote(t *testing.T) {
	if os.Getenv("CRABBOX_TDX_DCAP_NETWORK_TEST") == "" {
		t.Skip("skipping network DCAP verification; set CRABBOX_TDX_DCAP_NETWORK_TEST=1 to run (reaches Intel PCS, fixture TCB may expire)")
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "real_tdx_quote.bin"))
	if err != nil {
		t.Fatalf("read real_tdx_quote.bin: %v", err)
	}
	if err := dcapVerifyQuote(raw); err != nil {
		t.Fatalf("DCAP verify of real quote failed: %v", err)
	}
}

// TestAttestEnabledDefaultsOn pins that the gate is on by default (nil config)
// and that an explicit false disables it.
func TestAttestEnabledDefaultsOn(t *testing.T) {
	var cfg core.Config
	if !attestEnabled(cfg) {
		t.Fatal("attestEnabled = false with nil Phala.Attest, want true (default on)")
	}
	disabled := false
	cfg.Phala.Attest = &disabled
	if attestEnabled(cfg) {
		t.Fatal("attestEnabled = true with Phala.Attest=false, want false")
	}
	enabled := true
	cfg.Phala.Attest = &enabled
	if !attestEnabled(cfg) {
		t.Fatal("attestEnabled = false with Phala.Attest=true, want true")
	}
}
