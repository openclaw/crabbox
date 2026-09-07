package shared

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestEncodeConnectJSONEnvelope(t *testing.T) {
	got, err := encodeConnectJSONEnvelope(map[string]string{"command": "ok"})
	want := append([]byte{0, 0, 0, 0, 16}, []byte(`{"command":"ok"}`)...)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("frame=%q err=%v, want %q", got, err, want)
	}
	_, err = encodeConnectJSONEnvelope(make(chan int))
	var unsupported *json.UnsupportedTypeError
	if !errors.As(err, &unsupported) {
		t.Fatalf("marshal error=%v", err)
	}
}

func TestEnvdProcessStreamDelivery(t *testing.T) {
	for _, code := range []int{0, 7, -1, 137} {
		for _, chunked := range []bool{false, true} {
			t.Run(fmt.Sprintf("code=%d/chunked=%t", code, chunked), func(t *testing.T) {
				body := bytes.Join([][]byte{
					envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"start": map[string]any{"pid": 42}}}),
					envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"data": map[string]any{"stdout": "aGU=", "stderr": "d2E="}}}),
					envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"data": map[string]any{"stdout": "bGxv"}}}),
					envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"data": map[string]any{"stderr": "cm4="}}}),
					envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": code, "exited": true}}}),
					envdTestEnvelope(t, 2, map[string]any{}),
				}, nil)
				var reader io.Reader = bytes.NewReader(body)
				if chunked {
					reader = iotest.OneByteReader(reader)
				}
				var stdout, stderr bytes.Buffer
				endCalls := 0
				got, err := ParseEnvdProcessStream("fixture", reader, &stdout, &stderr, func(end EnvdProcessEnd, _ io.Writer, _ ...string) (int, error) {
					endCalls++
					if end != (EnvdProcessEnd{ExitCode: code, Exited: true}) {
						t.Fatalf("end=%+v", end)
					}
					return end.ExitCode, nil
				})
				if got != code || err != nil || endCalls != 1 || stdout.String() != "hello" || stderr.String() != "warn" {
					t.Fatalf("code=%d err=%v ends=%d stdout=%q stderr=%q", got, err, endCalls, stdout.String(), stderr.String())
				}
			})
		}
	}
}

func TestEnvdProcessStreamNormalEndAndLaterFailure(t *testing.T) {
	readFailure := errors.New("fixture read failed")
	for _, code := range []int{0, 7, -1, 137} {
		for _, ending := range []string{"EOF", "RPC failure", "read failure", "truncated"} {
			t.Run(fmt.Sprintf("code=%d/%s", code, ending), func(t *testing.T) {
				body := envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": code, "exited": true}}})
				if ending == "RPC failure" {
					body = append(body, envdTestEnvelope(t, 2, map[string]any{"error": map[string]any{"code": "internal", "message": "fixture RPC failure"}})...)
				}
				if ending == "truncated" {
					body = append(body, 0)
				}
				var reader io.Reader = bytes.NewReader(body)
				if ending == "read failure" {
					reader = io.MultiReader(reader, iotest.ErrReader(readFailure))
				}
				got, err := ParseEnvdProcessStream("fixture", reader, io.Discard, io.Discard, envdTestEndCode)
				if ending == "EOF" {
					if got != code || err != nil {
						t.Fatalf("clean EOF code=%d err=%v", got, err)
					}
					return
				}
				if got != 1 || err == nil {
					t.Fatalf("later failure lost precedence: code=%d err=%v", got, err)
				}
				if ending == "RPC failure" && !strings.Contains(err.Error(), "fixture RPC failure") {
					t.Fatalf("RPC error=%v", err)
				}
				if ending == "read failure" && !errors.Is(err, readFailure) {
					t.Fatalf("read error=%v", err)
				}
				if ending == "truncated" && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("truncated frame error=%v", err)
				}
			})
		}
	}
}

func TestEnvdProcessStreamWriterFailure(t *testing.T) {
	writeFailure := errors.New("fixture output write failed")
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			body := append(envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"data": map[string]any{stream: "b3V0cHV0"}}}), envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"end": map[string]any{"exited": true}}})...)
			var stdout, stderr io.Writer = io.Discard, io.Discard
			if stream == "stdout" {
				stdout = envdWireFailureWriter{writeFailure}
			} else {
				stderr = envdWireFailureWriter{writeFailure}
			}
			endCalled := false
			code, err := ParseEnvdProcessStream("fixture", bytes.NewReader(body), stdout, stderr, func(EnvdProcessEnd, io.Writer, ...string) (int, error) { endCalled = true; return 0, nil })
			if code != 1 || !errors.Is(err, writeFailure) || endCalled {
				t.Fatalf("code=%d err=%v end=%t", code, err, endCalled)
			}
		})
	}
}

func TestEnvdProcessStreamRequiresProcessEnd(t *testing.T) {
	for _, trailer := range []bool{false, true} {
		t.Run(fmt.Sprintf("RPC trailer=%t", trailer), func(t *testing.T) {
			body := envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"data": map[string]any{"stdout": "cGFydGlhbA=="}}})
			if trailer {
				body = append(body, envdTestEnvelope(t, 2, map[string]any{})...)
			}
			var stdout bytes.Buffer
			code, err := ParseEnvdProcessStream("fixture", bytes.NewReader(body), &stdout, io.Discard, envdTestEndCode)
			if code != 1 || err == nil || err.Error() != "fixture process stream ended without end event" || stdout.String() != "partial" {
				t.Fatalf("code=%d err=%v stdout=%q", code, err, stdout.String())
			}
		})
	}
}

func TestEnvdProcessStreamRedactsRPCError(t *testing.T) {
	const secret = "envd-stream-secret"
	body := envdTestEnvelope(t, 2, map[string]any{"error": map[string]any{"code": "unauthorized", "message": "Bearer " + secret + " quota exceeded"}})
	code, err := ParseEnvdProcessStream("fixture", bytes.NewReader(body), io.Discard, io.Discard, envdTestEndCode, secret)
	if code != 1 || err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func envdTestEnvelope(t *testing.T, flags byte, value any) []byte {
	t.Helper()
	frame, err := encodeConnectJSONEnvelope(value)
	if err != nil {
		t.Fatal(err)
	}
	frame[0] = flags
	return frame
}

func envdTestEndCode(end EnvdProcessEnd, _ io.Writer, _ ...string) (int, error) {
	return end.ExitCode, nil
}

type envdWireFailureWriter struct{ err error }

func (w envdWireFailureWriter) Write([]byte) (int, error) { return 0, w.err }
