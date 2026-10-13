package cli

import (
	"flag"
	"io"
	"testing"
	"time"
)

func TestBlacksmithWaitTimeoutSources(t *testing.T) {
	for _, source := range []string{"file", "env", "flag"} {
		for _, raw := range []string{"90s", "", "bad", "0s", "-1s", " 90s "} {
			t.Run(source+"/"+raw, func(t *testing.T) {
				clearConfigEnv(t)
				cfg := BlacksmithConfig{WaitTimeout: time.Minute}
				var err error
				var accepted bool
				switch source {
				case "file":
					result, applyErr := cfg.applyFile(&fileBlacksmithConfig{WaitTimeout: raw})
					accepted, err = result.InputAccepted, applyErr
				case "env":
					t.Setenv("CRABBOX_BLACKSMITH_WAIT_TIMEOUT", raw)
					result, applyErr := cfg.applyEnvSuffix()
					accepted, err = result.InputAccepted, applyErr
				case "flag":
					fs := flag.NewFlagSet("test", flag.ContinueOnError)
					fs.SetOutput(io.Discard)
					values := RegisterBlacksmithConfigFlags(fs, cfg)
					if parseErr := fs.Parse([]string{"--blacksmith-wait-timeout", raw}); parseErr != nil {
						t.Fatal(parseErr)
					}
					result, applyErr := values.Apply(&cfg, fs)
					accepted, err = result.InputAccepted, applyErr
				}
				valid := raw == "90s"
				if source == "flag" && raw != "" && !valid {
					if err == nil {
						t.Fatalf("invalid explicit duration accepted: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				want := time.Minute
				if valid {
					want = 90 * time.Second
				}
				if cfg.WaitTimeout != want || accepted != valid {
					t.Fatalf("timeout=%s accepted=%t, want %s/%t", cfg.WaitTimeout, accepted, want, valid)
				}
			})
		}
	}
}

func TestBlacksmithWaitTimeoutPrecedence(t *testing.T) {
	clearConfigEnv(t)
	cfg := defaultBlacksmithConfig()
	if cfg.WaitTimeout != 0 {
		t.Fatal("unset timeout must preserve the native default")
	}
	if _, err := cfg.applyFile(&fileBlacksmithConfig{WaitTimeout: "5m"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_BLACKSMITH_WAIT_TIMEOUT", "3m")
	if _, err := cfg.applyEnvSuffix(); err != nil {
		t.Fatal(err)
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterBlacksmithConfigFlags(fs, cfg)
	if _, err := values.Apply(&cfg, fs); err != nil || cfg.WaitTimeout != 3*time.Minute {
		t.Fatalf("unvisited flag changed timeout: %s, %v", cfg.WaitTimeout, err)
	}
	if err := fs.Parse([]string{"--blacksmith-wait-timeout", "2m"}); err != nil {
		t.Fatal(err)
	}
	if _, err := values.Apply(&cfg, fs); err != nil || cfg.WaitTimeout != 2*time.Minute {
		t.Fatalf("flag override failed: %s, %v", cfg.WaitTimeout, err)
	}
}
