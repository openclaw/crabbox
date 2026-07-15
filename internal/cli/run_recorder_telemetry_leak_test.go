package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestFailedStopsTelemetrySampler proves a goroutine leak in runRecorder.Failed
// (internal/cli/run_recorder.go:195).
//
// Finish (run_recorder.go:185) calls stopTelemetrySampler before finishing the
// run, but Failed marks the run finished WITHOUT stopping the sampler started
// by StartTelemetrySampler. In production, runCommandWithBenchmarkRecord defers
// recorder.Failed(runFailure); every `return recordFailure(err)` site after
// StartTelemetrySampler therefore leaks the 15s-ticker sampler goroutine, whose
// context is derived from the long-lived CLI/bench-loop ctx and is never
// cancelled — it keeps SSH-probing the already-released lease forever.
//
// The test mirrors that exact sequence with the real entry points
// (StartTelemetrySampler, Failed) and asserts the sampler goroutine exits after
// Failed, exactly as it does after Finish. On current code it FAILS: the
// sampler's done channel never closes.
func TestFailedStopsTelemetrySampler(t *testing.T) {
	// Coordinator stub so Failed's run.failed event append succeeds quickly.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{}")
	}))
	defer server.Close()

	coord := &CoordinatorClient{BaseURL: server.URL, Token: "test-token", Client: server.Client()}
	rec := &runRecorder{coord: coord, runID: "run-leak-repro", stderr: io.Discard}

	// Models the long-lived CLI / bench-loop context (bench.go repeat loop),
	// which outlives any single failed repeat and is NOT cancelled on failure.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Production call site: run.go after lease acquisition + sync.
	rec.StartTelemetrySampler(ctx, SSHTarget{User: "root", Host: "192.0.2.1", Port: "22"})

	rec.telemetryMu.Lock()
	done := rec.telemetryDone
	rec.telemetryMu.Unlock()
	if done == nil {
		t.Fatal("telemetry sampler did not start")
	}

	// Production failure path: the deferred recorder.Failed(runFailure) that
	// runs when runCommandWithBenchmarkRecord hits any recordFailure(err)
	// return after the sampler was started.
	rec.Failed(errors.New("upload run env profile: connection reset"))

	// Finish guarantees the sampler goroutine has exited (stopTelemetrySampler
	// waits up to 1s on this same done channel). Failed must give the same
	// guarantee; otherwise the goroutine + ticker leak for the process lifetime.
	select {
	case <-done:
		// Sampler stopped — correct behavior.
	case <-time.After(3 * time.Second):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		var leaked string
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(g, "StartTelemetrySampler") {
				leaked = g
				break
			}
		}
		t.Fatalf("goroutine leak: telemetry sampler still running after runRecorder.Failed marked the run finished; "+
			"Failed (run_recorder.go:195) never calls stopTelemetrySampler, unlike Finish (run_recorder.go:185).\n"+
			"Leaked goroutine stack:\n%s", leaked)
	}
}
