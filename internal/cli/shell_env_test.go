package cli

import "testing"

func TestValidShellEnvName(t *testing.T) {
	for _, name := range []string{"_", "A", "z", "_OK_1", "PATH", "lower_case9"} {
		if !ValidShellEnvName(name) {
			t.Errorf("rejected portable environment name %q", name)
		}
	}
	for _, name := range []string{"", "9", "1_BAD", "BAD.NAME", "BAD-NAME", "A=B", " A", "A ", "A\n", "A\r", "A\x00", "é", "Aé", "A\xff", "A;echo"} {
		if ValidShellEnvName(name) {
			t.Errorf("accepted invalid environment name %q", name)
		}
	}
}

func TestIsShellEnvAssignment(t *testing.T) {
	for _, word := range []string{"A=", "_OK_1=value", "A=a=b", "A= spaces \n é", "A=$(echo value)"} {
		if !IsShellEnvAssignment(word) {
			t.Errorf("rejected assignment %q", word)
		}
	}
	for _, word := range []string{"", "A", "=value", "1_BAD=value", "A B=value", "A\n=value", "é=value", "A\xff=value"} {
		if IsShellEnvAssignment(word) {
			t.Errorf("accepted non-assignment %q", word)
		}
	}
}
