package blacksmith

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	metadataReceiptNonce = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	metadataDigestFirst  = "0123456789abcdef0123456789abcdef"
	metadataDigestLast   = "fedcba9876543210fedcba9876543210"
)

func metadataFrames(values ...string) string {
	var wire strings.Builder
	for _, value := range values {
		wire.WriteString("\x1eCRABBOX_BS_" + metadataReceiptNonce + ":" + value + "\x1f")
	}
	return wire.String()
}

func readMetadataReceipt(t *testing.T, chunks ...string) (*blacksmithArtifactReceipt, string, error, int) {
	t.Helper()
	var visible bytes.Buffer
	exits := 0
	r := &blacksmithArtifactReceipt{
		nonce: metadataReceiptNonce, now: func() time.Time { return time.Unix(123, 0) },
		output: &visible, onExit: func() { exits++ },
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	demux := blacksmithControlDemux{data: r.data, record: r.record, cancel: cancel}
	for _, chunk := range chunks {
		if n, err := demux.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatal("control stream write did not consume its input")
		}
	}
	demux.finish()
	if (demux.err == nil) != (context.Cause(ctx) == nil) {
		t.Fatal("receipt failure and transport cancellation disagree")
	}
	return r, visible.String(), demux.err, exits
}

func TestBlacksmithArtifactMetadataCompleteReceipt(t *testing.T) {
	for _, size := range []int64{1, 10 * 1024 * 1024} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			wire := "setup\n" + metadataFrames("start") + "workload\n" + metadataFrames("exit:23") +
				"private collector diagnostic" + metadataFrames("bytes:"+strconv.FormatInt(size, 10),
				"digest0:"+metadataDigestFirst, "digest1:"+metadataDigestLast, "end:0")
			r, visible, err, exits := readMetadataReceipt(t, wire)
			if err != nil || r.stage != 3 || r.validateArchiveReceipt() != nil {
				t.Fatal("complete canonical metadata was rejected")
			}
			if r.archiveSize != size || r.digest != metadataDigestFirst+metadataDigestLast {
				t.Fatal("archive metadata did not preserve size and ordered digest halves")
			}
			if r.code != 23 || r.collectCode != 0 || exits != 1 || !r.exitedAt.Equal(time.Unix(123, 0)) {
				t.Fatal("collection changed the workload terminal receipt")
			}
			if visible != "setup\nworkload\n" || r.payload.String() != "private collector diagnostic" {
				t.Fatal("workload output and private collector diagnostics were not separated")
			}
		})
	}
}

func TestBlacksmithArtifactMetadataRejectsInvalidReceipt(t *testing.T) {
	size := "bytes:1"
	first := "digest0:" + metadataDigestFirst
	last := "digest1:" + metadataDigestLast
	complete := func(metadata string) string {
		return metadataFrames("start", "exit:23") + metadata + metadataFrames("end:0")
	}
	cases := []struct{ name, wire string }{
		{"missing-all", complete("")},
		{"missing-size", complete(metadataFrames(first, last))},
		{"missing-digest", complete(metadataFrames(size))},
		{"missing-first-half", complete(metadataFrames(size, last))},
		{"missing-last-half", complete(metadataFrames(size, first))},
		{"duplicate-size", complete(metadataFrames(size, size, first, last))},
		{"duplicate-first-half", complete(metadataFrames(size, first, first, last))},
		{"duplicate-last-half", complete(metadataFrames(size, first, last, last))},
		{"digest-before-size", complete(metadataFrames(first, size, last))},
		{"reversed-halves", complete(metadataFrames(size, last, first))},
		{"unknown-half", complete(metadataFrames(size, first, "digest2:"+metadataDigestLast))},
		{"size-before-exit", metadataFrames("start", size, "exit:23", first, last, "end:0")},
		{"digest-before-exit", metadataFrames("start", first, "exit:23", size, last, "end:0")},
		{"metadata-after-end", complete(metadataFrames(size, first, last)) + metadataFrames(size)},
		{"second-complete-receipt", complete(metadataFrames(size, first, last)) + complete(metadataFrames(size, first, last))},
		{"metadata-after-signal", metadataFrames("start", "exit:137", size, first, last, "end:0")},
		{"wrong-nonce", complete(strings.Replace(metadataFrames(size, first, last), metadataReceiptNonce, strings.Repeat("b", 64), 1))},
		{"short-nonce", complete(strings.Replace(metadataFrames(size), metadataReceiptNonce, metadataReceiptNonce[:63], 1))},
		{"long-nonce", complete(strings.Replace(metadataFrames(size), metadataReceiptNonce, metadataReceiptNonce+"a", 1))},
		{"truncated-digest-frame", metadataFrames("start", "exit:23", size) + strings.TrimSuffix(metadataFrames(first), "\x1f")},
		{"control-frame-over-cap", complete(metadataFrames("bytes:" + strings.Repeat("1", 129-len("CRABBOX_BS_"+metadataReceiptNonce+":bytes:"))))},
	}
	for _, number := range []struct{ name, value string }{
		{"empty", ""}, {"zero", "0"}, {"negative", "-1"}, {"plus", "+1"}, {"leading-zero", "01"},
		{"leading-space", " 1"}, {"trailing-space", "1 "}, {"newline", "1\n"}, {"fraction", "1.0"},
		{"exponent", "1e0"}, {"hex", "0x1"}, {"over-cap", "10485761"}, {"integer-overflow", "9223372036854775808"},
	} {
		cases = append(cases, struct{ name, wire string }{
			name: "noncanonical-size-" + number.name,
			wire: complete(metadataFrames("bytes:"+number.value, first, last)),
		})
	}
	for _, half := range []string{"digest0", "digest1"} {
		for _, malformed := range []struct{ name, value string }{
			{"empty", ""}, {"short", metadataDigestFirst[:31]}, {"long", metadataDigestFirst + "0"},
			{"uppercase", strings.ToUpper(metadataDigestFirst)}, {"nonhex", "g" + metadataDigestFirst[1:]},
		} {
			parts := []string{size, first, last}
			index := 1
			if half == "digest1" {
				index = 2
			}
			parts[index] = half + ":" + malformed.value
			cases = append(cases, struct{ name, wire string }{half + "-" + malformed.name, complete(metadataFrames(parts...))})
		}
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r, visible, err, _ := readMetadataReceipt(t, "ordinary\n"+tt.wire)
			if err == nil {
				err = r.validateArchiveReceipt()
			}
			if err == nil {
				t.Fatal("invalid metadata was accepted")
			}
			if visible != "ordinary\n" || strings.Contains(err.Error(), metadataReceiptNonce) ||
				strings.Contains(err.Error(), metadataDigestFirst) || strings.Contains(err.Error(), metadataDigestLast) {
				t.Fatal("invalid control metadata escaped to console or error output")
			}
		})
	}
}

func TestBlacksmithArtifactMetadataCollectorFailure(t *testing.T) {
	for _, tt := range []struct {
		name, metadata string
		collectorCode  int
	}{
		{"required-artifact", "", 8},
		{"failure-after-size", metadataFrames("bytes:1"), 9},
		{"timeout-after-digest", metadataFrames("bytes:1", "digest0:"+metadataDigestFirst, "digest1:"+metadataDigestLast), 124},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wire := metadataFrames("start", "exit:23") + "private collection explanation" + tt.metadata +
				metadataFrames(fmt.Sprintf("end:%d", tt.collectorCode))
			r, visible, parseErr, exits := readMetadataReceipt(t, wire)
			if parseErr != nil || r.stage != 3 || r.code != 23 || exits != 1 {
				t.Fatal("collector failure corrupted the workload receipt")
			}
			err := r.validateArchiveReceipt()
			var ee core.ExitError
			if !errors.As(err, &ee) || ee.Code != 7 || !strings.Contains(err.Error(), fmt.Sprintf("collection exited %d", tt.collectorCode)) {
				t.Fatal("collector failure was replaced by missing-metadata validation")
			}
			if tt.collectorCode == 8 && !strings.Contains(err.Error(), "missing required artifact") {
				t.Fatal("required-artifact failure lost its explanation")
			}
			if visible != "" || strings.Contains(err.Error(), "private collection explanation") || strings.Contains(err.Error(), metadataDigestFirst) {
				t.Fatal("collector failure leaked private diagnostics or metadata")
			}
		})
	}
}

func TestBlacksmithArtifactMetadataFrameEverySplit(t *testing.T) {
	values := []string{"bytes:1", "digest0:" + metadataDigestFirst, "digest1:" + metadataDigestLast}
	for frameIndex, value := range values {
		frame := metadataFrames(value)
		for split := 0; split <= len(frame); split++ {
			r, visible, err, exits := readMetadataReceipt(t,
				"ordinary\n"+metadataFrames("start", "exit:23")+metadataFrames(values[:frameIndex]...),
				frame[:split], frame[split:], metadataFrames(values[frameIndex+1:]...)+metadataFrames("end:0"))
			if err != nil || r.stage != 3 || r.validateArchiveReceipt() != nil || r.archiveSize != 1 ||
				r.digest != metadataDigestFirst+metadataDigestLast || r.code != 23 || exits != 1 || visible != "ordinary\n" {
				t.Fatalf("metadata frame %d split %d changed acceptance or exposed control bytes", frameIndex, split)
			}
		}
	}
}
