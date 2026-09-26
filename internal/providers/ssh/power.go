package ssh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	staticPowerCommandTimeout = 5 * time.Minute
	staticPowerCommandGrace   = 5 * time.Second
	staticPowerStderrTailSize = 4 << 10
)

// lockStaticHostPower serializes start, claim publication, release, and stop
// for one static host so a stop never races a concurrent acquisition.
func lockStaticHostPower(ctx context.Context, host string) (func(), error) {
	sum := sha256.Sum256([]byte(strings.TrimSpace(host)))
	name := "static-host-" + hex.EncodeToString(sum[:8]) + "." + staticProvider + "-power.lock"
	return shared.LockOperation(ctx, staticProvider, name, "static host "+host)
}

func (b *staticLeaseBackend) runStaticPowerCommand(ctx context.Context, field string, argv []string, leaseID, host string) error {
	if b.RT.Exec == nil {
		return core.Exit(2, "%s cannot run: local command runner unavailable", field)
	}
	ctx, cancel := context.WithTimeout(ctx, staticPowerCommandTimeout)
	defer cancel()
	stderrTail := &tailBuffer{limit: staticPowerStderrTailSize}
	result, err := b.RT.Exec.Run(ctx, core.LocalCommandRequest{
		Name:                    argv[0],
		Args:                    argv[1:],
		Env:                     append(os.Environ(), "CRABBOX_LEASE_ID="+leaseID, "CRABBOX_STATIC_HOST="+host),
		Stdout:                  b.RT.Stderr,
		Stderr:                  io.MultiWriter(b.RT.Stderr, stderrTail),
		DisableOutputCapture:    true,
		CancelGracePeriod:       staticPowerCommandGrace,
		RequireProcessGroupJoin: true,
	})
	if err == nil && result.ExitCode == 0 {
		return nil
	}
	if err == nil {
		err = fmt.Errorf("exit status %d", result.ExitCode)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%w (timeout %s)", err, staticPowerCommandTimeout)
	}
	code := result.ExitCode
	if code < 0 {
		code = 1
	}
	return errors.Join(shared.LocalCommandError(field, core.LocalCommandResult{ExitCode: code, Stderr: stderrTail.String()}, err), err, ctx.Err())
}

func (b *staticLeaseBackend) startStaticHost(ctx context.Context, leaseID, host string) error {
	if len(b.Cfg.Static.StartCommand) == 0 {
		return nil
	}
	fmt.Fprintf(b.RT.Stderr, "starting static host=%s lease=%s\n", host, leaseID)
	return b.runStaticPowerCommand(ctx, "static.startCommand", b.Cfg.Static.StartCommand, leaseID, host)
}

type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if n >= t.limit {
		t.data = append(t.data[:0], p[n-t.limit:]...)
		return n, nil
	}
	if keep := t.limit - n; len(t.data) > keep {
		t.data = append(t.data[:0], t.data[len(t.data)-keep:]...)
	}
	t.data = append(t.data, p...)
	return n, nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.data)
}
