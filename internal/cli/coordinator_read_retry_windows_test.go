package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
)

func TestCoordinatorReadRetryWindowsConnectionErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"wsarecv_aborted", syscall.WSAECONNABORTED, "connection_reset"},
		{"wsarecv_reset", syscall.WSAECONNRESET, "connection_reset"},
		{"legacy_reset", syscall.ECONNRESET, "connection_reset"},
		{"permission", syscall.WSAEACCES, ""},
		{"canceled", context.Canceled, ""},
		{"certificate", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := &url.Error{Op: "Get", URL: "https://coordinator.fixture.invalid/v1/health", Err: &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "wsarecv", Err: test.err}}}
			if got := coordinatorReadRetryReason(err); got != test.want {
				t.Fatalf("retry reason=%q want=%q for %v", got, test.want, err)
			}
		})
	}
}
