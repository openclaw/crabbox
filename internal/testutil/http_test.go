package testutil

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

func TestPipeHTTPServerDistinctOrigins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first := NewPipeHTTPServer(t, http.NotFoundHandler())
		second := NewPipeHTTPServer(t, http.NotFoundHandler())
		if first.URL == second.URL {
			t.Fatal("independent servers must not share SDK cache identity")
		}
	})
}

func TestPipeHTTPServerVirtualTimeoutAndStreaming(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const timeout = 50 * time.Millisecond
		server := NewPipeHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/stalled" {
				<-r.Context().Done()
				return
			}
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			time.Sleep(2 * timeout)
			_, _ = io.WriteString(w, "complete")
		}))
		client := server.Client()
		client.Timeout = timeout
		start := time.Now()
		_, err := client.Get(server.URL + "/stalled")
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != timeout {
			t.Fatalf("stalled err=%v elapsed=%s", err, time.Since(start))
		}
		client.Timeout = 0
		client.Transport.(*http.Transport).ResponseHeaderTimeout = timeout
		start = time.Now()
		resp, err := client.Get(server.URL + "/stream")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil || string(body) != "complete" || time.Since(start) != 2*timeout {
			t.Fatalf("stream body=%q err=%v elapsed=%s", body, err, time.Since(start))
		}
	})
}
