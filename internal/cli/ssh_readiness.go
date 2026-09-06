package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

var errSSHHostKeyVerification = errors.New("SSH host-key verification failed; verify the lease identity and its SSH host trust before reconnecting")

const sshHostKeyVerificationDiagnostic = "Host key verification failed."

var sshHostKeyVerificationPrefixes = func() [len(sshHostKeyVerificationDiagnostic)]int {
	var prefixes [len(sshHostKeyVerificationDiagnostic)]int
	for i, matched := 1, 0; i < len(prefixes); i++ {
		for matched > 0 && sshHostKeyVerificationDiagnostic[i] != sshHostKeyVerificationDiagnostic[matched] {
			matched = prefixes[matched-1]
		}
		if sshHostKeyVerificationDiagnostic[i] == sshHostKeyVerificationDiagnostic[matched] {
			matched++
		}
		prefixes[i] = matched
	}
	return prefixes
}()

// Retain only matching state, never stderr bytes. Matching survives arbitrary
// write boundaries and output volume without weakening bounded-buffer semantics.
type sshReadinessDiagnostic struct {
	mu       sync.Mutex
	matched  int
	rejected bool
}

func (d *sshReadinessDiagnostic) Write(data []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.rejected {
		for _, b := range data {
			for d.matched > 0 && b != sshHostKeyVerificationDiagnostic[d.matched] {
				d.matched = sshHostKeyVerificationPrefixes[d.matched-1]
			}
			if b == sshHostKeyVerificationDiagnostic[d.matched] {
				d.matched++
			}
			if d.matched == len(sshHostKeyVerificationDiagnostic) {
				d.rejected = true
				break
			}
		}
	}
	return len(data), nil
}

func (d *sshReadinessDiagnostic) hostKeyRejected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rejected
}

func runSSHReadinessProbe(ctx context.Context, target SSHTarget, remote, connectTimeout, attempts string) error {
	var diagnostic sshReadinessDiagnostic
	err := executeSSH(ctx, &target, remote, nil, 0, 0, connectTimeout, attempts, io.Discard, &diagnostic)
	return sshReadinessProbeError(ctx, err, diagnostic.hostKeyRejected())
}

func sshReadinessProbeError(ctx context.Context, err error, hostKeyRejected bool) error {
	// A host-key rejection cannot recover while waiting for guest bootstrap.
	// Retain the exit cause, but never expose captured remote stderr or key data.
	if ctx.Err() == nil && exitCode(err) == 255 && hostKeyRejected {
		return errors.Join(errSSHHostKeyVerification, err)
	}
	return err
}

func sshReadinessError(err error, phase string) error {
	if errors.Is(err, errSSHHostKeyVerification) {
		return fmt.Errorf("%s: %w", phase, err)
	}
	return workspaceOwnerReadinessError(err, phase)
}
