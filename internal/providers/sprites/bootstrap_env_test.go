package sprites

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type bootstrapEnvRunner func(core.LocalCommandRequest) (core.LocalCommandResult, error)

func (f bootstrapEnvRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	return f(req)
}

func TestSpritesBootstrapEnvValidation(t *testing.T) {
	t.Setenv("CBX_TEST_SECRET", "fake-secret-value")
	t.Setenv("CBX_EMPTY_SECRET", "")
	t.Setenv("_"+strings.Repeat("A", 63), "boundary-value")
	// Ensure this case is genuinely unset, regardless of the invoking environment.
	t.Setenv("CBX_MISSING_SECRET", "")
	if err := os.Unsetenv("CBX_MISSING_SECRET"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ raw, want string }{
		{"", ""}, {"CBX_TEST_SECRET", ""}, {"_" + strings.Repeat("A", 63), ""},
		{"bad", "bad"}, {"1BAD", "1BAD"}, {"BAD-NAME", "BAD-NAME"},
		{strings.Repeat("A", 65), strings.Repeat("A", 65)},
		{"CBX_TEST_SECRET,", "invalid variable name"}, {" CBX_TEST_SECRET", "invalid variable name"},
		{"CBX_MISSING_SECRET", "CBX_MISSING_SECRET"}, {"CBX_EMPTY_SECRET", "CBX_EMPTY_SECRET"},
		{strings.TrimSuffix(strings.Repeat("CBX_TEST_SECRET,", 9), ","), "at most 8"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("CRABBOX_SPRITES_BOOTSTRAP_ENV", tc.raw)
			runner := &recordingRunner{}
			b := &spritesBackend{rt: core.Runtime{Exec: runner}, cfg: core.Config{Sprites: core.SpritesConfig{WorkRoot: "/home/sprite/crabbox"}}}
			values, err := spritesBootstrapEnv()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if tc.raw == "" && len(values) != 0 {
					t.Fatal("empty config must be a no-op")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unexpected validation error: %v", err)
			}
			if _, err := b.prepareLease(t.Context(), spritesInfo{Name: "test"}, "lease", "slug", nil, "key", "pub"); err == nil {
				t.Fatal("prepareLease accepted invalid config")
			}
			if len(runner.calls) != 0 {
				t.Fatal("validation must precede SSH bootstrap")
			}
		})
	}
	t.Setenv("CRABBOX_SPRITES_BOOTSTRAP_ENV", "")
	if err := os.Unsetenv("CRABBOX_SPRITES_BOOTSTRAP_ENV"); err != nil {
		t.Fatal(err)
	}
	b := &spritesBackend{}
	values, err := spritesBootstrapEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.bootstrapEnv(t.Context(), "unused", values); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_SPRITES_BOOTSTRAP_ENV", strings.TrimSuffix(strings.Repeat("CBX_TEST_SECRET,", 8), ","))
	if values, err := spritesBootstrapEnv(); err != nil || len(values) != 8 {
		t.Fatal("eight names must be accepted")
	}
}

func TestSpritesBootstrapEnvFixedReplayStdinAndOrdering(t *testing.T) {
	api, newBackend, req := fixedSpritesFixture(t)
	const secret = "fake-secret\n'$(printf unsafe)'\\bytes"
	t.Setenv("CRABBOX_SPRITES_BOOTSTRAP_ENV", "CBX_TEST_SECRET,CBX_SECOND_SECRET")
	t.Setenv("CBX_TEST_SECRET", secret)
	t.Setenv("CBX_SECOND_SECRET", "second-value")
	const script = `set -eu; if sudo test -e /var/lib/crabbox-bootstrap/consumed; then cat >/dev/null; exit 0; fi; sudo install -d -m 700 -o root -g root /run/crabbox-bootstrap; sudo sh -c 'umask 077; cat > "/run/crabbox-bootstrap/$1.tmp" && mv "/run/crabbox-bootstrap/$1.tmp" "/run/crabbox-bootstrap/$1"' sh "$1"`
	for attempt := 0; attempt < 2; attempt++ {
		b := newBackend()
		var order []string
		b.rt.Exec = bootstrapEnvRunner(func(cmd core.LocalCommandRequest) (core.LocalCommandResult, error) {
			for _, arg := range cmd.Args {
				if strings.Contains(arg, secret) || strings.Contains(arg, "second-value") {
					t.Fatal("value leaked to argv")
				}
			}
			if cmd.Stdin == nil {
				if strings.Contains(strings.Join(cmd.Args, " "), "/bin/bash") {
					order = append(order, "ssh")
				}
				return core.LocalCommandResult{}, nil
			}
			name := cmd.Args[len(cmd.Args)-1]
			wantArgs := []string{"exec", "-s", core.LeaseProviderName(fixedTestLeaseID, "fixed-sprite"), "--", "/bin/sh", "-c", script, "sh", name}
			if !reflect.DeepEqual(cmd.Args, wantArgs) {
				t.Fatal("unexpected bootstrap argv")
			}
			data, err := io.ReadAll(cmd.Stdin)
			if err != nil {
				t.Fatal(err)
			}
			want := secret
			if name == "CBX_SECOND_SECRET" {
				want = "second-value"
			}
			if string(data) != want {
				t.Fatal("stdin does not equal value exactly")
			}
			if !cmd.DisableOutputCapture || cmd.Stdout != io.Discard || cmd.Stderr != io.Discard {
				t.Fatal("secret transfer output must be discarded")
			}
			order = append(order, name)
			return core.LocalCommandResult{}, nil
		})
		if _, err := b.Acquire(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(order, []string{"ssh", "CBX_TEST_SECRET", "CBX_SECOND_SECRET"}) {
			t.Fatalf("wrong ordering: %v", order)
		}
	}
	if creates, _, live := api.counts(); creates != 1 || live != 1 {
		t.Fatal("replay created another Sprite")
	}
}

func TestSpritesBootstrapEnvFailureDoesNotLeak(t *testing.T) {
	for _, runnerErr := range []error{nil, errors.New("fake-secret-value")} {
		var output bytes.Buffer
		b := &spritesBackend{rt: core.Runtime{Stdout: &output, Stderr: &output, Exec: bootstrapEnvRunner(func(cmd core.LocalCommandRequest) (core.LocalCommandResult, error) {
			io.WriteString(cmd.Stdout, "fake-secret-value")
			io.WriteString(cmd.Stderr, "fake-secret-value")
			return core.LocalCommandResult{ExitCode: 7, Stdout: "fake-secret-value", Stderr: "fake-secret-value"}, runnerErr
		})}}
		err := b.bootstrapEnv(t.Context(), "test", []spriteBootstrapEnv{{name: "CBX_TEST_SECRET", value: "fake-secret-value"}})
		if err == nil || err.Error() != "sprites bootstrap env CBX_TEST_SECRET failed" || output.Len() != 0 {
			t.Fatal("failure must expose only the variable name")
		}
	}
}

func TestSpritesBootstrapEnvDoesNotRunAfterSSHFailure(t *testing.T) {
	t.Setenv("CRABBOX_SPRITES_BOOTSTRAP_ENV", "CBX_TEST_SECRET")
	t.Setenv("CBX_TEST_SECRET", "fake-secret-value")
	b := &spritesBackend{cfg: core.Config{Sprites: core.SpritesConfig{WorkRoot: "/home/sprite/crabbox"}}, rt: core.Runtime{Stderr: io.Discard, Exec: bootstrapEnvRunner(func(cmd core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if cmd.Stdin != nil {
			t.Fatal("secret transfer ran after failed SSH bootstrap")
		}
		return core.LocalCommandResult{ExitCode: 7}, nil
	})}}
	if _, err := b.prepareLease(t.Context(), spritesInfo{Name: "test"}, "lease", "slug", nil, "key", "pub"); err == nil {
		t.Fatal("SSH failure must abort bootstrap")
	}
}

func TestSpritesBootstrapEnvRejectedBeforeCreate(t *testing.T) {
	api, newBackend, req := fixedSpritesFixture(t)
	t.Setenv("CRABBOX_SPRITES_BOOTSTRAP_ENV", "CBX_UNSET_SECRET")
	t.Setenv("CBX_UNSET_SECRET", "")
	if _, err := newBackend().Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "CBX_UNSET_SECRET") {
		t.Fatalf("err=%v, want unset-variable error", err)
	}
	if creates, _, live := api.counts(); creates != 0 || live != 0 {
		t.Fatalf("creates=%d live=%d, want no Sprite", creates, live)
	}
}
