package cli

import "testing"

func TestStatusWaitStopsForRetainedLifecycleAdmission(t *testing.T) {
	for _, state := range []string{"suspended", "compute-expired", "deleted"} {
		t.Run(state, func(t *testing.T) {
			view := StatusView{State: state, Retention: &SessionDiskRetentionView{}}
			if !statusWaitDone(view) {
				t.Fatal("readiness wait continued although explicit lifecycle action is required")
			}
			if err := statusWaitTerminalError("cbx_123456abcdef", view); err == nil {
				t.Fatal("non-ready retained lease reported readiness success")
			}
		})
	}
	for _, state := range []string{"transitioning", "unknown", "running"} {
		view := StatusView{State: state, Retention: &SessionDiskRetentionView{}}
		if statusWaitDone(view) {
			t.Fatalf("uncertain %s observation ended readiness wait", state)
		}
	}
}
