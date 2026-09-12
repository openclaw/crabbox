package testutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type RoundTripFunc func(*http.Request) (*http.Response, error)

func (f RoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// RequireRequestEnvelope checks exact bytes and replay; an empty body expects nil Body and GetBody.
func RequireRequestEnvelope(t *testing.T, req *http.Request, ctx context.Context, method, endpoint, body string, headers http.Header) {
	t.Helper()
	if req.Context() != ctx || req.Method != method || req.URL.String() != endpoint {
		t.Fatalf("request=%s %s context=%v", req.Method, req.URL, req.Context() == ctx)
	}
	if !reflect.DeepEqual(req.Header, headers) {
		t.Fatalf("headers=%v want %v", req.Header, headers)
	}
	if req.ContentLength != int64(len(body)) || (req.Body == nil) != (body == "") {
		t.Fatalf("bodymetadata=%d/%v want%d", req.ContentLength, req.Body == nil, len(body))
	}
	if body == "" {
		if req.GetBody != nil {
			t.Fatal("nil body has replay")
		}
		return
	}
	data, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != body {
		t.Fatalf("body=%q want%q", data, body)
	}
	if req.GetBody == nil {
		t.Fatal("missing replay")
	}
	replay, err := req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	data, err = io.ReadAll(replay)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != body {
		t.Fatalf("replay=%q want%q", data, body)
	}
}

func RequireRedactedProviderError(t *testing.T, err error, secret string) {
	t.Helper()
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("error=%v, want redacted useful provider error", err)
	}
}

func GRPCWebEnvelope(flags byte, value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var out bytes.Buffer
	out.WriteByte(flags)
	out.Write([]byte{byte(len(data) >> 24), byte(len(data) >> 16), byte(len(data) >> 8), byte(len(data))})
	out.Write(data)
	return out.Bytes()
}

func TarGzipContains(t *testing.T, data []byte, name string) bool {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == name {
			return true
		}
	}
}
