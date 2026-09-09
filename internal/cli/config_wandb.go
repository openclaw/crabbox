package cli

//go:generate go run ../../scripts/configgen -source config_wandb.go -output config_wandb_generated.go -type WandbConfig -provider wandb

// Runtime fallbacks do not initialize the raw configuration or flag defaults.
const (
	WandbDefaultImageFallback       = "ubuntu:24.04"
	WandbMaxLifetimeSecondsFallback = 1800
)

// WandbConfig drives W&B Sandboxes. The client owns the vendor API-key and
// netrc fallback chain; the loader applies only its explicit primary override.
type WandbConfig struct {
	APIKey             string `config:"apiKey" env:"CRABBOX_WANDB_API_KEY" sources:"user,repo,env" fileIgnoreEmpty:"true"`
	DefaultImage       string `config:"defaultImage" env:"CRABBOX_WANDB_DEFAULT_IMAGE" envAlias:"WANDB_DEFAULT_IMAGE" flag:"wandb-image" sources:"user,repo,env,flag" help:"Container image used when acquiring a new W&B sandbox" fileIgnoreEmpty:"true"`
	MaxLifetimeSeconds int    `config:"maxLifetimeSeconds" env:"CRABBOX_WANDB_MAX_LIFETIME_SECONDS" envAlias:"WANDB_MAX_LIFETIME_SECONDS" flag:"wandb-max-lifetime" sources:"user,repo,env,flag" help:"Maximum sandbox lifetime in seconds before W&B reclaims it" nonnegative:"true" fileInt:"positive" envInt:"fallback"`
}
