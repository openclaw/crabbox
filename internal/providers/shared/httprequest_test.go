package shared

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"
)

func TestNewJSONRequestEnvelope(t *testing.T) {
	testJSONRequestEnvelope(t, NewJSONRequest, false)
}

func TestNewCompactJSONRequestEnvelope(t *testing.T) {
	testJSONRequestEnvelope(t, NewCompactJSONRequest, true)
}

func testJSONRequestEnvelope(t *testing.T, newRequest func(context.Context, string, string, any) (*http.Request, error), compact bool) {
	t.Helper()
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "request-context")
	var pointer *int
	var slice []string
	var mapping map[string]string
	for _, tc := range []struct {
		name        string
		body        any
		encoderWire string
		compactWire string
	}{
		{name: "nil interface"},
		{name: "typed nil pointer", body: pointer, encoderWire: "null\n", compactWire: "null"},
		{name: "typed nil slice", body: slice, encoderWire: "null\n", compactWire: "null"},
		{name: "typed nil map", body: mapping, encoderWire: "null\n", compactWire: "null"},
		{name: "encoded object", body: struct {
			Text  string `json:"text"`
			Count int    `json:"count"`
		}{Text: "<tag>&\n", Count: 7}, encoderWire: "{\"text\":\"\\u003ctag\\u003e\\u0026\\n\",\"count\":7}\n", compactWire: "{\"text\":\"\\u003ctag\\u003e\\u0026\\n\",\"count\":7}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := tc.encoderWire
			if compact {
				wire = tc.compactWire
			}
			const target = "https://example.invalid/resource?part=one"
			req, err := newRequest(ctx, http.MethodPost, target, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			if req.Context() != ctx || req.Method != http.MethodPost || req.URL.String() != target {
				t.Fatalf("request context/method/URL changed: %v", req)
			}
			if len(req.Header) != 0 {
				t.Fatalf("constructor added headers: %v", req.Header)
			}
			if req.ContentLength != int64(len(wire)) {
				t.Fatalf("ContentLength=%d want %d", req.ContentLength, len(wire))
			}
			if tc.body == nil {
				if req.Body != nil || req.GetBody != nil {
					t.Fatal("nil interface gained a body or replay closure")
				}
				return
			}
			if req.Body == nil || req.GetBody == nil {
				t.Fatal("encoded body or replay closure missing")
			}
			data, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := req.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if string(data) != wire {
				t.Fatalf("body=%q want %q", data, wire)
			}
			for range 2 {
				replay, err := req.GetBody()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(replay)
				if err != nil {
					t.Fatal(err)
				}
				if err := replay.Close(); err != nil {
					t.Fatal(err)
				}
				if string(data) != wire {
					t.Fatalf("replay=%q want %q", data, wire)
				}
			}
		})
	}
}

func TestNewJSONRequestEncodingErrorPrecedesConstruction(t *testing.T) {
	testJSONRequestEncodingErrorPrecedesConstruction(t, NewJSONRequest)
}

func TestNewCompactJSONRequestEncodingErrorPrecedesConstruction(t *testing.T) {
	testJSONRequestEncodingErrorPrecedesConstruction(t, NewCompactJSONRequest)
}

func testJSONRequestEncodingErrorPrecedesConstruction(t *testing.T, newRequest func(context.Context, string, string, any) (*http.Request, error)) {
	t.Helper()
	body := make(chan int)
	for _, tc := range []struct {
		name        string
		ctx         context.Context
		method, url string
	}{
		{name: "ordinary request", ctx: context.Background(), method: http.MethodPost, url: "https://example.invalid"},
		{name: "invalid method", ctx: context.Background(), method: "invalid method", url: "https://example.invalid"},
		{name: "invalid URL", ctx: context.Background(), method: http.MethodPost, url: "http://[::1"},
		{name: "nil context", method: http.MethodPost, url: "https://example.invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := newRequest(tc.ctx, tc.method, tc.url, body)
			encodingErr, ok := err.(*json.UnsupportedTypeError)
			if req != nil || !ok || encodingErr.Type != reflect.TypeOf(body) {
				t.Fatalf("got request=%v error=%T %v", req, err, err)
			}
		})
	}
}

func TestNewJSONRequestConstructionErrorsAndCanceledContext(t *testing.T) {
	testJSONRequestConstructionErrorsAndCanceledContext(t, NewJSONRequest)
}

func TestNewCompactJSONRequestConstructionErrorsAndCanceledContext(t *testing.T) {
	testJSONRequestConstructionErrorsAndCanceledContext(t, NewCompactJSONRequest)
}

func testJSONRequestConstructionErrorsAndCanceledContext(t *testing.T, newRequest func(context.Context, string, string, any) (*http.Request, error)) {
	t.Helper()
	for _, tc := range []struct {
		ctx         context.Context
		method, url string
	}{
		{context.Background(), "invalid method", "https://example.invalid"},
		{context.Background(), http.MethodGet, "http://[::1"},
		{nil, http.MethodGet, "https://example.invalid"},
	} {
		_, want := http.NewRequestWithContext(tc.ctx, tc.method, tc.url, nil)
		req, err := newRequest(tc.ctx, tc.method, tc.url, nil)
		if req != nil || err == nil || want == nil || err.Error() != want.Error() || reflect.TypeOf(err) != reflect.TypeOf(want) {
			t.Fatalf("request=%v error=%v want=%v", req, err, want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := newRequest(ctx, "", "https://example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Context() != ctx || req.Context().Err() != context.Canceled || req.Method != http.MethodGet {
		t.Fatal("constructor changed canceled-context or empty-method behavior")
	}
}
