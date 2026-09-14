package shared

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeUnboundedJSONResponse(t *testing.T) {
	readErr := errors.New("ordinary read failure")
	apiErr := errors.New("original adapter error")
	for _, tc := range []struct {
		name, body string
		code       int
		nilOutput  bool
		readErr    error
	}{
		{name: "json", body: `{"value":"new","extra":true}`},
		{name: "raw empty"},
		{name: "ASCII whitespace", body: " \t\r\n"},
		{name: "Unicode whitespace", body: "\u2003"},
		{name: "null", body: "null"},
		{name: "nil output empty", nilOutput: true},
		{name: "nil output consumes non JSON", body: strings.Repeat("ordinary text ", 400), nilOutput: true},
		{name: "last success status", code: 299, body: `{"value":"new"}`},
		{name: "malformed", body: "{"},
		{name: "trailing JSON", body: "{} {}"},
		{name: "wrong field type", body: `{"value":7}`},
		{name: "read error before status", code: 400, body: "partial ordinary text", readErr: readErr},
		{name: "read error with nil output", body: "ordinary text", nilOutput: true, readErr: readErr},
		{name: "below success", code: 199, body: " raw status body \n"},
		{name: "above success", code: 300, body: "\u2003raw status body\n", nilOutput: true},
		{name: "empty failure", code: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.code == 0 {
				tc.code = http.StatusOK
			}
			body := &responseBodyProbe{Reader: strings.NewReader(tc.body), readErr: tc.readErr}
			resp := &http.Response{StatusCode: tc.code, Status: "original status text", Body: body}
			value := struct{ Value string }{Value: "old"}
			wantValue := value
			var out any = &value
			if tc.nilOutput {
				out = nil
			}
			var wantErr error
			wantCalls := 0
			switch {
			case tc.readErr != nil:
				wantErr = tc.readErr
			case tc.code < 200 || tc.code >= 300:
				wantErr, wantCalls = apiErr, 1
			case !tc.nilOutput && len(tc.body) != 0:
				wantErr = json.Unmarshal([]byte(tc.body), &wantValue)
			}
			calls := 0
			err := DecodeUnboundedJSONResponse(resp, out, func(code int, status string, data []byte) error {
				calls++
				if code != tc.code || status != resp.Status || !bytes.Equal(data, []byte(tc.body)) {
					t.Fatalf("factory arguments changed: code=%d status=%q data=%q", code, status, data)
				}
				if body.closed != 0 {
					t.Fatal("borrowed body closed before adapter factory")
				}
				return apiErr
			})
			if tc.readErr != nil || wantCalls != 0 {
				if err != wantErr {
					t.Fatalf("original error not returned directly: got %T %v, want %v", err, err, wantErr)
				}
			} else if reflect.TypeOf(err) != reflect.TypeOf(wantErr) || !reflect.DeepEqual(err, wantErr) {
				t.Fatalf("unmarshal result changed or wrapped: got %T %v, want %T %v", err, err, wantErr, wantErr)
			}
			if value != wantValue || calls != wantCalls {
				t.Fatalf("value=%#v calls=%d, want %#v calls=%d", value, calls, wantValue, wantCalls)
			}
			if body.read != len(tc.body) || body.closed != 0 {
				t.Fatalf("borrowed body read=%d close=%d, want read=%d close=0", body.read, body.closed, len(tc.body))
			}
			_ = body.Close()
			if body.closed != 1 {
				t.Fatal("caller did not retain body-close ownership")
			}
		})
	}
}

type responseBodyProbe struct {
	io.Reader
	readErr error
	read    int
	closed  int
}

func (b *responseBodyProbe) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	if b.readErr != nil {
		return n, b.readErr
	}
	return n, err
}

func (b *responseBodyProbe) Close() error {
	b.closed++
	return errors.New("close is intentionally ignored")
}

func TestDecodeBoundedJSONResponse(t *testing.T) {
	readErr := errors.New("read failed")
	apiErr := errors.New("typed adapter error")
	for _, tc := range []struct {
		name, body, wantError string
		code                  int
		limit                 int64
		nilOutput             bool
		readErr               error
		want                  string
		wantAPI               bool
		wantSyntax            bool
	}{
		{name: "json", body: `{"value":"new","extra":true}`, want: "new"},
		{name: "exact limit", body: `{"value":"new"}`, limit: 15, want: "new"},
		{name: "empty", body: "", want: "old"},
		{name: "unicode whitespace", body: "\u2003\t\r\n", want: "old"},
		{name: "null", body: "null", want: "old"},
		{name: "nil output", body: "not json", nilOutput: true, want: "old"},
		{name: "last success status", code: 299, body: `{"value":"new"}`, want: "new"},
		{name: "malformed", body: "{", wantError: "decode example data:", wantSyntax: true},
		{name: "multiple json values", body: "{} {}", wantError: "decode example data:", wantSyntax: true},
		{name: "overflow", body: "123456", limit: 4, wantError: "example response exceeds 4 bytes"},
		{name: "nil output overflow", body: "123456", limit: 4, nilOutput: true, wantError: "example response exceeds 4 bytes"},
		{name: "overflow before status", code: 500, body: "123456", limit: 4, wantError: "example response exceeds 4 bytes"},
		{name: "read before status", code: 404, body: "x", readErr: readErr, wantError: "read failed"},
		{name: "read before overflow", code: 500, body: "123456", limit: 4, readErr: readErr, wantError: "read failed"},
		{name: "below success", code: 199, body: " status body ", wantAPI: true, wantError: "typed adapter error"},
		{name: "above success", code: 300, body: "\u2003status body\n", wantAPI: true, wantError: "typed adapter error"},
		{name: "empty failure", code: 500, wantAPI: true, wantError: "typed adapter error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.code == 0 {
				tc.code = 200
			}
			if tc.limit == 0 {
				tc.limit = 64
			}
			body := &responseBodyProbe{Reader: strings.NewReader(tc.body), readErr: tc.readErr}
			resp := &http.Response{StatusCode: tc.code, Status: "original status", Body: body}
			value := struct{ Value string }{Value: "old"}
			var out any = &value
			if tc.nilOutput {
				out = nil
			}
			calls := 0
			err := DecodeBoundedJSONResponse(resp, tc.limit, out, "example", func(code int, status, text string) error {
				calls++
				if code != tc.code || status != resp.Status || text != strings.TrimSpace(tc.body) {
					t.Fatalf("unexpected adapter error: %d %q %q", code, status, text)
				}
				return apiErr
			})
			if tc.wantError == "" {
				if err != nil || value.Value != tc.want {
					t.Fatalf("result=%q err=%v, want %q", value.Value, err, tc.want)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("err=%v, want %q", err, tc.wantError)
			}
			if tc.readErr != nil && err != tc.readErr {
				t.Fatalf("read cause changed: %v", err)
			}
			var syntax *json.SyntaxError
			if errors.As(err, &syntax) != tc.wantSyntax {
				t.Fatalf("syntax cause changed: %v", err)
			}
			if (calls == 1) != tc.wantAPI || calls > 1 || errors.Is(err, apiErr) != tc.wantAPI {
				t.Fatalf("adapter calls=%d err=%v", calls, err)
			}
			if body.closed != 1 || int64(body.read) > tc.limit+1 {
				t.Fatalf("body close=%d read=%d limit=%d", body.closed, body.read, tc.limit)
			}
		})
	}
}
