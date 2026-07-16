package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFailedStopsTelemetrySampler(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	}))
	defer server.Close()

	coord := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	rec := &runRecorder{coord: coord, runID: "run-leak-repro", stderr: io.Discard}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec.StartTelemetrySampler(ctx, SSHTarget{User: "root", Host: "192.0.2.1", Port: "22"})

	rec.telemetryMu.Lock()
	done := rec.telemetryDone
	rec.telemetryMu.Unlock()
	if done == nil {
		t.Fatal("telemetry sampler did not start")
	}

	rec.Failed(errors.New("upload run env profile: connection reset"))

	select {
	case <-done:
	default:
		t.Fatal("telemetry sampler still running after Failed returned")
	}
}
