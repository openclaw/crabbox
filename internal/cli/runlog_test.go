package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRunLogBufferKeepsLatestTail(t *testing.T) {
	var buffer runLogBuffer
	prefix := []byte("prefix")
	tail := bytes.Repeat([]byte("x"), maxRunLogBytes)
	input := append(prefix, tail...)
	if n, err := buffer.Write(input); err != nil || n != len(input) {
		t.Fatalf("Write returned %d, %v", n, err)
	}
	if got := buffer.String(); got != string(tail) {
		t.Fatalf("tail length=%d match=%v", len(got), got == string(tail))
	}
	if !buffer.Snapshot().Truncated {
		t.Fatal("buffer should be truncated")
	}
}

func TestRunLogBufferDropsOverflow(t *testing.T) {
	var buffer runLogBuffer
	first := bytes.Repeat([]byte("a"), maxRunLogBytes-2)
	second := []byte("bcde")
	if _, err := buffer.Write(first); err != nil {
		t.Fatal(err)
	}
	if buffer.Snapshot().Truncated {
		t.Fatal("buffer should not be truncated before overflow")
	}
	if _, err := buffer.Write(second); err != nil {
		t.Fatal(err)
	}
	want := string(append(first[2:], second...))
	if got := buffer.String(); got != want {
		t.Fatalf("tail length=%d want=%d", len(got), len(want))
	}
	if !buffer.Snapshot().Truncated {
		t.Fatal("buffer should be truncated after overflow")
	}
}

func TestRunLogBufferConcurrentWrites(t *testing.T) {
	var buffer runLogBuffer
	var wg sync.WaitGroup
	for _, text := range []string{"stdout-line\n", "stderr-line\n"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if _, err := buffer.Write([]byte(text)); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	log := buffer.String()
	if !strings.Contains(log, "stdout-line\n") || !strings.Contains(log, "stderr-line\n") {
		t.Fatalf("log missing expected output: %q", log)
	}
}

func TestSplitRunLogChunks(t *testing.T) {
	log := strings.Repeat("a", coordinatorRunLogChunkBytes) + "tail"
	chunks := splitRunLogChunks(log)
	if len(chunks) != 2 {
		t.Fatalf("chunks=%d, want 2", len(chunks))
	}
	if len(chunks[0]) != coordinatorRunLogChunkBytes {
		t.Fatalf("first chunk length=%d, want %d", len(chunks[0]), coordinatorRunLogChunkBytes)
	}
	if got := strings.Join(chunks, ""); got != log {
		t.Fatalf("joined chunks length=%d, want %d", len(got), len(log))
	}
}

func TestRunLogFallbackPreviewKeepsTail(t *testing.T) {
	log := strings.Repeat("a", runLogFallbackPreviewBytes) + "tail"
	preview := runLogFallbackPreview(log)
	if len(preview) != runLogFallbackPreviewBytes {
		t.Fatalf("preview length=%d, want %d", len(preview), runLogFallbackPreviewBytes)
	}
	if !strings.HasSuffix(preview, "tail") {
		t.Fatalf("preview does not keep tail: suffix=%q", preview[len(preview)-8:])
	}
}

func TestRunLogFallbackPreviewKeepsShortLogs(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		if got := runLogFallbackPreview("short log"); got != "short log" {
			t.Fatalf("short preview truncated=%t got=%q", truncated, got)
		}
	}
	if got := runLogFallbackPreview(""); got != "" {
		t.Fatalf("empty preview=%q", got)
	}
}

func TestRunLogBufferDigestSerializesConcurrentStreams(t *testing.T) {
	var writer runLogBuffer
	stdout := io.MultiWriter(io.Discard, &writer)
	stderr := io.MultiWriter(io.Discard, &writer)
	chunk := bytes.Repeat([]byte("a"), 1024)
	rounds := 64
	var wg sync.WaitGroup
	for _, stream := range []io.Writer{stdout, stderr} {
		wg.Add(1)
		go func(stream io.Writer) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if _, err := stream.Write(chunk); err != nil {
					t.Error(err)
				}
			}
		}(stream)
	}
	wg.Wait()
	expected := sha256.Sum256(bytes.Repeat([]byte("a"), 2*rounds*1024))
	if got := writer.Snapshot().FullSHA256; got != "sha256:"+hex.EncodeToString(expected[:]) {
		t.Fatalf("unexpected digest %s", got)
	}
}

func TestRunLogBufferDigestHashesMixedStreamsInArrivalOrder(t *testing.T) {
	var writer runLogBuffer
	stdout := io.MultiWriter(io.Discard, &writer)
	stderr := io.MultiWriter(io.Discard, &writer)
	if _, err := stdout.Write([]byte("out line\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.Write([]byte("err line\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := stdout.Write([]byte("done\n")); err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256([]byte("out line\nerr line\ndone\n"))
	if got := writer.Snapshot().FullSHA256; got != "sha256:"+hex.EncodeToString(expected[:]) {
		t.Fatalf("unexpected digest %s", got)
	}
}

func TestRetainedRunLogText(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		cap               int
		changed           bool
	}{
		{"empty", "", "", 8, false},
		{"unicode", "é€😀", "é€😀", 9, false},
		{"replacement is valid", "�", "�", 3, false},
		{"invalid bytes", "\xff\xfea", "��a", 7, true},
		{"overlong", "\xc0\xaf", "��", 6, true},
		{"surrogate", "\xed\xa0\x80", "���", 9, true},
		{"unfinished", "\xf0\x9f", "��", 6, true},
		{"replacement expansion", "a\xff\xfe", "�", 4, true},
		{"two byte cut", "éabc", "abc", 4, true},
		{"three byte cut", "€ab", "ab", 4, true},
		{"four byte cut", "😀a", "a", 4, true},
		{"bom preserved", "x\ufeff", "\ufeff", 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := retainedRunLogText(tc.input, tc.cap)
			if got != tc.want || changed != tc.changed || !utf8.ValidString(got) || len(got) > tc.cap {
				t.Fatalf("text=%q changed=%t; want %q changed=%t", got, changed, tc.want, tc.changed)
			}
		})
	}
}

func TestRunLogBufferUnicodeBoundaries(t *testing.T) {
	for _, char := range []string{"é", "€", "😀"} {
		for cut := 0; cut <= len(char); cut++ {
			raw := char + strings.Repeat("a", maxRunLogBytes-len(char)+cut)
			want := raw
			if cut > 0 {
				want = raw[len(char):]
			}
			for _, split := range []int{0, 1, len(char) - 1, len(char), len(raw) - 1} {
				var b runLogBuffer
				_, _ = b.Write([]byte(raw[:split]))
				_ = b.Snapshot() // Reads must not make an unfinished rune permanently lossy.
				_, _ = b.Write([]byte(raw[split:]))
				got := b.Snapshot()
				if got.Log != want || got.Truncated != (cut > 0) || got.FullSHA256 != sha256Digest([]byte(raw)) {
					t.Fatalf("char=%q cut=%d split=%d: bytes=%d truncated=%t", char, cut, split, len(got.Log), got.Truncated)
				}
			}
		}
	}
}

func TestRunLogBufferPendingRuneSnapshots(t *testing.T) {
	for _, char := range []string{"é", "€", "😀"} {
		var b runLogBuffer
		for i := range len(char) {
			_, _ = b.Write([]byte{char[i]})
			got := b.Snapshot()
			if got.Truncated != (i+1 < len(char)) || got.FullSHA256 != sha256Digest([]byte(char[:i+1])) {
				t.Fatalf("char=%q written=%d snapshot=%+v", char, i+1, got)
			}
		}
		if b.String() != char {
			t.Fatalf("completed rune=%q", b.String())
		}
	}
}

func TestRunLogBufferReplacementExpansion(t *testing.T) {
	raw := bytes.Repeat([]byte{0xff}, maxRunLogBytes/3+1)
	var b runLogBuffer
	for _, part := range [][]byte{raw[:1], raw[1:]} {
		_, _ = b.Write(part)
	}
	got := b.Snapshot()
	want := strings.Repeat("�", maxRunLogBytes/3)
	if got.Log != want || !got.Truncated || got.FullSHA256 != sha256Digest(raw) {
		t.Fatalf("bytes=%d truncated=%t rawHash=%s", len(got.Log), got.Truncated, got.FullSHA256)
	}
}

func TestRunLogBufferExactCapAndOlderBytes(t *testing.T) {
	for _, older := range []string{"", "old"} {
		for _, split := range []int{0, maxRunLogBytes / 2, maxRunLogBytes} {
			var b runLogBuffer
			_, _ = b.Write([]byte(older))
			raw := bytes.Repeat([]byte("a"), maxRunLogBytes)
			_, _ = b.Write(raw[:split])
			_, _ = b.Write(raw[split:])
			got := b.Snapshot()
			if got.Log != string(raw) || got.Truncated != (older != "") {
				t.Fatalf("older=%q split=%d: bytes=%d truncated=%t", older, split, len(got.Log), got.Truncated)
			}
		}
	}
}

func TestRunLogBufferSnapshotConcurrentDigest(t *testing.T) {
	var b runLogBuffer
	var wg sync.WaitGroup
	for _, text := range []string{"é\n", "€\n", "😀\n"} {
		wg.Go(func() {
			for range 100 {
				_, _ = b.Write([]byte(text))
				got := b.Snapshot()
				if got.Truncated || got.FullSHA256 != sha256Digest([]byte(got.Log)) {
					t.Errorf("incoherent snapshot: bytes=%d truncated=%t", len(got.Log), got.Truncated)
				}
			}
		})
	}
	wg.Wait()
}

func TestRunLogBufferCapturedBytesStayRaw(t *testing.T) {
	var b runLogBuffer
	capture := capturedRunLogWriter{&b}
	_, _ = capture.Write(nil)
	if b.Snapshot().Truncated {
		t.Fatal("empty capture is complete")
	}
	_, _ = b.Write([]byte("out"))
	_, _ = capture.Write([]byte{0xff, 0xfe})
	_, _ = b.Write([]byte("err"))
	got := b.Snapshot()
	if got.Log != "outerr" || !got.Truncated || got.FullSHA256 != sha256Digest([]byte("out\xff\xfeerr")) {
		t.Fatalf("snapshot=%+v", got)
	}
}

func TestRunLogChunksAndPreviewUnicode(t *testing.T) {
	for _, char := range []string{"é", "€", "😀", "\xff"} {
		for cut := 1; cut <= 3; cut++ {
			log := strings.Repeat("a", coordinatorRunLogChunkBytes-cut) + char + strings.Repeat("b", runLogFallbackPreviewBytes-1)
			want, _ := retainedRunLogText(log, maxRunLogBytes)
			chunks := splitRunLogChunks(log)
			if strings.Join(chunks, "") != want {
				t.Fatal("chunks changed log")
			}
			for _, chunk := range chunks {
				if !utf8.ValidString(chunk) || len(chunk) > coordinatorRunLogChunkBytes {
					t.Fatal("invalid chunk")
				}
			}
			preview := runLogFallbackPreview(log)
			if !utf8.ValidString(preview) || len(preview) > runLogFallbackPreviewBytes || !strings.HasSuffix(want, preview) {
				t.Fatalf("invalid preview for %q at %d", char, cut)
			}
		}
	}
}

// Text runs keep the cross-language FinishRun JSON golden small; no large log
// fixture is checked in. Both sides expand exactly these captured wire strings.
type logTextRun struct {
	Text  string `json:"text"`
	Count int    `json:"count"`
}

type terminalLogWireFixture struct {
	Name string `json:"name"`
	Raw  []struct {
		Hex   string `json:"hex"`
		Count int    `json:"count"`
	} `json:"raw"`
	Finish map[string]any `json:"finish"`
}

func compactLogText(log string) []logTextRun {
	runs := []logTextRun{}
	for _, r := range log {
		text := string(r)
		if len(runs) > 0 && runs[len(runs)-1].Text == text {
			runs[len(runs)-1].Count++
		} else {
			runs = append(runs, logTextRun{Text: text, Count: 1})
		}
	}
	return runs
}

func captureTerminalLogWire(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var buffer runLogBuffer
	// Include a read between writes, even when the first byte starts a rune.
	first := min(1, len(raw))
	_, _ = buffer.Write(raw[:first])
	_ = buffer.Snapshot()
	_, _ = buffer.Write(raw[first:])
	retained := buffer.Snapshot()
	started := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	receipt, err := buildTerminalRunReceiptWithKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), terminalRunReceiptInput{
		Provider: "aws", RunID: "run_unicode", Command: []string{"synthetic-output"}, CommandDisplay: "synthetic-output",
		ExitCode: 0, CommandMs: 1000, StartedAt: started, EndedAt: started.Add(time.Second),
		LogSHA256: retained.FullSHA256, RetainedLogSHA256: sha256Digest([]byte(retained.Log)), LogTruncated: retained.Truncated,
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.LogSHA256 != sha256Digest(raw) {
		t.Fatal("full hash is not raw")
	}
	var wire map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs/run_unicode/finish" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			t.Error(err)
		}
		_, _ = io.WriteString(w, `{"run":{"id":"run_unicode"}}`)
	}))
	defer server.Close()
	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	if _, err := client.FinishRun(t.Context(), "run_unicode", 0, 0, time.Second, retained.Log, retained.Truncated, nil, nil, FailureClassification{}, &receipt); err != nil {
		t.Fatal(err)
	}
	var joined strings.Builder
	chunks := wire["logChunks"].([]any)
	type chunkRun struct {
		Parts []logTextRun `json:"parts"`
		Count int          `json:"count"`
	}
	chunkRuns := []chunkRun{}
	for _, value := range chunks {
		chunk := value.(string)
		if len(chunk) > coordinatorRunLogChunkBytes || !utf8.ValidString(chunk) {
			t.Fatal("invalid wire chunk")
		}
		joined.WriteString(chunk)
		parts := compactLogText(chunk)
		if len(chunkRuns) > 0 && reflect.DeepEqual(chunkRuns[len(chunkRuns)-1].Parts, parts) {
			chunkRuns[len(chunkRuns)-1].Count++
		} else {
			chunkRuns = append(chunkRuns, chunkRun{Parts: parts, Count: 1})
		}
	}
	if joined.String() != retained.Log || sha256Digest([]byte(joined.String())) != receipt.RetainedLogSHA256 {
		t.Fatal("wire changed signed text")
	}
	preview := wire["log"].(string)
	if len(preview) > runLogFallbackPreviewBytes || !utf8.ValidString(preview) || !strings.HasSuffix(retained.Log, preview) {
		t.Fatal("invalid wire preview")
	}
	wire["log"] = compactLogText(preview)
	wire["logChunks"] = chunkRuns
	return wire
}

func TestTerminalLogFinishRunWireGolden(t *testing.T) {
	data, err := os.ReadFile("testdata/terminal-log-wire.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []terminalLogWireFixture
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			var raw []byte
			for _, part := range fixture.Raw {
				b, err := hex.DecodeString(part.Hex)
				if err != nil {
					t.Fatal(err)
				}
				raw = append(raw, bytes.Repeat(b, part.Count)...)
			}
			actual := captureTerminalLogWire(t, raw)
			encoded, err := json.Marshal(actual)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, fixture.Finish) {
				t.Fatalf("FinishRun JSON differs from golden for %s", fixture.Name)
			}
		})
	}
}
