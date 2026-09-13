package cli

import (
	"os/exec"
	"testing"
)

func TestCommandIntentShellScript(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command []string
		shell   bool
		literal map[int]bool
		want    string
		output  string
	}{
		{name: "spaced argument", command: []string{"printf", "%s", "two words"}, want: "'printf' '%s' 'two words'", output: "two words"},
		{name: "quotes", command: []string{"printf", "%s", "it's quoted"}, want: "'printf' '%s' 'it'\\''s quoted'", output: "it's quoted"},
		{name: "single source", command: []string{"printf first && printf second"}, want: "printf first && printf second", output: "firstsecond"},
		{name: "explicit source", command: []string{"printf", "first", "&&", "printf", "second"}, shell: true, want: "printf first && printf second", output: "firstsecond"},
		{name: "operators", command: []string{"printf", "first", "&&", "printf", "second"}, want: "'printf' 'first' && 'printf' 'second'", output: "firstsecond"},
		{name: "environment", command: []string{"MESSAGE=hello there", "sh", "-c", "printf %s \"$MESSAGE\""}, want: "MESSAGE='hello there' 'sh' '-c' 'printf %s \"$MESSAGE\"'", output: "hello there"},
		{name: "literal operator", command: []string{"printf", "%s", "&&"}, literal: map[int]bool{2: true}, want: "'printf' '%s' '&&'", output: "&&"},
		{name: "literal source in shell segment", command: []string{"printf", "%s", "&&", ";", "printf", "done"}, literal: map[int]bool{2: true}, want: "'printf' '%s' '&&' ; 'printf' 'done'", output: "&&done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			intent, err := ParseCommandIntent(tc.command, tc.shell, tc.literal)
			if err != nil {
				t.Fatal(err)
			}
			if got := intent.ShellScript(); got != tc.want {
				t.Fatalf("source=%q want=%q", got, tc.want)
			}
			sh, err := exec.LookPath("sh")
			if err != nil {
				t.Skip("POSIX shell unavailable")
			}
			cmd := exec.CommandContext(t.Context(), sh, "-c", intent.ShellScript())
			cmd.Dir = t.TempDir()
			output, err := cmd.CombinedOutput()
			if err != nil || string(output) != tc.output {
				t.Fatalf("output=%q want=%q error=%v", output, tc.output, err)
			}
		})
	}
}
