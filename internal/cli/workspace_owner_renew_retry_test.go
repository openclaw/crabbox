package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// budgetedOwnerTransport is a scripted owner transport with an explicit call budget,
// so renewal timing can be exercised in milliseconds instead of SSH-sized seconds.
type budgetedOwnerTransport struct {
	budget time.Duration
	renew  func(attempt int) (string, error)

	mu       sync.Mutex
	acquired time.Time
	renewals []time.Time
}

func (b *budgetedOwnerTransport) CallBudget() time.Duration { return b.budget }

func (b *budgetedOwnerTransport) Do(_ context.Context, req workspaceOwnerRemoteRequest) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch req.Action {
	case workspaceOwnerAcquire:
		b.acquired = time.Now()
		return "ACQUIRED", nil
	case workspaceOwnerRenew:
		b.renewals = append(b.renewals, time.Now())
		return b.renew(len(b.renewals))
	default:
		return "", errors.New("unexpected owner action " + string(req.Action))
	}
}

func (b *budgetedOwnerTransport) snapshot() (time.Time, []time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.acquired, append([]time.Time(nil), b.renewals...)
}

func acquireBudgetedOwner(t *testing.T, transport *budgetedOwnerTransport, ttl, interval time.Duration) *workspaceOwner {
	t.Helper()
	owner, err := acquireWorkspaceOwnerWithTransport(t.Context(), SSHTarget{}, "cbx_renew_retry", io.Discard, transport, time.Minute, ttl, interval)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() {
		select {
		case <-owner.done:
		default:
			close(owner.stop)
			<-owner.done
		}
	})
	return owner
}

// A renewal that got no protocol answer (the SSH call itself failed) is retried while
// the retry can still finish inside the ownership the last confirmed renewal proved.
// Before this, one lost renewal call killed a healthy long-running job (exit 7).
func TestWorkspaceOwnerRenewalSurvivesOneTransportFailure(t *testing.T) {
	transport := &budgetedOwnerTransport{budget: 100 * time.Millisecond, renew: func(attempt int) (string, error) {
		if attempt == 1 {
			return "", errors.New("ssh: connect to host: Operation timed out")
		}
		return "RENEWED", nil
	}}
	owner := acquireBudgetedOwner(t, transport, 3*time.Second, 50*time.Millisecond)
	time.Sleep(600 * time.Millisecond)
	if err := owner.Err(); err != nil {
		t.Fatalf("one transport failure inside the ownership window failed the owner: %v", err)
	}
	if err := owner.Context().Err(); err != nil {
		t.Fatalf("one transport failure inside the ownership window canceled the job: %v", err)
	}
	if _, renewals := transport.snapshot(); len(renewals) < 3 {
		t.Fatalf("renewal stopped after the retried failure: %d calls", len(renewals))
	}
}

// When every renewal call fails, the owner still fails closed, and no retry starts
// that could finish after the last confirmed ownership lapses.
func TestWorkspaceOwnerRenewalFailsClosedWhenTheWindowCloses(t *testing.T) {
	const (
		ttl    = 3 * time.Second
		budget = 200 * time.Millisecond
	)
	transport := &budgetedOwnerTransport{budget: budget, renew: func(int) (string, error) {
		return "", errors.New("ssh: connect to host: Operation timed out")
	}}
	owner := acquireBudgetedOwner(t, transport, ttl, 50*time.Millisecond)
	select {
	case <-owner.done:
	case <-time.After(2 * ttl):
		t.Fatal("renewal never failed closed")
	}
	failedAt := time.Now()
	var exitErr ExitError
	if err := owner.Err(); !AsExitError(err, &exitErr) || exitErr.Code != 7 || !strings.Contains(err.Error(), "renewal failed closed") {
		t.Fatalf("err=%v; want exit 7 renewal failed closed", err)
	}
	if owner.Context().Err() != context.Canceled {
		t.Fatalf("job context=%v; want canceled", owner.Context().Err())
	}
	acquired, renewals := transport.snapshot()
	if len(renewals) < 2 {
		t.Fatalf("the transport failure was not retried: %d calls", len(renewals))
	}
	confirmedUntil := acquired.Add(ttl - workspaceOwnerRenewMargin)
	if last := renewals[len(renewals)-1]; last.Add(budget).After(confirmedUntil) {
		t.Fatalf("a retry started at %s could finish after ownership lapsed at %s", last, confirmedUntil)
	}
	if slack := 100 * time.Millisecond; failedAt.After(confirmedUntil.Add(slack)) {
		t.Fatalf("failed closed at %s, after ownership lapsed at %s", failedAt, confirmedUntil)
	}
}

// A recognized protocol answer is final: it is never retried.
func TestWorkspaceOwnerRenewalProtocolAnswerIsNotRetried(t *testing.T) {
	transport := &budgetedOwnerTransport{budget: 100 * time.Millisecond, renew: func(int) (string, error) {
		return "EXPIRED", errors.New("exit status 75")
	}}
	owner := acquireBudgetedOwner(t, transport, 3*time.Second, 50*time.Millisecond)
	select {
	case <-owner.done:
	case <-time.After(time.Second):
		t.Fatal("an EXPIRED renewal did not fail closed")
	}
	if err := owner.Err(); err == nil || !strings.Contains(err.Error(), "protocol state EXPIRED") {
		t.Fatalf("err=%v; want the EXPIRED state", err)
	}
	if _, renewals := transport.snapshot(); len(renewals) != 1 {
		t.Fatalf("an EXPIRED answer was retried: %d calls", len(renewals))
	}
}
