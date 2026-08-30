package cli

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type credentialTestInput struct {
	io.Reader
	closed   bool
	closeErr error
	reads    int
}

func (r *credentialTestInput) Read(p []byte) (int, error) {
	r.reads++
	return r.Reader.Read(p)
}

func (r *credentialTestInput) Close() error {
	r.closed = true
	return r.closeErr
}

type credentialErrorReader struct{ err error }

func (r credentialErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestWebVNCCredentialInputPreservesContract(t *testing.T) {
	for _, tt := range []struct {
		name, input, wantErr string
	}{
		{"exact bytes", "  synthetic\ncredential\x00 ", ""},
		{"limit", strings.Repeat("x", 4096), ""},
		{"empty", "", "external desktop credential is empty"},
		{"whitespace", " \n\t", "external desktop credential is empty"},
		{"oversized", strings.Repeat("x", 4097), "external desktop credential exceeds 4096 bytes"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := &credentialTestInput{Reader: strings.NewReader(tt.input)}
			got, err := readWebVNCDaemonCredentialStdin(input)
			if tt.wantErr == "" {
				if err != nil || got != tt.input {
					t.Fatalf("credential bytes changed or read failed: %v", err)
				}
			} else if err == nil || err.Error() != tt.wantErr || got != "" {
				t.Fatalf("err=%v, want %s; returned value length=%d", err, tt.wantErr, len(got))
			}
			if input.closed {
				t.Fatal("WebVNC reader changed its input ownership")
			}
		})
	}
	_, err := readWebVNCDaemonCredentialStdin(credentialErrorReader{io.ErrUnexpectedEOF})
	if err == nil || err.Error() != "read external desktop credential: unexpected EOF" {
		t.Fatalf("read error wording changed: %v", err)
	}
}

func TestEgressTicketInputRejectsInvalidAndCloses(t *testing.T) {
	const ticket = "egress_0123456789abcdef0123456789abcdef"
	for _, tt := range []struct {
		name, input, wantErr string
		readErr, closeErr    error
	}{
		{name: "valid", input: ticket},
		{name: "empty", wantErr: "egress ticket input is empty"},
		{name: "whitespace", input: " \n", wantErr: "egress ticket input is empty"},
		{name: "oversized", input: ticket + strings.Repeat("x", 4097), wantErr: "egress ticket input exceeds 4096 bytes"},
		{name: "newline", input: ticket + "\n", wantErr: "malformed egress ticket input"},
		{name: "leading space", input: " " + ticket, wantErr: "malformed egress ticket input"},
		{name: "truncated", input: ticket[:len(ticket)-1], wantErr: "malformed egress ticket input"},
		{name: "wrong prefix", input: "wvnc_0123456789abcdef0123456789abcdef", wantErr: "malformed egress ticket input"},
		{name: "uppercase", input: strings.ToUpper(ticket), wantErr: "malformed egress ticket input"},
		{name: "non hex", input: ticket[:len(ticket)-1] + "z", wantErr: "malformed egress ticket input"},
		{name: "nul", input: ticket[:len(ticket)-1] + "\x00", wantErr: "malformed egress ticket input"},
		{name: "read failure", input: ticket, readErr: errors.New(ticket), wantErr: "read egress ticket input failed"},
		{name: "close failure", input: ticket, closeErr: errors.New(ticket), wantErr: "close egress ticket input failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var reader io.Reader = strings.NewReader(tt.input)
			if tt.readErr != nil {
				reader = io.MultiReader(reader, credentialErrorReader{tt.readErr})
			}
			input := &credentialTestInput{Reader: reader, closeErr: tt.closeErr}
			got, err := readEgressTicketStdin(input)
			if tt.wantErr == "" {
				if err != nil || got != ticket {
					t.Fatalf("exact ticket read failed: %v", err)
				}
				if _, err := reader.Read(make([]byte, 1)); err != io.EOF {
					t.Fatalf("input not consumed to EOF: %v", err)
				}
			} else if err == nil || err.Error() != tt.wantErr || got != "" {
				t.Fatalf("wrong rejection or returned credential: err=%v length=%d", err, len(got))
			}
			if err != nil && strings.Contains(err.Error(), ticket) {
				t.Fatal("credential exposed by error")
			}
			if !input.closed {
				t.Fatal("consumed credential input left open")
			}
		})
	}
}

func TestCredentialInputStopsAtBound(t *testing.T) {
	r := strings.NewReader(strings.Repeat("x", 8192))
	_, err := readCredentialInput(r)
	if err != errCredentialInputTooLarge || r.Len() != 8192-4097 {
		t.Fatalf("reader exceeded bound: err=%v remaining=%d", err, r.Len())
	}
}
