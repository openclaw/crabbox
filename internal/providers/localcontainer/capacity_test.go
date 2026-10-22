package localcontainer

import (
	"errors"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestLocalContainerCapacityRefusalTagging(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "resource exhausted", err: errors.New("container run failed: resource exhausted"), want: true},
		{name: "too many containers", err: errors.New("too many containers"), want: true},
		{name: "disk full stays final", err: errors.New("no space left on device"), want: false},
		{name: "cannot allocate stays final", err: errors.New("cannot allocate memory"), want: false},
		{name: "plain", err: errors.New("image not found"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isLocalContainerCapacityError(tc.err)
			if got != tc.want {
				t.Fatalf("isLocalContainerCapacityError=%v want %v", got, tc.want)
			}
			marked := markLocalContainerCapacityRefusal(tc.err)
			if core.IsCapacityRefusal(marked) != tc.want {
				t.Fatalf("marked capacity=%v want %v (%v)", core.IsCapacityRefusal(marked), tc.want, marked)
			}
		})
	}
}
