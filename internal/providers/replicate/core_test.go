package replicate

import (
	"strings"
	"testing"
)

func TestParseRunnerOutputRequiresIntegerExitCode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		wantErr string
	}{
		{name: "missing", payload: `{"stdout":"ok","status":"succeeded"}`, wantErr: "missing required exit_code"},
		{name: "string", payload: `{"exit_code":"0"}`, wantErr: "must be an integer"},
		{name: "float", payload: `{"exit_code":0.5}`, wantErr: "must be an integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseRunnerOutput([]byte(tc.payload)); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ParseRunnerOutput error=%v want %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseRunnerOutputPreservesExitCode(t *testing.T) {
	out, err := ParseRunnerOutput([]byte(`{"status":"succeeded","exit_code":7,"stdout":"out","stderr":"err"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 7 || out.Stdout != "out" || out.Stderr != "err" {
		t.Fatalf("output=%#v", out)
	}
}

func TestResolveAPITokenPrecedence(t *testing.T) {
	t.Setenv(envReplicateToken, "vendor-token")
	t.Setenv(envCrabboxReplicateToken, "crabbox-token")
	token, source, ok := ResolveAPIToken()
	if !ok || token != "crabbox-token" || source != envCrabboxReplicateToken {
		t.Fatalf("token=%q source=%q ok=%t", token, source, ok)
	}
}
