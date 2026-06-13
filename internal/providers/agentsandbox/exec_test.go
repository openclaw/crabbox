package agentsandbox

import (
	"reflect"
	"testing"
)

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
