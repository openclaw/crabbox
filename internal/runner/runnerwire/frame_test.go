package runnerwire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func rawFrame(header string, body []byte) []byte {
	var data bytes.Buffer
	data.Write(magic[:])
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(header)))
	data.Write(size[:])
	data.WriteString(header)
	data.Write(body)
	return data.Bytes()
}

func TestFramePreservesBinaryPayloadAndBoundaries(t *testing.T) {
	data := []byte("\x00CBXR\n__CRABBOX_RESULT_FILE__:inert\n{\"version\":7}\xff")
	var stream bytes.Buffer
	if err := WriteFrame(&stream, Header{Kind: File, Size: uint64(len(data)), Meta: json.RawMessage(`{"path":"report\n.xml"}`)}, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&stream, Header{Kind: End}, nil); err != nil {
		t.Fatal(err)
	}
	reader := NewReader(&stream, 1024)
	frame, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reader.Next(); !errors.Is(err, ErrUnreadPayload) {
		t.Fatalf("unread error=%v", err)
	}
	got, err := io.ReadAll(frame.Body)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("payload changed: %q %v", got, err)
	}
	end, err := reader.Next()
	if err != nil || end.Header.Kind != End {
		t.Fatalf("end=%+v err=%v", end.Header, err)
	}
	if _, err = reader.Next(); err != io.EOF {
		t.Fatalf("terminal error=%v", err)
	}
}

func TestMalformedHeaderIsTerminal(t *testing.T) {
	for _, header := range []string{
		`{"version":2,"kind":"end","size":0}`,
		`{"version":1,"kind":"unknown","size":0}`,
		`{"version":1,"kind":"file","size":1025}`,
		`{"version":1,"kind":"file","size":-1}`,
		`{"version":1,"kind":"file","size":null}`,
		`{"version":1,"kind":"end","size":1}`,
		`{"version":1,"kind":"end"}`,
		`{"version":1,"kind":"end","size":0,"unknown":true}`,
		`{"version":1,"Version":1,"kind":"end","size":0}`,
		`{"version":2,"version":1,"kind":"end","size":0}`,
		`{"version":1,"kind":"end","size":0,"meta":{"path":"one","path":"two"}}`,
		`{"version":1,"kind":"end","size":0,"meta":[]}`,
		`{"version":1,"kind":"end","size":0} {}`,
		`null`,
	} {
		t.Run(header, func(t *testing.T) {
			data := append(rawFrame(header, nil), rawFrame(`{"version":1,"kind":"end","size":0}`, nil)...)
			reader := NewReader(bytes.NewReader(data), 1024)
			_, first := reader.Next()
			if first == nil {
				t.Fatal("malformed header accepted")
			}
			if _, second := reader.Next(); second != first {
				t.Fatalf("reader attempted recovery: %v / %v", first, second)
			}
		})
	}
}

func TestTruncatedBodyCannotAdvanceToAnotherFrame(t *testing.T) {
	reader := NewReader(bytes.NewReader(rawFrame(`{"version":1,"kind":"file","size":9}`, []byte("short"))), 1024)
	frame, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(frame.Body)
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("body error=%v", err)
	}
	if _, err = reader.Next(); err != io.ErrUnexpectedEOF {
		t.Fatalf("next error=%v", err)
	}
}

func TestTruncatedAndOversizedHeader(t *testing.T) {
	for _, data := range [][]byte{[]byte("CBX"), rawFrame(`{"version":1}`, nil)[:10], rawFrame(strings.Repeat("x", MaxHeaderBytes+1), nil)} {
		if _, err := NewReader(bytes.NewReader(data), 1024).Next(); err == nil {
			t.Fatal("invalid header accepted")
		}
	}
}

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

func TestWriterReportsIncompleteTransport(t *testing.T) {
	if err := WriteFrame(shortWriter{}, Header{Kind: End}, nil); err != io.ErrShortWrite {
		t.Fatalf("short writer=%v", err)
	}
	if err := WriteFrame(io.Discard, Header{Kind: File, Size: 10}, strings.NewReader("short")); err != io.ErrUnexpectedEOF {
		t.Fatalf("short source=%v", err)
	}
	if err := WriteFrame(io.Discard, Header{Kind: File, Size: 10}, nil); err == nil {
		t.Fatal("missing source accepted")
	}
}

func TestMetadataPreservesCaseSensitiveMapKeys(t *testing.T) {
	var stream bytes.Buffer
	err := WriteFrame(&stream, Header{Kind: Request, Meta: json.RawMessage(`{"env":{"FOO":"one","foo":"two"}}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewReader(&stream, 0).Next(); err != nil {
		t.Fatal(err)
	}
}

func FuzzFrameReader(f *testing.F) {
	f.Add(rawFrame(`{"version":1,"kind":"end","size":0}`, nil))
	f.Add(rawFrame(`{"version":1,"kind":"file","size":4}`, []byte("data")))
	f.Add([]byte("CBXR\x00\x10\x00\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		reader := NewReader(bytes.NewReader(data), 1024)
		for i := 0; i < 8; i++ {
			frame, err := reader.Next()
			if err != nil {
				return
			}
			body, err := io.ReadAll(frame.Body)
			if len(body) > 1024 {
				t.Fatal("payload limit bypassed")
			}
			if err != nil {
				return
			}
		}
	})
}
