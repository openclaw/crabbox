package runpod

import (
	"errors"
	"net/http"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestIsRunpodCapacityErrorTagged(t *testing.T) {
	t.Parallel()
	err := &runpodAPIError{StatusCode: http.StatusInternalServerError, Body: "no instances currently available"}
	if !isRunpodCapacityError(err) {
		t.Fatal("expected capacity classifier match")
	}
	if !core.IsCapacityRefusal(core.MarkCapacityRefusal(err)) {
		t.Fatal("expected CapacityRefusal marker")
	}
	if isRunpodCapacityError(errors.New("unauthorized")) {
		t.Fatal("non-capacity must not match")
	}
}
