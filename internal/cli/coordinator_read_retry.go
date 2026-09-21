package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	coordinatorReadBudget     = time.Minute
	coordinatorReadAttempts   = 5
	coordinatorReadBackoffCap = 8 * time.Second
)

// Opt in only read-only operations; mutations retain their own replay contracts.
func (c *CoordinatorClient) doRead(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, coordinatorReadBudget)
	defer cancel()
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A failed log-body read must not prepend a partial response to its retry.
		if attempt > 1 {
			if buffer, ok := out.(*bytes.Buffer); ok {
				buffer.Reset()
			}
		}
		attemptCtx, stopAttempt := context.WithTimeout(ctx, coordinatorControlTimeout)
		err := c.do(attemptCtx, http.MethodGet, path, nil, out)
		stopAttempt()
		if ctx.Err() != nil {
			return errors.Join(ctx.Err(), err)
		}
		reason := coordinatorReadRetryReason(err)
		if err == nil || reason == "" || attempt == coordinatorReadAttempts {
			return err
		}
		delay := coordinatorReadBackoff(attempt, err)
		if attempt == 1 {
			writer := c.readRetryWriter
			if writer == nil {
				writer = os.Stderr
			}
			fmt.Fprintf(writer, "coordinator read retry %d/%d reason=%s\n", attempt, coordinatorReadAttempts-1, reason)
		}
		if err := sleepContext(ctx, delay); err != nil {
			return err
		}
	}
}

func coordinatorReadRetryReason(err error) string {
	if err == nil || errors.Is(err, context.Canceled) {
		return ""
	}
	var response CoordinatorHTTPError
	if errors.As(err, &response) {
		switch response.StatusCode {
		case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return fmt.Sprintf("http_%d", response.StatusCode)
		default:
			return ""
		}
	}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout() {
		return "timeout"
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "connection_reset"
	}
	return ""
}

func coordinatorReadBackoff(attempt int, err error) time.Duration {
	delay := min(time.Second<<(attempt-1), coordinatorReadBackoffCap)
	// Positive jitter avoids synchronized retries without shortening Retry-After.
	delay = min(delay+time.Duration(rand.Int64N(int64(delay/4))), coordinatorReadBackoffCap)
	var response CoordinatorHTTPError
	if errors.As(err, &response) {
		delay = max(delay, response.retryAfter)
	}
	return min(delay, coordinatorReadBackoffCap)
}

func coordinatorReadRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Duration(min(max(seconds, 0), int64(coordinatorReadBackoffCap/time.Second))) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		return min(max(date.Sub(now), 0), coordinatorReadBackoffCap)
	}
	return 0
}
