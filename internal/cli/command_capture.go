package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// Polling cancels runaway writers; it permits transient disk overshoot between
// observations and process termination. Readback has a separate hard byte cap.
const commandCapturePollInterval = 10 * time.Millisecond

type commandCaptureFile struct {
	writer *os.File
	reader *os.File
}

type commandFileCapture struct {
	streams [2]commandCaptureFile
	limit   int
}

type commandFileCaptureOutcome struct {
	settled  bool
	overflow bool
	err      error
}

func newCommandFileCapture(limit int) (_ *commandFileCapture, err error) {
	capture := &commandFileCapture{limit: limit}
	defer func() {
		if err != nil {
			capture.close()
		}
	}()
	for i := range capture.streams {
		stream := &capture.streams[i]
		stream.writer, err = os.CreateTemp("", "crabbox-command-output-*")
		if err != nil {
			return nil, fmt.Errorf("create command output capture: %w", err)
		}
		defer os.Remove(stream.writer.Name())
		stream.reader, err = os.Open(stream.writer.Name())
		if err != nil {
			return nil, fmt.Errorf("open command output capture: %w", err)
		}
		if _, err = lockCommandCaptureFile(stream.writer, true); err != nil {
			return nil, fmt.Errorf("lock command output capture: %w", err)
		}
		// The child inherits the locked description through dup2. Unlink before
		// launch so an outer owner's SIGKILL cannot leave a named output file.
		if err = os.Remove(stream.writer.Name()); err != nil {
			return nil, fmt.Errorf("unlink command output capture: %w", err)
		}
	}
	return capture, nil
}

func (c *commandFileCapture) closeWriters() {
	for i := range c.streams {
		if c.streams[i].writer != nil {
			_ = c.streams[i].writer.Close()
			c.streams[i].writer = nil
		}
	}
}

func (c *commandFileCapture) close() {
	c.closeWriters()
	for _, stream := range c.streams {
		if stream.reader != nil {
			_ = stream.reader.Close()
		}
	}
}

func (c *commandFileCapture) observe() (settled, overflow bool, err error) {
	settled = true
	for _, stream := range c.streams {
		info, err := stream.reader.Stat()
		if err != nil {
			return false, overflow, err
		}
		overflow = overflow || info.Size() > int64(c.limit)
		closed, err := lockCommandCaptureFile(stream.reader, false)
		if err != nil {
			return false, overflow, err
		}
		settled = settled && closed
	}
	return settled, overflow, nil
}

func (c *commandFileCapture) watch(cancel context.CancelFunc, stop func() error, waitDelay time.Duration) func() commandFileCaptureOutcome {
	commandDone := make(chan struct{})
	result := make(chan commandFileCaptureOutcome, 1)
	go func() {
		done := commandDone
		var outcome commandFileCaptureOutcome
		defer func() { result <- outcome }()
		ticker := time.NewTicker(commandCapturePollInterval)
		defer ticker.Stop()
		var deadline <-chan time.Time
		completed, stopped := false, false
		for {
			settled, overflow, err := c.observe()
			outcome.overflow = outcome.overflow || overflow
			if err != nil && outcome.err == nil {
				outcome.err = fmt.Errorf("inspect command output capture: %w", err)
			}
			if outcome.overflow || outcome.err != nil {
				cancel()
			}
			if completed {
				// Wait has consumed os/exec's context watcher. Post-Wait failures
				// must invoke the same cancellation owner directly.
				if !stopped && (outcome.overflow || outcome.err != nil) {
					_ = stop()
					stopped = true
				}
				if outcome.err != nil {
					return
				}
				if settled {
					outcome.settled = true
					return
				}
			}
			select {
			case <-done:
				completed = true
				done = nil
				timer := time.NewTimer(waitDelay)
				defer timer.Stop()
				deadline = timer.C
			case <-deadline:
				_ = stop()
				outcome.err = errors.Join(outcome.err, fmt.Errorf("command output writers did not close: %w", exec.ErrWaitDelay))
				return
			case <-ticker.C:
			}
		}
	}()
	return func() commandFileCaptureOutcome {
		close(commandDone)
		return <-result
	}
}

func (c *commandFileCapture) read(stdout, stderr *commandCaptureBuffer) error {
	for i, output := range []*commandCaptureBuffer{stdout, stderr} {
		if _, err := io.Copy(output, io.NewSectionReader(c.streams[i].reader, 0, int64(c.limit)+1)); err != nil {
			return fmt.Errorf("read command output capture: %w", err)
		}
	}
	return nil
}
