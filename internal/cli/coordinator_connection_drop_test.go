package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
)

func TestCoordinatorReadRetriesConnectionDrops(t *testing.T) {
	idleClosed := errors.New("http: server closed idle connection")
	reset := &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}
	for _, test := range []struct {
		name string
		err  error
	}{
		{"EOF", io.EOF},
		{"unexpected EOF", io.ErrUnexpectedEOF},
		{"connection reset", reset},
		{"connection aborted", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNABORTED}},
		{"broken pipe", &net.OpError{Op: "write", Net: "tcp", Err: syscall.EPIPE}},
		{"closed socket", net.ErrClosed},
		{"closed pipe", io.ErrClosedPipe},
		{"idle connection closed", idleClosed},
		{"wrapped idle connection closed", fmt.Errorf("round trip: %w", idleClosed)},
		{"broken transport EOF", fmt.Errorf("net/http: HTTP/1.x transport connection broken: %w", io.EOF)},
		{"broken transport reset", fmt.Errorf("net/http: HTTP/1.x transport connection broken: %w", reset)},
		{"broken transport idle close", fmt.Errorf("net/http: HTTP/1.x transport connection broken: %w", idleClosed)},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
				t.Run(method, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						var progress bytes.Buffer
						calls := 0
						client := &CoordinatorClient{BaseURL: "https://coordinator.example.test", Token: "synthetic-drop", readRetryWriter: &progress, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
							calls++
							if r.Method != method || r.URL.Path != "/v1/health" || r.Header.Get("Authorization") != "Bearer synthetic-drop" {
								t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
							}
							if calls == 1 {
								return nil, test.err
							}
							return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
						})}}
						if method == http.MethodGet {
							if err := client.Health(t.Context()); err != nil || calls != 2 {
								t.Fatalf("read recovery err=%v requests=%d", err, calls)
							}
							if progress.String() != "coordinator read retry 1/4 reason=connection_reset\n" {
								t.Fatalf("progress=%q", progress.String())
							}
						} else if err := client.doControl(t.Context(), method, "/v1/health", nil, nil); !errors.Is(err, test.err) || calls != 1 || progress.Len() != 0 {
							t.Fatalf("mutation err=%v requests=%d progress=%q", err, calls, progress.String())
						}
					})
				})
			}
			t.Run("exhausted", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					calls := 0
					client := &CoordinatorClient{BaseURL: "https://coordinator.example.test", readRetryWriter: io.Discard, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						calls++
						return nil, test.err
					})}}
					if err := client.Health(t.Context()); !errors.Is(err, test.err) || calls != 5 {
						t.Fatalf("exhausted read err=%v requests=%d", err, calls)
					}
				})
			})
		})
	}
}

func TestCoordinatorReadRejectsNonDropErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{"untyped EOF", errors.New("EOF")},
		{"untyped reset", errors.New("connection reset by peer")},
		{"idle close substring", errors.New("invalid response: http: server closed idle connection")},
		{"malformed HTTP", fmt.Errorf("net/http: HTTP/1.x transport connection broken: %w", &http.ProtocolError{ErrorString: "malformed response"})},
		{"certificate", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}},
		{"permission", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EACCES}},
		{"canceled drop", errors.Join(net.ErrClosed, context.Canceled)},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client := &CoordinatorClient{BaseURL: "https://coordinator.example.test", readRetryWriter: io.Discard, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					return nil, test.err
				})}}
				if err := client.Health(t.Context()); !errors.Is(err, test.err) || calls != 1 {
					t.Fatalf("terminal read err=%v requests=%d", err, calls)
				}
			})
		})
	}
}
