package localcontainer

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type statsRunner struct {
	output string
	args   []string
}

func (r *statsRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.args = req.Args
	return core.LocalCommandResult{Stdout: r.output}, nil
}
func TestServedCPUCores(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   float64
		bad    bool
	}{
		{"owned-a 150.00%\nowned-b 50.00%", 2, false},
		{"owned-a 0.00%\nowned-b 0.00%", 0, false},
		{"unowned 900.00%", 0, true}, {"owned-a 10.00%", 0, true},
		{"owned-a NaN%\nowned-b 0%", 0, true}, {"owned-a -1%\nowned-b 0%", 0, true},
	} {
		t.Run(fmt.Sprint(tc.output), func(t *testing.T) {
			runner := &statsRunner{output: tc.output}
			rt := NewServedRuntime(core.Config{LocalContainer: core.LocalContainerConfig{Runtime: "docker"}}, core.Runtime{Exec: runner})
			got, err := rt.CPUCores(t.Context(), []ServedContainer{{ID: "owned-a"}, {ID: "owned-b"}})
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("got %v %v", got, err)
			}
			if !reflect.DeepEqual(runner.args, []string{"stats", "--no-stream", "--no-trunc", "--format", "{{.ID}} {{.CPUPerc}}", "owned-a", "owned-b"}) {
				t.Fatalf("unscoped stats: %v", runner.args)
			}
		})
	}
}
