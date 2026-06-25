package replicate

import (
	"flag"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestApplyReplicateProviderFlagsOnlyWhenExplicit(t *testing.T) {
	cfg := core.Config{
		Provider: "replicate",
		Replicate: ReplicateConfig{
			APIURL:           "https://api.replicate.com/v1",
			Deployment:       "owner/initial",
			Workdir:          "/workspace/original",
			WaitSecs:         1,
			PollIntervalSecs: 2,
			ExecTimeoutSecs:  3,
			CancelAfterSecs:  4,
			MaxArchiveBytes:  5,
		},
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterReplicateProviderFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--replicate-version", "version-123",
		"--replicate-workdir", "/workspace/next",
		"--replicate-wait-secs", "10",
		"--replicate-poll-interval-secs", "11",
		"--replicate-exec-timeout-secs", "12",
		"--replicate-cancel-after-secs", "13",
		"--replicate-max-archive-bytes", "14",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyReplicateProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Replicate.APIURL != "https://api.replicate.com/v1" {
		t.Fatalf("APIURL changed without explicit flag: %#v", cfg.Replicate)
	}
	if cfg.Replicate.Deployment != "owner/initial" || cfg.Replicate.Version != "version-123" || cfg.Replicate.Workdir != "/workspace/next" {
		t.Fatalf("string flags not applied correctly: %#v", cfg.Replicate)
	}
	if cfg.Replicate.WaitSecs != 10 || cfg.Replicate.PollIntervalSecs != 11 || cfg.Replicate.ExecTimeoutSecs != 12 || cfg.Replicate.CancelAfterSecs != 13 || cfg.Replicate.MaxArchiveBytes != 14 {
		t.Fatalf("numeric flags not applied correctly: %#v", cfg.Replicate)
	}
}

func TestReplicateProviderFlagsDoNotExposeToken(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterReplicateProviderFlags(fs, core.Config{Replicate: DefaultConfig()})
	if fs.Lookup("replicate-api-token") != nil {
		t.Fatal("unexpected --replicate-api-token flag")
	}
}
