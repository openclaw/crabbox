package phala

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func TestD2BootstrapScriptExact(t *testing.T) {
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(phalaToolBootstrapCommand()))); got != "af79a35234c12f430fa56ee70999677bca692849f8002cb692159cda046a9318" {
		t.Fatalf("bootstrap script changed: %s", got)
	}
}
func TestD2PrepareSSHPhases(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake ssh")
	}
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "calls")
			t.Setenv("D2_SSH_LOG", log)
			t.Setenv("D2_BOOTSTRAP_FAIL", fmt.Sprint(fail))
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			script := `#!/bin/sh
port=
next=
for arg do
 if [ "$next" = port ]; then port=$arg; fi
 next=
 if [ "$arg" = -p ]; then next=port; fi
 last=$arg
done
printf '%s\n%s\n---\n' "$port" "$last" >> "$D2_SSH_LOG"
if [ "$port" = 22 ]; then exit 1; fi
case "$last" in
 *crabbox-phala-apt-update.log*) if [ "$D2_BOOTSTRAP_FAIL" = true ]; then exit 1; fi ;;
esac
exit 0
`
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			b := &backend{rt: core.Runtime{Stderr: io.Discard}}
			target := core.SSHTarget{Host: "synthetic.invalid", User: "root", Port: "22", FallbackPorts: []string{"2222"}, SSHConfigProxy: true, ReadyCheck: "command -v rsync", DisableHostKeyChecking: true}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := shared.PrepareSSHWithBootstrap(ctx, core.BaseConfig(), &target, b.rt.Stderr, "Phala CVM", phalaToolBootstrapCommand())
			if fail {
				if err == nil || !strings.Contains(err.Error(), "Phala CVM tool bootstrap failed:") {
					t.Fatalf("err=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if target.Port != "2222" || target.ReadyCheck != "command -v rsync" {
				t.Fatalf("target=%#v", target)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			bootstrap := strings.Index(text, phalaToolBootstrapCommand())
			if bootstrap < 0 || !strings.Contains(text[:bootstrap], "true") {
				t.Fatalf("missing initial probe/bootstrap: %s", text)
			}
			tools := strings.Contains(text[bootstrap+len(phalaToolBootstrapCommand()):], "command -v rsync")
			if tools == fail {
				t.Fatalf("tools phase=%v after fail=%v: %s", tools, fail, text)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := &backend{rt: core.Runtime{Stderr: io.Discard}}
	target := core.SSHTarget{Host: "synthetic.invalid", Port: "22", ReadyCheck: "tools"}
	if err := shared.PrepareSSHWithBootstrap(ctx, core.BaseConfig(), &target, b.rt.Stderr, "Phala CVM", phalaToolBootstrapCommand()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if target.Port != "22" || target.ReadyCheck != "tools" {
		t.Fatal("cancelled probe mutated target")
	}
}

type d2RecoveryRunner struct {
	calls  []time.Time
	cancel context.CancelFunc
}

func (r *d2RecoveryRunner) Run(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, time.Now())
	if r.cancel != nil {
		r.cancel()
	}
	return core.LocalCommandResult{Stderr: fmt.Sprintf("attempt-%d", len(r.calls))}, errors.New("retryable")
}
func TestD2RecoveryCadenceAndLastError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &d2RecoveryRunner{}
		b := &backend{rt: core.Runtime{Exec: r, Stderr: io.Discard}}
		start := time.Now()
		_, err := shared.RetryLeaseLookup(context.Background(), core.BaseConfig(), "cbx_d2", 600*time.Millisecond, b.findByLease)
		if err == nil || !strings.Contains(err.Error(), "attempt-3") || len(r.calls) != 3 || time.Since(start) != 600*time.Millisecond {
			t.Fatalf("err=%v calls=%v elapsed=%s", err, r.calls, time.Since(start))
		}
		for i, at := range r.calls {
			if at.Sub(start) != time.Duration(i)*250*time.Millisecond {
				t.Fatalf("cadence=%v", r.calls)
			}
		}
	})
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := &d2RecoveryRunner{cancel: cancel}
		b := &backend{rt: core.Runtime{Exec: r, Stderr: io.Discard}}
		_, err := shared.RetryLeaseLookup(ctx, core.BaseConfig(), "cbx_d2", time.Minute, b.findByLease)
		if !errors.Is(err, context.Canceled) || len(r.calls) != 1 {
			t.Fatalf("err=%v calls=%d", err, len(r.calls))
		}
	})
}
