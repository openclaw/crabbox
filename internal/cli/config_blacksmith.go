package cli

import "time"

//go:generate go run ../../scripts/configgen -source config_blacksmith.go -output config_blacksmith_generated.go -type BlacksmithConfig -provider blacksmith-testbox

// BlacksmithConfig owns the six ordinary bindings; environment halves retain loader order.
type BlacksmithConfig struct {
	Org         string        `config:"org" env:"CRABBOX_BLACKSMITH_ORG" flag:"blacksmith-org" help:"Blacksmith organization" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	Workflow    string        `config:"workflow" env:"CRABBOX_BLACKSMITH_WORKFLOW" flag:"blacksmith-workflow" help:"Blacksmith Testbox workflow file, name, or id" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	Job         string        `config:"job" env:"CRABBOX_BLACKSMITH_JOB" flag:"blacksmith-job" help:"Blacksmith Testbox workflow job" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	Ref         string        `config:"ref" env:"CRABBOX_BLACKSMITH_REF" flag:"blacksmith-ref" help:"Blacksmith Testbox git ref" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	IdleTimeout time.Duration `config:"idleTimeout" env:"CRABBOX_BLACKSMITH_IDLE_TIMEOUT" sources:"user,repo,env" duration:"positive-overlay" fileStorage:"value" envSplitBefore:"true"`
	Debug       bool          `config:"debug" env:"CRABBOX_BLACKSMITH_DEBUG" sources:"user,repo,env"`
}
