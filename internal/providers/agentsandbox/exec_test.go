package agentsandbox

import (
	"context"
	"reflect"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestExecContextHonorsZeroAsNoDeadline(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.AgentSandbox.ExecTimeoutSecs = 0
	b := backend{cfg: cfg}

	ctx, cancel := b.execContext(context.Background())
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("zero exec timeout created a deadline")
	}

	b.cfg.AgentSandbox.ExecTimeoutSecs = 1
	ctx, cancel = b.execContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("positive exec timeout did not create a deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > time.Second {
		t.Fatalf("deadline remaining=%s, want within one second", remaining)
	}
}

func TestBuildCommandUsesBashCompatibleShell(t *testing.T) {
	tests := []struct {
		name      string
		command   []string
		shellMode bool
		want      []string
	}{
		{name: "shell mode", command: []string{"echo ok"}, shellMode: true, want: []string{"bash", "-lc", "echo ok"}},
		{name: "single string shell syntax", command: []string{"echo ok && pwd"}, want: []string{"bash", "-lc", "echo ok && pwd"}},
		{name: "leading env assignment", command: []string{"FOO=bar", "printenv", "FOO"}, want: []string{"bash", "-lc", "FOO='bar' 'printenv' 'FOO'"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildCommand(tt.command, tt.shellMode)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("buildCommand()=%#v want %#v", got, tt.want)
			}
		})
	}
}
