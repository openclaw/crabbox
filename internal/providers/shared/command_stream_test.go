package shared

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

func commandStreamResponse(contentType, body string) *http.Response {
	return &http.Response{Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestCommandStreamProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, body, stdout, stderr, errorText string
		code                                  int
	}{
		{name: "output and completion", body: "\n{\"type\":\"start\"}\n{\"type\":\"stdout\",\"data\":\"one\"}\n{\"type\":\"heartbeat\"}\n{\"type\":\"stderr\",\"data\":\"two\"}\n{\"type\":\"complete\",\"exitCode\":23}\n", stdout: "one", stderr: "two", code: 23},
		{name: "default completion", body: `{"type":"complete"}`},
		{name: "null completion code", body: `{"type":"complete","exitCode":null}`},
		{name: "signed completion", body: `{"type":"complete","exitCode":-1}`, code: -1},
		{name: "first terminal wins", body: "{\"type\":\"complete\",\"exitCode\":7}\nnot json", code: 7},
		{name: "missing completion", body: `{"type":"stdout","data":"partial"}`, stdout: "partial", errorText: "fixture stream ended before completion"},
		{name: "malformed event", body: `{"type":`, errorText: "decode fixture stream event:"},
		{name: "unknown event", body: `{"type":"surprise"}`, errorText: `unknown fixture stream event "surprise"`},
		{name: "redacted error", body: `{"type":"error","error":"synthetic-secret failed"}`, errorText: "[redacted] failed"},
		{name: "default error", body: `{"type":"error"}`, errorText: "stream error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			stream := CommandStream{Provider: "fixture", RedactError: func(s string) string { return strings.ReplaceAll(s, "synthetic-secret", "[redacted]") }}
			code, err := stream.Read(t.Context(), commandStreamResponse("application/x-ndjson", tc.body), &stdout, &stderr)
			if code != tc.code || stdout.String() != tc.stdout || stderr.String() != tc.stderr {
				t.Fatalf("code=%d stdout=%q stderr=%q error=%v", code, stdout.String(), stderr.String(), err)
			}
			if tc.errorText == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.errorText) {
				t.Fatalf("error=%v want=%q", err, tc.errorText)
			}
		})
	}
}

func TestCommandStreamAdmissionAndBounds(t *testing.T) {
	stream := CommandStream{Provider: "fixture"}
	for _, contentType := range []string{"", "application/x-ndjson", "application/jsonl; charset=utf-8"} {
		if _, err := stream.Read(t.Context(), commandStreamResponse(contentType, `{"type":"complete"}`), nil, nil); err != nil {
			t.Fatalf("%q: %v", contentType, err)
		}
	}
	if _, err := stream.Read(t.Context(), commandStreamResponse("text/html", `{"type":"complete"}`), nil, nil); err == nil || !strings.Contains(err.Error(), "unexpected fixture stream content-type") {
		t.Fatalf("content type admitted: %v", err)
	}
	if _, err := stream.Read(t.Context(), commandStreamResponse("", strings.Repeat("x", 4*1024*1024)), nil, nil); !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("unbounded event: %v", err)
	}
	large := `{"type":"stdout","data":"` + strings.Repeat("x", 128*1024) + "\"}\n{\"type\":\"complete\"}"
	if _, err := stream.Read(t.Context(), commandStreamResponse("", large), nil, nil); err != nil {
		t.Fatalf("valid large event rejected: %v", err)
	}
}

func TestCommandStreamCancellationAndReadErrorPrecedence(t *testing.T) {
	cause := errors.New("caller canceled")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(cause)
	stream := CommandStream{Provider: "fixture"}
	if code, err := stream.Read(ctx, commandStreamResponse("", `{"type":"complete","exitCode":19}`), nil, nil); code != 19 || err != nil {
		t.Fatalf("completion lost: code=%d err=%v", code, err)
	}
	if _, err := stream.Read(ctx, commandStreamResponse("", ""), nil, nil); err != context.Canceled {
		t.Fatalf("EOF cancellation changed: %v", err)
	}
	readErr := errors.New("read failed")
	resp := commandStreamResponse("", "")
	resp.Body = io.NopCloser(iotest.ErrReader(readErr))
	if _, err := stream.Read(ctx, resp, nil, nil); err != readErr {
		t.Fatalf("read error replaced: %v", err)
	}
}

type commandStreamFailedWriter struct{}

func (commandStreamFailedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCommandStreamWriterFailures(t *testing.T) {
	for _, channel := range []string{"stdout", "stderr"} {
		body := "{\"type\":\"" + channel + "\",\"data\":\"one\"}\n{\"type\":\"complete\"}"
		_, err := (CommandStream{Provider: "fixture"}).Read(t.Context(), commandStreamResponse("", body), commandStreamFailedWriter{}, commandStreamFailedWriter{})
		if !errors.Is(err, io.ErrClosedPipe) || !strings.Contains(err.Error(), "write fixture "+channel) {
			t.Fatalf("%s: %v", channel, err)
		}
	}
}
