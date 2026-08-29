package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestEgressInternalTicketInputFailsClosedBeforeCoordinator(t *testing.T) {
	clearConfigEnv(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	defer server.Close()
	const ticket = "egress_0123456789abcdef0123456789abcdef"
	for _, tt := range []struct {
		name, input, wantErr string
		extra                []string
		readErr              error
	}{
		{name: "empty", wantErr: "is empty"},
		{name: "malformed", input: ticket + "\n", wantErr: "malformed"},
		{name: "oversized", input: strings.Repeat("x", 4097), wantErr: "exceeds 4096"},
		{name: "read error", readErr: errors.New(ticket), wantErr: "read egress ticket input failed"},
		{name: "literal", input: ticket, extra: []string{"--ticket", ticket}, wantErr: "cannot be combined"},
		{name: "empty literal", input: ticket, extra: []string{"--ticket="}, wantErr: "cannot be combined"},
		{name: "single dash literal", input: ticket, extra: []string{"-ticket=" + ticket}, wantErr: "cannot be combined"},
		{name: "trailing flags", input: ticket, extra: []string{"--", "--ticket", ticket}, wantErr: "unexpected positional"},
		{name: "public listener", input: ticket, extra: []string{"--listen", "0.0.0.0:3128"}, wantErr: "loopback-only"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, selector := range []string{egressTicketStdinArg, egressTicketChildArg} {
				t.Run(selector, func(t *testing.T) {
					var reader io.Reader = strings.NewReader(tt.input)
					if tt.readErr != nil {
						reader = credentialErrorReader{tt.readErr}
					}
					input := &credentialTestInput{Reader: reader}
					var output bytes.Buffer
					args := append([]string{"egress", "client", selector, "--id", "cbx_0123456789ab", "--coordinator", server.URL}, tt.extra...)
					err := (App{Stdout: &output, Stderr: &output, Stdin: input}).Run(context.Background(), args)
					if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
						t.Fatalf("wrong failure: %v", err)
					}
					if strings.Contains(err.Error()+output.String(), ticket) {
						t.Fatal("credential exposed in diagnostics")
					}
					if requests.Load() != 0 {
						t.Fatal("invalid internal input reached coordinator")
					}
					if input.reads > 0 && !input.closed {
						t.Fatal("consumed input not closed")
					}
				})
			}
		})
	}
}

func TestEgressTicketInputPreservesPublicHelpAndManualModes(t *testing.T) {
	clearConfigEnv(t)
	for _, args := range [][]string{
		{"--help"}, {"egress", "--help"}, {"help", "egress"}, {"egress", "client", "--help"},
		{"egress", "client", egressTicketStdinArg, "--help"}, {"providers", "describe", "aws", "--json"},
		{"egress", "client", egressTicketChildArg, "--help"},
	} {
		var output bytes.Buffer
		input := &credentialTestInput{Reader: credentialErrorReader{errors.New("must not read")}}
		err := (App{Stdout: &output, Stderr: &output, Stdin: input}).Run(context.Background(), args)
		var exitErr ExitError
		if err != nil && (!errors.As(err, &exitErr) || exitErr.Code != 0) {
			t.Fatalf("help/schema failed for %v: %v", args, err)
		}
		if strings.Contains(output.String(), "internal-ticket") || input.reads != 0 {
			t.Fatalf("private receiver leaked into help/schema or consumed input: %v", args)
		}
		if strings.Contains(strings.Join(args, " "), "client") && !strings.Contains(output.String(), "-ticket") {
			t.Fatal("public manual --ticket help disappeared")
		}
	}
	for _, ticket := range []string{"manual-ticket", "-"} {
		t.Run(ticket, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/v1/leases/cbx_0123456789ab/egress/client" || r.Header.Get("X-Crabbox-Bridge-Ticket") != ticket {
					t.Error("manual ticket was not passed literally to client upgrade")
				}
				http.NotFound(w, r)
			}))
			defer server.Close()
			input := &credentialTestInput{Reader: credentialErrorReader{errors.New("must not read")}}
			err := (App{Stdout: io.Discard, Stderr: io.Discard, Stdin: input}).Run(context.Background(), []string{
				"egress", "client", "--id", "cbx_0123456789ab", "--coordinator", server.URL, "--ticket", ticket,
			})
			if err == nil || calls.Load() != 1 || input.reads != 0 || input.closed {
				t.Fatalf("manual ticket behavior changed: err=%v calls=%d reads=%d closed=%t", err, calls.Load(), input.reads, input.closed)
			}
			// No implicit stdin: missing --ticket must still require login.
			err = (App{Stdout: io.Discard, Stderr: io.Discard, Stdin: input}).Run(context.Background(), []string{
				"egress", "client", "--id", "cbx_0123456789ab", "--coordinator", server.URL,
			})
			if err == nil || !strings.Contains(err.Error(), "configured coordinator login") || calls.Load() != 1 || input.reads != 0 {
				t.Fatalf("no-ticket login behavior changed: %v", err)
			}
		})
	}
}
