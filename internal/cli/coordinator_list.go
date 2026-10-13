package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"sync"
)

type coordinatorLeaseQuery struct {
	provider string
	state    string
	current  bool
	admin    bool
}

// visitLeasePages hands each bounded page to the caller before requesting the
// next one. Legacy servers retain their capped, newest-first response contract.
func (c *CoordinatorClient) visitLeasePages(ctx context.Context, query coordinatorLeaseQuery, visit func([]CoordinatorLease, bool) error) (bool, error) {
	const pagination = "keyset-v1"
	values := url.Values{"limit": {"100"}, "projection": {"summary"}, "pagination": {pagination}}
	if query.provider != "" {
		values.Set("provider", query.provider)
	}
	if query.state != "" {
		values.Set("state", query.state)
	}
	if query.current {
		values.Set("view", "current")
	}
	path := "/v1/leases"
	if query.admin {
		path = "/v1/admin/leases"
	}
	seenCursors := make(map[string]bool)
	for {
		var res struct {
			Leases     []CoordinatorLease `json:"leases"`
			Pagination string             `json:"pagination"`
			NextCursor string             `json:"nextCursor"`
		}
		if err := c.doRead(ctx, path+"?"+values.Encode(), &res); err != nil {
			return false, err
		}
		if res.Pagination != pagination {
			if values.Get("cursor") != "" {
				return false, fmt.Errorf("coordinator lease pagination changed during listing; retry list")
			}
			if query.admin || len(res.Leases) >= 100 {
				values.Del("pagination")
				values.Del("projection")
				values.Set("limit", "500")
				res.Leases = nil
				if err := c.doRead(ctx, path+"?"+values.Encode(), &res); err != nil {
					return false, err
				}
			}
			return len(res.Leases) >= 500, visit(res.Leases, false)
		}
		if res.NextCursor != "" {
			if seenCursors[res.NextCursor] {
				return false, fmt.Errorf("coordinator lease pagination did not advance")
			}
			seenCursors[res.NextCursor] = true
		}
		if err := visit(res.Leases, res.NextCursor != ""); err != nil {
			return false, err
		}
		if res.NextCursor == "" {
			return false, nil
		}
		values.Set("cursor", res.NextCursor)
	}
}

const coordinatorListConcurrency = 4

type coordinatorListPage struct {
	leases    []CoordinatorLease
	more      bool
	ack       chan struct{}
	done      bool
	truncated bool
	err       error
}

type listRetryWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *listRetryWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

// Read sections concurrently but drain them in the requested order. A worker
// waits for rendering acknowledgement before fetching another page, so even a
// slow first section cannot accumulate the other sections' history in memory.
func (c *CoordinatorClient) streamLeaseSections(ctx context.Context, queries []coordinatorLeaseQuery, visit func(int, []CoordinatorLease, bool) error) ([]bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	retryWriter := c.readRetryWriter
	if retryWriter == nil {
		retryWriter = os.Stderr
	}
	client := &CoordinatorClient{
		BaseURL: c.BaseURL, Token: c.Token, TokenCommand: c.TokenCommand,
		Access: c.Access, Client: c.Client, ChildEnvDenylist: c.ChildEnvDenylist,
		readRetryWriter: &listRetryWriter{w: retryWriter},
	}
	channels := make([]chan coordinatorListPage, len(queries))
	// Start the first sections first; later sections must not take every slot
	// while the consumer waits for a section that has not started yet.
	for i := range queries {
		channels[i] = make(chan coordinatorListPage)
	}
	jobs := make(chan int)
	for range min(len(queries), coordinatorListConcurrency) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				query := queries[i]
				ch := channels[i]
				truncated, err := client.visitLeasePages(ctx, query, func(leases []CoordinatorLease, more bool) error {
					ack := make(chan struct{})
					select {
					case ch <- coordinatorListPage{leases: leases, more: more, ack: ack}:
					case <-ctx.Done():
						return ctx.Err()
					}
					select {
					case <-ack:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
				select {
				case ch <- coordinatorListPage{done: true, truncated: truncated, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		defer close(jobs)
		for i := range queries {
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	truncated := make([]bool, len(queries))
	for i, ch := range channels {
		for {
			var page coordinatorListPage
			select {
			case page = <-ch:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if page.done {
				if page.err != nil {
					return nil, page.err
				}
				truncated[i] = page.truncated
				break
			}
			if err := visit(i, page.leases, page.more); err != nil {
				return nil, err
			}
			close(page.ack)
		}
	}
	return truncated, nil
}
