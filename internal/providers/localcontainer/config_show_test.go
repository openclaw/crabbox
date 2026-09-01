package localcontainer

import (
	"reflect"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestNormalizeConfigForShowPreservesUnrelatedSettings(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.WorkRoot = "/work/generic"
	cfg.LocalContainer.User = "test-runner"
	cfg.LocalContainer.Volumes = []string{"/synthetic-private-volume:/mnt/data"}
	cfg.LocalContainer.CheckpointMetadata = map[string]string{checkpointMetadataForkName: "synthetic-private-checkpoint"}

	want := cfg
	want.LocalContainer.WorkRoot = cfg.WorkRoot
	got := (Provider{}).NormalizeConfigForShow(cfg)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("display normalization changed fields beyond container defaults and work root")
	}
	if cfg.LocalContainer.WorkRoot != "" {
		t.Fatal("display normalization changed execution input")
	}
	if got.ServerType == "synthetic-private-checkpoint" || got.SSHUser == "test-runner" {
		t.Fatal("display normalization exposed checkpoint metadata or changed SSH defaults")
	}
}
