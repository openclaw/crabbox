package remoteruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type progressReader struct {
	reader io.Reader
	start  time.Time
	last   atomic.Int64
}

var errFrameCanceled = errors.New("command frame transfer canceled")

func interruptibleInput(input *os.File) (*os.File, error) {
	raw, err := input.SyscallConn()
	if err != nil {
		return nil, err
	}
	fd := -1
	var duplicateErr error
	if err := raw.Control(func(original uintptr) {
		fd, duplicateErr = unix.FcntlInt(original, unix.F_DUPFD_CLOEXEC, 0)
	}); err != nil {
		return nil, err
	}
	if duplicateErr != nil {
		return nil, duplicateErr
	}
	// Inherited stdin may be an unpollable blocking os.File. Register a private
	// nonblocking duplicate so Close interrupts reads when upload or owner ends.
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "remote-runtime-input"), nil
}

func (p *progressReader) Read(buffer []byte) (int, error) {
	n, err := p.reader.Read(buffer)
	if n > 0 {
		p.last.Store(time.Since(p.start).Nanoseconds())
	}
	return n, err
}

func receiveFrame(ctx context.Context, s *stage, req request, input *os.File) error {
	reader := &progressReader{reader: input, start: time.Now()}
	done := make(chan error, 1)
	go func() {
		err := s.receive(reader, "command", req.commandBytes)
		if err == nil {
			err = s.receive(reader, "input", req.inputBytes)
		}
		done <- err
	}()
	timer := time.NewTimer(req.idle)
	defer timer.Stop()
	cancellation := time.NewTicker(100 * time.Millisecond)
	defer cancellation.Stop()
	for {
		var stop error
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			stop = errors.Join(errFrameCanceled, context.Cause(ctx))
		case <-cancellation.C:
			canceled, err := s.canceled(ctx)
			if err == nil && !canceled {
				continue
			}
			stop = err
			if canceled {
				stop = errors.Join(errFrameCanceled, err)
			}
		case <-timer.C:
			remaining := req.idle - (time.Since(reader.start) - time.Duration(reader.last.Load()))
			if remaining > 0 {
				timer.Reset(remaining)
				continue
			}
			stop = errors.New("command frame transfer idle timeout")
		}
		_ = input.Close()
		return errors.Join(stop, <-done)
	}
}

func run(ctx context.Context, req request, stdin, stdout, stderr *os.File) (int, error) {
	// Keep this owner alive long enough to finish cleanup after SSH hangs up.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGHUP)
	defer stop()
	if err := context.Cause(ctx); err != nil {
		return failureCode, err
	}
	inputStream, err := interruptibleInput(stdin)
	if err != nil {
		return failureCode, fmt.Errorf("prepare interruptible command input: %w", err)
	}
	defer inputStream.Close()
	stdin = inputStream
	s, err := newStage(req.nonce)
	if err != nil {
		return failureCode, err
	}
	defer s.root.Close()
	if err := receiveFrame(ctx, s, req, stdin); err != nil {
		if errors.Is(err, errFrameCanceled) {
			return failureCode, s.finish(req.preflight, failureCode, "canceled")
		}
		return failureCode, errors.Join(err, s.finish(false, failureCode, ""))
	}
	if canceled, err := s.canceled(ctx); err != nil {
		return failureCode, errors.Join(err, s.finish(false, failureCode, ""))
	} else if canceled {
		return failureCode, s.finish(req.preflight, failureCode, "canceled")
	}
	input, err := s.root.Open("input")
	if err != nil {
		return failureCode, errors.Join(err, s.finish(false, failureCode, ""))
	}
	defer input.Close()
	setupCtx, cancelSetup := context.WithTimeout(ctx, req.idle)
	g, err := startGroup(setupCtx, req.grace)
	cancelSetup()
	if err != nil {
		if g == nil {
			if ctx.Err() != nil {
				return failureCode, s.finish(req.preflight, failureCode, "canceled")
			}
			return failureCode, errors.Join(err, s.finish(false, failureCode, ""))
		}
		// Only unconfirmed group cleanup retains the stage.
		return failureCode, err
	}
	if canceled, err := s.canceled(ctx); canceled || err != nil {
		if cleanupErr := g.close(); cleanupErr != nil {
			return failureCode, errors.Join(err, cleanupErr)
		}
		if err != nil {
			return failureCode, errors.Join(err, s.finish(false, failureCode, ""))
		}
		return failureCode, s.finish(req.preflight, failureCode, "canceled")
	}
	started := time.Now()
	if err := g.start(s.commandPath(), input, stdout, stderr); err != nil {
		if cleanupErr := g.close(); cleanupErr != nil {
			return failureCode, errors.Join(err, cleanupErr)
		}
		return failureCode, errors.Join(err, s.finish(false, failureCode, ""))
	}
	var deadline <-chan time.Time
	if req.execution > 0 {
		timer := time.NewTimer(max(0, req.execution-time.Since(started)))
		defer timer.Stop()
		deadline = timer.C
	}
	var lost <-chan struct{}
	if req.watchInput {
		done := make(chan struct{})
		lost = done
		defer stdin.Close()
		go func() {
			var one [1]byte
			_, _ = stdin.Read(one[:])
			close(done)
		}()
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	code, reason := failureCode, ""
running:
	for {
		select {
		case waitErr := <-g.result:
			g.result = nil // Wait completed; cleanup must not wait on it twice.
			code = commandExitCode(waitErr)
			if req.execution > 0 && time.Since(started) >= req.execution {
				code, reason = failureCode, "timed-out"
			}
			break running
		case <-ctx.Done():
			reason = "canceled"
			break running
		case <-lost:
			reason = "canceled"
			break running
		case <-deadline:
			reason = "timed-out"
			break running
		case <-ticker.C:
			canceled, checkErr := s.canceled(ctx)
			if checkErr != nil {
				err = checkErr
				break running
			}
			if canceled {
				reason = "canceled"
				break running
			}
		}
	}
	if cleanupErr := g.close(); cleanupErr != nil {
		return failureCode, errors.Join(err, cleanupErr)
	}
	if err != nil {
		return failureCode, err
	}
	if err := s.finish(req.preflight, code, reason); err != nil {
		return failureCode, fmt.Errorf("command stage cleanup unconfirmed: %w", err)
	}
	return code, nil
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exit.ExitCode()
	}
	return failureCode
}
